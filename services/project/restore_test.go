package project

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
)

// TestVerifyDuplicate pins the off-by-one that let exactly two entities with
// the same name pass verification.
func TestVerifyDuplicate(t *testing.T) {
	env := func(name string) BackupEnvironment {
		return BackupEnvironment{Environment: db.Environment{Name: name}}
	}

	tests := []struct {
		name    string
		items   []BackupEnvironment
		wantErr bool
	}{
		{"single", []BackupEnvironment{env("a")}, false},
		{"distinct", []BackupEnvironment{env("a"), env("b")}, false},
		{"two equal", []BackupEnvironment{env("a"), env("a")}, true},
		{"three equal", []BackupEnvironment{env("a"), env("a"), env("a")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyDuplicate[BackupEnvironment]("a", tt.items)
			if tt.wantErr {
				assert.ErrorContains(t, err, "duplicate")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
