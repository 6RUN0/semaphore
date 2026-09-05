package sql

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/util"
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
		// Read the row instead of reusing the outer role, whose fields the
		// sibling subtests mutate: writing anything but the stored values
		// makes this a value change under some -run selections.
		stored, err := store.GetProjectRole(projA.ID, customSlug)
		require.NoError(t, err)

		require.NoError(t, store.UpdateProjectRole(projA.ID, stored))

		got, err := store.GetProjectRole(projA.ID, customSlug)
		require.NoError(t, err)
		assert.Equal(t, stored, got)
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

// afterQueryHook is a gorp logger: gorp reports every statement to it right
// after the statement completes, which is the only seam a test has to act
// between two statements of one store method.
type afterQueryHook func(query string)

func (h afterQueryHook) Printf(_ string, v ...any) {
	if len(v) < 2 {
		return
	}
	if query, ok := v[1].(string); ok {
		h(query)
	}
}

// newFileBackedTestStore is InitConfigCreateTestStore on a WAL file with two
// connections: a hook can then write while a cursor is still open on the
// other connection. The in-memory store keeps one connection, and a second
// one would open a second, empty database.
func newFileBackedTestStore(t *testing.T) *SqlDb {
	t.Helper()

	util.Config = &util.ConfigType{
		SQLite:  &util.DbConfig{Hostname: filepath.Join(t.TempDir(), "test.db")},
		Dialect: "sqlite",
		Log: &util.ConfigLog{
			Events: &util.EventLogType{},
			Tasks:  &util.TaskLogType{},
		},
		Process: &util.ConfigProcess{},
		Runners: &util.RunnersConfig{},
		Apps:    map[string]util.App{"ansible": {}, "bash": {}},
	}
	store := CreateDb(util.DbDriverSQLite)
	store.Connect()
	t.Cleanup(store.Close)

	_, err := store.Sql().Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	store.Sql().Db.SetMaxOpenConns(2)
	require.NoError(t, db.Migrate(store, nil))

	return store
}

// TestUpdateProjectRole_RoleRemovedAfterRead pins the HA case: another node
// deletes the role between a read and the write. Reporting success then
// leaves the caller believing a role exists that does not.
func TestUpdateProjectRole_RoleRemovedAfterRead(t *testing.T) {
	store := newFileBackedTestStore(t)

	proj, err := store.CreateProject(db.Project{Name: "p"})
	require.NoError(t, err)
	role, err := store.CreateRole(db.Role{Slug: "custom", Name: "Custom", ProjectID: &proj.ID})
	require.NoError(t, err)

	removed := false
	store.Sql().TraceOn("", afterQueryHook(func(query string) {
		q := strings.ToLower(query)
		if removed || !strings.HasPrefix(q, "select") || !strings.Contains(q, "role") {
			return
		}
		removed = true
		require.NoError(t, store.DeleteRole(role.Slug))
	}))
	t.Cleanup(store.Sql().TraceOff)

	role.Name = "Renamed"
	err = store.UpdateProjectRole(proj.ID, role)

	if err != nil {
		assert.ErrorIs(t, err, db.ErrNotFound)
		return
	}
	got, err := store.GetProjectRole(proj.ID, role.Slug)
	require.NoError(t, err, "update reported success for a role that no longer exists")
	assert.Equal(t, "Renamed", got.Name)
}
