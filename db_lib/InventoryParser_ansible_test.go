package db_lib

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The differential tests below compare the parser against the tool whose
// behaviour it has to reproduce. They need an ansible-inventory binary, so on a
// machine without one they skip, and are opt-in through PATH or
// SEMAPHORE_TEST_ANSIBLE_INVENTORY.
//
// Only host sets are compared, not the children lists: ansible-inventory
// reports the implicit "all" and "ungrouped" groups in its own shape, while
// what has to agree is which hosts `--limit <group>` selects.

// ansibleRequiredEnv makes the absence of the binary a failure instead of a
// skip. Somewhere that is meant to run these — CI above all — has to set it:
// a skipped test still prints "ok", so without it an environment that quietly
// stops running them looks exactly like one that runs them and passes.
const ansibleRequiredEnv = "SEMAPHORE_TEST_ANSIBLE_REQUIRED"

func ansibleInventoryBinary(t *testing.T) string {
	t.Helper()

	if path := os.Getenv("SEMAPHORE_TEST_ANSIBLE_INVENTORY"); path != "" {
		return path
	}

	path, err := exec.LookPath("ansible-inventory")
	if err != nil {
		missing := "ansible-inventory not found; set SEMAPHORE_TEST_ANSIBLE_INVENTORY to run the differential tests"

		if os.Getenv(ansibleRequiredEnv) != "" {
			t.Fatal(missing + " (" + ansibleRequiredEnv + " is set, so skipping them is not allowed here)")
		}

		t.Skip(missing)
	}

	return path
}

// runAnsibleInventory writes content to a temporary inventory file and returns
// the host sets ansible resolves from it, keyed by group name.
func runAnsibleInventory(t *testing.T, binary string, filename string, content string) map[string][]string {
	t.Helper()

	path := filepath.Join(t.TempDir(), filename)
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))

	return ansibleHostSets(t, binary, path)
}

// ansibleHostSets asks ansible to resolve an inventory source — a file or a
// directory — and reduces its listing to the hosts each group selects.
func ansibleHostSets(t *testing.T, binary string, path string) map[string][]string {
	t.Helper()

	out, err := exec.Command(binary, "-i", path, "--list").Output()
	require.NoError(t, err, "ansible-inventory failed on the fixture")

	var listing map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &listing))

	type groupNode struct {
		Hosts    []string `json:"hosts"`
		Children []string `json:"children"`
	}

	nodes := map[string]groupNode{}

	for name, raw := range listing {
		if name == "_meta" {
			continue
		}

		var node groupNode
		require.NoError(t, json.Unmarshal(raw, &node))

		nodes[name] = node
	}

	var resolve func(name string, into map[string]bool, visited map[string]bool)
	resolve = func(name string, into map[string]bool, visited map[string]bool) {
		if visited[name] {
			return
		}

		visited[name] = true

		for _, host := range nodes[name].Hosts {
			into[host] = true
		}

		for _, child := range nodes[name].Children {
			resolve(child, into, visited)
		}
	}

	result := map[string][]string{}

	for name := range nodes {
		hosts := map[string]bool{}
		resolve(name, hosts, map[string]bool{})

		if name != "all" && len(hosts) == 0 {
			// A group with no hosts selects nothing, and ansible reports
			// placeholder groups the parser has no reason to invent.
			continue
		}

		result[name] = sortedKeys(hosts)
	}

	return result
}

// groupHostSets reduces a parse result to the same shape, so that the two can
// be compared directly.
func groupHostSets(inventory InventoryHosts) map[string][]string {
	result := map[string][]string{"all": inventory.Hosts}

	for _, group := range inventory.Groups {
		if len(group.Hosts) == 0 {
			continue
		}

		result[group.Name] = group.Hosts
	}

	return result
}

func TestParserAgreesWithAnsibleOnINI(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	tests := []struct {
		name    string
		content string
	}{
		{
			name: "groups and ungrouped hosts",
			content: `
bastion.example.com

[web]
web01.example.com
web02.example.com

[db]
db01.example.com
`,
		},
		{
			name: "children and vars",
			content: `
[web]
web01

[db]
db01

[prod:children]
web
db

[prod:vars]
ansible_user=deploy
`,
		},
		{
			name: "nested children",
			content: `
[web]
web01

[prod:children]
web

[dc1:children]
prod
`,
		},
		{
			name: "zero padded range",
			content: `
[web]
web[01:04].example.com
`,
		},
		{
			name: "range with a step",
			content: `
[web]
node[1:9:3]
`,
		},
		{
			name: "alphabetic range",
			content: `
[web]
host[a:e]
`,
		},
		{
			name: "two ranges combine",
			content: `
[web]
r[1:2]-n[a:c]
`,
		},
		{
			name: "host variables",
			content: `
[web]
web01 ansible_host=10.0.0.1 ansible_port=22
web02 ansible_user=deploy
`,
		},
		{
			name: "a host in two groups",
			content: `
[web]
web01

[prod]
web01
`,
		},
		{
			name: "comments",
			content: `
# hash
; semicolon
[web]
web01
`,
		},
		{
			name: "an omitted begin bound",
			content: `
[web]
web[:3]
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseINIInventory(tt.content)
			require.NoError(t, err)

			expected := runAnsibleInventory(t, binary, "inventory.ini", tt.content)

			assert.Equal(t, expected, groupHostSets(parsed))
		})
	}
}

func TestParserAgreesWithAnsibleOnYAML(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	tests := []struct {
		name    string
		content string
	}{
		{
			name: "all with hosts and nested children",
			content: `
all:
  hosts:
    bastion.example.com:
  children:
    web:
      hosts:
        web01:
        web02:
          ansible_host: 10.0.0.2
    prod:
      children:
        web:
      vars:
        env: prod
`,
		},
		{
			name: "top level groups",
			content: `
web:
  hosts:
    web01:
    web02:
db:
  hosts:
    db01:
`,
		},
		{
			name: "ranges in host keys",
			content: `
web:
  hosts:
    "web[01:04]":
`,
		},
		{
			name: "vars carry no hosts",
			content: `
web:
  vars:
    ansible_user: deploy
  hosts:
    web01:
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseYAMLInventory(tt.content)
			require.NoError(t, err)

			expected := runAnsibleInventory(t, binary, "inventory.yml", tt.content)

			assert.Equal(t, expected, groupHostSets(parsed))
		})
	}
}
