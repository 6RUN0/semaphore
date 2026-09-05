package sql

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/pkg/tz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUnfinishedTasksFixture builds the one project, repository, inventory and
// template every task row needs, and returns a maker for tasks of a given
// status.
func newUnfinishedTasksFixture(t *testing.T, store *SqlDb) func(task_logger.TaskStatus) db.Task {
	t.Helper()

	project, err := store.CreateProject(db.Project{Name: "unfinished"})
	require.NoError(t, err)

	key, err := store.CreateAccessKey(db.AccessKey{ProjectID: &project.ID, Type: db.AccessKeyNone})
	require.NoError(t, err)

	repository, err := store.CreateRepository(db.Repository{
		ProjectID: project.ID,
		SSHKeyID:  key.ID,
		Name:      "repo",
		GitURL:    "git@example.com:test/test",
		GitBranch: "main",
	})
	require.NoError(t, err)

	template, err := store.CreateTemplate(db.Template{
		Name:         "tpl",
		Playbook:     "test.yml",
		ProjectID:    project.ID,
		RepositoryID: repository.ID,
	})
	require.NoError(t, err)

	return func(status task_logger.TaskStatus) db.Task {
		t.Helper()

		task, err := store.CreateTask(db.Task{
			ProjectID:  project.ID,
			TemplateID: template.ID,
			Created:    tz.Now(),
		}, 0)
		require.NoError(t, err)
		require.False(t, task.Created.IsZero(),
			"a zero Created is written as '0000-00-00' and strict MySQL rejects it")

		task.Status = status
		require.NoError(t, store.UpdateTask(task))

		return task
	}
}

// GetUnfinishedTasks reads the largest table in the database at every start.
// These pin what it selects and that the cap is honoured, so a long-neglected
// installation cannot turn a restart into a full-table materialization.
func TestGetUnfinishedTasks(t *testing.T) {
	store := newSnapshotTestStore(t)
	newTask := newUnfinishedTasksFixture(t, store)

	unfinished := map[int]bool{}
	for _, status := range task_logger.UnfinishedTaskStatuses() {
		unfinished[newTask(status).ID] = true
	}

	finished := []task_logger.TaskStatus{
		task_logger.TaskSuccessStatus,
		task_logger.TaskFailStatus,
		task_logger.TaskStoppedStatus,
	}
	for _, status := range finished {
		newTask(status)
	}

	tasks, total, err := store.GetUnfinishedTasks(task_logger.UnfinishedTaskStatuses(), 100)
	require.NoError(t, err)

	assert.Equal(t, len(unfinished), total)
	assert.Len(t, tasks, len(unfinished))
	for _, task := range tasks {
		assert.True(t, unfinished[task.ID], "a finished task must not be recovered")
	}
}

func TestGetUnfinishedTasksHonoursTheLimit(t *testing.T) {
	store := newSnapshotTestStore(t)
	newTask := newUnfinishedTasksFixture(t, store)

	created := make([]int, 0, 5)
	for i := 0; i < 5; i++ {
		created = append(created, newTask(task_logger.TaskRunningStatus).ID)
	}

	tasks, total, err := store.GetUnfinishedTasks([]task_logger.TaskStatus{task_logger.TaskRunningStatus}, 2)
	require.NoError(t, err)

	// total is what lets the caller report how much it left behind.
	assert.Equal(t, 5, total)
	require.Len(t, tasks, 2)

	// Oldest first: the rows left over are the freshest, which are the ones
	// something else may still settle.
	assert.Equal(t, created[0], tasks[0].ID)
	assert.Equal(t, created[1], tasks[1].ID)
}

func TestGetUnfinishedTasksWithNothingToRecover(t *testing.T) {
	store := newSnapshotTestStore(t)
	newUnfinishedTasksFixture(t, store)

	tasks, total, err := store.GetUnfinishedTasks(task_logger.UnfinishedTaskStatuses(), 100)

	require.NoError(t, err)
	assert.Empty(t, tasks)
	assert.Equal(t, 0, total)
}

// The status filter belongs in the query, not in the caller: the limit has to
// bound the rows the caller acts on. Queued and rejected rows are never settled
// by anything, so they pile up over an installation's lifetime and hold the
// lowest ids — selected together with the running ones, they would fill the
// whole page and leave recovery with nothing to do, permanently.
func TestGetUnfinishedTasksLimitsWithinTheRequestedStatuses(t *testing.T) {
	store := newSnapshotTestStore(t)
	newTask := newUnfinishedTasksFixture(t, store)

	for i := 0; i < 5; i++ {
		newTask(task_logger.TaskRejected)
	}

	running := newTask(task_logger.TaskRunningStatus).ID

	tasks, total, err := store.GetUnfinishedTasks([]task_logger.TaskStatus{task_logger.TaskRunningStatus}, 2)

	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, running, tasks[0].ID)
	assert.Equal(t, 1, total)
}

func TestGetUnfinishedTasksWithNoStatusesOrNoRoom(t *testing.T) {
	store := newSnapshotTestStore(t)
	newTask := newUnfinishedTasksFixture(t, store)
	newTask(task_logger.TaskRunningStatus)

	tasks, total, err := store.GetUnfinishedTasks(nil, 100)
	require.NoError(t, err)
	assert.Empty(t, tasks)
	assert.Equal(t, 0, total)

	tasks, total, err = store.GetUnfinishedTasks(task_logger.UnfinishedTaskStatuses(), 0)
	require.NoError(t, err)
	assert.Empty(t, tasks)
	assert.Equal(t, 0, total)
}
