package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The nil check used to be "e.RunnerTag == nil && *e.RunnerTag == """, which
// dereferences the pointer precisely when it is nil.
func TestInventoryValidateRunnerTag(t *testing.T) {
	empty := ""
	tag := "linux"

	tests := []struct {
		name      string
		runnerTag *string
		wantError bool
	}{
		{"tag unset", nil, false},
		{"tag set", &tag, false},
		{"tag set to an empty string", &empty, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Inventory{RunnerTag: tt.runnerTag}.Validate()

			if tt.wantError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "runner tag")

				return
			}

			assert.NoError(t, err)
		})
	}
}
