package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/api/sockets"
	"github.com/semaphoreui/semaphore/db"
	proFactory "github.com/semaphoreui/semaphore/pro/db/factory"
	proServer "github.com/semaphoreui/semaphore/pro/services/server"
	proTasks "github.com/semaphoreui/semaphore/pro/services/tasks"
	"github.com/semaphoreui/semaphore/services/server"
	"github.com/semaphoreui/semaphore/services/tasks"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startSocketsHub guards the process-wide websocket hub: it must consume log
// pushes for the pool to make progress, and it can only be started once.
var startSocketsHub sync.Once

// Refreshing a dynamic inventory cannot be served by the server alone: it
// dispatches a resolve task instead. These tests drive that dispatch through
// the real router, middlewares and a real task pool, because the pieces are
// wired in three different places (controller, store claim, pool) and only
// this level proves they meet.

// startTaskPool builds the pool the way cli/cmd/root.go does — real services
// on the same store — starts it, and injects it into the request context the
// way the server does.
func (a *snapshotAPI) startTaskPool(t *testing.T) *tasks.TaskPool {
	t.Helper()

	util.Config.TmpPath = t.TempDir()

	// The pool asks the runner settings whether tasks go to remote runners;
	// the answer for these tests is always "run locally".
	util.Config.Runners = &util.RunnersConfig{}

	// Templates only accept whitelisted apps; the server seeds the built-ins
	// at boot, the test seeds the one it uses.
	util.Config.Apps = map[string]util.App{"ansible": {}}

	// ansible refuses to run with an empty HOME ("/.ansible"); pointing HOME
	// at the checkout keeps everything under the test's own temp directory.
	util.Config.HomeDirMode = util.HomeDirModeProjectHome

	// Task log lines are pushed to the websocket hub before anything else, and
	// that push blocks until the hub consumes it. The server starts the hub at
	// boot; without it the pool's own "added to queue" line would deadlock the
	// whole queue.
	startSocketsHub.Do(func() { go sockets.StartWS() })

	encryption := server.NewAccessKeyEncryptionService(a.store, a.store, a.store, a.store)

	pool := tasks.CreateTaskPool(
		a.store,
		proTasks.NewTaskStateStore(),
		proFactory.NewAnsibleTaskRepository(a.store),
		server.NewInventoryService(a.store, a.store, a.store, encryption),
		encryption,
		server.NewAccessKeyInstallationService(encryption),
		proServer.NewLogWriteService(),
		nil,
		nil,
	)

	router := a.router.(*mux.Router)
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, helpers.SetContextValue(r, "task_pool", &pool))
		})
	})

	go pool.Run()
	t.Cleanup(pool.Stop)

	return &pool
}

func dispatchGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@localhost",
	)

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// addDynamicInventory commits an executable inventory script into a fresh
// local repository and registers the repository, the inventory and a template
// tying them together. The script sleeps so that the resolve stays visibly
// in flight while the test issues its second request.
func (a *snapshotAPI) addDynamicInventory(t *testing.T, script string) (db.Inventory, db.Template) {
	t.Helper()

	dir := t.TempDir()
	dispatchGit(t, dir, "init", "--initial-branch=main", ".")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.py"), []byte(script), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "site.yml"), []byte("---\n- hosts: all\n"), 0o600))
	dispatchGit(t, dir, "add", ".")
	dispatchGit(t, dir, "commit", "-m", "dynamic inventory")

	key, err := a.store.CreateAccessKey(db.AccessKey{
		ProjectID: &a.projectID,
		Type:      db.AccessKeyNone,
	})
	require.NoError(t, err)

	repository, err := a.store.CreateRepository(db.Repository{
		ProjectID: a.projectID,
		SSHKeyID:  key.ID,
		Name:      "dynamic-repo",
		GitURL:    "file://" + dir,
		GitBranch: "main",
	})
	require.NoError(t, err)

	inventory, err := a.store.CreateInventory(db.Inventory{
		ProjectID:    a.projectID,
		Name:         "dynamic",
		Type:         db.InventoryFile,
		Inventory:    "dynamic.py",
		RepositoryID: &repository.ID,
	})
	require.NoError(t, err)

	template, err := a.store.CreateTemplate(db.Template{
		Name:         "resolve",
		App:          db.AppAnsible,
		Playbook:     "site.yml",
		ProjectID:    a.projectID,
		RepositoryID: repository.ID,
		InventoryID:  &inventory.ID,
	})
	require.NoError(t, err)

	return inventory, template
}

const slowDynamicInventory = "#!/usr/bin/env python3\nimport time\ntime.sleep(15)\nprint('{}')\n"

const fastDynamicInventory = `#!/usr/bin/env python3
import json
print(json.dumps({
    "_meta": {"hostvars": {"web01": {"ansible_password": "hostvar-secret"}}},
    "web": {"hosts": ["web01", "web02"]},
}))
`

func (a *snapshotAPI) refreshPath(inventory db.Inventory, template db.Template) string {
	return a.snapshotPath(inventory, fmt.Sprintf("/refresh?template_id=%d", template.ID))
}

func (a *snapshotAPI) projectTaskCount(t *testing.T) int {
	t.Helper()

	rows, err := a.store.GetProjectTasks(a.projectID, db.RetrieveQueryParams{})
	require.NoError(t, err)

	return len(rows)
}

func TestSnapshotAPI_DispatchesAResolveTask(t *testing.T) {
	api := newSnapshotAPI(t)
	api.startTaskPool(t)
	inventory, template := api.addDynamicInventory(t, slowDynamicInventory)

	w := api.request(http.MethodPost, api.refreshPath(inventory, template), api.memberToken)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	var pending db.InventorySnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &pending))
	assert.Equal(t, db.InventorySnapshotStatusPending, pending.Status)
	require.NotNil(t, pending.TaskID, "the caller needs the task to watch its progress")

	assert.Equal(t, 1, api.projectTaskCount(t))

	// While the resolve is in flight, another refresh must report the pending
	// resolve rather than pile a second task onto the queue.
	w = api.request(http.MethodPost, api.refreshPath(inventory, template), api.memberToken)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var again db.InventorySnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &again))
	assert.Equal(t, db.InventorySnapshotStatusPending, again.Status)

	assert.Equal(t, 1, api.projectTaskCount(t), "a pending resolve must not spawn another task")
}

func TestSnapshotAPI_DispatchNeedsATemplate(t *testing.T) {
	api := newSnapshotAPI(t)
	api.startTaskPool(t)
	inventory, _ := api.addDynamicInventory(t, slowDynamicInventory)

	w := api.request(http.MethodPost, api.snapshotPath(inventory, "/refresh"), api.memberToken)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, 0, api.projectTaskCount(t))
}

// A role that may manage inventories but not run tasks passes the subtree's
// CanManageProjectResources gate, so only the dispatch's own permission check
// stands between it and executing the inventory program.
func TestSnapshotAPI_DispatchRefusesWithoutRunPermission(t *testing.T) {
	api := newSnapshotAPI(t)
	api.startTaskPool(t)
	inventory, template := api.addDynamicInventory(t, slowDynamicInventory)

	role, err := api.store.CreateRole(db.Role{
		Slug:        "inventory-editor",
		Name:        "Inventory editor",
		ProjectID:   &api.projectID,
		Permissions: db.CanManageProjectResources,
	})
	require.NoError(t, err)

	editor, err := api.store.CreateUser(db.UserWithPwd{
		Pwd:  "password-editor",
		User: db.User{Username: "editor", Name: "editor", Email: "editor@localhost"},
	})
	require.NoError(t, err)

	editorAPIToken, err := api.store.CreateAPIToken(db.APIToken{
		ID:     fmt.Sprintf("token-editor-%d", editor.ID),
		UserID: editor.ID,
	})
	require.NoError(t, err)
	editorToken := editorAPIToken.ID

	_, err = api.store.CreateProjectUser(db.ProjectUser{
		ProjectID: api.projectID,
		UserID:    editor.ID,
		Role:      db.ProjectUserRole(role.Slug),
	})
	require.NoError(t, err)

	w := api.request(http.MethodPost, api.refreshPath(inventory, template), editorToken)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 0, api.projectTaskCount(t), "a refused refresh must not have queued a task")
}

// ansibleRequiredEnv makes the absence of the binary a failure instead of a
// skip. Somewhere that is meant to run this test — CI above all — has to set
// it: a skipped test still prints "ok", so an environment that quietly stops
// running it looks exactly like one that runs it and passes.
const ansibleRequiredEnv = "SEMAPHORE_TEST_ANSIBLE_REQUIRED"

// requireAnsibleInventoryOnPath locates ansible-inventory and puts its
// directory on PATH.
//
// SEMAPHORE_TEST_ANSIBLE_INVENTORY is consulted first, because CI installs
// ansible into a venv and exports only that variable. PATH has to be extended
// on top of reading it: the binary is run by the task executor under test and
// not by the test itself, and the executor resolves the command by name.
//
// db_lib has the same helper. The two packages cannot share an unexported test
// helper, and exporting one only so tests can reach it is worse than the
// duplication.
func requireAnsibleInventoryOnPath(t *testing.T) {
	t.Helper()

	binary := os.Getenv("SEMAPHORE_TEST_ANSIBLE_INVENTORY")

	if binary == "" {
		found, err := exec.LookPath("ansible-inventory")
		if err != nil {
			missing := "ansible-inventory not found; set SEMAPHORE_TEST_ANSIBLE_INVENTORY to run this test"

			if os.Getenv(ansibleRequiredEnv) != "" {
				t.Fatal(missing + " (" + ansibleRequiredEnv + " is set, so skipping it is not allowed here)")
			}

			t.Skip(missing)
		}

		binary = found
	}

	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The whole loop: the dispatched task checks the repository out, runs
// ansible-inventory, and the resolved names come back through the API — with
// every host variable stripped on the runner side, so the secret planted in
// the script must be nowhere the picker can see.
func TestSnapshotAPI_ResolvesADynamicInventoryEndToEnd(t *testing.T) {
	requireAnsibleInventoryOnPath(t)

	api := newSnapshotAPI(t)
	api.startTaskPool(t)
	inventory, template := api.addDynamicInventory(t, fastDynamicInventory)

	w := api.request(http.MethodPost, api.refreshPath(inventory, template), api.memberToken)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	deadline := time.Now().Add(2 * time.Minute)

	var snapshot db.InventorySnapshot
	for {
		api.get(t, api.snapshotPath(inventory, ""), &snapshot)
		if snapshot.Status != db.InventorySnapshotStatusPending {
			break
		}
		if !time.Now().Before(deadline) {
			api.dumpProjectTasks(t)
			t.Fatal("the resolve task never finished")
		}
		time.Sleep(200 * time.Millisecond)
	}

	require.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 2, snapshot.HostCount)

	var hosts []db.InventorySnapshotHost
	api.get(t, api.snapshotPath(inventory, "/hosts"), &hosts)

	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	assert.ElementsMatch(t, []string{"web01", "web02"}, names)

	var groups []db.InventorySnapshotGroup
	api.get(t, api.snapshotPath(inventory, "/groups"), &groups)

	groupNames := make([]string, 0, len(groups))
	for _, group := range groups {
		groupNames = append(groupNames, group.Name)
	}
	assert.Contains(t, groupNames, "web")

	// The hostvar planted in the script must not survive the runner-side
	// reduction: not in the snapshot rows, not in the task log.
	assertStoreHoldsNo(t, api, "hostvar-secret")
}

// dumpProjectTasks prints every task with its log, so that a hanging resolve
// fails with the reason on screen instead of a bare timeout.
func (a *snapshotAPI) dumpProjectTasks(t *testing.T) {
	t.Helper()

	rows, err := a.store.GetProjectTasks(a.projectID, db.RetrieveQueryParams{})
	require.NoError(t, err)

	for _, row := range rows {
		t.Logf("task %d: status=%s", row.ID, row.Status)

		outputs, err := a.store.GetTaskOutputs(a.projectID, row.ID, db.RetrieveQueryParams{})
		require.NoError(t, err)

		for _, output := range outputs {
			t.Logf("  | %s", output.Output)
		}
	}
}

// assertStoreHoldsNo asserts that the given secret never reached any snapshot
// row or task log line.
func assertStoreHoldsNo(t *testing.T, api *snapshotAPI, secret string) {
	t.Helper()

	tasksRows, err := api.store.GetProjectTasks(api.projectID, db.RetrieveQueryParams{})
	require.NoError(t, err)

	for _, row := range tasksRows {
		outputs, err := api.store.GetTaskOutputs(api.projectID, row.ID, db.RetrieveQueryParams{})
		require.NoError(t, err)

		for _, output := range outputs {
			assert.NotContains(t, output.Output, secret)
		}
	}
}
