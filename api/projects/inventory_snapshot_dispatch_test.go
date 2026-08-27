package projects

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/server"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Dispatching a resolve is the one snapshot path that causes code to run, so
// these pin its gates: no template means nothing to run the listing in (400),
// and no run permission means no way to make the project execute anything
// (403). The happy path needs a live task pool and is covered by the e2e
// tests in the api package.

// newDispatchFixture creates the full chain a template needs, plus a user who
// is a member of the project under the given role.
func newDispatchFixture(t *testing.T, role db.ProjectUserRole) (store *sql.SqlDb, inventory db.Inventory, template db.Template, user db.User) {
	t.Helper()

	store = sql.InitConfigCreateTestStore()

	// Templates only accept whitelisted apps; seed the ones these tests use.
	util.Config.Apps = map[string]util.App{"ansible": {}, "terraform": {}}

	project, err := store.CreateProject(db.Project{Name: "dispatch"})
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

	inventory, err = store.CreateInventory(db.Inventory{
		ProjectID: project.ID,
		Name:      "dynamic",
		Type:      db.InventoryFile,
		Inventory: "dynamic.py",
	})
	require.NoError(t, err)

	template, err = store.CreateTemplate(db.Template{
		Name:         "resolve",
		App:          db.AppAnsible,
		Playbook:     "test.yml",
		ProjectID:    project.ID,
		RepositoryID: repository.ID,
		InventoryID:  &inventory.ID,
	})
	require.NoError(t, err)

	user, err = store.CreateUser(db.UserWithPwd{
		Pwd:  "password",
		User: db.User{Username: "member", Email: "member@localhost", Name: "member"},
	})
	require.NoError(t, err)

	_, err = store.CreateProjectUser(db.ProjectUser{
		ProjectID: project.ID,
		UserID:    user.ID,
		Role:      role,
	})
	require.NoError(t, err)

	return
}

// newDispatchRequest builds the request the way the middleware chain would:
// inventory, user, project permissions and the store all live in the context.
func newDispatchRequest(target string, inventory db.Inventory, user db.User, permissions db.ProjectUserPermission, store db.Store) (*http.Request, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req = helpers.SetContextValue(req, "inventory", inventory)
	req = helpers.SetContextValue(req, "user", &user)
	req = helpers.SetContextValue(req, "permissions", permissions)
	req = helpers.SetContextValue(req, "store", store)

	return req, httptest.NewRecorder()
}

func TestRefreshDispatch_NeedsATemplate(t *testing.T) {
	svc := &mockInventorySnapshotService{err: server.InventoryNeedsRunnerError{InventoryID: 7}}
	ctrl := NewInventorySnapshotController(svc)

	// No template_id in the query: there is nothing to run the listing in, so
	// the request must die before any permission or pool is consulted.
	req, w := newSnapshotRequest(http.MethodPost, "/snapshot/refresh", contextInventory())
	ctrl.RefreshInventorySnapshot(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// A member who may edit inventories but not run tasks must not be able to make
// a runner execute the inventory program: the refresh subtree is gated on
// CanManageProjectResources, which says nothing about executing code.
func TestRefreshDispatch_RefusesWithoutRunPermission(t *testing.T) {
	store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)

	svc := &mockInventorySnapshotService{err: server.InventoryNeedsRunnerError{InventoryID: inventory.ID}}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newDispatchRequest(
		"/snapshot/refresh?template_id="+strconv.Itoa(template.ID),
		inventory,
		user,
		db.CanManageProjectResources,
		store,
	)
	ctrl.RefreshInventorySnapshot(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)

	tasks, err := store.GetProjectTasks(inventory.ProjectID, db.RetrieveQueryParams{})
	require.NoError(t, err)
	assert.Empty(t, tasks, "a refused refresh must not have queued a task")
}

// The resolve flag is only understood by the Ansible executor; any other app
// would run the template for real, so the dispatcher must refuse it no matter
// what permissions the caller holds.
func TestRefreshDispatch_RefusesANonAnsibleTemplate(t *testing.T) {
	store, inventory, template, user := newDispatchFixture(t, db.ProjectOwner)

	terraform, err := store.CreateTemplate(db.Template{
		Name:         "infra",
		App:          db.AppTerraform,
		Playbook:     "main.tf",
		ProjectID:    inventory.ProjectID,
		RepositoryID: template.RepositoryID,
		InventoryID:  &inventory.ID,
	})
	require.NoError(t, err)

	svc := &mockInventorySnapshotService{err: server.InventoryNeedsRunnerError{InventoryID: inventory.ID}}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newDispatchRequest(
		"/snapshot/refresh?template_id="+strconv.Itoa(terraform.ID),
		inventory,
		user,
		db.CanManageProjectResources|db.CanRunProjectTasks,
		store,
	)
	ctrl.RefreshInventorySnapshot(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	tasks, err := store.GetProjectTasks(inventory.ProjectID, db.RetrieveQueryParams{})
	require.NoError(t, err)
	assert.Empty(t, tasks, "a refused refresh must not have queued a task")
}

// The claim is what deduplicates resolves, so it must be checked before any
// task is created: a loser that reaches AddTask would run the inventory script
// again regardless of who won.
func TestRefreshDispatch_ClaimLoserCreatesNoTask(t *testing.T) {
	store, inventory, template, user := newDispatchFixture(t, db.ProjectOwner)

	// Somebody else's resolve is in flight.
	claimed, err := store.ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
	require.NoError(t, err)
	require.True(t, claimed)

	svc := &mockInventorySnapshotService{
		refreshErr: server.InventoryNeedsRunnerError{InventoryID: inventory.ID},
		snapshot: db.InventorySnapshot{
			InventoryID: inventory.ID,
			ProjectID:   inventory.ProjectID,
			Status:      db.InventorySnapshotStatusPending,
		},
	}
	ctrl := NewInventorySnapshotController(svc)

	// The request context carries no task pool, so reaching AddTask would
	// panic: coming back with a clean 200 is itself proof the pool was never
	// consulted.
	req, w := newDispatchRequest(
		"/snapshot/refresh?template_id="+strconv.Itoa(template.ID),
		inventory,
		user,
		db.CanManageProjectResources|db.CanRunProjectTasks,
		store,
	)

	ctrl.RefreshInventorySnapshot(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	tasks, err := store.GetProjectTasks(inventory.ProjectID, db.RetrieveQueryParams{})
	require.NoError(t, err)
	assert.Empty(t, tasks, "the loser of the claim must not queue a task")
}

func TestCanRunTemplate(t *testing.T) {
	t.Run("an admin may always run", func(t *testing.T) {
		store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)
		user.Admin = true

		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{})
		req, _ := newDispatchRequest("/snapshot/refresh", inventory, user, 0, store)

		assert.True(t, ctrl.canRunTemplate(req, inventory.ProjectID, template.ID))
	})

	t.Run("the project role may carry the permission", func(t *testing.T) {
		store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)

		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{})
		req, _ := newDispatchRequest("/snapshot/refresh", inventory, user, db.CanRunProjectTasks, store)

		assert.True(t, ctrl.canRunTemplate(req, inventory.ProjectID, template.ID))
	})

	t.Run("no permission from any source is refused", func(t *testing.T) {
		store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)

		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{})
		req, _ := newDispatchRequest("/snapshot/refresh", inventory, user, db.CanManageProjectResources, store)

		assert.False(t, ctrl.canRunTemplate(req, inventory.ProjectID, template.ID))
	})

	// A template can grant run permission to a custom role on top of what the
	// project gives; forgetting that OR would lock out exactly the users the
	// grant exists for.
	t.Run("a template grant on the role is honoured", func(t *testing.T) {
		store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)

		grantTemplateRunToCustomRole(t, store, inventory.ProjectID, template.ID, user.ID)

		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{})
		req, _ := newDispatchRequest("/snapshot/refresh", inventory, user, 0, store)

		assert.True(t, ctrl.canRunTemplate(req, inventory.ProjectID, template.ID))
	})

	// The grant sits on another template of the same project: it must not leak
	// onto this one.
	t.Run("a grant on another template does not count", func(t *testing.T) {
		store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)

		other, err := store.CreateTemplate(db.Template{
			Name:         "other",
			Playbook:     "other.yml",
			ProjectID:    template.ProjectID,
			RepositoryID: template.RepositoryID,
			InventoryID:  &inventory.ID,
		})
		require.NoError(t, err)

		grantTemplateRunToCustomRole(t, store, inventory.ProjectID, other.ID, user.ID)

		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{})
		req, _ := newDispatchRequest("/snapshot/refresh", inventory, user, 0, store)

		assert.False(t, ctrl.canRunTemplate(req, inventory.ProjectID, template.ID))
	})
}

// grantTemplateRunToCustomRole moves the user onto a fresh custom role of the
// project and grants that role CanRunProjectTasks on the given template — the
// only shape a template grant can take, since project__template_role references
// the role table and built-in roles have no row there.
func grantTemplateRunToCustomRole(t *testing.T, store *sql.SqlDb, projectID int, templateID int, userID int) {
	t.Helper()

	role, err := store.CreateRole(db.Role{
		Slug:      "deployer",
		Name:      "Deployer",
		ProjectID: &projectID,
	})
	require.NoError(t, err)

	require.NoError(t, store.UpdateProjectUser(db.ProjectUser{
		ProjectID: projectID,
		UserID:    userID,
		Role:      db.ProjectUserRole(role.Slug),
	}))

	_, err = store.CreateTemplateRole(db.TemplateRolePerm{
		ProjectID:   projectID,
		TemplateID:  templateID,
		RoleSlug:    role.Slug,
		Permissions: db.CanRunProjectTasks,
	})
	require.NoError(t, err)
}

// The task names the inventory to resolve, but the executor honours that only
// when the template allows its inventory to be overridden. Dispatching anyway
// makes the runner list the hosts of the template's own inventory while the
// result is stored under the requested one — which then answers every read of
// it, showing a picker hosts that are not in that inventory at all (review
// finding MEDIUM-1).
func TestRefreshDispatch_RefusesAnInventoryTheTemplateWouldNotUse(t *testing.T) {
	store, inventory, template, user := newDispatchFixture(t, db.ProjectOwner)

	other, err := store.CreateInventory(db.Inventory{
		ProjectID: inventory.ProjectID,
		Name:      "other-dynamic",
		Type:      db.InventoryFile,
		Inventory: "other.py",
	})
	require.NoError(t, err)

	svc := &mockInventorySnapshotService{err: server.InventoryNeedsRunnerError{InventoryID: other.ID}}
	ctrl := NewInventorySnapshotController(svc)

	// The template of the fixture carries no AnsibleTemplateParams, so its
	// inventory may not be overridden.
	req, w := newDispatchRequest(
		"/snapshot/refresh?template_id="+strconv.Itoa(template.ID),
		other,
		user,
		db.CanManageProjectResources|db.CanRunProjectTasks,
		store,
	)
	ctrl.RefreshInventorySnapshot(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "overridden", "the refusal must name the reason")

	tasks, err := store.GetProjectTasks(inventory.ProjectID, db.RetrieveQueryParams{})
	require.NoError(t, err)
	assert.Empty(t, tasks, "a refused refresh must not have queued a task")

	// The gate sits before the claim, so a refusal leaves nothing behind that a
	// later refresh would have to wait out.
	_, err = store.GetInventorySnapshot(other.ProjectID, other.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "no claim may be taken for a resolve that will not happen")
}

// A template that allows the override really does resolve the inventory it was
// asked for, so the dispatch must go through.
func TestRefreshDispatch_AcceptsAnotherInventoryWhenOverrideIsAllowed(t *testing.T) {
	store, inventory, _, user := newDispatchFixture(t, db.ProjectOwner)

	other, err := store.CreateInventory(db.Inventory{
		ProjectID: inventory.ProjectID,
		Name:      "other-dynamic",
		Type:      db.InventoryFile,
		Inventory: "other.py",
	})
	require.NoError(t, err)

	overriding, err := store.CreateTemplate(db.Template{
		Name:         "overriding",
		App:          db.AppAnsible,
		Playbook:     "test.yml",
		ProjectID:    inventory.ProjectID,
		RepositoryID: 1,
		InventoryID:  &inventory.ID,
		TaskParams:   db.MapStringAnyField{"allow_override_inventory": true},
	})
	require.NoError(t, err)

	// Somebody else's resolve is in flight, so the dispatcher reports its state
	// instead of creating a task: the context carries no task pool, and coming
	// back with a clean 200 proves the override gate was passed rather than
	// tripped.
	claimed, err := store.ClaimInventorySnapshotResolve(other.ProjectID, other.ID)
	require.NoError(t, err)
	require.True(t, claimed)

	svc := &mockInventorySnapshotService{
		refreshErr: server.InventoryNeedsRunnerError{InventoryID: other.ID},
		snapshot: db.InventorySnapshot{
			InventoryID: other.ID,
			ProjectID:   other.ProjectID,
			Status:      db.InventorySnapshotStatusPending,
		},
	}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newDispatchRequest(
		"/snapshot/refresh?template_id="+strconv.Itoa(overriding.ID),
		other,
		user,
		db.CanManageProjectResources|db.CanRunProjectTasks,
		store,
	)
	ctrl.RefreshInventorySnapshot(w, req)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// Every other error of this endpoint carries a JSON body with an "error" field;
// a bare status here left the one refusal a caller is most likely to hit as the
// odd one out (review finding m-10).
func TestRefreshDispatch_ForbiddenCarriesAnErrorBody(t *testing.T) {
	store, inventory, template, user := newDispatchFixture(t, db.ProjectGuest)

	svc := &mockInventorySnapshotService{err: server.InventoryNeedsRunnerError{InventoryID: inventory.ID}}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newDispatchRequest(
		"/snapshot/refresh?template_id="+strconv.Itoa(template.ID),
		inventory,
		user,
		db.CanManageProjectResources,
		store,
	)
	ctrl.RefreshInventorySnapshot(w, req)

	// The status is what the picker keys on; only the body is new.
	require.Equal(t, http.StatusForbidden, w.Code)

	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotEmpty(t, body.Error)
}
