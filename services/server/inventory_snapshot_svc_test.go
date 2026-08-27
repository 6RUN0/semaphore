package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockInventoryManagerForSnapshot struct {
	inventory db.Inventory
	refs      db.ObjectReferrers
}

func (m *mockInventoryManagerForSnapshot) GetInventory(projectID int, inventoryID int) (db.Inventory, error) {
	return m.inventory, nil
}

func (m *mockInventoryManagerForSnapshot) GetInventoryRefs(int, int) (db.ObjectReferrers, error) {
	return m.refs, nil
}

func (m *mockInventoryManagerForSnapshot) GetInventories(int, db.RetrieveQueryParams, []db.InventoryType) ([]db.Inventory, error) {
	return nil, nil
}

func (m *mockInventoryManagerForSnapshot) UpdateInventory(db.Inventory) error { return nil }

func (m *mockInventoryManagerForSnapshot) CreateInventory(inventory db.Inventory) (db.Inventory, error) {
	return inventory, nil
}

func (m *mockInventoryManagerForSnapshot) DeleteInventory(int, int) error { return nil }

type mockSnapshotManager struct {
	stored  *db.InventorySnapshot
	content db.InventorySnapshotContent
	writes  int

	claimed     bool
	claimTaskID int
	claims      int
}

func (m *mockSnapshotManager) GetInventorySnapshot(projectID int, inventoryID int) (db.InventorySnapshot, error) {
	if m.stored == nil {
		return db.InventorySnapshot{}, db.ErrNotFound
	}

	return *m.stored, nil
}

func (m *mockSnapshotManager) GetInventorySnapshotHosts(int, int, db.InventoryHostFilter) ([]db.InventorySnapshotHost, error) {
	return nil, nil
}

func (m *mockSnapshotManager) GetInventorySnapshotGroups(
	int, int, db.InventoryGroupFilter,
) ([]db.InventorySnapshotGroup, error) {
	return nil, nil
}

func (m *mockSnapshotManager) SetInventorySnapshot(snapshot db.InventorySnapshot, content db.InventorySnapshotContent) error {
	m.writes++
	m.stored = &snapshot
	m.content = content

	return nil
}

func (m *mockSnapshotManager) SetResolvedInventorySnapshot(
	snapshot db.InventorySnapshot,
	content db.InventorySnapshotContent,
	taskID int,
) (bool, error) {
	if m.stored == nil ||
		m.stored.Status != db.InventorySnapshotStatusPending ||
		m.stored.TaskID == nil ||
		*m.stored.TaskID != taskID {
		return false, nil
	}

	m.writes++
	m.stored = &snapshot
	m.content = content

	return true, nil
}

func (m *mockSnapshotManager) ClaimInventorySnapshotResolve(projectID int, inventoryID int) (bool, error) {
	m.claims++

	if m.stored != nil && m.stored.Status == db.InventorySnapshotStatusPending {
		return false, nil
	}

	m.claimed = true

	// Mirrors the store: a claim clears the fingerprint and the error but
	// leaves the host rows alone, and so the count that describes them.
	claim := db.InventorySnapshot{
		InventoryID: inventoryID,
		ProjectID:   projectID,
		Status:      db.InventorySnapshotStatusPending,
	}

	if m.stored != nil {
		claim.HostCount = m.stored.HostCount
	}

	m.stored = &claim

	return true, nil
}

func (m *mockSnapshotManager) AssignInventorySnapshotResolveTask(projectID int, inventoryID int, taskID int) error {
	m.claimTaskID = taskID

	if m.stored != nil && m.stored.Status == db.InventorySnapshotStatusPending && m.stored.TaskID == nil {
		m.stored.TaskID = &taskID
	}

	return nil
}

func (m *mockSnapshotManager) ReleaseInventorySnapshotResolve(projectID int, inventoryID int, message string) error {
	if m.stored != nil && m.stored.Status == db.InventorySnapshotStatusPending && m.stored.TaskID == nil {
		m.stored.Status = db.InventorySnapshotStatusError
		m.stored.Error = message
	}

	return nil
}

func (m *mockSnapshotManager) FailInventorySnapshotResolve(projectID int, inventoryID int, taskID int, message string) error {
	if m.stored != nil && m.stored.Status == db.InventorySnapshotStatusPending &&
		m.stored.TaskID != nil && *m.stored.TaskID == taskID {
		m.stored.Status = db.InventorySnapshotStatusError
		m.stored.Error = message
	}

	return nil
}

// The embedded interfaces are left nil: these tests only reach a static
// inventory, whose content is in the database, so any call into the template or
// repository stores would be a bug and panics rather than passing quietly.
type mockTemplateManager struct {
	db.TemplateManager

	template db.Template
	byID     map[int]db.Template
	err      error
}

func (m *mockTemplateManager) GetTemplate(projectID int, templateID int) (db.Template, error) {
	if m.err != nil {
		return db.Template{}, m.err
	}

	if m.byID != nil {
		template, ok := m.byID[templateID]
		if !ok {
			return db.Template{}, db.ErrNotFound
		}

		return template, nil
	}

	return m.template, nil
}

type mockRepositoryManager struct {
	db.RepositoryManager

	repository db.Repository
	err        error
}

func (m *mockRepositoryManager) GetRepository(projectID int, repositoryID int) (db.Repository, error) {
	return m.repository, m.err
}

func newSnapshotServiceUnderTest(inventory db.Inventory) (InventorySnapshotService, *mockSnapshotManager) {
	snapshots := &mockSnapshotManager{}
	return newSnapshotServiceWith(&mockInventoryManagerForSnapshot{inventory: inventory}, snapshots), snapshots
}

func newSnapshotServiceWith(
	inventoryRepo db.InventoryManager,
	snapshots db.InventorySnapshotManager,
) InventorySnapshotService {
	return NewInventorySnapshotService(
		inventoryRepo,
		snapshots,
		&mockTemplateManager{},
		&mockRepositoryManager{},
		nil,
	)
}

func staticInventory(content string) db.Inventory {
	return db.Inventory{
		ID:        7,
		ProjectID: 3,
		Type:      db.InventoryStatic,
		Inventory: content,
	}
}

func TestSnapshotIsBuiltOnFirstRead(t *testing.T) {
	service, snapshots := newSnapshotServiceUnderTest(staticInventory("[web]\nweb01\nweb02\n"))

	snapshot, err := service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 2, snapshot.HostCount)
	assert.Equal(t, 1, snapshots.writes)
	assert.Equal(t, []string{"web01", "web02"}, snapshots.content.Hosts)

	require.Len(t, snapshots.content.Groups, 1)
	assert.Equal(t, "web", snapshots.content.Groups[0].Name)
	assert.Equal(t, []string{"web01", "web02"}, snapshots.content.Groups[0].Hosts)
}

// Rebuilding on every read would reparse the inventory for every keystroke of
// a search box.
func TestSnapshotIsNotRebuiltWhileInventoryIsUnchanged(t *testing.T) {
	service, snapshots := newSnapshotServiceUnderTest(staticInventory("[web]\nweb01\n"))

	_, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	_, err = service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, snapshots.writes)
}

func TestSnapshotIsRebuiltWhenInventoryChanges(t *testing.T) {
	inventoryRepo := &mockInventoryManagerForSnapshot{inventory: staticInventory("[web]\nweb01\n")}
	snapshots := &mockSnapshotManager{}
	service := newSnapshotServiceWith(inventoryRepo, snapshots)

	_, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	inventoryRepo.inventory = staticInventory("[web]\nweb01\nweb02\n")

	snapshot, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, snapshots.writes)
	assert.Equal(t, 2, snapshot.HostCount)
}

// The same body means different hosts in the two static formats, so the format
// has to take part in the freshness check.
func TestSnapshotIsRebuiltWhenOnlyTheTypeChanges(t *testing.T) {
	inventoryRepo := &mockInventoryManagerForSnapshot{inventory: staticInventory("web:\n  hosts:\n    web01:\n")}
	snapshots := &mockSnapshotManager{}
	service := newSnapshotServiceWith(inventoryRepo, snapshots)

	_, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	changed := inventoryRepo.inventory
	changed.Type = db.InventoryStaticYaml
	inventoryRepo.inventory = changed

	snapshot, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, snapshots.writes)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, []string{"web01"}, snapshots.content.Hosts)
}

func TestRefreshRebuildsUnchangedSnapshot(t *testing.T) {
	service, snapshots := newSnapshotServiceUnderTest(staticInventory("[web]\nweb01\n"))

	_, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	_, err = service.Refresh(3, 7, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, snapshots.writes)
}

// A file, Terraform workspace or dynamic inventory is not a failure: the
// caller has to be able to tell it apart from an inventory with no hosts.
func TestSnapshotReportsUnsupportedInventoryTypes(t *testing.T) {
	for _, inventoryType := range []db.InventoryType{
		db.InventoryFile,
		db.InventoryTerraformWorkspace,
		db.InventoryTofuWorkspace,
		db.InventoryTerragruntWorkspace,
	} {
		t.Run(string(inventoryType), func(t *testing.T) {
			inventory := staticInventory("")
			inventory.Type = inventoryType

			service, snapshots := newSnapshotServiceUnderTest(inventory)

			snapshot, err := service.GetSnapshot(3, 7, nil)

			require.NoError(t, err)
			assert.Equal(t, db.InventorySnapshotStatusUnsupported, snapshot.Status)
			assert.Equal(t, 0, snapshot.HostCount)
			assert.Empty(t, snapshots.content.Hosts)

			// "unsupported" alone leaves a user with nothing to act on, so the
			// state carries the reason it could not be read.
			assert.NotEmpty(t, snapshot.Error)
		})
	}
}

// A broken inventory must clear the host list rather than leave the previous
// one to be shown as current.
func TestSnapshotReportsParseFailure(t *testing.T) {
	inventoryRepo := &mockInventoryManagerForSnapshot{inventory: staticInventory("[web]\nweb01\n")}
	snapshots := &mockSnapshotManager{}
	service := newSnapshotServiceWith(inventoryRepo, snapshots)

	_, err := service.GetSnapshot(3, 7, nil)
	require.NoError(t, err)

	inventoryRepo.inventory = staticInventory("[web]\nweb[9:1:0]\n")

	snapshot, err := service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Contains(t, snapshot.Error, "step must be a positive number")
	assert.Equal(t, 0, snapshot.HostCount)
	assert.Empty(t, snapshots.content.Hosts)
	assert.Empty(t, snapshots.content.Groups)
}

// The column is narrower than a parse error can be.
func TestSnapshotErrorIsTruncatedToColumnWidth(t *testing.T) {
	longKey := strings.Repeat("a", 4096)

	service, _ := newSnapshotServiceUnderTest(staticInventory("[web]\nweb[" + longKey + ":1]\n"))

	snapshot, err := service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.LessOrEqual(t, len(snapshot.Error), db.MaxInventorySnapshotErrorLength)
}

// Storing a truncated name would silently point a run at a different host.
func TestSnapshotRefusesNamesWiderThanTheColumn(t *testing.T) {
	longHost := strings.Repeat("h", db.MaxInventoryHostNameLength+1)

	service, snapshots := newSnapshotServiceUnderTest(staticInventory("[web]\n" + longHost + "\n"))

	snapshot, err := service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusError, snapshot.Status)
	assert.Contains(t, snapshot.Error, "longer than")
	assert.Empty(t, snapshots.content.Hosts)
}

// validateSnapshotBounds is db_lib's parsing-time budget re-checked against
// whatever content actually reaches the snapshot tables, the same way
// StoreResolved re-checks it against a runner's report. This exercises that
// second line of defence directly, since db_lib's own budget already refuses
// to hand back content this large through the honest parsing path.
func TestValidateSnapshotBoundsRefusesTooManyGroups(t *testing.T) {
	groups := make([]db.InventorySnapshotGroupContent, db.MaxInventorySnapshotGroups+1)
	for i := range groups {
		groups[i] = db.InventorySnapshotGroupContent{Name: fmt.Sprintf("g%d", i)}
	}

	err := validateSnapshotBounds(db.InventorySnapshotContent{Groups: groups})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than")
	assert.Contains(t, err.Error(), "groups")
}

func TestValidateSnapshotBoundsRefusesTooManyMemberships(t *testing.T) {
	hosts := make([]string, 100)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d", i)
	}

	groupCount := db.MaxInventorySnapshotMemberships/len(hosts) + 1
	groups := make([]db.InventorySnapshotGroupContent, groupCount)
	for i := range groups {
		groups[i] = db.InventorySnapshotGroupContent{Name: fmt.Sprintf("g%d", i), Hosts: hosts}
	}

	err := validateSnapshotBounds(db.InventorySnapshotContent{Hosts: hosts, Groups: groups})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than")
	assert.Contains(t, err.Error(), "memberships")
}

func TestSnapshotAcceptsNameAtColumnWidth(t *testing.T) {
	host := strings.Repeat("h", db.MaxInventoryHostNameLength)

	service, _ := newSnapshotServiceUnderTest(staticInventory("[web]\n" + host + "\n"))

	snapshot, err := service.GetSnapshot(3, 7, nil)

	require.NoError(t, err)
	assert.Equal(t, db.InventorySnapshotStatusOK, snapshot.Status)
	assert.Equal(t, 1, snapshot.HostCount)
}
