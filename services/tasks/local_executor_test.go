package tasks

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/ssh"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
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
// silently ignore it and run the template for real (review finding C4).
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
// vault secrets found" (review finding M3).
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
