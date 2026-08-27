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

// A directory inventory is read by rules that live in Ansible's source and
// change between releases, so agreement is measured against the real thing
// rather than asserted from a reading of it. These tests skip without an
// ansible-inventory binary; see ansibleInventoryBinary.

// writeInventoryTree materialises a directory fixture, where each key is a path
// relative to the root. A key ending in "/" makes an empty directory.
func writeInventoryTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range files {
		path := filepath.Join(root, name)

		if content == "" && name[len(name)-1] == '/' {
			require.NoError(t, os.MkdirAll(path, 0700))
			continue
		}

		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	}

	return root
}

func TestSourceAgreesWithAnsibleOnDirectories(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	for _, tt := range []struct {
		name  string
		files map[string]string
	}{
		{
			name: "groups merge across files",
			files: map[string]string{
				"01-web": "[web]\nweb01\nweb02\n",
				"02-db":  "[db]\ndb01\n",
			},
		},
		{
			name: "a later file adds hosts to an earlier group",
			files: map[string]string{
				"01-first":  "[web]\nweb01\n",
				"02-second": "[web]\nweb02\nweb03\n",
			},
		},
		{
			name: "children resolve across files",
			files: map[string]string{
				"01-groups":   "[web]\nweb01\n\n[db]\ndb01\n",
				"02-children": "[prod:children]\nweb\ndb\n",
			},
		},
		{
			name: "ignored extensions contribute nothing",
			files: map[string]string{
				"hosts":       "[web]\nweb01\n",
				"notes.md":    "[ghost]\nmd01\n",
				"list.txt":    "[ghost]\ntxt01\n",
				"old.bak":     "[ghost]\nbak01\n",
				"editor~":     "[ghost]\ntilde01\n",
				"ansible.cfg": "[ghost]\ncfg01\n",
				"run.retry":   "[ghost]\nretry01\n",
				"readme.rst":  "[ghost]\nrst01\n",
			},
		},
		{
			name: "hidden entries are skipped",
			files: map[string]string{
				"hosts":         "[web]\nweb01\n",
				".hidden-hosts": "[ghost]\nhidden01\n",
			},
		},
		{
			// The planted files are valid YAML inventories, so anything that
			// read them would report ghost hosts — that is what makes skipping
			// these directories observable. They must stay valid YAML: Ansible
			// does load group_vars and host_vars as variable files, and INI
			// content there fails the whole run rather than being ignored.
			name: "variable directories are skipped",
			files: map[string]string{
				"hosts":             "[web]\nweb01\n",
				"group_vars/web":    "ghost:\n  hosts:\n    groupvar01:\n",
				"host_vars/web01":   "ghost:\n  hosts:\n    hostvar01:\n",
				"vars_plugins/more": "ghost:\n  hosts:\n    plugin01:\n",
			},
		},
		{
			name: "subdirectories are read recursively",
			files: map[string]string{
				"top":              "[web]\nweb01\n",
				"nested/more":      "[db]\ndb01\n",
				"nested/deep/even": "[cache]\ncache01\n",
			},
		},
		{
			name: "yaml and ini mix in one directory",
			files: map[string]string{
				"01-ini.yml": "web:\n  hosts:\n    web01:\n",
				"02-plain":   "[db]\ndb01\n",
			},
		},
		{
			name: "a range expands inside a directory",
			files: map[string]string{
				"hosts": "[web]\nweb[01:04].example.com\n",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := writeInventoryTree(t, tt.files)

			expected := ansibleHostSets(t, binary, root)

			parsed, err := ParseInventorySource(root, ".")
			require.NoError(t, err)

			assert.Equal(t, expected, groupHostSets(parsed))
		})
	}
}

// Ansible parses whatever source it is handed; the ignore rules apply only to
// the entries it finds while descending a directory. A file named hosts.txt is
// therefore read when named directly and skipped when found in a directory.
func TestSourceAgreesWithAnsibleOnDirectlyNamedIgnoredFile(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	root := writeInventoryTree(t, map[string]string{
		"hosts.txt": "[web]\nweb01\n",
	})

	expected := ansibleHostSets(t, binary, filepath.Join(root, "hosts.txt"))

	parsed, err := ParseInventorySource(root, "hosts.txt")
	require.NoError(t, err)

	assert.Equal(t, expected, groupHostSets(parsed))
	assert.Contains(t, expected, "web", "ansible should read a directly named .txt source")
}

// The picker must show the same hosts whichever route produced them: the
// server parsing an inventory it can read, or a runner reducing what
// ansible-inventory reported for one it cannot. This pins the two against each
// other on the same source.
func TestListingReductionAgreesWithParser(t *testing.T) {
	binary := ansibleInventoryBinary(t)

	for _, tt := range []struct {
		name    string
		content string
	}{
		{"groups and ungrouped hosts", "bastion\n\n[web]\nweb01\nweb02\n"},
		{"nested children", "[web]\nweb01\n\n[db]\ndb01\n\n[prod:children]\nweb\ndb\n"},
		{"a range", "[web]\nweb[01:04].example.com\n"},
		{"host and group variables", "bastion ansible_password=s3cret\n\n[web]\nweb01\n\n[web:vars]\ntoken=abc\n"},
		{"a host in two groups", "[web]\nshared\n\n[db]\nshared\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := writeInventoryTree(t, map[string]string{"hosts": tt.content})
			path := filepath.Join(root, "hosts")

			out, err := exec.Command(binary, "-i", path, "--list").Output()
			require.NoError(t, err)

			reduced, err := ParseInventoryListing(out)
			require.NoError(t, err)

			parsed, err := ParseINIInventory(tt.content)
			require.NoError(t, err)

			assert.Equal(t, parsed.Hosts, reduced.Hosts)
			assert.Equal(t, groupHostSets(parsed), groupHostSets(reduced))

			// Whatever the source carried, the reduction of the real listing
			// keeps none of it.
			assert.NotContains(t, string(mustJSON(t, reduced)), "s3cret")
			assert.NotContains(t, string(mustJSON(t, reduced)), "abc")
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	out, err := json.Marshal(value)
	require.NoError(t, err)

	return out
}
