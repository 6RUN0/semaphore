package db

import (
	"strings"

	"github.com/semaphoreui/semaphore/pkg/common_errors"
)

// ValidateAnsibleLimit rejects host patterns carrying Jinja delimiters.
//
// The limit does not only become the --limit argument, which Ansible never
// templates. It is also passed as the value of an extra variable
// (semaphore_vars.limit, plus the name in target_host_var when configured), and
// extra vars are rendered as templates: a pattern containing {{ ... }} is
// evaluated on the control node when the play is rendered, under the account
// running the task. Ansible renders the play's attributes before it selects
// hosts, so the value runs even when the pattern matches nothing.
//
// A brace never names a host, so rejecting the character costs no legitimate
// pattern: ranges (web[01:05]), exclusions (!db), intersections (web:&prod),
// wildcards (*.example.com) and regexes (~(web|db).*) all pass.
func ValidateAnsibleLimit(limit []string, objectName string) error {
	for _, host := range limit {
		if strings.ContainsAny(host, "{}") {
			return common_errors.NewValidationError(
				objectName + " limit must not contain '{' or '}': " + host)
		}
	}

	return nil
}
