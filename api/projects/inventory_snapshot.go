package projects

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/server"
	log "github.com/sirupsen/logrus"
)

// InventorySnapshotController serves the cached host list of an inventory, so
// that a task can be limited to hosts and groups picked from a list instead of
// typed by hand.
type InventorySnapshotController struct {
	snapshotService server.InventorySnapshotService
}

func NewInventorySnapshotController(snapshotService server.InventorySnapshotService) *InventorySnapshotController {
	return &InventorySnapshotController{snapshotService: snapshotService}
}

// GetInventorySnapshot returns the state of the host list: when it was built,
// how many hosts it holds, and whether the inventory is one whose hosts the
// server can enumerate at all.
func (c *InventorySnapshotController) GetInventorySnapshot(w http.ResponseWriter, r *http.Request) {
	inventory := helpers.GetFromContext(r, "inventory").(db.Inventory)

	snapshot, err := c.snapshotService.GetSnapshot(inventory.ProjectID, inventory.ID, queryTemplateID(r))
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, snapshot)
}

// GetInventorySnapshotHosts returns one page of host names, optionally
// narrowed to a group or to a substring of the name.
func (c *InventorySnapshotController) GetInventorySnapshotHosts(w http.ResponseWriter, r *http.Request) {
	inventory := helpers.GetFromContext(r, "inventory").(db.Inventory)

	filter := db.InventoryHostFilter{
		Group:  queryName(r, "group"),
		Search: queryName(r, "search"),
		Limit:  queryInt(r, "limit"),
		Offset: queryInt(r, "offset"),
	}

	hosts, err := c.snapshotService.GetHosts(inventory.ProjectID, inventory.ID, queryTemplateID(r), filter)
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	if hosts == nil {
		hosts = []db.InventorySnapshotHost{}
	}

	helpers.WriteJSON(w, http.StatusOK, hosts)
}

// GetInventorySnapshotGroups returns one page of the groups of the inventory,
// each with the size of its host set.
func (c *InventorySnapshotController) GetInventorySnapshotGroups(w http.ResponseWriter, r *http.Request) {
	inventory := helpers.GetFromContext(r, "inventory").(db.Inventory)

	filter := db.InventoryGroupFilter{
		Limit:  queryInt(r, "limit"),
		Offset: queryInt(r, "offset"),
	}

	groups, err := c.snapshotService.GetGroups(inventory.ProjectID, inventory.ID, queryTemplateID(r), filter)
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	if groups == nil {
		groups = []db.InventorySnapshotGroup{}
	}

	helpers.WriteJSON(w, http.StatusOK, groups)
}

// RefreshInventorySnapshot rebuilds the host list even when the inventory has
// not changed.
//
// For most inventories the server does that itself. For one whose hosts can
// only be listed by running a program, it dispatches a task instead — see
// dispatchInventoryResolve for why that path needs a permission of its own.
func (c *InventorySnapshotController) RefreshInventorySnapshot(w http.ResponseWriter, r *http.Request) {
	inventory := helpers.GetFromContext(r, "inventory").(db.Inventory)

	snapshot, err := c.snapshotService.Refresh(inventory.ProjectID, inventory.ID, queryTemplateID(r))

	var needsRunner server.InventoryNeedsRunnerError

	if errors.As(err, &needsRunner) {
		c.dispatchInventoryResolve(w, r, inventory)
		return
	}

	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, snapshot)
}

// dispatchInventoryResolve asks a runner to list the hosts of a dynamic
// inventory.
//
// This is the one place in the picker that causes code to run, so it is gated
// on the permission that running code needs. The subtree already requires
// CanManageProjectResources, but that is a different permission held by a
// different set of roles: it says nothing about being allowed to execute
// anything. Reading the host list, by contrast, stays open to every project
// member, because GET bypasses the check entirely.
func (c *InventorySnapshotController) dispatchInventoryResolve(
	w http.ResponseWriter,
	r *http.Request,
	inventory db.Inventory,
) {
	templateID := queryTemplateID(r)

	if templateID == nil {
		helpers.WriteErrorStatus(w,
			"Listing the hosts of this inventory runs a program, which needs a template to run it in.",
			http.StatusBadRequest)
		return
	}

	template, err := helpers.Store(r).GetTemplate(inventory.ProjectID, *templateID)
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	// Asked before anything is reported about the template: whether it may be
	// run is the caller's own permission, and the answers below describe the
	// template itself.
	if !c.canRunTemplate(r, inventory.ProjectID, *templateID) {
		// The body matches every other error of this endpoint. The status is
		// what a caller keys on, and it is unchanged.
		helpers.WriteErrorStatus(w,
			"Listing the hosts of this inventory runs a task, which you are not allowed to do.",
			http.StatusForbidden)
		return
	}

	// The resolve flag is only understood by the Ansible executor. Any other
	// app would ignore it and run the template for real — a host list refresh
	// must never be able to do that, permissions or not.
	if template.App != db.AppAnsible {
		helpers.WriteErrorStatus(w,
			"Only an Ansible template can list the hosts of an inventory.",
			http.StatusBadRequest)
		return
	}

	// The task below names the inventory to resolve, but the executor only
	// honours that when the template allows its inventory to be overridden.
	// Without this check the runner would list the hosts of the template's own
	// inventory while the result was stored under the requested one, which then
	// answers every read of it — a picker showing hosts that are not there.
	canOverride, err := template.CanOverrideInventory()
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	if !canOverride && (template.InventoryID == nil || *template.InventoryID != inventory.ID) {
		helpers.WriteErrorStatus(w,
			"This template does not allow its inventory to be overridden, so it can not list the hosts of another one.",
			http.StatusBadRequest)
		return
	}

	// The claim comes before the task: it is the claim that deduplicates
	// resolves, and a task created first would run the inventory script no
	// matter who won.
	claimed, err := helpers.Store(r).ClaimInventorySnapshotResolve(inventory.ProjectID, inventory.ID)
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	if !claimed {
		// Somebody's resolve is already in flight; report its state rather
		// than pretending this call started something.
		snapshot, err := c.snapshotService.GetSnapshot(inventory.ProjectID, inventory.ID, templateID)
		if err != nil {
			helpers.WriteError(w, err)
			return
		}

		helpers.WriteJSON(w, http.StatusOK, snapshot)
		return
	}

	task, err := taskPool(r).AddTask(db.Task{
		ProjectID:   inventory.ProjectID,
		TemplateID:  *templateID,
		InventoryID: &inventory.ID,
		Params: db.MapStringAnyField{
			"resolve_inventory_only": true,
		},
	}, currentUserID(r), currentUsername(r), inventory.ProjectID, false)

	if err != nil {
		// The claim is ours and its task will never exist; give it up so the
		// next refresh is not locked out until the claim expires.
		if releaseErr := helpers.Store(r).ReleaseInventorySnapshotResolve(
			inventory.ProjectID, inventory.ID, "failed to queue the resolve task",
		); releaseErr != nil {
			log.WithError(releaseErr).WithFields(log.Fields{
				"project_id":   inventory.ProjectID,
				"inventory_id": inventory.ID,
			}).Warn("failed to release an inventory resolve claim")
		}

		helpers.WriteError(w, err)
		return
	}

	// Best effort: an unassigned claim still expires on its own, it just
	// cannot be settled early when the task fails.
	if err := helpers.Store(r).AssignInventorySnapshotResolveTask(inventory.ProjectID, inventory.ID, task.ID); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"project_id":   inventory.ProjectID,
			"inventory_id": inventory.ID,
			"task_id":      task.ID,
		}).Warn("failed to record the resolve task on the claim")
	}

	helpers.WriteJSON(w, http.StatusAccepted, db.InventorySnapshot{
		InventoryID: inventory.ID,
		ProjectID:   inventory.ProjectID,
		Status:      db.InventorySnapshotStatusPending,
		TaskID:      &task.ID,
	})
}

// canRunTemplate answers whether the caller may run tasks of this template,
// counting the grants a template can carry on top of the project role.
func (c *InventorySnapshotController) canRunTemplate(r *http.Request, projectID int, templateID int) bool {
	user := helpers.GetFromContext(r, "user").(*db.User)

	if user.Admin {
		return true
	}

	permissions := helpers.GetFromContext(r, "permissions").(db.ProjectUserPermission)

	if perm, err := helpers.Store(r).GetTemplatePermission(projectID, templateID, user.ID); err == nil {
		permissions |= perm
	}

	return permissions&db.CanRunProjectTasks == db.CanRunProjectTasks
}

func currentUserID(r *http.Request) *int {
	user := helpers.GetFromContext(r, "user").(*db.User)

	return &user.ID
}

func currentUsername(r *http.Request) string {
	user := helpers.GetFromContext(r, "user").(*db.User)

	return user.Username
}

// queryInt reads a non-negative integer query parameter, treating an absent or
// malformed one as unset. The store clamps the resulting page size, so a
// nonsense limit cannot widen the response.
func queryInt(r *http.Request, name string) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || value < 0 {
		return 0
	}

	return value
}

// queryTemplateID reads the optional template a host list is being asked for.
// It only matters for a file inventory that names no repository of its own; the
// service resolves the template within the inventory's project, so an id from
// another project resolves to nothing rather than to that project's repository.
func queryTemplateID(r *http.Request) *int {
	value := queryInt(r, "template_id")
	if value <= 0 {
		return nil
	}

	return &value
}

// queryName reads a query parameter that is matched against a stored host or
// group name, cut to the width those names can have. Anything longer cannot
// name a host that exists, and a megabyte of it is a megabyte the database
// would compare against every row for nothing.
func queryName(r *http.Request, name string) string {
	value := r.URL.Query().Get(name)

	// Cut by runes: the column counts characters, and a byte cut can split a
	// multi-byte character into something that matches nothing at all.
	runes := []rune(value)

	if len(runes) <= db.MaxInventoryHostNameLength {
		return value
	}

	return string(runes[:db.MaxInventoryHostNameLength])
}
