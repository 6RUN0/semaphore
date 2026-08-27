package sql

import (
	"testing"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 2.20.3 exists for GetUnfinishedTasks, which reads `task` -- the
// largest table of a typical installation -- once per server start. Without
// the index that is a full scan.
func TestMigration2_20_3CreatesTheStatusIndex(t *testing.T) {
	store := newSnapshotTestStore(t)

	count, err := store.Sql().SelectInt(
		"select count(1) from sqlite_master where type = 'index' and name = 'task__status'")

	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the index the recovery query relies on must exist")
}

// The rollback of an index is the one statement whose syntax genuinely differs
// between the engines: MySQL wants the table named and has no "if exists" for
// an index, while SQLite and PostgreSQL want "if exists" and no table. The
// dialect template is what reconciles them, so pin what it renders.
func TestMigration2_20_3RollbackRendersPerDialect(t *testing.T) {
	tests := []struct {
		dialect  string
		expected string
	}{
		{"mysql", "drop index `task__status` on `task`"},
		{util.DbDriverSQLite, "drop index if exists `task__status`"},
		{"postgres", "drop index if exists `task__status`"},
	}

	for _, tt := range tests {
		t.Run(tt.dialect, func(t *testing.T) {
			queries := getVersionSQL(tt.dialect, "v2.20.3.err.sql", false)

			require.NotEmpty(t, queries)
			assert.Equal(t, tt.expected, queries[0])
		})
	}
}
