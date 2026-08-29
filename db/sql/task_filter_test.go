package sql

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taskFilterFixture is one project with two templates plus a second project,
// which is what makes "the filter narrowed nothing" distinguishable from "the
// filter narrowed correctly".
type taskFilterFixture struct {
	projectID  int
	templateID int
	otherTpl   int
	otherProj  int

	// tasks of templateID, oldest first.
	tasks []db.Task
}

func newTaskFilterFixture(t *testing.T, store *SqlDb) taskFilterFixture {
	t.Helper()

	newTemplate := func(projectID int, name string) int {
		key, err := store.CreateAccessKey(db.AccessKey{ProjectID: &projectID, Type: db.AccessKeyNone})
		require.NoError(t, err)

		repository, err := store.CreateRepository(db.Repository{
			ProjectID: projectID,
			SSHKeyID:  key.ID,
			Name:      name,
			GitURL:    "git@example.com:test/test",
			GitBranch: "main",
		})
		require.NoError(t, err)

		template, err := store.CreateTemplate(db.Template{
			Name:         name,
			Playbook:     "test.yml",
			ProjectID:    projectID,
			RepositoryID: repository.ID,
		})
		require.NoError(t, err)

		return template.ID
	}

	project, err := store.CreateProject(db.Project{Name: "filters"})
	require.NoError(t, err)

	other, err := store.CreateProject(db.Project{Name: "elsewhere"})
	require.NoError(t, err)

	fixture := taskFilterFixture{
		projectID: project.ID,
		otherProj: other.ID,
	}
	fixture.templateID = newTemplate(project.ID, "main")
	fixture.otherTpl = newTemplate(project.ID, "second")
	otherProjectTpl := newTemplate(other.ID, "foreign")

	newTask := func(projectID, templateID int, status task_logger.TaskStatus, runID *int) db.Task {
		task, err := store.CreateTask(db.Task{
			ProjectID:     projectID,
			TemplateID:    templateID,
			WorkflowRunID: runID,
		}, 0)
		require.NoError(t, err)

		task.Status = status
		require.NoError(t, store.UpdateTask(task))

		return task
	}

	runID := 1
	fixture.tasks = []db.Task{
		newTask(project.ID, fixture.templateID, task_logger.TaskSuccessStatus, nil),
		newTask(project.ID, fixture.templateID, task_logger.TaskFailStatus, nil),
		newTask(project.ID, fixture.templateID, task_logger.TaskRunningStatus, &runID),
	}

	// Neighbours that every filter has to leave out.
	newTask(project.ID, fixture.otherTpl, task_logger.TaskSuccessStatus, nil)
	newTask(other.ID, otherProjectTpl, task_logger.TaskSuccessStatus, nil)

	return fixture
}

func taskIDsOf(tasks []db.TaskWithTpl) []int {
	ids := make([]int, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func TestGetTasksFilters(t *testing.T) {
	store := newSnapshotTestStore(t)
	fixture := newTaskFilterFixture(t, store)

	t.Run("project scope", func(t *testing.T) {
		tasks, err := store.GetProjectTasks(fixture.projectID, db.RetrieveQueryParams{})
		require.NoError(t, err)

		// Four of the five tasks: the fifth belongs to another project.
		assert.Len(t, tasks, 4)
		for _, task := range tasks {
			assert.Equal(t, fixture.projectID, task.ProjectID)
		}
	})

	t.Run("template scope", func(t *testing.T) {
		tasks, err := store.GetTemplateTasks(fixture.projectID, fixture.templateID, db.RetrieveQueryParams{})
		require.NoError(t, err)

		assert.Len(t, tasks, len(fixture.tasks))
		for _, task := range tasks {
			assert.Equal(t, fixture.templateID, task.TemplateID)
		}
	})

	t.Run("workflow run", func(t *testing.T) {
		tasks, err := store.GetWorkflowRunTasks(fixture.projectID, 1, db.RetrieveQueryParams{})
		require.NoError(t, err)

		require.Len(t, tasks, 1)
		assert.Equal(t, fixture.tasks[2].ID, tasks[0].ID)
	})

	t.Run("status", func(t *testing.T) {
		tasks, err := store.GetTemplateTasks(fixture.projectID, fixture.templateID, db.RetrieveQueryParams{
			TaskFilter: &db.TaskFilter{Status: []task_logger.TaskStatus{task_logger.TaskFailStatus}},
		})
		require.NoError(t, err)

		require.Len(t, tasks, 1)
		assert.Equal(t, fixture.tasks[1].ID, tasks[0].ID)
	})

	// An empty status list is "no status chosen", which the UI sends whenever
	// the filter is cleared. Reading it as "match nothing" empties the list.
	t.Run("empty status list", func(t *testing.T) {
		tasks, err := store.GetTemplateTasks(fixture.projectID, fixture.templateID, db.RetrieveQueryParams{
			TaskFilter: &db.TaskFilter{},
		})
		require.NoError(t, err)

		assert.Len(t, tasks, len(fixture.tasks))
	})

	t.Run("count", func(t *testing.T) {
		tasks, err := store.GetTemplateTasks(fixture.projectID, fixture.templateID, db.RetrieveQueryParams{Count: 2})
		require.NoError(t, err)

		// Newest first, so the page starts at the last task created.
		require.Len(t, tasks, 2)
		assert.Equal(t, fixture.tasks[2].ID, tasks[0].ID)
		assert.Equal(t, fixture.tasks[1].ID, tasks[1].ID)
	})

	// The cursor is exclusive and zero means "from the start"; treating zero as
	// a cursor would return nothing at all on the first page.
	t.Run("before id", func(t *testing.T) {
		tasks, err := store.GetTemplateTasks(fixture.projectID, fixture.templateID, db.RetrieveQueryParams{
			BeforeID: fixture.tasks[2].ID,
		})
		require.NoError(t, err)

		assert.Equal(t, []int{fixture.tasks[1].ID, fixture.tasks[0].ID}, taskIDsOf(tasks))
	})
}

// The id list has three-way semantics that no exported caller exercises yet:
// nil is "no filter", empty is "nothing can match", and a list restricts.
func TestGetTasksByIDs(t *testing.T) {
	store := newSnapshotTestStore(t)
	fixture := newTaskFilterFixture(t, store)

	get := func(taskIDs []int) []db.TaskWithTpl {
		t.Helper()

		var tasks []db.TaskWithTpl
		require.NoError(t, store.getTasks(fixture.projectID, nil, nil, taskIDs, db.RetrieveQueryParams{}, &tasks))
		return tasks
	}

	t.Run("nil selects everything in the project", func(t *testing.T) {
		assert.Len(t, get(nil), 4)
	})

	t.Run("empty selects nothing", func(t *testing.T) {
		assert.Empty(t, get([]int{}))
	})

	t.Run("a list selects exactly those", func(t *testing.T) {
		wanted := []int{fixture.tasks[0].ID, fixture.tasks[2].ID}

		got := taskIDsOf(get(wanted))

		assert.ElementsMatch(t, wanted, got)
	})
}
