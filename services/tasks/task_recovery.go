package tasks

import (
	"strconv"
	"strings"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/pkg/tz"
	"github.com/semaphoreui/semaphore/util"
	log "github.com/sirupsen/logrus"
)

// recoverableTaskStatuses are the statuses recovery ever touches: each one
// implies a process was doing something (running the job, about to start it,
// or being asked to stop it) that died along with the server. A queued task
// (waiting, waiting_confirmation, confirmed, rejected) never had a process
// behind it, so it survives a restart unharmed and must be left in the queue
// rather than swept up with the rest of task_logger.UnfinishedTaskStatuses().
//
// This is kept separate from UnfinishedTaskStatuses() rather than narrowing
// that function, because GetUnfinishedTasks and other callers need the full
// set for their own purposes.
var recoverableTaskStatuses = map[task_logger.TaskStatus]bool{
	task_logger.TaskRunningStatus:  true,
	task_logger.TaskStartingStatus: true,
	task_logger.TaskStoppingStatus: true,
}

// maxTasksToRecover caps one recovery pass. Every row costs several queries
// (the update, the log line, the event, and for a dispatched task a full
// rehydration), so an installation that accumulated stale unfinished rows over
// months would otherwise turn each restart into a long stall before anything
// can be started. What is left over is reported and picked up by the next
// restart; the rows are stale either way, and the ones skipped are the newest,
// which are also the ones a live runner may still settle by itself.
const maxTasksToRecover = 1000

// recoverInterruptedTasks reconciles the database against the fact that this
// process has just started: whatever the previous process was doing died with
// it, while the task rows it wrote live on. Without this, a task interrupted
// by a restart shows "running" forever, its Stop button reaches a pool that
// has never heard of it, and only a raw SQL update can put it out of its
// misery (issues #1376, #1385).
//
// Local tasks are declared stopped: their ansible process was a child of the
// dead server, so nothing is running any more, and stopped — not failed — is
// all that is honestly known.
//
// Tasks dispatched to a runner are the opposite case: the runner is a separate
// process and its job survived our restart. Those are re-registered in the
// pool, which is all the existing machinery needs — a live runner's next
// progress report finds the task again, and a dead runner's task is failed by
// the reconciler once its offline timeout expires.
//
// HA is excluded, but the check is util.HAEnabled(), not "am I the only
// process on this database" — the two are not the same thing. A Kubernetes
// Deployment at replicas:1 with the default RollingUpdate strategy starts the
// new pod before the old one terminates (maxSurge rounds up to at least 1), and
// a plain docker-compose or blue-green restart without HA configured does the
// same: two processes briefly share one database with HA still turned off. If
// this ever runs while the previous process is still alive, the statuses below
// are chosen to limit the damage — a live process it steps on will simply
// resume once it notices its task changed under it — but the pro build's
// orphan cleaner (services/ha), which is meant to be the real answer for a
// dead node in HA, does not fill this gap either: it only runs when HA is
// enabled.
func (p *TaskPool) recoverInterruptedTasks() {
	if util.HAEnabled() {
		return
	}

	tasks, total, err := p.store.GetUnfinishedTasks(maxTasksToRecover)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"context": "task_recovery",
		}).Error("failed to list unfinished tasks; stale statuses will remain")
		return
	}

	selected := len(tasks)

	recoverable := tasks[:0]
	for _, task := range tasks {
		if recoverableTaskStatuses[task.Status] {
			recoverable = append(recoverable, task)
		}
	}
	tasks = recoverable

	log.WithFields(log.Fields{
		"count":      len(tasks),
		"unfinished": total,
		"examined":   selected,
		"context":    "task_recovery",
	}).Info("recovering tasks interrupted by a server restart")

	if selected < total {
		log.WithFields(log.Fields{
			"examined":   selected,
			"unfinished": total,
			"context":    "task_recovery",
		}).Warn("more unfinished tasks than one recovery pass handles; the rest are left for the next restart")
	}

	for _, task := range tasks {
		if task.RunnerID == nil {
			p.stopInterruptedTask(task, "Task was interrupted by a server restart.")
			continue
		}

		runner, err := p.hydrateTaskRunner(task.ID, task.ProjectID)
		if err != nil {
			// The task cannot be reconstructed — a broken or deleted
			// dependency. Its progress reports would be dropped anyway, so
			// close it out rather than leave it running forever.
			log.WithError(err).WithFields(log.Fields{
				"task_id": task.ID,
				"context": "task_recovery",
			}).Warn("failed to rehydrate a dispatched task; stopping it")

			p.stopInterruptedTask(task, "Task could not be restored after a server restart.")
			continue
		}

		p.state.SetRunning(runner)
		p.state.AddActive(runner.Task.ProjectID, runner)

		log.WithFields(log.Fields{
			"task_id":   task.ID,
			"runner_id": *task.RunnerID,
			"context":   "task_recovery",
		}).Info("restored a dispatched task after restart")
	}
}

// stopInterruptedTask closes out a task row directly, without a TaskRunner:
// the task this row described no longer exists as a process, so there is
// nothing to signal — only the record to correct.
//
// A task that already reached a final status is left alone. This path is also
// reached from TaskPool.StopTask when a task is not in the pool and its
// details can no longer be loaded (a broken template or repository), and the
// caller there passes whatever row it already had in hand — which, for a task
// that finished on its own between that load and this call, would otherwise
// turn a successful run into a false "stopped".
func (p *TaskPool) stopInterruptedTask(task db.Task, reason string) {
	if task.Status.IsFinished() {
		return
	}

	now := tz.Now()
	task.Status = task_logger.TaskStoppedStatus

	if task.End == nil {
		task.End = &now
	}

	if err := p.store.UpdateTask(task); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"task_id": task.ID,
			"context": "task_recovery",
		}).Error("failed to stop an interrupted task")
		return
	}

	// The line in the task log is what tells a person why their task ended
	// without a result. Failing to write it does not undo the stop.
	if _, err := p.store.CreateTaskOutput(db.TaskOutput{
		TaskID: task.ID,
		Time:   now,
		Output: reason,
	}); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"task_id": task.ID,
			"context": "task_recovery",
		}).Warn("failed to note the interruption in the task log")
	}

	// The event feed is how an open UI learns the status changed; the normal
	// stop path writes one via createTaskEvent, and this path must not be the
	// silent exception.
	desc := "Task ID " + strconv.Itoa(task.ID) + " finished with status " +
		strings.ToUpper(string(task_logger.TaskStoppedStatus))
	objType := db.EventTask

	if _, err := p.store.CreateEvent(db.Event{
		UserID:      task.UserID,
		ProjectID:   &task.ProjectID,
		ObjectType:  &objType,
		ObjectID:    &task.ID,
		Description: &desc,
	}); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"task_id": task.ID,
			"context": "task_recovery",
		}).Warn("failed to record the interruption in the event feed")
	}

	// An interrupted inventory resolve would otherwise leave its snapshot
	// saying "resolving" until the claim expires; settle it the same way a
	// failed resolve task does.
	if task.IsInventoryResolve() && task.InventoryID != nil {
		if err := p.store.FailInventorySnapshotResolve(
			task.ProjectID, *task.InventoryID, task.ID, reason,
		); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"task_id": task.ID,
				"context": "task_recovery",
			}).Warn("failed to settle the inventory resolve claim")
		}
	}
}
