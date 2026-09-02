package project

import (
	"encoding/json"
	"testing"

	"github.com/semaphoreui/semaphore/db/sql"

	"github.com/semaphoreui/semaphore/db"
	proFactory "github.com/semaphoreui/semaphore/pro/db/factory"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testItem struct {
	Name string
}

func TestBackupProject(t *testing.T) {
	util.Config = &util.ConfigType{
		TmpPath: "/tmp",
	}

	store := sql.InitConfigCreateTestStore()

	proj, err := store.CreateProject(db.Project{
		Name: "Test 123",
	})
	assert.NoError(t, err)

	key, err := store.CreateAccessKey(db.AccessKey{
		ProjectID: &proj.ID,
		Type:      db.AccessKeyNone,
	})
	assert.NoError(t, err)

	repo, err := store.CreateRepository(db.Repository{
		ProjectID: proj.ID,
		SSHKeyID:  key.ID,
		Name:      "Test",
		GitURL:    "git@example.com:test/test",
		GitBranch: "master",
	})
	assert.NoError(t, err)

	inv, err := store.CreateInventory(db.Inventory{
		ProjectID: proj.ID,
		ID:        1,
	})
	assert.NoError(t, err)

	env, err := store.CreateEnvironment(db.Environment{
		ProjectID: proj.ID,
		Name:      "test",
		JSON:      `{"author": "Denis", "comment": "Hello, World!"}`,
	})
	assert.NoError(t, err)

	_, err = store.CreateTemplate(db.Template{
		Name:           "Test",
		Playbook:       "test.yml",
		ProjectID:      proj.ID,
		RepositoryID:   repo.ID,
		InventoryID:    &inv.ID,
		EnvironmentIDs: []int{env.ID},
	})
	assert.NoError(t, err)

	backup, err := GetBackup(proj.ID, store, proFactory.NewWorkflowStore(store))
	assert.NoError(t, err)
	assert.Equal(t, proj.ID, backup.Meta.ID)

	str, err := backup.Marshal()
	assert.NoError(t, err)

	restoredBackup := &BackupFormat{}
	err = restoredBackup.Unmarshal(str)
	assert.NoError(t, err)
	assert.Equal(t, restoredBackup.Meta.Name, "Test 123")

	restoredBackup.Meta.Name = "Test 1234"

	user, err := store.CreateUser(db.UserWithPwd{
		Pwd: "3412341234123",
		User: db.User{
			Username: "test",
			Name:     "Test",
			Email:    "test@example.com",
			Admin:    true,
		},
	})
	assert.NoError(t, err)

	restoredProj, err := restoredBackup.Restore(user, store, proFactory.NewWorkflowStore(store))
	assert.NoError(t, err)
	assert.Equal(t, restoredProj.Name, "Test 1234")

	restoredTemplates, err := store.GetTemplates(restoredProj.ID, db.TemplateFilter{}, db.RetrieveQueryParams{})
	assert.NoError(t, err)
	assert.Len(t, restoredTemplates, 1)
	assert.Len(t, restoredTemplates[0].EnvironmentIDs, 1)

	restoredEnvs, err := store.GetEnvironments(restoredProj.ID, db.RetrieveQueryParams{})
	assert.NoError(t, err)
	assert.Len(t, restoredEnvs, 1)
	assert.Equal(t, restoredEnvs[0].ID, restoredTemplates[0].EnvironmentIDs[0])
	assert.Equal(t, "test", restoredEnvs[0].Name)
}

func TestBackup_BackupSecretStorage(t *testing.T) {
	util.Config = &util.ConfigType{
		TmpPath: "/tmp",
	}

	store := sql.InitConfigCreateTestStore()

	proj, err := store.CreateProject(db.Project{
		Name: "Test 123",
	})
	assert.NoError(t, err)

	storage, err := store.CreateSecretStorage(db.SecretStorage{
		ProjectID: proj.ID,
		Type:      "vault",
		Name:      "Test",
	})
	assert.NoError(t, err)

	_, err = store.CreateAccessKey(db.AccessKey{
		ProjectID: &proj.ID,
		Type:      db.AccessKeyNone,
		StorageID: &storage.ID,
		Name:      "Test Key",
		Owner:     "vault",
	})
	assert.NoError(t, err)

	backup, err := GetBackup(proj.ID, store, proFactory.NewWorkflowStore(store))
	assert.NoError(t, err)
	assert.Equal(t, proj.ID, backup.Meta.ID)
	backup.Meta.Name = "Test 1234"

	str, err := backup.Marshal()
	assert.NoError(t, err)

	var res map[string]any
	if err := json.Unmarshal([]byte(str), &res); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, `{
  "environments": [],
  "integration_aliases": [],
  "integrations": [],
  "inventories": [],
  "keys": [
    {
      "name": "Test Key",
      "owner": "vault",
      "storage": "Test",
      "type": "none"
    }
  ],
  "meta": {
    "alert": false,
    "max_parallel_tasks": 0,
    "name": "Test 1234",
    "type": ""
  },
  "repositories": [],
  "roles": [],
  "runners": [],
  "schedules": [],
  "secret_storages": [
    {
      "name": "Test",
      "params": {},
      "readonly": false,
      "sync_enabled": false,
      "sync_interval": 0,
      "type": "vault"
    }
  ],
  "templates": [],
  "views": [],
  "workflows": []
}`, str)

	restoredBackup := &BackupFormat{}
	err = restoredBackup.Unmarshal(str)
	assert.NoError(t, err)
	assert.Equal(t, restoredBackup.Meta.Name, "Test 1234")

	user, err := store.CreateUser(db.UserWithPwd{
		Pwd: "3412341234123",
		User: db.User{
			Username: "test",
			Name:     "Test",
			Email:    "test@example.com",
			Admin:    true,
		},
	})
	assert.NoError(t, err)

	restoredProj, err := restoredBackup.Restore(user, store, proFactory.NewWorkflowStore(store))
	assert.Nil(t, err)

	restoredStorages, err := store.GetSecretStorages(restoredProj.ID)
	assert.NoError(t, err)
	assert.Len(t, restoredStorages, 1)

	restoredKeys, err := store.GetAccessKeys(restoredProj.ID, db.GetAccessKeyOptions{IgnoreOwner: true}, db.RetrieveQueryParams{})
	assert.NoError(t, err)
	assert.Len(t, restoredKeys, 1)

	assert.Equal(t, *restoredKeys[0].StorageID, restoredStorages[0].ID)
}

// TestBackup_RestoreScheduleWithoutTaskParams is a regression test for
// https://github.com/semaphoreui/semaphore/issues/3858 . Backups written by
// older Semaphore versions omit the per-schedule "task_params" object; on
// restore, BackupSchedule.Restore used to dereference the nil pointer and
// crash the HTTP handler with a runtime nil-pointer panic.
func TestBackup_RestoreScheduleWithoutTaskParams(t *testing.T) {
	util.Config = &util.ConfigType{
		TmpPath: "/tmp",
	}

	store := sql.InitConfigCreateTestStore()

	// An old-format backup payload: a single template plus a single
	// schedule with no "task_params" object at all. Restore() should
	// succeed and recreate the schedule, not panic.
	payload := `{
  "environments": [],
  "integration_aliases": [],
  "integrations": [],
  "inventories": [],
  "keys": [
    {
      "name": "noop",
      "owner": "",
      "type": "none"
    }
  ],
  "meta": {
    "alert": false,
    "max_parallel_tasks": 0,
    "name": "Restored Project",
    "type": ""
  },
  "repositories": [
    {
      "git_branch": "master",
      "git_url": "git@example.com:test/test.git",
      "name": "Test Repo",
      "ssh_key": "noop"
    }
  ],
  "roles": [],
  "runners": [],
  "schedules": [
    {
      "active": true,
      "cron_format": "0 0 * * *",
      "delete_after_run": false,
      "name": "nightly",
      "template": "Test Template",
      "type": ""
    }
  ],
  "secret_storages": [],
  "templates": [
    {
      "allow_override_args_in_task": false,
      "app": "",
      "autorun": false,
      "name": "Test Template",
      "playbook": "test.yml",
      "repository": "Test Repo",
      "roles": [],
      "suppress_success_alerts": false,
      "type": "",
      "vaults": [],
      "view": null,
      "environments": []
    }
  ],
  "views": []
}`

	restoredBackup := &BackupFormat{}
	err := restoredBackup.Unmarshal(payload)
	assert.NoError(t, err)

	user, err := store.CreateUser(db.UserWithPwd{
		Pwd: "3412341234123",
		User: db.User{
			Username: "schedrestore",
			Name:     "Test",
			Email:    "schedrestore@example.com",
			Admin:    true,
		},
	})
	assert.NoError(t, err)

	restoredProj, err := restoredBackup.Restore(user, store, proFactory.NewWorkflowStore(store))
	assert.NoError(t, err)

	restoredSchedules, err := store.GetSchedules()
	assert.NoError(t, err)
	var found bool
	for _, s := range restoredSchedules {
		if s.ProjectID == restoredProj.ID && s.Name == "nightly" {
			found = true
			break
		}
	}
	assert.True(t, found, "restored schedule should be persisted")
}

// TestBackup_RestoreDropsSecretSurveyVarDefault pins the restore side of
// TestBackupFormat_DropsSecretSurveyVarDefault: a backup written before the
// default was refused on secret variables still restores, and the default is
// dropped rather than stored as plain text.
func TestBackup_RestoreDropsSecretSurveyVarDefault(t *testing.T) {
	util.Config = &util.ConfigType{
		TmpPath: "/tmp",
	}

	store := sql.InitConfigCreateTestStore()

	payload := `{
  "environments": [],
  "integration_aliases": [],
  "integrations": [],
  "inventories": [],
  "keys": [{"name": "noop", "owner": "", "type": "none"}],
  "meta": {"alert": false, "max_parallel_tasks": 0, "name": "Restored Project", "type": ""},
  "repositories": [
    {"git_branch": "master", "git_url": "git@example.com:test/test.git", "name": "Test Repo", "ssh_key": "noop"}
  ],
  "roles": [],
  "runners": [],
  "schedules": [],
  "secret_storages": [],
  "templates": [
    {
      "name": "Test Template",
      "playbook": "test.yml",
      "repository": "Test Repo",
      "roles": [],
      "type": "",
      "vaults": [],
      "view": null,
      "environments": [],
      "survey_vars": [
        {"name": "pw", "type": "secret", "default_value": "hunter2"},
        {"name": "host", "default_value": "db"}
      ]
    }
  ],
  "views": []
}`

	restoredBackup := &BackupFormat{}
	require.NoError(t, restoredBackup.Unmarshal(payload))

	user, err := store.CreateUser(db.UserWithPwd{
		Pwd: "3412341234123",
		User: db.User{
			Username: "secretrestore",
			Name:     "Test",
			Email:    "secretrestore@example.com",
			Admin:    true,
		},
	})
	require.NoError(t, err)

	restoredProj, err := restoredBackup.Restore(user, store, proFactory.NewWorkflowStore(store))
	require.NoError(t, err)

	templates, err := store.GetTemplates(restoredProj.ID, db.TemplateFilter{}, db.RetrieveQueryParams{})
	require.NoError(t, err)
	require.Len(t, templates, 1)
	require.Len(t, templates[0].SurveyVars, 2)

	assert.Nil(t, templates[0].SurveyVars[0].DefaultValue)
	require.NotNil(t, templates[0].SurveyVars[1].DefaultValue)
	assert.Equal(t, "db", templates[0].SurveyVars[1].DefaultValue.String())
	require.NotNil(t, templates[0].SurveyVarsJSON)
	assert.NotContains(t, *templates[0].SurveyVarsJSON, "hunter2")
}

// TestBackup_Workflow moved to pro_impl/db/sql/backup_workflow_test.go because
// workflow persistence is a Pro feature requiring the real workflow store
// (the open-source build only has the no-op stub).

func isUnique(items []testItem) bool {
	for i, item := range items {
		for k, other := range items {
			if i == k {
				continue
			}

			if item.Name == other.Name {
				return false
			}
		}
	}

	return true
}

// TestBackupFormat_SkipsEntriesWithMissingTemplate pins the slice
// construction in format(): `continue` inside an index-assigned loop used to
// leave a zero-valued schedule or integration behind.
func TestBackupFormat_SkipsEntriesWithMissingTemplate(t *testing.T) {
	b := BackupDB{
		meta:         db.Project{ID: 1, Name: "p"},
		repositories: []db.Repository{{ID: 20, Name: "repo"}},
		templates:    []db.Template{{ID: 10, Name: "existing", RepositoryID: 20}},
		schedules: []db.Schedule{
			{ID: 1, Name: "kept", TemplateID: 10},
			{ID: 2, Name: "orphan", TemplateID: 999},
		},
		integrations: []db.Integration{
			{ID: 1, Name: "kept", TemplateID: 10},
			{ID: 2, Name: "orphan", TemplateID: 999},
		},
		integrationAliases:       map[int][]db.IntegrationAlias{},
		integrationMatchers:      map[int][]db.IntegrationMatcher{},
		integrationExtractValues: map[int][]db.IntegrationExtractValue{},
		templateRoles:            map[int][]db.TemplateRolePerm{},
	}

	format, err := b.format()
	require.NoError(t, err)

	require.Len(t, format.Schedules, 1)
	assert.Equal(t, "kept", format.Schedules[0].Name)
	assert.Equal(t, "existing", format.Schedules[0].Template)

	require.Len(t, format.Integration, 1)
	assert.Equal(t, "kept", format.Integration[0].Name)
	assert.Equal(t, "existing", format.Integration[0].Template)
}

// TestBackupFormat_TemplateWithoutRepositoryIsAnError pins that format()
// reports a template whose repository is missing instead of dereferencing
// the nil name pointer.
func TestBackupFormat_TemplateWithoutRepositoryIsAnError(t *testing.T) {
	b := BackupDB{
		meta:          db.Project{ID: 1, Name: "p"},
		templates:     []db.Template{{ID: 10, Name: "orphan", RepositoryID: 999}},
		templateRoles: map[int][]db.TemplateRolePerm{},
	}

	_, err := b.format()
	assert.ErrorContains(t, err, "orphan")
}

// TestBackupFormat_SecretStoragesGetUniqueNames pins that two storages with
// one name export under distinct names: the table has no unique constraint
// on the name, while Verify on restore refuses the duplicate.
func TestBackupFormat_SecretStoragesGetUniqueNames(t *testing.T) {
	b := BackupDB{
		meta: db.Project{ID: 1, Name: "p"},
		secretStorages: []db.SecretStorage{
			{ID: 1, Name: "vault", Type: "vault"},
			{ID: 2, Name: "vault", Type: "vault"},
		},
		templateRoles: map[int][]db.TemplateRolePerm{},
	}

	b.makeUniqueNames()

	format, err := b.format()
	require.NoError(t, err)
	require.Len(t, format.SecretStorages, 2)
	assert.NotEqual(t, format.SecretStorages[0].Name, format.SecretStorages[1].Name)
	assert.NoError(t, format.Verify())
}

// TestBackupFormat_DropsSecretSurveyVarDefault pins that a stored default
// of a secret survey variable never reaches the backup file, whatever the
// row holds: validation rejects it on save, rows written earlier still exist.
func TestBackupFormat_DropsSecretSurveyVarDefault(t *testing.T) {
	surveyVars := `[{"name":"pw","type":"secret","default_value":"hunter2"},{"name":"host","default_value":"db"}]`
	b := BackupDB{
		meta:          db.Project{ID: 1, Name: "p"},
		repositories:  []db.Repository{{ID: 20, Name: "repo"}},
		templates:     []db.Template{{ID: 10, Name: "t", RepositoryID: 20, SurveyVarsJSON: &surveyVars}},
		templateRoles: map[int][]db.TemplateRolePerm{},
	}

	format, err := b.format()
	require.NoError(t, err)
	require.Len(t, format.Templates, 1)
	require.Len(t, format.Templates[0].SurveyVars, 2)

	assert.Nil(t, format.Templates[0].SurveyVars[0].DefaultValue)
	require.NotNil(t, format.Templates[0].SurveyVars[1].DefaultValue)
	assert.Equal(t, "db", format.Templates[0].SurveyVars[1].DefaultValue.String())

	str, err := format.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, str, "hunter2")
}

func TestMakeUniqueNames(t *testing.T) {
	items := []testItem{
		{Name: "Project"},
		{Name: "Solution"},
		{Name: "Project"},
		{Name: "Project"},
		{Name: "Project"},
		{Name: "Project"},
	}

	makeUniqueNames(items, func(item *testItem) string {
		return item.Name
	}, func(item *testItem, name string) {
		item.Name = name
	})

	assert.True(t, isUnique(items), "Not unique names")
}
