package sql

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/tz"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSnapshotTestStore brings up an in-memory SQLite database with every
// migration applied, which is what exercises the snapshot schema itself.
func newSnapshotTestStore(t *testing.T) *SqlDb {
	t.Helper()

	// util.Config is package state shared by every test in this package, and a
	// helper that leaves its own value behind makes whatever runs next depend on
	// the order tests happen to run in.
	previous := util.Config
	t.Cleanup(func() { util.Config = previous })

	util.Config = &util.ConfigType{
		SQLite: &util.DbConfig{
			Hostname: ":memory:",
		},
		Dialect: "sqlite",
		Log: &util.ConfigLog{
			Events: &util.EventLogType{},
			Tasks:  &util.TaskLogType{},
		},
		Process: &util.ConfigProcess{},
	}

	store := CreateDb(util.DbDriverSQLite)
	store.Connect()
	require.NoError(t, db.Migrate(store, nil))

	return store
}

func newSnapshotTestInventory(t *testing.T, store *SqlDb) db.Inventory {
	t.Helper()

	project, err := store.CreateProject(db.Project{Name: "snapshot-test"})
	require.NoError(t, err)

	inventory, err := store.CreateInventory(db.Inventory{
		ProjectID: project.ID,
		Name:      "hosts",
		Type:      db.InventoryStatic,
		Inventory: "[web]\nweb01\n",
	})
	require.NoError(t, err)

	return inventory
}

func sampleSnapshotContent() db.InventorySnapshotContent {
	return db.InventorySnapshotContent{
		Hosts: []string{"db01", "web01", "web02", "web_03"},
		Groups: []db.InventorySnapshotGroupContent{
			{Name: "db", Hosts: []string{"db01"}},
			{Name: "prod", Hosts: []string{"db01", "web01", "web02", "web_03"}},
			{Name: "web", Hosts: []string{"web01", "web02", "web_03"}},
		},
	}
}

func writeSampleSnapshot(t *testing.T, store *SqlDb, inventory db.Inventory) {
	t.Helper()

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-1",
		Created:     time.Now().UTC().Truncate(time.Second),
	}, sampleSnapshotContent()))
}

func hostNames(hosts []db.InventorySnapshotHost) []string {
	names := make([]string, 0, len(hosts))

	for _, host := range hosts {
		names = append(names, host.Name)
	}

	return names
}

func TestInventorySnapshotRoundTrip(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	writeSampleSnapshot(t, store, inventory)

	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, "hash-1", snapshot.SourceHash)
	assert.Equal(t, 4, snapshot.HostCount)
	assert.Empty(t, snapshot.Error)
	assert.Nil(t, snapshot.TaskID)

	groups, err := store.GetInventorySnapshotGroups(inventory.ProjectID, inventory.ID, db.InventoryGroupFilter{})
	require.NoError(t, err)
	require.Len(t, groups, 3)
	assert.Equal(t, "db", groups[0].Name)
	assert.Equal(t, 1, groups[0].HostCount)
	assert.Equal(t, "prod", groups[1].Name)
	assert.Equal(t, 4, groups[1].HostCount)

	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"db01", "web01", "web02", "web_03"}, hostNames(hosts))
}

func TestInventorySnapshotNotFound(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	_, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)

	assert.ErrorIs(t, err, db.ErrNotFound)
}

// A snapshot belongs to a project, and asking for it from another one must not
// serve it even when the inventory id is guessed correctly.
func TestInventorySnapshotIsScopedToProject(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, inventory)

	other, err := store.CreateProject(db.Project{Name: "other"})
	require.NoError(t, err)

	_, err = store.GetInventorySnapshot(other.ID, inventory.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)

	hosts, err := store.GetInventorySnapshotHosts(other.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Empty(t, hosts)

	groups, err := store.GetInventorySnapshotGroups(other.ID, inventory.ID, db.InventoryGroupFilter{})
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestInventorySnapshotHostFilters(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, inventory)

	tests := []struct {
		name     string
		filter   db.InventoryHostFilter
		expected []string
	}{
		{
			name:     "by group",
			filter:   db.InventoryHostFilter{Group: "web"},
			expected: []string{"web01", "web02", "web_03"},
		},
		{
			name:     "by search",
			filter:   db.InventoryHostFilter{Search: "web0"},
			expected: []string{"web01", "web02"},
		},
		{
			name:     "search is case insensitive",
			filter:   db.InventoryHostFilter{Search: "WEB0"},
			expected: []string{"web01", "web02"},
		},
		{
			// Without escaping, the underscore of a LIKE pattern matches any
			// character, so this would also return web01 and web02.
			name:     "underscore is a literal in search",
			filter:   db.InventoryHostFilter{Search: "web_"},
			expected: []string{"web_03"},
		},
		{
			name:     "percent is a literal in search",
			filter:   db.InventoryHostFilter{Search: "%"},
			expected: []string{},
		},
		{
			name:     "group and search combine",
			filter:   db.InventoryHostFilter{Group: "web", Search: "02"},
			expected: []string{"web02"},
		},
		{
			name:     "unknown group matches nothing",
			filter:   db.InventoryHostFilter{Group: "staging"},
			expected: []string{},
		},
		{
			name:     "limit and offset",
			filter:   db.InventoryHostFilter{Limit: 2, Offset: 1},
			expected: []string{"web01", "web02"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, tt.filter)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, hostNames(hosts))
		})
	}
}

// A caller asking for more than the cap must not be able to widen the page.
func TestInventorySnapshotHostsClampPageSize(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	content := db.InventorySnapshotContent{
		Hosts:  make([]string, 0, db.MaxInventoryHostPageSize+10),
		Groups: []db.InventorySnapshotGroupContent{},
	}

	for i := 0; i < db.MaxInventoryHostPageSize+10; i++ {
		content.Hosts = append(content.Hosts, fmtHost(i))
	}

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-1",
		Created:     time.Now().UTC(),
	}, content))

	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Limit: 10000})

	require.NoError(t, err)
	assert.Len(t, hosts, db.MaxInventoryHostPageSize)
}

// The batching of the insert must not lose or duplicate rows at a batch edge.
func TestInventorySnapshotWritesMoreRowsThanOneBatch(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	const count = inventorySnapshotInsertBatch*2 + 7

	content := db.InventorySnapshotContent{
		Hosts:  make([]string, 0, count),
		Groups: []db.InventorySnapshotGroupContent{{Name: "web", Hosts: make([]string, 0, count)}},
	}

	for i := 0; i < count; i++ {
		content.Hosts = append(content.Hosts, fmtHost(i))
		content.Groups[0].Hosts = append(content.Groups[0].Hosts, fmtHost(i))
	}

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-1",
		Created:     time.Now().UTC(),
	}, content))

	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, count, snapshot.HostCount)

	stored, err := store.Sql().SelectInt(
		"select count(*) from project__inventory_snapshot_host_group where inventory_id = ?", inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(count), stored)
}

// Replacing must leave nothing of the previous snapshot behind.
func TestInventorySnapshotReplacesPreviousContent(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, inventory)

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusError,
		Error:       "broken range",
		SourceHash:  "hash-2",
		Created:     time.Now().UTC(),
	}, db.InventorySnapshotContent{
		Hosts:  []string{"only01"},
		Groups: []db.InventorySnapshotGroupContent{},
	}))

	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Equal(t, "broken range", snapshot.Error)
	assert.Equal(t, "hash-2", snapshot.SourceHash)
	assert.Equal(t, 1, snapshot.HostCount)

	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"only01"}, hostNames(hosts))

	groups, err := store.GetInventorySnapshotGroups(inventory.ProjectID, inventory.ID, db.InventoryGroupFilter{})
	require.NoError(t, err)
	assert.Empty(t, groups)

	memberships, err := store.Sql().SelectInt(
		"select count(*) from project__inventory_snapshot_host_group where inventory_id = ?", inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), memberships)
}

// Deleting an inventory must take its snapshot with it rather than leaving
// rows that a future inventory could inherit by reusing the id.
func TestDeletingInventoryRemovesSnapshot(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, inventory)

	require.NoError(t, store.DeleteInventory(inventory.ProjectID, inventory.ID))

	count, err := store.Sql().SelectInt(
		"select count(*) from project__inventory_snapshot where inventory_id = ?", inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// fmtHost pads the index so that the generated names sort the same way the
// query orders them.
func fmtHost(i int) string {
	return fmt.Sprintf("host%04d", i)
}

// newSnapshotResolveTask builds a task for the inventory. The snapshot row
// points at the task that is resolving it, so the claim cannot be tested with
// an id that does not exist.
func newSnapshotResolveTask(t *testing.T, store *SqlDb, inventory db.Inventory) db.Task {
	t.Helper()

	key, err := store.CreateAccessKey(db.AccessKey{
		ProjectID: &inventory.ProjectID,
		Type:      db.AccessKeyNone,
	})
	require.NoError(t, err)

	repository, err := store.CreateRepository(db.Repository{
		ProjectID: inventory.ProjectID,
		SSHKeyID:  key.ID,
		Name:      "repo",
		GitURL:    "git@example.com:test/test",
		GitBranch: "main",
	})
	require.NoError(t, err)

	template, err := store.CreateTemplate(db.Template{
		Name:         "resolve",
		Playbook:     "test.yml",
		ProjectID:    inventory.ProjectID,
		RepositoryID: repository.ID,
		InventoryID:  &inventory.ID,
	})
	require.NoError(t, err)

	// Created is set explicitly: the store's PreInsert keeps a zero time as
	// is, and strict MySQL rejects the resulting '0000-00-00'. Production
	// always stamps it in AddTask before the row is written.
	task, err := store.CreateTask(db.Task{
		ProjectID:   inventory.ProjectID,
		TemplateID:  template.ID,
		InventoryID: &inventory.ID,
		Created:     tz.Now(),
	}, 0)
	require.NoError(t, err)

	return task
}

// The claim is what keeps a second refresh from running somebody's inventory
// script while the first is still running, so these exercise it against a real
// database rather than a mock.
func TestClaimInventorySnapshotResolve(t *testing.T) {
	t.Run("claims an inventory that was never synchronised", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)

		claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)

		require.NoError(t, err)
		assert.True(t, claimed)

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status)
		assert.Nil(t, snapshot.TaskID, "the claim is taken before its task exists")

		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

		snapshot, err = store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		require.NotNil(t, snapshot.TaskID)
		assert.Equal(t, task.ID, *snapshot.TaskID)
	})

	t.Run("claims an inventory that already has a host list", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)

		require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusOK,
			HostCount:   4,
			Created:     tz.Now(),
		}, sampleSnapshotContent()))

		claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)

		require.NoError(t, err)
		assert.True(t, claimed)
	})

	t.Run("refuses while a resolve is in flight", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)

		first, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		require.True(t, first)

		second, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)

		require.NoError(t, err)
		assert.False(t, second, "a resolve is already running")
	})

	// A server that dies mid-resolve leaves the row pending and the task gone.
	// Refusing every later claim would strand the inventory forever.
	t.Run("takes over a claim that has been held too long", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)

		require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusPending,
			Created:     tz.Now().Add(-db.MaxInventoryResolveAge - time.Minute),
		}, db.InventorySnapshotContent{}))

		claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)

		require.NoError(t, err)
		assert.True(t, claimed)
	})

	// Claiming clears whatever the previous state said, so a stale error or
	// fingerprint cannot be read as belonging to the resolve now running. The
	// host count is the exception: the host rows themselves stay, so a zero
	// there would contradict a listing taken at the same moment.
	t.Run("clears the previous state", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)

		require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusError,
			Error:       "broken range",
			HostCount:   9,
			SourceHash:  "deadbeef",
			Created:     tz.Now(),
		}, sampleSnapshotContent()))

		_, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)

		assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status)
		assert.Empty(t, snapshot.Error)
		assert.Empty(t, snapshot.SourceHash)

		hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
		require.NoError(t, err)
		assert.Equal(t, len(hosts), snapshot.HostCount,
			"the count must describe the host rows the claim left in place")
	})

	// The foreign keys do not catch a caller pairing a real project with
	// another project's inventory: both ids exist and only the pair is wrong.
	t.Run("refuses an inventory that belongs to another project", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)

		other, err := store.CreateProject(db.Project{Name: "another"})
		require.NoError(t, err)

		claimed, err := store.ClaimInventorySnapshotResolve(other.ID, inventory.ID)

		require.NoError(t, err)
		assert.False(t, claimed, "the pair does not exist, so nothing is claimed")

		_, err = store.GetInventorySnapshot(other.ID, inventory.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "no row may be written for the wrong project")

		_, err = store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "the inventory's own project must be untouched")
	})
}

// A claim is settled by exactly one owner: released by the dispatcher that
// could not create its task, or failed by the task it was assigned to. Anybody
// else's attempt must fall through to nothing.
func TestInventorySnapshotResolveSettlement(t *testing.T) {
	claim := func(t *testing.T, store *SqlDb, inventory db.Inventory) {
		t.Helper()

		claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		require.True(t, claimed)
	}

	t.Run("release gives up an unassigned claim", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		claim(t, store, inventory)

		require.NoError(t, store.ReleaseInventorySnapshotResolve(inventory.ProjectID, inventory.ID, "could not queue the task"))

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
		assert.Equal(t, "could not queue the task", snapshot.Error)
	})

	t.Run("release leaves an assigned claim alone", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)

		claim(t, store, inventory)
		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

		require.NoError(t, store.ReleaseInventorySnapshotResolve(inventory.ProjectID, inventory.ID, "not yours"))

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status, "an assigned claim belongs to its task")
	})

	t.Run("fail settles the claim of its own task", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)

		claim(t, store, inventory)
		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

		require.NoError(t, store.FailInventorySnapshotResolve(inventory.ProjectID, inventory.ID, task.ID, "the resolve task ended with status error"))

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
		assert.Equal(t, "the resolve task ended with status error", snapshot.Error)
	})

	t.Run("fail ignores another task's claim", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)

		claim(t, store, inventory)
		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

		require.NoError(t, store.FailInventorySnapshotResolve(inventory.ProjectID, inventory.ID, task.ID+1000, "stale task reporting in"))

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status)
	})

	t.Run("fail ignores a snapshot that is not pending", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)
		writeSampleSnapshot(t, store, inventory)

		require.NoError(t, store.FailInventorySnapshotResolve(inventory.ProjectID, inventory.ID, task.ID, "too late"))

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status, "a finished snapshot is not a claim")
	})

	// The assign must not re-attribute a claim that expired and was taken over
	// by a later refresh.
	t.Run("assign skips a claim it no longer owns", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)
		other := newSnapshotResolveTask(t, store, inventory)

		claim(t, store, inventory)
		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, other.ID))

		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		require.NotNil(t, snapshot.TaskID)
		assert.Equal(t, other.ID, *snapshot.TaskID, "the first assignment wins")
	})
}

// SetResolvedInventorySnapshot is the write path a runner's progress report
// takes; only the task currently holding the pending claim may use it, the
// same ownership scoping FailInventorySnapshotResolve enforces.
func TestSetResolvedInventorySnapshot(t *testing.T) {
	claim := func(t *testing.T, store *SqlDb, inventory db.Inventory) {
		t.Helper()

		claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		require.True(t, claimed)
	}

	t.Run("writes when the task owns the pending claim", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)

		claim(t, store, inventory)
		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID))

		written, err := store.SetResolvedInventorySnapshot(db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusOK,
			TaskID:      &task.ID,
			Created:     tz.Now(),
		}, db.InventorySnapshotContent{Hosts: []string{"cloud01"}}, task.ID)

		require.NoError(t, err)
		assert.True(t, written)

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)

		hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
		require.NoError(t, err)
		assert.Equal(t, []string{"cloud01"}, hostNames(hosts))
	})

	// A claim that expired and was handed to a fresher task must not be
	// overwritten by the loser's late report.
	t.Run("refuses a claim it does not own", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)
		other := newSnapshotResolveTask(t, store, inventory)

		claim(t, store, inventory)
		require.NoError(t, store.AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, other.ID))

		written, err := store.SetResolvedInventorySnapshot(db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusOK,
			TaskID:      &task.ID,
			Created:     tz.Now(),
		}, db.InventorySnapshotContent{Hosts: []string{"stale01"}}, task.ID)

		require.NoError(t, err)
		assert.False(t, written, "the claim belongs to another task")

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status, "the active claim must be untouched")

		hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
		require.NoError(t, err)
		assert.Empty(t, hosts)
	})

	// A report that arrives after the claim already settled (or was never a
	// resolve claim at all) must not clobber whatever is current now.
	t.Run("refuses a claim that is not pending", func(t *testing.T) {
		store := newSnapshotTestStore(t)
		inventory := newSnapshotTestInventory(t, store)
		task := newSnapshotResolveTask(t, store, inventory)
		writeSampleSnapshot(t, store, inventory)

		written, err := store.SetResolvedInventorySnapshot(db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusOK,
			TaskID:      &task.ID,
			Created:     tz.Now(),
		}, db.InventorySnapshotContent{Hosts: []string{"late01"}}, task.ID)

		require.NoError(t, err)
		assert.False(t, written, "a late report of a resolve that already settled must not overwrite it")

		snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
		require.NoError(t, err)
		assert.Equal(t, 4, snapshot.HostCount, "the previously written snapshot must be untouched")
	})
}

// Two writers replacing the same inventory's snapshot at once — two nodes in
// HA, or two requests on one — must both come back clean: the database answers
// the collision with a deadlock or duplicate key, and the store retries
// because every writer derives the same rows.
//
// What this proves here is that the write path is safe to call from several
// goroutines, not that the retry loop works: the SQLite pool holds a single
// connection, so these writes serialise in the driver and no conflict is ever
// raised. The retry loop itself is covered by assertConcurrentWritesConverge in
// the dialect run, against engines that really do collide.
func TestConcurrentSetInventorySnapshot(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	var wg sync.WaitGroup
	errs := make([]error, 8)

	for i := range errs {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			errs[slot] = store.SetInventorySnapshot(db.InventorySnapshot{
				InventoryID: inventory.ID,
				ProjectID:   inventory.ProjectID,
				Status:      db.InventorySnapshotStatusOK,
				SourceHash:  "hash-1",
				Created:     tz.Now(),
			}, sampleSnapshotContent())
		}(i)
	}
	wg.Wait()

	for slot, err := range errs {
		assert.NoError(t, err, "writer %d", slot)
	}

	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"db01", "web01", "web02", "web_03"}, hostNames(hosts),
		"the surviving snapshot must be one writer's complete content")
}

// SQLite serialises the concurrent-write test above at the driver level, so
// the conflict classifier is pinned directly: these are the literal verdicts
// MySQL, PostgreSQL and SQLite hand out when two replace transactions collide.
func TestIsSnapshotWriteConflict(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		conflict bool
	}{
		{"mysql deadlock", "Error 1213: Deadlock found when trying to get lock; try restarting transaction", true},
		{"postgres deadlock", "pq: deadlock detected", true},
		{"sqlite busy", "database is locked", true},
		{"sqlite table busy", "database table is locked", true},
		{"mysql duplicate", "Error 1062: Duplicate entry '7' for key 'PRIMARY'", true},
		{"postgres duplicate", "pq: duplicate key value violates unique constraint", true},
		{"sqlite duplicate", "UNIQUE constraint failed: project__inventory_snapshot.inventory_id", true},
		{"foreign key is not a conflict", "FOREIGN KEY constraint failed", false},
		{"disk full is not a conflict", "database or disk is full", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.conflict, isSnapshotWriteConflict(fmt.Errorf("%s", tt.message)))
		})
	}
}

// The write path must refuse a caller pairing one project with another's
// inventory before anything is deleted: the deletes go by inventory id alone,
// so without the guard the victim's snapshot would be wiped and rewritten
// under the caller's project.
func TestSetInventorySnapshotRefusesAForeignPair(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)
	writeSampleSnapshot(t, store, inventory)

	other, err := store.CreateProject(db.Project{Name: "another"})
	require.NoError(t, err)

	err = store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   other.ID,
		Status:      db.InventorySnapshotStatusOK,
		Created:     tz.Now(),
	}, db.InventorySnapshotContent{Hosts: []string{"stolen"}})

	assert.ErrorIs(t, err, db.ErrNotFound)

	snapshot, err := store.GetInventorySnapshot(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, snapshot.HostCount, "the victim's snapshot must be untouched")

	hosts, err := store.GetInventorySnapshotHosts(inventory.ProjectID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"db01", "web01", "web02", "web_03"}, hostNames(hosts))
}

// A host name folds by whatever rules the engine has, and the pattern has to
// fold by the same ones. Lowering the pattern in Go instead folded it by
// Unicode while SQLite's lower() folds only ASCII, so "ÄB" became "äb" and
// stopped matching "ÄB01".
func TestInventorySnapshotHostsSearchHandlesNonASCII(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-1",
		Created:     tz.Now(),
	}, db.InventorySnapshotContent{
		Hosts:  []string{"ÄB01", "web01", "хост01"},
		Groups: []db.InventorySnapshotGroupContent{},
	}))

	tests := []struct {
		name     string
		search   string
		expected []string
	}{
		{"non-ascii name matched as written", "ÄB", []string{"ÄB01"}},
		{"non-latin name matched as written", "хост", []string{"хост01"}},
		{"ascii stays case insensitive", "WEB0", []string{"web01"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hosts, err := store.GetInventorySnapshotHosts(
				inventory.ProjectID, inventory.ID, db.InventoryHostFilter{Search: tt.search})

			require.NoError(t, err)
			assert.Equal(t, tt.expected, hostNames(hosts))
		})
	}
}

// Groups are capped as high as hosts are, and the reason hosts are served a
// page at a time applies to them unchanged.
func TestInventorySnapshotGroupsArePaged(t *testing.T) {
	store := newSnapshotTestStore(t)
	inventory := newSnapshotTestInventory(t, store)

	groups := make([]db.InventorySnapshotGroupContent, db.MaxInventoryGroupPageSize+10)
	for i := range groups {
		groups[i] = db.InventorySnapshotGroupContent{Name: fmt.Sprintf("g%04d", i)}
	}

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "hash-1",
		Created:     tz.Now(),
	}, db.InventorySnapshotContent{Hosts: []string{}, Groups: groups}))

	t.Run("an unpaged caller gets the first page", func(t *testing.T) {
		page, err := store.GetInventorySnapshotGroups(
			inventory.ProjectID, inventory.ID, db.InventoryGroupFilter{})

		require.NoError(t, err)
		assert.Len(t, page, db.MaxInventoryGroupPageSize)
		assert.Equal(t, "g0000", page[0].Name)
	})

	t.Run("offset walks the rest", func(t *testing.T) {
		page, err := store.GetInventorySnapshotGroups(
			inventory.ProjectID, inventory.ID,
			db.InventoryGroupFilter{Limit: 5, Offset: db.MaxInventoryGroupPageSize})

		require.NoError(t, err)
		require.Len(t, page, 5)
		assert.Equal(t, fmt.Sprintf("g%04d", db.MaxInventoryGroupPageSize), page[0].Name)
	})

	// Asking for more than the cap must not widen the page.
	t.Run("the page size is clamped", func(t *testing.T) {
		page, err := store.GetInventorySnapshotGroups(
			inventory.ProjectID, inventory.ID,
			db.InventoryGroupFilter{Limit: db.MaxInventoryGroupPageSize * 10})

		require.NoError(t, err)
		assert.Len(t, page, db.MaxInventoryGroupPageSize)
	})
}

// MySQL renders its error messages in the server's lc_messages, so a classifier
// reading them by substring answers "not a conflict" on any installation not
// running an English locale, which switches the retry loop off exactly where it
// is needed. The numeric codes do not move.
func TestIsSnapshotWriteConflictReadsDriverCodes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		conflict bool
	}{
		{"mysql deadlock in another language", &mysql.MySQLError{Number: 1213, Message: "Взаимоблокировка"}, true},
		{"mysql duplicate entry", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, true},
		{"mysql lock wait timeout", &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}, true},
		// Observed on MariaDB 12.3 in the dialect run, where MySQL and MariaDB
		// 10.11 report the same collision as a deadlock. Left out, eight
		// concurrent writers of one snapshot produce failed writes rather than
		// retries.
		{"mariadb record changed", &mysql.MySQLError{
			Number:  1020,
			Message: "Record has changed since last read in table 'project__inventory_snapshot_host_group'",
		}, true},
		{"mysql unrelated failure", &mysql.MySQLError{Number: 1114, Message: "The table is full"}, false},
		{"postgres deadlock", &pq.Error{Code: "40P01"}, true},
		{"postgres unique violation", &pq.Error{Code: "23505"}, true},
		{"postgres foreign key violation", &pq.Error{Code: "23503"}, false},
		{"wrapped mysql deadlock", fmt.Errorf("saving the snapshot: %w", &mysql.MySQLError{Number: 1213}), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.conflict, isSnapshotWriteConflict(tt.err))
		})
	}
}
