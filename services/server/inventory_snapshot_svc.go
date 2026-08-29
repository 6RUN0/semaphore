package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/semaphoreui/semaphore/pkg/keylock"
	"github.com/semaphoreui/semaphore/pkg/tz"
	log "github.com/sirupsen/logrus"
)

// InventorySnapshotService serves the cached host list of an inventory and
// keeps it in step with the inventory it is derived from.
// templateID is optional throughout: it is only consulted for a file inventory
// that names no repository of its own, where the path is relative to the
// repository of the template about to run it.
type InventorySnapshotService interface {
	GetSnapshot(projectID int, inventoryID int, templateID *int) (db.InventorySnapshot, error)
	GetHosts(projectID int, inventoryID int, templateID *int, filter db.InventoryHostFilter) ([]db.InventorySnapshotHost, error)
	GetGroups(projectID int, inventoryID int, templateID *int, filter db.InventoryGroupFilter) ([]db.InventorySnapshotGroup, error)

	// Refresh rebuilds the snapshot even when the inventory has not changed.
	Refresh(projectID int, inventoryID int, templateID *int) (db.InventorySnapshot, error)

	// StoreResolved records a host list produced by executing a dynamic
	// inventory on a runner. The server cannot obtain one itself — running a
	// program out of a repository is a runner's job — so for those inventories
	// this is the only way a snapshot is ever written.
	StoreResolved(projectID int, inventoryID int, taskID int, content db.InventorySnapshotContent) error
}

func NewInventorySnapshotService(
	inventoryRepo db.InventoryManager,
	snapshotRepo db.InventorySnapshotManager,
	templateRepo db.TemplateManager,
	repositoryRepo db.RepositoryManager,
	keyInstaller db_lib.AccessKeyInstaller,
) InventorySnapshotService {
	return &InventorySnapshotServiceImpl{
		inventoryRepo: inventoryRepo,
		snapshotRepo:  snapshotRepo,
		locator: &inventorySourceLocator{
			inventoryRepo:  inventoryRepo,
			templateRepo:   templateRepo,
			repositoryRepo: repositoryRepo,
			keyInstaller:   keyInstaller,
			checkoutLock:   &keylock.KeyLock{},

			remoteHeadFetchLock: &keylock.KeyLock{},
		},
	}
}

type InventorySnapshotServiceImpl struct {
	inventoryRepo db.InventoryManager
	snapshotRepo  db.InventorySnapshotManager
	locator       *inventorySourceLocator
}

func (s *InventorySnapshotServiceImpl) GetSnapshot(
	projectID int,
	inventoryID int,
	templateID *int,
) (db.InventorySnapshot, error) {
	return s.ensureFresh(projectID, inventoryID, templateID, false)
}

func (s *InventorySnapshotServiceImpl) GetHosts(
	projectID int,
	inventoryID int,
	templateID *int,
	filter db.InventoryHostFilter,
) ([]db.InventorySnapshotHost, error) {

	if _, err := s.ensureFresh(projectID, inventoryID, templateID, false); err != nil {
		return nil, err
	}

	return s.snapshotRepo.GetInventorySnapshotHosts(projectID, inventoryID, filter)
}

func (s *InventorySnapshotServiceImpl) GetGroups(
	projectID int,
	inventoryID int,
	templateID *int,
	filter db.InventoryGroupFilter,
) ([]db.InventorySnapshotGroup, error) {

	if _, err := s.ensureFresh(projectID, inventoryID, templateID, false); err != nil {
		return nil, err
	}

	return s.snapshotRepo.GetInventorySnapshotGroups(projectID, inventoryID, filter)
}

func (s *InventorySnapshotServiceImpl) Refresh(
	projectID int,
	inventoryID int,
	templateID *int,
) (db.InventorySnapshot, error) {
	return s.ensureFresh(projectID, inventoryID, templateID, true)
}

// ensureFresh rebuilds the snapshot when it is missing or was built from
// different inventory content.
//
// Synchronisation happens on read rather than on save so that no scheduler is
// involved: a background job would have to be deduplicated across nodes, and
// the deduplicator is not implemented in this build. Two nodes rebuilding the
// same snapshot at once write identical derived rows, so the race is harmless.
func (s *InventorySnapshotServiceImpl) ensureFresh(
	projectID int,
	inventoryID int,
	templateID *int,
	force bool,
) (db.InventorySnapshot, error) {

	inventory, err := s.inventoryRepo.GetInventory(projectID, inventoryID)
	if err != nil {
		return db.InventorySnapshot{}, err
	}

	stored, err := s.snapshotRepo.GetInventorySnapshot(projectID, inventoryID)

	hasStored := false

	switch {
	case err == nil:
		hasStored = true
	case errors.Is(err, db.ErrNotFound):
		// Never synchronised, fall through and build it.
	default:
		return db.InventorySnapshot{}, err
	}

	if hasStored && resolveAnswersAsIs(stored, force) {
		return stored, nil
	}

	source, err := s.locator.locate(inventory, templateID, force)

	if err != nil {
		if isStructuralSourceError(err) {
			structuralHash := inventorySourceHash(inventory, inventorySource{})

			// The same unreadable inventory, failing the same way: rewriting
			// the row on every read would turn a GET any project member may
			// issue into a write, so the stored answer stands as is.
			if hasStored &&
				stored.SourceHash == structuralHash &&
				stored.Status == db.InventorySnapshotStatusUnsupported {
				return stored, nil
			}

			return s.write(projectID, inventoryID, structuralHash, err, emptySnapshotContent())
		}

		if hasStored {
			// The repository could not be reached this time. A host list that
			// was good a moment ago is worth more than an error, so the stored
			// one stands until the next attempt succeeds.
			return stored, nil
		}

		return s.write(projectID, inventoryID, inventorySourceHash(inventory, inventorySource{}), err, emptySnapshotContent())
	}

	hash := inventorySourceHash(inventory, source)

	if hasStored && !force && stored.SourceHash == hash {
		return stored, nil
	}

	// Everything below rebuilds, so it runs under the lock of the checkout
	// directory the rebuild reads from.
	unlock := s.locator.lock(inventory, source)
	defer unlock()

	if fresh, freshErr := s.snapshotRepo.GetInventorySnapshot(projectID, inventoryID); freshErr == nil {
		// Read once more now that the lock is held: N concurrent readers of the
		// same stale snapshot queue here, and without this second look every
		// one of them would check the repository out and rewrite four tables
		// with the rows the first one already wrote.
		stored, hasStored = fresh, true

		if resolveAnswersAsIs(stored, force) {
			return stored, nil
		}

		if !force && stored.SourceHash == hash {
			return stored, nil
		}
	}

	content, err := s.readContent(inventory, source)

	var executable db_lib.InventoryExecutableError

	if errors.As(err, &executable) && force {
		// Only a runner may list these hosts. Saying so lets the caller decide
		// whether it is allowed to ask for one, and leaves whatever is stored
		// untouched in the meantime.
		return stored, InventoryNeedsRunnerError{InventoryID: inventoryID}
	}

	if errors.As(err, &executable) && hasStored && stored.TaskID != nil {
		// The hosts of a dynamic inventory exist only once a runner has been
		// asked for them, and the server is never allowed to ask itself. The
		// stored list is what the last resolve produced, and replacing it with
		// "unsupported" would throw away the only answer there is.
		//
		// A task id is what marks the row as a resolve's, and it survives the
		// claim taken for the next one: the claim clears the fingerprint and
		// moves the status off "ok", so a status check here would stop
		// recognising exactly the snapshot whose host rows are still on disk.
		if stored.Status == db.InventorySnapshotStatusPending {
			// The claim outlived its task — the guard at the top of this
			// function refused to keep waiting for it. Settle it, so the
			// inventory stops reporting "resolving" for good, but leave the
			// host rows where they are.
			return s.settleExpiredResolve(projectID, inventoryID, stored)
		}

		return stored, nil
	}

	return s.write(projectID, inventoryID, hash, err, content)
}

// resolveAnswersAsIs reports a stored snapshot that belongs to a runner's
// resolve and that no rebuild here can improve on.
func resolveAnswersAsIs(stored db.InventorySnapshot, force bool) bool {
	// A resolve a runner is still working on is the freshest answer there is.
	// Once it has been pending too long the task behind it is gone, so the
	// claim is treated as expired and the normal path runs again.
	if stored.Status == db.InventorySnapshotStatusPending &&
		tz.Now().Sub(stored.Created) < db.MaxInventoryResolveAge {
		return true
	}

	// A snapshot a runner resolved has no source fingerprint to compare: its
	// hosts follow the cloud, not the repository, and only another resolve can
	// produce a fresher answer. For a plain read it is always current —
	// without this, every read would check the repository out only to
	// rediscover that the inventory is executable and fall back to this very
	// snapshot.
	return !force &&
		stored.Status == db.InventorySnapshotStatusOK &&
		stored.TaskID != nil
}

// settleExpiredResolve moves a claim whose task never reported onto "error",
// so that a reader is told the resolve failed instead of being shown
// "resolving" forever. The host rows of the previous resolve are left alone:
// they are the only host list this inventory has.
func (s *InventorySnapshotServiceImpl) settleExpiredResolve(
	projectID int,
	inventoryID int,
	stored db.InventorySnapshot,
) (db.InventorySnapshot, error) {

	const message = "the runner did not report a host list before the resolve expired"

	// The store scopes this to the claim's own task, so a claim some later
	// refresh has already taken over stays with its new owner. Two nodes
	// settling the same expired claim write the same row.
	if err := s.snapshotRepo.FailInventorySnapshotResolve(
		projectID, inventoryID, *stored.TaskID, message,
	); err != nil {
		return db.InventorySnapshot{}, err
	}

	stored.Status = db.InventorySnapshotStatusError
	stored.Error = message

	return stored, nil
}

func (s *InventorySnapshotServiceImpl) StoreResolved(
	projectID int,
	inventoryID int,
	taskID int,
	content db.InventorySnapshotContent,
) error {

	if content.Hosts == nil {
		content.Hosts = []string{}
	}

	if content.Groups == nil {
		content.Groups = []db.InventorySnapshotGroupContent{}
	}

	// Every bound is re-checked here even though the honest path already
	// enforces them while parsing: this content is a runner's report, and a
	// runner is trusted to run code, not to write unlimited rows into four
	// tables.
	if len(content.Hosts) > db.MaxInventorySnapshotHosts {
		return fmt.Errorf("resolved inventory holds more than %d hosts", db.MaxInventorySnapshotHosts)
	}

	if len(content.Groups) > db.MaxInventorySnapshotGroups {
		return fmt.Errorf("resolved inventory holds more than %d groups", db.MaxInventorySnapshotGroups)
	}

	memberships := 0
	for _, group := range content.Groups {
		memberships += len(group.Hosts)
	}

	if memberships > db.MaxInventorySnapshotMemberships {
		return fmt.Errorf("resolved inventory holds more than %d group memberships", db.MaxInventorySnapshotMemberships)
	}

	content = dedupeSnapshotContent(content)

	if err := validateSnapshotNames(content); err != nil {
		return err
	}

	snapshot := db.InventorySnapshot{
		InventoryID: inventoryID,
		ProjectID:   projectID,
		Status:      db.InventorySnapshotStatusOK,
		HostCount:   len(content.Hosts),
		Created:     tz.Now(),
		TaskID:      &taskID,

		// A dynamic inventory has no fingerprint that means "still current":
		// the hosts follow the cloud, not the repository. Keying the hash to
		// the resolve that produced it keeps every resolve a distinct state and
		// keeps a read from ever mistaking this for something it could rebuild.
		SourceHash: resolvedSourceHash(taskID),
	}

	written, err := s.snapshotRepo.SetResolvedInventorySnapshot(snapshot, content, taskID)
	if err != nil {
		return err
	}

	if !written {
		// The claim this report was settling expired and a later refresh took
		// it over before this write landed. The play that produced this host
		// list still ran successfully, so this is not the task's error — only
		// its result is discarded in favor of whatever is current now.
		log.WithFields(log.Fields{
			"task_id":      taskID,
			"inventory_id": inventoryID,
			"project_id":   projectID,
			"context":      "inventory_snapshot",
		}).Info("resolved inventory report arrived after its claim was taken over, discarding")
	}

	return nil
}

// dedupeSnapshotContent drops names a report repeats. Every snapshot table is
// keyed by name, so a repeated one is a primary key violation — and the store
// cannot tell that apart from two writers colliding, so it would sleep and
// retry three times before failing the report. The honest parsing path cannot
// produce duplicates; a runner's report is not the honest path.
//
// The first occurrence of a name wins, which keeps the order the report chose.
func dedupeSnapshotContent(content db.InventorySnapshotContent) db.InventorySnapshotContent {
	deduped := db.InventorySnapshotContent{
		Hosts:  dedupeNames(content.Hosts),
		Groups: make([]db.InventorySnapshotGroupContent, 0, len(content.Groups)),
	}

	seen := make(map[string]bool, len(content.Groups))

	for _, group := range content.Groups {
		if seen[group.Name] {
			continue
		}

		seen[group.Name] = true

		deduped.Groups = append(deduped.Groups, db.InventorySnapshotGroupContent{
			Name:  group.Name,
			Hosts: dedupeNames(group.Hosts),
		})
	}

	return deduped
}

func dedupeNames(names []string) []string {
	deduped := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))

	for _, name := range names {
		if seen[name] {
			continue
		}

		seen[name] = true
		deduped = append(deduped, name)
	}

	return deduped
}

func resolvedSourceHash(taskID int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("resolved-by-task:%d", taskID)))

	return hex.EncodeToString(sum[:])
}

// write records the outcome of a synchronisation, mapping the failure to the
// state that describes it.
func (s *InventorySnapshotServiceImpl) write(
	projectID int,
	inventoryID int,
	hash string,
	cause error,
	content db.InventorySnapshotContent,
) (db.InventorySnapshot, error) {

	snapshot := db.InventorySnapshot{
		InventoryID: inventoryID,
		ProjectID:   projectID,
		SourceHash:  hash,
		Created:     tz.Now(),
		HostCount:   len(content.Hosts),
	}

	switch {
	case cause == nil:
		snapshot.Status = db.InventorySnapshotStatusOK
	case isUnsupportedSourceError(cause):
		snapshot.Status = db.InventorySnapshotStatusUnsupported
		snapshot.Error = db.TruncateSnapshotError(cause.Error())
	default:
		snapshot.Status = db.InventorySnapshotStatusError
		snapshot.Error = db.TruncateSnapshotError(cause.Error())
	}

	if err := s.snapshotRepo.SetInventorySnapshot(snapshot, content); err != nil {
		return db.InventorySnapshot{}, err
	}

	return snapshot, nil
}

// isUnsupportedSourceError separates "this inventory has no host list the
// server can read" from "reading it failed". The first is a state the picker
// explains and falls back from; the second is a fault worth showing.
func isUnsupportedSourceError(err error) bool {
	var notParseable InventoryNotParseableError
	var ambiguous InventoryAmbiguousRepositoryError
	var executable db_lib.InventoryExecutableError

	return errors.As(err, &notParseable) ||
		errors.As(err, &ambiguous) ||
		errors.As(err, &executable)
}

// isStructuralSourceError reports a failure that repeating will not fix, and so
// one that should replace a stored snapshot rather than be waited out.
func isStructuralSourceError(err error) bool {
	var notParseable InventoryNotParseableError
	var ambiguous InventoryAmbiguousRepositoryError

	return errors.As(err, &notParseable) || errors.As(err, &ambiguous)
}

// InventoryNeedsRunnerError reports an inventory whose hosts can only be listed
// by executing a program, which is a runner's job. It is a request for the
// caller to dispatch one, not a failure.
type InventoryNeedsRunnerError struct {
	InventoryID int
}

func (e InventoryNeedsRunnerError) Error() string {
	return fmt.Sprintf("inventory %d can only be listed by a runner", e.InventoryID)
}

// InventoryNotParseableError marks an inventory whose hosts cannot be
// enumerated without running something, which is a supported state rather than
// a failure.
type InventoryNotParseableError struct {
	Type db.InventoryType
}

func (e InventoryNotParseableError) Error() string {
	return fmt.Sprintf("inventory of type %s holds no host list the server can read", e.Type)
}

func emptySnapshotContent() db.InventorySnapshotContent {
	return db.InventorySnapshotContent{
		Hosts:  []string{},
		Groups: []db.InventorySnapshotGroupContent{},
	}
}

// readContent turns a located inventory into the rows of a snapshot. It always
// returns usable content, empty when the inventory could not be read, so that a
// failed synchronisation clears a stale host list instead of leaving it to be
// shown as current.
func (s *InventorySnapshotServiceImpl) readContent(
	inventory db.Inventory,
	source inventorySource,
) (db.InventorySnapshotContent, error) {

	empty := emptySnapshotContent()

	parsed, err := s.locator.read(inventory, source)
	if err != nil {
		return empty, err
	}

	content := db.InventorySnapshotContent{
		Hosts:  parsed.Hosts,
		Groups: make([]db.InventorySnapshotGroupContent, 0, len(parsed.Groups)),
	}

	for _, group := range parsed.Groups {
		content.Groups = append(content.Groups, db.InventorySnapshotGroupContent{
			Name:  group.Name,
			Hosts: group.Hosts,
		})
	}

	if err = validateSnapshotBounds(content); err != nil {
		return empty, err
	}

	if err = validateSnapshotNames(content); err != nil {
		return empty, err
	}

	return content, nil
}

// validateSnapshotBounds mirrors the caps StoreResolved enforces against a
// runner's report. db_lib already enforces the same caps while parsing, so
// this is a second line of defence rather than the only one: the bound
// belongs to the snapshot tables themselves, not to whichever code path
// happens to produce the content that fills them, and the two must not be
// able to drift apart.
func validateSnapshotBounds(content db.InventorySnapshotContent) error {
	if len(content.Hosts) > db.MaxInventorySnapshotHosts {
		return fmt.Errorf("inventory holds more than %d hosts", db.MaxInventorySnapshotHosts)
	}

	if len(content.Groups) > db.MaxInventorySnapshotGroups {
		return fmt.Errorf("inventory holds more than %d groups", db.MaxInventorySnapshotGroups)
	}

	memberships := 0
	for _, group := range content.Groups {
		memberships += len(group.Hosts)
	}

	if memberships > db.MaxInventorySnapshotMemberships {
		return fmt.Errorf("inventory holds more than %d group memberships", db.MaxInventorySnapshotMemberships)
	}

	return nil
}

// validateSnapshotNames refuses a snapshot carrying a name the tables cannot
// hold. Storing a truncated name would silently target the wrong host.
func validateSnapshotNames(content db.InventorySnapshotContent) error {
	// Lengths are measured in runes because varchar(255) counts characters in
	// MySQL and PostgreSQL, not bytes; a byte count would reject non-ASCII
	// names that actually fit the column.
	for _, host := range content.Hosts {
		if utf8.RuneCountInString(host) > db.MaxInventoryHostNameLength {
			return fmt.Errorf("host name is longer than %d characters", db.MaxInventoryHostNameLength)
		}
	}

	for _, group := range content.Groups {
		if utf8.RuneCountInString(group.Name) > db.MaxInventoryHostNameLength {
			return fmt.Errorf("group name is longer than %d characters", db.MaxInventoryHostNameLength)
		}

		// Group membership names their own hosts independently of the top
		// list, so they need their own check: a truncated name in a membership
		// row would silently target the wrong host just the same.
		for _, host := range group.Hosts {
			if utf8.RuneCountInString(host) > db.MaxInventoryHostNameLength {
				return fmt.Errorf("host name is longer than %d characters", db.MaxInventoryHostNameLength)
			}
		}
	}

	return nil
}

// inventorySourceHash identifies the content a snapshot was built from.
//
// The type takes part in it, so that switching an inventory between the INI and
// YAML formats without editing its body still rebuilds the snapshot. So does
// the source fingerprint, which for a file inventory carries the repository and
// its current commit — that is what lets two templates pointing at the same
// repository share one snapshot while two pointing at different ones do not.
func inventorySourceHash(inventory db.Inventory, source inventorySource) string {
	sum := sha256.Sum256([]byte(string(inventory.Type) + "\n" + source.fingerprint))

	return hex.EncodeToString(sum[:])
}
