package sql

import (
	"fmt"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectScopedRoleMutations pins that a role of project A cannot be
// changed or removed through project B: UpdateRole and DeleteRole key on
// slug alone, and config sync must not inherit that.
func TestProjectScopedRoleMutations(t *testing.T) {
	assertProjectScopedRoleMutations(t, InitConfigCreateTestStore())
}

// assertProjectScopedRoleMutations is shared with the dialect suites in
// role_dialect_test.go, which run it on a persistent MySQL or PostgreSQL
// database: slugs and names carry a per-run suffix so reruns do not collide
// on the slug primary key.
func assertProjectScopedRoleMutations(t *testing.T, store *SqlDb) {
	t.Helper()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	customSlug := "custom-" + suffix
	globalSlug := "global-" + suffix

	projA, err := store.CreateProject(db.Project{Name: "a-" + suffix})
	require.NoError(t, err)
	projB, err := store.CreateProject(db.Project{Name: "b-" + suffix})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = store.DeleteRole(globalSlug)
		_ = store.DeleteProject(projA.ID)
		_ = store.DeleteProject(projB.ID)
	})

	role, err := store.CreateRole(db.Role{Slug: customSlug, Name: "Custom", ProjectID: &projA.ID})
	require.NoError(t, err)

	t.Run("update through another project is refused", func(t *testing.T) {
		role.Name = "Renamed"
		err := store.UpdateProjectRole(projB.ID, role)
		assert.ErrorIs(t, err, db.ErrNotFound)

		got, err := store.GetProjectRole(projA.ID, customSlug)
		require.NoError(t, err)
		assert.Equal(t, "Custom", got.Name)
	})

	t.Run("update through the owning project works", func(t *testing.T) {
		role.Name = "Renamed"
		require.NoError(t, store.UpdateProjectRole(projA.ID, role))

		got, err := store.GetProjectRole(projA.ID, customSlug)
		require.NoError(t, err)
		assert.Equal(t, "Renamed", got.Name)
	})

	t.Run("update to identical values succeeds", func(t *testing.T) {
		// On MySQL an unchanged row reports zero affected rows; existence
		// must not be derived from that.
		require.NoError(t, store.UpdateProjectRole(projA.ID, role))
	})

	t.Run("delete through another project is refused", func(t *testing.T) {
		err := store.DeleteProjectRole(projB.ID, customSlug)
		assert.ErrorIs(t, err, db.ErrNotFound)

		_, err = store.GetProjectRole(projA.ID, customSlug)
		assert.NoError(t, err)
	})

	t.Run("global role is not reachable through a project", func(t *testing.T) {
		_, err := store.CreateRole(db.Role{Slug: globalSlug, Name: "Global"})
		require.NoError(t, err)

		assert.ErrorIs(t, store.DeleteProjectRole(projA.ID, globalSlug), db.ErrNotFound)
		_, err = store.GetGlobalRoleBySlug(globalSlug)
		assert.NoError(t, err)
	})

	t.Run("delete through the owning project works", func(t *testing.T) {
		require.NoError(t, store.DeleteProjectRole(projA.ID, customSlug))
		_, err := store.GetProjectRole(projA.ID, customSlug)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})
}
