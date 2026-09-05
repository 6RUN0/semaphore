package tasks

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"path"
	"strconv"

	"github.com/semaphoreui/semaphore/pkg/ssh"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/semaphoreui/semaphore/pkg/keylock"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
	log "github.com/sirupsen/logrus"
)

type LocalExecutor struct {
	Task        db.Task
	Template    db.Template
	Inventory   db.Inventory
	Repository  db.Repository
	Environment db.Environment
	Secret      string             // Secret contains secrets received from Survey variables
	Logger      task_logger.Logger // Logger allows to send logs and status to the server
	JWT         string             // server-signed JWT

	App db_lib.LocalApp

	killed  bool // killed means that API request to stop the job has been received
	Process *os.Process

	sshKeyInstallation     ssh.AccessKeyInstallation
	becomeKeyInstallation  ssh.AccessKeyInstallation
	vaultFileInstallations map[string]ssh.AccessKeyInstallation

	KeyInstaller db_lib.AccessKeyInstaller

	// RepoLock serializes git operations on the shared per-template repository
	// directory. Tasks of the same template may run in parallel
	// (AllowParallelTasks) but share one working copy — concurrent `git pull`
	// on it is a race. Wired by TaskPool (local) and LocalExecutorProvider
	// (runner). Must be non-nil when Prepare is called for a git repository.
	RepoLock *keylock.KeyLock

	WorkflowArtifacts map[string]any

	// Prepared state — populated by Prepare(), consumed by Run(). Lifted out of Run()
	// local variables so the lifecycle phases (Prepare / underlying App.Run / Cleanup)
	// can be invoked independently by callers that want phased execution (e.g. the
	// upcoming KubernetesExecutor, which will call Prepare to compute args/env on the
	// runner host and then dispatch them into a Pod).
	preparedEnv       []string
	preparedArgsMap   map[string][]string
	preparedInputs    map[string]string
	preparedParams    any
	preparedTplParams any
	prepared          bool
}

func (t *LocalExecutor) IsKilled() bool {
	return t.killed
}

// Async is false: LocalJob.Run executes the task synchronously and returns only
// once it has finished, so the task is finalized by TaskRunner.run.
func (t *LocalExecutor) Async() bool {
	return false
}

func (t *LocalExecutor) Kill() {
	t.killed = true

	if t.Process == nil {
		return
	}

	err := t.Process.Kill()
	if err != nil {
		t.Log(err.Error())
	}
}

func (t *LocalExecutor) Log(msg string) {
	t.Logger.Log(msg)
}

func (t *LocalExecutor) SetStatus(status task_logger.TaskStatus) {
	t.Logger.SetStatus(status)
}

// SetLogger wires the task log sink into the executor and threads it through to the
// underlying App so framework-specific logging (PTY output for Ansible, structured
// stages for Terraform) lands in the same stream.
func (t *LocalExecutor) SetLogger(logger task_logger.Logger) {
	if t.App != nil {
		t.Logger = t.App.SetLogger(logger)
		return
	}
	t.Logger = logger
}

func (t *LocalExecutor) SetCommit(hash, message string) {
	// TODO: is this the correct place to do?
	t.Task.CommitHash = &hash
	t.Task.CommitMessage = message
	t.Logger.SetCommit(hash, message)
}

func (t *LocalExecutor) SetResolvedInventory(hosts []string, groups map[string][]string) {
	t.Logger.SetResolvedInventory(hosts, groups)
}

func (t *LocalExecutor) getTaskDetails(username string, incomingVersion *string) (taskDetails map[string]any) {
	taskDetails = make(map[string]any)

	taskDetails["id"] = t.Task.ID

	if t.Task.Message != "" {
		taskDetails["message"] = t.Task.Message
	}

	taskDetails["username"] = username
	taskDetails["url"] = t.Task.GetUrl()
	taskDetails["commit_hash"] = t.Task.CommitHash
	taskDetails["commit_message"] = t.Task.CommitMessage
	taskDetails["inventory_name"] = t.Inventory.Name
	taskDetails["inventory_id"] = t.Inventory.ID
	taskDetails["repository_name"] = t.Repository.Name
	taskDetails["repository_id"] = t.Repository.ID

	if t.Template.Type != db.TemplateTask {
		taskDetails["type"] = t.Template.Type
		if incomingVersion != nil {
			taskDetails["incoming_version"] = incomingVersion
		}
		if t.Template.Type == db.TemplateBuild {
			taskDetails["target_version"] = t.Task.Version
		}
	}

	return
}

func (t *LocalExecutor) getEnvironmentExtraVars(username string, incomingVersion *string) (extraVars map[string]any, err error) {

	extraVars = make(map[string]any)

	if t.Environment.JSON != "" {
		err = json.Unmarshal([]byte(t.Environment.JSON), &extraVars)
		if err != nil {
			return
		}
	}

	// Merge Survey Secret variables. They arrive via the Secret field (kept
	// separate from Environment.JSON so they can be masked in logs and the
	// re-run dialog) but must still be passed to the task, including Shell and
	// Terraform tasks.
	if t.Secret != "" {
		extraSecretVars := make(map[string]any)
		if err = json.Unmarshal([]byte(t.Secret), &extraSecretVars); err != nil {
			return
		}
		maps.Copy(extraVars, extraSecretVars)
	}

	// Survey vars with the "env" target are delivered as process environment
	// variables (see getSurveyEnvVars), not as extra vars / CLI args.
	for _, v := range t.Template.SurveyVars {
		if v.Target == db.SurveyVarTargetEnv {
			delete(extraVars, v.Name)
		}
	}

	vars := make(map[string]any)
	vars["task_details"] = t.getTaskDetails(username, incomingVersion)
	extraVars["semaphore_vars"] = vars

	return
}

// effectiveAnsibleLimit is the host pattern the run is restricted to: the
// template's own value, replaced by the task's when the template allows it.
// Empty means the play is not restricted at all.
//
// Extracted rather than inlined because the pattern is needed twice: for
// --limit and, so that a playbook can select hosts through a variable, for the
// extra vars. Deriving it twice would let the two disagree.
func effectiveAnsibleLimit(tplParams db.AnsibleTemplateParams, params db.AnsibleTaskParams) string {
	if tplParams.AllowOverrideLimit && params.Limit != nil {
		return strings.Join(params.Limit, ",")
	}

	return strings.Join(tplParams.Limit, ",")
}

// ansibleLimitFlagInArgs reports whether a raw argument list restricts the play
// to hosts with ansible's --limit.
//
// The recognised forms follow python argparse, which ansible parses its command
// line with: an unambiguous long-option prefix, a value attached with "=", the
// short option alone, its value attached to it, and the short option inside a
// cluster of other short options. Option values are skipped rather than
// scanned, so an argument that only looks like the flag (--ssh-extra-args
// "-l root") is not mistaken for one. An option missing from the tables below
// can therefore cause a false rejection, never a missed one.
func ansibleLimitFlagInArgs(args []string) bool {
	// Long options of ansible-playbook that consume the next argument.
	longOptionsWithValue := map[string]bool{
		"--become-method":            true,
		"--become-pass-file":         true,
		"--become-password-file":     true,
		"--become-user":              true,
		"--conn-pass-file":           true,
		"--connection":               true,
		"--connection-password-file": true,
		"--extra-vars":               true,
		"--forks":                    true,
		"--inventory":                true,
		"--inventory-file":           true,
		"--key-file":                 true,
		"--module-path":              true,
		"--playbook-dir":             true,
		"--private-key":              true,
		"--scp-extra-args":           true,
		"--sftp-extra-args":          true,
		"--skip-tags":                true,
		"--ssh-common-args":          true,
		"--ssh-extra-args":           true,
		"--start-at-task":            true,
		"--tags":                     true,
		"--task-timeout":             true,
		"--timeout":                  true,
		"--user":                     true,
		"--vault-id":                 true,
		"--vault-pass-file":          true,
		"--vault-password-file":      true,
	}

	// Short options of ansible-playbook that consume a value. "l" is left out
	// on purpose: it is the one being looked for.
	const shortOptionsWithValue = "TMcefitu"

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			// Everything past the separator is positional.
			return false
		}

		if strings.HasPrefix(arg, "--") {
			name, _, hasAttachedValue := strings.Cut(arg, "=")

			if isAnsibleLimitLongOption(name) {
				return true
			}

			if !hasAttachedValue && longOptionsWithValue[name] {
				i++
			}

			continue
		}

		if len(arg) < 2 || arg[0] != '-' {
			continue
		}

		for j := 1; j < len(arg); j++ {
			if arg[j] == 'l' {
				return true
			}

			if !strings.ContainsRune(shortOptionsWithValue, rune(arg[j])) {
				continue
			}

			// The rest of the cluster is this option's value; when the option
			// closes the token, the value is the next argument.
			if j == len(arg)-1 {
				i++
			}

			break
		}
	}

	return false
}

// isAnsibleLimitLongOption reports whether name is --limit or one of the
// prefixes argparse accepts for it. Anything shorter than --lim is ambiguous
// with --list-hosts, --list-tasks and --list-tags, so argparse rejects it.
func isAnsibleLimitLongOption(name string) bool {
	const shortestAccepted = len("--lim")

	return len(name) >= shortestAccepted && strings.HasPrefix("--limit", name)
}

// getEnvironmentExtraVarsJSON renders the extra vars of an Ansible task. The
// limit is the host pattern the task form produced, empty when the run is not
// restricted to particular hosts.
//
// The alias name comes from the task (stamped by the server, see
// db.Task.TargetHostVar) and not from util.Config, because this runs in the
// process that executes the task, which for a remote runner is not the process
// that was configured.
func (t *LocalExecutor) getEnvironmentExtraVarsJSON(username string, incomingVersion *string, limit string) (str string, err error) {
	extraVars, err := t.getEnvironmentExtraVars(username, incomingVersion)
	if err != nil {
		return
	}

	// The alias names a variable Semaphore owns, so it is cleared whatever the
	// limit turns out to be. Leaving it alone when nothing is picked would let a
	// value the caller placed in task.environment under that name reach the
	// playbook as if Semaphore had selected the hosts — and it would do so
	// exactly when the template forbids overriding the limit, since that is a
	// case which produces an empty one.
	name := t.Task.TargetHostVar
	if name != "" {
		if _, taken := extraVars[name]; taken {
			// The name is configured server-wide while the colliding variable
			// belongs to one project, so the clash is invisible to whoever set
			// either of them. Without this line the symptom is a project variable
			// that inexplicably holds a list of hosts.
			t.Log(fmt.Sprintf(
				"Variable %q is reserved for the picked hosts (target_host_var) and its environment value is ignored",
				name))
		}

		delete(extraVars, name)
	}

	// An empty limit leaves both variables undefined rather than setting them to
	// an empty string. Jinja's default() replaces an undefined value and not an
	// empty one, so hosts: "{{ picked | default('all') }}" would otherwise match
	// no host at all the moment nothing was picked.
	if limit != "" {
		if vars, ok := extraVars["semaphore_vars"].(map[string]any); ok {
			vars["limit"] = limit
		} else {
			// Unreachable while getEnvironmentExtraVars assigns semaphore_vars
			// last and unconditionally. Reported rather than ignored: losing the
			// canonical variable sends a play that selects hosts through it to
			// default('all') — the whole inventory — and a silent skip would
			// leave nothing in the task log to explain why.
			t.Log("semaphore_vars is missing from the extra vars, semaphore_vars.limit was not set")
		}

		// The alias is what lets an existing playbook keep its own variable name
		// instead of being rewritten around semaphore_vars.
		if name != "" {
			extraVars[name] = limit
		}
	}

	ev, err := json.Marshal(extraVars)
	if err != nil {
		return
	}

	str = string(ev)

	return
}

func (t *LocalExecutor) getEnvironmentENV() (res []string, err error) {
	environmentVars := make(map[string]string)

	if t.Environment.ENV != nil {
		err = json.Unmarshal([]byte(*t.Environment.ENV), &environmentVars)
		if err != nil {
			return
		}
	}

	for key, val := range environmentVars {
		res = append(res, fmt.Sprintf("%s=%s", key, val))
	}

	for _, secret := range t.Environment.Secrets {
		if secret.Type != db.EnvironmentSecretEnv {
			continue
		}
		res = append(res, fmt.Sprintf("%s=%s", secret.Name, secret.Secret))
	}

	if t.JWT != "" {
		res = append(res, fmt.Sprintf("SEMAPHORE_JWT=%s", t.JWT))
	}

	return
}

// formatVarValue renders a survey/extra var value for single-string contexts
// (process env vars, terraform -var). Arrays and objects (produced by
// multi-select survey vars) are JSON-encoded so lists survive the round-trip
// instead of degrading to Go's fmt representation like "[1 2]". JSON is also
// what terraform expects for list/object values passed via -var.
func formatVarValue(val any) string {
	switch v := val.(type) {
	case string:
		return v
	case []any, map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// getSurveyEnvVars returns NAME=value pairs for survey vars with Target "env".
// Values are read from the merged task environment (Environment.JSON) and the
// Secret field — the same sources getEnvironmentExtraVars reads, which excludes
// these vars so each one is delivered exactly once.
func (t *LocalExecutor) getSurveyEnvVars() (res []string, err error) {
	vars := make(map[string]any)

	if t.Environment.JSON != "" {
		if err = json.Unmarshal([]byte(t.Environment.JSON), &vars); err != nil {
			return
		}
	}

	if t.Secret != "" {
		secretVars := make(map[string]any)
		if err = json.Unmarshal([]byte(t.Secret), &secretVars); err != nil {
			return
		}
		maps.Copy(vars, secretVars)
	}

	for _, v := range t.Template.SurveyVars {
		if v.Target != db.SurveyVarTargetEnv {
			continue
		}
		if val, ok := vars[v.Name]; ok {
			res = append(res, fmt.Sprintf("%s=%s", v.Name, formatVarValue(val)))
		}
	}

	return
}

func (t *LocalExecutor) getShellEnvironmentExtraENV(username string, incomingVersion *string) (extraShellVars []string) {
	taskDetails := t.getTaskDetails(username, incomingVersion)

	for taskDetail, taskDetailValue := range taskDetails {
		envVarName := fmt.Sprintf("SEMAPHORE_TASK_DETAILS_%s", strings.ToUpper(taskDetail))

		detailAsStr := ""
		switch taskDetailValueOfType := taskDetailValue.(type) {
		case string:
			detailAsStr = taskDetailValueOfType
		case *string:
			if taskDetailValueOfType != nil {
				detailAsStr = *taskDetailValueOfType
			}

		case int:
			detailAsStr = strconv.Itoa(taskDetailValueOfType)
		case *int:
			if taskDetailValueOfType != nil {
				detailAsStr = strconv.Itoa(*taskDetailValueOfType)
			}

		default:
			continue
		}

		if detailAsStr != "" {
			extraShellVars = append(extraShellVars, fmt.Sprintf("%s=%s", envVarName, util.ShellQuote(util.ShellStripUnsafe(detailAsStr))))
		}
	}

	return
}

// nolint: gocyclo
func (t *LocalExecutor) getShellArgs(username string, incomingVersion *string) (args []string, err error) {
	extraVars, err := t.getEnvironmentExtraVars(username, incomingVersion)

	if err != nil {
		t.Log(err.Error())
		t.Log("Error getting environment extra vars")
		return
	}

	templateArgs, taskArgs, err := t.getCLIArgs()
	if err != nil {
		t.Log(err.Error())
		return
	}

	// Script to run
	args = append(args, t.Template.Playbook)

	// Include Environment Secret Vars
	for _, secret := range t.Environment.Secrets {
		if secret.Type == db.EnvironmentSecretVar {
			args = append(args, fmt.Sprintf("%s=%s", secret.Name, secret.Secret))
		}
	}

	// Include extra args from template
	args = append(args, templateArgs...)

	// Include ExtraVars and Survey Vars
	for name, value := range extraVars {
		if name != "semaphore_vars" {
			args = append(args, fmt.Sprintf("%s=%s", name, formatVarValue(value)))
		}
	}

	// Include extra args from task
	args = append(args, taskArgs...)

	return
}

// nolint: gocyclo
func (t *LocalExecutor) getTerraformArgs(username string, incomingVersion *string) (argsMap map[string][]string, err error) {

	argsMap = make(map[string][]string)

	extraVars, err := t.getEnvironmentExtraVars(username, incomingVersion)

	if err != nil {
		t.Log(err.Error())
		t.Log("Could not remove command environment, if existent it will be passed to --extra-vars. This is not fatal but be aware of side effects")
		return
	}

	var params db.TerraformTaskParams
	err = t.Task.ExtractParams(&params)
	if err != nil {
		return
	}

	// Common args for destroy flag
	destroyArgs := []string{}
	if params.Destroy {
		destroyArgs = append(destroyArgs, "-destroy")
	}

	// Common args for environment variables
	varArgs := []string{}
	for name, value := range extraVars {
		if name == "semaphore_vars" {
			continue
		}
		varArgs = append(varArgs, "-var", fmt.Sprintf("%s=%s", name, formatVarValue(value)))
	}

	templateArgsMap, taskArgsMap, err := t.getCLIArgsMap()
	if err != nil {
		t.Log(err.Error())
		return
	}

	// Common args for environment secrets
	secretArgs := []string{}
	for _, secret := range t.Environment.Secrets {
		if secret.Type != db.EnvironmentSecretVar {
			continue
		}
		secretArgs = append(secretArgs, "-var", fmt.Sprintf("%s=%s", secret.Name, secret.Secret))
	}

	// Merge template and task args maps
	for stage, stageArgs := range templateArgsMap {
		argsMap[stage] = append([]string{}, stageArgs...)
	}

	for stage, stageArgs := range taskArgsMap {
		if existing, ok := argsMap[stage]; ok {
			argsMap[stage] = append(existing, stageArgs...)
		} else {
			argsMap[stage] = append([]string{}, stageArgs...)
		}
	}

	if len(argsMap) == 0 {
		argsMap["default"] = []string{}
	}

	// Add common args to each stage except init
	for stage := range argsMap {
		if stage == "init" {
			continue
		}
		// Prepend destroy args
		combined := append([]string{}, destroyArgs...)
		combined = append(combined, argsMap[stage]...)
		combined = append(combined, varArgs...)
		combined = append(combined, secretArgs...)
		argsMap[stage] = combined
	}

	return
}

// inventoryFilename is the path ansible is pointed at, which depends on where
// the inventory lives: a static one is written to a temporary file, a file one
// is either relative to the working directory or inside its own repository
// checkout.
func (t *LocalExecutor) inventoryFilename() (string, error) {
	switch t.Inventory.Type {
	case db.InventoryFile:
		if t.Inventory.RepositoryID == nil {
			return t.Inventory.GetFilename(), nil
		}

		return path.Join(t.tmpInventoryFullPath(), t.Inventory.GetFilename()), nil
	case db.InventoryStatic, db.InventoryStaticYaml:
		return t.tmpInventoryFullPath(), nil
	default:
		return "", fmt.Errorf("invalid inventory type")
	}
}

// resolveInventoryOnly reports whether this task lists an inventory's hosts
// instead of running a play.
func (t *LocalExecutor) resolveInventoryOnly() bool {
	return t.Task.IsInventoryResolve()
}

// checkResolvableVaults rejects a resolve whose template holds a stored vault
// password. Such a password is answered by typing into a pty, which a play has
// (getPlaybookArgs pairs --vault-id=name@prompt with an entry in inputs, and
// RunPlaybook feeds it) and a listing has not: RunInventoryListing runs
// ansible-inventory with no terminal and passes no inputs at all. Without this
// the run would hang on a prompt nobody can answer.
//
// It is checked in Prepare, before the repository is checked out and the keys
// are installed, so a person gets the reason rather than a stuck task.
func (t *LocalExecutor) checkResolvableVaults() error {
	for _, vault := range t.Template.Vaults {
		if vault.Type == db.TemplateVaultPassword {
			return fmt.Errorf("inventory cannot be resolved for a template whose vault is a " +
				"stored password, because a listing has no terminal to enter it into; " +
				"use a vault script instead")
		}
	}

	return nil
}

// getInventoryArgs builds the arguments for listing an inventory's hosts rather
// than running a play. Nothing of the task's own parameters belongs here: a
// listing takes no limit, no tags and no check mode.
//
// Vault ids are the exception: an inventory file, or the group_vars beside it,
// may itself be encrypted, and without them ansible-inventory fails with a raw
// "Attempting to decrypt but no vault secrets found" that says nothing about
// what to fix.
func (t *LocalExecutor) getInventoryArgs() ([]string, error) {
	inventoryFilename, err := t.inventoryFilename()
	if err != nil {
		return nil, err
	}

	args := []string{"-i", inventoryFilename, "--list"}

	// Sorted so the command line a person sees in the log is the same on every
	// run; map order alone would shuffle it.
	names := make([]string, 0, len(t.vaultFileInstallations))
	for name := range t.vaultFileInstallations {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		install := t.vaultFileInstallations[name]

		if install.Password != "" {
			// A password vault is answered over a pty, and a listing has
			// none -- see the check in Prepare, which is where this is
			// supposed to be caught.
			return nil, fmt.Errorf(
				"vault %q asks for a password, which cannot be supplied while listing an inventory", name)
		}

		if install.Script != "" {
			args = append(args, fmt.Sprintf("--vault-id=%s@%s", name, install.Script))
		}
	}

	return args, nil
}

// nolint: gocyclo
func (t *LocalExecutor) getPlaybookArgs(username string, incomingVersion *string) (args []string, inputs map[string]string, err error) {

	inputMap := make(map[db.AccessKeyRole]string)
	inputs = make(map[string]string)

	playbookName := t.Task.Playbook
	if playbookName == "" {
		playbookName = t.Template.Playbook
	}

	inventoryFilename, err := t.inventoryFilename()
	if err != nil {
		return
	}

	args = []string{
		"-i", inventoryFilename,
	}

	if t.Inventory.SSHKeyID != nil {
		switch t.Inventory.SSHKey.Type {
		case db.AccessKeySSH:
			if t.sshKeyInstallation.Login != "" {
				args = append(args, "--user", t.sshKeyInstallation.Login)
			}
		case db.AccessKeyLoginPassword:
			if t.sshKeyInstallation.Login != "" {
				args = append(args, "--user", t.sshKeyInstallation.Login)
			}
			if t.sshKeyInstallation.Password != "" {
				args = append(args, "--ask-pass")
				inputMap[db.AccessKeyRoleAnsibleUser] = t.sshKeyInstallation.Password
			}
		case db.AccessKeyNone:
		default:
			err = fmt.Errorf("access key does not suite for inventory's user credentials")
			return
		}
	}

	if t.Inventory.BecomeKeyID != nil {
		switch t.Inventory.BecomeKey.Type {
		case db.AccessKeyLoginPassword:
			if t.becomeKeyInstallation.Login != "" {
				args = append(args, "--become-user", t.becomeKeyInstallation.Login)
			}
			if t.becomeKeyInstallation.Password != "" {
				args = append(args, "--ask-become-pass")
				inputMap[db.AccessKeyRoleAnsibleBecomeUser] = t.becomeKeyInstallation.Password
			}
		case db.AccessKeyNone:
		default:
			err = fmt.Errorf("access key does not suite for inventory's sudo user credentials")
			return
		}
	}

	var tplParams db.AnsibleTemplateParams

	err = t.Template.FillParams(&tplParams)
	if err != nil {
		return
	}

	var params db.AnsibleTaskParams

	err = t.Task.ExtractParams(&params)
	if err != nil {
		return
	}

	if tplParams.AllowDebug && params.Debug {
		if params.DebugLevel < 1 {
			params.DebugLevel = 4
		}

		if params.DebugLevel > 6 {
			params.DebugLevel = 6
		}

		args = append(args, "-"+strings.Repeat("v", params.DebugLevel))
	}

	if params.Diff {
		args = append(args, "--diff")
	}

	if params.DryRun {
		args = append(args, "--check")
	}

	for name, install := range t.vaultFileInstallations {
		if install.Password != "" {
			args = append(args, fmt.Sprintf("--vault-id=%s@prompt", name))
			inputs[fmt.Sprintf("Vault password (%s):", name)] = install.Password
		}
		if install.Script != "" {
			args = append(args, fmt.Sprintf("--vault-id=%s@%s", name, install.Script))
		}
	}

	// Computed here rather than next to the other host-selection arguments below
	// because the extra vars carry it too, and they are rendered first.
	limit := effectiveAnsibleLimit(tplParams, params)

	extraVars, err := t.getEnvironmentExtraVarsJSON(username, incomingVersion, limit)
	if err != nil {
		t.Log(err.Error())
		t.Log("Could not remove command environment, if existent it will be passed to --extra-vars. This is not fatal but be aware of side effects")
	} else if extraVars != "" {
		args = append(args, "--extra-vars", extraVars)
	}

	for _, secret := range t.Environment.Secrets {
		if secret.Type != db.EnvironmentSecretVar {
			continue
		}
		args = append(args, "--extra-vars", fmt.Sprintf("%s=%s", secret.Name, secret.Secret))
	}

	templateArgs, taskArgs, err := t.getCLIArgs()
	if err != nil {
		t.Log(err.Error())
		return
	}

	// The raw task arguments are appended after the computed --limit, where
	// ansible lets the last one win. A limit hidden in them would therefore
	// override the host selection the parameters produced, bypassing
	// AllowOverrideLimit and contradicting the limit published through the
	// target host variable. The task limit parameter is the only way in.
	if ansibleLimitFlagInArgs(taskArgs) {
		err = fmt.Errorf("--limit is not allowed in the task arguments, use the task limit instead")
		t.Log(err.Error())
		return
	}

	var tags string
	var skipTags string

	// Fill fields from template
	if len(tplParams.Tags) > 0 {
		tags = strings.Join(tplParams.Tags, ",")
	}

	if len(tplParams.SkipTags) > 0 {
		skipTags = strings.Join(tplParams.SkipTags, ",")
	}

	// Fill fields from task

	if tplParams.AllowOverrideTags && params.Tags != nil {
		tags = strings.Join(params.Tags, ",")
	}

	if tplParams.AllowOverrideSkipTags && params.SkipTags != nil {
		skipTags = strings.Join(params.SkipTags, ",")
	}

	// Add final args

	if limit != "" {
		templateArgs = append(templateArgs, "--limit="+limit)
	}

	if tags != "" {
		templateArgs = append(templateArgs, "--tags="+tags)
	}

	if skipTags != "" {
		templateArgs = append(templateArgs, "--skip-tags="+skipTags)
	}

	args = append(args, templateArgs...)
	args = append(args, taskArgs...)
	args = append(args, playbookName)

	if line, ok := inputMap[db.AccessKeyRoleAnsibleUser]; ok {
		inputs["SSH password:"] = line
	}

	if line, ok := inputMap[db.AccessKeyRoleAnsibleBecomeUser]; ok {
		inputs["BECOME password"] = line
	}

	if line, ok := inputMap[db.AccessKeyRoleAnsibleBecomeUser]; ok {
		inputs["SUDO password"] = line
	}

	return
}

func (t *LocalExecutor) getCLIArgs() (templateArgs []string, taskArgs []string, err error) {

	if t.Template.Arguments != nil {
		err = json.Unmarshal([]byte(*t.Template.Arguments), &templateArgs)
		if err != nil {
			err = fmt.Errorf("invalid format of the template extra arguments, must be valid JSON")
			return
		}
	}

	if t.Template.AllowOverrideArgsInTask && t.Task.Arguments != nil {
		err = json.Unmarshal([]byte(*t.Task.Arguments), &taskArgs)
		if err != nil {
			err = fmt.Errorf("invalid format of the TaskRunner extra arguments, must be valid JSON")
			return
		}
	}

	return
}

// convertArgsJSONIfArray converts array format JSON to map format with "default" key and returns the parsed result
func convertArgsJSONIfArray(argsJSON string) (map[string][]string, error) {
	if argsJSON == "" {
		return nil, nil
	}

	// Try to parse as array first
	var arr []string
	if err := json.Unmarshal([]byte(argsJSON), &arr); err == nil {
		// It's an array, convert to map format
		mapArgs := map[string][]string{
			"default": arr,
		}
		return mapArgs, nil
	}

	// If not an array, verify it's a valid map format
	var mapArgs map[string][]string
	if err := json.Unmarshal([]byte(argsJSON), &mapArgs); err != nil {
		return nil, fmt.Errorf("invalid format of arguments, must be valid JSON array or map: %v", err)
	}

	return mapArgs, nil
}

// getCLIArgsMap returns args that support both array and map formats
// Array format is automatically converted to map with "default" key for backward compatibility
// Returns: templateArgsMap (map), taskArgsMap (map), err
func (t *LocalExecutor) getCLIArgsMap() (templateArgsMap map[string][]string, taskArgsMap map[string][]string, err error) {

	// Convert template arguments if needed
	if t.Template.Arguments != nil {
		templateArgsMap, err = convertArgsJSONIfArray(*t.Template.Arguments)
		if err != nil {
			return nil, nil, err
		}
	}

	// Convert task arguments if needed
	if t.Template.AllowOverrideArgsInTask && t.Task.Arguments != nil {
		taskArgsMap, err = convertArgsJSONIfArray(*t.Task.Arguments)
		if err != nil {
			return nil, nil, err
		}
	}

	return
}

func (t *LocalExecutor) getTemplateParams() (any, error) {
	var params any
	switch t.Template.App {
	case db.AppAnsible:
		params = &db.AnsibleTemplateParams{}
	case db.AppTerraform, db.AppTofu, db.AppTerragrunt:
		params = &db.TerraformTemplateParams{}
	default:
		return nil, nil
	}

	err := t.Template.FillParams(params)
	return params, err
}

func (t *LocalExecutor) getParams() (params any, err error) {
	switch t.Template.App {
	case db.AppAnsible:
		params = &db.AnsibleTaskParams{}
	case db.AppTerraform, db.AppTofu, db.AppTerragrunt:
		params = &db.TerraformTaskParams{}
	default:
		params = &db.DefaultTaskParams{}
	}

	err = t.Task.ExtractParams(params)

	if err != nil {
		return
	}

	return
}

// Run executes the task: prepares the working environment, dispatches it to the underlying
// app (Ansible / Terraform / shell), and tears everything down. It is the entry point the
// job pool uses (satisfying the Job interface); the lifecycle methods Prepare/Cleanup are
// available for callers that want to drive the phases explicitly.
func (t *LocalExecutor) Run(username string, incomingVersion *string, alias string) (err error) {
	defer t.Cleanup()

	if err = t.Prepare(username, incomingVersion, alias); err != nil {
		return
	}

	if t.killed {
		t.SetStatus(task_logger.TaskStoppedStatus)
		return nil
	}

	return t.App.Run(db_lib.LocalAppRunningArgs{
		CliArgs:         t.preparedArgsMap,
		EnvironmentVars: t.preparedEnv,
		Inputs:          t.preparedInputs,
		TaskParams:      t.preparedParams,
		TemplateParams:  t.preparedTplParams,
		Callback: func(p *os.Process) {
			t.Process = p
		},
	})
}

// Prepare resolves everything the executor needs to start the underlying app: environment
// variables, task / template parameters, repository checkout, inventory file, access key
// and vault password installation, and the final CLI args / extra env. The result is
// stashed on the receiver so a subsequent Run() (or, in the future, a non-local executor)
// can consume it without recomputing.
//
// Prepare is idempotent: a second call is a no-op once the first one has succeeded. It
// also performs the SetStatus(running) transition the legacy Run() did inline, since
// callers driving the lifecycle manually still expect that signal to fire here.
func (t *LocalExecutor) Prepare(username string, incomingVersion *string, alias string) (err error) {
	if t.prepared {
		return nil
	}

	// Defense in depth behind the dispatcher's own check: the resolve flag is
	// only understood by the Ansible branch below, and any other app would
	// silently ignore it and run the template for real.
	if t.resolveInventoryOnly() && t.Template.App != db.AppAnsible {
		err = fmt.Errorf("inventory can only be resolved with an ansible template, not %q", t.Template.App)
		t.Log(err.Error())
		return
	}

	if t.resolveInventoryOnly() {
		if err = t.checkResolvableVaults(); err != nil {
			t.Log(err.Error())
			return
		}
	}

	t.SetStatus(task_logger.TaskRunningStatus) // It is required for local mode. Don't delete

	// Defense in depth: reject playbook paths pointing outside the repository
	// even if they were stored before validation was added.
	if err = db.ValidatePlaybookPath(t.Template.Playbook, "template"); err != nil {
		t.Log(err.Error())
		return
	}
	if err = db.ValidatePlaybookPath(t.Task.Playbook, "task"); err != nil {
		t.Log(err.Error())
		return
	}

	environmentVariables, err := t.getEnvironmentENV()
	if err != nil {
		return
	}

	surveyEnvVars, err := t.getSurveyEnvVars()
	if err != nil {
		return
	}
	environmentVariables = append(environmentVariables, surveyEnvVars...)

	tplParams, err := t.getTemplateParams()
	if err != nil {
		return
	}

	params, err := t.getParams()
	if err != nil {
		return
	}

	if t.Template.App.IsTerraform() && alias != "" {
		environmentVariables = append(environmentVariables, "TF_HTTP_ADDRESS="+util.GetPublicAliasURL("terraform", alias))
	}

	// For Terraform apps, get args first so we can pass init args to prepareRun
	var argsMap map[string][]string
	var inputs map[string]string

	if t.Template.App.IsTerraform() {
		argsMap, err = t.getTerraformArgs(username, incomingVersion)
		if err != nil {
			return
		}
		// Use Terraform-specific prepareRun with init args
		if tfApp, ok := t.App.(*db_lib.TerraformApp); ok {
			initArgs := []string(nil)
			if argsMap != nil {
				if stageArgs, ok := argsMap["init"]; ok {
					initArgs = stageArgs
				}
			}

			err = t.prepareRunTerraform(tfApp, db_lib.LocalAppInstallingArgs{
				EnvironmentVars: environmentVariables,
				TplParams:       tplParams,
				Params:          params,
				Installer:       t.KeyInstaller,
			}, initArgs)
			if err != nil {
				return err
			}
		} else {
			err = t.prepareRun(db_lib.LocalAppInstallingArgs{
				EnvironmentVars: environmentVariables,
				TplParams:       tplParams,
				Params:          params,
				Installer:       t.KeyInstaller,
			})
			if err != nil {
				return err
			}
		}
	} else {
		err = t.prepareRun(db_lib.LocalAppInstallingArgs{
			EnvironmentVars: environmentVariables,
			TplParams:       tplParams,
			Params:          params,
			Installer:       t.KeyInstaller,
		})
		if err != nil {
			return err
		}
	}

	// Get args for non-Terraform apps
	var args []string
	switch t.Template.App {
	case db.AppAnsible:
		if t.resolveInventoryOnly() {
			args, err = t.getInventoryArgs()
		} else {
			args, inputs, err = t.getPlaybookArgs(username, incomingVersion)
		}

		if err != nil {
			return
		}
		// Convert to map format with "default" key
		argsMap = map[string][]string{"default": args}
	case db.AppTerraform, db.AppTofu, db.AppTerragrunt:
		// Already got args earlier for Terraform
	default:
		args, err = t.getShellArgs(username, incomingVersion)
		if err != nil {
			return
		}
		// Convert to map format with "default" key
		argsMap = map[string][]string{"default": args}
	}

	// Get extra environment vars for non-Terraform apps
	switch t.Template.App {
	case db.AppAnsible:
		// Semaphore vars / task details were already passed
		// as 'extra vars' in JSON format
		break
	case db.AppTerraform, db.AppTofu, db.AppTerragrunt:
		break
	default:
		environmentVariables = append(environmentVariables, t.getShellEnvironmentExtraENV(username, incomingVersion)...)
	}

	if sshEnv := t.getSSHAgentEnv(); sshEnv != "" {
		environmentVariables = append(environmentVariables, sshEnv)
	}

	if t.Template.Type != db.TemplateTask {

		environmentVariables = append(environmentVariables, fmt.Sprintf("SEMAPHORE_TASK_TYPE=%s", t.Template.Type))

		if incomingVersion != nil {
			environmentVariables = append(
				environmentVariables,
				fmt.Sprintf("SEMAPHORE_TASK_INCOMING_VERSION=%s", *incomingVersion))
		}

		if t.Template.Type == db.TemplateBuild && t.Task.Version != nil {
			environmentVariables = append(
				environmentVariables,
				fmt.Sprintf("SEMAPHORE_TASK_TARGET_VERSION=%s", *t.Task.Version))
		}
	}

	t.preparedEnv = environmentVariables
	t.preparedArgsMap = argsMap
	t.preparedInputs = inputs
	t.preparedParams = params
	t.preparedTplParams = tplParams
	t.prepared = true

	return nil
}

// Cleanup releases every resource Prepare allocated: installed access keys, written
// inventory files, app-level working dirs. Safe to invoke on a partially-prepared
// executor — the underlying destroyers tolerate missing state.
func (t *LocalExecutor) Cleanup() {
	t.destroyKeys()
	t.destroyInventoryFile()
	if t.App != nil {
		t.App.Clear()
	}
}

// resolveGitBranch computes the effective git branch for a run, applying the
// template-level and task-level overrides on top of the repository's configured
// branch. The task-supplied branch is honored only when the template explicitly
// permits it via AllowOverrideBranchInTask; otherwise it is ignored. This gate is
// what prevents a user who may only run tasks (Task Runner) from redirecting a
// branch-pinned template to an arbitrary branch of the repository.
func resolveGitBranch(repoBranch string, template db.Template, task db.Task) string {
	branch := repoBranch

	// Override git branch from template if set.
	if template.GitBranch != nil && *template.GitBranch != "" {
		branch = *template.GitBranch
	}

	// Override git branch from task only if the template allows it.
	if template.AllowOverrideBranchInTask && task.GitBranch != nil && *task.GitBranch != "" {
		branch = *task.GitBranch
	}

	return branch
}

func (t *LocalExecutor) prepareRun(installingArgs db_lib.LocalAppInstallingArgs) error {

	t.Log("Preparing: " + strconv.Itoa(t.Task.ID))

	if err := checkTmpDir(util.Config.GetProjectTmpDir(t.Template.ProjectID)); err != nil {
		t.Log("Creating tmp dir failed: " + err.Error())
		return err
	}

	if util.Config.HomeDirMode != util.HomeDirModeProjectHome {
		if err := checkTmpDir(t.Repository.GetHomePath(t.Template.ID)); err != nil {
			t.Log("Creating task home dir failed: " + err.Error())
			return err
		}
		if err := util.ChownDir(t.Repository.GetHomePath(t.Template.ID)); err != nil {
			t.Log("Chowning task home dir failed: " + err.Error())
			return err
		}
	}

	t.Repository.GitBranch = resolveGitBranch(t.Repository.GitBranch, t.Template, t.Task)

	if t.Repository.GetType() == db.RepositoryLocal {
		localPath := t.Repository.GetGitURL(true)
		if _, err := os.Stat(localPath); err != nil {
			t.Log("Failed in finding static repository: " + err.Error())
			return err
		}
	} else {
		if err := t.updateAndCheckoutRepository(); err != nil {
			return err
		}
	}

	if err := t.installInventory(); err != nil {
		t.Log("Failed to install inventory: " + err.Error())
		return err
	}

	// The ssh agent socket is deliberately kept out of the installation
	// environment: ansible-galaxy executes the roles and collections named by
	// the repository, which would give repository content a signing oracle for
	// the key Semaphore uses to reach the managed hosts. The socket belongs to
	// the task run only, where it is added in Prepare.
	if err := t.App.InstallRequirements(installingArgs); err != nil {
		t.Log("Failed to install requirements: " + err.Error())
		return err
	}

	if err := t.installVaultKeyFiles(); err != nil {
		t.Log("Failed to install vault password files: " + err.Error())
		return err
	}

	return nil
}

// terraformInstaller is the part of db_lib.TerraformApp the terraform
// preparation uses. Taken as an interface so the preparation can be exercised
// without a terraform binary on the host.
type terraformInstaller interface {
	InstallRequirementsWithInitArgs(args db_lib.LocalAppInstallingArgs, initArgs []string) error
}

func (t *LocalExecutor) prepareRunTerraform(tfApp terraformInstaller, installingArgs db_lib.LocalAppInstallingArgs, initArgs []string) error {

	t.Log("Preparing: " + strconv.Itoa(t.Task.ID))

	if err := checkTmpDir(util.Config.GetProjectTmpDir(t.Template.ProjectID)); err != nil {
		t.Log("Creating tmp dir failed: " + err.Error())
		return err
	}

	if util.Config.HomeDirMode != util.HomeDirModeProjectHome {
		if err := checkTmpDir(t.Repository.GetHomePath(t.Template.ID)); err != nil {
			t.Log("Creating task home dir failed: " + err.Error())
			return err
		}
		if err := util.ChownDir(t.Repository.GetHomePath(t.Template.ID)); err != nil {
			t.Log("Chowning task home dir failed: " + err.Error())
			return err
		}
	}

	t.Repository.GitBranch = resolveGitBranch(t.Repository.GitBranch, t.Template, t.Task)

	if t.Repository.GetType() == db.RepositoryLocal {
		localPath := t.Repository.GetGitURL(true)
		if _, err := os.Stat(localPath); err != nil {
			t.Log("Failed in finding static repository: " + err.Error())
			return err
		}
	} else {
		if err := t.updateAndCheckoutRepository(); err != nil {
			return err
		}
	}

	if err := t.installInventory(); err != nil {
		t.Log("Failed to install inventory: " + err.Error())
		return err
	}

	// The ssh agent socket is deliberately kept out of the installation
	// environment: terraform init downloads and executes providers named by the
	// repository, which would give repository content a signing oracle for the
	// key Semaphore uses to reach the managed hosts. The socket belongs to the
	// task run only, where it is added in Prepare.
	if err := tfApp.InstallRequirementsWithInitArgs(installingArgs, initArgs); err != nil {
		t.Log("Failed to install requirements: " + err.Error())
		return err
	}

	if err := t.installVaultKeyFiles(); err != nil {
		t.Log("Failed to install vault password files: " + err.Error())
		return err
	}

	return nil
}

// updateAndCheckoutRepository runs the pull/clone + checkout sequence as one
// critical section per repository directory, so parallel tasks of the same
// template cannot run concurrent git operations on the shared working copy.
//
// ponytail: the lock covers git operations only; parallel tasks pinned to
// different commits still share the working tree afterwards — per-task
// worktrees if that ever matters.
func (t *LocalExecutor) updateAndCheckoutRepository() error {
	unlock := t.RepoLock.Lock(t.Repository.GetFullPath(t.Template.ID))
	defer unlock()

	if err := t.updateRepository(); err != nil {
		t.Log("Failed updating repository: " + err.Error())
		return err
	}

	if err := t.checkoutRepository(); err != nil {
		t.Log("Failed to checkout repository to required commit: " + err.Error())
		return err
	}

	return nil
}

func (t *LocalExecutor) updateRepository() error {
	repo := db_lib.GitRepository{
		Logger:     t.Logger,
		TemplateID: t.Template.ID,
		Repository: t.Repository,
		Client:     db_lib.CreateDefaultGitClient(t.KeyInstaller),
	}

	err := repo.ValidateRepo()

	if err != nil {
		if !os.IsNotExist(err) {
			err = os.RemoveAll(repo.GetFullPath())
			if err != nil {
				return err
			}
		}
		return repo.Clone()
	}

	if repo.CanBePulled() {
		err = repo.Pull()
		if err == nil {
			return nil
		}
	}

	err = os.RemoveAll(repo.GetFullPath())
	if err != nil {
		return err
	}

	return repo.Clone()
}

func (t *LocalExecutor) checkoutRepository() error {

	repo := db_lib.GitRepository{
		Logger:     t.Logger,
		TemplateID: t.Template.ID,
		Repository: t.Repository,
		Client:     db_lib.CreateDefaultGitClient(t.KeyInstaller),
	}

	err := repo.ValidateRepo()

	if err != nil {
		return err
	}

	if t.Task.CommitHash != nil {
		// checkout to commit if it is provided for TaskRunner
		return repo.Checkout(*t.Task.CommitHash)
	}

	// store commit to TaskRunner table

	commitHash, err := repo.GetLastCommitHash()

	if err != nil {
		return err
	}

	commitMessage, err := repo.GetLastCommitMessage()

	if err != nil {
		t.Log(err.Error())
	}

	t.SetCommit(commitHash, commitMessage)

	return nil
}

func (t *LocalExecutor) installVaultKeyFiles() (err error) {
	t.vaultFileInstallations = make(map[string]ssh.AccessKeyInstallation)

	if len(t.Template.Vaults) == 0 {
		return nil
	}

	for _, vault := range t.Template.Vaults {
		var name string
		if vault.Name != nil {
			name = *vault.Name
		} else {
			name = "default"
		}

		var install ssh.AccessKeyInstallation
		if vault.Type == db.TemplateVaultPassword {
			install, err = t.KeyInstaller.Install(*vault.Vault, db.AccessKeyRoleAnsiblePasswordVault, t.Logger)
			if err != nil {
				return
			}
		}
		if vault.Type == db.TemplateVaultScript && vault.Script != nil {
			install.Script = *vault.Script
		}

		t.vaultFileInstallations[name] = install
	}

	return
}

func (t *LocalExecutor) getSSHAgentEnv() string {
	if t.Inventory.SSHKeyID == nil || t.Inventory.SSHKey.Type != db.AccessKeySSH {
		return ""
	}

	if t.sshKeyInstallation.SSHAgent == nil {
		// Defensive: KeyInstaller.Install always fills SSHAgent for
		// AccessKeySSH, and a failing installInventory aborts preparation
		// before this point runs, so the normal path never reaches here.
		// Kept as a guard and logged in case a future installation path
		// stops setting it. Without the socket ansible falls back to
		// whatever credentials the process happens to have and fails on the
		// managed hosts, with a message that does not point back here.
		log.WithFields(log.Fields{
			"context":      "task_running",
			"task_id":      t.Task.ID,
			"inventory_id": t.Inventory.ID,
		}).Warn("inventory ssh key has no running agent")

		return ""
	}

	return fmt.Sprintf("SSH_AUTH_SOCK=%s", t.sshKeyInstallation.SSHAgent.SocketFile)
}
