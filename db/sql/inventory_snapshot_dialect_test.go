package sql

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

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
// The database must be empty: the test applies every migration to it.

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
	assertSnapshotWorksOn(t, newDialectTestStore(t, "mysql", envTestMySQL))
}

func TestInventorySnapshotOnDialectPostgres(t *testing.T) {
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

	require.NoError(t, store.DeleteInventory(inventory.ProjectID, inventory.ID))

	// PrepareQuery rewrites the placeholder for the dialect, which the store
	// does for its own queries and a raw one here has to do too.
	count, err := store.Sql().SelectInt(
		store.PrepareQuery("select count(*) from project__inventory_snapshot where inventory_id = ?"),
		inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}
