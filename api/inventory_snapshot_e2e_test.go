package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/server"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This exercises the inventory snapshot through the whole server: the real
// route table, the real authentication and project middlewares, the real
// service, the real parser and a real database. The layers below have their
// own tests; what only this level can prove is that they are wired together,
// and that the wiring does not hand one project's hosts to another.

// snapshotAPI is a running API with a database behind it and two projects in
// it, so that isolation between them can be tested rather than assumed.
type snapshotAPI struct {
	router http.Handler
	store  *sql.SqlDb

	memberToken   string
	outsiderToken string
	adminToken    string

	projectID      int
	otherProjectID int
}

func newSnapshotAPI(t *testing.T) *snapshotAPI {
	t.Helper()

	util.Config = &util.ConfigType{
		SQLite:    &util.DbConfig{Hostname: ":memory:"},
		Dialect:   "sqlite",
		Debugging: &util.DebuggingConfig{},
		Log: &util.ConfigLog{
			Events: &util.EventLogType{},
			Tasks:  &util.TaskLogType{},
		},
		Process: &util.ConfigProcess{},
	}

	store := sql.CreateDb(util.DbDriverSQLite)
	store.Connect()
	require.NoError(t, db.Migrate(store, nil))

	api := &snapshotAPI{store: store}

	// Route builds every controller up front; the ones this test never calls
	// are content with a nil dependency. The snapshot service, however, needs
	// the real key installer: fingerprinting a repository-backed inventory
	// installs the repository key before asking git for the remote head.
	encryption := server.NewAccessKeyEncryptionService(store, store, store, store)

	route := Route(
		store, nil, nil, nil, nil, nil, nil, encryption,
		server.NewAccessKeyInstallationService(encryption),
		nil, nil, nil, nil, nil, nil, nil, nil,
	)

	// The server injects the store per request from cli/cmd/root.go; without
	// the same wrapper every handler below would panic on a missing store.
	route.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, helpers.SetContextValue(r, "store", store))
		})
	})
	api.router = route

	project, err := store.CreateProject(db.Project{Name: "snapshot-e2e"})
	require.NoError(t, err)
	api.projectID = project.ID

	otherProject, err := store.CreateProject(db.Project{Name: "snapshot-e2e-other"})
	require.NoError(t, err)
	api.otherProjectID = otherProject.ID

	api.memberToken = api.addUser(t, "member", false, &project.ID)
	api.outsiderToken = api.addUser(t, "outsider", false, nil)
	api.adminToken = api.addUser(t, "root", true, nil)

	return api
}

// addUser creates a user with an API token, optionally as a member of a
// project, and returns the token.
func (a *snapshotAPI) addUser(t *testing.T, name string, admin bool, projectID *int) string {
	t.Helper()

	user, err := a.store.CreateUser(db.UserWithPwd{
		Pwd: "password-" + name,
		User: db.User{
			Username: name,
			Name:     name,
			Email:    name + "@localhost",
			Admin:    admin,
		},
	})
	require.NoError(t, err)

	if projectID != nil {
		_, err = a.store.CreateProjectUser(db.ProjectUser{
			ProjectID: *projectID,
			UserID:    user.ID,
			Role:      db.ProjectOwner,
		})
		require.NoError(t, err)
	}

	// The authorization header is lower-cased before the token is looked up,
	// so an id with capitals could never be presented successfully.
	token, err := a.store.CreateAPIToken(db.APIToken{
		ID:     fmt.Sprintf("token-%s-%d", name, user.ID),
		UserID: user.ID,
	})
	require.NoError(t, err)

	return token.ID
}

func (a *snapshotAPI) addInventory(t *testing.T, projectID int, inventoryType db.InventoryType, content string) db.Inventory {
	t.Helper()

	inventory, err := a.store.CreateInventory(db.Inventory{
		ProjectID: projectID,
		Name:      fmt.Sprintf("hosts-%d-%s", projectID, inventoryType),
		Type:      inventoryType,
		Inventory: content,
	})
	require.NoError(t, err)

	return inventory
}

func (a *snapshotAPI) request(method string, path string, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "bearer "+token)
	}

	w := httptest.NewRecorder()
	a.router.ServeHTTP(w, req)

	return w
}

// get issues an authenticated GET as the project member and requires it to
// succeed, which is the common case in the assertions below.
func (a *snapshotAPI) get(t *testing.T, path string, out any) {
	t.Helper()

	w := a.request(http.MethodGet, path, a.memberToken)
	require.Equal(t, http.StatusOK, w.Code, "GET %s: %s", path, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), out))
}

func (a *snapshotAPI) snapshotPath(inventory db.Inventory, suffix string) string {
	return fmt.Sprintf(
		"/api/project/%d/inventory/%d/snapshot%s",
		inventory.ProjectID, inventory.ID, suffix,
	)
}

const e2eInventory = `bastion.example.com

[web]
web[01:03].example.com

[db]
db01

[prod:children]
web
db
`

func TestSnapshotAPI_BuildsTheHostListOnFirstRead(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, e2eInventory)

	var snapshot db.InventorySnapshot
	api.get(t, api.snapshotPath(inventory, ""), &snapshot)

	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 5, snapshot.HostCount)
	assert.Empty(t, snapshot.Error)

	var groups []db.InventorySnapshotGroup
	api.get(t, api.snapshotPath(inventory, "/groups"), &groups)

	counts := map[string]int{}
	for _, group := range groups {
		counts[group.Name] = group.HostCount
	}

	// prod holds no hosts of its own; its size is the union of its children.
	assert.Equal(t, map[string]int{"web": 3, "db": 1, "prod": 4, "ungrouped": 1}, counts)
}

func TestSnapshotAPI_FiltersHosts(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, e2eInventory)

	for _, tt := range []struct {
		name     string
		query    string
		expected []string
	}{
		{
			name:     "everything",
			query:    "",
			expected: []string{"bastion.example.com", "db01", "web01.example.com", "web02.example.com", "web03.example.com"},
		},
		{
			name:     "by group",
			query:    "?group=web",
			expected: []string{"web01.example.com", "web02.example.com", "web03.example.com"},
		},
		{
			name:     "by a parent group",
			query:    "?group=prod",
			expected: []string{"db01", "web01.example.com", "web02.example.com", "web03.example.com"},
		},
		{
			name:     "by name",
			query:    "?search=web0",
			expected: []string{"web01.example.com", "web02.example.com", "web03.example.com"},
		},
		{
			name:     "by group and name together",
			query:    "?group=prod&search=01",
			expected: []string{"db01", "web01.example.com"},
		},
		{
			name:     "paged",
			query:    "?group=web&limit=2",
			expected: []string{"web01.example.com", "web02.example.com"},
		},
		{
			name:     "paged past the first page",
			query:    "?group=web&limit=2&offset=2",
			expected: []string{"web03.example.com"},
		},
		{
			name:     "no such group",
			query:    "?group=staging",
			expected: []string{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var hosts []db.InventorySnapshotHost
			api.get(t, api.snapshotPath(inventory, "/hosts")+tt.query, &hosts)

			names := make([]string, 0, len(hosts))
			for _, host := range hosts {
				names = append(names, host.Name)
			}

			assert.ElementsMatch(t, tt.expected, names)
		})
	}
}

// A user who edits the inventory expects the picker to show the new hosts
// without being told to press anything.
func TestSnapshotAPI_FollowsTheInventoryContent(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, "[web]\nweb01\n")

	var snapshot db.InventorySnapshot
	api.get(t, api.snapshotPath(inventory, ""), &snapshot)
	require.Equal(t, 1, snapshot.HostCount)

	inventory.Inventory = "[web]\nweb01\nweb02\nweb03\n"
	require.NoError(t, api.store.UpdateInventory(inventory))

	api.get(t, api.snapshotPath(inventory, ""), &snapshot)
	assert.Equal(t, 3, snapshot.HostCount)

	var hosts []db.InventorySnapshotHost
	api.get(t, api.snapshotPath(inventory, "/hosts"), &hosts)
	assert.Len(t, hosts, 3)
}

func TestSnapshotAPI_RefreshRebuildsAnUnchangedInventory(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, e2eInventory)

	var first db.InventorySnapshot
	api.get(t, api.snapshotPath(inventory, ""), &first)

	w := api.request(http.MethodPost, api.snapshotPath(inventory, "/refresh"), api.memberToken)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var refreshed db.InventorySnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &refreshed))
	assert.Equal(t, db.InventorySnapshotStatusOK, refreshed.Status)
	assert.Equal(t, first.HostCount, refreshed.HostCount)
}

// An inventory the server cannot read must say so rather than answer with an
// empty list, which the picker would render as "this inventory has no hosts".
func TestSnapshotAPI_ReportsInventoriesItCannotEnumerate(t *testing.T) {
	api := newSnapshotAPI(t)

	for _, tt := range []struct {
		name          string
		inventoryType db.InventoryType
		content       string
		status        db.InventorySnapshotStatus
	}{
		{"a path to a file on the runner", db.InventoryFile, "inventories/prod", db.InventorySnapshotStatusUnsupported},
		{"a broken host range", db.InventoryStatic, "[web]\nweb[01:100]\n", db.InventorySnapshotStatusError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inventory := api.addInventory(t, api.projectID, tt.inventoryType, tt.content)

			var snapshot db.InventorySnapshot
			api.get(t, api.snapshotPath(inventory, ""), &snapshot)
			assert.Equal(t, tt.status, snapshot.Status)
			assert.Zero(t, snapshot.HostCount)

			var hosts []db.InventorySnapshotHost
			api.get(t, api.snapshotPath(inventory, "/hosts"), &hosts)
			assert.Empty(t, hosts)

			w := api.request(http.MethodGet, api.snapshotPath(inventory, "/hosts"), api.memberToken)
			assert.JSONEq(t, `[]`, w.Body.String(), "an empty list must not serialise as null")
		})
	}
}

// The controller reads the inventory the middleware resolved, and the
// middleware resolves it within the project from the path. Addressing another
// project's inventory therefore has to fail rather than reveal its hosts.
func TestSnapshotAPI_DoesNotLeakAcrossProjects(t *testing.T) {
	api := newSnapshotAPI(t)
	foreign := api.addInventory(t, api.otherProjectID, db.InventoryStatic, e2eInventory)

	for _, suffix := range []string{"", "/hosts", "/groups"} {
		t.Run("GET"+suffix, func(t *testing.T) {
			// The member's own project in the path, the other project's
			// inventory id after it.
			path := fmt.Sprintf(
				"/api/project/%d/inventory/%d/snapshot%s",
				api.projectID, foreign.ID, suffix,
			)

			w := api.request(http.MethodGet, path, api.memberToken)

			assert.Equal(t, http.StatusNotFound, w.Code)
			assert.NotContains(t, w.Body.String(), "web01.example.com")
		})
	}

	t.Run("POST/refresh", func(t *testing.T) {
		path := fmt.Sprintf(
			"/api/project/%d/inventory/%d/snapshot/refresh",
			api.projectID, foreign.ID,
		)

		w := api.request(http.MethodPost, path, api.memberToken)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestSnapshotAPI_RequiresAuthentication(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, e2eInventory)

	for _, tt := range []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"unknown token", "token-that-was-never-issued"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, suffix := range []string{"", "/hosts", "/groups"} {
				w := api.request(http.MethodGet, api.snapshotPath(inventory, suffix), tt.token)

				assert.Equal(t, http.StatusUnauthorized, w.Code, "GET %s", suffix)
				assert.NotContains(t, w.Body.String(), "web01.example.com")
			}

			w := api.request(http.MethodPost, api.snapshotPath(inventory, "/refresh"), tt.token)
			assert.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}
}

// Authentication alone is not enough: a user outside the project must not read
// the hosts of a project they were never added to.
func TestSnapshotAPI_RefusesNonMembers(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, e2eInventory)

	for _, suffix := range []string{"", "/hosts", "/groups"} {
		w := api.request(http.MethodGet, api.snapshotPath(inventory, suffix), api.outsiderToken)

		// The exact code, not merely "not 200": a test that accepts anything
		// else stops telling a closed door from a broken endpoint.
		assert.Equal(t, http.StatusNotFound, w.Code, "GET %s must not succeed", suffix)
		assert.NotContains(t, w.Body.String(), "web01.example.com")
	}
}

// Every route has to be reachable; a handler registered under a path nobody
// can address is the failure mode this catches.
func TestSnapshotAPI_RoutesAreRegistered(t *testing.T) {
	api := newSnapshotAPI(t)
	inventory := api.addInventory(t, api.projectID, db.InventoryStatic, e2eInventory)

	route := Route(
		api.store, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)

	for _, tt := range []struct {
		method string
		suffix string
	}{
		{http.MethodGet, ""},
		{http.MethodHead, ""},
		{http.MethodGet, "/hosts"},
		{http.MethodGet, "/groups"},
		{http.MethodPost, "/refresh"},
	} {
		t.Run(tt.method+tt.suffix, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, api.snapshotPath(inventory, tt.suffix), nil)

			var match mux.RouteMatch
			require.True(t, route.Match(req, &match), "no route serves this request")
			assert.NoError(t, match.MatchErr)
		})
	}

	t.Run("refresh rejects GET", func(t *testing.T) {
		w := api.request(http.MethodGet, api.snapshotPath(inventory, "/refresh"), api.memberToken)

		// 404 rather than 405: the route is registered for POST only, and the
		// subrouter it lives under has no other handler for this path, so mux
		// reports it as unrouted.
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}
