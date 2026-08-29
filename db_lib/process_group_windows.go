//go:build windows

package db_lib

import "os/exec"

// isolateProcessGroup is a no-op on Windows: a job object would be needed to
// make the child's descendants killable as a unit, and Semaphore does not
// create one for its child processes.
func isolateProcessGroup(*exec.Cmd) {
}

// killProcessGroup kills the process itself. Its descendants survive, so a
// listing that leaves a background helper behind can still hold the pipes it
// inherited; the read timeouts of the caller are what bound that case here.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}

	_ = cmd.Process.Kill()
}
