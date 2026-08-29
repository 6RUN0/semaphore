//go:build !windows

package db_lib

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts a command in a process group of its own so that
// killProcessGroup can reach everything it spawns.
//
// It exists for commands that run a program from a user's repository: killing
// only the direct child leaves its own children holding the inherited pipes,
// and whoever reads those pipes then waits forever.
func isolateProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup terminates the command together with the processes it
// spawned. A negative pid addresses the whole group; when the group was never
// created the call fails and only the process itself is killed.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
