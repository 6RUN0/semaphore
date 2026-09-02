package sql

import "testing"

// The role mutations depend on RowsAffected semantics that SQLite cannot
// exercise: MySQL counts changed rows, not matched rows, after UPDATE.
func TestProjectScopedRoleMutationsOnDialectMySQL(t *testing.T) {
	dialectSuitesRan.mysql.Store(true)

	assertProjectScopedRoleMutations(t, newDialectTestStore(t, "mysql", envTestMySQL))
}

func TestProjectScopedRoleMutationsOnDialectPostgres(t *testing.T) {
	dialectSuitesRan.postgres.Store(true)

	assertProjectScopedRoleMutations(t, newDialectTestStore(t, "postgres", envTestPostgres))
}
