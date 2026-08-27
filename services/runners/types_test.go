package runners

import (
	"encoding/json"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A runner and the server it reports to are upgraded separately, so a progress
// report has to survive being read by the other version of itself. These pin
// both directions for ResolvedInventory the way Commit and TerminatedJobs
// already behave.

// legacyJobProgress is JobProgress as it was before ResolvedInventory existed,
// which is what an older server decodes into.
type legacyJobProgress struct {
	ID         int
	Status     task_logger.TaskStatus
	LogRecords []LogRecord
	Commit     *CommitInfo
}

func TestNewRunnerReportsToOldServer(t *testing.T) {
	sent, err := json.Marshal(JobProgress{
		ID:     7,
		Status: task_logger.TaskRunningStatus,
		Commit: &CommitInfo{Hash: "abc123", Message: "a commit"},
		ResolvedInventory: &db.InventorySnapshotContent{
			Hosts: []string{"web01"},
		},
	})
	require.NoError(t, err)

	var received legacyJobProgress
	require.NoError(t, json.Unmarshal(sent, &received), "an old server must not choke on the new field")

	assert.Equal(t, 7, received.ID)
	assert.Equal(t, task_logger.TaskRunningStatus, received.Status)
	require.NotNil(t, received.Commit)
	assert.Equal(t, "abc123", received.Commit.Hash)
}

func TestOldRunnerReportsToNewServer(t *testing.T) {
	sent, err := json.Marshal(legacyJobProgress{
		ID:     7,
		Status: task_logger.TaskRunningStatus,
		Commit: &CommitInfo{Hash: "abc123", Message: "a commit"},
	})
	require.NoError(t, err)

	var received JobProgress
	require.NoError(t, json.Unmarshal(sent, &received))

	assert.Equal(t, 7, received.ID)
	require.NotNil(t, received.Commit)
	assert.Nil(t, received.ResolvedInventory, "absence must read as nothing to store, not as an empty host list")
}

// The difference matters at the far end: nil means the job reported nothing,
// while an empty list means it resolved and found no hosts.
func TestResolvedInventoryRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content *db.InventorySnapshotContent
	}{
		{"nothing reported", nil},
		{"resolved to nothing", &db.InventorySnapshotContent{
			Hosts:  []string{},
			Groups: []db.InventorySnapshotGroupContent{},
		}},
		{"hosts and groups", &db.InventorySnapshotContent{
			Hosts: []string{"db01", "web01"},
			Groups: []db.InventorySnapshotGroupContent{
				{Name: "db", Hosts: []string{"db01"}},
				{Name: "web", Hosts: []string{"web01"}},
			},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sent, err := json.Marshal(JobProgress{ID: 7, ResolvedInventory: tt.content})
			require.NoError(t, err)

			var received JobProgress
			require.NoError(t, json.Unmarshal(sent, &received))

			assert.Equal(t, tt.content, received.ResolvedInventory)
		})
	}
}

// Host variables never travel: the reduction happens on the runner, and the
// wire type has nowhere to put them even if it did not.
func TestResolvedInventoryCarriesNamesOnly(t *testing.T) {
	sent, err := json.Marshal(JobProgress{
		ID: 7,
		ResolvedInventory: &db.InventorySnapshotContent{
			Hosts:  []string{"web01"},
			Groups: []db.InventorySnapshotGroupContent{{Name: "web", Hosts: []string{"web01"}}},
		},
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(sent, &decoded))

	resolved, ok := decoded["ResolvedInventory"].(map[string]any)
	require.True(t, ok)

	assert.ElementsMatch(t, []string{"Hosts", "Groups"}, keysOf(resolved))

	groups := resolved["Groups"].([]any)
	require.Len(t, groups, 1)
	assert.ElementsMatch(t, []string{"Name", "Hosts"}, keysOf(groups[0].(map[string]any)))
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	return keys
}
