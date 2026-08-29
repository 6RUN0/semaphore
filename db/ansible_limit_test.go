package db

import (
	"testing"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAnsibleLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit []string
		valid bool
	}{
		{name: "nothing picked", limit: nil, valid: true},
		{name: "plain host", limit: []string{"web01"}, valid: true},
		{name: "several hosts", limit: []string{"web01", "web02"}, valid: true},
		{name: "group", limit: []string{"webservers"}, valid: true},
		{name: "range", limit: []string{"web[01:05]"}, valid: true},
		{name: "exclusion", limit: []string{"!db"}, valid: true},
		{name: "intersection", limit: []string{"webservers:&production"}, valid: true},
		{name: "wildcard", limit: []string{"*.example.com"}, valid: true},
		{name: "regex", limit: []string{"~(web|db).*"}, valid: true},

		{name: "jinja expression", limit: []string{"{{ lookup('pipe', 'id') }}"}, valid: false},
		{name: "jinja statement", limit: []string{"{% for h in groups %}"}, valid: false},
		{name: "opening brace alone", limit: []string{"web{01"}, valid: false},
		{name: "closing brace alone", limit: []string{"web}01"}, valid: false},
		{name: "payload hidden behind a valid host", limit: []string{"web01", "{{ 7*7 }}"}, valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAnsibleLimit(tt.limit, "task")

			if tt.valid {
				assert.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.ErrorContains(t, err, "must not contain")
		})
	}
}

// The limit reaches Ansible as a template through the extra vars, so the check
// has to sit on the path that accepts a task, not only in the helper.
func TestValidateNewTaskRejectsTemplatedLimit(t *testing.T) {
	template := Template{App: AppAnsible}

	task := Task{Params: MapStringAnyField{
		"limit": []any{"{{ lookup('pipe', 'id') }}"},
	}}

	err := task.ValidateNewTask(template)

	require.Error(t, err)
	assert.ErrorContains(t, err, "must not contain")
}

func TestValidateNewTaskAcceptsOrdinaryLimit(t *testing.T) {
	template := Template{App: AppAnsible}

	task := Task{Params: MapStringAnyField{
		"limit": []any{"web[01:05]", "!db", "webservers:&production"},
	}}

	assert.NoError(t, task.ValidateNewTask(template))
}

// A non-Ansible app has no limit at all; the check must not reject its params
// or trip over their different shape.
func TestValidateNewTaskIgnoresLimitForOtherApps(t *testing.T) {
	task := Task{Params: MapStringAnyField{"destroy": true}}

	assert.NoError(t, task.ValidateNewTask(Template{App: AppTerraform}))
}

// The template's limit is published through the same variable as the task's, so
// a privileged user must not be able to plant a payload there either.
func TestTemplateValidateRejectsTemplatedLimit(t *testing.T) {
	orig := util.Config
	defer func() { util.Config = orig }()
	util.Config = &util.ConfigType{Apps: map[string]util.App{"ansible": {Active: true}}}

	inventoryID := 1
	tpl := &Template{
		Name:        "deploy",
		Playbook:    "site.yml",
		App:         AppAnsible,
		InventoryID: &inventoryID,
		TaskParams:  MapStringAnyField{"limit": []any{"{{ lookup('pipe', 'id') }}"}},
	}

	err := tpl.Validate()

	require.Error(t, err)
	assert.ErrorContains(t, err, "must not contain")
}

func TestTemplateValidateAcceptsOrdinaryLimit(t *testing.T) {
	orig := util.Config
	defer func() { util.Config = orig }()
	util.Config = &util.ConfigType{Apps: map[string]util.App{"ansible": {Active: true}}}

	inventoryID := 1
	tpl := &Template{
		Name:        "deploy",
		Playbook:    "site.yml",
		App:         AppAnsible,
		InventoryID: &inventoryID,
		TaskParams:  MapStringAnyField{"limit": []any{"webservers"}},
	}

	assert.NoError(t, tpl.Validate())
}
