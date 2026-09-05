package tasks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveWithTaskMiddleware routes the request through TaskMiddleware so that
// mux route variables are populated the same way they are in the real router.
func serveWithTaskMiddleware(req *http.Request) (*httptest.ResponseRecorder, bool, int) {
	nextCalled := false
	seenTaskID := -1

	router := mux.NewRouter()
	router.Use(TaskMiddleware)
	router.HandleFunc("/api/tasks/{task_id}", func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		seenTaskID = helpers.GetFromContext(r, "task_id").(int)
		w.WriteHeader(http.StatusOK)
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w, nextCalled, seenTaskID
}

func TestTaskMiddleware_InvalidTaskIDStopsChain(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/not-a-number", nil)
	req.Header.Set("Accept", "application/json")

	w, nextCalled, _ := serveWithTaskMiddleware(req)

	assert.False(t, nextCalled, "handler must not run with an unparsed task_id")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, w.Body.String(), "GetIntParam already wrote the response")
}

func TestTaskMiddleware_InvalidTaskIDRedirectsBrowser(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/not-a-number", nil)
	req.Header.Set("Accept", "text/html")

	w, nextCalled, _ := serveWithTaskMiddleware(req)

	assert.False(t, nextCalled, "handler must not run with an unparsed task_id")
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "/404", w.Header().Get("Location"))
	assert.NotContains(t, w.Body.String(), "not-a-number",
		"the raw path segment must not be echoed back to the client")
}

func TestTaskMiddleware_ValidTaskIDReachesHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/42", nil)
	req.Header.Set("Accept", "application/json")

	w, nextCalled, seenTaskID := serveWithTaskMiddleware(req)

	require.True(t, nextCalled, "handler must run for a valid task_id")
	assert.Equal(t, 42, seenTaskID)
	assert.Equal(t, http.StatusOK, w.Code)
}
