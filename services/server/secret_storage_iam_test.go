package server

import (
	"testing"

	"github.com/semaphoreui/semaphore/db"
	pro "github.com/semaphoreui/semaphore/pro/services/server"
	"github.com/stretchr/testify/assert"
)

// StorageRequiresSecret decides whether a storage keeps credentials in
// Semaphore. Create skips creating an access key when it says no, and Update
// deletes the keys a storage already had — that deletion is the only thing that
// takes a long-lived AWS key out of the database when an operator switches the
// storage to an IAM role.
//
// The form offers that switch ("Use IAM Role") and hides the credential fields
// once it is on, so an operator reads a successful save as "the key is gone"
// and stops treating it as live. Answering "yes, a secret is required" for such
// a storage leaves the encrypted secret_access_key in project__access_key while
// the UI says otherwise.
func TestStorageRequiresSecret(t *testing.T) {
	tests := []struct {
		name     string
		storage  db.SecretStorage
		expected bool
	}{
		{
			name: "aws storage on an IAM role keeps no credentials",
			storage: db.SecretStorage{
				Type:   db.SecretStorageTypeAwsSm,
				Params: db.MapStringAnyField{"use_iam_role": true},
			},
			expected: false,
		},
		{
			name: "aws storage with the flag off still keeps its key",
			storage: db.SecretStorage{
				Type:   db.SecretStorageTypeAwsSm,
				Params: db.MapStringAnyField{"use_iam_role": false},
			},
			expected: true,
		},
		{
			name: "aws storage predating the flag keeps its key",
			storage: db.SecretStorage{
				Type:   db.SecretStorageTypeAwsSm,
				Params: db.MapStringAnyField{},
			},
			expected: true,
		},
		{
			name: "storage without params at all keeps its key",
			storage: db.SecretStorage{
				Type: db.SecretStorageTypeAwsSm,
			},
			expected: true,
		},
		{
			// The flag is an AWS concept; a vault token is always stored, and a
			// stray parameter must never delete it.
			name: "vault ignores the flag",
			storage: db.SecretStorage{
				Type:   db.SecretStorageTypeVault,
				Params: db.MapStringAnyField{"use_iam_role": true},
			},
			expected: true,
		},
		{
			// Params come from JSON, where the value could be anything. Only a
			// real boolean true drops the credentials: deleting a key because a
			// parameter happened to be a non-empty string would be worse than
			// keeping one that is no longer needed.
			name: "non-boolean flag is not a switch",
			storage: db.SecretStorage{
				Type:   db.SecretStorageTypeAwsSm,
				Params: db.MapStringAnyField{"use_iam_role": "true"},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, pro.StorageRequiresSecret(tt.storage))
		})
	}
}
