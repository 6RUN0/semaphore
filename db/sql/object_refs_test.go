package sql

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReferenceQueriesForPrune pins the three inbound references that
// getObjectRefs does not cover: template roles, template views and template
// vault keys. Config sync prune refuses to delete an object that has any.
func TestReferenceQueriesForPrune(t *testing.T) {
	store := InitConfigCreateTestStore()

	proj, err := store.CreateProject(db.Project{Name: "p"})
	require.NoError(t, err)

	key, err := store.CreateAccessKey(db.AccessKey{ProjectID: &proj.ID, Type: db.AccessKeyNone, Name: "none"})
	require.NoError(t, err)
	vaultKey, err := store.CreateAccessKey(db.AccessKey{ProjectID: &proj.ID, Type: db.AccessKeyLoginPassword, Name: "vault"})
	require.NoError(t, err)

	repo, err := store.CreateRepository(db.Repository{ProjectID: proj.ID, SSHKeyID: key.ID, Name: "r", GitURL: "git@example.com:t/t", GitBranch: "main"})
	require.NoError(t, err)
	inv, err := store.CreateInventory(db.Inventory{ProjectID: proj.ID, Name: "i", Type: db.InventoryStatic})
	require.NoError(t, err)
	view, err := store.CreateView(db.View{ProjectID: proj.ID, Title: "v"})
	require.NoError(t, err)
	role, err := store.CreateRole(db.Role{Slug: "custom1", Name: "Custom", ProjectID: &proj.ID})
	require.NoError(t, err)

	tpl, err := store.CreateTemplate(db.Template{
		Name: "t", Playbook: "p.yml", ProjectID: proj.ID,
		RepositoryID: repo.ID, InventoryID: &inv.ID, ViewID: &view.ID,
		Vaults: []db.TemplateVault{{Type: db.TemplateVaultPassword, VaultKeyID: &vaultKey.ID}},
	})
	require.NoError(t, err)

	_, err = store.CreateTemplateRole(db.TemplateRolePerm{ProjectID: proj.ID, TemplateID: tpl.ID, RoleSlug: role.Slug})
	require.NoError(t, err)

	other, err := store.CreateProject(db.Project{Name: "other"})
	require.NoError(t, err)

	want := []db.ObjectReferrer{{ID: tpl.ID, Name: "t"}}

	t.Run("role refs", func(t *testing.T) {
		refs, err := store.GetProjectRoleRefs(proj.ID, role.Slug)
		require.NoError(t, err)
		assert.Equal(t, want, refs)

		refs, err = store.GetProjectRoleRefs(other.ID, role.Slug)
		require.NoError(t, err)
		assert.Empty(t, refs)
	})

	t.Run("view refs", func(t *testing.T) {
		refs, err := store.GetViewRefs(proj.ID, view.ID)
		require.NoError(t, err)
		assert.Equal(t, want, refs)

		refs, err = store.GetViewRefs(other.ID, view.ID)
		require.NoError(t, err)
		assert.Empty(t, refs)
	})

	t.Run("vault key refs", func(t *testing.T) {
		refs, err := store.GetTemplateVaultRefsByKey(proj.ID, vaultKey.ID)
		require.NoError(t, err)
		assert.Equal(t, want, refs)

		refs, err = store.GetTemplateVaultRefsByKey(proj.ID, key.ID)
		require.NoError(t, err)
		assert.Empty(t, refs)

		refs, err = store.GetTemplateVaultRefsByKey(other.ID, vaultKey.ID)
		require.NoError(t, err)
		assert.Empty(t, refs)
	})
}
