package projects

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	proFactory "github.com/semaphoreui/semaphore/pro/db/factory"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetBackup_IntegrationAliasesOnlyOnRequest pins the query flag that
// lets a backup carry webhook aliases; without it the download holds none,
// since the alias is the whole secret of the public webhook route.
func TestGetBackup_IntegrationAliasesOnlyOnRequest(t *testing.T) {
	store := sql.InitConfigCreateTestStore()
	proj, err := store.CreateProject(db.Project{Name: "backup-aliases"})
	require.NoError(t, err)
	_, err = store.CreateIntegrationAlias(db.IntegrationAlias{Alias: "projectalias0003", ProjectID: proj.ID})
	require.NoError(t, err)

	ctrl := NewBackupController(proFactory.NewWorkflowStore(store))

	get := func(url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req = helpers.SetContextValue(req, "project", proj)
		req = helpers.SetContextValue(req, "store", store)
		w := httptest.NewRecorder()
		ctrl.GetBackup(w, req)
		return w
	}

	w := get("/api/project/1/backup")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "projectalias0003")

	w = get("/api/project/1/backup?include_integration_aliases=true")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "projectalias0003")
}

// endlessBody never reaches EOF: what an attacker sends, and what the plain
// io.Copy into the decoder would follow forever.
type endlessBody struct{}

func (endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestRestore_BodyLimit pins the cap on the restore body. The route is open to
// any user once NonAdminCanCreateProject is on, and the handler copied the
// whole body into memory before it looked at it, so one endless body was
// enough to exhaust the server.
func TestRestore_BodyLimit(t *testing.T) {
	util.Config = &util.ConfigType{TmpPath: "/tmp"}
	store := sql.InitConfigCreateTestStore()

	user, err := store.CreateUser(db.UserWithPwd{
		Pwd:  "3412341234123",
		User: db.User{Username: "restorer", Name: "restorer", Email: "restorer@example.com", Admin: true},
	})
	require.NoError(t, err)

	ctrl := NewBackupController(proFactory.NewWorkflowStore(store))

	post := func(body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/restore", body)
		req = helpers.SetContextValue(req, "user", &user)
		req = helpers.SetContextValue(req, "store", store)
		w := httptest.NewRecorder()
		ctrl.Restore(w, req)
		return w
	}

	w := post(endlessBody{})
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	// The body never reached the decoder, so the answer must not be a
	// complaint about its syntax.
	assert.NotContains(t, w.Body.String(), "invalid character")

	// A backup under the cap still restores; the limit must not cost the
	// route its normal traffic.
	w = post(strings.NewReader(`{"meta": {"name": "restore-under-the-cap"}}`))
	assert.Equal(t, http.StatusOK, w.Code)
}
