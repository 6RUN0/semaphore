package tasks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/tz"

	"github.com/semaphoreui/semaphore/api/sockets"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
	log "github.com/sirupsen/logrus"
)

func (t *TaskRunner) Log(msg string) {
	t.LogWithTime(tz.Now(), msg)
}

func (t *TaskRunner) Logf(format string, a ...any) {
	t.LogfWithTime(tz.Now(), format, a...)
}

func (t *TaskRunner) LogWithTime(now time.Time, msg string) {
	t.sendToWs(now, msg)

	t.pool.logger <- logRecord{
		task:   t,
		output: msg,
		time:   now,
	}

	for _, l := range t.logListeners {
		l(now, msg)
	}
}

func (t *TaskRunner) sendToWs(now time.Time, msg string) {
	for _, user := range t.users {
		b, err := json.Marshal(&map[string]any{
			"type":       "log",
			"output":     msg,
			"time":       now,
			"task_id":    t.Task.ID,
			"project_id": t.Task.ProjectID,
		})

		util.LogPanic(err)
		sockets.Message(user, b)
	}
}

func (t *TaskRunner) LogfWithTime(now time.Time, format string, a ...any) {
	t.LogWithTime(now, fmt.Sprintf(format, a...))
}

func (t *TaskRunner) LogCmd(cmd *exec.Cmd) func() {
	// io.PipeWriter is not *os.File, so os/exec owns and waits for output copying.
	stderr, stderrWriter := io.Pipe()
	stdout, stdoutWriter := io.Pipe()
	cmd.Stderr = stderrWriter
	cmd.Stdout = stdoutWriter

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		t.logPipe(stderr)
	}()
	go func() {
		defer wg.Done()
		t.logPipe(stdout)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = stderrWriter.Close()
			_ = stdoutWriter.Close()
			wg.Wait()
		})
	}
}

func (t *TaskRunner) SetCommit(hash, message string) {

	t.Task.CommitHash = &hash
	t.Task.CommitMessage = message

	if err := t.pool.store.UpdateTask(t.Task); err != nil {
		t.panicOnError(err, "Failed to update task commit")
	}
}

// SetResolvedInventory stores a host list a runner produced by executing a
// dynamic inventory.
//
// The inventory it is stored against comes from the task row on this side, not
// from the report, so a runner can only ever affect the inventory its own task
// named. A task that resolved nothing has no inventory and is ignored.
//
// The IsInventoryResolve check is what keeps an ordinary playbook run from
// overwriting an inventory's snapshot at all: db.Task.Params is written by the
// server, never by the runner, so this is a server-side decision even though
// it runs in response to the runner's report.
func (t *TaskRunner) SetResolvedInventory(hosts []string, groups map[string][]string) {
	if !t.Task.IsInventoryResolve() {
		t.Log("resolved inventory reported for a task that is not an inventory resolve, ignoring")
		return
	}

	content := db.NewInventorySnapshotContent(hosts, groups)

	if t.Task.InventoryID == nil {
		t.Log("resolved inventory reported for a task that has no inventory, ignoring")
		return
	}

	// The inventory the task asked for is not always the one that ran:
	// populateDetails substitutes the template's own inventory when the
	// template forbids overriding it. Storing the result under the requested
	// id would fill inventory A with the hosts of inventory B and then freeze
	// it, because a stored snapshot is not rebuilt until someone refreshes it
	// by hand. t.Inventory is what ansible was actually pointed at, so it has
	// to agree.
	if t.Inventory.ID != *t.Task.InventoryID {
		t.Log("resolved inventory belongs to a different inventory than the task requested, ignoring")
		return
	}

	if t.pool == nil {
		return
	}

	err := t.pool.inventorySnapshotService.StoreResolved(
		t.Task.ProjectID,
		*t.Task.InventoryID,
		t.Task.ID,
		content,
	)

	if err != nil {
		// A host list that cannot be stored must not fail the task that
		// produced it: the play itself ran, and the picker simply keeps
		// whatever it had.
		t.Log("failed to store the resolved inventory: " + err.Error())
	}
}

func (t *TaskRunner) SetStatus(status task_logger.TaskStatus) {
	if status == t.Task.Status {
		return
	}

	oldStatus := t.Task.Status

	switch t.Task.Status { // check old status
	case task_logger.TaskConfirmed:
		if status == task_logger.TaskWaitingConfirmation {
			return
		}
	case task_logger.TaskRunningStatus:
		if status == task_logger.TaskWaitingStatus {
			return
		}
	case task_logger.TaskStoppingStatus:
		if status == task_logger.TaskWaitingStatus || status == task_logger.TaskRunningStatus || status == task_logger.TaskWaitingConfirmation {
			//panic("stopping TaskRunner cannot be " + status)
			return
		}
	case task_logger.TaskSuccessStatus:
	case task_logger.TaskFailStatus:
	case task_logger.TaskStoppedStatus:
		return
	}

	t.Task.Status = status
	if t.pool != nil {
		t.pool.metrics.RecordTaskStatusChange(oldStatus, status)
	}

	if status == task_logger.TaskRunningStatus {
		now := tz.Now()
		t.Task.Start = &now
	}

	t.saveStatus()

	if localJob, ok := t.job.(*LocalExecutor); ok {
		localJob.SetStatus(status)
	}

	// An inventory resolve is the host picker's errand, not a run anyone
	// subscribed to: "Task #N succeeded" on every refresh would train people
	// to ignore the real ones. Its failure surfaces on the snapshot instead.
	if !t.Task.IsInventoryResolve() {
		if status == task_logger.TaskFailStatus {
			t.sendMailAlert()
		}

		if status.IsNotifiable() {
			t.sendTelegramAlert()
			t.sendSlackAlert()
			t.sendRocketChatAlert()
			t.sendMicrosoftTeamsAlert()
			t.sendDingTalkAlert()
			t.sendGotifyAlert()
		}
	}

	t.settleInventoryResolve(status)

	for _, l := range t.statusListeners {
		l(status)
	}

	log.WithFields(log.Fields{
		"task_id": t.Task.ID,
		"context": "task_logger",
		"status":  status,
	}).Info("Task status updated")
}

// settleInventoryResolve closes out the pending snapshot claim of an inventory
// resolve whose task died without producing a host list. Without it the picker
// would show "resolving" until the claim's 30-minute expiry, with no error and
// no way to retry (issue class: a typo in the inventory script).
//
// A success is settled too, and for the same reason: the store call is scoped
// to a claim that is still pending, so a resolve that did report its hosts has
// nothing left to settle — StoreResolved already replaced the row. What is left
// is a task that ended successfully without ever reporting a list: an older
// runner that does not know the field, or a report the server refused because
// the inventory that ran was not the one the task asked for. Leaving those
// pending locks the inventory for the claim's 30 minutes, with no error shown
// and no way to retry.
//
// The claim is scoped to this task's own id, so one that expired and was taken
// over by a later refresh stays untouched.
func (t *TaskRunner) settleInventoryResolve(status task_logger.TaskStatus) {
	if !status.IsFinished() {
		return
	}

	if !t.Task.IsInventoryResolve() || t.Task.InventoryID == nil {
		return
	}

	if t.pool == nil {
		return
	}

	message := "the resolve task ended with status " + string(status)
	if status == task_logger.TaskSuccessStatus {
		message = "the resolve task finished without reporting a host list"
	}

	err := t.pool.store.FailInventorySnapshotResolve(
		t.Task.ProjectID,
		*t.Task.InventoryID,
		t.Task.ID,
		message,
	)

	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"task_id": t.Task.ID,
			"context": "task_logger",
		}).Warn("failed to settle the inventory resolve claim")
	}
}

func (t *TaskRunner) panicOnError(err error, msg string) {
	if err == nil {
		return
	}

	t.Log(msg)
	util.LogPanicF(err, log.Fields{"error": msg})
}

func (t *TaskRunner) logPipe(reader io.Reader) {
	linesCh := make(chan string, 100000)
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		for line := range linesCh {
			t.Log(line)
		}
	}()
	defer wg.Wait()

	scanner := bufio.NewScanner(reader)
	const maxCapacity = 10 * 1024 * 1024 // 10 MB
	buf := make([]byte, maxCapacity)
	scanner.Buffer(buf, maxCapacity)

	for scanner.Scan() {
		line := scanner.Text()
		linesCh <- line
	}

	close(linesCh)

	err := scanner.Err()

	if err != nil {
		msg := "Failed to read TaskRunner output"

		switch err.Error() {
		case "EOF",
			"os: process already finished",
			"read |0: file already closed":
			return // it is ok
		case "bufio.Scanner: token too long":
			msg = "TaskRunner output exceeds the maximum allowed size of 10MB"
		}

		t.kill() // kill the job because stdout cannot be read.

		log.WithError(err).WithFields(log.Fields{
			"task_id": t.Task.ID,
			"context": "task_logger",
		}).Error(msg)

		t.Log("Fatal error: " + msg)
	}
}
