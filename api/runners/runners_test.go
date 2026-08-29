package runners

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/pkg/tz"
	proServer "github.com/semaphoreui/semaphore/pro/services/server"
	"github.com/semaphoreui/semaphore/services/runners"
	"github.com/semaphoreui/semaphore/services/tasks"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterRunner_InvalidTokenReturnsBadRequest(t *testing.T) {
	store := sql.InitConfigCreateTestStore()

	body, err := json.Marshal(map[string]any{
		"registration_token": "not-a-valid-token",
		"name":               "test-runner",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/internal/runners", bytes.NewReader(body))
	req = helpers.SetContextValue(req, "store", store)

	w := httptest.NewRecorder()
	RegisterRunner(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var res map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.Equal(t, "Invalid registration token", res["error"])
}

func newProgressRequest(t *testing.T, store db.Store, runner db.Runner, progress runners.RunnerProgress) *http.Request {
	t.Helper()

	body, err := json.Marshal(progress)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPut, "/api/internal/runners", bytes.NewReader(body))
	req = helpers.SetContextValue(req, "store", store)
	req = helpers.SetContextValue(req, "runner", runner)
	return req
}

func decodeProgressResponse(t *testing.T, w *httptest.ResponseRecorder) runners.RunnerProgressResponse {
	t.Helper()

	var res runners.RunnerProgressResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	return res
}

func TestUpdateRunner_StoppedTaskReportedAsTerminated(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	store := sql.InitConfigCreateTestStore() // also initializes util.Config

	pool := tasks.CreateTaskPool(
		store,
		tasks.NewMemoryTaskStateStore(),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	ctrl := NewRunnerController(nil, &pool, nil, nil)

	runnerID := 1

	// The task was stopped on the server while the runner was offline.
	tr := tasks.NewTaskRunner(db.Task{
		ID:        5,
		ProjectID: 1,
		RunnerID:  &runnerID,
		Status:    task_logger.TaskStoppedStatus,
	}, &pool, "", nil)
	pool.StateStore().SetRunning(tr)

	req := newProgressRequest(t, store, db.Runner{ID: runnerID}, runners.RunnerProgress{
		Jobs: []runners.JobProgress{{
			ID:     5,
			Status: task_logger.TaskRunningStatus,
			LogRecords: []runners.LogRecord{
				{Time: tz.Now(), Message: "late output"},
			},
		}},
	})
	w := httptest.NewRecorder()

	ctrl.UpdateRunner(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []int{5}, decodeProgressResponse(t, w).TerminatedJobs)

	// The late report must not overwrite the terminal status.
	assert.Equal(t, task_logger.TaskStoppedStatus, tr.Task.Status)
}

func TestUpdateRunner_UnknownTaskReportedAsTerminated(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	store := sql.InitConfigCreateTestStore()

	pool := tasks.CreateTaskPool(
		store,
		tasks.NewMemoryTaskStateStore(),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	ctrl := NewRunnerController(nil, &pool, nil, nil)

	req := newProgressRequest(t, store, db.Runner{ID: 1}, runners.RunnerProgress{
		Jobs: []runners.JobProgress{{
			ID:     999, // neither in the pool nor in the database
			Status: task_logger.TaskRunningStatus,
		}},
	})
	w := httptest.NewRecorder()

	ctrl.UpdateRunner(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []int{999}, decodeProgressResponse(t, w).TerminatedJobs)
}

func TestUpdateRunner_ReassignedTaskReportedAsTerminated(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	store := sql.InitConfigCreateTestStore()

	pool := tasks.CreateTaskPool(
		store,
		tasks.NewMemoryTaskStateStore(),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	ctrl := NewRunnerController(nil, &pool, nil, nil)

	oldRunnerID := 1
	newRunnerID := 2

	// Task was reassigned from runner 1 to runner 2 while runner 1 still had
	// the job in its local pool (e.g. after requeueTaskRunnerOffline).
	tr := tasks.NewTaskRunner(db.Task{
		ID:        8,
		ProjectID: 1,
		RunnerID:  &newRunnerID,
		Status:    task_logger.TaskStartingStatus,
	}, &pool, "", nil)
	pool.StateStore().SetRunning(tr)

	req := newProgressRequest(t, store, db.Runner{ID: oldRunnerID}, runners.RunnerProgress{
		Jobs: []runners.JobProgress{{
			ID:     8,
			Status: task_logger.TaskRunningStatus,
			LogRecords: []runners.LogRecord{
				{Time: tz.Now(), Message: "stale runner output"},
			},
		}},
	})
	w := httptest.NewRecorder()

	ctrl.UpdateRunner(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []int{8}, decodeProgressResponse(t, w).TerminatedJobs)

	// The late report must not overwrite status or assignee.
	assert.Equal(t, task_logger.TaskStartingStatus, tr.Task.Status)
	assert.Equal(t, newRunnerID, *tr.Task.RunnerID)
}

func TestUpdateRunner_RunningTaskAcceptedWithoutTermination(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	store := sql.InitConfigCreateTestStore()

	pool := tasks.CreateTaskPool(
		store,
		tasks.NewMemoryTaskStateStore(),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	ctrl := NewRunnerController(nil, &pool, nil, nil)

	runnerID := 1

	tr := tasks.NewTaskRunner(db.Task{
		ID:        7,
		ProjectID: 1,
		RunnerID:  &runnerID,
		Status:    task_logger.TaskRunningStatus,
	}, &pool, "", nil)
	pool.StateStore().SetRunning(tr)

	req := newProgressRequest(t, store, db.Runner{ID: runnerID}, runners.RunnerProgress{
		Jobs: []runners.JobProgress{{
			ID:     7,
			Status: task_logger.TaskRunningStatus,
			LogRecords: []runners.LogRecord{
				{Time: tz.Now(), Message: "normal output"},
			},
		}},
	})
	w := httptest.NewRecorder()

	ctrl.UpdateRunner(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, decodeProgressResponse(t, w).TerminatedJobs)
	assert.Equal(t, task_logger.TaskRunningStatus, tr.Task.Status)
}

// helpers.Bind decodes the request body into memory with nothing bounding it,
// so before the limit an authenticated runner could hold the server hostage
// with one endless progress report.
func TestBodyLimitMiddlewareStopsAnEndlessBody(t *testing.T) {
	var read int64
	var readErr error

	handler := BodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, readErr = io.Copy(io.Discard, r.Body)
	}))

	// A body with no end to it: what an attacker sends, and what a plain
	// io.Copy into a decoder would follow forever.
	req := httptest.NewRequest(http.MethodPut, "/api/internal/runners", endlessReader{})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Error(t, readErr, "reading past the cap must fail rather than continue")
	assert.Equal(t, MaxRunnerReportBodySize, read, "no more than the cap may be read")
}

// The cap has to sit above the largest report a runner is allowed to have
// accepted, or the limit would reject reports StoreResolved would have taken.
func TestMaxRunnerReportBodySizeCoversTheLargestLegalReport(t *testing.T) {
	progress := runners.RunnerProgress{
		Jobs: []runners.JobProgress{{
			ID:                1,
			Status:            task_logger.TaskSuccessStatus,
			ResolvedInventory: &db.InventorySnapshotContent{},
		}},
	}

	name := strings.Repeat("h", db.MaxInventoryHostNameLength)
	content := progress.Jobs[0].ResolvedInventory

	for i := 0; i < db.MaxInventorySnapshotHosts; i++ {
		content.Hosts = append(content.Hosts, name)
	}

	// The membership cap is the real weight of a report: it is ten times the
	// host cap, spread over the groups.
	const hostsPerGroup = db.MaxInventorySnapshotMemberships / db.MaxInventorySnapshotGroups
	members := make([]string, hostsPerGroup)
	for i := range members {
		members[i] = name
	}

	for i := 0; i < db.MaxInventorySnapshotGroups; i++ {
		content.Groups = append(content.Groups, db.InventorySnapshotGroupContent{
			Name:  name,
			Hosts: members,
		})
	}

	body, err := json.Marshal(progress)
	require.NoError(t, err)

	assert.Less(t, int64(len(body)), MaxRunnerReportBodySize,
		"a report at every cap must still fit under the body limit")
}

// endlessReader never reaches EOF.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestRegisterRunner_NonSmrsTokenWithoutGlobalMatchReturnsBadRequest(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })
	util.Config = &util.ConfigType{
		RunnerRegistrationToken: "global-reg-token",
	}

	store := sql.InitConfigCreateTestStore()

	body, err := json.Marshal(map[string]any{
		"registration_token": "legacy-one-time-token-without-smrs-prefix",
		"name":               "test-runner",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/internal/runners", bytes.NewReader(body))
	req = helpers.SetContextValue(req, "store", store)

	w := httptest.NewRecorder()
	RegisterRunner(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// A runner's progress report carries the status and the resolved host list in
// one message. The status is what settles the pending snapshot claim, so
// applying it first left StoreResolved with no pending row to replace and the
// host list — already produced, already correct — was dropped with an Info log.
func TestUpdateRunner_StoresTheResolvedInventoryOfAStoppedTask(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	store := sql.InitConfigCreateTestStore()

	project, err := store.CreateProject(db.Project{Name: "resolve"})
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

	runner, err := store.CreateRunner(db.Runner{Name: "worker", Token: "resolve-order-test-token"})
	require.NoError(t, err)

	runnerID := runner.ID

	task, err := store.CreateTask(db.Task{
		ProjectID:   project.ID,
		TemplateID:  template.ID,
		InventoryID: &inventory.ID,
		RunnerID:    &runnerID,
		Status:      task_logger.TaskRunningStatus,
		Params:      db.MapStringAnyField{"resolve_inventory_only": true},
	}, 0)
	require.NoError(t, err)

	claimed, err := store.ClaimInventorySnapshotResolve(project.ID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.AssignInventorySnapshotResolveTask(project.ID, inventory.ID, task.ID))

	// A terminal status finalizes the task in a goroutine of its own, and
	// finalization writes the task log, so this pool needs a real log service
	// where the pools above could do with nil.
	pool := tasks.CreateTaskPool(
		store,
		tasks.NewMemoryTaskStateStore(),
		nil,
		nil,
		nil,
		nil,
		proServer.NewLogWriteService(),
		nil,
		nil,
	)
	ctrl := NewRunnerController(nil, &pool, nil, nil)

	tr := tasks.NewTaskRunner(task, &pool, "", nil)
	tr.Inventory = inventory
	pool.StateStore().SetRunning(tr)

	req := newProgressRequest(t, store, runner, runners.RunnerProgress{
		Jobs: []runners.JobProgress{{
			ID:     task.ID,
			Status: task_logger.TaskStoppedStatus,
			ResolvedInventory: &db.InventorySnapshotContent{
				Hosts:  []string{"i-0a1"},
				Groups: []db.InventorySnapshotGroupContent{{Name: "web", Hosts: []string{"i-0a1"}}},
			},
		}},
	})
	w := httptest.NewRecorder()

	ctrl.UpdateRunner(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	snapshot, err := store.GetInventorySnapshot(project.ID, inventory.ID)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 1, snapshot.HostCount)
}

// Registration is the one route of the internal API that takes no token, and it
// decodes the body before it looks at the token it was sent. The report limit
// is applied to the authenticated subrouter, which does not cover it.
func TestRegistrationBodyLimitStopsAnEndlessBody(t *testing.T) {
	var read int64
	var readErr error

	handler := RegistrationBodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, readErr = io.Copy(io.Discard, r.Body)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/internal/runners", endlessReader{})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Error(t, readErr, "reading past the cap must fail rather than continue")
	assert.Equal(t, MaxRunnerRegistrationBodySize, read)
	assert.Less(t, MaxRunnerRegistrationBodySize, MaxRunnerReportBodySize,
		"a registration is a handful of short fields, not a progress report")
}
