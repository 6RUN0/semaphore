package db_lib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The listing is produced by a program from somebody's repository, so both of
// its streams have to be bounded while they are read. The tests below stand in
// a fake ansible-inventory on PATH, which is the only way to make a run emit an
// amount of output no real inventory would.

// fakeAnsibleInventory installs a shell script named ansible-inventory ahead of
// everything else on PATH and returns the environment entries a run needs to
// find it.
//
// The environment is handed to the run rather than set on the process, because
// makeCmd builds the command's environment from scratch: without PATH in it the
// script would have no interpreter for anything it calls.
func fakeAnsibleInventory(t *testing.T, script string) []string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ansible-inventory"), []byte(script), 0o700))

	path := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)

	return []string{"PATH=" + path}
}

func newListingPlaybook(t *testing.T) (*AnsiblePlaybook, *recordingLogger) {
	t.Helper()

	app, logger := newInventoryApp(t, t.TempDir())

	return app.Playbook, logger
}

func TestRunInventoryListingReturnsOutput(t *testing.T) {
	environment := fakeAnsibleInventory(t, "#!/bin/sh\nprintf '{}'\n")
	playbook, _ := newListingPlaybook(t)

	out, err := playbook.RunInventoryListing([]string{"--list"}, environment, nil)

	require.NoError(t, err)
	assert.Equal(t, "{}", string(out))
}

// A dynamic inventory printing gigabytes would take the runner's memory down
// with it long before ParseInventoryListing ever got to check the size, so the
// run itself has to stop reading.
func TestRunInventoryListingRefusesOversizedOutput(t *testing.T) {
	environment := fakeAnsibleInventory(t, fmt.Sprintf(
		"#!/bin/sh\nhead -c %d /dev/zero\n", maxInventoryListingBytes+1))
	playbook, _ := newListingPlaybook(t)

	out, err := playbook.RunInventoryListing([]string{"--list"}, environment, nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, fmt.Sprintf("larger than %d bytes", maxInventoryListingBytes))
	assert.Nil(t, out, "an oversized listing must not be handed on to the parser")
}

// A run with -vvv writes as much to stderr as it likes, and the whole buffer
// would otherwise be held in memory and pushed as a single log record.
func TestRunInventoryListingTruncatesDiagnostics(t *testing.T) {
	environment := fakeAnsibleInventory(t, fmt.Sprintf(
		"#!/bin/sh\nhead -c %d /dev/zero | tr '\\0' 'v' >&2\nprintf '{}'\n",
		maxInventoryListingStderrBytes*4))
	playbook, logger := newListingPlaybook(t)

	out, err := playbook.RunInventoryListing([]string{"--list"}, environment, nil)

	require.NoError(t, err)
	assert.Equal(t, "{}", string(out))

	logged := logger.log()
	assert.Contains(t, logged, "truncated")
	assert.Less(t, len(logged), maxInventoryListingStderrBytes*2,
		"the log record must be bounded by the stderr budget, not by what the run wrote")
}

// A failing run reports its failure rather than an empty listing, which the
// parser would otherwise reject with a misleading "not valid JSON".
func TestRunInventoryListingReportsCommandFailure(t *testing.T) {
	environment := fakeAnsibleInventory(t, "#!/bin/sh\nprintf 'boom\\n' >&2\nexit 2\n")
	playbook, logger := newListingPlaybook(t)

	_, err := playbook.RunInventoryListing([]string{"--list"}, environment, nil)

	require.Error(t, err)
	assert.Contains(t, logger.log(), "boom", "diagnostics still reach the log")
}

func TestBoundedBuffer(t *testing.T) {
	for _, tt := range []struct {
		name     string
		limit    int
		writes   []string
		expected string
	}{
		{"under the limit", 16, []string{"abc", "def"}, "abcdef"},
		{"exactly at the limit", 6, []string{"abc", "def"}, "abcdef"},
		{"cut mid-write", 4, []string{"abc", "def"}, "abcd\n... (truncated at 4 bytes)"},
		{"nothing kept", 0, []string{"abc"}, "\n... (truncated at 0 bytes)"},
		{"trailing newlines dropped", 16, []string{"abc\n\n"}, "abc"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			buffer := &boundedBuffer{limit: tt.limit}

			for _, write := range tt.writes {
				written, err := buffer.Write([]byte(write))

				// A capped write must still look like a full one: the command
				// must not fail because its diagnostics were truncated.
				require.NoError(t, err)
				assert.Equal(t, len(write), written)
			}

			assert.Equal(t, tt.expected, buffer.text())
		})
	}
}

func TestBoundedBufferKeepsTheFirstBytes(t *testing.T) {
	buffer := &boundedBuffer{limit: 8}

	_, err := buffer.Write([]byte(strings.Repeat("x", 1024)))
	require.NoError(t, err)

	assert.True(t, buffer.overflow)
	assert.Equal(t, strings.Repeat("x", 8), buffer.buf.String())
}

// A listing used to be the one Ansible command a user could not stop: the
// callback that hands the executor the process was passed to RunPlaybook only,
// so LocalExecutor.Kill found a nil process and returned silently.
func TestRunInventoryListingIsStoppableThroughTheCallback(t *testing.T) {
	environment := fakeAnsibleInventory(t, "#!/bin/sh\nsleep 60\nprintf '{}'\n")
	playbook, _ := newListingPlaybook(t)

	var stopped *os.Process

	_, err := playbook.RunInventoryListing([]string{"--list"}, environment, func(p *os.Process) {
		stopped = p
		require.NoError(t, p.Kill())
	})

	require.Error(t, err)
	assert.NotNil(t, stopped, "the run must hand out its process before it blocks")
}

// os/exec drains an io.Writer stderr in a goroutine that Wait waits for, and
// that wait ends only when every holder of the write end closes it — a
// background helper started by the listing holds it for as long as it runs.
// The run has to give up on the diagnostics instead of on returning.
func TestRunInventoryListingDoesNotWaitForAnInheritedStderr(t *testing.T) {
	// stdout is redirected away from the helper on purpose: this test pins the
	// stderr wait, and a helper holding stdout too would block the read above
	// it instead.
	environment := fakeAnsibleInventory(t, "#!/bin/sh\nsleep 20 >/dev/null &\nprintf '{}'\n")
	playbook, _ := newListingPlaybook(t)

	done := make(chan struct{})

	go func() {
		defer close(done)

		out, err := playbook.RunInventoryListing([]string{"--list"}, environment, nil)

		assert.NoError(t, err)
		assert.Equal(t, "{}", string(out))
	}()

	select {
	case <-done:
	case <-time.After(stderrDrainGrace + 10*time.Second):
		t.Fatal("the listing did not return while a helper held its stderr open")
	}
}
