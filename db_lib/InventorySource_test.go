package db_lib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cover what the differential tests cannot: the source lives in a
// repository somebody else controls, so the reader has to stay inside the
// checkout, refuse to run anything, and stop before an oversized tree.

func writeSourceFile(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func hostNames(t *testing.T, root string, relPath string) []string {
	t.Helper()

	parsed, err := ParseInventorySource(root, relPath)
	require.NoError(t, err)

	return parsed.Hosts
}

func TestParseInventorySourceReadsFile(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "hosts"), "[web]\nweb01\nweb02\n", 0600)

	assert.Equal(t, []string{"web01", "web02"}, hostNames(t, root, "hosts"))
}

func TestParseInventorySourceDetectsFormat(t *testing.T) {
	for _, tt := range []struct {
		name     string
		content  string
		expected []string
	}{
		{"ini", "[web]\nweb01\n", []string{"web01"}},
		{"yaml", "web:\n  hosts:\n    web01:\n", []string{"web01"}},
		{"yaml with a document marker", "---\nweb:\n  hosts:\n    web01:\n", []string{"web01"}},
		{"empty", "", []string{}},
		{"bare host list", "web01\nweb02\n", []string{"web01", "web02"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeSourceFile(t, filepath.Join(root, "hosts"), tt.content, 0600)

			parsed, err := ParseInventorySource(root, "hosts")
			require.NoError(t, err)
			assert.Equal(t, tt.expected, parsed.Hosts)
		})
	}
}

// A dynamic inventory is a program. Reporting it as unsupported is the whole
// point: parsed as text it would yield a plausible list of nonsense hosts, and
// somebody would run a play against them.
func TestParseInventorySourceRefusesExecutables(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{"executable bit", "[web]\nweb01\n", 0700},
		{"shebang without the bit", "#!/usr/bin/env python\nprint('{}')\n", 0600},
		{"group executable only", "[web]\nweb01\n", 0610},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeSourceFile(t, filepath.Join(root, "hosts"), tt.content, tt.mode)

			_, err := ParseInventorySource(root, "hosts")

			var executable InventoryExecutableError
			assert.ErrorAs(t, err, &executable)
		})
	}
}

func TestParseInventorySourceRefusesExecutableInsideDirectory(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "inv", "static"), "[web]\nweb01\n", 0600)
	writeSourceFile(t, filepath.Join(root, "inv", "dynamic"), "#!/bin/sh\necho '{}'\n", 0700)

	_, err := ParseInventorySource(root, "inv")

	var executable InventoryExecutableError
	require.ErrorAs(t, err, &executable)
	assert.Contains(t, executable.Path, "dynamic")
}

func TestParseInventorySourceStaysInsideRepository(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("[web]\nsecret01\n"), 0600))

	t.Run("a relative path that climbs out", func(t *testing.T) {
		root := t.TempDir()
		writeSourceFile(t, filepath.Join(root, "hosts"), "[web]\nweb01\n", 0600)

		_, err := ParseInventorySource(root, filepath.Join("..", filepath.Base(outside), "secret"))

		assert.Error(t, err)
	})

	t.Run("a symlink that points out", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "hosts")))

		_, err := ParseInventorySource(root, "hosts")

		var escaped InventoryOutsideRepositoryError
		assert.ErrorAs(t, err, &escaped)
	})

	t.Run("a symlink inside a directory is skipped, the rest is read", func(t *testing.T) {
		root := t.TempDir()
		writeSourceFile(t, filepath.Join(root, "inv", "real"), "[web]\nweb01\n", 0600)
		require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "inv", "escape")))

		parsed, err := ParseInventorySource(root, "inv")

		require.NoError(t, err)
		assert.Equal(t, []string{"web01"}, parsed.Hosts)
		assert.NotContains(t, parsed.Hosts, "secret01")
	})
}

// A symlink to its own parent stays inside the checkout, so containment lets it
// through. The walker remembers which physical directory it already visited,
// so the loop is a no-op rather than a failure of the whole parse: the valid
// "hosts" file next to it is still read.
func TestParseInventorySourceSkipsSymlinkLoop(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "inv", "hosts"), "[web]\nweb01\n", 0600)
	require.NoError(t, os.Symlink(filepath.Join(root, "inv"), filepath.Join(root, "inv", "loop")))

	parsed, err := ParseInventorySource(root, "inv")

	require.NoError(t, err)
	assert.Equal(t, []string{"web01"}, parsed.Hosts)
}

// A chain of distinct real directories never repeats a canonical path, so the
// visited-directory dedup cannot short-circuit it; only the depth bound stops
// the descent.
func TestParseInventorySourceStopsAtMaxDepth(t *testing.T) {
	root := t.TempDir()

	path := filepath.Join(root, "inv")
	for i := 0; i <= maxInventorySourceDepth+1; i++ {
		path = filepath.Join(path, fmt.Sprintf("d%d", i))
	}
	writeSourceFile(t, filepath.Join(path, "hosts"), "[web]\nweb01\n", 0600)

	_, err := ParseInventorySource(root, "inv")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nested deeper")
}

// Each directory here holds several symlinks that all resolve to the same
// child directory, which is a DAG rather than a tree: without deduplicating
// visited directories, the number of root-to-leaf paths through it is the
// product of the branching factor at every level (3^14 here), all while
// staying well inside maxInventorySourceDepth because no link ever points
// back at an ancestor. That let a handful of directories cost minutes of CPU
// under the checkoutLock before this fix. The 5s budget is generous: a fixed
// walker finishes in milliseconds, an unfixed one does not finish in it.
func TestParseInventorySourceStopsSymlinkDAGFanout(t *testing.T) {
	root := t.TempDir()

	const levels = 14
	const fanout = 3

	hubs := filepath.Join(root, "hubs")

	for i := 0; i < levels; i++ {
		hubDir := filepath.Join(hubs, fmt.Sprintf("hub%d", i))
		require.NoError(t, os.MkdirAll(hubDir, 0700))

		if i == levels-1 {
			writeSourceFile(t, filepath.Join(hubDir, "hosts"), "[web]\nweb01\n", 0600)
			continue
		}

		next := filepath.Join("..", fmt.Sprintf("hub%d", i+1))
		for f := 0; f < fanout; f++ {
			name := fmt.Sprintf("link%d", f)
			require.NoError(t, os.Symlink(next, filepath.Join(hubDir, name)))
		}
	}

	require.NoError(t, os.Symlink(filepath.Join(hubs, "hub0"), filepath.Join(root, "inv")))

	type result struct {
		parsed InventoryHosts
		err    error
	}

	done := make(chan result, 1)

	go func() {
		parsed, err := ParseInventorySource(root, "inv")
		done <- result{parsed, err}
	}()

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, []string{"web01"}, r.parsed.Hosts)
	case <-time.After(5 * time.Second):
		t.Fatal("ParseInventorySource did not return within the budget: a symlink DAG must not cause exponential fan-out")
	}
}

func TestParseInventorySourceSkipsDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "inv", "real"), "[web]\nweb01\n", 0600)
	require.NoError(t, os.Symlink(filepath.Join(root, "inv", "gone"), filepath.Join(root, "inv", "broken")))

	parsed, err := ParseInventorySource(root, "inv")

	require.NoError(t, err)
	assert.Equal(t, []string{"web01"}, parsed.Hosts)
}

func TestParseInventorySourceRefusesOversizedFile(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "hosts"), strings.Repeat("h", maxInventoryFileBytes+1), 0600)

	_, err := ParseInventorySource(root, "hosts")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than")
}

func TestParseInventorySourceRefusesTooManyFiles(t *testing.T) {
	root := t.TempDir()

	for i := 0; i <= maxInventorySourceFiles; i++ {
		writeSourceFile(t, filepath.Join(root, "inv", fmt.Sprintf("part-%04d", i)), "[web]\nweb01\n", 0600)
	}

	_, err := ParseInventorySource(root, "inv")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than")
}

// The budget bounds the whole source, not each file, so a directory cannot get
// past the limit by spreading generated names across many files.
func TestParseInventorySourceBudgetsWholeDirectory(t *testing.T) {
	root := t.TempDir()

	for i := 0; i < 4; i++ {
		writeSourceFile(
			t,
			filepath.Join(root, "inv", fmt.Sprintf("part-%d", i)),
			fmt.Sprintf("[g%d]\nhost%d-[1:8000]\n", i, i),
			0600,
		)
	}

	_, err := ParseInventorySource(root, "inv")

	var tooLarge InventoryTooLargeError
	assert.ErrorAs(t, err, &tooLarge)
}

func TestParseInventorySourceMergesDirectory(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "inv", "01-web"), "[web]\nweb01\n", 0600)
	writeSourceFile(t, filepath.Join(root, "inv", "02-web"), "[web]\nweb02\n", 0600)
	writeSourceFile(t, filepath.Join(root, "inv", "03-parent"), "[prod:children]\nweb\n", 0600)

	parsed, err := ParseInventorySource(root, "inv")
	require.NoError(t, err)

	assert.Equal(t, []string{"web01", "web02"}, parsed.Hosts)

	groups := map[string][]string{}
	for _, group := range parsed.Groups {
		groups[group.Name] = group.Hosts
	}

	assert.Equal(t, []string{"web01", "web02"}, groups["web"])
	assert.Equal(t, []string{"web01", "web02"}, groups["prod"], "a parent group collects its children's hosts")
}

func TestAnsibleIgnoresEntry(t *testing.T) {
	for _, tt := range []struct {
		name     string
		expected bool
	}{
		{"hosts", false},
		{"hosts.yml", false},
		{"hosts.yaml", false},
		{"hosts.ini", false},
		{".hidden", true},
		{"group_vars", true},
		{"host_vars", true},
		{"vars_plugins", true},
		{"notes.md", true},
		{"list.txt", true},
		{"readme.rst", true},
		{"old.bak", true},
		{"editor~", true},
		{"ansible.cfg", true},
		{"run.retry", true},
		{"module.pyc", true},
		{"package.rpm", true},
		// The name has to end with the extension; it is not a substring match.
		{"md.hosts", false},
		{"group_vars_extra", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ansibleIgnoresEntry(tt.name))
		})
	}
}

// Every error this path returns is copied into the snapshot's error column,
// which every member of the project can read. Where the checkout sits on the
// server is not theirs to learn, so no message may carry an absolute path.
func TestParseInventorySourceKeepsServerPathsOutOfErrors(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "inventory", "hosts"), "web01\n", 0600)

	t.Run("missing entry", func(t *testing.T) {
		_, err := ParseInventorySource(root, "nowhere")

		require.Error(t, err)
		assert.NotContains(t, err.Error(), root)
	})

	t.Run("missing checkout", func(t *testing.T) {
		_, err := ParseInventorySource(filepath.Join(root, "gone"), "hosts")

		require.Error(t, err)
		assert.NotContains(t, err.Error(), root)
	})

	t.Run("unreadable directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a directory whatever its mode says")
		}

		dir := filepath.Join(root, "inventory")
		require.NoError(t, os.Chmod(dir, 0000))
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

		_, err := ParseInventorySource(root, "inventory")

		require.Error(t, err)
		assert.NotContains(t, err.Error(), root)
	})

	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its mode says")
		}

		writeSourceFile(t, filepath.Join(root, "secret"), "web01\n", 0000)
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "secret"), 0600) })

		_, err := ParseInventorySource(root, "secret")

		require.Error(t, err)
		assert.NotContains(t, err.Error(), root)
	})
}

// An inventory whose path inside the repository is empty used to resolve to the
// checkout root and be walked as a directory inventory, turning the top-level
// keys of every YAML file in the repository into group names.
func TestParseInventorySourceRefusesAnEmptyPath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "unrelated.yml"), []byte("secrets:\n  password: hunter2\n"), 0o600))

	for _, relPath := range []string{"", "   "} {
		_, err := ParseInventorySource(root, relPath)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no path inside its repository")
	}
}

// Ansible picks the inventory plugin by extension before it looks at the
// content: the yaml plugin's verify_file claims a file only when the extension
// is empty or one of YAML_FILENAME_EXTENSIONS (.yml, .yaml, .json), so an
// inventory called hosts.ini never reaches it however it is written. Where the
// extension leaves the choice open, the content decides, and a document that
// declares no group at all is not a YAML inventory.
func TestParseInventorySourceChoosesFormatLikeAnsible(t *testing.T) {
	// An entry an INI inventory can plausibly hold, which also decodes as a
	// one-key YAML mapping whose value is a scalar rather than a group.
	ambiguous := "web1 ansible_host: 10.0.0.1\n"
	yamlInventory := "web:\n  hosts:\n    web01:\n"

	for _, tt := range []struct {
		name     string
		filename string
		content  string
		hosts    []string
		groups   []string
	}{
		{"no extension reads an ini entry as ini", "hosts", ambiguous, []string{"web1"}, []string{"ungrouped"}},
		{"an ini extension is never yaml", "hosts.ini", ambiguous, []string{"web1"}, []string{"ungrouped"}},
		{"a yaml extension stays yaml", "hosts.yml", ambiguous, []string{}, []string{}},
		{"no extension still reads a yaml inventory as yaml", "hosts", yamlInventory, []string{"web01"}, []string{"web"}},
		{"a yaml extension keeps a yaml inventory", "inv.yaml", yamlInventory, []string{"web01"}, []string{"web"}},
		{"json counts as yaml", "inv.json", `{"web": {"hosts": {"web01": null}}}`, []string{"web01"}, []string{"web"}},
		{"an ini extension keeps an ini inventory", "hosts.ini", "[web]\nweb01\n", []string{"web01"}, []string{"web"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeSourceFile(t, filepath.Join(root, tt.filename), tt.content, 0600)

			parsed, err := ParseInventorySource(root, tt.filename)

			require.NoError(t, err)
			assert.Equal(t, tt.hosts, parsed.Hosts)
			assert.Equal(t, tt.groups, inventoryGroupNames(parsed))
		})
	}
}
