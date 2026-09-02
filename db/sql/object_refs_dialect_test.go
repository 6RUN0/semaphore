package sql

import "testing"

// The reference queries join across three tables with column aliases that
// each engine quotes and cases in its own way; SQLite alone cannot show that
// the joins resolve on MySQL and PostgreSQL.
func TestReferenceQueriesForPruneOnDialectMySQL(t *testing.T) {
	dialectSuitesRan.referenceQueriesMySQL.Store(true)

	assertReferenceQueriesForPrune(t, newDialectTestStore(t, "mysql", envTestMySQL))
}

func TestReferenceQueriesForPruneOnDialectPostgres(t *testing.T) {
	dialectSuitesRan.referenceQueriesPostgres.Store(true)

	assertReferenceQueriesForPrune(t, newDialectTestStore(t, "postgres", envTestPostgres))
}
