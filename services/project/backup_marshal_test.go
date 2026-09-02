package project

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_MarshalValue_NilPointer_ReturnsNil(t *testing.T) {
	var ptr *int
	result, err := marshalValue(reflect.ValueOf(ptr))
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func Test_MarshalValue_StructWithFields_ReturnsMap(t *testing.T) {
	type TestStruct struct {
		Field1 string `backup:"field1"`
		Field2 int    `backup:"field2"`
	}
	testStruct := TestStruct{Field1: "value1", Field2: 42}
	result, err := marshalValue(reflect.ValueOf(testStruct))
	assert.NoError(t, err)
	expected := map[string]any{"field1": "value1", "field2": 42}
	assert.Equal(t, expected, result)
}

func Test_MarshalValue_Slice_ReturnsSlice(t *testing.T) {
	slice := []int{1, 2, 3}
	result, err := marshalValue(reflect.ValueOf(slice))
	assert.NoError(t, err)
	expected := []any{1, 2, 3}
	assert.Equal(t, expected, result)
}

func Test_MarshalValue_Runner_ExcludesSecrets(t *testing.T) {
	regToken := "reg-secret"
	runner := db.Runner{
		ID:                    7,
		Name:                  "runner-1",
		Token:                 "super-secret-token",
		RegistrationTokenHash: &regToken,
		Webhook:               "https://example.com/hook",
	}

	result, err := marshalValue(reflect.ValueOf(runner))
	assert.NoError(t, err)

	m, ok := result.(map[string]any)
	assert.True(t, ok)

	// Authentication secrets must never be present in a project backup.
	assert.NotContains(t, m, "token")
	assert.NotContains(t, m, "registration_token")
	assert.NotContains(t, m, "registration_token_expires_at")

	// Non-secret fields are still exported; the webhook URL is a secret
	// since it commonly carries a token.
	assert.Equal(t, "runner-1", m["name"])
	assert.NotContains(t, m, "webhook")
}

func Test_UnmarshalValueWithBackupTags_StructWithFields_SetsFields(t *testing.T) {
	type TestStruct struct {
		//Field1     string               `backup:"field1"`
		//Field2     int                  `backup:"field2"`
		TaskParams db.MapStringAnyField `backup:"task_params"`
	}
	data := map[string]any{
		//"field1": "value1",
		//"field2": 42,
		"task_params": map[string]any{
			"allow_debug": true,
			"skip_tags":   []string{"123"},
		},
	}
	var testStruct TestStruct
	err := unmarshalValueWithBackupTags(data, reflect.ValueOf(&testStruct).Elem())
	assert.NoError(t, err)
	//assert.Equal(t, "value1", testStruct.Field1)
	//assert.Equal(t, 42, testStruct.Field2)
}
func Test_UnmarshalValueWithBackupTags_Slice_SetsElements(t *testing.T) {
	data := []any{1, 2, 3}
	var slice []int
	err := unmarshalValueWithBackupTags(data, reflect.ValueOf(&slice).Elem())
	assert.NoError(t, err)
	expected := []int{1, 2, 3}
	assert.Equal(t, expected, slice)
}

func Test_UnmarshalValueWithBackupTags_Map_SetsEntries(t *testing.T) {
	data := map[string]any{"key1": "value1", "key2": "value2"}
	var m map[string]string
	err := unmarshalValueWithBackupTags(data, reflect.ValueOf(&m).Elem())
	assert.NoError(t, err)
	expected := map[string]string{"key1": "value1", "key2": "value2"}
	assert.Equal(t, expected, m)
}
func Test_SetBasicType_InvalidType_ReturnsError(t *testing.T) {
	var v reflect.Value
	err := setBasicType("string", v)
	assert.Error(t, err)
}

func Test_ToFloat64_ValidInt_ReturnsFloat64(t *testing.T) {
	result, ok := toFloat64(42)
	assert.True(t, ok)
	assert.Equal(t, 42.0, result)
}

func Test_ToFloat64_InvalidType_ReturnsFalse(t *testing.T) {
	_, ok := toFloat64("string")
	assert.False(t, ok)
}

// Test_MarshalValue_SurveyVarDefaultValue pins the backup shape of a survey
// default: the value itself, in the scalar-or-array form of its JSON codec.
// Walking the struct by tags yields an empty object, and the default is lost.
func Test_MarshalValue_SurveyVarDefaultValue(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		expected any
	}{
		{"scalar", `"one"`, "one"},
		{"array", `["a","b"]`, []any{"a", "b"}},
		{"array of one", `["a"]`, []any{"a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v db.SurveyVar
			require.NoError(t, json.Unmarshal([]byte(`{"name":"v","default_value":`+tt.json+`}`), &v))

			result, err := marshalValue(reflect.ValueOf(v))
			require.NoError(t, err)
			m, ok := result.(map[string]any)
			require.True(t, ok, "expected a map, got %T", result)
			assert.Equal(t, tt.expected, m["default_value"])
		})
	}
}

// Test_UnmarshalValueWithBackupTags_SurveyVarDefaultValue pins the restore
// side: the scalar-or-array value lands in the codec, shape included.
func Test_UnmarshalValueWithBackupTags_SurveyVarDefaultValue(t *testing.T) {
	tests := []struct {
		name      string
		data      any
		values    []string
		wantArray bool
	}{
		{"scalar", "one", []string{"one"}, false},
		{"array", []any{"a", "b"}, []string{"a", "b"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v db.SurveyVar
			data := map[string]any{"name": "v", "default_value": tt.data}
			require.NoError(t, unmarshalValueWithBackupTags(data, reflect.ValueOf(&v).Elem()))

			require.NotNil(t, v.DefaultValue)
			assert.Equal(t, tt.values, v.DefaultValue.Values)
			assert.Equal(t, tt.wantArray, v.DefaultValue.IsArray())
		})
	}
}

// Test_MarshalValue_ExcludesPreservedFields pins the backup:"-" tags on
// columns that are runtime state or secret material: without them the JSON
// backup exports access_key.plain, environment.password and runner
// heartbeat fields. Values are synthetic markers, so a failure prints
// nothing sensitive.
func Test_MarshalValue_ExcludesPreservedFields(t *testing.T) {
	plain := "plaintext-secret"
	password := "env-password"
	now := time.Now()
	pub := "ssh-rsa AAAA"
	projectID := 3

	tests := []struct {
		name     string
		value    any
		excluded []string
	}{
		{
			name:     "access key",
			value:    db.AccessKey{Name: "k", Plain: &plain, Synchronized: true},
			excluded: []string{"plain", "synchronized"},
		},
		{
			name:     "environment",
			value:    db.Environment{Name: "e", Password: &password},
			excluded: []string{"password"},
		},
		{
			name: "runner",
			value: db.Runner{
				ID: 7, Name: "r", Webhook: "https://example.com/hook",
				Touched: &now, CleaningRequested: &now, StartedAt: &now, PublicKey: &pub,
			},
			excluded: []string{"id", "webhook", "touched", "cleaning_requested", "started_at", "public_key"},
		},
		{
			name:     "role",
			value:    db.Role{Slug: "s", Name: "n", ProjectID: &projectID},
			excluded: []string{"project_id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := marshalValue(reflect.ValueOf(tt.value))
			require.NoError(t, err)
			m, ok := result.(map[string]any)
			require.True(t, ok, "expected a map, got %T", result)
			for _, key := range tt.excluded {
				assert.NotContains(t, m, key)
			}
			assert.Contains(t, m, "name")
		})
	}
}

// TestBackupRoundTrip_DropsRunnerWebhook pins the restore side of the
// backup:"-" tag on Runner.Webhook: a backup that still carries a webhook
// (written before the tag existed) restores a runner without one. This is
// the documented behaviour change of the tag; see the commit body.
func TestBackupRoundTrip_DropsRunnerWebhook(t *testing.T) {
	src := BackupFormat{
		Meta:    BackupMeta{Project: db.Project{Name: "p"}},
		Runners: []BackupRunner{{Runner: db.Runner{Name: "r", Webhook: "https://example.com/hook"}}},
	}
	str, err := src.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, str, "example.com/hook")

	// A pre-tag backup file has the key; Unmarshal must ignore it. Marshal
	// uses json.MarshalIndent, hence the space after the colon.
	legacy := strings.Replace(str, `"name": "r"`, `"name": "r", "webhook": "https://example.com/hook"`, 1)
	require.NotEqual(t, str, legacy, "fixture did not inject the legacy key")

	var restored BackupFormat
	require.NoError(t, restored.Unmarshal(legacy))
	require.Len(t, restored.Runners, 1)
	assert.Equal(t, "r", restored.Runners[0].Name)
	assert.Empty(t, restored.Runners[0].Webhook)
}
