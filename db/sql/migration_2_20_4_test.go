package sql

import (
	"testing"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// project__inventory_snapshot.task_id references task(id) with "on delete set
// null". InnoDB indexes a foreign key by itself, PostgreSQL does not: without
// this index every row deleted from `task` — history cleanup deletes them in
// bulk — scans the snapshot table looking for referencing rows.
func TestMigration2_20_4CreatesTheSnapshotTaskIndex(t *testing.T) {
	store := newSnapshotTestStore(t)

	count, err := store.Sql().SelectInt(
		"select count(1) from sqlite_master where type = 'index' " +
			"and name = 'project__inventory_snapshot__task'")

	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// Dropping an index is the one statement whose syntax genuinely differs between
// the engines; see the same test for 2.20.3.
func TestMigration2_20_4RollbackRendersPerDialect(t *testing.T) {
	tests := []struct {
		dialect  string
		expected string
	}{
		{"mysql", "drop index `project__inventory_snapshot__task` on `project__inventory_snapshot`"},
		{util.DbDriverSQLite, "drop index if exists `project__inventory_snapshot__task`"},
		{"postgres", "drop index if exists `project__inventory_snapshot__task`"},
	}

	for _, tt := range tests {
		t.Run(tt.dialect, func(t *testing.T) {
			queries := getVersionSQL(tt.dialect, "v2.20.4.err.sql", false)

			require.NotEmpty(t, queries)
			assert.Equal(t, tt.expected, queries[0])
		})
	}
}
