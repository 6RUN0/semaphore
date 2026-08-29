package db_lib

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipeLogger attaches io.Pipe writers to the command, which is what the real
// task loggers do (services/tasks/TaskRunner_logging.go and
// services/runners/running_job.go both build their pipes with io.Pipe).
//
// The distinction matters and is the whole reason this logger exists next to
// recordingLogger: that one calls cmd.StdoutPipe(), which puts an *os.File in
// cmd.Stdout, and os/exec then hands the descriptor to the child and waits for
// nothing. An io.Pipe writer is not an *os.File, so os/exec copies the output
// itself and Wait blocks until that copy reaches EOF — which happens only once
// every inherited copy of the write end is closed, grandchildren included.
// A logger built on StdoutPipe cannot reproduce that wait at all.
type pipeLogger struct {
	task_logger.NopLogger

	mu    sync.Mutex
	wg    sync.WaitGroup
	lines []string
}

func (l *pipeLogger) Log(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
}

func (l *pipeLogger) Logf(format string, a ...any) {
	l.Log(format)
}

func (l *pipeLogger) LogCmd(cmd *exec.Cmd) func() {
	stdout, stdoutWriter := io.Pipe()
	stderr, stderrWriter := io.Pipe()

	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	for _, reader := range []io.Reader{stdout, stderr} {
		l.wg.Add(1)

		go func(reader io.Reader) {
			defer l.wg.Done()

			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				l.Log(scanner.Text())
			}
		}(reader)
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
			l.wg.Wait()
		})
	}
}

func (l *pipeLogger) log() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.lines, "\n")
}

// newDrainTestPlaybook builds an AnsiblePlaybook that runs commands in a
// temporary directory, with the package globals a run needs.
func newDrainTestPlaybook(t *testing.T) (AnsiblePlaybook, *pipeLogger) {
	t.Helper()

	dir := t.TempDir()

	util.Config = &util.ConfigType{
		TmpPath:     t.TempDir(),
		HomeDirMode: util.HomeDirModeProjectHome,
		Log: &util.ConfigLog{
			Events: &util.EventLogType{},
			Tasks:  &util.TaskLogType{},
		},
		Process: &util.ConfigProcess{},
	}

	require.NoError(t, os.MkdirAll(util.Config.GetProjectTmpDir(1), 0o700))

	logger := &pipeLogger{}

	return AnsiblePlaybook{
		TemplateID: 1,
		Repository: db.Repository{ID: 1, ProjectID: 1, GitURL: dir},
		Logger:     logger,
	}, logger
}

// grandchildHolder is a command whose direct child exits at once while the
// grandchild it spawned keeps the inherited output descriptor open. This is the
// shape of every command in this package: git spawns ssh and credential
// helpers, ansible spawns connection multiplexers, terraform spawns providers.
const grandchildHolder = "printf 'output line\\n'; sleep 5 &"

// requireBash skips when the shell the fixtures need is unavailable.
func requireBash(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
}

// A command must not be waited on for longer than it runs. os/exec waits for
// its own output-copying goroutine, and a surviving grandchild holds the write
// end of that pipe open for as long as it likes — without a bound the task
// holds its runner slot until the grandchild decides to exit.
func TestRunCmdReturnsWhileGrandchildHoldsThePipe(t *testing.T) {
	requireBash(t)

	playbook, logger := newDrainTestPlaybook(t)

	startedAt := time.Now()
	err := playbook.runCmd("bash", []string{"-c", grandchildHolder}, nil)
	elapsed := time.Since(startedAt)

	// The grandchild lives 5s. Anything close to that means the run waited for
	// it; the bound is generous so that a loaded machine does not fail here.
	assert.Less(t, elapsed, 2*time.Second,
		"the run must not wait for a grandchild holding the inherited pipe")

	// The command itself succeeded, so the run must report success. Reporting
	// the drain timeout as a task failure would be a regression of its own.
	assert.NoError(t, err, "a successful command must not fail because of a slow drain")

	// Bounding the drain must not cost the output the command did write.
	assert.Contains(t, logger.log(), "output line")
}

// The same bound must hold for the playbook path, which additionally attaches a
// pty. Terraform and git share the helper this pins, and share no test binary.
func TestRunPlaybookReturnsWhileGrandchildHoldsThePipe(t *testing.T) {
	requireBash(t)

	playbook, logger := newDrainTestPlaybook(t)

	// RunPlaybook always runs "ansible-playbook"; point that name at a script
	// that behaves like the fixture instead of requiring ansible to be present.
	binDir := t.TempDir()
	script := filepath.Join(binDir, "ansible-playbook")
	require.NoError(t, os.WriteFile(script, []byte("#!/usr/bin/env bash\n"+grandchildHolder+"\n"), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// makeCmd builds cmd.Env from nothing, so the script would run without a
	// PATH and its `sleep` would never be found — the fixture would then pass
	// by spawning no grandchild at all.
	inheritedPath := []string{"PATH=" + os.Getenv("PATH")}

	startedAt := time.Now()
	err := playbook.RunPlaybook(nil, inheritedPath, nil, func(*os.Process) {})
	elapsed := time.Since(startedAt)

	assert.Less(t, elapsed, 2*time.Second,
		"the playbook run must not wait for a grandchild holding the inherited pipe")
	assert.NoError(t, err, "a successful playbook must not fail because of a slow drain")

	// Proves the fixture actually ran: without this the test passes even when
	// the script fails to start.
	assert.Contains(t, logger.log(), "output line")
}
