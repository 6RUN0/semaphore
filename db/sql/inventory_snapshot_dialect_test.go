package sql

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot schema puts host and group names into composite primary keys,
// and the width an engine allows for an indexed varchar differs between them.
// SQLite accepts anything, so the tests below run the same schema against a
// real MySQL and a real PostgreSQL when one is offered:
//
//	SEMAPHORE_TEST_MYSQL='root:test@127.0.0.1:13306/semaphore_test' \
//	SEMAPHORE_TEST_POSTGRES='postgres:test@127.0.0.1:15432/semaphore_test?sslmode=disable' \
//	go test ./db/sql/ -run TestInventorySnapshotOnDialect -count=1
//
// `task test:dialects` does this across every engine in compose.test.yml, which
// is the list CI runs too. A database that already carries the schema is fine:
// db.Migrate skips what it has applied, and every assertion is scoped to an
// inventory the run creates.

const (
	envTestMySQL    = "SEMAPHORE_TEST_MYSQL"
	envTestPostgres = "SEMAPHORE_TEST_POSTGRES"

	// envTestDialectsRequired turns the skip on a missing database into a
	// failure. CI sets it next to the service containers: a skipped test still
	// reports ok, and an environment that quietly stopped offering the
	// databases would look exactly like one where the dialect tests pass.
	envTestDialectsRequired = "SEMAPHORE_TEST_DIALECTS_REQUIRED"
)

// parseTestDSN reads "user:pass@host:port/dbname?option=value&option=value".
func parseTestDSN(t *testing.T, raw string) *util.DbConfig {
	t.Helper()

	const shape = "expected user:pass@host:port/dbname"

	credentials, rest, ok := strings.Cut(raw, "@")
	require.True(t, ok, shape)

	user, password, ok := strings.Cut(credentials, ":")
	require.True(t, ok, shape)

	host, name, ok := strings.Cut(rest, "/")
	require.True(t, ok, shape)

	name, query, _ := strings.Cut(name, "?")

	options := map[string]string{}

	for _, pair := range strings.Split(query, "&") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}

		options[key] = value
	}

	return &util.DbConfig{
		Hostname: host,
		Username: user,
		Password: password,
		DbName:   name,
		Options:  options,
	}
}

// requireReachableServer fails the single test rather than the whole package
// when the database server is not answering yet. SqlDbConnection.Connect
// panics on an unreachable server, and a panic takes every remaining test in
// the binary with it — which is exactly what happens while a container is
// still starting up.
//
// The probe deliberately omits the database name, matching Connect's own
// fallback: a server that is up but has no database yet is a case Connect
// handles by creating it.
func requireReachableServer(t *testing.T) {
	t.Helper()

	config, err := util.Config.GetDBConfig()
	require.NoError(t, err)

	connectionString, err := config.GetConnectionString(false)
	require.NoError(t, err)

	probe, err := sql.Open(config.Dialect, connectionString)
	require.NoError(t, err)

	defer probe.Close() //nolint:errcheck

	require.NoError(t, probe.Ping(), "the %s server is not reachable", config.Dialect)
}

func newDialectTestStore(t *testing.T, dialect string, envName string) *SqlDb {
	t.Helper()

	raw := os.Getenv(envName)
	if raw == "" {
		if os.Getenv(envTestDialectsRequired) != "" {
			t.Fatalf("%s demands the dialect tests, but %s is not set", envTestDialectsRequired, envName)
		}

		t.Skipf("%s is not set", envName)
	}

	config := parseTestDSN(t, raw)

	// Tests in this package run one after another and share util.Config, so a
	// dialect left behind here would be inherited by everything that follows
	// and does not build a store of its own.
	saved := util.Config
	t.Cleanup(func() { util.Config = saved })

	util.Config = &util.ConfigType{
		Dialect: dialect,
		Log: &util.ConfigLog{
			Events: &util.EventLogType{},
			Tasks:  &util.TaskLogType{},
		},
		Process: &util.ConfigProcess{},
	}

	switch dialect {
	case "mysql":
		util.Config.MySQL = config
	case "postgres":
		util.Config.Postgres = config
	default:
		t.Fatalf("unsupported dialect %s", dialect)
	}

	requireReachableServer(t)

	store := CreateDb(dialect)
	store.Connect()

	require.NoError(t, db.Migrate(store, nil))

	return store
}

func TestInventorySnapshotOnDialectMySQL(t *testing.T) {
	dialectSuitesRan.inventorySnapshotMySQL.Store(true)

	assertSnapshotWorksOn(t, newDialectTestStore(t, "mysql", envTestMySQL))
}

func TestInventorySnapshotOnDialectPostgres(t *testing.T) {
	dialectSuitesRan.inventorySnapshotPostgres.Store(true)

	assertSnapshotWorksOn(t, newDialectTestStore(t, "postgres", envTestPostgres))
}

// assertSnapshotWorksOn repeats the checks the SQLite tests make, so that a
// dialect difference in the schema or in a query shows up as a failure rather
// than only in production.
func assertSnapshotWorksOn(t *testing.T, store *SqlDb) {
	t.Helper()

	inventory := newSnapshotTestInventory(t, store)

	writeSampleSnapshot(t, store, inventory)

	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 4, snapshot.HostCount)

	groups, err := store.GetInventorySnapshotGroups(inventory.ProjectID, inventory.ID, db.InventoryGroupFilter{})
	require.NoError(t, err)
	require.Len(t, groups, 3)
	assert.Equal(t, 4, groups[1].HostCount)

	// Ordering follows the database collation, which differs between engines,
	// so only membership is asserted here.
	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Group: "web"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"web01", "web02", "web_03"}, hostNames(hosts))

	// Ansible host names are case sensitive. A case-insensitive collation on
	// the name columns would make these two collide on the primary key and
	// take the whole snapshot write down with them.
	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-case",
		Created:     time.Now().UTC(),
	}, db.InventorySnapshotContent{
		Hosts: []string{"Host01", "host01"},
		Groups: []db.InventorySnapshotGroupContent{
			{Name: "Web", Hosts: []string{"Host01"}},
			{Name: "web", Hosts: []string{"host01"}},
		},
	}))

	hosts, err = store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Host01", "host01"}, hostNames(hosts))

	hosts, err = store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Group: "web"})
	require.NoError(t, err)
	assert.Equal(t, []string{"host01"}, hostNames(hosts))

	writeSampleSnapshot(t, store, inventory)

	// The LIKE escape clause is written by hand, and its escape character has
	// to behave the same on every engine.
	hosts, err = store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Search: "web_"})
	require.NoError(t, err)
	assert.Equal(t, []string{"web_03"}, hostNames(hosts))

	hosts, err = store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Search: "WEB0"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"web01", "web02"}, hostNames(hosts))

	// Case folding for the search is done by the engine on both sides of the
	// comparison, and the name columns are deliberately binary so that host
	// names stay case sensitive. Whether folding a multi-byte name under a
	// binary collation works at all depends on the connection charset, which
	// only a run against the real engine can settle: MySQL folds byte by byte
	// on a latin1 connection and mangles every non-ASCII name it touches.
	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-non-ascii",
		Created:     time.Now().UTC(),
	}, db.InventorySnapshotContent{
		Hosts: []string{"ÄB01", "Хост01", "web01"},
	}))

	for _, search := range []string{"äb", "ÄB", "хост", "Хост"} {
		hosts, err = store.GetInventorySnapshotHosts(
			inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Search: search})
		require.NoError(t, err, "searching for %q", search)

		expected := "ÄB01"
		if []rune(search)[0] == 'х' || []rune(search)[0] == 'Х' {
			expected = "Хост01"
		}

		assert.Equal(t, []string{expected}, hostNames(hosts),
			"searching for %q must find %q regardless of case", search, expected)
	}

	writeSampleSnapshot(t, store, inventory)

	// A name at the full column width must round-trip rather than be
	// truncated into a different host.
	longName := strings.Repeat("a", db.MaxInventoryHostNameLength-len(".example.com")) + ".example.com"

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-2",
		Created:     time.Now().UTC(),
	}, db.InventorySnapshotContent{
		Hosts:  []string{longName},
		Groups: []db.InventorySnapshotGroupContent{{Name: longName, Hosts: []string{longName}}},
	}))

	hosts, err = store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Group: longName})
	require.NoError(t, err)
	assert.Equal(t, []string{longName}, hostNames(hosts))

	// The claim lifecycle carries this file's least standard SQL: an
	// INSERT ... SELECT ... WHERE EXISTS with no FROM clause, and a
	// "created < ?" comparison whose outcome depends on how the driver
	// serialises time.Time for the dialect. SQLite proves neither. It runs on
	// an inventory of its own, because the task chain it creates references
	// the inventory and would block the deletion checked below.
	resolveInventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, resolveInventory)

	claimed, err := store.ClaimInventorySnapshotResolve(resolveInventory.ProjectID, resolveInventory.ID)
	require.NoError(t, err)
	assert.True(t, claimed, "an ok snapshot must be claimable")

	claimed, err = store.ClaimInventorySnapshotResolve(resolveInventory.ProjectID, resolveInventory.ID)
	require.NoError(t, err)
	assert.False(t, claimed, "a live pending claim must refuse the next one")

	task := newSnapshotResolveTask(t, store, resolveInventory)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(resolveInventory.ProjectID, resolveInventory.ID, task.ID))

	require.NoError(t, store.FailInventorySnapshotResolve(resolveInventory.ProjectID, resolveInventory.ID, task.ID, "dialect check"))

	snapshot, err = store.GetInventorySnapshot(resolveInventory.ProjectID, resolveInventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)

	// A pending claim older than the expiry must be taken over — this is the
	// datetime comparison, and getting it wrong strands the inventory in
	// "resolving" forever.
	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: resolveInventory.ID,
		ProjectID:   resolveInventory.ProjectID,
		Status:      db.InventorySnapshotStatusPending,
		Created:     time.Now().UTC().Add(-db.MaxInventoryResolveAge - time.Minute),
	}, db.InventorySnapshotContent{}))

	claimed, err = store.ClaimInventorySnapshotResolve(resolveInventory.ProjectID, resolveInventory.ID)
	require.NoError(t, err)
	assert.True(t, claimed, "an expired pending claim must be taken over")

	// Storing a resolved list is gated on still owning the claim, and that gate
	// reads RowsAffected. MySQL counts changed rows rather than matched ones,
	// so a gate written as a no-op statement reports zero there and silently
	// drops every resolved host list while passing on SQLite. Only a run
	// against the real dialect can tell the two apart.
	resolveTask := newSnapshotResolveTask(t, store, resolveInventory)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(
		resolveInventory.ProjectID, resolveInventory.ID, resolveTask.ID))

	written, err := store.SetResolvedInventorySnapshot(db.InventorySnapshot{
		InventoryID: resolveInventory.ID,
		ProjectID:   resolveInventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "resolved-hash",
		Created:     time.Now().UTC().Truncate(time.Second),
		TaskID:      &resolveTask.ID,
	}, sampleSnapshotContent(), resolveTask.ID)
	require.NoError(t, err)
	assert.True(t, written, "the task holding the claim must be able to store its result")

	snapshot, err = store.GetInventorySnapshot(resolveInventory.ProjectID, resolveInventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)

	// The claim is settled now, so the same report arriving twice must not
	// replace the snapshot a fresher resolve may already have written.
	written, err = store.SetResolvedInventorySnapshot(db.InventorySnapshot{
		InventoryID: resolveInventory.ID,
		ProjectID:   resolveInventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "stale-hash",
		Created:     time.Now().UTC().Truncate(time.Second),
		TaskID:      &resolveTask.ID,
	}, sampleSnapshotContent(), resolveTask.ID)
	require.NoError(t, err)
	assert.False(t, written, "a report whose claim is already settled must be refused")

	assertClaimRaceHasOneWinner(t, store)
	assertLongErrorFitsTheColumn(t, store)
	assertConcurrentWritesConverge(t, store)

	require.NoError(t, store.DeleteInventory(inventory.ProjectID, inventory.ID))

	// PrepareQuery rewrites the placeholder for the dialect, which the store
	// does for its own queries and a raw one here has to do too.
	count, err := store.Sql().SelectInt(
		store.PrepareQuery("select count(*) from project__inventory_snapshot where inventory_id = ?"),
		inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// assertClaimRaceHasOneWinner covers the claim of an inventory that has never
// been synchronised, which is the one path that inserts the snapshot row:
// UPDATE, SELECT and INSERT are three statements outside a transaction, so two
// claimants can both reach the insert and the loser gets a duplicate key back.
//
// It belongs in the dialect run rather than beside the SQLite tests: the SQLite
// pool holds a single connection, so writes there serialise in the driver and
// the race cannot happen at all.
func assertClaimRaceHasOneWinner(t *testing.T, store *SqlDb) {
	t.Helper()

	inventory := newSnapshotTestInventory(t, store)

	const claimants = 8

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		errs    []error
	)

	for i := 0; i < claimants; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
				return
			}

			if claimed {
				winners++
			}
		}()
	}

	close(start)
	wg.Wait()

	assert.Empty(t, errs, "losing the race must read as a taken claim, not as an error")
	assert.Equal(t, 1, winners, "exactly one claimant may win")

	// The race above cannot be forced to overlap on demand, and the whole fix
	// rests on the engine's duplicate-key error being recognised as a lost
	// race rather than reported as a failure. Provoke that error directly:
	// isSnapshotWriteConflict reads driver codes, which is exactly what a
	// SQLite-only test cannot exercise.
	_, err := store.exec(
		"insert into project__inventory_snapshot "+
			"(inventory_id, project_id, status, error, source_hash, host_count, created) "+
			"values (?, ?, ?, '', '', 0, ?)",
		inventory.ID, inventory.ProjectID, db.InventorySnapshotStatusPending, time.Now().UTC())

	require.Error(t, err, "the row is already there, so a second insert must fail")
	assert.True(t, isSnapshotWriteConflict(err),
		"the engine's duplicate-key error must read as a lost race: %v", err)
}

// assertLongErrorFitsTheColumn pins the width of the error column against what
// the callers may hand the store.
//
// SQLite ignores a varchar's declared length, so a message over the limit is
// stored whole there and every SQLite test passes without the truncation. MySQL
// in strict mode answers "Data too long for column 'error'" and PostgreSQL
// "value too long for type character varying(1000)" — and since this write is
// how a failed resolve settles its claim, the rejection would leave the claim
// pending until it expires.
func assertLongErrorFitsTheColumn(t *testing.T, store *SqlDb) {
	t.Helper()

	inventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, inventory)

	// Multi-byte, so that a truncation cutting bytes rather than runes leaves
	// invalid UTF-8 behind — which PostgreSQL rejects outright.
	long := strings.Repeat("ы", db.MaxInventorySnapshotErrorLength+50)

	claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)

	task := newSnapshotResolveTask(t, store, inventory)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

	require.NoError(t, store.FailInventorySnapshotResolve(inventory.ProjectID, inventory.ID, task.ID, long),
		"a long failure message must settle the claim, not be rejected by the engine")

	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Equal(t, db.MaxInventorySnapshotErrorLength, utf8.RuneCountInString(snapshot.Error))

	// The same message on the insert path, which writes the column too.
	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusError,
		Error:       long,
		SourceHash:  "hash-long-error",
		Created:     time.Now().UTC(),
	}, db.InventorySnapshotContent{}))

	snapshot, err = store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.MaxInventorySnapshotErrorLength, utf8.RuneCountInString(snapshot.Error))
}

// assertConcurrentWritesConverge is the concurrent rewrite of one snapshot: a
// whole-list replacement is a delete of four tables and an insert into four
// more, and two of those running at once deadlock on MySQL and serialisation-
// fail on PostgreSQL. SetInventorySnapshot answers that with a retry loop, and
// the loop only ever runs when the engine reports the conflict.
//
// The same eight goroutines against SQLite prove nothing: its pool holds one
// connection, so the writes queue up in the driver and no retry ever happens —
// that test stays green with the retry loop deleted outright.
func assertConcurrentWritesConverge(t *testing.T, store *SqlDb) {
	t.Helper()

	inventory := newSnapshotTestInventory(t, store)

	const writers = 8

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
		errs  = make([]error, writers)
	)

	for i := 0; i < writers; i++ {
		wg.Add(1)

		go func(slot int) {
			defer wg.Done()

			<-start

			errs[slot] = store.SetInventorySnapshot(db.InventorySnapshot{
				InventoryID: inventory.ID,
				ProjectID:   inventory.ProjectID,
				Status:      db.InventorySnapshotStatusOK,
				SourceHash:  fmt.Sprintf("hash-%d", slot),
				Created:     time.Now().UTC(),
			}, sampleSnapshotContent())
		}(i)
	}

	close(start)
	wg.Wait()

	for slot, err := range errs {
		assert.NoError(t, err, "writer %d lost a conflict the retry loop should have absorbed", slot)
	}

	// Whichever writer landed last, the row and its host rows have to describe
	// the same snapshot: the replacement is transactional, so a torn result
	// would mean the transaction is not doing its job.
	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)

	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Len(t, hosts, snapshot.HostCount)
}
