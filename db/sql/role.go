package sql

import "github.com/semaphoreui/semaphore/db"

func (d *SqlDb) GetGlobalRoleBySlug(slug string) (db.Role, error) {
	var role db.Role
	err := d.selectOne(&role, "select * from `role` where slug=? and project_id is null", slug)
	return role, err
}

func (d *SqlDb) GetProjectRoles(projectID int) ([]db.Role, error) {
	var roles []db.Role
	_, err := d.selectAll(&roles, "select * from `role` where project_id=? order by name", projectID)
	return roles, err
}

func (d *SqlDb) GetGlobalRoles() ([]db.Role, error) {
	var roles []db.Role
	_, err := d.selectAll(&roles, "select * from `role` where project_id is null order by name")
	return roles, err
}

func (d *SqlDb) UpdateRole(role db.Role) error {
	_, err := d.exec(
		"update `role` set name=?, permissions=? where slug=?",
		role.Name,
		role.Permissions,
		role.Slug)
	return err
}

func (d *SqlDb) CreateRole(role db.Role) (db.Role, error) {
	_, err := d.insert(
		"",
		"insert into `role` (slug, name, permissions, project_id) values (?, ?, ?, ?)",
		role.Slug,
		role.Name,
		role.Permissions,
		role.ProjectID)

	if err != nil {
		return role, err
	}

	return role, nil
}

func (d *SqlDb) DeleteRole(slug string) error {
	res, err := d.exec("delete from `role` where slug=?", slug)
	return validateMutationResult(res, err)
}

func (d *SqlDb) GetProjectRole(projectID int, slug string) (db.Role, error) {
	var role db.Role
	err := d.selectOne(&role, "select * from `role` where slug=? and project_id=?", slug, projectID)
	return role, err
}

func (d *SqlDb) GetProjectOrGlobalRoleBySlug(projectID int, slug string) (db.Role, error) {
	var role db.Role
	err := d.selectOne(
		&role,
		"select * from `role` where slug=? and (project_id=? or project_id is null)",
		slug,
		projectID)
	return role, err
}

func (d *SqlDb) UpdateProjectRole(projectID int, role db.Role) error {
	// Existence is checked by a select: RowsAffected after UPDATE counts
	// changed rows on MySQL, so an idempotent update would look missing.
	if _, err := d.GetProjectRole(projectID, role.Slug); err != nil {
		return err
	}

	res, err := d.exec(
		"update `role` set name=?, permissions=? where slug=? and project_id=?",
		role.Name,
		role.Permissions,
		role.Slug,
		projectID)
	return validateMutationResult(res, err)
}

func (d *SqlDb) DeleteProjectRole(projectID int, slug string) error {
	res, err := d.exec("delete from `role` where slug=? and project_id=?", slug, projectID)
	if err != nil {
		return validateMutationResult(res, err)
	}
	return requireAffectedRow(res)
}

func (d *SqlDb) GetProjectRoleRefs(projectID int, slug string) ([]db.ObjectReferrer, error) {
	refs := make([]db.ObjectReferrer, 0)
	_, err := d.selectAll(&refs,
		"select t.id as id, t.name as name "+
			"from project__template_role r "+
			"join project__template t on t.id = r.template_id "+
			"where r.project_id = ? and r.role_slug = ? "+
			"order by t.name",
		projectID, slug)
	return refs, err
}
