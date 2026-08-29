package sql

import (
	"testing"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A migration that creates four tables has to survive being run twice.
//
// ApplyMigration ends the process with log.Fatal when a statement fails, so the
// rollback file never runs, and MySQL commits the DDL that already succeeded
// regardless of the surrounding transaction. A failure on the third table
// therefore leaves the first two behind with no migrations row to say so, and
// the next start replays the file from its first statement. Without
// "if not exists" that replay fails forever on "table already exists", and the
// server never comes up again.
func TestMigration2_20_2CanBeReplayedAfterPartialApply(t *testing.T) {
	store := newSnapshotTestStore(t)

	queries := getVersionSQL(util.DbDriverSQLite, "v2.20.2.sql", false)
	require.NotEmpty(t, queries)

	for _, query := range queries {
		q := store.prepareMigration(query)
		if q == "" {
			continue
		}

		_, err := store.Sql().Exec(q)
		require.NoError(t, err, "replaying %q must not fail", q)
	}
}

// The MySQL branch of prepareMigration strips "if exists" so that statements
// written for SQLite/PostgreSQL still parse. The pattern is anchored on word
// boundaries and must leave "if not exists" alone — stripping it there would
// put the replay defect above straight back on MySQL, the one engine that
// cannot roll a failed DDL back.
func TestMigration2_20_2KeepsIfNotExistsOnMysql(t *testing.T) {
	queries := getVersionSQL("mysql", "v2.20.2.sql", false)

	require.NotEmpty(t, queries)
	for _, query := range queries {
		// Splitting on ";\n" leaves an empty tail after the last statement.
		if query == "" {
			continue
		}

		assert.Contains(t, query, "create table if not exists")
		assert.Equal(t, query, ifExistsRE.ReplaceAllString(query, ""),
			"the MySQL rewrite must leave \"if not exists\" untouched")
	}
}
