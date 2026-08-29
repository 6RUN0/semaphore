package server

import (
	"github.com/semaphoreui/semaphore/db"
)

func GetSecretStorages(repo db.SecretStorageRepository, projectID int) (storages []db.SecretStorage, err error) {
	storages = make([]db.SecretStorage, 0)
	return
}

// StorageRequiresSecret reports whether the storage keeps credentials of its
// own in Semaphore. Create skips creating an access key when it does not, and
// Update deletes the keys it already had — which is what takes a long-lived AWS
// key out of the database once a storage is switched to an IAM role.
//
// Only a real boolean true counts: params are decoded from JSON, and deleting
// credentials because a value happened to be a non-empty string would break a
// working storage.
func StorageRequiresSecret(storage db.SecretStorage) bool {
	if storage.Type != db.SecretStorageTypeAwsSm {
		return true
	}

	useIamRole, _ := storage.Params["use_iam_role"].(bool)

	return !useIamRole
}

func SyncSecrets(
	sync db.SecretSync,
	storageRepo db.SecretStorageRepository,
	accessKeyRepo db.AccessKeyManager,
	decryptor DvlsStorageTokenDeserializer,
) error {
	return nil
}
