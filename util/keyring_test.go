package util

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genKey returns a valid base64-encoded 32-byte AES key whose bytes are all b.
func genKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// mustKeyset builds a Config whose runtime keyset is resolved from the given
// keys-file config plus flat fields, bypassing ConfigInit.
func mustKeyset(t *testing.T, enc *EncryptionKeysConfig, flatAccess, flatOption string) *ConfigType {
	t.Helper()
	ks, err := resolveEncryptionKeysFrom(enc, flatAccess, flatOption)
	require.NoError(t, err)
	c := &ConfigType{keys: &keyringStore{}, AccessKeyEncryption: flatAccess, OptionEncryption: flatOption}
	c.keys.current.Store(ks)
	return c
}

func keysCfg(keys map[string]string, accessLabel, optionLabel string) *EncryptionKeysConfig {
	enc := &EncryptionKeysConfig{Keys: map[string]KeySource{}, Active: ActivePointers{SecretKey: accessLabel, OptionKey: optionLabel}}
	for label, val := range keys {
		enc.Keys[label] = KeySource{Value: val}
	}
	return enc
}

func TestKeyset_AccessRoundTripWithIDPrefix(t *testing.T) {
	keyA := genKey(0x01)
	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA}, "a", ""), "", "")

	ct, err := Config.EncryptAccessSecret([]byte("super-secret"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ct, keyID(keyA)+":"), "stored value carries the key id prefix")

	pt, err := Config.DecryptAccessSecret(ct)
	require.NoError(t, err)
	assert.Equal(t, "super-secret", string(pt))
}

func TestKeyset_RotationByID(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA}, "a", ""), "", "")
	old, err := Config.EncryptAccessSecret([]byte("rotate-me"))
	require.NoError(t, err)

	// Rotate: both keys in the registry, active = b.
	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA, "b": keyB}, "b", ""), "", "")

	// Old value (id a) still decrypts via direct lookup.
	pt, err := Config.DecryptAccessSecret(old)
	require.NoError(t, err)
	assert.Equal(t, "rotate-me", string(pt))

	// New writes use b.
	fresh, err := Config.EncryptAccessSecret([]byte("fresh"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(fresh, keyID(keyB)+":"))
	assert.Equal(t, keyID(keyB), Config.ActiveAccessKeyID())
}

func TestKeyset_UnknownIDFailsLoud(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA}, "a", ""), "", "")
	old, err := Config.EncryptAccessSecret([]byte("orphan"))
	require.NoError(t, err)

	// Key a removed from the registry.
	Config = mustKeyset(t, keysCfg(map[string]string{"b": keyB}, "b", ""), "", "")

	_, err = Config.DecryptAccessSecret(old)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestKeyset_LegacyNoPrefixDecrypts(t *testing.T) {
	keyA := genKey(0x01)

	// A value written before the key-id envelope: raw ciphertext, no prefix.
	legacy, err := EncryptAESGCM([]byte("legacy"), keyA)
	require.NoError(t, err)
	assert.NotContains(t, legacy, ":")

	t.Run("key in registry", func(t *testing.T) {
		Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA}, "a", ""), "", "")
		pt, err := Config.DecryptAccessSecret(legacy)
		require.NoError(t, err)
		assert.Equal(t, "legacy", string(pt))
	})

	t.Run("key only in flat field", func(t *testing.T) {
		Config = mustKeyset(t, nil, keyA, "")
		pt, err := Config.DecryptAccessSecret(legacy)
		require.NoError(t, err)
		assert.Equal(t, "legacy", string(pt))
	})
}

func TestKeyset_EmptyKeyPassthrough(t *testing.T) {
	Config = mustKeyset(t, nil, "", "")

	ct, err := Config.EncryptAccessSecret([]byte("plain"))
	require.NoError(t, err)
	// No key, no prefix: just base64 of the plaintext.
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("plain")), ct)
	assert.Empty(t, Config.ActiveAccessKeyID())

	pt, err := Config.DecryptAccessSecret(ct)
	require.NoError(t, err)
	assert.Equal(t, "plain", string(pt))
}

func TestKeyset_OptionFallsBackToAccess(t *testing.T) {
	keyA := genKey(0x01)
	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA}, "a", ""), "", "")

	assert.Equal(t, keyID(keyA), Config.ActiveOptionKeyID())

	ct, err := Config.EncryptOption([]byte("jwt-pem"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ct, keyID(keyA)+":"))

	pt, err := Config.DecryptOption(ct)
	require.NoError(t, err)
	assert.Equal(t, "jwt-pem", string(pt))
}

func TestKeyset_SeparateOptionKey(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)
	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA, "b": keyB}, "a", "b"), "", "")

	assert.Equal(t, keyID(keyB), Config.ActiveOptionKeyID())

	ct, err := Config.EncryptOption([]byte("jwt"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ct, keyID(keyB)+":"))

	// Encrypted with b, not a.
	_, ctRaw, _ := parseEnvelope(ct)
	pt, err := DecryptAESGCM(ctRaw, keyB)
	require.NoError(t, err)
	assert.Equal(t, "jwt", string(pt))
}

func TestKeyset_OptionLegacyAccessFallback(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	// Pre-split JWT: encrypted with the access key, no prefix.
	legacy, err := EncryptAESGCM([]byte("legacy-jwt"), keyA)
	require.NoError(t, err)

	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA, "b": keyB}, "a", "b"), "", "")

	pt, err := Config.DecryptOption(legacy)
	require.NoError(t, err)
	assert.Equal(t, "legacy-jwt", string(pt))
}

func TestKeyset_Classify(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	// Encrypt under a, then rotate active to b (a retired).
	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA}, "a", ""), "", "")
	underA, err := Config.EncryptAccessSecret([]byte("x"))
	require.NoError(t, err)
	legacy, err := EncryptAESGCM([]byte("x"), keyA)
	require.NoError(t, err)

	Config = mustKeyset(t, keysCfg(map[string]string{"a": keyA, "b": keyB}, "b", ""), "", "")
	underB, err := Config.EncryptAccessSecret([]byte("x"))
	require.NoError(t, err)

	assert.Equal(t, "active:"+keyID(keyB), Config.ClassifyAccessSecret(underB))
	assert.Equal(t, "rekey pending:"+keyID(keyA), Config.ClassifyAccessSecret(underA))
	assert.Equal(t, "legacy (no id)", Config.ClassifyAccessSecret(legacy))

	// Drop key a → a value under a is MISSING.
	Config = mustKeyset(t, keysCfg(map[string]string{"b": keyB}, "b", ""), "", "")
	assert.Contains(t, Config.ClassifyAccessSecret(underA), "MISSING KEY")
}

func TestResolveEncryptionKeysFrom(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	t.Run("flat access only", func(t *testing.T) {
		ks, err := resolveEncryptionKeysFrom(nil, keyA, "")
		require.NoError(t, err)
		assert.Equal(t, keyID(keyA), ks.accessID)
		assert.Equal(t, keyA, ks.byID[keyID(keyA)])
		assert.Empty(t, ks.optionID)
	})

	t.Run("flat option key", func(t *testing.T) {
		ks, err := resolveEncryptionKeysFrom(nil, keyA, keyB)
		require.NoError(t, err)
		assert.Equal(t, keyID(keyA), ks.accessID)
		assert.Equal(t, keyID(keyB), ks.optionID)
	})

	t.Run("keys + active pointers", func(t *testing.T) {
		ks, err := resolveEncryptionKeysFrom(keysCfg(map[string]string{"a": keyA, "b": keyB}, "a", "b"), "", "")
		require.NoError(t, err)
		assert.Equal(t, keyID(keyA), ks.accessID)
		assert.Equal(t, keyID(keyB), ks.optionID)
		assert.Len(t, ks.byID, 2)
	})

	t.Run("missing active label errors", func(t *testing.T) {
		_, err := resolveEncryptionKeysFrom(keysCfg(map[string]string{"a": keyA}, "nope", ""), "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no key labelled")
	})

	t.Run("invalid key errors", func(t *testing.T) {
		_, err := resolveEncryptionKeysFrom(&EncryptionKeysConfig{Keys: map[string]KeySource{"a": {Value: "not-base64!!!"}}}, "", "")
		require.Error(t, err)
	})

	t.Run("mutually exclusive value+file errors", func(t *testing.T) {
		_, err := resolveEncryptionKeysFrom(&EncryptionKeysConfig{Keys: map[string]KeySource{"a": {Value: keyA, File: "/tmp/x"}}}, "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("key from file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "k")
		require.NoError(t, os.WriteFile(path, []byte(keyA+"\n"), 0o600))
		ks, err := resolveEncryptionKeysFrom(&EncryptionKeysConfig{
			Keys:   map[string]KeySource{"a": {File: path}},
			Active: ActivePointers{SecretKey: "a"},
		}, "", "")
		require.NoError(t, err)
		assert.Equal(t, keyID(keyA), ks.accessID)
	})
}

func TestKeyset_KeysFolder(t *testing.T) {
	keyA, keyB, keyOld := genKey(0x01), genKey(0x02), genKey(0x03)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "access_key_primary.txt"), []byte(keyA), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "option_key_primary.txt"), []byte(keyB), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "access_key_old.txt"), []byte(keyOld), 0o600))
	// Kubernetes-style hidden entry that must be skipped (else "garbage" would fail validation).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "..data"), []byte("garbage"), 0o600))

	enc := &EncryptionKeysConfig{
		KeysFolder: dir,
		Active:     ActivePointers{SecretKeyFile: "access_key_primary.txt", OptionKeyFile: "option_key_primary.txt"},
	}
	Config = mustKeyset(t, enc, "", "")

	assert.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())
	assert.Equal(t, keyID(keyB), Config.ActiveOptionKeyID())
	// Every folder file is in the registry (a retired key stays as a file).
	assert.True(t, Config.HasKeyID(keyID(keyA)))
	assert.True(t, Config.HasKeyID(keyID(keyB)))
	assert.True(t, Config.HasKeyID(keyID(keyOld)))

	// Data encrypted under the retired key still decrypts by id.
	underOld, err := EncryptAESGCM([]byte("retired"), keyOld) // legacy, no prefix
	require.NoError(t, err)
	pt, err := Config.DecryptAccessSecret(underOld)
	require.NoError(t, err)
	assert.Equal(t, "retired", string(pt))

	ct, err := Config.EncryptAccessSecret([]byte("s"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ct, keyID(keyA)+":"))
}

func TestReloadEncryptionKeys(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.json")
	write := func(body string) { require.NoError(t, os.WriteFile(keysPath, []byte(body), 0o600)) }

	write(`{"keys":{"a":{"value":"` + keyA + `"}},"active":{"secret_key":"a"}}`)
	Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: keysPath}}
	resolveEncryptionKeys()
	require.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())

	old, err := Config.EncryptAccessSecret([]byte("rotate-me"))
	require.NoError(t, err)

	// Rotate via the file: add b, make it active.
	write(`{"keys":{"a":{"value":"` + keyA + `"},"b":{"value":"` + keyB + `"}},"active":{"secret_key":"b"}}`)
	require.NoError(t, ReloadEncryptionKeys())

	assert.Equal(t, keyID(keyB), Config.ActiveAccessKeyID())
	pt, err := Config.DecryptAccessSecret(old)
	require.NoError(t, err)
	assert.Equal(t, "rotate-me", string(pt))

	// Invalid reload is rejected; active keyset untouched.
	write(`{"keys":{"a":{"value":"not-base64!!!"}},"active":{"secret_key":"a"}}`)
	require.Error(t, ReloadEncryptionKeys())
	assert.Equal(t, keyID(keyB), Config.ActiveAccessKeyID())
}

func TestEncryptionKeysFile_FormatAgnostic(t *testing.T) {
	keyA := genKey(0x01)
	dir := t.TempDir()

	t.Run("YAML, no extension", func(t *testing.T) {
		path := filepath.Join(dir, "encryption_keys")
		require.NoError(t, os.WriteFile(path, []byte(
			"keys:\n  a:\n    value: "+keyA+"\nactive:\n  secret_key: a\n"), 0o600))
		Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: path}}
		resolveEncryptionKeys()
		assert.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())
	})

	t.Run("JSON, no extension", func(t *testing.T) {
		path := filepath.Join(dir, "keys_json")
		require.NoError(t, os.WriteFile(path, []byte(
			`{"keys":{"a":{"value":"`+keyA+`"}},"active":{"secret_key":"a"}}`), 0o600))
		Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: path}}
		resolveEncryptionKeys()
		assert.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())
	})
}

func TestEncryptionKeysFile_ReferencedKeyFileContentChange(t *testing.T) {
	keyA := genKey(0x01)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "access.key")
	require.NoError(t, os.WriteFile(keyPath, []byte(keyA), 0o600))

	keysPath := filepath.Join(dir, "keys.yaml")
	require.NoError(t, os.WriteFile(keysPath, []byte(
		"keys:\n  a:\n    file: "+keyPath+"\nactive:\n  secret_key: a\n"), 0o600))

	Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: keysPath}}
	resolveEncryptionKeys()
	assert.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())

	// Changing the referenced key file's content is detected and applied.
	keyA2 := genKey(0x03)
	require.NoError(t, os.WriteFile(keyPath, []byte(keyA2), 0o600))
	changed, err := ReloadEncryptionKeysIfChanged()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, keyID(keyA2), Config.ActiveAccessKeyID())
}

func TestReloadEncryptionKeys_ConcurrentReadsAreRaceFree(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.json")
	cfgA := `{"keys":{"a":{"value":"` + keyA + `"},"b":{"value":"` + keyB + `"}},"active":{"secret_key":"a"}}`
	cfgB := `{"keys":{"a":{"value":"` + keyA + `"},"b":{"value":"` + keyB + `"}},"active":{"secret_key":"b"}}`

	require.NoError(t, os.WriteFile(keysPath, []byte(cfgA), 0o600))
	Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: keysPath}}
	resolveEncryptionKeys()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ct, err := Config.EncryptAccessSecret([]byte("data"))
				if err != nil {
					t.Errorf("encrypt: %v", err)
					return
				}
				if _, err := Config.DecryptAccessSecret(ct); err != nil {
					t.Errorf("decrypt: %v", err)
					return
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		body := cfgA
		if i%2 == 1 {
			body = cfgB
		}
		require.NoError(t, os.WriteFile(keysPath, []byte(body), 0o600))
		require.NoError(t, ReloadEncryptionKeys())
	}
	close(stop)
	wg.Wait()
}

func TestEncryptionConfigGetters(t *testing.T) {
	t.Run("keys file path", func(t *testing.T) {
		Config = &ConfigType{}
		assert.Empty(t, Config.EncryptionKeysFile())
		Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: "/x"}}
		assert.Equal(t, "/x", Config.EncryptionKeysFile())
	})

	t.Run("poll interval default/custom/zero/invalid", func(t *testing.T) {
		Config = &ConfigType{}
		assert.Equal(t, 15*time.Second, Config.EncryptionKeysPollInterval())
		Config = &ConfigType{Encryption: &EncryptionConfig{KeysPollInterval: "30s"}}
		assert.Equal(t, 30*time.Second, Config.EncryptionKeysPollInterval())
		Config = &ConfigType{Encryption: &EncryptionConfig{KeysPollInterval: "0"}}
		assert.Equal(t, time.Duration(0), Config.EncryptionKeysPollInterval())
		Config = &ConfigType{Encryption: &EncryptionConfig{KeysPollInterval: "nonsense"}}
		assert.Equal(t, 15*time.Second, Config.EncryptionKeysPollInterval())
	})
}

// keysetsEqual decides whether a reload swaps the live keyset. A false "equal"
// keeps a rotated key out of the process, so every field it compares needs a
// case that differs in that field alone.
func TestKeysetsEqual(t *testing.T) {
	base := func() *keyset {
		return &keyset{
			byID:         map[string]string{"id-a": "material-a"},
			accessID:     "id-a",
			optionID:     "id-o",
			legacyAccess: "flat-access",
			legacyOption: "flat-option",
		}
	}

	t.Run("both nil", func(t *testing.T) {
		assert.True(t, keysetsEqual(nil, nil))
	})

	t.Run("one nil", func(t *testing.T) {
		assert.False(t, keysetsEqual(nil, base()))
		assert.False(t, keysetsEqual(base(), nil))
	})

	// Distinct pointers, so an identity comparison would report them unequal.
	t.Run("equal content", func(t *testing.T) {
		assert.True(t, keysetsEqual(base(), base()))
	})

	tests := []struct {
		name   string
		change func(*keyset)
	}{
		{"active access id", func(k *keyset) { k.accessID = "id-b" }},
		{"active option id", func(k *keyset) { k.optionID = "id-p" }},
		{"flat access key", func(k *keyset) { k.legacyAccess = "other" }},
		{"flat option key", func(k *keyset) { k.legacyOption = "other" }},
		{"registry size", func(k *keyset) { k.byID["id-b"] = "material-b" }},
		{"key material under the same id", func(k *keyset) { k.byID["id-a"] = "rotated" }},
		{"same size, different ids", func(k *keyset) {
			delete(k.byID, "id-a")
			k.byID["id-b"] = "material-a"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base()
			tt.change(changed)

			assert.False(t, keysetsEqual(base(), changed))
			assert.False(t, keysetsEqual(changed, base()))
		})
	}
}

// currentKeyset is the path a unit test (or a process whose ConfigInit has not
// run) takes: the flat fields become a one-key registry. An id must be minted
// only for a key that is actually set, otherwise the empty passthrough key is
// registered under the empty id and every lookup finds it.
func TestCurrentKeysetFallsBackToFlatFields(t *testing.T) {
	access := genKey(0x01)
	option := genKey(0x02)

	t.Run("no keys configured", func(t *testing.T) {
		ks := (&ConfigType{}).currentKeyset()

		assert.Empty(t, ks.byID)
		assert.Empty(t, ks.accessID)
		assert.Empty(t, ks.optionID)
	})

	t.Run("flat access key only", func(t *testing.T) {
		ks := (&ConfigType{AccessKeyEncryption: access}).currentKeyset()

		assert.Equal(t, keyID(access), ks.accessID)
		assert.Equal(t, map[string]string{keyID(access): access}, ks.byID)
		assert.Empty(t, ks.optionID)
	})

	t.Run("both flat keys", func(t *testing.T) {
		ks := (&ConfigType{AccessKeyEncryption: access, OptionEncryption: option}).currentKeyset()

		assert.Equal(t, keyID(access), ks.accessID)
		assert.Equal(t, keyID(option), ks.optionID)
		assert.Len(t, ks.byID, 2)
	})
}

// An active key file the folder scan did not register is read on its own:
// relative to keys_folder, or as given when it is absolute. The subdirectory
// covers both halves at once — the scan has to skip a non-regular entry, and
// the file inside it is then only reachable through the join.
func TestKeyset_ActiveKeyFileOutsideFolder(t *testing.T) {
	keyA, keyB := genKey(0x01), genKey(0x02)

	folder := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(folder, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(folder, "nested", "access.txt"), []byte(keyA), 0o600))

	elsewhere := filepath.Join(t.TempDir(), "option.txt")
	require.NoError(t, os.WriteFile(elsewhere, []byte(keyB), 0o600))

	enc := &EncryptionKeysConfig{
		KeysFolder: folder,
		Active: ActivePointers{
			SecretKeyFile: filepath.Join("nested", "access.txt"),
			OptionKeyFile: elsewhere,
		},
	}

	Config = mustKeyset(t, enc, "", "")

	assert.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())
	assert.Equal(t, keyID(keyB), Config.ActiveOptionKeyID())
}

// A reload can arrive (SIGHUP, the keys-file watcher) before anything resolved
// the keyring, so the store has to be created rather than assumed present.
func TestReloadEncryptionKeys_WithoutPriorResolve(t *testing.T) {
	keyA := genKey(0x01)
	keysPath := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(keysPath,
		[]byte(`{"keys":{"a":{"value":"`+keyA+`"}},"active":{"secret_key":"a"}}`), 0o600))

	Config = &ConfigType{Encryption: &EncryptionConfig{KeysFile: keysPath}}
	require.Nil(t, Config.keys)

	require.NoError(t, ReloadEncryptionKeys())

	assert.Equal(t, keyID(keyA), Config.ActiveAccessKeyID())
}
