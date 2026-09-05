//go:build !windows

package db_lib

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunBoundedReturnsOutputOfCommandThatFinishes(t *testing.T) {
	out, err := runBounded(exec.Command("sh", "-c", "printf hello"), time.Minute)

	require.NoError(t, err)
	assert.Equal(t, "hello", string(out))
}

func TestRunBoundedReportsCommandFailure(t *testing.T) {
	_, err := runBounded(exec.Command("sh", "-c", "exit 3"), time.Minute)

	assert.Error(t, err, "a non-zero exit must reach the caller as it does without the bound")
}

// git has no timeout of its own: a remote that accepts the connection and then
// says nothing holds the request goroutine for as long as it likes.
func TestRunBoundedKillsCommandThatOverrunsItsTimeout(t *testing.T) {
	started := time.Now()

	_, err := runBounded(exec.Command("sh", "-c", "sleep 30"), 200*time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Less(t, time.Since(started), 5*time.Second, "the timeout must not wait for the command")
}

// The reason the whole process group is killed rather than the command itself.
//
// `git ls-remote` over ssh leaves an ssh process holding the output pipe it
// inherited; here a background sleep stands in for it. The command exits at
// once, but Wait does not return while anything still holds that pipe open —
// killing the direct child would achieve nothing, because it is already dead.
func TestRunBoundedDoesNotWaitForGrandchildHoldingThePipe(t *testing.T) {
	started := time.Now()

	_, err := runBounded(exec.Command("sh", "-c", "sleep 30 & exit 0"), 200*time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Less(t, time.Since(started), 5*time.Second,
		"a surviving grandchild must not keep the call waiting")
}

// git rejects `--jobs 0` outright: it aborts with "you must provide a non-zero
// number of processes" (git 2.54, exit code 134), and a negative count ends the
// command as well. util.Config is a plain struct that any code path can fill in
// without ConfigInit, so the zero value reaches the client and must not be
// passed on.
func TestGitCloneArgsOmitJobsWhenCountIsBelowOne(t *testing.T) {
	tests := []struct {
		name         string
		jobs         int
		expectedJobs []string
	}{
		{"zero jobs", 0, nil},
		{"negative jobs", -1, nil},
		{"configured jobs", 4, []string{"--jobs", "4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := gitCloneArgs("main", "https://example.com/repo.git", "repo", tt.jobs)

			expected := []string{"clone", "--recursive"}
			expected = append(expected, tt.expectedJobs...)
			expected = append(expected,
				"--branch", "main",
				"--end-of-options", "https://example.com/repo.git", "repo")

			assert.Equal(t, expected, args)
		})
	}
}

func TestGitSubmoduleUpdateArgsOmitJobsWhenCountIsBelowOne(t *testing.T) {
	tests := []struct {
		name         string
		jobs         int
		expectedJobs []string
	}{
		{"zero jobs", 0, nil},
		{"negative jobs", -1, nil},
		{"configured jobs", 4, []string{"--jobs", "4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := gitSubmoduleUpdateArgs(tt.jobs)

			expected := []string{"submodule", "update", "--init", "--recursive"}
			expected = append(expected, tt.expectedJobs...)

			assert.Equal(t, expected, args)
		})
	}
}

// The ceiling on the job count belongs to config validation, not to the client:
// clamping here would silently disagree with the value the administrator set.
func TestGitCloneArgsDoNotClampLargeJobCounts(t *testing.T) {
	args := gitCloneArgs("main", "https://example.com/repo.git", "repo", 1000)

	assert.Contains(t, strings.Join(args, " "), "--jobs 1000")
}
