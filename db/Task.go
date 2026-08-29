package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/semaphoreui/semaphore/pkg/git"
	"github.com/semaphoreui/semaphore/pkg/tz"

	"github.com/go-gorp/gorp/v3"

	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
)

type DefaultTaskParams struct {
}

type TerraformTaskParams struct {
	Plan        bool `json:"plan"`
	Destroy     bool `json:"destroy"`
	AutoApprove bool `json:"auto_approve"`
	Upgrade     bool `json:"upgrade"`
	Reconfigure bool `json:"reconfigure"`
}

type AnsibleTaskParams struct {
	Debug             bool     `json:"debug"`
	DebugLevel        int      `json:"debug_level"`
	DryRun            bool     `json:"dry_run"`
	Diff              bool     `json:"diff"`
	Limit             []string `json:"limit"`
	Tags              []string `json:"tags"`
	SkipTags          []string `json:"skip_tags"`
	SkipGalaxyInstall bool     `json:"skip_galaxy_install"`

	// ResolveInventoryOnly turns the task into a listing of the inventory's
	// hosts instead of a play. It exists for dynamic inventories, whose hosts
	// only a runner may enumerate because doing so runs a program.
	ResolveInventoryOnly bool `json:"resolve_inventory_only"`
}

// Task is a model of a task which will be executed by the runner
type Task struct {
	ID         int `db:"id" json:"id"`
	TemplateID int `db:"template_id" json:"template_id" binding:"required"`
	ProjectID  int `db:"project_id" json:"project_id"`

	Status task_logger.TaskStatus `db:"status" json:"status"`

	// override variables
	Playbook    string  `db:"playbook" json:"playbook"`
	Environment string  `db:"environment" json:"environment,omitempty"`
	Secret      string  `db:"-" json:"secret,omitempty"`
	Arguments   *string `db:"arguments" json:"arguments,omitempty"`
	GitBranch   *string `db:"git_branch" json:"git_branch,omitempty"`

	UserID        *int `db:"user_id" json:"user_id,omitempty"`
	IntegrationID *int `db:"integration_id" json:"integration_id,omitempty"`
	ScheduleID    *int `db:"schedule_id" json:"schedule_id,omitempty"`
	// RunnerID is set while a task is assigned to a remote runner (cleared when the task finishes).
	// Used so runner progress API can authorize updates on any HA node.
	RunnerID *int `db:"runner_id" json:"-"`

	Created time.Time  `db:"created" json:"created"`
	Start   *time.Time `db:"start" json:"start,omitempty"`
	End     *time.Time `db:"end" json:"end,omitempty"`

	Message string `db:"message" json:"message,omitempty"`

	// CommitHash is a git commit hash of playbook repository which
	// was active when task was created.
	CommitHash *string `db:"commit_hash" json:"commit_hash,omitempty"`
	// CommitMessage contains message retrieved from git repository after checkout to CommitHash.
	// It is readonly by API.
	CommitMessage  string `db:"commit_message" json:"commit_message,omitempty"`
	BuildTaskID    *int   `db:"build_task_id" json:"build_task_id,omitempty"`
	WorkflowRunID  *int   `db:"workflow_run_id" json:"workflow_run_id,omitempty"`
	WorkflowNodeID *int   `db:"workflow_node_id" json:"workflow_node_id,omitempty"`
	// Version is a build version.
	// This field available only for Build tasks.
	Version *string `db:"version" json:"version,omitempty"`

	InventoryID *int `db:"inventory_id" json:"inventory_id,omitempty"`

	Params MapStringAnyField `db:"params" json:"params,omitempty"`

	Artifacts *string `db:"artifacts" json:"artifacts,omitempty"`

	// Limit is deprecated, use Params.Limit instead
	Limit string `db:"-" json:"limit"`

	// TargetHostVar is the name of the extra variable the picked hosts are
	// published under, resolved from the server's configuration and carried to
	// whichever process executes the task. It travels rather than being read
	// from util.Config by the executor because a remote runner is a separate
	// process with its own configuration file: reading it there would make the
	// setting silently ineffective on every runner it was not copied to, and
	// make one template behave differently depending on which node picked the
	// task up.
	//
	// Not persisted, and stamped unconditionally by TaskRunner.populateDetails:
	// the field is bound from the request body like the rest of the task, so a
	// value the caller supplied must never survive.
	TargetHostVar string `db:"-" json:"target_host_var,omitempty"`
}

func (task *Task) ExtractParams(target any) (err error) {
	content, err := json.Marshal(task.Params)
	if err != nil {
		return
	}
	err = json.Unmarshal(content, target)
	return
}

// TaskParamResolveInventoryOnly is the params key behind
// AnsibleTaskParams.ResolveInventoryOnly. It is named here because
// IsInventoryResolve reads the flag out of the map directly and the two must
// not drift apart.
const TaskParamResolveInventoryOnly = "resolve_inventory_only"

// IsInventoryResolve reports whether this task lists an inventory's hosts
// instead of running its template. Such a task is a service errand of the host
// picker: it must not trigger anything a real run triggers — no autorun
// children, no alerts, no webhooks — and its failure settles the pending
// snapshot claim that dispatched it.
//
// The flag is read out of the params map rather than through ExtractParams:
// this is asked twice on every status change plus on both sides of the resolve
// path, and a marshal and unmarshal of the whole map for one boolean is not
// worth it. Reading the key directly is also the stricter answer — an
// unrelated malformed param (a "limit" stored as a string, say) used to make
// ExtractParams fail and turn a genuine resolve into an ordinary run.
func (task *Task) IsInventoryResolve() bool {
	resolve, ok := task.Params[TaskParamResolveInventoryOnly].(bool)
	return ok && resolve
}

// NormalizeDeprecatedLimit moves the deprecated top-level "limit" field of a
// task into Params, which is where everything downstream reads it from.
//
// It has to run before the task is validated, not only from the PreInsert hook:
// the hook fires inside CreateTask, which ValidateNewTask has already returned
// from, so a limit left for the hook to move would reach --limit and the extra
// vars without ValidateAnsibleLimit ever having seen it.
//
// Idempotent by design — an existing Params["limit"] always wins, so calling it
// from both places moves the value exactly once.
func (task *Task) NormalizeDeprecatedLimit() {
	if task.Limit == "" {
		return
	}

	if _, ok := task.Params["limit"]; ok {
		return
	}

	if task.Params == nil {
		task.Params = make(MapStringAnyField)
	}

	limits := strings.Split(task.Limit, ",")

	for i := range limits {
		limits[i] = strings.TrimSpace(limits[i])
	}

	task.Params["limit"] = limits
}

// PreInsert is a hook which is called before inserting task into database.
// Called directly in BoltDB implementation.
func (task *Task) PreInsert(gorp.SqlExecutor) error {
	task.Created = tz.In(task.Created)

	task.NormalizeDeprecatedLimit()

	return nil
}

func (task *Task) PreUpdate(gorp.SqlExecutor) error {
	if task.Start != nil {
		start := tz.In(*task.Start)
		task.Start = &start
	}

	if task.End != nil {
		end := tz.In(*task.End)
		task.End = &end
	}
	return nil
}

func (task *Task) GetIncomingVersion(d Store) *string {
	if task.BuildTaskID == nil {
		return nil
	}

	buildTask, err := d.GetTask(task.ProjectID, *task.BuildTaskID)

	if err != nil {
		return nil
	}

	tpl, err := d.GetTemplate(task.ProjectID, buildTask.TemplateID)
	if err != nil {
		return nil
	}

	if tpl.Type == TemplateBuild {
		return buildTask.Version
	}

	return buildTask.GetIncomingVersion(d)
}

func (task *Task) GetUrl() *string {
	if util.Config.WebHost != "" {
		taskUrl := fmt.Sprintf("%s/project/%d/history?t=%d", util.Config.WebHost, task.ProjectID, task.ID)
		return &taskUrl
	}

	return nil
}

func (task *Task) ValidateNewTask(template Template) error {
	// The deprecated field is a second way to say the same thing, and the hook
	// that used to move it runs after this function. Moving it here is what
	// makes the checks below see the value the executor will actually use.
	task.NormalizeDeprecatedLimit()

	if task.GitBranch != nil {
		if err := git.ValidateGitBranch(*task.GitBranch, "task"); err != nil {
			return err
		}
	}

	if task.CommitHash != nil {
		if err := git.ValidateCommitHash(*task.CommitHash, "task"); err != nil {
			return err
		}
	}

	if err := ValidatePlaybookPath(task.Playbook, "task"); err != nil {
		return err
	}

	var params any
	switch template.App {
	case AppAnsible:
		params = &AnsibleTaskParams{}
	case AppTerraform, AppTofu, AppTerragrunt:
		params = &TerraformTaskParams{}
	default:
		params = &DefaultTaskParams{}
	}

	if err := task.ExtractParams(params); err != nil {
		return err
	}

	if ansibleParams, ok := params.(*AnsibleTaskParams); ok {
		if err := ValidateAnsibleLimit(ansibleParams.Limit, "task"); err != nil {
			return err
		}

		if err := ValidateTaskEnvironment(task.Environment); err != nil {
			return err
		}

		return ValidateTaskSecret(task.Secret)
	}

	return nil
}

func (task *TaskWithTpl) Fill(d Store) error {
	if task.BuildTaskID != nil {
		build, err := d.GetTask(task.ProjectID, *task.BuildTaskID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		task.BuildTask = &build
	}
	return nil
}

// TaskWithTpl is the task data with additional fields
type TaskWithTpl struct {
	Task
	TemplatePlaybook string       `db:"tpl_playbook" json:"tpl_playbook"`
	TemplateAlias    string       `db:"tpl_alias" json:"tpl_alias"`
	TemplateType     TemplateType `db:"tpl_type" json:"tpl_type,omitempty"`
	TemplateApp      TemplateApp  `db:"tpl_app" json:"tpl_app,omitempty"`
	UserName         *string      `db:"user_name" json:"user_name,omitempty"`
	// UsedRunnerID exposes Task.RunnerID through the API. Task.RunnerID itself
	// stays unexported (json:"-"); we re-select it under a distinct column so the
	// embedded struct's mapping is not duplicated.
	UsedRunnerID   *int    `db:"used_runner_id" json:"used_runner_id,omitempty"`
	UsedRunnerName *string `db:"used_runner_name" json:"used_runner_name,omitempty"`
	BuildTask      *Task   `db:"-" json:"build_task,omitempty"`
}

// TaskOutput is the ansible log output from the task
type TaskOutput struct {
	ID      int       `db:"id" json:"id"`
	TaskID  int       `db:"task_id" json:"task_id"`
	Time    time.Time `db:"time" json:"time"`
	Output  string    `db:"output" json:"output"`
	StageID *int      `db:"stage_id" json:"stage_id"`
}

type TaskStageType string

const (
	TaskStageInit          TaskStageType = "init"
	TaskStageTerraformPlan TaskStageType = "terraform_plan"
	TaskStageRunning       TaskStageType = "running"
	TaskStagePrintResult   TaskStageType = "print_result"
)

type TaskStage struct {
	ID     int           `db:"id" json:"id"`
	TaskID int           `db:"task_id" json:"task_id"`
	Start  *time.Time    `db:"start" json:"start"`
	End    *time.Time    `db:"end" json:"end"`
	Type   TaskStageType `db:"type" json:"type"`
}

type TaskStageWithResult struct {
	ID            int           `db:"id" json:"id"`
	TaskID        int           `db:"task_id" json:"task_id"`
	Start         *time.Time    `db:"start" json:"start"`
	End           *time.Time    `db:"end" json:"end"`
	StartOutputID *int          `db:"start_output_id" json:"start_output_id"`
	EndOutputID   *int          `db:"end_output_id" json:"end_output_id"`
	Type          TaskStageType `db:"type" json:"type"`
	JSON          string        `db:"json" json:"-"`
	Result        any           `db:"-" json:"result"`
}

type TaskStageResult struct {
	ID      int    `db:"id" json:"id"`
	TaskID  int    `db:"task_id" json:"task_id"`
	StageID int    `db:"stage_id" json:"stage_id"`
	JSON    string `db:"json" json:"json"`
}
