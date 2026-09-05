package tasks

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/semaphoreui/semaphore/pkg/ssh"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupExecutorConfig(t *testing.T) {
	t.Helper()
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })
	if util.Config == nil {
		util.Config = &util.ConfigType{}
	}
}

// TestGetShellArgs_PassesSurveySecretVar verifies that Survey variables of type
// "Secret" (delivered via LocalExecutor.Secret) are passed to Bash/Shell tasks,
// alongside plain survey vars that arrive in Environment.JSON.
func TestGetShellArgs_PassesSurveySecretVar(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Template: db.Template{
			Type:     db.TemplateTask,
			Playbook: "date.sh",
		},
		Environment: db.Environment{
			JSON: `{"PLAIN_VAR":"hello"}`,
		},
		Secret: `{"MY_VAR":"s3cr3t"}`,
	}

	args, err := exec.getShellArgs("admin", nil)
	require.NoError(t, err)

	assert.Contains(t, args, "PLAIN_VAR=hello", "plain survey var must be passed")
	assert.Contains(t, args, "MY_VAR=s3cr3t", "secret survey var must be passed")
}

// TestGetEnvironmentExtraVars_MergesSecret checks the shared helper merges the
// Secret field into the extra vars map used by Shell and Terraform tasks.
func TestGetEnvironmentExtraVars_MergesSecret(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Environment: db.Environment{JSON: `{"PLAIN_VAR":"hello"}`},
		Secret:      `{"MY_VAR":"s3cr3t"}`,
	}

	extraVars, err := exec.getEnvironmentExtraVars("admin", nil)
	require.NoError(t, err)

	assert.Equal(t, "hello", extraVars["PLAIN_VAR"])
	assert.Equal(t, "s3cr3t", extraVars["MY_VAR"])
}

// TestGetEnvironmentExtraVars_EmptySecret pins that "" and "{}" are equivalent
// "no survey secrets" values: "" comes from DB-loaded tasks and API clients
// omitting the field, "{}" from the UI and the AddTask sanitization.
func TestGetEnvironmentExtraVars_EmptySecret(t *testing.T) {
	setupExecutorConfig(t)

	for _, secret := range []string{"", "{}"} {
		t.Run("secret "+secret, func(t *testing.T) {
			exec := &LocalExecutor{
				Environment: db.Environment{JSON: `{"PLAIN_VAR":"hello"}`},
				Secret:      secret,
			}

			extraVars, err := exec.getEnvironmentExtraVars("admin", nil)
			require.NoError(t, err)

			assert.Equal(t, "hello", extraVars["PLAIN_VAR"])
			assert.Len(t, extraVars, 2) // PLAIN_VAR + semaphore_vars only
		})
	}
}

// TestGetEnvironmentExtraVars_SkipsEnvTargetVars verifies that survey vars with
// Target "env" are excluded from the extra-vars map (they must not reach
// --extra-vars / -var / shell CLI args).
func TestGetEnvironmentExtraVars_SkipsEnvTargetVars(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Template: db.Template{
			SurveyVars: []db.SurveyVar{
				{Name: "ENV_VAR", Target: db.SurveyVarTargetEnv},
				{Name: "CLI_VAR"},
			},
		},
		Environment: db.Environment{JSON: `{"ENV_VAR":"via-env","CLI_VAR":"via-cli"}`},
		Secret:      `{"SECRET_ENV_VAR":"s3cr3t"}`,
	}
	exec.Template.SurveyVars = append(exec.Template.SurveyVars,
		db.SurveyVar{Name: "SECRET_ENV_VAR", Type: "secret", Target: db.SurveyVarTargetEnv})

	extraVars, err := exec.getEnvironmentExtraVars("admin", nil)
	require.NoError(t, err)

	assert.NotContains(t, extraVars, "ENV_VAR")
	assert.NotContains(t, extraVars, "SECRET_ENV_VAR")
	assert.Equal(t, "via-cli", extraVars["CLI_VAR"])
}

// TestGetSurveyEnvVars verifies env-target survey vars are collected as
// NAME=value pairs from both Environment.JSON and the Secret field, and that
// CLI-target vars are ignored.
func TestGetSurveyEnvVars(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Template: db.Template{
			SurveyVars: []db.SurveyVar{
				{Name: "ENV_VAR", Target: db.SurveyVarTargetEnv},
				{Name: "SECRET_ENV_VAR", Type: "secret", Target: db.SurveyVarTargetEnv},
				{Name: "CLI_VAR"},
				{Name: "MISSING_VAR", Target: db.SurveyVarTargetEnv}, // no value provided
			},
		},
		Environment: db.Environment{JSON: `{"ENV_VAR":"via-env","CLI_VAR":"via-cli"}`},
		Secret:      `{"SECRET_ENV_VAR":"s3cr3t"}`,
	}

	envVars, err := exec.getSurveyEnvVars()
	require.NoError(t, err)

	assert.Contains(t, envVars, "ENV_VAR=via-env")
	assert.Contains(t, envVars, "SECRET_ENV_VAR=s3cr3t")
	assert.Len(t, envVars, 2, "CLI-target and valueless vars must not be included")
}

// TestFormatVarValue verifies values from multi-select survey vars (arrays)
// are JSON-encoded instead of degrading to Go's fmt representation.
func TestFormatVarValue(t *testing.T) {
	tests := []struct {
		name     string
		input    any
		expected string
	}{
		{"string", "hello", "hello"},
		{"array", []any{"1", "2"}, `["1","2"]`},
		{"empty array", []any{}, `[]`},
		{"object", map[string]any{"k": "v"}, `{"k":"v"}`},
		{"number", float64(42), "42"},
		{"bool", true, "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, formatVarValue(tt.input))
		})
	}
}

// TestGetSurveyEnvVars_MultiSelect verifies env-target multi-select survey
// vars are delivered as JSON arrays, not Go-formatted slices like "[1 2]".
func TestGetSurveyEnvVars_MultiSelect(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Template: db.Template{
			SurveyVars: []db.SurveyVar{
				{Name: "MULTI_VAR", Type: db.SurveyVarSelect, Target: db.SurveyVarTargetEnv},
			},
		},
		Environment: db.Environment{JSON: `{"MULTI_VAR":["1","2"]}`},
	}

	envVars, err := exec.getSurveyEnvVars()
	require.NoError(t, err)

	assert.Contains(t, envVars, `MULTI_VAR=["1","2"]`)
}

// TestGetShellArgs_MultiSelect verifies multi-select survey vars reach shell
// tasks as JSON arrays in KEY=value CLI args, not Go's "[1 2]" formatting.
func TestGetShellArgs_MultiSelect(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Template: db.Template{
			Type:     db.TemplateTask,
			Playbook: "run.sh",
		},
		Environment: db.Environment{JSON: `{"multi_var":["1","2"]}`},
	}

	args, err := exec.getShellArgs("admin", nil)
	require.NoError(t, err)

	assert.Contains(t, args, `multi_var=["1","2"]`)
	assert.NotContains(t, args, "multi_var=[1 2]")
}

// TestGetTerraformArgs_MultiSelect verifies multi-select survey vars reach
// terraform as -var name=<JSON list>, which terraform parses as a list value.
func TestGetTerraformArgs_MultiSelect(t *testing.T) {
	setupExecutorConfig(t)

	exec := &LocalExecutor{
		Template: db.Template{
			Type: db.TemplateTask,
			App:  db.AppTerraform,
		},
		Environment: db.Environment{JSON: `{"multi_var":["1","2"]}`},
	}

	argsMap, err := exec.getTerraformArgs("admin", nil)
	require.NoError(t, err)

	defaultArgs := argsMap["default"]
	found := false
	for i, arg := range defaultArgs {
		if arg == "-var" && i+1 < len(defaultArgs) && defaultArgs[i+1] == `multi_var=["1","2"]` {
			found = true
		}
	}
	assert.True(t, found, "expected -var multi_var=[\"1\",\"2\"] in %v", defaultArgs)
}

// Defense in depth behind the dispatcher's own app check: the resolve flag is
// only understood by the Ansible branch of Prepare, and any other app would
// silently ignore it and run the template for real.
func TestPrepareRefusesResolveOnNonAnsibleTemplate(t *testing.T) {
	executor := &LocalExecutor{
		Task: db.Task{
			Params: db.MapStringAnyField{"resolve_inventory_only": true},
		},
		Template: db.Template{App: db.AppTerraform},
		Logger:   task_logger.NopLogger{},
	}

	err := executor.Prepare("tester", nil, "")

	assert.ErrorContains(t, err, "ansible")
}

// newInventoryListingExecutor builds the executor a resolve task ends up with:
// an ansible template pointed at a dynamic inventory file.
func newInventoryListingExecutor(vaults map[string]ssh.AccessKeyInstallation) *LocalExecutor {
	return &LocalExecutor{
		Task: db.Task{
			Params: db.MapStringAnyField{db.TaskParamResolveInventoryOnly: true},
		},
		Template:               db.Template{App: db.AppAnsible},
		Inventory:              db.Inventory{Type: db.InventoryFile, Inventory: "dynamic.py"},
		Logger:                 task_logger.NopLogger{},
		vaultFileInstallations: vaults,
	}
}

// An inventory file, or the group_vars beside it, may be encrypted with
// ansible-vault. getPlaybookArgs passes the vault ids; a listing has to pass
// them too, or ansible-inventory fails with a raw "Attempting to decrypt but no
// vault secrets found".
func TestGetInventoryArgsPassesVaultScripts(t *testing.T) {
	executor := newInventoryListingExecutor(map[string]ssh.AccessKeyInstallation{
		"default": {Script: "/tmp/vault-default.sh"},
		"other":   {Script: "/tmp/vault-other.sh"},
	})

	args, err := executor.getInventoryArgs()
	require.NoError(t, err)

	assert.Equal(t, []string{
		"-i", "dynamic.py", "--list",
		"--vault-id=default@/tmp/vault-default.sh",
		"--vault-id=other@/tmp/vault-other.sh",
	}, args, "vault ids must be passed, in a stable order")
}

func TestGetInventoryArgsWithoutVaults(t *testing.T) {
	executor := newInventoryListingExecutor(nil)

	args, err := executor.getInventoryArgs()
	require.NoError(t, err)

	assert.Equal(t, []string{"-i", "dynamic.py", "--list"}, args)
}

// A stored vault password is answered by typing into a pty, and
// RunInventoryListing has none: passing --vault-id=name@prompt would hang
// ansible on a prompt nobody can answer. Refuse instead.
func TestGetInventoryArgsRefusesPasswordVault(t *testing.T) {
	executor := newInventoryListingExecutor(map[string]ssh.AccessKeyInstallation{
		"default": {Password: "s3cr3t"},
	})

	_, err := executor.getInventoryArgs()

	require.Error(t, err)
	assert.ErrorContains(t, err, "password")
	assert.NotContains(t, err.Error(), "s3cr3t", "the refusal must not quote the password")
}

// The same refusal, but where it belongs: before the repository is checked out
// and the keys are installed, so the message is what a person gets instead of
// a hung ansible.
func TestCheckResolvableVaults(t *testing.T) {
	vaultKeyID := 1
	script := "/tmp/vault.sh"

	tests := []struct {
		name    string
		vaults  []db.TemplateVault
		refused bool
	}{
		{"no vaults", nil, false},
		{
			"a vault script needs no terminal",
			[]db.TemplateVault{{Type: db.TemplateVaultScript, Script: &script}},
			false,
		},
		{
			"a stored password has nowhere to be typed",
			[]db.TemplateVault{{Type: db.TemplateVaultPassword, VaultKeyID: &vaultKeyID}},
			true,
		},
		{
			"one password among scripts is enough to refuse",
			[]db.TemplateVault{
				{Type: db.TemplateVaultScript, Script: &script},
				{Type: db.TemplateVaultPassword, VaultKeyID: &vaultKeyID},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newInventoryListingExecutor(nil)
			executor.Template.Vaults = tt.vaults

			err := executor.checkResolvableVaults()

			if !tt.refused {
				assert.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.ErrorContains(t, err, "password")
			assert.ErrorContains(t, err, "script")
		})
	}
}

// Prepare is where the check has to sit, so the refusal happens before the
// repository is checked out rather than when the arguments are built.
func TestPrepareRefusesResolveWithPasswordVault(t *testing.T) {
	vaultKeyID := 1
	executor := newInventoryListingExecutor(nil)
	executor.Template.Vaults = []db.TemplateVault{
		{Type: db.TemplateVaultPassword, VaultKeyID: &vaultKeyID},
	}

	err := executor.Prepare("tester", nil, "")

	require.Error(t, err)
	assert.ErrorContains(t, err, "password")
}

// recordingLogger captures the lines a task writes to its log. The embedded nil
// interface panics on any method a test did not expect to be called, which keeps
// the double honest without reimplementing the whole Logger surface.
type recordingLogger struct {
	task_logger.Logger
	lines []string
}

func (l *recordingLogger) Log(msg string) {
	l.lines = append(l.lines, msg)
}

func TestEffectiveAnsibleLimit(t *testing.T) {
	tests := []struct {
		name      string
		tplParams db.AnsibleTemplateParams
		params    db.AnsibleTaskParams
		expected  string
	}{
		{
			name:      "nothing set anywhere",
			tplParams: db.AnsibleTemplateParams{},
			params:    db.AnsibleTaskParams{},
			expected:  "",
		},
		{
			name:      "template value with no override allowed",
			tplParams: db.AnsibleTemplateParams{Limit: []string{"webservers"}},
			params:    db.AnsibleTaskParams{Limit: []string{"db01"}},
			expected:  "webservers",
		},
		{
			name: "task overrides the template when allowed",
			tplParams: db.AnsibleTemplateParams{
				AllowOverrideLimit: true,
				Limit:              []string{"webservers"},
			},
			params:   db.AnsibleTaskParams{Limit: []string{"web01", "web02"}},
			expected: "web01,web02",
		},
		{
			name:      "override allowed but the task picked nothing",
			tplParams: db.AnsibleTemplateParams{AllowOverrideLimit: true, Limit: []string{"webservers"}},
			params:    db.AnsibleTaskParams{},
			expected:  "webservers",
		},
		{
			// Distinct from the case above: an empty selection is not nil, so it
			// takes the override branch and clears the template's limit. Clearing
			// every host in the form therefore means the whole inventory, not
			// nothing — both through --limit and through default() in a playbook
			// that selects hosts by variable.
			name:      "override allowed and the task cleared the selection",
			tplParams: db.AnsibleTemplateParams{AllowOverrideLimit: true, Limit: []string{"webservers"}},
			params:    db.AnsibleTaskParams{Limit: []string{}},
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, effectiveAnsibleLimit(tt.tplParams, tt.params))
		})
	}
}

func TestGetEnvironmentExtraVarsJSONCarriesTheLimit(t *testing.T) {
	// Not for the feature — getTaskDetails builds the task URL out of the global
	// config — but a fresh one, so that a TargetHostVar left there by another
	// test cannot decide the outcome of these.
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })
	util.Config = &util.ConfigType{}

	decode := func(t *testing.T, raw string) map[string]any {
		t.Helper()
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
		return decoded
	}

	t.Run("canonical variable always carries the limit", func(t *testing.T) {
		exec := &LocalExecutor{}

		raw, err := exec.getEnvironmentExtraVarsJSON("admin", nil, "web01,web02")
		require.NoError(t, err)

		decoded := decode(t, raw)
		semaphoreVars, ok := decoded["semaphore_vars"].(map[string]any)
		require.True(t, ok, "semaphore_vars must be an object")
		assert.Equal(t, "web01,web02", semaphoreVars["limit"])
	})

	// The name is read off the task, where the server stamped it, and not off
	// util.Config: this code also runs inside a remote runner, whose own
	// configuration file knows nothing about the setting.
	t.Run("alias named by the task carries the same value", func(t *testing.T) {
		exec := &LocalExecutor{Task: db.Task{TargetHostVar: "involved_hosts"}}

		raw, err := exec.getEnvironmentExtraVarsJSON("admin", nil, "web01,web02")
		require.NoError(t, err)

		decoded := decode(t, raw)
		assert.Equal(t, "web01,web02", decoded["involved_hosts"])

		semaphoreVars, ok := decoded["semaphore_vars"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "web01,web02", semaphoreVars["limit"],
			"the alias must not replace the canonical variable")
	})

	t.Run("the configured name is ignored when the task does not carry it", func(t *testing.T) {
		prevCfg := util.Config
		t.Cleanup(func() { util.Config = prevCfg })
		util.Config = &util.ConfigType{TargetHostVar: "involved_hosts"}

		exec := &LocalExecutor{}

		raw, err := exec.getEnvironmentExtraVarsJSON("admin", nil, "web01,web02")
		require.NoError(t, err)

		assert.NotContains(t, decode(t, raw), "involved_hosts",
			"the executor must not fall back to its own process configuration")
	})

	// The whole point of leaving the variable undefined: Jinja's default()
	// replaces an undefined value and not an empty one, so an empty string here
	// would turn hosts: "{{ picked | default('all') }}" into a play matching no
	// host at all.
	t.Run("an empty limit defines neither variable", func(t *testing.T) {
		exec := &LocalExecutor{Task: db.Task{TargetHostVar: "involved_hosts"}}

		raw, err := exec.getEnvironmentExtraVarsJSON("admin", nil, "")
		require.NoError(t, err)

		decoded := decode(t, raw)
		assert.NotContains(t, decoded, "involved_hosts")

		semaphoreVars, ok := decoded["semaphore_vars"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, semaphoreVars, "limit")
	})

	// Without the unconditional reservation the caller could set the variable
	// itself through task.environment and pick the hosts a playbook targets —
	// precisely when the template forbids overriding the limit, since a forbidden
	// override with no template limit is what produces an empty one here.
	t.Run("an empty limit still clears a value supplied under the alias", func(t *testing.T) {
		logger := &recordingLogger{}
		exec := &LocalExecutor{
			Task:        db.Task{TargetHostVar: "involved_hosts"},
			Environment: db.Environment{JSON: `{"involved_hosts":"prod-db01","unrelated":"kept"}`},
			Logger:      logger,
		}

		raw, err := exec.getEnvironmentExtraVarsJSON("admin", nil, "")
		require.NoError(t, err)

		decoded := decode(t, raw)
		assert.NotContains(t, decoded, "involved_hosts")
		assert.Equal(t, "kept", decoded["unrelated"], "other variables must survive")
		assert.Contains(t, strings.Join(logger.lines, "\n"), "involved_hosts",
			"the dropped variable must be reported in the task log")
	})

	t.Run("a picked limit overrides a value supplied under the alias", func(t *testing.T) {
		logger := &recordingLogger{}
		exec := &LocalExecutor{
			Task:        db.Task{TargetHostVar: "involved_hosts"},
			Environment: db.Environment{JSON: `{"involved_hosts":"prod-db01"}`},
			Logger:      logger,
		}

		raw, err := exec.getEnvironmentExtraVarsJSON("admin", nil, "web01")
		require.NoError(t, err)

		assert.Equal(t, "web01", decode(t, raw)["involved_hosts"])
		assert.NotEmpty(t, logger.lines, "the overridden variable must be reported")
	})
}

// The refactoring that introduced effectiveAnsibleLimit exists to keep --limit
// and the published variable derived from one value. This pins that they agree
// all the way through the real argument builder, which is where they would drift
// apart again.
func TestGetPlaybookArgsPublishesTheLimitItRestrictsTo(t *testing.T) {
	setupExecutorConfig(t)
	util.Config.TmpPath = t.TempDir()

	exec := &LocalExecutor{
		Task: db.Task{
			TargetHostVar: "involved_hosts",
			Params:        db.MapStringAnyField{"limit": []any{"web01", "web02"}},
		},
		Template: db.Template{
			Playbook:   "test.yml",
			TaskParams: db.MapStringAnyField{"allow_override_limit": true},
		},
		Inventory: db.Inventory{Type: db.InventoryStatic},
		Logger:    &recordingLogger{},
	}

	args, _, err := exec.getPlaybookArgs("admin", nil)
	require.NoError(t, err)

	assert.Contains(t, args, "--limit=web01,web02")

	extraVars := extraVarsArg(t, args)
	assert.Equal(t, "web01,web02", extraVars["involved_hosts"])

	semaphoreVars, ok := extraVars["semaphore_vars"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "web01,web02", semaphoreVars["limit"])
}

// A template that forbids the override must publish its own limit, not the one
// the caller asked for — otherwise the variable would hand the caller the host
// selection that --limit denies them.
func TestGetPlaybookArgsIgnoresForbiddenLimitOverride(t *testing.T) {
	setupExecutorConfig(t)
	util.Config.TmpPath = t.TempDir()

	exec := &LocalExecutor{
		Task: db.Task{
			TargetHostVar: "involved_hosts",
			Params:        db.MapStringAnyField{"limit": []any{"prod-db01"}},
		},
		Template: db.Template{
			Playbook:   "test.yml",
			TaskParams: db.MapStringAnyField{"limit": []any{"webservers"}},
		},
		Inventory: db.Inventory{Type: db.InventoryStatic},
		Logger:    &recordingLogger{},
	}

	args, _, err := exec.getPlaybookArgs("admin", nil)
	require.NoError(t, err)

	assert.Contains(t, args, "--limit=webservers")
	assert.Equal(t, "webservers", extraVarsArg(t, args)["involved_hosts"])
}

// extraVarsArg decodes the JSON that follows the --extra-vars flag.
func extraVarsArg(t *testing.T, args []string) map[string]any {
	t.Helper()

	for i, arg := range args {
		if arg != "--extra-vars" {
			continue
		}

		require.Greater(t, len(args), i+1, "--extra-vars must be followed by a value")

		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(args[i+1]), &decoded))
		return decoded
	}

	require.Fail(t, "--extra-vars is missing from the arguments")
	return nil
}

// playbookArgs runs the real argument builder over a template/task parameter
// pair, which is where the override rules are actually applied.
func playbookArgs(t *testing.T, tplParams, taskParams db.MapStringAnyField) []string {
	t.Helper()

	exec := &LocalExecutor{
		Task:      db.Task{Params: taskParams},
		Template:  db.Template{Playbook: "test.yml", TaskParams: tplParams},
		Inventory: db.Inventory{Type: db.InventoryStatic},
		Logger:    &recordingLogger{},
	}

	args, _, err := exec.getPlaybookArgs("admin", nil)
	require.NoError(t, err)
	return args
}

// hasArgPrefix reports whether any argument starts with prefix, so a test can
// state that a flag is absent without guessing its value.
func hasArgPrefix(args []string, prefix string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// The template decides whether a task may replace its tags. Both halves matter:
// a task value must not leak through when the override is off, and the
// template's own value must survive when the task sends none.
func TestGetPlaybookArgsTagOverrides(t *testing.T) {
	setupExecutorConfig(t)
	util.Config.TmpPath = t.TempDir()

	for _, flag := range []struct {
		arg      string
		tplKey   string
		taskKey  string
		allowKey string
	}{
		{"--tags=", "tags", "tags", "allow_override_tags"},
		{"--skip-tags=", "skip_tags", "skip_tags", "allow_override_skip_tags"},
	} {
		t.Run(flag.arg, func(t *testing.T) {
			t.Run("neither side sets any", func(t *testing.T) {
				args := playbookArgs(t, nil, nil)

				assert.False(t, hasArgPrefix(args, flag.arg))
			})

			t.Run("template value is passed", func(t *testing.T) {
				args := playbookArgs(t, db.MapStringAnyField{flag.tplKey: []any{"deploy"}}, nil)

				assert.Contains(t, args, flag.arg+"deploy")
			})

			t.Run("task value is ignored without the override", func(t *testing.T) {
				args := playbookArgs(t,
					db.MapStringAnyField{flag.tplKey: []any{"deploy"}},
					db.MapStringAnyField{flag.taskKey: []any{"hotfix"}})

				assert.Contains(t, args, flag.arg+"deploy")
				assert.NotContains(t, args, flag.arg+"hotfix")
			})

			t.Run("task value replaces the template's with the override", func(t *testing.T) {
				args := playbookArgs(t,
					db.MapStringAnyField{flag.tplKey: []any{"deploy"}, flag.allowKey: true},
					db.MapStringAnyField{flag.taskKey: []any{"hotfix", "urgent"}})

				assert.Contains(t, args, flag.arg+"hotfix,urgent")
			})

			// The override is permission to replace, not an instruction to clear.
			t.Run("template value survives an override the task did not use", func(t *testing.T) {
				args := playbookArgs(t,
					db.MapStringAnyField{flag.tplKey: []any{"deploy"}, flag.allowKey: true},
					nil)

				assert.Contains(t, args, flag.arg+"deploy")
			})
		})
	}
}

// Verbosity is a template-level permission because -vvvv makes Ansible print
// the values it is working with, which includes what secrets expand to.
func TestGetPlaybookArgsDebugLevel(t *testing.T) {
	setupExecutorConfig(t)
	util.Config.TmpPath = t.TempDir()

	t.Run("task debug is ignored without the permission", func(t *testing.T) {
		args := playbookArgs(t, nil, db.MapStringAnyField{"debug": true, "debug_level": 3})

		assert.False(t, hasArgPrefix(args, "-v"))
	})

	t.Run("permission alone adds nothing", func(t *testing.T) {
		args := playbookArgs(t, db.MapStringAnyField{"allow_debug": true}, nil)

		assert.False(t, hasArgPrefix(args, "-v"))
	})

	t.Run("unset level defaults to four", func(t *testing.T) {
		args := playbookArgs(t,
			db.MapStringAnyField{"allow_debug": true},
			db.MapStringAnyField{"debug": true})

		assert.Contains(t, args, "-vvvv")
	})

	t.Run("level is capped at six", func(t *testing.T) {
		args := playbookArgs(t,
			db.MapStringAnyField{"allow_debug": true},
			db.MapStringAnyField{"debug": true, "debug_level": 9})

		assert.Contains(t, args, "-vvvvvv")
	})

	// One is a level Ansible accepts, so it must not be mistaken for "unset".
	t.Run("lowest explicit level is kept", func(t *testing.T) {
		args := playbookArgs(t,
			db.MapStringAnyField{"allow_debug": true},
			db.MapStringAnyField{"debug": true, "debug_level": 1})

		assert.Contains(t, args, "-v")
	})

	t.Run("level is passed through", func(t *testing.T) {
		args := playbookArgs(t,
			db.MapStringAnyField{"allow_debug": true},
			db.MapStringAnyField{"debug": true, "debug_level": 2})

		assert.Contains(t, args, "-vv")
	})
}

// Extra CLI arguments are a template setting; a task may add its own only where
// the template says so, in both the list and the map format.
func TestGetCLIArgsOverride(t *testing.T) {
	executor := func(allowOverride bool, taskArgs *string) *LocalExecutor {
		tplArgs := `["--forks=5"]`
		return &LocalExecutor{
			Template: db.Template{Arguments: &tplArgs, AllowOverrideArgsInTask: allowOverride},
			Task:     db.Task{Arguments: taskArgs},
		}
	}
	taskArgs := `["--forks=50"]`

	t.Run("list format", func(t *testing.T) {
		tpl, task, err := executor(false, &taskArgs).getCLIArgs()
		require.NoError(t, err)
		assert.Equal(t, []string{"--forks=5"}, tpl)
		assert.Empty(t, task)

		tpl, task, err = executor(true, &taskArgs).getCLIArgs()
		require.NoError(t, err)
		assert.Equal(t, []string{"--forks=5"}, tpl)
		assert.Equal(t, []string{"--forks=50"}, task)

		_, task, err = executor(true, nil).getCLIArgs()
		require.NoError(t, err)
		assert.Empty(t, task)
	})

	t.Run("map format", func(t *testing.T) {
		tpl, task, err := executor(false, &taskArgs).getCLIArgsMap()
		require.NoError(t, err)
		assert.Equal(t, map[string][]string{"default": {"--forks=5"}}, tpl)
		assert.Empty(t, task)

		tpl, task, err = executor(true, &taskArgs).getCLIArgsMap()
		require.NoError(t, err)
		assert.Equal(t, map[string][]string{"default": {"--forks=5"}}, tpl)
		assert.Equal(t, map[string][]string{"default": {"--forks=50"}}, task)

		_, task, err = executor(true, nil).getCLIArgsMap()
		require.NoError(t, err)
		assert.Empty(t, task)
	})
}

// A local inventory is a file the node already has; anything else is copied
// into the project's tmp dir under a generated name.
func TestTmpInventoryFullPath(t *testing.T) {
	setupExecutorConfig(t)
	util.Config.TmpPath = t.TempDir()

	exec := func(repo *db.Repository) *LocalExecutor {
		return &LocalExecutor{
			Template:  db.Template{ID: 7, ProjectID: 1},
			Inventory: db.Inventory{ID: 3, Type: db.InventoryStatic, Repository: repo},
		}
	}

	t.Run("no repository", func(t *testing.T) {
		assert.Equal(t,
			filepath.Join(util.Config.GetProjectTmpDir(1), "inventory_3"),
			exec(nil).tmpInventoryFullPath())
	})

	t.Run("local repository is used in place", func(t *testing.T) {
		repo := &db.Repository{GitURL: "/srv/inventories"}

		assert.Equal(t, "/srv/inventories", exec(repo).tmpInventoryFullPath())
	})

	t.Run("remote repository is checked out into tmp", func(t *testing.T) {
		repo := &db.Repository{ID: 5, GitURL: "https://example.com/inv.git", GitBranch: "main"}

		path := exec(repo).tmpInventoryFullPath()

		assert.True(t, strings.HasPrefix(path, util.Config.GetProjectTmpDir(1)), path)
		assert.True(t, strings.HasSuffix(path, "_inventory_3"), path)
	})
}

// TestGetArgs_AnsibleForks verifies passing -f / --forks in template and task arguments,
// including invalid JSON error handling and AllowOverrideArgsInTask behavior.
func TestGetArgs_AnsibleForks(t *testing.T) {
	setupExecutorConfig(t)

	tests := []struct {
		name                  string
		templateArgs          *string
		allowOverride         bool
		taskArgs              *string
		expectedEffectiveFork string
		expectedForksSubArgs  []string
		mustNotContain        []string
		expectError           bool
		expectedErrorMsg      string
	}{
		{
			name:                  "Template arguments with --forks 10",
			templateArgs:          strPtr(`["--forks", "10"]`),
			allowOverride:         false,
			taskArgs:              nil,
			expectedEffectiveFork: "10",
			expectedForksSubArgs:  []string{"--forks", "10"},
		},
		{
			name:                  "Template arguments with -f 10",
			templateArgs:          strPtr(`["-f", "10"]`),
			allowOverride:         false,
			taskArgs:              nil,
			expectedEffectiveFork: "10",
			expectedForksSubArgs:  []string{"-f", "10"},
		},
		{
			name:                  "Task level overrides template forks when AllowOverrideArgsInTask is true",
			templateArgs:          strPtr(`["--forks", "5"]`),
			allowOverride:         true,
			taskArgs:              strPtr(`["--forks", "10"]`),
			expectedEffectiveFork: "10",
			expectedForksSubArgs:  []string{"--forks", "5", "--forks", "10"},
		},
		{
			name:                  "Task level ignored when AllowOverrideArgsInTask is false",
			templateArgs:          strPtr(`["--forks", "5"]`),
			allowOverride:         false,
			taskArgs:              strPtr(`["--forks", "10"]`),
			expectedEffectiveFork: "5",
			expectedForksSubArgs:  []string{"--forks", "5"},
			mustNotContain:        []string{"10"},
		},
		{
			name:             "Invalid JSON in template arguments returns descriptive error",
			templateArgs:     strPtr(`--forks 10`),
			allowOverride:    false,
			taskArgs:         nil,
			expectError:      true,
			expectedErrorMsg: "invalid format of the template extra arguments, must be valid JSON",
		},
		{
			name:             "Invalid JSON in task arguments returns descriptive error when AllowOverrideArgsInTask is true",
			templateArgs:     strPtr(`["--forks", "5"]`),
			allowOverride:    true,
			taskArgs:         strPtr(`invalid-json`),
			expectError:      true,
			expectedErrorMsg: "invalid format of the TaskRunner extra arguments, must be valid JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &LocalExecutor{
				Logger: task_logger.NopLogger{},
				Template: db.Template{
					Type:                    db.TemplateTask,
					Playbook:                "site.yml",
					Arguments:               tt.templateArgs,
					AllowOverrideArgsInTask: tt.allowOverride,
				},
				Task: db.Task{
					Arguments: tt.taskArgs,
				},
				Inventory: db.Inventory{
					Type: db.InventoryStatic,
				},
			}

			args, _, err := exec.getPlaybookArgs("admin", nil)
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErrorMsg)
				return
			}

			require.NoError(t, err)

			// Check that expected forks sub-arguments are present in sequence
			if len(tt.expectedForksSubArgs) > 0 {
				found := false
				for i := 0; i <= len(args)-len(tt.expectedForksSubArgs); i++ {
					match := true
					for j := 0; j < len(tt.expectedForksSubArgs); j++ {
						if args[i+j] != tt.expectedForksSubArgs[j] {
							match = false
							break
						}
					}
					if match {
						found = true
						break
					}
				}
				assert.True(t, found, "Expected sequence %v in generated args: %v", tt.expectedForksSubArgs, args)
			}

			// Verify the effective (last) fork argument value matches the expected setting
			if tt.expectedEffectiveFork != "" {
				var lastForkVal string
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--forks" || args[i] == "-f" {
						lastForkVal = args[i+1]
					}
				}
				assert.Equal(t, tt.expectedEffectiveFork, lastForkVal, "Expected final effective fork value to be %s", tt.expectedEffectiveFork)
			}

			// Verify disqualified values are absent if specified
			for _, prohibited := range tt.mustNotContain {
				assert.NotContains(t, args, prohibited, "Generated args must not contain overridden task arg %q: %v", prohibited, args)
			}

			// Verify playbook is the trailing argument
			assert.Equal(t, "site.yml", args[len(args)-1], "Playbook must be the last argument")
		})
	}
}

func strPtr(s string) *string {
	return &s
}

// recordingApp captures what the preparation hands to the dependency
// installation. The embedded nil interface panics on any other method, which
// keeps the double from silently covering more than it should.
type recordingApp struct {
	db_lib.LocalApp
	installingArgs db_lib.LocalAppInstallingArgs
	installed      bool
}

func (a *recordingApp) InstallRequirements(args db_lib.LocalAppInstallingArgs) error {
	a.installingArgs = args
	a.installed = true
	return nil
}

// recordingTerraformInstaller is the same double for the terraform preparation,
// which installs through its own entry point so it can pass init arguments.
type recordingTerraformInstaller struct {
	installingArgs db_lib.LocalAppInstallingArgs
	initArgs       []string
	installed      bool
}

func (a *recordingTerraformInstaller) InstallRequirementsWithInitArgs(
	args db_lib.LocalAppInstallingArgs,
	initArgs []string,
) error {
	a.installingArgs = args
	a.initArgs = initArgs
	a.installed = true
	return nil
}

// agentKeyInstaller installs every key as a running ssh agent, which is the
// state in which the socket can leak into stages that must not have it.
type agentKeyInstaller struct {
	socketFile string
}

func (i *agentKeyInstaller) Install(
	key db.AccessKey,
	usage db.AccessKeyRole,
	logger task_logger.Logger,
) (ssh.AccessKeyInstallation, error) {
	return ssh.AccessKeyInstallation{SSHAgent: &ssh.Agent{SocketFile: i.socketFile}}, nil
}

// executorWithInstalledAgent builds an executor whose preparation runs end to
// end without a network or a git binary: a local repository, a static
// inventory, and an ssh key that installs an agent.
func executorWithInstalledAgent(t *testing.T, app db_lib.LocalApp) *LocalExecutor {
	t.Helper()

	setupExecutorConfig(t)
	tmp := t.TempDir()
	util.Config.TmpPath = tmp
	util.Config.HomeDirMode = util.HomeDirModeProjectHome

	repoDir := filepath.Join(tmp, "repository")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))

	return &LocalExecutor{
		Task:       db.Task{ID: 42},
		Template:   db.Template{ID: 1, ProjectID: 1, Playbook: "test.yml"},
		Repository: db.Repository{ProjectID: 1, GitURL: repoDir},
		Inventory: db.Inventory{
			ID:        3,
			Type:      db.InventoryStatic,
			Inventory: "localhost",
			SSHKeyID:  intPtr(9),
			SSHKey:    db.AccessKey{ID: 9, Type: db.AccessKeySSH},
		},
		KeyInstaller: &agentKeyInstaller{socketFile: filepath.Join(tmp, "agent.sock")},
		App:          app,
		Logger:       &recordingLogger{},
	}
}

// assertNoAgentSocket fails when the installation stage was handed the socket of
// the agent that holds the inventory key.
func assertNoAgentSocket(t *testing.T, env []string) {
	t.Helper()

	for _, v := range env {
		assert.False(t, strings.HasPrefix(v, "SSH_AUTH_SOCK="),
			"the dependency installation must not receive the agent socket, got %q", v)
	}
}

// Dependency installation executes content of the template repository:
// ansible-galaxy runs the roles listed in requirements.yml and terraform init
// fetches and runs providers. Handing it the agent socket would turn that
// content into a signing oracle for the key Semaphore uses to reach the managed
// hosts.
func TestPrepareRunKeepsAgentSocketOutOfInstallStage(t *testing.T) {
	app := &recordingApp{}
	exec := executorWithInstalledAgent(t, app)

	require.NoError(t, exec.prepareRun(db_lib.LocalAppInstallingArgs{
		EnvironmentVars: []string{"ANSIBLE_FORCE_COLOR=True"},
	}))

	require.True(t, app.installed, "the installation stage must have run")
	require.NotEmpty(t, exec.getSSHAgentEnv(),
		"the agent must be installed, otherwise the test proves nothing")
	assertNoAgentSocket(t, app.installingArgs.EnvironmentVars)
}

func TestPrepareRunTerraformKeepsAgentSocketOutOfInstallStage(t *testing.T) {
	installer := &recordingTerraformInstaller{}
	exec := executorWithInstalledAgent(t, nil)

	require.NoError(t, exec.prepareRunTerraform(
		installer,
		db_lib.LocalAppInstallingArgs{EnvironmentVars: []string{"TF_IN_AUTOMATION=1"}},
		[]string{"-input=false"},
	))

	require.True(t, installer.installed, "the installation stage must have run")
	require.NotEmpty(t, exec.getSSHAgentEnv(),
		"the agent must be installed, otherwise the test proves nothing")
	assert.Equal(t, []string{"-input=false"}, installer.initArgs)
	assertNoAgentSocket(t, installer.installingArgs.EnvironmentVars)
}

// The environment slice belongs to the caller and is appended to after the
// preparation returns. Appending to it here would write into the caller's
// backing array and the caller's own append would then overwrite the value.
func TestPrepareRunDoesNotWriteIntoTheCallersEnvironment(t *testing.T) {
	app := &recordingApp{}
	exec := executorWithInstalledAgent(t, app)

	env := make([]string, 1, 4)
	env[0] = "ANSIBLE_FORCE_COLOR=True"

	require.NoError(t, exec.prepareRun(db_lib.LocalAppInstallingArgs{EnvironmentVars: env}))

	for i, v := range env[:cap(env)][len(env):] {
		assert.Empty(t, v, "the preparation wrote into the caller's spare capacity at %d", i)
	}
}

// captureLogrus routes the standard logger into the returned hook for the
// duration of the test and keeps its output off the test log.
func captureLogrus(t *testing.T) *logtest.Hook {
	t.Helper()

	std := logrus.StandardLogger()

	prevHooks := make(logrus.LevelHooks)
	for level, hooks := range std.Hooks {
		prevHooks[level] = append([]logrus.Hook(nil), hooks...)
	}
	prevOut := std.Out

	t.Cleanup(func() {
		std.ReplaceHooks(prevHooks)
		std.SetOutput(prevOut)
	})

	std.SetOutput(io.Discard)

	return logtest.NewGlobal()
}

// An inventory key of type SSH whose agent was never started leaves the task
// without SSH_AUTH_SOCK, and ansible then fails on the managed hosts with a
// message that does not point back here.
func TestGetSSHAgentEnv(t *testing.T) {
	t.Run("reports a key whose agent is missing", func(t *testing.T) {
		hook := captureLogrus(t)

		exec := &LocalExecutor{
			Task:      db.Task{ID: 42},
			Inventory: db.Inventory{ID: 3, SSHKeyID: intPtr(9), SSHKey: db.AccessKey{Type: db.AccessKeySSH}},
		}

		assert.Empty(t, exec.getSSHAgentEnv())

		entry := hook.LastEntry()
		require.NotNil(t, entry, "the missing agent must be reported")
		assert.Equal(t, logrus.WarnLevel, entry.Level)
		assert.Equal(t, 42, entry.Data["task_id"])
		assert.Equal(t, 3, entry.Data["inventory_id"])
	})

	t.Run("stays quiet when the agent is running", func(t *testing.T) {
		hook := captureLogrus(t)

		exec := &LocalExecutor{
			Task:               db.Task{ID: 42},
			Inventory:          db.Inventory{ID: 3, SSHKeyID: intPtr(9), SSHKey: db.AccessKey{Type: db.AccessKeySSH}},
			sshKeyInstallation: ssh.AccessKeyInstallation{SSHAgent: &ssh.Agent{SocketFile: "/run/agent.sock"}},
		}

		assert.Equal(t, "SSH_AUTH_SOCK=/run/agent.sock", exec.getSSHAgentEnv())
		assert.Nil(t, hook.LastEntry(), "a working agent is not worth a log line")
	})

	t.Run("stays quiet when the inventory has no ssh key", func(t *testing.T) {
		hook := captureLogrus(t)

		exec := &LocalExecutor{
			Task:      db.Task{ID: 42},
			Inventory: db.Inventory{ID: 3},
		}

		assert.Empty(t, exec.getSSHAgentEnv())
		assert.Nil(t, hook.LastEntry(), "an inventory without a key needs no agent")
	})
}

// The host selection must come from the task parameters alone: they are what
// AllowOverrideLimit is checked against and what the target host variable
// publishes to the playbook. A --limit smuggled into the raw task arguments
// lands after the computed one on the command line, where ansible lets it win.
func TestGetPlaybookArgsRejectsLimitInTaskArguments(t *testing.T) {
	tests := []struct {
		name               string
		taskArgs           string
		allowOverrideArgs  bool
		allowOverrideLimit bool
		wantRejected       bool
	}{
		{
			name:              "long option with an attached value",
			taskArgs:          `["--limit=prod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      true,
		},
		{
			name:              "long option with a separate value",
			taskArgs:          `["--limit", "prod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      true,
		},
		{
			name:              "unambiguous long abbreviation",
			taskArgs:          `["--lim", "prod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      true,
		},
		{
			name:              "short option with a separate value",
			taskArgs:          `["-l", "prod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      true,
		},
		{
			name:              "short option with an attached value",
			taskArgs:          `["-lprod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      true,
		},
		{
			name:              "short option inside a cluster",
			taskArgs:          `["-Dl", "prod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      true,
		},
		{
			name:               "rejected even when the task may set the limit",
			taskArgs:           `["--limit=prod-db01"]`,
			allowOverrideArgs:  true,
			allowOverrideLimit: true,
			wantRejected:       true,
		},
		{
			name:              "the arguments are ignored anyway when the template forbids them",
			taskArgs:          `["--limit=prod-db01"]`,
			allowOverrideArgs: false,
			wantRejected:      false,
		},
		{
			name:              "unrelated arguments",
			taskArgs:          `["--forks", "10", "--diff"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "a long option that merely starts like the limit",
			taskArgs:          `["--list-hosts"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "a value that looks like the short option",
			taskArgs:          `["--ssh-extra-args", "-l root"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "a value attached to another short option",
			taskArgs:          `["-elocalhost"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "everything after the separator is positional",
			taskArgs:          `["--", "-l", "prod-db01"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "become password file long form value starting with a dash",
			taskArgs:          `["--become-password-file", "-l"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "become password file short form value starting with a dash",
			taskArgs:          `["--become-pass-file", "-l"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "connection password file long form value starting with a dash",
			taskArgs:          `["--connection-password-file", "-later/pass"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "connection password file short form value starting with a dash",
			taskArgs:          `["--conn-pass-file", "-later/pass"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
		{
			name:              "vault password file short form value starting with a dash",
			taskArgs:          `["--vault-pass-file", "-l"]`,
			allowOverrideArgs: true,
			wantRejected:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupExecutorConfig(t)
			util.Config.TmpPath = t.TempDir()

			exec := &LocalExecutor{
				Task: db.Task{
					TargetHostVar: "involved_hosts",
					Arguments:     strPtr(tt.taskArgs),
				},
				Template: db.Template{
					Playbook:                "test.yml",
					AllowOverrideArgsInTask: tt.allowOverrideArgs,
					TaskParams: db.MapStringAnyField{
						"limit":                []any{"webservers"},
						"allow_override_limit": tt.allowOverrideLimit,
					},
				},
				Inventory: db.Inventory{Type: db.InventoryStatic},
				Logger:    &recordingLogger{},
			}

			args, _, err := exec.getPlaybookArgs("admin", nil)

			if tt.wantRejected {
				require.Error(t, err)
				assert.ErrorContains(t, err, "--limit")
				return
			}

			require.NoError(t, err)
			assert.Contains(t, args, "--limit=webservers",
				"the limit computed from the parameters must still be applied")
		})
	}
}
