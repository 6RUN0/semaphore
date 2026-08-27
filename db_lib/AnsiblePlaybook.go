package db_lib

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"

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
	p.Logger.LogCmd(cmd)
	err := cmd.Run()
	// Wait for all log processing to complete before returning
	p.Logger.WaitLog()
	return err
}

func (p AnsiblePlaybook) RunPlaybook(args []string, environmentVars []string, inputs map[string]string, cb func(*os.Process)) error {
	cmd := p.makeCmd("ansible-playbook", args, environmentVars)
	p.Logger.LogCmd(cmd)

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
	// Wait for all log processing to complete before returning
	p.Logger.WaitLog()
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
type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
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
	collected := strings.TrimRight(b.buf.String(), "\n")

	if b.overflow {
		collected += fmt.Sprintf("\n... (truncated at %d bytes)", b.limit)
	}

	return collected
}

// RunInventoryListing runs ansible-inventory and returns its output.
//
// The output is captured and returned rather than logged, which is the whole
// difference from every other command here: a listing carries every variable of
// every host, ansible_password included, and the task log is readable by anyone
// who can see the task. Only diagnostics on stderr reach the log.
//
// Both streams are bounded while they are read, not after. The listing comes
// from a program in somebody's repository, and a dynamic inventory that prints
// gigabytes would otherwise take the runner's memory down with it long before
// ParseInventoryListing ever got to check the size.
func (p AnsiblePlaybook) RunInventoryListing(args []string, environmentVars []string) ([]byte, error) {
	cmd := p.makeCmd("ansible-inventory", args, environmentVars)

	stderr := &boundedBuffer{limit: maxInventoryListingStderrBytes}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err = cmd.Start(); err != nil {
		return nil, err
	}

	// One byte past the budget: a listing sitting exactly on the limit is
	// still accepted, while a larger one is detected without being read.
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxInventoryListingBytes+1))

	tooLarge := len(out) > maxInventoryListingBytes
	if tooLarge {
		// Nothing drains the pipe from here on, so a program that keeps
		// writing would block on a full pipe buffer and never be reaped.
		_ = cmd.Process.Kill()
	}

	waitErr := cmd.Wait()

	if stderr.buf.Len() > 0 {
		p.Logger.Log(stderr.text())
	}

	switch {
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
