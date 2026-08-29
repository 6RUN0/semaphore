package db

import (
	"encoding/json"
	"strings"

	"github.com/semaphoreui/semaphore/pkg/common_errors"
)

// ValidateTaskEnvironment rejects Jinja delimiters in the variables supplied
// with an Ansible task.
//
// Task.Environment is bound straight from the request body and merged over the
// project's own variables, so anyone permitted to run a task can define any
// variable with any content. Extra vars are rendered as templates, which turns
// {{ ... }} in a value into code executed on the control node under the account
// running the task — the same defect ValidateAnsibleLimit covers on the other
// input reaching that path.
//
// Only Ansible tasks are checked: the risk is Jinja rendering, and the other
// apps pass these variables as plain -var or NAME=value arguments, where a brace
// is just a character.
//
// This is deliberately narrower than Ansible's own answer, which is to mark user
// input unsafe so that it is never rendered at all. Doing that means emitting the
// extra vars as tagged YAML instead of JSON, changing the argument for every task
// and unverifiable without running Ansible; rejecting a brace closes the same
// hole and costs only the rare variable whose value is meant to be a template.
func ValidateTaskEnvironment(environment string) error {
	return validateTaskVars(environment, "task variable", containsBrace)
}

// ValidateTaskSecret applies the same checks to Task.Secret, which is a second
// map of extra vars from the same request body and reaches --extra-vars by the
// same path.
//
// Its values are held to the narrower rule: a secret is usually a password, and
// a brace is an ordinary character in one, while "{{", "{%" and "{#" are the
// only sequences Jinja acts on. A password containing a lone brace therefore
// keeps working, and a value that would be executed does not.
func ValidateTaskSecret(secret string) error {
	return validateTaskVars(secret, "task secret", containsJinjaDelimiter)
}

// validateTaskVars walks a JSON object of task variables, rejecting names
// Ansible owns and values the given predicate objects to.
func validateTaskVars(content string, subject string, rejectValue func(string) bool) error {
	if content == "" {
		return nil
	}

	var vars map[string]any
	if err := json.Unmarshal([]byte(content), &vars); err != nil {
		// Malformed JSON is reported where the environment is merged. Guessing
		// about it here would give the caller two different errors for one fault.
		return nil
	}

	for name, value := range vars {
		if err := rejectAnsibleName(subject, name); err != nil {
			return err
		}

		if err := rejectTemplatedInput(subject, name, name, rejectValue); err != nil {
			return err
		}

		if err := rejectTemplatedInput(subject, name, value, rejectValue); err != nil {
			return err
		}
	}

	return nil
}

// rejectAnsibleName refuses a variable that would overwrite Ansible's own
// connection settings.
//
// Extra vars have the highest precedence there is, so ansible_ssh_common_args
// with a ProxyCommand, or ansible_connection set to local, redirects the run
// itself — code executed on the control node without a single brace, which the
// delimiter checks above cannot see. The same prefix is already refused for
// target_host_var, for the same reason.
//
// Variables of a project environment are not touched by this: those are set by
// somebody who administers the project, not by whoever runs the task.
func rejectAnsibleName(subject string, name string) error {
	if !strings.HasPrefix(name, "ansible_") {
		return nil
	}

	return common_errors.NewValidationError(
		subject + " \"" + name + "\" must not start with 'ansible_': " +
			"those names carry Ansible's own connection settings and would redirect the run")
}

func containsBrace(value string) bool {
	return strings.ContainsAny(value, "{}")
}

// containsJinjaDelimiter reports whether Jinja would evaluate anything in the
// value: those three sequences are what opens an expression, a statement and a
// comment.
func containsJinjaDelimiter(value string) bool {
	return strings.Contains(value, "{{") ||
		strings.Contains(value, "{%") ||
		strings.Contains(value, "{#")
}

// rejectTemplatedInput walks a decoded JSON value: a variable may hold an object
// or a list, and Ansible renders the strings nested inside those too.
func rejectTemplatedInput(subject string, name string, value any, reject func(string) bool) error {
	switch v := value.(type) {
	case string:
		if reject(v) {
			return common_errors.NewValidationError(
				subject + " \"" + name + "\" must not contain a template expression")
		}
	case []any:
		for _, item := range v {
			if err := rejectTemplatedInput(subject, name, item, reject); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range v {
			if err := rejectTemplatedInput(subject, name, key, reject); err != nil {
				return err
			}

			if err := rejectTemplatedInput(subject, name, item, reject); err != nil {
				return err
			}
		}
	}

	return nil
}
