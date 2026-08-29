//go:build !windows

package db_lib

import (
	"os/exec"
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
