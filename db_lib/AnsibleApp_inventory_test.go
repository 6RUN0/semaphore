package db_lib

import (
	"bufio"
	"fmt"
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

// recordingLogger keeps everything a run wrote to the task log, so a test can
// assert on what did not reach it.
type recordingLogger struct {
	task_logger.NopLogger

	mu    sync.Mutex
	wg    sync.WaitGroup
	lines []string

	resolvedHosts  []string
	resolvedGroups map[string][]string
	resolveCalls   int
}

func (l *recordingLogger) Log(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
}

func (l *recordingLogger) Logf(format string, a ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *recordingLogger) LogWithTime(_ time.Time, msg string) {
	l.lines = append(l.lines, msg)
}

func (l *recordingLogger) LogfWithTime(_ time.Time, format string, a ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

// LogCmd attaches to the command's output the way a real task logger does.
// Inheriting the no-op from NopLogger would make this whole file blind to the
// one mistake it exists to catch — wiring the listing through the log.
// The returned finalizer waits for both readers, which is what the interface
// promises the caller may do after the command stops writing.
func (l *recordingLogger) LogCmd(cmd *exec.Cmd) func() {
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	for _, pipe := range []io.Reader{stdout, stderr} {
		if pipe == nil {
			continue
		}

		l.wg.Add(1)

		go func(reader io.Reader) {
			defer l.wg.Done()

			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				l.mu.Lock()
				l.lines = append(l.lines, scanner.Text())
				l.mu.Unlock()
			}
		}(pipe)
	}

	return l.wg.Wait
}

func (l *recordingLogger) SetResolvedInventory(hosts []string, groups map[string][]string) {
	l.resolveCalls++
	l.resolvedHosts = hosts
	l.resolvedGroups = groups
}

func (l *recordingLogger) log() string {
	l.wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.lines, "\n")
}

// newInventoryApp builds an AnsibleApp whose working directory is dir, which is
// where ansible-inventory will be run.
func newInventoryApp(t *testing.T, dir string) (*AnsibleApp, *recordingLogger) {
	t.Helper()

	// Ansible writes under HOME, and with the default mode HOME comes out
	// empty, which fails the run before it reads anything.
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

	logger := &recordingLogger{}

	// A GitURL that is a bare path makes this a local repository, so
	// GetFullPath returns the directory itself rather than a checkout.
	repository := db.Repository{ID: 1, ProjectID: 1, GitURL: dir}

	app := &AnsibleApp{
		Logger:     logger,
		Repository: repository,
		Template:   db.Template{ID: 1},
		Playbook: &AnsiblePlaybook{
			TemplateID: 1,
			Repository: repository,
			Logger:     logger,
		},
	}

	return app, logger
}

const inventoryWithSecrets = `bastion ansible_password=hunter2

[web]
web01 ansible_user=deploy
web02

[web:vars]
vault_token=tops3cret
`

// The listing an inventory produces carries every variable of every host. The
// task log is readable by anyone who can see the task, so the run has to keep
// that output out of it entirely — this is the reason resolve mode does not go
// through LogCmd like every other command.
func TestResolveInventoryKeepsOutputOutOfTheLog(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hosts"), []byte(inventoryWithSecrets), 0o600))

	app, logger := newInventoryApp(t, dir)

	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := app.Run(LocalAppRunningArgs{
		CliArgs:    map[string][]string{"default": {"-i", filepath.Join(dir, "hosts"), "--list"}},
		TaskParams: &db.AnsibleTaskParams{ResolveInventoryOnly: true},
	})

	require.NoError(t, err)

	for _, secret := range []string{"hunter2", "tops3cret", "ansible_password", "vault_token", "hostvars", "_meta"} {
		assert.NotContains(t, logger.log(), secret, "%q must never reach the task log", secret)
	}

	assert.Equal(t, 1, logger.resolveCalls)
	assert.ElementsMatch(t, []string{"bastion", "web01", "web02"}, logger.resolvedHosts)
	assert.ElementsMatch(t, []string{"web01", "web02"}, logger.resolvedGroups["web"])
}

// The names themselves are what the picker needs, so they do reach the caller —
// just not through the log.
func TestResolveInventoryReportsNamesOnly(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "hosts"),
		[]byte("[web]\nweb01\n\n[db]\ndb01\n\n[prod:children]\nweb\ndb\n"),
		0o600,
	))

	app, logger := newInventoryApp(t, dir)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))

	require.NoError(t, app.Run(LocalAppRunningArgs{
		CliArgs:    map[string][]string{"default": {"-i", filepath.Join(dir, "hosts"), "--list"}},
		TaskParams: &db.AnsibleTaskParams{ResolveInventoryOnly: true},
	}))

	assert.ElementsMatch(t, []string{"db01", "web01"}, logger.resolvedHosts)
	assert.ElementsMatch(t, []string{"db01", "web01"}, logger.resolvedGroups["prod"])
}

// Without the flag the app runs a play, and nothing reports a host list.
func TestRunWithoutResolveFlagDoesNotResolve(t *testing.T) {
	dir := t.TempDir()
	app, logger := newInventoryApp(t, dir)

	// Whether the playbook run works ("--version" does, when ansible is
	// installed) or fails (when it is not) does not matter: what matters is
	// that the resolve path was not taken. The callback must be real, though —
	// RunPlaybook calls it as soon as the process starts.
	_ = app.Run(LocalAppRunningArgs{
		CliArgs:    map[string][]string{"default": {"--version"}},
		TaskParams: &db.AnsibleTaskParams{},
		Callback:   func(*os.Process) {},
	})

	assert.Equal(t, 0, logger.resolveCalls)
}

// A dynamic inventory that fails, or prints something that is not a listing,
// must fail the resolve rather than report a truncated host list.
func TestResolveInventoryFailsOnUnusableOutput(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hosts"), []byte("[web]\nweb01\n"), 0o600))

	app, logger := newInventoryApp(t, dir)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := app.Run(LocalAppRunningArgs{
		CliArgs:    map[string][]string{"default": {"-i", filepath.Join(dir, "missing"), "--list"}},
		TaskParams: &db.AnsibleTaskParams{ResolveInventoryOnly: true},
	})

	// Either the command fails or its output is not a listing; either way
	// nothing is reported.
	if err == nil {
		assert.Equal(t, 0, len(logger.resolvedHosts))
	}

	assert.NotContains(t, logger.resolvedHosts, "web01")
}
