package sql

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// dialectSuitesRan records that a dialect suite actually executed. TestMain
// below is the only reader; the suites themselves are the only writers, and
// they run one at a time in one binary.
//
// One flag per suite, not per engine: a shared per-engine flag lets a renamed
// suite hide behind its sibling, since either suite setting the engine's flag
// makes the engine look covered even though the renamed suite dropped out of
// the selector.
var dialectSuitesRan struct {
	inventorySnapshotMySQL    atomic.Bool
	inventorySnapshotPostgres atomic.Bool
	projectScopedRoleMySQL    atomic.Bool
	projectScopedRolePostgres atomic.Bool
}

// TestMain turns a suite that never ran into a failure when the environment
// demanded it.
//
// `go test -run <Name>` prints "testing: warning: no tests to run" and exits 0
// when nothing matches, so the selector the CI matrix and the Taskfile use
// stops selecting anything the moment a suite is renamed or removed — and every
// job in the matrix goes permanently green while testing nothing.
// SEMAPHORE_TEST_DIALECTS_REQUIRED does not catch that on its own: it is
// enforced inside newDialectTestStore, which a test that was never selected
// never reaches.
func TestMain(m *testing.M) {
	code := m.Run()

	if code == 0 && os.Getenv(envTestDialectsRequired) != "" {
		for _, suite := range []struct {
			envName string
			name    string
			ran     *atomic.Bool
		}{
			{envTestMySQL, "TestInventorySnapshotOnDialectMySQL", &dialectSuitesRan.inventorySnapshotMySQL},
			{envTestPostgres, "TestInventorySnapshotOnDialectPostgres", &dialectSuitesRan.inventorySnapshotPostgres},
			{envTestMySQL, "TestProjectScopedRoleMutationsOnDialectMySQL", &dialectSuitesRan.projectScopedRoleMySQL},
			{envTestPostgres, "TestProjectScopedRoleMutationsOnDialectPostgres", &dialectSuitesRan.projectScopedRolePostgres},
		} {
			if os.Getenv(suite.envName) == "" || suite.ran.Load() {
				continue
			}

			fmt.Fprintf(os.Stderr,
				"%s is set and %s demands the dialect tests, but %s did not run\n",
				suite.envName, envTestDialectsRequired, suite.name)

			code = 1
		}
	}

	os.Exit(code)
}
