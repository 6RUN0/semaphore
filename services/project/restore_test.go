package project

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db/sql"
	proFactory "github.com/semaphoreui/semaphore/pro/db/factory"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyDuplicate pins the off-by-one that let exactly two entities with
// the same name pass verification.
func TestVerifyDuplicate(t *testing.T) {
	env := func(name string) BackupEnvironment {
		return BackupEnvironment{Environment: db.Environment{Name: name}}
	}

	tests := []struct {
		name    string
		items   []BackupEnvironment
		wantErr bool
	}{
		{"single", []BackupEnvironment{env("a")}, false},
		{"distinct", []BackupEnvironment{env("a"), env("b")}, false},
		{"two equal", []BackupEnvironment{env("a"), env("a")}, true},
		{"three equal", []BackupEnvironment{env("a"), env("a"), env("a")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyDuplicate[BackupEnvironment]("a", tt.items)
			if tt.wantErr {
				assert.ErrorContains(t, err, "duplicate")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// restoreTestUser creates the owner a restore needs. Every restore attaches
// the project to a user, so each test wants a fresh one.
func restoreTestUser(t *testing.T, store db.Store, username string) db.User {
	t.Helper()
	user, err := store.CreateUser(db.UserWithPwd{
		Pwd: "3412341234123",
		User: db.User{
			Username: username,
			Name:     username,
			Email:    username + "@example.com",
			Admin:    true,
		},
	})
	require.NoError(t, err)
	return user
}

// TestBackupRoundTrip_GlobalTemplateRole pins the pairing of the two sides of
// a global template permission: what the export writes into
// templates[].roles[].role has to be what the restore looks up. Role.GetName
// returns the slug for a global role, so both sides speak slugs, and a global
// role whose name differs from its slug must survive the round trip.
func TestBackupRoundTrip_GlobalTemplateRole(t *testing.T) {
	util.Config = &util.ConfigType{TmpPath: "/tmp"}
	store := sql.InitConfigCreateTestStore()

	_, err := store.CreateRole(db.Role{Slug: "auditor-slug", Name: "Auditor"})
	require.NoError(t, err)

	proj, err := store.CreateProject(db.Project{Name: "global-role-source"})
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

	_, err = store.CreateTemplateRole(db.TemplateRolePerm{
		TemplateID: tpl.ID, ProjectID: proj.ID, RoleSlug: "auditor-slug",
	})
	require.NoError(t, err)

	backup, err := GetBackup(proj.ID, store, proFactory.NewWorkflowStore(store), BackupOptions{})
	require.NoError(t, err)
	require.Len(t, backup.Templates, 1)
	require.Len(t, backup.Templates[0].Roles, 1)
	assert.True(t, backup.Templates[0].Roles[0].IsGlobal)

	user := restoreTestUser(t, store, "restore-global-role")
	backup.Meta.Name = "global-role-restored"

	restored, err := backup.Restore(user, store, proFactory.NewWorkflowStore(store))
	require.NoError(t, err)

	templates, err := store.GetTemplates(restored.ID, db.TemplateFilter{}, db.RetrieveQueryParams{})
	require.NoError(t, err)
	require.Len(t, templates, 1)

	perms, err := store.GetTemplateRoles(restored.ID, templates[0].ID)
	require.NoError(t, err)
	require.Len(t, perms, 1)
	assert.Equal(t, "auditor-slug", perms[0].RoleSlug)
}
