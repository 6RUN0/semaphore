package tasks

import (
	"fmt"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These pin the restart story: a server that dies mid-task leaves rows saying
// "running" with no process behind them, and before this recovery existed the
// only cure was raw SQL (issues #1376, #1385).

func newRecoveryPool(t *testing.T) (*TaskPool, *sql.SqlDb) {
	t.Helper()

	store := sql.InitConfigCreateTestStore()

	pool := CreateTaskPool(
		store,
		NewMemoryTaskStateStore(),
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

// setTaskState pushes a task row into the state the previous process would
// have left behind.
func setTaskState(t *testing.T, store *sql.SqlDb, task db.Task, status task_logger.TaskStatus, runnerID *int) db.Task {
	t.Helper()

	task.Status = status
	task.RunnerID = runnerID
	require.NoError(t, store.UpdateTask(task))

	return task
}

func taskLogContains(t *testing.T, store *sql.SqlDb, task db.Task, text string) bool {
	t.Helper()

	outputs, err := store.GetTaskOutputs(task.ProjectID, task.ID, db.RetrieveQueryParams{})
	require.NoError(t, err)

	for _, output := range outputs {
		if output.Output == text {
			return true
		}
	}

	return false
}

func TestRecoverInterruptedTasks(t *testing.T) {
	for _, status := range []task_logger.TaskStatus{
		task_logger.TaskRunningStatus,
		task_logger.TaskStartingStatus,
		task_logger.TaskStoppingStatus,
	} {
		t.Run("a local "+string(status)+" task is stopped", func(t *testing.T) {
			pool, store := newRecoveryPool(t)
			_, _, task := newResolveTask(t, store)
			task = setTaskState(t, store, task, status, nil)

			pool.recoverInterruptedTasks()

			row, err := store.GetTaskByID(task.ID)
			require.NoError(t, err)
			assert.Equal(t, task_logger.TaskStoppedStatus, row.Status)
			assert.NotNil(t, row.End, "a stopped task must not show a still-running duration")
			assert.True(t, taskLogContains(t, store, task, "Task was interrupted by a server restart."),
				"the task log must say why the task ended without a result")
		})
	}

	// A queued task never had a process behind it, so a restart is not an
	// interruption for it: it survives to be picked up normally (review
	// finding B1).
	for _, status := range []task_logger.TaskStatus{
		task_logger.TaskWaitingStatus,
		task_logger.TaskWaitingConfirmation,
		task_logger.TaskConfirmed,
		task_logger.TaskRejected,
	} {
		t.Run("a queued "+string(status)+" task survives recovery", func(t *testing.T) {
			pool, store := newRecoveryPool(t)
			_, _, task := newResolveTask(t, store)
			task = setTaskState(t, store, task, status, nil)

			pool.recoverInterruptedTasks()

			row, err := store.GetTaskByID(task.ID)
			require.NoError(t, err)
			assert.Equal(t, status, row.Status, "recovery must not touch a task that never had a process")
			assert.Nil(t, row.End)
		})
	}

	t.Run("a finished task is left alone", func(t *testing.T) {
		pool, store := newRecoveryPool(t)
		_, _, task := newResolveTask(t, store)
		task = setTaskState(t, store, task, task_logger.TaskSuccessStatus, nil)

		pool.recoverInterruptedTasks()

		row, err := store.GetTaskByID(task.ID)
		require.NoError(t, err)
		assert.Equal(t, task_logger.TaskSuccessStatus, row.Status)
	})

	// A task on a runner survived the restart: the runner is a separate
	// process. It is restored into the pool so its progress reports find it
	// again, and its status is not touched.
	t.Run("a dispatched task is restored, not stopped", func(t *testing.T) {
		pool, store := newRecoveryPool(t)
		_, _, task := newResolveTask(t, store)

		runner, err := store.CreateRunner(db.Runner{Name: "worker", Token: "recovery-test-token"})
		require.NoError(t, err)

		task = setTaskState(t, store, task, task_logger.TaskRunningStatus, &runner.ID)

		pool.recoverInterruptedTasks()

		row, err := store.GetTaskByID(task.ID)
		require.NoError(t, err)
		assert.Equal(t, task_logger.TaskRunningStatus, row.Status, "a live runner's task keeps running")

		restored, err := pool.GetTask(task.ID)
		require.NoError(t, err)
		require.NotNil(t, restored, "the pool must know the task again, or progress reports are dropped")
		require.NotNil(t, restored.Task.RunnerID)
		assert.Equal(t, runner.ID, *restored.Task.RunnerID)
	})

	// With several nodes, an unfinished row may belong to a node that is alive
	// and well; recovery must not shoot another node's task.
	t.Run("HA leaves everything alone", func(t *testing.T) {
		pool, store := newRecoveryPool(t)
		_, _, task := newResolveTask(t, store)
		task = setTaskState(t, store, task, task_logger.TaskRunningStatus, nil)

		util.Config.HA = &util.HAConfig{Enabled: true}
		defer func() { util.Config.HA = nil }()

		pool.recoverInterruptedTasks()

		row, err := store.GetTaskByID(task.ID)
		require.NoError(t, err)
		assert.Equal(t, task_logger.TaskRunningStatus, row.Status)
	})
}

// slowRecoveryStore holds recovery inside GetUnfinishedTasks so a test can look
// at the pool while recovery is still in flight.
type slowRecoveryStore struct {
	db.Store

	started chan struct{}
	release chan struct{}
}

func (s *slowRecoveryStore) GetUnfinishedTasks([]task_logger.TaskStatus, int) ([]db.Task, int, error) {
	close(s.started)
	<-s.release
	return nil, 0, nil
}

// limitRecordingStore captures what recovery asks the store for.
type limitRecordingStore struct {
	db.Store

	statuses []task_logger.TaskStatus
	limit    int
}

func (s *limitRecordingStore) GetUnfinishedTasks(statuses []task_logger.TaskStatus, limit int) ([]db.Task, int, error) {
	s.statuses = statuses
	s.limit = limit
	return nil, 0, nil
}

// Recovery costs several queries per row, so it takes a bounded slice of the
// unfinished rows rather than all of them.
func TestRecoveryAsksForABoundedNumberOfTasks(t *testing.T) {
	store := sql.InitConfigCreateTestStore()
	recording := &limitRecordingStore{Store: store}

	pool := CreateTaskPool(
		recording,
		NewMemoryTaskStateStore(),
		nil,
		&InventoryServiceMock{},
		&EncryptionServiceMock{},
		&KeyInstallerMock{},
		&mockLogWriteService{},
		nil,
		nil,
	)

	pool.recoverInterruptedTasks()

	assert.Equal(t, maxTasksToRecover, recording.limit)
	assert.Positive(t, maxTasksToRecover)

	// The cap has to bound the rows recovery acts on, so the narrowing belongs
	// in the query. Asked for every unfinished status instead, a pass would
	// spend its whole budget on queued and rejected rows, which hold the lowest
	// ids and which nothing ever settles.
	assert.ElementsMatch(t, recoverableTaskStatuses(), recording.statuses)
	assert.NotContains(t, recording.statuses, task_logger.TaskWaitingStatus)
	assert.NotContains(t, recording.statuses, task_logger.TaskRejected)
}

// AddTask finishes by writing to the pool's unbuffered register channel, which
// only Run's select loop reads. Recovery used to run before that loop started,
// so every API "run task" call and every schedule that fired during recovery
// hung until it finished.
func TestRecoveryDoesNotBlockTheQueue(t *testing.T) {
	store := sql.InitConfigCreateTestStore()
	slow := &slowRecoveryStore{
		Store:   store,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}

	pool := CreateTaskPool(
		slow,
		NewMemoryTaskStateStore(),
		nil,
		&InventoryServiceMock{},
		&EncryptionServiceMock{},
		&KeyInstallerMock{},
		&mockLogWriteService{},
		nil,
		nil,
	)

	go pool.Run()

	// Released before Stop, not after: Stop waits for the recovery goroutine to
	// exit, and this store is holding it inside GetUnfinishedTasks.
	defer func() {
		close(slow.release)
		pool.Stop()
	}()

	<-slow.started

	_, _, task := newResolveTask(t, store)

	// Failed on arrival, so that once recovery lets the queue run the task is
	// dropped from it instead of actually being executed by the test.
	task.Status = task_logger.TaskFailStatus
	runner := &TaskRunner{pool: &pool, Task: task}

	select {
	case pool.register <- runner:
	case <-time.After(5 * time.Second):
		t.Fatal("registering a task blocked while recovery was running")
	}

	// The other half of the same fix: events are accepted throughout, but
	// nothing is started until recovery has said what is already running.
	require.Eventually(t, func() bool {
		for _, queued := range pool.state.QueueRange() {
			if queued.Task.ID == task.ID {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the task must be queued while recovery runs")

	assert.False(t, pool.isRecovered(), "recovery is still in flight")

	// Two events rather than a sleep: queueEvents is unbuffered and handleQueue
	// is its only reader, so accepting the second one proves the first was
	// processed to the end of the loop body — the point where the task would
	// have been started had the barrier been open. A sleep would assert the
	// same thing only for as long as a loaded machine keeps up.
	for i := 0; i < 2; i++ {
		select {
		case pool.queueEvents <- PoolEvent{EventTypeEmpty, nil}:
		case <-time.After(5 * time.Second):
			t.Fatal("the queue loop never accepted the wake-up event")
		}
	}

	found := false
	for _, queued := range pool.state.QueueRange() {
		if queued.Task.ID == task.ID {
			found = true
		}
	}
	assert.True(t, found, "the task must still be waiting in the queue, not started")
}

// failingEncryptionService breaks populateDetails the way a rotated or corrupt
// access key does in the field.
type failingEncryptionService struct {
	EncryptionServiceMock
}

func (s *failingEncryptionService) DeserializeSecret(key *db.AccessKey) error {
	return fmt.Errorf("the key can no longer be decrypted")
}

// The Stop button must correct the record even when the task's dependencies
// can no longer be loaded — a stuck task with a broken key was exactly the
// case where only raw SQL helped.
func TestStopTaskSurvivesBrokenDetails(t *testing.T) {
	store := sql.InitConfigCreateTestStore()

	pool := CreateTaskPool(
		store,
		NewMemoryTaskStateStore(),
		nil,
		&InventoryServiceMock{},
		&failingEncryptionService{},
		&KeyInstallerMock{},
		&mockLogWriteService{},
		nil,
		nil,
	)

	_, _, task := newResolveTask(t, store)
	task = setTaskState(t, store, task, task_logger.TaskRunningStatus, nil)

	require.NoError(t, pool.StopTask(task, false))

	row, err := store.GetTaskByID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, task_logger.TaskStoppedStatus, row.Status)
	assert.NotNil(t, row.End)
}

// A task that already finished before the Stop request reached this path must
// not have its result rewritten to "stopped" just because its details can no
// longer be loaded.
func TestStopTaskWithBrokenDetailsLeavesAFinishedTaskAlone(t *testing.T) {
	store := sql.InitConfigCreateTestStore()

	pool := CreateTaskPool(
		store,
		NewMemoryTaskStateStore(),
		nil,
		&InventoryServiceMock{},
		&failingEncryptionService{},
		&KeyInstallerMock{},
		&mockLogWriteService{},
		nil,
		nil,
	)

	_, _, task := newResolveTask(t, store)
	task = setTaskState(t, store, task, task_logger.TaskSuccessStatus, nil)

	require.NoError(t, pool.StopTask(task, false))

	row, err := store.GetTaskByID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, task_logger.TaskSuccessStatus, row.Status, "a successful run must not become a false stop")
}

// The happy path of stopping an absent task keeps working as before.
func TestStopTaskOnAbsentTask(t *testing.T) {
	pool, store := newRecoveryPool(t)
	_, _, task := newResolveTask(t, store)
	task = setTaskState(t, store, task, task_logger.TaskRunningStatus, nil)

	require.NoError(t, pool.StopTask(task, false))

	row, err := store.GetTaskByID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, task_logger.TaskStoppedStatus, row.Status)
}

// An open UI learns of status changes from the event feed; recovery must not
// be the silent exception to that.
func TestRecoveryWritesAnEvent(t *testing.T) {
	pool, store := newRecoveryPool(t)
	_, _, task := newResolveTask(t, store)
	task = setTaskState(t, store, task, task_logger.TaskRunningStatus, nil)

	pool.recoverInterruptedTasks()

	events, err := store.GetEvents(task.ProjectID, db.RetrieveQueryParams{})
	require.NoError(t, err)

	found := false
	for _, event := range events {
		if event.ObjectID != nil && *event.ObjectID == task.ID {
			found = true
		}
	}

	assert.True(t, found, "the event feed must record the interruption")
}

// An interrupted resolve task must settle the pending snapshot it was
// dispatched for, or the picker keeps saying "resolving" for half an hour.
func TestRecoverySettlesAPendingResolve(t *testing.T) {
	pool, store := newRecoveryPool(t)
	project, inventory, task := newResolveTask(t, store)
	task = setTaskState(t, store, task, task_logger.TaskRunningStatus, nil)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	pool.recoverInterruptedTasks()

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.NotEmpty(t, snapshot.Error)
}
