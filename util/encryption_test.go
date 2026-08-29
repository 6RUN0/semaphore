package util

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecryptAESGCM_ShortCiphertext(t *testing.T) {
	key := genKey(0x01)

	// AES-GCM's standard nonce. The split below assumes the ciphertext is at
	// least this long, so the guard has to reject anything shorter rather than
	// let the slice expression run.
	const nonceSize = 12

	t.Run("shorter than the nonce", func(t *testing.T) {
		_, err := DecryptAESGCM(base64.StdEncoding.EncodeToString(make([]byte, nonceSize-1)), key)

		assert.ErrorContains(t, err, "ciphertext too short")
	})

	// Exactly the nonce means an empty payload, which is a decryption failure
	// and not a malformed value: authentication has to be what rejects it.
	t.Run("exactly the nonce", func(t *testing.T) {
		_, err := DecryptAESGCM(base64.StdEncoding.EncodeToString(make([]byte, nonceSize)), key)

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "too short")
	})

	t.Run("round trip is unaffected", func(t *testing.T) {
		ct, err := EncryptAESGCM([]byte("payload"), key)
		require.NoError(t, err)

		plaintext, err := DecryptAESGCM(ct, key)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(plaintext))
	})
}
