package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInventorySnapshotService records what the controller asked for, so that a
// test can assert on the arguments rather than on a database.
type mockInventorySnapshotService struct {
	snapshot db.InventorySnapshot
	hosts    []db.InventorySnapshotHost
	groups   []db.InventorySnapshotGroup
	err      error

	// refreshErr, when set, is returned by Refresh alone, so a test can drive
	// the dispatch path (Refresh fails with needs-runner) while the reads keep
	// working.
	refreshErr error

	gotProjectID   int
	gotInventoryID int
	gotTemplateID  *int
	gotFilter      db.InventoryHostFilter
	gotGroupFilter db.InventoryGroupFilter
	refreshed      bool
}

func (m *mockInventorySnapshotService) GetSnapshot(projectID int, inventoryID int, templateID *int) (db.InventorySnapshot, error) {
	m.gotProjectID, m.gotInventoryID, m.gotTemplateID = projectID, inventoryID, templateID
	return m.snapshot, m.err
}

func (m *mockInventorySnapshotService) GetHosts(projectID int, inventoryID int, templateID *int, filter db.InventoryHostFilter) ([]db.InventorySnapshotHost, error) {
	m.gotProjectID, m.gotInventoryID, m.gotTemplateID, m.gotFilter = projectID, inventoryID, templateID, filter
	return m.hosts, m.err
}

func (m *mockInventorySnapshotService) GetGroups(
	projectID int,
	inventoryID int,
	templateID *int,
	filter db.InventoryGroupFilter,
) ([]db.InventorySnapshotGroup, error) {
	m.gotProjectID, m.gotInventoryID, m.gotTemplateID = projectID, inventoryID, templateID
	m.gotGroupFilter = filter

	return m.groups, m.err
}

func (m *mockInventorySnapshotService) Refresh(projectID int, inventoryID int, templateID *int) (db.InventorySnapshot, error) {
	m.gotProjectID, m.gotInventoryID, m.gotTemplateID, m.refreshed = projectID, inventoryID, templateID, true

	if m.refreshErr != nil {
		return m.snapshot, m.refreshErr
	}

	return m.snapshot, m.err
}

// StoreResolved is never reached through a controller: a runner reports a
// resolved host list on the progress endpoint, not here.
func (m *mockInventorySnapshotService) StoreResolved(int, int, int, db.InventorySnapshotContent) error {
	panic("the controller must not store a resolved inventory")
}

// newSnapshotRequest puts the inventory into the context the way
// InventoryMiddleware does, and deliberately points the URL at a different
// project and inventory so that a handler reading either from the path is
// caught.
func newSnapshotRequest(method string, target string, inventory db.Inventory) (*http.Request, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, target, nil)
	req = helpers.SetContextValue(req, "inventory", inventory)
	return req, httptest.NewRecorder()
}

func contextInventory() db.Inventory {
	return db.Inventory{ID: 7, ProjectID: 3}
}

// The URL carries project 999 and inventory 888; the context carries project 3
// and inventory 7. Only the context has been checked against the caller's
// membership, so a handler that trusted the path would serve another project's
// hosts.
func TestInventorySnapshot_IgnoresIdsInThePath(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(c *InventorySnapshotController, w http.ResponseWriter, r *http.Request)
	}{
		{"snapshot", (*InventorySnapshotController).GetInventorySnapshot},
		{"hosts", (*InventorySnapshotController).GetInventorySnapshotHosts},
		{"groups", (*InventorySnapshotController).GetInventorySnapshotGroups},
		{"refresh", (*InventorySnapshotController).RefreshInventorySnapshot},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &mockInventorySnapshotService{}
			ctrl := NewInventorySnapshotController(svc)

			req, w := newSnapshotRequest(
				http.MethodGet,
				"/api/project/999/inventory/888/snapshot",
				contextInventory(),
			)

			tt.call(ctrl, w, req)

			assert.Equal(t, 3, svc.gotProjectID, "project must come from the context inventory")
			assert.Equal(t, 7, svc.gotInventoryID, "inventory must come from the context inventory")
		})
	}
}

func TestGetInventorySnapshot_ReturnsTheSnapshot(t *testing.T) {
	svc := &mockInventorySnapshotService{
		snapshot: db.InventorySnapshot{
			InventoryID: 7,
			ProjectID:   3,
			Status:      db.InventorySnapshotStatusOK,
			HostCount:   12,
		},
	}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newSnapshotRequest(http.MethodGet, "/snapshot", contextInventory())
	ctrl.GetInventorySnapshot(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var got db.InventorySnapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, db.InventorySnapshotStatusOK, got.Status)
	assert.Equal(t, 12, got.HostCount)
}

// The source hash is an implementation detail of the sync, and publishing it
// would leak a fingerprint of the inventory content to every project member.
func TestGetInventorySnapshot_HidesTheSourceHash(t *testing.T) {
	svc := &mockInventorySnapshotService{
		snapshot: db.InventorySnapshot{SourceHash: "deadbeef"},
	}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newSnapshotRequest(http.MethodGet, "/snapshot", contextInventory())
	ctrl.GetInventorySnapshot(w, req)

	assert.NotContains(t, w.Body.String(), "deadbeef")
	assert.NotContains(t, w.Body.String(), "source_hash")
}

// An empty result has to serialise as [] and never as null: the picker calls
// .length on it, and null would blank the list instead of showing "no hosts".
func TestInventorySnapshot_EmptyListsSerialiseAsArrays(t *testing.T) {
	t.Run("hosts", func(t *testing.T) {
		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{hosts: nil})

		req, w := newSnapshotRequest(http.MethodGet, "/snapshot/hosts", contextInventory())
		ctrl.GetInventorySnapshotHosts(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, `[]`, w.Body.String())
	})

	t.Run("groups", func(t *testing.T) {
		ctrl := NewInventorySnapshotController(&mockInventorySnapshotService{groups: nil})

		req, w := newSnapshotRequest(http.MethodGet, "/snapshot/groups", contextInventory())
		ctrl.GetInventorySnapshotGroups(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, `[]`, w.Body.String())
	})
}

func TestGetInventorySnapshotHosts_PassesTheFilter(t *testing.T) {
	svc := &mockInventorySnapshotService{}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newSnapshotRequest(
		http.MethodGet,
		"/snapshot/hosts?group=web&search=web0&limit=50&offset=100",
		contextInventory(),
	)
	ctrl.GetInventorySnapshotHosts(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, db.InventoryHostFilter{
		Group:  "web",
		Search: "web0",
		Limit:  50,
		Offset: 100,
	}, svc.gotFilter)
}

func TestGetInventorySnapshotHosts_UnfilteredByDefault(t *testing.T) {
	svc := &mockInventorySnapshotService{}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newSnapshotRequest(http.MethodGet, "/snapshot/hosts", contextInventory())
	ctrl.GetInventorySnapshotHosts(w, req)

	assert.Equal(t, db.InventoryHostFilter{}, svc.gotFilter)
}

// The template only matters for a file inventory that names no repository, so
// it is optional; passing it through unchanged is what lets the service resolve
// the path against the repository of the template about to run.
func TestInventorySnapshot_PassesTheTemplate(t *testing.T) {
	for _, tt := range []struct {
		name     string
		query    string
		expected *int
	}{
		{"absent", "", nil},
		{"given", "?template_id=42", intPtr(42)},
		{"zero is no template", "?template_id=0", nil},
		{"negative is no template", "?template_id=-1", nil},
		{"malformed is no template", "?template_id=abc", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &mockInventorySnapshotService{}
			ctrl := NewInventorySnapshotController(svc)

			req, w := newSnapshotRequest(http.MethodGet, "/snapshot/hosts"+tt.query, contextInventory())
			ctrl.GetInventorySnapshotHosts(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, tt.expected, svc.gotTemplateID)
		})
	}
}

func TestRefreshInventorySnapshot_ForcesARebuild(t *testing.T) {
	svc := &mockInventorySnapshotService{}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newSnapshotRequest(http.MethodPost, "/snapshot/refresh", contextInventory())
	ctrl.RefreshInventorySnapshot(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, svc.refreshed, "refresh must not be served from the stored snapshot")
}

func TestInventorySnapshot_ReportsServiceFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		expected int
	}{
		{"missing inventory", db.ErrNotFound, http.StatusNotFound},
		// An unclassified error is the server's failure, not the request's:
		// answering 400 told a client to fix its input and not to retry a
		// database that had merely gone away.
		{"unexpected failure", errors.New("database is gone"), http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &mockInventorySnapshotService{err: tt.err}
			ctrl := NewInventorySnapshotController(svc)

			req, w := newSnapshotRequest(http.MethodGet, "/snapshot/hosts", contextInventory())
			ctrl.GetInventorySnapshotHosts(w, req)

			assert.Equal(t, tt.expected, w.Code)
		})
	}
}

// A page size the store cannot make sense of must fall back to the default
// rather than reach the query, where a negative offset or an overflowing limit
// would be a syntax error on some dialects.
func TestQueryInt(t *testing.T) {
	for _, tt := range []struct {
		name     string
		query    string
		expected int
	}{
		{"absent", "", 0},
		{"empty", "limit=", 0},
		{"not a number", "limit=abc", 0},
		{"negative", "limit=-5", 0},
		{"zero", "limit=0", 0},
		{"positive", "limit=42", 42},
		{"wider than int64", "limit=99999999999999999999", 0},
		{"float", "limit=1.5", 0},
		// "+" is a space once the query string is decoded, so a client that
		// signs its number gets the default rather than the number.
		{"signed", "limit=%2B7", 7},
		{"plus is a space", "limit=+7", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/hosts?"+tt.query, nil)

			assert.Equal(t, tt.expected, queryInt(req, "limit"))
		})
	}
}

func TestGetInventorySnapshotGroups_PassesThePaging(t *testing.T) {
	svc := &mockInventorySnapshotService{}
	ctrl := NewInventorySnapshotController(svc)

	req, w := newSnapshotRequest(
		http.MethodGet,
		"/snapshot/groups?limit=50&offset=100",
		contextInventory(),
	)
	ctrl.GetInventorySnapshotGroups(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, db.InventoryGroupFilter{Limit: 50, Offset: 100}, svc.gotGroupFilter)
}

// Neither parameter can name anything wider than a host name column, so a
// megabyte of either is a megabyte the database would compare against every
// row for nothing.
func TestGetInventorySnapshotHosts_CutsOversizedFilters(t *testing.T) {
	svc := &mockInventorySnapshotService{}
	ctrl := NewInventorySnapshotController(svc)

	// Multi-byte on purpose: the column counts characters, and a byte cut would
	// split one of these into a fragment that matches nothing at all.
	oversized := strings.Repeat("щ", db.MaxInventoryHostNameLength+50)

	req, w := newSnapshotRequest(
		http.MethodGet,
		"/snapshot/hosts?group="+url.QueryEscape(oversized)+"&search="+url.QueryEscape(oversized),
		contextInventory(),
	)
	ctrl.GetInventorySnapshotHosts(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, db.MaxInventoryHostNameLength, utf8.RuneCountInString(svc.gotFilter.Group))
	assert.Equal(t, db.MaxInventoryHostNameLength, utf8.RuneCountInString(svc.gotFilter.Search))
	assert.True(t, utf8.ValidString(svc.gotFilter.Search), "cutting must not split a character")
}

func TestGetInventorySnapshotHosts_KeepsFiltersThatFit(t *testing.T) {
	svc := &mockInventorySnapshotService{}
	ctrl := NewInventorySnapshotController(svc)

	name := strings.Repeat("щ", db.MaxInventoryHostNameLength)

	req, w := newSnapshotRequest(
		http.MethodGet,
		"/snapshot/hosts?search="+url.QueryEscape(name),
		contextInventory(),
	)
	ctrl.GetInventorySnapshotHosts(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, name, svc.gotFilter.Search)
}
