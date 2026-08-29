package server

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/semaphoreui/semaphore/pkg/keylock"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
)

// inventorySource is a located host list, together with the fingerprint that
// decides whether a stored snapshot is still current.
//
// Locating is deliberately split from reading: a file inventory lives in a
// repository, and asking git for the current commit costs one round trip with
// no transfer, while fetching the content costs a clone. So the fingerprint is
// obtained first, and the checkout only happens once it has proved the snapshot
// is out of date.
type inventorySource struct {
	// inline holds the content of a static inventory, which is stored in the
	// database rather than in a repository.
	inline string

	// repository and relPath locate a file inventory. repository is nil for an
	// inline one.
	repository *db.Repository
	relPath    string

	fingerprint string
}

func (s inventorySource) isInline() bool {
	return s.repository == nil
}

// remoteHeadCacheTTL bounds how long a fetched remote head keeps answering
// reads without a new `git ls-remote`. Every snapshot read consults the
// fingerprint, and the picker's search box fires one read per keystroke;
// without the cache each of those spawned a git process and a network round
// trip. A Refresh bypasses the cache, so a stale window never stands between
// a person and a rebuild they asked for.
const remoteHeadCacheTTL = 30 * time.Second

type remoteHeadEntry struct {
	head    string
	expires time.Time
}

// inventorySourceLocator resolves an inventory to the content its host list is
// built from.
type inventorySourceLocator struct {
	inventoryRepo  db.InventoryManager
	templateRepo   db.TemplateManager
	repositoryRepo db.RepositoryManager
	keyInstaller   db_lib.AccessKeyInstaller

	// gitClient overrides the configured client. It exists so that the
	// fetch-only-when-the-commit-moved logic can be tested without a network.
	gitClient db_lib.GitClient

	// checkoutLock serializes work inside a checkout directory. The directory
	// is shared by every inventory of the same repository and branch, and read
	// on a plain GET that any project member may issue — authenticated, but
	// needing no particular permission and trivially concurrent. Without this,
	// two requests run `git pull` in the same tree at once, or one parses it
	// while the other is half way through updating it. The tasks' repoLock guards the tasks'
	// own directories; these are different directories with the same hazard.
	//
	// It is a pointer for the same reason repoLock is one in TaskPool and
	// LocalExecutorProvider: the lock is shared, never copied along with the
	// struct that carries it.
	checkoutLock *keylock.KeyLock

	// remoteHeads caches `git ls-remote` answers per repository, see
	// remoteHeadCacheTTL. Guarded by remoteHeadsMu. Per node by design: the
	// cache only saves round trips, correctness comes from the fingerprint
	// comparison against the stored snapshot.
	remoteHeadsMu sync.Mutex
	remoteHeads   map[string]remoteHeadEntry

	// remoteHeadFetchLock collapses concurrent misses on one key into a single
	// `git ls-remote`, see remoteHead. A pointer for the same reason
	// checkoutLock is one.
	remoteHeadFetchLock *keylock.KeyLock
}

func (l *inventorySourceLocator) client() db_lib.GitClient {
	if l.gitClient != nil {
		return l.gitClient
	}

	return db_lib.CreateDefaultGitClient(l.keyInstaller)
}

// locate resolves an inventory without reading its content. templateID may be
// nil; it only matters for a file inventory that names no repository of its
// own, where the path is relative to the repository of the template running it.
// freshFingerprint forces a real `git ls-remote` past the remote-head cache;
// a forced Refresh sets it so the rebuild it triggers is against the true
// remote state.
func (l *inventorySourceLocator) locate(
	inventory db.Inventory,
	templateID *int,
	freshFingerprint bool,
) (inventorySource, error) {

	if inventory.Type.IsStatic() {
		return inventorySource{
			inline:      inventory.Inventory,
			fingerprint: inventory.Inventory,
		}, nil
	}

	if inventory.Type != db.InventoryFile {
		return inventorySource{}, InventoryNotParseableError{Type: inventory.Type}
	}

	repository, err := l.resolveRepository(inventory, templateID)
	if err != nil {
		return inventorySource{}, err
	}

	fingerprint, err := l.fingerprintOf(repository, inventory, freshFingerprint)
	if err != nil {
		return inventorySource{}, err
	}

	return inventorySource{
		repository:  &repository,
		relPath:     inventory.GetFilename(),
		fingerprint: fingerprint,
	}, nil
}

// resolveRepository decides which repository a file inventory's path is
// relative to, mirroring what the executor does when it runs a task:
// an inventory that names a repository uses that one, and otherwise the path
// belongs to the repository of the template being run.
func (l *inventorySourceLocator) resolveRepository(
	inventory db.Inventory,
	templateID *int,
) (db.Repository, error) {

	if inventory.RepositoryID != nil {
		return l.repositoryRepo.GetRepository(inventory.ProjectID, *inventory.RepositoryID)
	}

	if templateID != nil {
		template, err := l.templateRepo.GetTemplate(inventory.ProjectID, *templateID)
		if err != nil {
			return db.Repository{}, err
		}

		return l.repositoryRepo.GetRepository(inventory.ProjectID, template.RepositoryID)
	}

	// Without a template to ask, the referring templates answer instead — but
	// only when they agree, since a path relative to two different repositories
	// names two different inventories.
	return l.resolveRepositoryFromReferrers(inventory)
}

func (l *inventorySourceLocator) resolveRepositoryFromReferrers(inventory db.Inventory) (db.Repository, error) {
	refs, err := l.inventoryRepo.GetInventoryRefs(inventory.ProjectID, inventory.ID)
	if err != nil {
		return db.Repository{}, err
	}

	repositoryIDs := map[int]bool{}

	for _, referrer := range refs.Templates {
		template, err := l.templateRepo.GetTemplate(inventory.ProjectID, referrer.ID)
		if err != nil {
			return db.Repository{}, err
		}

		repositoryIDs[template.RepositoryID] = true
	}

	if len(repositoryIDs) != 1 {
		return db.Repository{}, InventoryAmbiguousRepositoryError{Referrers: len(repositoryIDs)}
	}

	for repositoryID := range repositoryIDs {
		return l.repositoryRepo.GetRepository(inventory.ProjectID, repositoryID)
	}

	return db.Repository{}, InventoryAmbiguousRepositoryError{}
}

// fingerprintOf identifies the repository content without fetching it.
func (l *inventorySourceLocator) fingerprintOf(
	repository db.Repository,
	inventory db.Inventory,
	fresh bool,
) (string, error) {

	base := fmt.Sprintf("repo:%d\nbranch:%s\npath:%s\n", repository.ID, repository.GitBranch, inventory.GetFilename())

	if repository.GetType() == db.RepositoryLocal {
		// A local repository has no remote to ask, so the newest modification
		// time under the source stands in for a commit. Adding, removing or
		// editing a file moves it; a Refresh covers anything it misses.
		root := repository.GetFullPath(0)

		// The read path resolves the file inside the repository root and
		// refuses escapes; the fingerprint must not be the one place where a
		// "../" filename walks the server's filesystem, even if all it leaks
		// is an mtime and an error text.
		joined, err := resolveUnderRoot(root, inventory.GetFilename())
		if err != nil {
			return "", err
		}

		stamp, err := newestModTime(joined)
		if err != nil {
			return "", err
		}

		return base + "mtime:" + strconv.FormatInt(stamp.UnixNano(), 10), nil
	}

	hash, err := l.remoteHead(repository, fresh)
	if err != nil {
		return "", err
	}

	return base + "commit:" + hash, nil
}

// remoteHead answers what commit the repository's branch points at, from the
// cache when a recent answer exists — see remoteHeadCacheTTL.
func (l *inventorySourceLocator) remoteHead(repository db.Repository, fresh bool) (string, error) {
	key := fmt.Sprintf("%d\n%s\n%s", repository.ID, repository.GitURL, repository.GitBranch)

	if !fresh {
		if head, ok := l.cachedRemoteHead(key); ok {
			return head, nil
		}
	}

	// Reading the cache and filling it are two steps, and between them sits a
	// network round trip: with a cold or just-expired entry every concurrent
	// reader misses and spawns a `git ls-remote` of its own. The picker's
	// search box fires a read per keystroke, so that is one process per
	// keystroke per open picker, each holding an HTTP goroutine for as long as
	// an unreachable remote takes to time out.
	//
	// Serialising on the key and looking again inside collapses them into one
	// fetch: the first caller fills the cache, the rest read what it left.
	unlock := l.remoteHeadFetchLock.Lock(key)
	defer unlock()

	if !fresh {
		if head, ok := l.cachedRemoteHead(key); ok {
			return head, nil
		}
	}

	hash, err := l.gitRepository(repository).GetLastRemoteCommitHash()
	if err != nil {
		return "", err
	}

	l.remoteHeadsMu.Lock()
	if l.remoteHeads == nil {
		l.remoteHeads = make(map[string]remoteHeadEntry)
	}
	l.remoteHeads[key] = remoteHeadEntry{head: hash, expires: time.Now().Add(remoteHeadCacheTTL)}
	l.remoteHeadsMu.Unlock()

	return hash, nil
}

// cachedRemoteHead answers from the cache while the entry is fresh, and drops
// it when it is not: the key carries the branch name, so renaming a branch or
// re-pointing a repository leaves an entry nothing will ever read again.
func (l *inventorySourceLocator) cachedRemoteHead(key string) (string, bool) {
	l.remoteHeadsMu.Lock()
	defer l.remoteHeadsMu.Unlock()

	entry, ok := l.remoteHeads[key]
	if !ok {
		return "", false
	}

	if time.Now().Before(entry.expires) {
		return entry.head, true
	}

	delete(l.remoteHeads, key)

	return "", false
}

// lock serializes everything done with one checkout directory and returns the
// unlock function.
//
// An inline inventory shares no directory with anybody, but its readers still
// share the snapshot rows they are about to replace, so it is serialized on the
// inventory itself: without that, every concurrent reader of a stale snapshot
// parses and rewrites four tables with the rows the first one already wrote.
//
// The lock is taken by the caller rather than by read below, because the
// decision to rebuild at all has to be made under it: see ensureFresh.
func (l *inventorySourceLocator) lock(inventory db.Inventory, source inventorySource) func() {
	if source.isInline() {
		return l.checkoutLock.Lock(
			fmt.Sprintf("inventory_%d_%d", inventory.ProjectID, inventory.ID))
	}

	return l.checkoutLock.Lock(l.gitRepository(*source.repository).TmpDirName)
}

// read fetches the content and parses it. For a repository inventory this is
// where the clone or pull happens, and the caller must hold lock() for the
// source.
//
// The lock has to cover the parse as well as the git operation: the directory
// is shared by every inventory of the repository and branch, and parsing a
// tree that another request is half way through pulling would hand out a
// truncated host list as a complete one.
func (l *inventorySourceLocator) read(
	inventory db.Inventory,
	source inventorySource,
) (db_lib.InventoryHosts, error) {

	if source.isInline() {
		return db_lib.ParseInventory(inventory.Type, source.inline)
	}

	rootDir, err := l.checkout(*source.repository)
	if err != nil {
		return db_lib.InventoryHosts{}, err
	}

	return db_lib.ParseInventorySource(rootDir, source.relPath)
}

// checkout brings the repository into a scratch directory of its own and
// returns its root.
//
// The directory lives under util.Config.TmpPath, and checkoutLock is a lock
// inside one process, so coordination between nodes rests on tmp_path being
// node-local. Point two nodes at one shared volume and they pull into the same
// working tree with nothing between them — the same requirement the tasks'
// repoLock already makes of the same setting.
func (l *inventorySourceLocator) checkout(repository db.Repository) (string, error) {
	// Only a bare filesystem path is read where it lies. A file:// URL is a git
	// remote like any other and is cloned, which is what keeps the checkout on
	// the branch the repository is configured for.
	if repository.GetType() == db.RepositoryLocal {
		return repository.GetFullPath(0), nil
	}

	git := l.gitRepository(repository)

	if err := git.ValidateRepo(); err != nil {
		if err := git.Clone(); err != nil {
			return "", err
		}
	} else if err := git.Pull(); err != nil {
		return "", err
	}

	return git.GetFullPath(), nil
}

func (l *inventorySourceLocator) gitRepository(repository db.Repository) db_lib.GitRepository {
	// Clone() clones a single branch, so a checkout can only ever serve the
	// branch it was made with. Keying the scratch directory by branch keeps a
	// reconfigured repository from reusing a checkout of the old one.
	branch := sha1.Sum([]byte(repository.GitBranch))

	return db_lib.GitRepository{
		Repository: repository,
		TmpDirName: fmt.Sprintf("repository_%d_inventory_snapshot_%x", repository.ID, branch[:4]),
		Client:     l.client(),
		Logger:     task_logger.NopLogger{},
	}
}

// resolveUnderRoot joins a stored filename onto the repository root and
// refuses the result when it escapes it: filepath.Join collapses "..", so a
// hostile filename could otherwise name any path on the server.
func resolveUnderRoot(root string, filename string) (string, error) {
	joined := filepath.Join(root, filename)

	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("inventory path %q escapes its repository", filename)
	}

	return joined, nil
}

// newestModTime returns the latest modification time at or under a path. It
// stats rather than reads, so a directory costs far less here than it does to
// parse.
//
// Error texts carry no filesystem paths on purpose: they end up in the
// snapshot's error column, which every project member can read, and the
// repository path is the server's business, not theirs.
func newestModTime(root string) (time.Time, error) {
	info, err := os.Stat(root)
	if err != nil {
		return time.Time{}, fmt.Errorf("local inventory cannot be read")
	}

	if !info.IsDir() {
		return info.ModTime(), nil
	}

	newest := info.ModTime()
	seen := 0

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("local inventory cannot be listed")
		}

		seen++
		if seen > maxLocalInventoryEntries {
			return fmt.Errorf("local inventory holds more than %d entries", maxLocalInventoryEntries)
		}

		entryInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("local inventory cannot be listed")
		}

		if entryInfo.ModTime().After(newest) {
			newest = entryInfo.ModTime()
		}

		return nil
	})

	return newest, err
}

// maxLocalInventoryEntries bounds the stat walk of a local repository, which
// happens on every read of the host list.
const maxLocalInventoryEntries = 4096

// InventoryAmbiguousRepositoryError reports a file inventory whose path cannot
// be resolved because it names no repository and the templates using it do not
// agree on one.
type InventoryAmbiguousRepositoryError struct {
	Referrers int
}

func (e InventoryAmbiguousRepositoryError) Error() string {
	if e.Referrers == 0 {
		return "inventory names no repository and no template uses it, so its path resolves to nothing"
	}

	return fmt.Sprintf(
		"inventory names no repository and the %d templates using it disagree on one",
		e.Referrers,
	)
}
