package db_lib

import (
	"errors"
	"os/exec"
	"time"

	"github.com/semaphoreui/semaphore/pkg/task_logger"
)

// outputDrainDelay bounds how long a command that has already exited is waited
// on for its output to drain.
//
// The task loggers attach io.Pipe writers to cmd.Stdout and cmd.Stderr. Those
// are not *os.File, so os/exec copies the output itself and Wait blocks until
// that copy reaches EOF — which needs every inherited copy of the write end to
// be closed, including the ones a grandchild kept. git spawns ssh and
// credential helpers, terraform spawns providers, and any of them can outlive
// the command and hold the task in Wait indefinitely.
//
// The value matches ShellApp.Run, which bounds the same wait. It only ever
// costs output that arrives after the command already exited.
const outputDrainDelay = 250 * time.Millisecond

// boundOutputDrain caps the wait described on outputDrainDelay. A delay already
// set by the caller wins, so a command with its own deadline keeps it.
func boundOutputDrain(cmd *exec.Cmd) {
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = outputDrainDelay
	}
}

// acceptDrainTimeout maps a drain timeout back to success.
//
// os/exec returns ErrWaitDelay *in place of* a nil error, so it appears exactly
// when the command itself succeeded and only its output was slow. Returning it
// unchanged would fail tasks whose command worked, which is why setting
// WaitDelay without this is worse than not setting it at all.
func acceptDrainTimeout(logger task_logger.Logger, err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		logger.Logf("command output draining exceeded %s and was stopped", outputDrainDelay)
		return nil
	}

	return err
}
