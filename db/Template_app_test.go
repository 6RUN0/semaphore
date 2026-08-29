package db

import (
	"testing"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
)

func TestTemplate_Validate_App(t *testing.T) {
	orig := util.Config
	defer func() { util.Config = orig }()

	util.Config = &util.ConfigType{
		Apps: map[string]util.App{"bash": {Active: true}},
	}

	// Arbitrary binary not in the whitelist must be rejected before persisting.
	tpl := &Template{Name: "rce", Playbook: "-c", App: "/bin/sh"}
	assert.EqualError(t, tpl.Validate(), "invalid app: /bin/sh")

	// Whitelisted app is accepted.
	ok := &Template{Name: "legit", Playbook: "script.sh", App: "bash"}
	assert.NoError(t, ok.Validate())

	// Empty legacy app runs no command and stays allowed.
	empty := &Template{Name: "legacy", Playbook: "site.yml", App: ""}
	assert.NoError(t, empty.Validate())
}

// IsTerraform gates state handling, the workspace inventory types and the
// "playbook may be empty" rule, so a missing app in the list is a silent
// behaviour change rather than an error.
func TestTemplateApp_IsTerraform(t *testing.T) {
	tests := []struct {
		app  TemplateApp
		want bool
	}{
		{AppTerraform, true},
		{AppTofu, true},
		{AppTerragrunt, true},

		{AppAnsible, false},
		{AppBash, false},
		{AppPowerShell, false},
		{AppPython, false},
		{AppPulumi, false},
		{TemplateApp(""), false},
		{TemplateApp("terraforming"), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.app), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.app.IsTerraform())
		})
	}
}
