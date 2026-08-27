package server

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot read path reaches git on an unauthenticated GET, so what it
// spawns and how often is a security property, not a performance detail
// (review findings C1, M-2). These pin the three guards: git operations on one
// checkout directory never overlap, the remote head is cached between reads,
// and a runner-resolved snapshot answers reads with no git at all.

// countingGitClient is a thread-safe GitClient fake that records how many
// calls ran and how many overlapped.
type countingGitClient struct {
	mu          sync.Mutex
	remoteCalls int
	treeCalls   int
	inFlight    int
	maxInFlight int
}

func (c *countingGitClient) enterTree() func() {
	c.mu.Lock()
	c.treeCalls++
	c.inFlight++
	if c.inFlight > c.maxInFlight {
		c.maxInFlight = c.inFlight
	}
	c.mu.Unlock()

	// Long enough that unserialized goroutines would reliably overlap.
	time.Sleep(10 * time.Millisecond)

	return func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}
}

func (c *countingGitClient) Clone(db_lib.GitRepository) error {
	defer c.enterTree()()
	return nil
}

func (c *countingGitClient) Pull(db_lib.GitRepository) error {
	defer c.enterTree()()
	return nil
}

func (c *countingGitClient) Checkout(db_lib.GitRepository, string) error { return nil }
func (c *countingGitClient) CanBePulled(db_lib.GitRepository) bool       { return true }

func (c *countingGitClient) GetLastCommitMessage(db_lib.GitRepository) (string, error) {
	return "", nil
}

func (c *countingGitClient) GetLastCommitHash(db_lib.GitRepository) (string, error) {
	return "abc123", nil
}

func (c *countingGitClient) GetLastRemoteCommitHash(db_lib.GitRepository) (string, error) {
	c.mu.Lock()
	c.remoteCalls++
	c.mu.Unlock()

	return "abc123", nil
}

func (c *countingGitClient) GetRemoteBranches(db_lib.GitRepository) ([]string, error) {
	return []string{"main"}, nil
}

// lockedSnapshotManager makes the snapshot mock safe for the concurrency test.
type lockedSnapshotManager struct {
	mu sync.Mutex
	mockSnapshotManager
}

func (m *lockedSnapshotManager) GetInventorySnapshot(projectID int, inventoryID int) (db.InventorySnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mockSnapshotManager.GetInventorySnapshot(projectID, inventoryID)
}

func (m *lockedSnapshotManager) SetInventorySnapshot(snapshot db.InventorySnapshot, content db.InventorySnapshotContent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mockSnapshotManager.SetInventorySnapshot(snapshot, content)
}

func remoteRepository() db.Repository {
	return db.Repository{
		ID:        1,
		ProjectID: 3,
		GitURL:    "https://git.example.com/inventories.git",
		GitBranch: "main",
	}
}

func newCountingHarness(t *testing.T, snapshots db.InventorySnapshotManager) (InventorySnapshotService, *countingGitClient) {
	t.Helper()
	setupSourceConfig(t)

	client := &countingGitClient{}

	service := NewInventorySnapshotService(
		&mockInventoryManagerForSnapshot{inventory: fileInventory("hosts", repositoryID(1))},
		snapshots,
		&mockTemplateManager{},
		&mockRepositoryManager{repository: remoteRepository()},
		nil,
	)
	service.(*InventorySnapshotServiceImpl).locator.gitClient = client

	return service, client
}

func TestConcurrentReadsNeverOverlapInTheCheckout(t *testing.T) {
	service, client := newCountingHarness(t, &lockedSnapshotManager{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The parse of the (never actually created) checkout fails; what
			// matters here is only how the git operations interleaved.
			_, _ = service.GetSnapshot(3, 7, nil)
		}()
	}
	wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()
	assert.LessOrEqual(t, client.maxInFlight, 1,
		"git operations in one checkout directory must never overlap")
	assert.Positive(t, client.treeCalls, "the test must have actually reached git")
}

// The readers that queued on the checkout lock look at the snapshot again once
// they hold it. Without that second look each of them in turn checks the
// repository out and rewrites four tables with the rows the first one already
// wrote — and in HA that is multiplied by the number of nodes, on a path a
// search box drives with one request per keystroke (review finding MEDIUM-3).
func TestConcurrentReadsRebuildOnlyOnce(t *testing.T) {
	snapshots := &lockedSnapshotManager{}
	service, _ := newCountingHarness(t, snapshots)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = service.GetSnapshot(3, 7, nil)
		}()
	}
	wg.Wait()

	snapshots.mu.Lock()
	defer snapshots.mu.Unlock()
	assert.Equal(t, 1, snapshots.writes,
		"only the first reader through the lock may rebuild; the rest read what it wrote")
}

func TestRemoteHeadIsCachedAcrossReads(t *testing.T) {
	service, client := newCountingHarness(t, &mockSnapshotManager{})

	_, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	_, err = service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	client.mu.Lock()
	remoteCalls := client.remoteCalls
	client.mu.Unlock()

	assert.Equal(t, 1, remoteCalls,
		"the second read inside the TTL must be answered from the cache")

	// A refresh is a person asking for a rebuild; it must bypass the cache.
	_, _ = service.Refresh(3, 7, nil)

	client.mu.Lock()
	defer client.mu.Unlock()
	assert.Equal(t, 2, client.remoteCalls)
}

// A snapshot a runner resolved is only ever replaced by another resolve, so a
// plain read has no reason to touch git — before this rule, every keystroke in
// the picker's search box checked the repository out.
func TestResolvedSnapshotAnswersReadsWithoutGit(t *testing.T) {
	taskID := 42
	snapshots := &mockSnapshotManager{
		stored: &db.InventorySnapshot{
			InventoryID: 7,
			ProjectID:   3,
			Status:      db.InventorySnapshotStatusOK,
			TaskID:      &taskID,
			HostCount:   2,
		},
	}

	service, client := newCountingHarness(t, snapshots)

	snapshot, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)

	client.mu.Lock()
	defer client.mu.Unlock()
	assert.Zero(t, client.remoteCalls+client.treeCalls,
		"a resolved snapshot must be served without any git activity")
}

// filepath.Join collapses "..", so a hostile inventory filename could point
// the local-repository fingerprint at any path on the server (review finding
// L-3).
func TestResolveUnderRootRefusesEscapes(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		escapes  bool
	}{
		{"plain file", "hosts", false},
		{"nested file", "inventories/hosts", false},
		{"dot segments that stay inside", "inventories/../hosts", false},
		{"parent escape", "../secrets", true},
		{"deep escape", "a/../../secrets", true},
		{"absolute-looking escape", "../../etc/passwd", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joined, err := resolveUnderRoot("/srv/repo", tt.filename)

			if tt.escapes {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Contains(t, joined, "/srv/repo")
		})
	}
}

// A byte cut can split a multi-byte character and PostgreSQL rejects the
// resulting invalid UTF-8, turning a long error message into a failed write
// (review finding L-4).
func TestTruncateSnapshotErrorKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("ы", db.MaxInventorySnapshotErrorLength+10)

	truncated := truncateSnapshotError(long)

	assert.True(t, utf8.ValidString(truncated))
	assert.Equal(t, db.MaxInventorySnapshotErrorLength, utf8.RuneCountInString(truncated))

	short := "short"
	assert.Equal(t, short, truncateSnapshotError(short))
}
