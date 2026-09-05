package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/ssh"
	"github.com/semaphoreui/semaphore/pkg/tz"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file inventory lives in a repository, so these run against a real git
// repository served over file://. That exercises the parts a fake client cannot
// vouch for — that the checkout lands where the reader looks, and that asking
// for the remote commit is enough to decide whether to fetch at all.

func gitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@localhost",
	)

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// newInventoryRepository builds a git repository holding the given files and
// returns its path.
func newInventoryRepository(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	gitCommand(t, dir, "init", "--initial-branch=main")

	commitFiles(t, dir, files, "initial")

	return dir
}

func commitFiles(t *testing.T, dir string, files map[string]string, message string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	gitCommand(t, dir, "add", "-A")
	gitCommand(t, dir, "commit", "-m", message)
}

// setupSourceConfig points the checkout scratch space at a directory the test
// owns, so that nothing is written outside it.
func setupSourceConfig(t *testing.T) {
	t.Helper()

	util.Config = &util.ConfigType{
		TmpPath: t.TempDir(),
		// ConfigInit fills this from the default tag; a hand-built config
		// leaves it at 0 and git aborts on "--jobs 0".
		GitSubmoduleJobs: 1,
		Log: &util.ConfigLog{
			Events: &util.EventLogType{},
			Tasks:  &util.TaskLogType{},
		},
		Process: &util.ConfigProcess{},
	}
}

func fileInventory(path string, repositoryID *int) db.Inventory {
	return db.Inventory{
		ID:           7,
		ProjectID:    3,
		Type:         db.InventoryFile,
		Inventory:    path,
		RepositoryID: repositoryID,
	}
}

func repositoryAt(dir string) db.Repository {
	return db.Repository{
		ID:        1,
		ProjectID: 3,
		GitURL:    "file://" + dir,
		GitBranch: "main",
	}
}

type sourceHarness struct {
	service   InventorySnapshotService
	snapshots *mockSnapshotManager
	inventory *mockInventoryManagerForSnapshot
	templates *mockTemplateManager
}

func newSourceHarness(t *testing.T, inventory db.Inventory, repository db.Repository) *sourceHarness {
	t.Helper()
	setupSourceConfig(t)

	inventories := &mockInventoryManagerForSnapshot{inventory: inventory}
	snapshots := &mockSnapshotManager{}
	templates := &mockTemplateManager{}

	return &sourceHarness{
		service: NewInventorySnapshotService(
			inventories,
			snapshots,
			templates,
			&mockRepositoryManager{repository: repository},
			ssh.KeyInstaller{},
		),
		snapshots: snapshots,
		inventory: inventories,
		templates: templates,
	}
}

// claim primes the mock with a pending resolve claim owned by taskID, the
// state a real StoreResolved call always finds: production claims and
// assigns a task before it ever runs (see mockSnapshotManager.SetResolvedInventorySnapshot).
func (h *sourceHarness) claim(projectID, inventoryID, taskID int) {
	h.snapshots.stored = &db.InventorySnapshot{
		InventoryID: inventoryID,
		ProjectID:   projectID,
		Status:      db.InventorySnapshotStatusPending,
		TaskID:      &taskID,
	}
}

func repositoryID(id int) *int { return &id }

func TestFileInventoryIsReadFromRepository(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{
		"inventories/hosts": "[web]\nweb01\nweb02\n\n[db]\ndb01\n",
	})

	h := newSourceHarness(t, fileInventory("inventories/hosts", repositoryID(1)), repositoryAt(dir))

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err, snapshot.Error)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status, snapshot.Error)
	assert.Equal(t, 3, snapshot.HostCount)
	assert.ElementsMatch(t, []string{"web01", "web02", "db01"}, h.snapshots.content.Hosts)
}

func TestFileInventoryReadsDirectory(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{
		"inventories/01-web":   "[web]\nweb01\n",
		"inventories/02-db":    "[db]\ndb01\n",
		"inventories/notes.md": "[ghost]\nghost01\n",
	})

	h := newSourceHarness(t, fileInventory("inventories", repositoryID(1)), repositoryAt(dir))

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status, snapshot.Error)
	assert.ElementsMatch(t, []string{"web01", "db01"}, h.snapshots.content.Hosts)
	assert.NotContains(t, h.snapshots.content.Hosts, "ghost01")
}

// Asking the remote for its commit is one round trip with no transfer, so a
// host list that has not moved costs no fetch and no reparse.
func TestFileInventoryIsNotRebuiltWhileCommitIsUnchanged(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{
		"inventories/hosts": "[web]\nweb01\n",
	})

	h := newSourceHarness(t, fileInventory("inventories/hosts", repositoryID(1)), repositoryAt(dir))

	_, err := h.service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	_, err = h.service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, h.snapshots.writes)
}

func TestFileInventoryIsRebuiltOnNewCommit(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{
		"inventories/hosts": "[web]\nweb01\n",
	})

	h := newSourceHarness(t, fileInventory("inventories/hosts", repositoryID(1)), repositoryAt(dir))

	first, err := h.service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.HostCount)

	commitFiles(t, dir, map[string]string{
		"inventories/hosts": "[web]\nweb01\nweb02\nweb03\n",
	}, "more hosts")

	// A plain read inside the remote-head cache window serves the stored
	// snapshot: the cache exists precisely so that every read is not a git
	// round trip, and a list up to thirty seconds old is a fair trade for
	// that.
	stale, err := h.service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stale.HostCount)
	assert.Equal(t, 1, h.snapshots.writes)

	// A refresh bypasses the cache: the person asking for a rebuild must get
	// one against the true remote state, not against the cache.
	refreshed, err := h.service.Refresh(3, 7, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, refreshed.HostCount)
	assert.Equal(t, 2, h.snapshots.writes)
}

// A dynamic inventory is reported as unsupported rather than parsed, so that
// the picker offers free-form entry instead of a list of nonsense hosts.
func TestFileInventoryRefusesExecutable(t *testing.T) {
	dir := t.TempDir()
	gitCommand(t, dir, "init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.py"), []byte("#!/usr/bin/env python\nprint('{}')\n"), 0o700))
	gitCommand(t, dir, "add", "-A")
	gitCommand(t, dir, "commit", "-m", "dynamic inventory")

	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), repositoryAt(dir))

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusUnsupported, snapshot.Status)
	assert.Contains(t, snapshot.Error, "executable")
	assert.Empty(t, h.snapshots.content.Hosts)
}

func TestFileInventoryRefusesPathOutsideRepository(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})

	h := newSourceHarness(t, fileInventory("../../../etc/hosts", repositoryID(1)), repositoryAt(dir))

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Empty(t, h.snapshots.content.Hosts)
}

func TestFileInventoryResolvesRepositoryFromTemplate(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})

	h := newSourceHarness(t, fileInventory("hosts", nil), repositoryAt(dir))
	h.templates.template = db.Template{ID: 42, ProjectID: 3, RepositoryID: 1}

	templateID := 42
	snapshot, err := h.service.GetSnapshot(3, 7, &templateID)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status, snapshot.Error)
	assert.Equal(t, []string{"web01"}, h.snapshots.content.Hosts)
}

// Without a template of its own the inventory asks the templates that use it,
// and answers only when they agree — a path relative to two repositories names
// two different inventories.
func TestFileInventoryResolvesRepositoryFromReferrers(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})

	h := newSourceHarness(t, fileInventory("hosts", nil), repositoryAt(dir))
	h.inventory.refs = db.ObjectReferrers{
		Templates: []db.ObjectReferrer{{ID: 42}, {ID: 43}},
	}
	h.templates.template = db.Template{ProjectID: 3, RepositoryID: 1}

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status, snapshot.Error)
	assert.Equal(t, []string{"web01"}, h.snapshots.content.Hosts)
}

func TestFileInventoryReportsAmbiguousRepository(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})

	h := newSourceHarness(t, fileInventory("hosts", nil), repositoryAt(dir))
	h.inventory.refs = db.ObjectReferrers{
		Templates: []db.ObjectReferrer{{ID: 42}, {ID: 43}},
	}
	h.templates.byID = map[int]db.Template{
		42: {ProjectID: 3, RepositoryID: 1},
		43: {ProjectID: 3, RepositoryID: 2},
	}

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusUnsupported, snapshot.Status)
	assert.Contains(t, snapshot.Error, "disagree")
}

func TestFileInventoryReportsNoReferrers(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})

	h := newSourceHarness(t, fileInventory("hosts", nil), repositoryAt(dir))

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusUnsupported, snapshot.Status)
	assert.Contains(t, snapshot.Error, "no template")
}

// The template id arrives from the client, so it is resolved within the
// inventory's project. One from another project must find nothing rather than
// that project's repository.
func TestFileInventoryRefusesTemplateFromAnotherProject(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})

	h := newSourceHarness(t, fileInventory("hosts", nil), repositoryAt(dir))
	h.templates.err = db.ErrNotFound

	templateID := 999
	snapshot, err := h.service.GetSnapshot(3, 7, &templateID)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Empty(t, h.snapshots.content.Hosts)
}

// A repository that cannot be reached right now must not blank a host list that
// was good a moment ago.
func TestFileInventoryKeepsStoredSnapshotWhenRepositoryIsUnreachable(t *testing.T) {
	dir := newInventoryRepository(t, map[string]string{"hosts": "[web]\nweb01\n"})
	repository := repositoryAt(dir)

	h := newSourceHarness(t, fileInventory("hosts", repositoryID(1)), repository)

	good, err := h.service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)
	require.Equal(t, db.InventorySnapshotStatusOK, good.Status)

	// The remote goes away, which is what a network fault looks like from here.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git")))

	after, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, after.Status, "a stored host list outlives a failed fetch")
	assert.Equal(t, 1, h.snapshots.writes, "nothing is rewritten while the remote is unreachable")
}

func TestInventorySourceHashSeparatesRepositories(t *testing.T) {
	inventory := fileInventory("hosts", repositoryID(1))

	first := inventorySourceHash(inventory, inventorySource{fingerprint: "repo:1\ncommit:aaa"})
	same := inventorySourceHash(inventory, inventorySource{fingerprint: "repo:1\ncommit:aaa"})
	other := inventorySourceHash(inventory, inventorySource{fingerprint: "repo:2\ncommit:aaa"})

	assert.Equal(t, first, same, "the same repository and commit share a snapshot")
	assert.NotEqual(t, first, other, "a different repository is a different inventory")
}

func TestStoreResolvedWritesSnapshot(t *testing.T) {
	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), db.Repository{})
	h.claim(3, 7, 99)

	err := h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts: []string{"i-0a1", "i-0b2"},
		Groups: []db.InventorySnapshotGroupContent{
			{Name: "tag_role_web", Hosts: []string{"i-0a1"}},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, h.snapshots.stored)
	assert.Equal(t, db.InventorySnapshotStatusOK, h.snapshots.stored.Status)
	assert.Equal(t, 2, h.snapshots.stored.HostCount)
	require.NotNil(t, h.snapshots.stored.TaskID)
	assert.Equal(t, 99, *h.snapshots.stored.TaskID, "the snapshot names the resolve that produced it")
	assert.Equal(t, []string{"i-0a1", "i-0b2"}, h.snapshots.content.Hosts)
}

// A2: a report from a task whose claim expired and was taken over by a
// fresher refresh must not overwrite the newer state, and losing the race is
// not the task's error -- the play it came from genuinely ran.
func TestStoreResolvedIgnoresAnExpiredClaim(t *testing.T) {
	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), db.Repository{})
	h.claim(3, 7, 100) // a fresher resolve, task 100, holds the claim now

	err := h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts: []string{"stale01"},
	})

	require.NoError(t, err, "losing a claim race is not the reporting task's error")
	require.NotNil(t, h.snapshots.stored)
	assert.Equal(t, db.InventorySnapshotStatusPending, h.snapshots.stored.Status, "the fresher claim must be untouched")
	require.NotNil(t, h.snapshots.stored.TaskID)
	assert.Equal(t, 100, *h.snapshots.stored.TaskID)
	assert.Equal(t, 0, h.snapshots.writes, "a losing report must not write anything")
}

func TestStoreResolvedNormalisesEmptyLists(t *testing.T) {
	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), db.Repository{})
	h.claim(3, 7, 99)

	require.NoError(t, h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{}))

	assert.NotNil(t, h.snapshots.content.Hosts, "an empty result must be stored as an empty list")
	assert.NotNil(t, h.snapshots.content.Groups)
	assert.Equal(t, 0, h.snapshots.stored.HostCount)
}

// A runner is not trusted to bound its own report: the payload arrives over the
// network and is written straight to the tables.
func TestStoreResolvedRefusesOversizedResult(t *testing.T) {
	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), db.Repository{})

	hosts := make([]string, db.MaxInventorySnapshotHosts+1)
	for i := range hosts {
		hosts[i] = "host"
	}

	err := h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{Hosts: hosts})

	require.Error(t, err)
	assert.Equal(t, 0, h.snapshots.writes, "nothing is written when the report is refused")
}

func TestStoreResolvedRefusesNameWiderThanColumn(t *testing.T) {
	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), db.Repository{})

	err := h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts: []string{strings.Repeat("h", db.MaxInventoryHostNameLength+1)},
	})

	require.Error(t, err)
	assert.Equal(t, 0, h.snapshots.writes)
}

// Each resolve is its own state, so a later one replaces the row rather than
// being mistaken for the same content.
func TestStoreResolvedSeparatesResolves(t *testing.T) {
	assert.NotEqual(t, resolvedSourceHash(1), resolvedSourceHash(2))
	assert.Equal(t, resolvedSourceHash(1), resolvedSourceHash(1))
}

// The server cannot resolve a dynamic inventory itself, so reading one must
// serve what the last runner reported. Rebuilding would find an executable,
// call it unsupported and throw the host list away.
func TestReadingDynamicInventoryKeepsResolvedSnapshot(t *testing.T) {
	dir := t.TempDir()
	gitCommand(t, dir, "init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.py"), []byte("#!/usr/bin/env python\nprint('{}')\n"), 0o700))
	gitCommand(t, dir, "add", "-A")
	gitCommand(t, dir, "commit", "-m", "dynamic inventory")

	h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), repositoryAt(dir))
	h.claim(3, 7, 99)

	require.NoError(t, h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts:  []string{"i-0a1"},
		Groups: []db.InventorySnapshotGroupContent{{Name: "tag_role_web", Hosts: []string{"i-0a1"}}},
	}))

	writesAfterResolve := h.snapshots.writes

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status, "a read must not turn a resolved list into unsupported")
	assert.Equal(t, 1, snapshot.HostCount)
	assert.Equal(t, writesAfterResolve, h.snapshots.writes, "a read must not rewrite what only a runner can produce")
	assert.Equal(t, []string{"i-0a1"}, h.snapshots.content.Hosts)
}

func dynamicInventoryHarness(t *testing.T) *sourceHarness {
	t.Helper()

	dir := t.TempDir()
	gitCommand(t, dir, "init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.py"), []byte("#!/usr/bin/env python\nprint('{}')\n"), 0o700))
	gitCommand(t, dir, "add", "-A")
	gitCommand(t, dir, "commit", "-m", "dynamic inventory")

	return newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), repositoryAt(dir))
}

// Refresh on a dynamic inventory is a request the server cannot serve. Saying
// so lets the caller decide whether it may ask a runner, and leaves the stored
// host list alone in the meantime.
func TestRefreshReportsThatARunnerIsNeeded(t *testing.T) {
	h := dynamicInventoryHarness(t)

	_, err := h.service.Refresh(3, 7, nil)

	var needsRunner InventoryNeedsRunnerError
	require.ErrorAs(t, err, &needsRunner)
	assert.Equal(t, 7, needsRunner.InventoryID)
	assert.Equal(t, 0, h.snapshots.writes, "nothing is written when a runner has to be asked")
}

func TestRefreshKeepsResolvedListWhenARunnerIsNeeded(t *testing.T) {
	h := dynamicInventoryHarness(t)
	h.claim(3, 7, 99)

	require.NoError(t, h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts: []string{"i-0a1"},
	}))

	snapshot, err := h.service.Refresh(3, 7, nil)

	var needsRunner InventoryNeedsRunnerError
	require.ErrorAs(t, err, &needsRunner)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 1, snapshot.HostCount, "the previous resolve survives the request")
}

// While a runner is working on it, the pending state is the freshest answer
// there is, and a read has nothing to add.
func TestReadingWhilePendingReturnsPending(t *testing.T) {
	h := dynamicInventoryHarness(t)

	taskID := 99
	h.snapshots.stored = &db.InventorySnapshot{
		InventoryID: 7,
		ProjectID:   3,
		Status:      db.InventorySnapshotStatusPending,
		Created:     tz.Now(),
		TaskID:      &taskID,
	}

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusPending, snapshot.Status)
	assert.Equal(t, 0, h.snapshots.writes, "a read must not disturb a resolve in flight")
}

// A server that dies mid-resolve leaves the row pending and the task gone. A
// picker stuck on "resolving" forever is worse than one that says it failed.
func TestPendingExpires(t *testing.T) {
	h := dynamicInventoryHarness(t)

	taskID := 99
	h.snapshots.stored = &db.InventorySnapshot{
		InventoryID: 7,
		ProjectID:   3,
		Status:      db.InventorySnapshotStatusPending,
		Created:     tz.Now().Add(-db.MaxInventoryResolveAge - time.Minute),
		TaskID:      &taskID,
	}

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.NotEqual(t, db.InventorySnapshotStatusPending, snapshot.Status, "an expired claim stops being believed")
}

// The report is a runner's word, not the parser's: hosts and groups are each
// capped, but their product and the names inside group membership are
// independent JSON fields and need caps of their own.
func TestStoreResolvedRefusesHostileGroupContent(t *testing.T) {
	tests := []struct {
		name    string
		content db.InventorySnapshotContent
	}{
		{
			name: "too many groups",
			content: db.InventorySnapshotContent{
				Groups: make([]db.InventorySnapshotGroupContent, db.MaxInventorySnapshotGroups+1),
			},
		},
		{
			name: "too many group memberships in total",
			content: db.InventorySnapshotContent{
				Groups: []db.InventorySnapshotGroupContent{
					{Name: "g1", Hosts: make([]string, db.MaxInventorySnapshotMemberships/2+1)},
					{Name: "g2", Hosts: make([]string, db.MaxInventorySnapshotMemberships/2+1)},
				},
			},
		},
		{
			name: "membership name wider than the column",
			content: db.InventorySnapshotContent{
				Groups: []db.InventorySnapshotGroupContent{
					{Name: "g", Hosts: []string{strings.Repeat("h", db.MaxInventoryHostNameLength+1)}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSourceHarness(t, fileInventory("dynamic.py", repositoryID(1)), db.Repository{})

			err := h.service.StoreResolved(3, 7, 99, tt.content)

			require.Error(t, err)
			assert.Equal(t, 0, h.snapshots.writes, "nothing is written when the report is refused")
		})
	}
}

// A resolve that failed must not cost the inventory the host list the previous
// one produced. The claim moves the row off "ok" and clears its fingerprint, so
// a read that recognised a resolved snapshot by its status alone would rebuild,
// find an executable, and store "unsupported" over the only answer there is.
func TestReadingDynamicInventoryKeepsHostsOfAFailedResolve(t *testing.T) {
	h := dynamicInventoryHarness(t)
	h.claim(3, 7, 99)

	require.NoError(t, h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts:  []string{"i-0a1"},
		Groups: []db.InventorySnapshotGroupContent{{Name: "tag_role_web", Hosts: []string{"i-0a1"}}},
	}))

	// The whole chain a person walks through: refresh takes the claim, the
	// task it dispatched dies, and the next plain read finds the wreckage.
	claimed, err := h.snapshots.ClaimInventorySnapshotResolve(3, 7)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, h.snapshots.AssignInventorySnapshotResolveTask(3, 7, 100))
	require.NoError(t, h.snapshots.FailInventorySnapshotResolve(3, 7, 100, "inventory script exited 1"))

	writesBeforeRead := h.snapshots.writes

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Equal(t, "inventory script exited 1", snapshot.Error, "the reason the resolve failed is what a reader needs")
	assert.Equal(t, 1, snapshot.HostCount)
	assert.Equal(t, writesBeforeRead, h.snapshots.writes, "a failed resolve must not be answered by wiping the host list")
	assert.Equal(t, []string{"i-0a1"}, h.snapshots.content.Hosts)
}

// A claim whose task is gone stops being believed, but settling it must not
// take the host rows with it: they are still the last answer a resolve
// produced.
func TestReadingDynamicInventorySettlesAnExpiredClaimWithoutLosingHosts(t *testing.T) {
	h := dynamicInventoryHarness(t)
	h.claim(3, 7, 99)

	require.NoError(t, h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts: []string{"i-0a1"},
	}))

	claimed, err := h.snapshots.ClaimInventorySnapshotResolve(3, 7)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, h.snapshots.AssignInventorySnapshotResolveTask(3, 7, 100))

	// The node that dispatched the task died with it; nothing will ever settle
	// this claim.
	h.snapshots.stored.Created = tz.Now().Add(-db.MaxInventoryResolveAge - time.Minute)

	writesBeforeRead := h.snapshots.writes

	snapshot, err := h.service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status, "an expired claim must stop reporting resolving")
	assert.NotEmpty(t, snapshot.Error)
	assert.Equal(t, 1, snapshot.HostCount)
	assert.Equal(t, writesBeforeRead, h.snapshots.writes, "settling a claim must not rewrite the snapshot content")
	assert.Equal(t, []string{"i-0a1"}, h.snapshots.content.Hosts)
}

// Every snapshot table is keyed by name, so a report repeating one is a primary
// key violation the store cannot tell from two writers colliding: it would
// sleep and retry three times before failing a report that is perfectly usable.
func TestStoreResolvedDedupesReportedNames(t *testing.T) {
	h := dynamicInventoryHarness(t)
	h.claim(3, 7, 99)

	require.NoError(t, h.service.StoreResolved(3, 7, 99, db.InventorySnapshotContent{
		Hosts: []string{"i-0a1", "i-0b2", "i-0a1"},
		Groups: []db.InventorySnapshotGroupContent{
			{Name: "web", Hosts: []string{"i-0a1", "i-0a1"}},
			{Name: "web", Hosts: []string{"i-0b2"}},
		},
	}))

	assert.Equal(t, []string{"i-0a1", "i-0b2"}, h.snapshots.content.Hosts)

	require.Len(t, h.snapshots.content.Groups, 1)
	assert.Equal(t, "web", h.snapshots.content.Groups[0].Name)
	assert.Equal(t, []string{"i-0a1"}, h.snapshots.content.Groups[0].Hosts,
		"the first group of a repeated name wins, membership rows included")

	assert.Equal(t, 2, h.snapshots.stored.HostCount, "the count must describe the rows actually written")
}
