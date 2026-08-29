package sql

import (
	"errors"
	"math/rand"
	"strings"
	"time"

	"github.com/go-gorp/gorp/v3"
	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/tz"
)

// inventorySnapshotInsertBatch is how many rows one multi-row INSERT carries.
// A snapshot may hold tens of thousands of rows, and one statement per row
// makes the replacing transaction hold its locks for seconds.
const inventorySnapshotInsertBatch = 500

// likeEscapeChar is used instead of the default backslash: MySQL treats a
// backslash as an escape character inside string literals while PostgreSQL
// does not, so a literal ESCAPE '\' clause cannot be written portably.
const likeEscapeChar = "!"

// escapeLikePattern makes a user's search string a literal for LIKE: the
// wildcards and the escape character itself are prefixed with likeEscapeChar,
// which the queries then name in an ESCAPE clause.
func escapeLikePattern(search string) string {
	return strings.NewReplacer(
		likeEscapeChar, likeEscapeChar+likeEscapeChar,
		"%", likeEscapeChar+"%",
		"_", likeEscapeChar+"_",
	).Replace(search)
}

func (d *SqlDb) GetInventorySnapshot(projectID int, inventoryID int) (snapshot db.InventorySnapshot, err error) {
	err = d.selectOne(
		&snapshot,
		"select * from project__inventory_snapshot where project_id=? and inventory_id=?",
		projectID,
		inventoryID)

	return
}

func (d *SqlDb) GetInventorySnapshotHosts(
	projectID int,
	inventoryID int,
	filter db.InventoryHostFilter,
) (hosts []db.InventorySnapshotHost, err error) {

	query := "select h.inventory_id, h.name from project__inventory_snapshot_host h " +
		"inner join project__inventory_snapshot s on s.inventory_id = h.inventory_id " +
		"where s.project_id=? and h.inventory_id=?"

	args := []any{projectID, inventoryID}

	if filter.Group != "" {
		query += " and exists (select 1 from project__inventory_snapshot_host_group m " +
			"where m.inventory_id = h.inventory_id and m.group_name = ? and m.host_name = h.name)"
		args = append(args, filter.Group)
	}

	if filter.Search != "" {
		// Both sides are lowered by the same engine on purpose. Lowering the
		// pattern in Go instead would fold it by Unicode rules while SQLite's
		// lower() folds only ASCII, so a search for "ÄB" became "äb" and stopped
		// matching the host "ÄB01". Whatever an engine folds, it now folds
		// identically on both sides: MySQL and PostgreSQL stay Unicode
		// case-insensitive, SQLite stays case-insensitive for ASCII and exact
		// for the rest.
		query += " and lower(h.name) like lower(?) escape '" + likeEscapeChar + "'"
		args = append(args, "%"+escapeLikePattern(filter.Search)+"%")
	}

	limit := filter.Limit
	if limit <= 0 || limit > db.MaxInventoryHostPageSize {
		limit = db.MaxInventoryHostPageSize
	}

	query += " order by h.name limit ?"
	args = append(args, limit)

	if filter.Offset > 0 {
		query += " offset ?"
		args = append(args, filter.Offset)
	}

	_, err = d.selectAll(&hosts, query, args...)

	return
}

func (d *SqlDb) GetInventorySnapshotGroups(
	projectID int,
	inventoryID int,
	filter db.InventoryGroupFilter,
) (groups []db.InventorySnapshotGroup, err error) {

	query := "select g.inventory_id, g.name, g.host_count from project__inventory_snapshot_group g " +
		"inner join project__inventory_snapshot s on s.inventory_id = g.inventory_id " +
		"where s.project_id=? and g.inventory_id=? order by g.name"

	args := []any{projectID, inventoryID}

	limit := filter.Limit
	if limit <= 0 || limit > db.MaxInventoryGroupPageSize {
		limit = db.MaxInventoryGroupPageSize
	}

	query += " limit ?"
	args = append(args, limit)

	if filter.Offset > 0 {
		query += " offset ?"
		args = append(args, filter.Offset)
	}

	_, err = d.selectAll(&groups, query, args...)

	return
}

// inventorySnapshotWriteAttempts bounds the conflict retries of a snapshot
// write. Two writers need one retry; the rest of the budget is for the case
// several of them collide at once, where a retry lands on yet another writer.
//
// Measured against MariaDB 12.3 with eight concurrent writers of one snapshot
// (assertConcurrentWritesConverge in the dialect run): at three attempts with a
// fixed pause, five of the eight still came back with a conflict, because every
// loser waited the same 50 ms and they all retried in step.
const inventorySnapshotWriteAttempts = 5

// inventorySnapshotWriteBackoff is the base of the wait between attempts. It
// only has to outlast the other transaction's commit; snapshotWriteBackoff
// spreads the retries out from there.
const inventorySnapshotWriteBackoff = 50 * time.Millisecond

// snapshotWriteBackoff returns how long to wait before the given attempt
// (1 being the first retry).
//
// It doubles per attempt and keeps half the window random. Doubling is for the
// collision that has not cleared yet; the random half is what stops a group of
// losers from waking together and colliding again — with a fixed pause, the
// conflict they were retrying is exactly what synchronised them.
func snapshotWriteBackoff(attempt int) time.Duration {
	window := inventorySnapshotWriteBackoff << (attempt - 1)

	return window/2 + time.Duration(rand.Int63n(int64(window/2)))
}

func (d *SqlDb) SetInventorySnapshot(snapshot db.InventorySnapshot, content db.InventorySnapshotContent) error {
	var err error

	// Two writers replacing the same inventory's rows at once — two nodes in
	// HA, or two requests on one — do not get last-writer-wins from the
	// database: the delete/insert pairs collide as a deadlock or a duplicate
	// key. Both verdicts mean "the other writer got there first", and since
	// every writer derives the same rows from the same inventory, retrying
	// once the other has committed is the whole conflict resolution.
	for attempt := 0; attempt < inventorySnapshotWriteAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(snapshotWriteBackoff(attempt))
		}

		err = d.setInventorySnapshotOnce(snapshot, content)

		if err == nil || !isSnapshotWriteConflict(err) {
			return err
		}
	}

	return err
}

// isSnapshotWriteConflict recognises the three dialects' ways of saying two
// transactions collided. Anything else — a foreign key, a full disk — is not
// cured by retrying and is returned as is.
//
// The driver error codes are asked first because they are the only portable
// signal: MySQL renders its messages in the server's lc_messages, so on an
// installation running a non-English locale the substrings below match nothing
// and the retry loop silently stops retrying. The substrings remain for SQLite,
// whose driver reports its busy and constraint errors as plain text, and as a
// fallback for a wrapped error the codes cannot be recovered from.
func isSnapshotWriteConflict(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		// 1213 deadlock found, 1062 duplicate entry, 1205 lock wait timeout,
		// 1020 record has changed since last read.
		//
		// 1020 is what MariaDB 12 answers with where MySQL and MariaDB 10
		// report a deadlock: the same collision between two writers replacing
		// the same inventory, reported by the optimistic check rather than by
		// the lock manager. Its own message ends in "try restarting
		// transaction", which is exactly what the loop above does.
		switch mysqlErr.Number {
		case 1213, 1062, 1205, 1020:
			return true
		default:
			return false
		}
	}

	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		// 40P01 deadlock detected, 23505 unique violation, 40001 serialization
		// failure.
		return pqErr.Code == "40P01" || pqErr.Code == "23505" || pqErr.Code == "40001"
	}

	message := strings.ToLower(err.Error())

	for _, marker := range []string{
		"deadlock",                 // mysql: "Deadlock found...", postgres: "deadlock detected"
		"database is locked",       // sqlite busy
		"database table is locked", // sqlite busy, table-level
		"duplicate entry",          // mysql unique violation
		"duplicate key",            // postgres unique violation
		"unique constraint",        // sqlite: "UNIQUE constraint failed"
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}

	return false
}

func (d *SqlDb) setInventorySnapshotOnce(snapshot db.InventorySnapshot, content db.InventorySnapshotContent) error {
	tx, err := d.Sql().Begin()
	if err != nil {
		return err
	}

	if err = d.replaceInventorySnapshot(tx, snapshot, content); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

func (d *SqlDb) replaceInventorySnapshot(
	tx *gorp.Transaction,
	snapshot db.InventorySnapshot,
	content db.InventorySnapshotContent,
) error {

	// The pair is verified before anything is deleted: the deletes below go by
	// inventory id alone, so a caller pairing one project with another's
	// inventory would otherwise wipe that project's snapshot — and the insert
	// would then move it under the caller's project, where reads scoped by
	// project would serve it. The foreign keys cannot catch this: both ids
	// exist, only the pair is wrong. Same reasoning as in
	// ClaimInventorySnapshotResolve.
	owned, err := tx.SelectInt(
		d.PrepareQuery("select count(1) from project__inventory where id=? and project_id=?"),
		snapshot.InventoryID, snapshot.ProjectID,
	)

	if err != nil {
		return err
	}

	if owned == 0 {
		return db.ErrNotFound
	}

	for _, statement := range []string{
		"delete from project__inventory_snapshot_host_group where inventory_id=?",
		"delete from project__inventory_snapshot_group where inventory_id=?",
		"delete from project__inventory_snapshot_host where inventory_id=?",
		"delete from project__inventory_snapshot where inventory_id=?",
	} {
		if _, err := d.execTx(tx, statement, snapshot.InventoryID); err != nil {
			return err
		}
	}

	_, err = d.execTx(
		tx,
		"insert into project__inventory_snapshot "+
			"(inventory_id, project_id, status, error, source_hash, host_count, created, task_id) "+
			"values (?, ?, ?, ?, ?, ?, ?, ?)",
		snapshot.InventoryID,
		snapshot.ProjectID,
		snapshot.Status,
		db.TruncateSnapshotError(snapshot.Error),
		snapshot.SourceHash,
		len(content.Hosts),
		snapshot.Created,
		snapshot.TaskID)

	if err != nil {
		return err
	}

	err = insertInventoryRows(d, tx, len(content.Hosts), 2,
		"insert into project__inventory_snapshot_host (inventory_id, name) values ",
		func(i int) []any {
			return []any{snapshot.InventoryID, content.Hosts[i]}
		})

	if err != nil {
		return err
	}

	err = insertInventoryRows(d, tx, len(content.Groups), 3,
		"insert into project__inventory_snapshot_group (inventory_id, name, host_count) values ",
		func(i int) []any {
			return []any{snapshot.InventoryID, content.Groups[i].Name, len(content.Groups[i].Hosts)}
		})

	if err != nil {
		return err
	}

	type membership struct {
		group string
		host  string
	}

	var memberships []membership

	for _, group := range content.Groups {
		for _, host := range group.Hosts {
			memberships = append(memberships, membership{group: group.Name, host: host})
		}
	}

	return insertInventoryRows(d, tx, len(memberships), 3,
		"insert into project__inventory_snapshot_host_group (inventory_id, group_name, host_name) values ",
		func(i int) []any {
			return []any{snapshot.InventoryID, memberships[i].group, memberships[i].host}
		})
}

// insertInventoryRows runs prefix as multi-row INSERT statements of at most
// inventorySnapshotInsertBatch rows, taking the values of row i from valuesOf.
func insertInventoryRows(
	d *SqlDb,
	tx *gorp.Transaction,
	count int,
	columns int,
	prefix string,
	valuesOf func(i int) []any,
) error {

	placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", columns), ",") + ")"

	for start := 0; start < count; start += inventorySnapshotInsertBatch {
		end := start + inventorySnapshotInsertBatch
		if end > count {
			end = count
		}

		args := make([]any, 0, (end-start)*columns)
		for i := start; i < end; i++ {
			args = append(args, valuesOf(i)...)
		}

		query := prefix + strings.TrimSuffix(strings.Repeat(placeholder+",", end-start), ",")

		if _, err := d.execTx(tx, query, args...); err != nil {
			return err
		}
	}

	return nil
}

// SetResolvedInventorySnapshot writes a runner's resolved host list, but only
// while the pending claim it is settling is still assigned to taskID. Every
// other claim transition (Assign, Release, Fail) is already scoped to its
// owner this way; without the same guard here, a report from a task whose
// claim expired and was taken over by a later refresh would overwrite that
// fresher pending resolve (or its finished result) with a stale one.
//
// The bool return tells the caller whether the write actually happened: a
// losing report is not an error, since the play it came from genuinely ran.
func (d *SqlDb) SetResolvedInventorySnapshot(
	snapshot db.InventorySnapshot,
	content db.InventorySnapshotContent,
	taskID int,
) (bool, error) {
	var written bool
	var err error

	for attempt := 0; attempt < inventorySnapshotWriteAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(inventorySnapshotWriteBackoff)
		}

		written, err = d.setResolvedInventorySnapshotOnce(snapshot, content, taskID)

		if err == nil || !isSnapshotWriteConflict(err) {
			return written, err
		}
	}

	return written, err
}

// setResolvedInventorySnapshotOnce asserts ownership with a self-assigning
// UPDATE rather than a plain SELECT: it takes the row's write lock for the
// rest of the transaction, so no other writer can hand the claim to someone
// else between the check and the replace below.
func (d *SqlDb) setResolvedInventorySnapshotOnce(
	snapshot db.InventorySnapshot,
	content db.InventorySnapshotContent,
	taskID int,
) (bool, error) {
	tx, err := d.Sql().Begin()
	if err != nil {
		return false, err
	}

	// Ownership is claimed by deleting the pending row this task holds, rather
	// than by reading it first: the delete is atomic against a concurrent
	// takeover, so there is no window between checking the owner and writing.
	// It has to be a statement that really changes a row — MySQL reports
	// changed rows, not matched ones, in RowsAffected unless the DSN asks for
	// clientFoundRows, so a self-assigning update would report zero here and
	// every resolve would look like a lost race. The row is replaced by
	// replaceInventorySnapshot below, whose own unscoped delete then matches
	// nothing.
	result, err := d.execTx(
		tx,
		"delete from project__inventory_snapshot "+
			"where project_id=? and inventory_id=? and status=? and task_id=?",
		snapshot.ProjectID, snapshot.InventoryID, db.InventorySnapshotStatusPending, taskID,
	)

	if err != nil {
		_ = tx.Rollback()
		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}

	if affected == 0 {
		_ = tx.Rollback()
		return false, nil
	}

	if err = d.replaceInventorySnapshot(tx, snapshot, content); err != nil {
		_ = tx.Rollback()
		return false, err
	}

	return true, tx.Commit()
}

func (d *SqlDb) ClaimInventorySnapshotResolve(projectID int, inventoryID int) (bool, error) {
	now := tz.Now()

	// A row that is already pending is left alone, which is the whole point:
	// whoever set it has a resolve running. Unless it has been pending too
	// long — the task that would have cleared it is gone, and refusing every
	// later claim would leave the inventory stuck forever.
	//
	// The claim is taken before its task exists, so task_id starts empty; the
	// dispatcher fills it in with AssignInventorySnapshotResolveTask once the
	// task has actually been created.
	//
	// host_count is deliberately left alone: the host rows of the previous
	// answer stay in place for the whole resolve, so zeroing the count would
	// make the snapshot report no hosts while a listing of the same snapshot
	// still returned them.
	result, err := d.exec(
		"update project__inventory_snapshot "+
			"set status=?, task_id=null, created=?, error='', source_hash='' "+
			"where project_id=? and inventory_id=? and (status<>? or created<?)",
		db.InventorySnapshotStatusPending, now,
		projectID, inventoryID, db.InventorySnapshotStatusPending,
		now.Add(-db.MaxInventoryResolveAge),
	)

	if err != nil {
		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	if affected > 0 {
		return true, nil
	}

	// Nothing was updated: either somebody holds the claim, or the inventory
	// has never been synchronised and there is no row to update.
	_, err = d.GetInventorySnapshot(projectID, inventoryID)

	if err == nil {
		return false, nil
	}

	if !errors.Is(err, db.ErrNotFound) {
		return false, err
	}

	// The insert is conditional on the inventory actually belonging to the
	// project. The foreign keys alone would not catch a caller pairing one
	// project with another's inventory: both ids exist, only the pair is wrong,
	// and the row would then answer reads scoped to the wrong project.
	result, err = d.exec(
		"insert into project__inventory_snapshot "+
			"(inventory_id, project_id, status, error, source_hash, host_count, created, task_id) "+
			"select ?, ?, ?, '', '', 0, ?, null "+
			"where exists (select 1 from project__inventory where id=? and project_id=?)",
		inventoryID, projectID, db.InventorySnapshotStatusPending, now,
		inventoryID, projectID,
	)

	if err != nil {
		// The update, the select and this insert are three statements outside a
		// transaction, so another claimant — the other node in HA, or another
		// request on this one — can insert the row in between. inventory_id is
		// the primary key, so the loser gets a duplicate key back; that is the
		// same verdict the update path reports as "somebody else holds the
		// claim", and it must read the same way here rather than as a 500.
		if isSnapshotWriteConflict(err) {
			return false, nil
		}

		return false, err
	}

	affected, err = result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected > 0, nil
}

func (d *SqlDb) AssignInventorySnapshotResolveTask(projectID int, inventoryID int, taskID int) error {
	// Scoped to the unassigned pending claim: by the time this runs, the claim
	// may already have expired and been taken over by somebody else, and their
	// resolve must not be re-attributed to this task.
	_, err := d.exec(
		"update project__inventory_snapshot set task_id=? "+
			"where project_id=? and inventory_id=? and status=? and task_id is null",
		taskID, projectID, inventoryID, db.InventorySnapshotStatusPending,
	)

	return err
}

func (d *SqlDb) ReleaseInventorySnapshotResolve(projectID int, inventoryID int, message string) error {
	_, err := d.exec(
		"update project__inventory_snapshot set status=?, error=? "+
			"where project_id=? and inventory_id=? and status=? and task_id is null",
		db.InventorySnapshotStatusError, db.TruncateSnapshotError(message),
		projectID, inventoryID, db.InventorySnapshotStatusPending,
	)

	return err
}

func (d *SqlDb) FailInventorySnapshotResolve(projectID int, inventoryID int, taskID int, message string) error {
	// task_id in the predicate is what makes this safe to call from any place
	// that watches tasks die: an expired claim that some later refresh took
	// over belongs to a different task and stays untouched.
	_, err := d.exec(
		"update project__inventory_snapshot set status=?, error=? "+
			"where project_id=? and inventory_id=? and status=? and task_id=?",
		db.InventorySnapshotStatusError, db.TruncateSnapshotError(message),
		projectID, inventoryID, db.InventorySnapshotStatusPending, taskID,
	)

	return err
}
