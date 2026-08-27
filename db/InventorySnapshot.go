package db

import (
	"sort"
	"time"
)

// MaxInventoryHostNameLength mirrors the width of the name columns of the
// snapshot tables. A parsed inventory carrying a longer name is rejected as a
// whole rather than silently losing that host.
const MaxInventoryHostNameLength = 255

// MaxInventorySnapshotErrorLength mirrors the width of the error column.
const MaxInventorySnapshotErrorLength = 1000

// MaxInventoryHostPageSize caps one page of a host listing. A snapshot may
// hold tens of thousands of hosts, and serving them in one response is what
// storing them as rows was meant to avoid.
const MaxInventoryHostPageSize = 500

// MaxInventoryGroupPageSize caps one page of a group listing. Groups are
// bounded by MaxInventorySnapshotGroups, which is as large as the host cap, so
// the argument for paging hosts applies to them unchanged.
const MaxInventoryGroupPageSize = MaxInventoryHostPageSize

// MaxInventorySnapshotHosts bounds a host list a runner reports back. A cloud
// inventory can resolve to far more hosts than a person can pick from, and the
// list arrives inside a progress report that has to stay a reasonable size.
const MaxInventorySnapshotHosts = 20000

// MaxInventorySnapshotGroups bounds the group list the same way. The
// degenerate ceiling is a group per host.
const MaxInventorySnapshotGroups = MaxInventorySnapshotHosts

// MaxInventorySnapshotMemberships bounds the total host-in-group rows of one
// snapshot. Hosts and groups are each capped at 20000, but their product is
// not, and a report is the runner's word, not the parser's: without this cap a
// hostile report could turn one progress request into hundreds of millions of
// rows. 10x the host cap covers a host in ten groups each, which is far past
// any real inventory.
const MaxInventorySnapshotMemberships = 10 * MaxInventorySnapshotHosts

// MaxInventoryResolveAge is how long a snapshot may stay pending before a read
// stops believing it. The task that would clear it can be lost — a server that
// dies mid-resolve leaves the row behind — and a picker stuck on "resolving"
// forever is worse than one that says the resolve failed.
const MaxInventoryResolveAge = 30 * time.Minute

// InventorySnapshotStatus is the outcome of the last synchronisation.
type InventorySnapshotStatus string

const (
	InventorySnapshotStatusOK InventorySnapshotStatus = "ok"

	// InventorySnapshotStatusError means the last synchronisation failed. The
	// host list of the snapshot is the one written by the last successful run,
	// or empty if there was none.
	InventorySnapshotStatusError InventorySnapshotStatus = "error"

	// InventorySnapshotStatusPending means a runner has been asked to list the
	// hosts and has not answered yet. It is the only state whose meaning
	// expires: see MaxInventoryResolveAge.
	InventorySnapshotStatusPending InventorySnapshotStatus = "pending"

	// InventorySnapshotStatusUnsupported means the server can not enumerate
	// the hosts of this inventory: it is a file in a repository, a Terraform
	// workspace, or a dynamic inventory, none of which can be read without
	// running something. It is not an error, and consumers are expected to
	// offer free-form host entry instead of an empty list.
	InventorySnapshotStatusUnsupported InventorySnapshotStatus = "unsupported"
)

// InventorySnapshot is the cached host list of one inventory. It is derived
// data with a single writer, so there is exactly one row per inventory: a
// synchronisation replaces it rather than adding a version.
type InventorySnapshot struct {
	InventoryID int                     `db:"inventory_id" json:"inventory_id"`
	ProjectID   int                     `db:"project_id" json:"project_id"`
	Status      InventorySnapshotStatus `db:"status" json:"status"`
	Error       string                  `db:"error" json:"error,omitempty"`

	// SourceHash identifies the inventory content the snapshot was built from,
	// so that a stale snapshot can be detected without re-parsing. It is not
	// served: it says nothing a caller can act on.
	SourceHash string `db:"source_hash" json:"-"`

	// HostCount is the size of the host list currently stored, whatever Status
	// says. A claim taken for a resolve, and a resolve that then failed, leave
	// the previous host rows in place, so the count keeps describing them: it
	// has to agree with what a host listing returns at the same moment.
	HostCount int       `db:"host_count" json:"host_count"`
	Created   time.Time `db:"created" json:"created"`

	// TaskID is the task that produced the snapshot, for the inventory types
	// that need one to be run. It is null for inventories parsed in-process.
	TaskID *int `db:"task_id" json:"task_id,omitempty"`
}

// InventorySnapshotHost is one host of a snapshot.
type InventorySnapshotHost struct {
	InventoryID int    `db:"inventory_id" json:"-"`
	Name        string `db:"name" json:"name"`
}

// InventorySnapshotGroup is one group of a snapshot. HostCount is the size of
// the transitive host set, which is what limiting a run to this group selects.
type InventorySnapshotGroup struct {
	InventoryID int    `db:"inventory_id" json:"-"`
	Name        string `db:"name" json:"name"`
	HostCount   int    `db:"host_count" json:"host_count"`
}

// InventorySnapshotContent is the parsed inventory a synchronisation writes.
//
// It also travels between a runner and the server inside a progress report, so
// the json tags below are a wire contract rather than decoration: they pin the
// names that are already in use, which an unrelated rename of a Go field would
// otherwise change under a runner of the previous version. Renaming a field
// here means keeping its tag.
type InventorySnapshotContent struct {
	Hosts  []string                        `json:"Hosts"`
	Groups []InventorySnapshotGroupContent `json:"Groups"`
}

// InventorySnapshotGroupContent is a group together with its transitive host
// set, which is stored as membership rows. Its tags are part of the same wire
// contract as InventorySnapshotContent.
type InventorySnapshotGroupContent struct {
	Name  string   `json:"Name"`
	Hosts []string `json:"Hosts"`
}

// NewInventorySnapshotContent builds snapshot content from the shape a task
// logger reports, which is primitive because pkg/task_logger cannot depend on
// this package.
func NewInventorySnapshotContent(hosts []string, groups map[string][]string) InventorySnapshotContent {
	content := InventorySnapshotContent{
		Hosts:  hosts,
		Groups: make([]InventorySnapshotGroupContent, 0, len(groups)),
	}

	if content.Hosts == nil {
		content.Hosts = []string{}
	}

	for name, groupHosts := range groups {
		if groupHosts == nil {
			groupHosts = []string{}
		}

		content.Groups = append(content.Groups, InventorySnapshotGroupContent{
			Name:  name,
			Hosts: groupHosts,
		})
	}

	// Map iteration order is random, and this content is serialized into a
	// runner's progress report: without an order of its own, two reports of the
	// same inventory would differ on the wire and could not be compared.
	sort.Slice(content.Groups, func(i, j int) bool {
		return content.Groups[i].Name < content.Groups[j].Name
	})

	return content
}

// InventoryHostFilter narrows a host listing. An empty filter returns the
// whole snapshot up to Limit.
type InventoryHostFilter struct {
	// Group restricts the listing to the members of one group.
	Group string

	// Search matches host names containing this substring, case-insensitively.
	Search string

	Limit  int
	Offset int
}

// InventoryGroupFilter pages a group listing. The zero value asks for the
// first page, which is what a caller written before groups were paged sends.
type InventoryGroupFilter struct {
	Limit  int
	Offset int
}
