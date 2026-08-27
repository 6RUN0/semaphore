package db_lib

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realListing is the output of `ansible-inventory --list` on an inventory with
// host variables, group variables and nested groups, captured from
// ansible-core 2.21.3. It is kept verbatim because the point of these tests is
// what the real shape carries, secrets included.
const realListing = `{
    "_meta": {
        "hostvars": {
            "bastion": {
                "ansible_host": "10.0.0.1",
                "ansible_password": "s3cret"
            },
            "db01": {"env_token": "abcdef123456"},
            "web01": {"ansible_user": "deploy", "env_token": "abcdef123456"},
            "web02": {"env_token": "abcdef123456"}
        },
        "profile": "inventory_legacy"
    },
    "all": {"children": ["ungrouped", "prod"]},
    "db": {"hosts": ["db01"]},
    "prod": {"children": ["web", "db"]},
    "ungrouped": {"hosts": ["bastion"]},
    "web": {"hosts": ["web01", "web02"]}
}`

func TestParseInventoryListing(t *testing.T) {
	parsed, err := ParseInventoryListing([]byte(realListing))
	require.NoError(t, err)

	assert.Equal(t, []string{"bastion", "db01", "web01", "web02"}, parsed.Hosts)

	groups := map[string][]string{}
	for _, group := range parsed.Groups {
		groups[group.Name] = group.Hosts
	}

	assert.Equal(t, []string{"web01", "web02"}, groups["web"])
	assert.Equal(t, []string{"db01"}, groups["db"])
	assert.Equal(t, []string{"bastion"}, groups["ungrouped"])

	// prod holds no hosts of its own; its members come from its children.
	assert.Equal(t, []string{"db01", "web01", "web02"}, groups["prod"])
}

// This is the reason the reduction exists. The listing carries every variable
// of every host — a password on one, a group token copied onto all of them —
// and a snapshot is readable by any member of the project.
func TestParseInventoryListingDropsVariables(t *testing.T) {
	parsed, err := ParseInventoryListing([]byte(realListing))
	require.NoError(t, err)

	rendered, err := json.Marshal(parsed)
	require.NoError(t, err)

	for _, secret := range []string{
		"s3cret",
		"abcdef123456",
		"ansible_password",
		"ansible_host",
		"ansible_user",
		"env_token",
		"hostvars",
		"_meta",
	} {
		assert.NotContains(t, string(rendered), secret, "%q must not survive the reduction", secret)
	}
}

func TestParseInventoryListingRejectsMalformedOutput(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
	}{
		{"not json", "Traceback (most recent call last):\n"},
		{"a json array", `["web01"]`},
		{"a group that is not an object", `{"web": "web01"}`},
		{"hosts that are not strings", `{"web": {"hosts": [{"name": "web01"}]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseInventoryListing([]byte(tt.output))

			assert.Error(t, err)
		})
	}
}

func TestParseInventoryListingAcceptsEmptyInventory(t *testing.T) {
	parsed, err := ParseInventoryListing([]byte(`{"_meta": {"hostvars": {}}, "all": {"children": ["ungrouped"]}}`))

	require.NoError(t, err)
	assert.Empty(t, parsed.Hosts)
}

// A dynamic inventory can return far more hosts than anyone can pick from, and
// the result is written to the snapshot tables.
func TestParseInventoryListingRefusesOversizedResult(t *testing.T) {
	hosts := make([]string, maxInventoryHosts+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf(`"host-%d"`, i)
	}

	listing := fmt.Sprintf(`{"web": {"hosts": [%s]}}`, strings.Join(hosts, ","))

	_, err := ParseInventoryListing([]byte(listing))

	var tooLarge InventoryTooLargeError
	assert.ErrorAs(t, err, &tooLarge)
}

func TestParseInventoryListingRefusesOversizedOutput(t *testing.T) {
	_, err := ParseInventoryListing(make([]byte, maxInventoryListingBytes+1))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than")
}

// A resolved name is taken literally: ansible has already expanded any ranges,
// so expanding again could only corrupt a name that contains brackets.
func TestParseInventoryListingDoesNotExpandNames(t *testing.T) {
	parsed, err := ParseInventoryListing([]byte(`{"web": {"hosts": ["web[01:03]"]}}`))

	require.NoError(t, err)
	assert.Equal(t, []string{"web[01:03]"}, parsed.Hosts)
}
