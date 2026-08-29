package helpers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/common_errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unclassified error is the server's failure, not the caller's. Answering
// 400 with an empty body told a client that its request was wrong and that
// retrying was pointless, for a database that had simply gone away.
func TestWriteErrorReportsAnUnclassifiedErrorAsServerFailure(t *testing.T) {
	w := httptest.NewRecorder()

	WriteError(w, errors.New("database is gone"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotEmpty(t, body["error"], "a client needs something to report")

	// The message must not leak the internals it was built from.
	assert.NotContains(t, w.Body.String(), "database is gone")
}

func TestWriteErrorKeepsTheClassifiedCodes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{"not found", db.ErrNotFound, http.StatusNotFound},
		{"invalid operation", db.ErrInvalidOperation, http.StatusConflict},
		{"validation", common_errors.NewValidationError("bad input"), http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()

			WriteError(w, tt.err)

			assert.Equal(t, tt.expected, w.Code)
		})
	}
}
