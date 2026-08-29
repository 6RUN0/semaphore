package db_lib

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
)

type AnsiblePlaybook struct {
	TemplateID int
	Repository db.Repository
	Logger     task_logger.Logger
}

func (p AnsiblePlaybook) makeCmd(command string, args []string, environmentVars []string) *exec.Cmd {
	cmd := exec.Command(command, args...) //nolint: gas
	cmd.Dir = p.GetFullPath()

	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")
	cmd.Env = append(cmd.Env, "ANSIBLE_FORCE_COLOR=True")
	cmd.Env = append(cmd.Env, "ANSIBLE_HOST_KEY_CHECKING=False")
	//cmd.Env = append(cmd.Env, "ANSIBLE_SSH_ARGS=-o UserKnownHostsFile=/dev/null")
	cmd.Env = append(cmd.Env, getEnvironmentVars()...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("HOME=%s", getHomeDir(p.Repository, p.TemplateID)))
	cmd.Env = append(cmd.Env, fmt.Sprintf("PWD=%s", cmd.Dir))

	if util.Config.HomeDirMode == util.HomeDirModeTemplateDir {
		cmd.Env = append(cmd.Env, fmt.Sprintf("ANSIBLE_HOME=%s", path.Join(p.Repository.GetHomePath(p.TemplateID), ".ansible")))
	}

	cmd.Env = append(cmd.Env, environmentVars...)

	cmd.SysProcAttr = util.Config.GetAppSysProcAttr()

	return cmd
}

func (p AnsiblePlaybook) runCmd(command string, args []string, environmentVars []string) error {
	cmd := p.makeCmd(command, args, environmentVars)
	boundOutputDrain(cmd)
	finishLog := p.Logger.LogCmd(cmd)
	defer finishLog()

	err := cmd.Run()
	finishLog() // Flush the command output before the drain notice, if any.

	return acceptDrainTimeout(p.Logger, err)
}

func (p AnsiblePlaybook) RunPlaybook(args []string, environmentVars []string, inputs map[string]string, cb func(*os.Process)) error {
	cmd := p.makeCmd("ansible-playbook", args, environmentVars)
	finishLog := p.Logger.LogCmd(cmd)
	defer finishLog()

	ptmx, err := pty.Start(cmd)

	if err != nil {
		return err
	}

	go func() {

		b := make([]byte, 100)

		var e error

		for {
			var n int
			n, e = ptmx.Read(b)
			if e != nil {
				break
			}

			s := strings.TrimSpace(string(b[0:n]))

			for k, v := range inputs {
				if strings.HasPrefix(s, k) {
					_, _ = ptmx.WriteString(v + "\n")
				}
			}
		}

	}()

	defer func() { _ = ptmx.Close() }()
	cb(cmd.Process)
	err = cmd.Wait()
	return err
}

// maxInventoryListingStderrBytes bounds the diagnostics a listing run may add
// to the task log. A run with -vvv writes as much as it likes, and the whole
// buffer would otherwise be held in memory and then pushed as a single log
// record.
const maxInventoryListingStderrBytes = 64 << 10

// boundedBuffer collects at most limit bytes and discards the rest, recording
// that it had to. Writes never fail: capping the diagnostics of a command must
// not make the command itself fail.
//
// The mutex is not decoration: the writer is a goroutine draining a pipe, and
// the reader gives up on it after a grace period rather than waiting for a
// process that may never close its end.
type boundedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	room := b.limit - b.buf.Len()

	switch {
	case room >= len(p):
		b.buf.Write(p)
	case room > 0:
		b.buf.Write(p[:room])
		b.overflow = true
	case len(p) > 0:
		b.overflow = true
	}

	return len(p), nil
}

// text returns what was collected, marked as truncated when it was.
func (b *boundedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	collected := strings.TrimRight(b.buf.String(), "\n")

	if b.overflow {
		collected += fmt.Sprintf("\n... (truncated at %d bytes)", b.limit)
	}

	return collected
}

func (b *boundedBuffer) empty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Len() == 0
}

// inventoryListingTimeout bounds a whole listing run. A dynamic inventory is a
// program from somebody's repository: it may wait on a network that never
// answers, or read from a stdin nobody feeds, and neither ansible-inventory nor
// this call has a deadline of its own. A blocked run holds the runner slot it
// occupies for as long as it lasts.
//
// The cap sits well below db.MaxInventoryResolveAge (30 minutes) so that a run
// that hits it still ends as a failed task, which settles the pending snapshot,
// rather than as a claim left to expire.
const inventoryListingTimeout = 10 * time.Minute

// stderrDrainGrace bounds how long a finished run waits for its diagnostics.
// A grandchild that inherited stderr holds the write end open for as long as it
// likes, and losing the tail of a diagnostic is better than never returning.
const stderrDrainGrace = 5 * time.Second

// RunInventoryListing runs ansible-inventory and returns its output.
//
// The output is captured and returned rather than logged, which is the whole
// difference from every other command here: a listing carries every variable of
// every host, ansible_password included, and the task log is readable by anyone
// who can see the task. Only diagnostics on stderr reach the log — which still
// quote the inventory lines Ansible failed to parse, so a line holding a
// password reaches the log through them.
//
// Both streams are bounded while they are read, not after. The listing comes
// from a program in somebody's repository, and a dynamic inventory that prints
// gigabytes would otherwise take the runner's memory down with it long before
// ParseInventoryListing ever got to check the size.
//
// cb receives the process so that the caller can stop the run; a listing is the
// only Ansible command here that a user cannot otherwise interrupt.
func (p AnsiblePlaybook) RunInventoryListing(args []string, environmentVars []string, cb func(*os.Process)) ([]byte, error) {
	cmd := p.makeCmd("ansible-inventory", args, environmentVars)

	// Killing the direct child is not enough: anything it spawned keeps the
	// inherited pipes open, and the reads below would wait on them forever.
	isolateProcessGroup(cmd)

	stderr := &boundedBuffer{limit: maxInventoryListingStderrBytes}

	// A pipe rather than cmd.Stderr, which os/exec drains in a goroutine that
	// Wait then waits for — that wait is the one a surviving grandchild holds.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err = cmd.Start(); err != nil {
		return nil, err
	}

	if cb != nil {
		cb(cmd.Process)
	}

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		_, _ = io.Copy(stderr, stderrPipe)
	}()

	var timedOut atomic.Bool

	deadline := time.AfterFunc(inventoryListingTimeout, func() {
		timedOut.Store(true)
		killProcessGroup(cmd)
	})

	defer deadline.Stop()

	// One byte past the budget: a listing sitting exactly on the limit is
	// still accepted, while a larger one is detected without being read.
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxInventoryListingBytes+1))

	tooLarge := len(out) > maxInventoryListingBytes
	if tooLarge {
		// Nothing drains the pipe from here on, so a program that keeps
		// writing would block on a full pipe buffer and never be reaped.
		killProcessGroup(cmd)
	}

	select {
	case <-drained:
	case <-time.After(stderrDrainGrace):
	}

	waitErr := cmd.Wait()

	// Logged directly rather than through LogCmd: that attaches both streams,
	// and stdout here is the listing itself, which carries every host variable.
	// Delivery is asynchronous and there is nothing left to wait on for it —
	// what the logger hands to its own writer is out of this call's hands.
	if !stderr.empty() {
		p.Logger.Log(stderr.text())
	}

	switch {
	case timedOut.Load():
		return nil, fmt.Errorf("inventory listing did not finish within %s", inventoryListingTimeout)
	case tooLarge:
		return nil, fmt.Errorf("inventory listing is larger than %d bytes", maxInventoryListingBytes)
	case readErr != nil:
		return nil, readErr
	case waitErr != nil:
		return nil, waitErr
	}

	return out, nil
}

func (p AnsiblePlaybook) RunGalaxy(args []string, environmentVars []string) error {
	return p.runCmd("ansible-galaxy", args, environmentVars)
}

func (p AnsiblePlaybook) GetFullPath() (path string) {
	path = p.Repository.GetFullPath(p.TemplateID)
	return
}
