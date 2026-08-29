package tasks

import (
	"reflect"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/pkg/tz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A runner reports a resolved host list over the network, so what it can affect
// has to be decided here rather than there. These pin that the report lands on
// the inventory the task named, and nowhere else.

func newInventoryTaskPool(t *testing.T) (*TaskPool, *sql.SqlDb) {
	t.Helper()

	store := sql.InitConfigCreateTestStore()

	pool := CreateTaskPool(
		store,
		&MemoryTaskStateStore{},
		nil,
		&InventoryServiceMock{},
		&EncryptionServiceMock{},
		&KeyInstallerMock{},
		&mockLogWriteService{},
		nil,
		nil,
	)

	return &pool, store
}

// newResolveTask builds the whole chain a task needs to exist: the snapshot
// row points at the task that produced it, so without a real task the write is
// refused by the foreign key.
func newResolveTask(t *testing.T, store *sql.SqlDb) (db.Project, db.Inventory, db.Task) {
	t.Helper()

	project, err := store.CreateProject(db.Project{Name: "resolve"})
	require.NoError(t, err)

	key, err := store.CreateAccessKey(db.AccessKey{
		ProjectID: &project.ID,
		Type:      db.AccessKeyNone,
	})
	require.NoError(t, err)

	repository, err := store.CreateRepository(db.Repository{
		ProjectID: project.ID,
		SSHKeyID:  key.ID,
		Name:      "repo",
		GitURL:    "git@example.com:test/test",
		GitBranch: "main",
	})
	require.NoError(t, err)

	inventory, err := store.CreateInventory(db.Inventory{
		ProjectID: project.ID,
		Name:      "dynamic",
		Type:      db.InventoryFile,
		Inventory: "dynamic.py",
	})
	require.NoError(t, err)

	template, err := store.CreateTemplate(db.Template{
		Name:         "resolve",
		Playbook:     "test.yml",
		ProjectID:    project.ID,
		RepositoryID: repository.ID,
		InventoryID:  &inventory.ID,
	})
	require.NoError(t, err)

	task, err := store.CreateTask(db.Task{
		ProjectID:   project.ID,
		TemplateID:  template.ID,
		InventoryID: &inventory.ID,
		Params: db.MapStringAnyField{
			"resolve_inventory_only": true,
		},
	}, 0)
	require.NoError(t, err)

	return project, inventory, task
}

func TestSetResolvedInventoryStoresAgainstTheTasksInventory(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	// The dispatcher claims and assigns the task before it ever runs; the
	// write below is scoped to that claim's ownership.
	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	runner := &TaskRunner{pool: pool, Task: task, Inventory: inventory}

	runner.SetResolvedInventory(
		[]string{"i-0a1", "i-0b2"},
		map[string][]string{"tag_role_web": {"i-0a1"}},
	)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 2, snapshot.HostCount)
	require.NotNil(t, snapshot.TaskID)
	assert.Equal(t, task.ID, *snapshot.TaskID)

	hosts, err := store.GetInventorySnapshotHosts(project.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)

	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}

	assert.ElementsMatch(t, []string{"i-0a1", "i-0b2"}, names)
}

// A task that resolved nothing has no inventory to store against. Reporting one
// anyway must be dropped rather than land somewhere arbitrary.
func TestSetResolvedInventoryIgnoresTaskWithoutInventory(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	withoutInventory := task
	withoutInventory.InventoryID = nil

	runner := &TaskRunner{pool: pool, Task: withoutInventory, Inventory: inventory}

	runner.SetResolvedInventory([]string{"i-0a1"}, nil)

	_, err := store.GetInventorySnapshot(project.ID, inventory.ID)

	assert.ErrorIs(t, err, db.ErrNotFound, "nothing may be written for a task that names no inventory")
}

// A refusal is the runner's problem, not the task's: the play it ran either
// succeeded or failed on its own terms.
func TestSetResolvedInventorySurvivesRefusedReport(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	runner := &TaskRunner{pool: pool, Task: task, Inventory: inventory}

	oversized := make([]string, db.MaxInventorySnapshotHosts+1)
	for i := range oversized {
		oversized[i] = "host"
	}

	assert.NotPanics(t, func() {
		runner.SetResolvedInventory(oversized, nil)
	})

	_, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// A1: IsInventoryResolve is what decides whether a report is even considered,
// independent of whatever claim the task's id happens to hold. Params are the
// server's own record of what the task is, so this is the check that has the
// final word regardless of what a runner sends.
func TestSetResolvedInventoryIgnoresNonResolveTask(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	// The task row itself says this is not a resolve, even though it holds the
	// pending claim: IsInventoryResolve must decide, not the mere fact that the
	// task id matches a claim owner.
	notAResolve := task
	notAResolve.Params = db.MapStringAnyField{}

	runner := &TaskRunner{pool: pool, Task: notAResolve, Inventory: inventory}
	runner.SetResolvedInventory([]string{"attacker01"}, map[string][]string{"pwned": {"attacker01"}})

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status, "a non-resolve report must not settle or write the claim")

	hosts, err := store.GetInventorySnapshotHosts(project.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Empty(t, hosts)
}

// The attack this pins: a runner dispatched an ordinary playbook attaches a
// host list to its progress report, targeting the inventory its own task
// uses -- including a static inventory the server parses itself, which has no
// pending claim of its own to lean on.
func TestSetResolvedInventoryDoesNotOverwriteAStaticSnapshot(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   project.ID,
		Status:      db.InventorySnapshotStatusOK,
		SourceHash:  "static-hash",
		Created:     tz.Now(),
	}, db.InventorySnapshotContent{Hosts: []string{"real01"}}))

	ordinary := task
	ordinary.Params = db.MapStringAnyField{}

	runner := &TaskRunner{pool: pool, Task: ordinary, Inventory: inventory}
	runner.SetResolvedInventory([]string{"attacker01"}, nil)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, "static-hash", snapshot.SourceHash, "the static snapshot must be untouched")

	hosts, err := store.GetInventorySnapshotHosts(project.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)

	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	assert.Equal(t, []string{"real01"}, names)
}

// A2: a report whose claim was already taken over by a fresher resolve must
// not overwrite the newer pending state.
func TestSetResolvedInventoryLosesARaceToAFresherClaim(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	// The claim went stale (a server restart lost the task) and expired; a
	// later refresh took it over and assigned it to a fresh task, all before
	// the original task's report arrived.
	require.NoError(t, store.SetInventorySnapshot(db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   project.ID,
		Status:      db.InventorySnapshotStatusPending,
		TaskID:      &task.ID,
		Created:     tz.Now().Add(-db.MaxInventoryResolveAge - time.Minute),
	}, db.InventorySnapshotContent{}))

	claimed, err = store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed, "an expired pending claim must be taken over")

	_, _, freshTask := newResolveTask(t, store)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, freshTask.ID))

	runner := &TaskRunner{pool: pool, Task: task, Inventory: inventory}
	runner.SetResolvedInventory([]string{"stale01"}, nil)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status, "the fresher claim must still be pending")
	require.NotNil(t, snapshot.TaskID)
	assert.Equal(t, freshTask.ID, *snapshot.TaskID, "the fresher claim must keep its owner")

	hosts, err := store.GetInventorySnapshotHosts(project.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Empty(t, hosts, "the losing report must not have written anything")
}

// The API router builds its own inventory snapshot service, and each instance
// carries its own in-process checkout lock and remote-head cache while the
// checkout directory is derived from the inventory and therefore shared. The
// pool's instance is safe only as long as it never touches the read side, so
// the type it is held by is what has to enforce that -- not a comment.
func TestTaskPoolCanOnlyStoreResolvedInventories(t *testing.T) {
	narrowed := reflect.TypeOf((*resolvedInventoryStore)(nil)).Elem()

	require.Equal(t, 1, narrowed.NumMethod(), "widening this interface widens what the pool may do")
	assert.Equal(t, "StoreResolved", narrowed.Method(0).Name)

	field, ok := reflect.TypeOf(TaskPool{}).FieldByName("inventorySnapshotService")

	require.True(t, ok)
	assert.Equal(t, narrowed, field.Type,
		"the pool must hold the snapshot service by the narrow interface")
}

// A template that forbids overriding its inventory makes populateDetails
// substitute its own inventory for the requested one, silently. The host list
// that comes back is then B's, and storing it under A would fill A with B's
// hosts and freeze it there -- a stored snapshot is not rebuilt until someone
// refreshes it by hand.
func TestSetResolvedInventoryRefusesADifferentInventoryThanTheTaskRequested(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	// What ansible was actually pointed at: the template's own inventory.
	substituted, err := store.CreateInventory(db.Inventory{
		ProjectID: project.ID,
		Name:      "template-owned",
		Type:      db.InventoryFile,
		Inventory: "static.ini",
	})
	require.NoError(t, err)

	runner := &TaskRunner{pool: pool, Task: task, Inventory: substituted}
	runner.SetResolvedInventory([]string{"belongs-to-b"}, nil)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status,
		"the requested inventory must not be filled with another one's hosts")

	hosts, err := store.GetInventorySnapshotHosts(project.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	assert.Empty(t, hosts)

	_, err = store.GetInventorySnapshot(project.ID, substituted.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "nor may it land on the substituted inventory")
}

// A resolve that dies must not leave the picker saying "resolving" until the
// claim expires half an hour later: the task's terminal status settles the
// pending snapshot with an error.
func TestFailedResolveSettlesThePendingSnapshot(t *testing.T) {
	for _, status := range []task_logger.TaskStatus{
		task_logger.TaskFailStatus,
		task_logger.TaskStoppedStatus,
	} {
		t.Run(string(status), func(t *testing.T) {
			pool, store := newInventoryTaskPool(t)
			project, inventory, task := newResolveTask(t, store)

			claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

			runner := &TaskRunner{pool: pool, Task: task}
			runner.SetStatus(status)

			snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
			require.NoError(t, err)
			assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
			assert.Contains(t, snapshot.Error, string(status))
		})
	}
}

// A later refresh's claim belongs to a different task; a stale failure report
// from the first task must not shoot it down.
func TestFailedResolveLeavesAnotherClaimAlone(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	_, _, otherTask := newResolveTask(t, store)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, otherTask.ID))

	runner := &TaskRunner{pool: pool, Task: task}
	runner.SetStatus(task_logger.TaskFailStatus)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status)
}

// A resolve is the host picker's errand: its success must not fan out into the
// autorun children waiting for a real build.
func TestResolveTaskStartsNoAutorunTasks(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, _, task := newResolveTask(t, store)

	// An autorun child of the resolve's template. If autorun were consulted,
	// starting it would block forever on the pool's unbuffered register
	// channel — this test hanging IS the failure mode.
	_, err := store.CreateTemplate(db.Template{
		Name:            "autorun-child",
		Playbook:        "deploy.yml",
		ProjectID:       project.ID,
		RepositoryID:    task.TemplateID,
		InventoryID:     task.InventoryID,
		BuildTemplateID: &task.TemplateID,
		Autorun:         true,
	})
	require.NoError(t, err)

	task.Status = task_logger.TaskSuccessStatus
	runner := &TaskRunner{pool: pool, Task: task}

	runner.startAutorunTasks()

	tasks, err := store.GetProjectTasks(project.ID, db.RetrieveQueryParams{})
	require.NoError(t, err)

	for _, row := range tasks {
		assert.Nil(t, row.BuildTaskID, "no autorun child may have been queued by a resolve")
	}
}

// A resolve that succeeds without reporting a host list — an older runner that
// does not know the field, or a report the server refused — used to leave the
// claim pending for its whole 30-minute life, with no error and no way to retry.
func TestSuccessfulResolveWithoutAReportSettlesThePendingSnapshot(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	runner := &TaskRunner{pool: pool, Task: task}
	runner.SetStatus(task_logger.TaskSuccessStatus)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Contains(t, snapshot.Error, "without reporting a host list")
}

// The settle above is scoped to a claim that is still pending, so a resolve
// that did report its hosts keeps the snapshot StoreResolved wrote.
func TestSuccessfulResolveWithAReportKeepsItsSnapshot(t *testing.T) {
	pool, store := newInventoryTaskPool(t)
	project, inventory, task := newResolveTask(t, store)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	runner := &TaskRunner{pool: pool, Task: task, Inventory: inventory}
	runner.SetResolvedInventory([]string{"web01"}, map[string][]string{"web": {"web01"}})
	runner.SetStatus(task_logger.TaskSuccessStatus)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Empty(t, snapshot.Error)

	hosts, err := store.GetInventorySnapshotHosts(project.ID, inventory.ID, db.InventoryHostFilter{})
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	assert.Equal(t, "web01", hosts[0].Name)
}
