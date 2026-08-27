package db_lib

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// A source is read, never executed, so these bounds exist to keep a repository
// from turning a host-list request into an unbounded read.
const (
	maxInventoryFileBytes = 8 << 20

	// maxInventorySourceFiles bounds the regular files the walk parses, which
	// is what the parsing itself costs.
	maxInventorySourceFiles = 512

	// maxInventorySourceEntries bounds every path the walk touches, files,
	// directories and symlinks alike, which is what the walk costs regardless
	// of how much of it ends up parsed. It is deliberately looser than the
	// file budget: a wide directory inventory legitimately holds far more
	// directories than parsed files, and the two costs are not the same.
	maxInventorySourceEntries = 4096

	// maxInventorySourceDepth stops a symlink that points back at one of its
	// own ancestors. Such a link stays inside the checkout, so the containment
	// check below lets it through and only the depth bound ends the descent.
	maxInventorySourceDepth = 32
)

// isAlwaysIgnoredEntry reports whether Ansible skips a directory entry whatever
// the configuration says. From IGNORED_ALWAYS in ansible/inventory/manager.py,
// verified against ansible-core 2.21.3:
//
//	IGNORED_ALWAYS = [br"^\.", b"^host_vars$", b"^group_vars$", b"^vars_plugins$"]
//
// The lists this file matches against are held in functions rather than in
// package-level variables, which the project forbids.
func isAlwaysIgnoredEntry(name string) bool {
	switch name {
	case "host_vars", "group_vars", "vars_plugins":
		return true
	default:
		return false
	}
}

// hasIgnoredInventoryExt matches INVENTORY_IGNORE_EXTS as it resolves by
// default in ansible-core 2.21.3. Re-derive after an Ansible upgrade with:
//
//	python -c "from ansible import constants as C; print(C.INVENTORY_IGNORE_EXTS)"
//
// A project can override the setting in its ansible.cfg, which lives in the
// repository and not in the server's reach, so a directory inventory that
// relies on a custom list will disagree with what is reported here.
func hasIgnoredInventoryExt(name string) bool {
	for _, ext := range []string{
		".pyc", ".pyo", ".swp", ".bak", "~",
		".rpm", ".md", ".txt", ".rst",
		".orig", ".cfg", ".retry",
	} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}

	return false
}

// InventoryExecutableError reports an inventory that is a program rather than a
// file to read. Listing its hosts means running it, which only a runner may do.
type InventoryExecutableError struct {
	Path string
}

func (e InventoryExecutableError) Error() string {
	return fmt.Sprintf("inventory %q is executable, so its hosts can only be listed by running it", e.Path)
}

// InventoryOutsideRepositoryError reports a path that leaves the checkout it is
// supposed to be relative to.
type InventoryOutsideRepositoryError struct {
	Path string
}

func (e InventoryOutsideRepositoryError) Error() string {
	return fmt.Sprintf("inventory path %q leaves the repository", e.Path)
}

// ParseInventorySource reads an inventory from a checkout: relPath names either
// a regular file or a directory of inventory files, relative to rootDir.
//
// Nothing outside rootDir is read and nothing is ever executed. A directory is
// walked the way Ansible walks one, so that the hosts reported here are the
// hosts a play would address.
//
// No error text below carries a filesystem path from the server. Every one of
// them lands in the snapshot's error column, which every project member can
// read, and where the checkout sits is the server's business, not theirs. The
// paths that do appear are the relative ones the caller itself supplied.
func ParseInventorySource(rootDir string, relPath string) (InventoryHosts, error) {
	root, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return InventoryHosts{}, fmt.Errorf("the inventory checkout cannot be read")
	}

	target, err := resolveWithin(root, filepath.Join(root, relPath), relPath)
	if err != nil {
		return InventoryHosts{}, err
	}

	walker := &inventorySourceWalker{root: root, builder: newInventoryBuilder(), visitedDirs: map[string]bool{}}

	// The entry Ansible is handed is parsed whatever it is called; the ignore
	// rules apply only while descending into a directory.
	if err := walker.read(target, relPath, 0); err != nil {
		return InventoryHosts{}, err
	}

	return walker.builder.build()
}

// resolveWithin resolves a path and confirms it stays under root. Symlinks are
// followed first, so a link pointing out of the checkout is caught by where it
// lands rather than by where it sits.
func resolveWithin(root string, path string, reported string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// The raw error carries the server's absolute path. Only the
		// not-exist signal survives the rewrite, because readDir branches on
		// it to skip a dangling link instead of failing the directory.
		if os.IsNotExist(err) {
			return "", fmt.Errorf("inventory path %q cannot be resolved: %w", reported, fs.ErrNotExist)
		}

		return "", fmt.Errorf("inventory path %q cannot be resolved", reported)
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", InventoryOutsideRepositoryError{Path: reported}
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", InventoryOutsideRepositoryError{Path: reported}
	}

	return resolved, nil
}

type inventorySourceWalker struct {
	root    string
	builder *inventoryBuilder

	// visits is a budget shared by every path the walk touches, file or
	// directory. A chain of directories that each hold a few symlinks to
	// sibling directories forms a DAG rather than a tree: the number of
	// distinct root-to-leaf paths through it is the product of the branching
	// factors, which stays enormous well inside maxInventorySourceDepth even
	// though the directories themselves stay inside the checkout and never
	// revisit an ancestor. Counting only regular files, as the walk used to,
	// left that shape free to explore paths for as long as the caller waited.
	visits int

	// files counts only the regular files handed to the parser, so that the
	// cost of parsing stays bounded separately from the cost of walking.
	files int

	// visitedDirs remembers the canonical path of every directory already
	// walked (resolveWithin has already run EvalSymlinks on it, so distinct
	// symlinks that resolve to the same directory collapse to one key). The
	// second arrival at a directory returns immediately instead of repeating
	// its whole subtree, which is what turns the DAG back into a tree sized
	// by the number of distinct physical directories rather than the number
	// of paths that reach them.
	visitedDirs map[string]bool
}

func (w *inventorySourceWalker) read(path string, reported string, depth int) error {
	if depth > maxInventorySourceDepth {
		return fmt.Errorf("inventory directory %q is nested deeper than %d levels", reported, maxInventorySourceDepth)
	}

	w.visits++
	if w.visits > maxInventorySourceEntries {
		return fmt.Errorf("inventory holds more than %d entries", maxInventorySourceEntries)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inventory %q cannot be read", reported)
	}

	if info.IsDir() {
		if w.visitedDirs[path] {
			return nil
		}
		w.visitedDirs[path] = true

		return w.readDir(path, reported, depth)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("inventory %q is neither a file nor a directory", reported)
	}

	return w.readFile(path, reported, info)
}

func (w *inventorySourceWalker) readDir(path string, reported string, depth int) error {
	// ReadDir returns entries sorted by name, which is the order Ansible reads
	// them in. It matters: a later file can add hosts to a group an earlier one
	// opened.
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("inventory directory %q cannot be listed", reported)
	}

	for _, entry := range entries {
		if ansibleIgnoresEntry(entry.Name()) {
			continue
		}

		child, err := resolveWithin(w.root, filepath.Join(path, entry.Name()), filepath.Join(reported, entry.Name()))
		if err != nil {
			// A link that dangles or leaves the checkout is skipped rather than
			// failing the whole directory, because Ansible would likewise carry
			// on with the entries it can read.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			var outside InventoryOutsideRepositoryError
			if errors.As(err, &outside) {
				continue
			}

			return err
		}

		if err := w.read(child, filepath.Join(reported, entry.Name()), depth+1); err != nil {
			return err
		}
	}

	return nil
}

func (w *inventorySourceWalker) readFile(path string, reported string, info os.FileInfo) error {
	// An executable inventory is a dynamic inventory: Ansible would run it and
	// read its JSON. Running a program out of a repository is a runner's job,
	// never the server's, so it is refused here rather than parsed as text —
	// parsing a script as INI yields a plausible list of nonsense host names.
	if info.Mode().Perm()&0o111 != 0 {
		return InventoryExecutableError{Path: reported}
	}

	if info.Size() > maxInventoryFileBytes {
		return fmt.Errorf("inventory file %q is larger than %d bytes", reported, maxInventoryFileBytes)
	}

	w.files++
	if w.files > maxInventorySourceFiles {
		return fmt.Errorf("inventory holds more than %d files", maxInventorySourceFiles)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("inventory file %q cannot be read", reported)
	}

	// A script may be committed without its executable bit, so the shebang is
	// checked too.
	if bytes.HasPrefix(content, []byte("#!")) {
		return InventoryExecutableError{Path: reported}
	}

	if looksLikeYAMLInventory(content) {
		return feedYAMLInventory(w.builder, string(content))
	}

	return feedINIInventory(w.builder, string(content))
}

// ansibleIgnoresEntry reports whether Ansible skips a directory entry.
//
// Whatever it lets through is parsed as an inventory, so a file that merely
// happens to sit next to one (an extra_secrets.yml, say) contributes its
// root-level keys as group names. That is Ansible's own behaviour for a
// directory inventory rather than something this walk adds, and only names
// ever reach the snapshot, never values.
func ansibleIgnoresEntry(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}

	return isAlwaysIgnoredEntry(name) || hasIgnoredInventoryExt(name)
}

// looksLikeYAMLInventory picks the format the way it can be picked without
// running Ansible's plugin chain: a YAML inventory is always a mapping at its
// root, and an INI one never parses as one.
func looksLikeYAMLInventory(content []byte) bool {
	var root any

	if err := yaml.Unmarshal(content, &root); err != nil {
		return false
	}

	_, ok := normalizeMapping(root)

	return ok
}
