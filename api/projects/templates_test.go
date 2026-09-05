package projects

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacySurveyVars is the survey_vars document of a template written before
// ValidateSurveyVar refused a default for a secret variable. Both entries are
// needed: the secret one must lose its default, the plain one must keep it.
const legacySurveyVars = `[` +
	`{"name":"secret_var","type":"secret","default_value":"leaked-secret"},` +
	`{"name":"plain_var","default_value":"kept"}]`

// TestGetTemplate_HidesSecretSurveyDefaults pins that a default stored against
// a secret survey variable never reaches the client. Validation refuses such a
// default on save and the backup drops it, but rows written before that rule
// still carry one, and a template read served it as plain text.
func TestGetTemplate_HidesSecretSurveyDefaults(t *testing.T) {
	var surveyVars []db.SurveyVar
	require.NoError(t, json.Unmarshal([]byte(legacySurveyVars), &surveyVars))

	template := db.Template{ID: 1, ProjectID: 1, Name: "tpl", SurveyVars: surveyVars}

	req := httptest.NewRequest(http.MethodGet, "/api/project/1/templates/1", nil)
	req = helpers.SetContextValue(req, "template", template)
	req = helpers.SetContextValue(req, "permissions", db.ProjectUserPermission(0))
	w := httptest.NewRecorder()

	GetTemplate(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "leaked-secret")
	assert.Contains(t, w.Body.String(), "kept")
}

// TestGetTemplates_HidesSecretSurveyDefaults covers the list route, which
// reads the same rows through a different query.
func TestGetTemplates_HidesSecretSurveyDefaults(t *testing.T) {
	store := sql.InitConfigCreateTestStore()

	proj, err := store.CreateProject(db.Project{Name: "survey-secret"})
	require.NoError(t, err)

	user, err := store.CreateUser(db.UserWithPwd{
		Pwd:  "3412341234123",
		User: db.User{Username: "reader", Name: "reader", Email: "reader@example.com", Admin: true},
	})
	require.NoError(t, err)

	_, err = store.CreateProjectUser(db.ProjectUser{ProjectID: proj.ID, UserID: user.ID, Role: db.ProjectOwner})
	require.NoError(t, err)

	key, err := store.CreateAccessKey(db.AccessKey{ProjectID: &proj.ID, Name: "key", Type: db.AccessKeyNone})
	require.NoError(t, err)

	repo, err := store.CreateRepository(db.Repository{
		ProjectID: proj.ID, SSHKeyID: key.ID,
		Name: "repo", GitURL: "git@example.com:test/test", GitBranch: "master",
	})
	require.NoError(t, err)

	tpl, err := store.CreateTemplate(db.Template{
		Name: "tpl", Playbook: "test.yml", ProjectID: proj.ID, RepositoryID: repo.ID,
	})
	require.NoError(t, err)

	// CreateTemplate validates, so the pre-rule row is written straight to the
	// column the way an older Semaphore left it.
	_, err = store.Sql().Exec("update project__template set survey_vars = ? where id = ?", legacySurveyVars, tpl.ID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/project/1/templates", nil)
	req = helpers.SetContextValue(req, "project", proj)
	req = helpers.SetContextValue(req, "user", &user)
	req = helpers.SetContextValue(req, "store", store)
	w := httptest.NewRecorder()

	GetTemplates(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "leaked-secret")
	assert.Contains(t, w.Body.String(), "kept")
}
