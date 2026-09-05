package sql

import (
	"fmt"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReferenceQueriesForPrune pins the three inbound references that
// getObjectRefs does not cover: template roles, template views and template
// vault keys. Config sync prune refuses to delete an object that has any.
func TestReferenceQueriesForPrune(t *testing.T) {
	assertReferenceQueriesForPrune(t, InitConfigCreateTestStore())
}

// assertReferenceQueriesForPrune is shared with the dialect suites in
// object_refs_dialect_test.go, which run it on a persistent MySQL or
// PostgreSQL database: the role slug carries a per-run suffix so reruns do
// not collide on the slug primary key.
func assertReferenceQueriesForPrune(t *testing.T, store *SqlDb) {
	t.Helper()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	proj, err := store.CreateProject(db.Project{Name: "p-" + suffix})
	require.NoError(t, err)
	other, err := store.CreateProject(db.Project{Name: "other-" + suffix})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = store.DeleteProject(proj.ID)
		_ = store.DeleteProject(other.ID)
	})

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
	role, err := store.CreateRole(db.Role{Slug: "custom-" + suffix, Name: "Custom", ProjectID: &proj.ID})
	require.NoError(t, err)

	tpl, err := store.CreateTemplate(db.Template{
		Name: "t", Playbook: "p.yml", ProjectID: proj.ID,
		RepositoryID: repo.ID, InventoryID: &inv.ID, ViewID: &view.ID,
		Vaults: []db.TemplateVault{{Type: db.TemplateVaultPassword, VaultKeyID: &vaultKey.ID}},
	})
	require.NoError(t, err)

	_, err = store.CreateTemplateRole(db.TemplateRolePerm{ProjectID: proj.ID, TemplateID: tpl.ID, RoleSlug: role.Slug})
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

	t.Run("link rows naming a template of another project are ignored", func(t *testing.T) {
		// Both link tables carry their own project_id next to the template
		// id, and nothing keeps the two in step. A row whose project_id
		// disagrees with the template's own project would otherwise leak
		// that template's name into the referrer list of a project that
		// owns neither the template nor the link, and block a prune with a
		// name its operator cannot see.
		foreignKey, err := store.CreateAccessKey(db.AccessKey{ProjectID: &other.ID, Type: db.AccessKeyNone, Name: "none"})
		require.NoError(t, err)
		foreignRepo, err := store.CreateRepository(db.Repository{
			ProjectID: other.ID, SSHKeyID: foreignKey.ID,
			Name: "r", GitURL: "git@example.com:t/t", GitBranch: "main",
		})
		require.NoError(t, err)
		foreignInv, err := store.CreateInventory(db.Inventory{ProjectID: other.ID, Name: "i", Type: db.InventoryStatic})
		require.NoError(t, err)
		foreignTpl, err := store.CreateTemplate(db.Template{
			Name: "foreign", Playbook: "p.yml", ProjectID: other.ID,
			RepositoryID: foreignRepo.ID, InventoryID: &foreignInv.ID,
		})
		require.NoError(t, err)

		_, err = store.exec(
			"insert into project__template_role (template_id, role_slug, project_id) values (?, ?, ?)",
			foreignTpl.ID, role.Slug, proj.ID)
		require.NoError(t, err)

		_, err = store.exec(
			"insert into project__template_vault (project_id, template_id, vault_key_id, type) values (?, ?, ?, 'password')",
			proj.ID, foreignTpl.ID, vaultKey.ID)
		require.NoError(t, err)

		refs, err := store.GetProjectRoleRefs(proj.ID, role.Slug)
		require.NoError(t, err)
		assert.Equal(t, want, refs)

		refs, err = store.GetTemplateVaultRefsByKey(proj.ID, vaultKey.ID)
		require.NoError(t, err)
		assert.Equal(t, want, refs)
	})
}
