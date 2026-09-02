package projects

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	proFactory "github.com/semaphoreui/semaphore/pro/db/factory"
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
