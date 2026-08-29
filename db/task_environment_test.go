package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateTaskEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		environment string
		valid       bool
	}{
		{name: "nothing supplied", environment: "", valid: true},
		{name: "plain values", environment: `{"release":"1.4.2","dry_run":true}`, valid: true},
		{name: "nested plain values", environment: `{"cfg":{"retries":3,"hosts":["a","b"]}}`, valid: true},
		{name: "malformed json is someone else's error", environment: `{not json`, valid: true},

		{name: "expression in a value", environment: `{"x":"{{ lookup('pipe', 'id') }}"}`, valid: false},
		{name: "statement in a value", environment: `{"x":"{% for h in groups %}"}`, valid: false},
		{name: "expression in a variable name", environment: `{"{{ x }}":"1"}`, valid: false},
		{name: "expression nested in an object", environment: `{"cfg":{"cmd":"{{ 7*7 }}"}}`, valid: false},
		{name: "expression nested in a list", environment: `{"cfg":["ok","{{ 7*7 }}"]}`, valid: false},
		{name: "expression in a nested key", environment: `{"cfg":{"{{ x }}":"ok"}}`, valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTaskEnvironment(tt.environment)

			if tt.valid {
				assert.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.ErrorContains(t, err, "must not contain")
		})
	}
}

// The environment reaches the extra vars the same way the limit does, so the
// check has to sit on the path that accepts a task.
func TestValidateNewTaskRejectsTemplatedEnvironment(t *testing.T) {
	task := Task{Environment: `{"payload":"{{ lookup('pipe', 'id') }}"}`}

	err := task.ValidateNewTask(Template{App: AppAnsible})

	require.Error(t, err)
	assert.ErrorContains(t, err, "must not contain")
}

// Only Ansible renders these as templates. A shell or Terraform task passes them
// as plain arguments, where a brace carries no meaning and rejecting it would
// break a legitimate value for no gain.
func TestValidateNewTaskAllowsTemplatedEnvironmentForOtherApps(t *testing.T) {
	task := Task{Environment: `{"payload":"{{ not ansible }}"}`}

	assert.NoError(t, task.ValidateNewTask(Template{App: AppTerraform}))
	assert.NoError(t, task.ValidateNewTask(Template{App: AppBash}))
}
