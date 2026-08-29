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
	if environment == "" {
		return nil
	}

	var vars map[string]any
	if err := json.Unmarshal([]byte(environment), &vars); err != nil {
		// Malformed JSON is reported where the environment is merged. Guessing
		// about it here would give the caller two different errors for one fault.
		return nil
	}

	for name, value := range vars {
		if err := rejectTemplatedInput(name, name); err != nil {
			return err
		}

		if err := rejectTemplatedInput(name, value); err != nil {
			return err
		}
	}

	return nil
}

// rejectTemplatedInput walks a decoded JSON value: a variable may hold an object
// or a list, and Ansible renders the strings nested inside those too.
func rejectTemplatedInput(name string, value any) error {
	switch v := value.(type) {
	case string:
		if strings.ContainsAny(v, "{}") {
			return common_errors.NewValidationError(
				"task variable \"" + name + "\" must not contain '{' or '}'")
		}
	case []any:
		for _, item := range v {
			if err := rejectTemplatedInput(name, item); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range v {
			if err := rejectTemplatedInput(name, key); err != nil {
				return err
			}

			if err := rejectTemplatedInput(name, item); err != nil {
				return err
			}
		}
	}

	return nil
}
