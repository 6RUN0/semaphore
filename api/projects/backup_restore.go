package projects

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/semaphoreui/semaphore/util"

	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	projectService "github.com/semaphoreui/semaphore/services/project"
	log "github.com/sirupsen/logrus"
)

// MaxProjectRestoreBodySize bounds one uploaded backup file. Restore reads the
// whole body into memory before it looks at it, and NonAdminCanCreateProject
// opens the route to every user, so without the cap one endless body exhausts
// the server.
//
// Unlike the runner report limit the number cannot be derived: a backup holds
// every row of one project (templates with their survey vars, environments
// with their JSON documents, inventories, keys, integrations, workflows) and
// none of those columns has a length of its own to add up. It is an allowance
// instead, of the same order as the fixed part of MaxRunnerReportBodySize: a
// template serializes to a few kilobytes, so 32 MiB covers a project with
// thousands of them, far past any real one.
const MaxProjectRestoreBodySize = int64(32 * 1024 * 1024)

// BackupController serves project backup/restore. Workflows live outside
// db.Store (Pro feature, see db.WorkflowManager), so the workflow store is
// injected and threaded into the backup/restore routines.
type BackupController struct {
	workflowStore db.WorkflowManager
}

func NewBackupController(workflowStore db.WorkflowManager) *BackupController {
	return &BackupController{workflowStore: workflowStore}
}

func (c *BackupController) GetBackup(w http.ResponseWriter, r *http.Request) {
	project := helpers.GetFromContext(r, "project").(db.Project)

	store := helpers.Store(r)

	opts := projectService.BackupOptions{
		IncludeIntegrationAliases: r.URL.Query().Get("include_integration_aliases") == "true",
	}

	backup, err := projectService.GetBackup(project.ID, store, c.workflowStore, opts)

	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	str, err := backup.Marshal()
	if err != nil {
		helpers.WriteError(w, err)
		return
	}

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(str))
}

func (c *BackupController) Restore(w http.ResponseWriter, r *http.Request) {
	user := helpers.GetFromContext(r, "user").(*db.User)

	if !user.Admin && !util.Config.NonAdminCanCreateProject {
		log.Warn(user.Username + " is not permitted to restore the project")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var backup projectService.BackupFormat

	// Capped after the permission check so an unauthorized caller is turned
	// away before the body is read at all.
	r.Body = http.MaxBytesReader(w, r.Body, MaxProjectRestoreBodySize)

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, r.Body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Warn(user.Username + " sent a backup over the size limit")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		log.Error(err)
		helpers.WriteError(w, err)
		return
	}

	str := buf.String()

	if err := backup.Unmarshal(str); err != nil {
		log.Error(err)
		helpers.WriteError(w, err)
		return
	}

	store := helpers.Store(r)
	if err := backup.Verify(); err != nil {
		log.Error(err)
		helpers.WriteError(w, err)
		return
	}

	var p *db.Project
	p, err := backup.Restore(*user, store, c.workflowStore)

	if err != nil {
		log.Error(err)
		helpers.WriteError(w, err)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, p)
}
