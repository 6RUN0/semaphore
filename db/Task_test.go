package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// IsInventoryResolve is asked on every status change and on both sides of the
// resolve path, so it reads the flag straight out of the params map. These pin
// that reading it directly still answers exactly what the flag means.
func TestIsInventoryResolve(t *testing.T) {
	tests := []struct {
		name     string
		params   MapStringAnyField
		expected bool
	}{
		{"no params at all", nil, false},
		{"empty params", MapStringAnyField{}, false},
		{"the flag is set", MapStringAnyField{TaskParamResolveInventoryOnly: true}, true},
		{"the flag is off", MapStringAnyField{TaskParamResolveInventoryOnly: false}, false},
		{"the flag is a string", MapStringAnyField{TaskParamResolveInventoryOnly: "true"}, false},
		{"the flag is a number", MapStringAnyField{TaskParamResolveInventoryOnly: 1}, false},
		{"an ordinary task with other params", MapStringAnyField{"debug": true, "limit": []any{"web"}}, false},
		{
			"the flag survives an unrelated param of the wrong type",
			MapStringAnyField{TaskParamResolveInventoryOnly: true, "limit": "web"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := Task{Params: tt.params}
			assert.Equal(t, tt.expected, task.IsInventoryResolve())
		})
	}
}

// The map key and the json tag of AnsibleTaskParams.ResolveInventoryOnly are
// two spellings of one thing; if they drift, a resolve task stops being
// recognized as one.
func TestInventoryResolveFlagMatchesAnsibleTaskParams(t *testing.T) {
	task := Task{Params: MapStringAnyField{TaskParamResolveInventoryOnly: true}}

	var params AnsibleTaskParams
	assert.NoError(t, task.ExtractParams(&params))
	assert.True(t, params.ResolveInventoryOnly)
	assert.Equal(t, params.ResolveInventoryOnly, task.IsInventoryResolve())
}
