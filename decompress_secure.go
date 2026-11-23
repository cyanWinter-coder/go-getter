// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// secureJoinPath normalizes an archive entry name and joins it onto root,
// verifying that the resulting path stays inside root. It rejects absolute
// paths (including volume-name rooted paths on Windows) and any ".." parent
// traversal, so an entry can never be written outside of the destination.
func secureJoinPath(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("archive entry has an empty name")
	}

	// Disallow absolute paths outright; they must never be interpreted
	// relative to the filesystem root.
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("archive entry contains an absolute path: %s", name)
	}

	// Disallow parent traversal
	if containsDotDot(name) {
		return "", fmt.Errorf("entry contains '..': %s", name)
	}

	path := filepath.Join(root, name)

	// Defense in depth: verify the cleaned, joined path is still contained
	// within root.
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("failed to resolve archive entry %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("archive entry escapes the destination directory: %s", name)
	}

	return path, nil
}

// secureLinkTarget verifies that a symbolic link created at linkPath (which
// must already be validated inside root) with the given target resolves to a
// location that is still contained within root. The target is interpreted
// relative to the directory containing the link, exactly as the filesystem
// would resolve it.
func secureLinkTarget(root, linkPath, target string) error {
	if target == "" {
		return fmt.Errorf("archive link entry has an empty target")
	}

	// An absolute target would point outside of the destination tree.
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return fmt.Errorf("archive link target is an absolute path: %s", target)
	}

	// Resolve the target the way the filesystem would: relative to the
	// directory the link lives in.
	resolved := filepath.Join(filepath.Dir(linkPath), target)

	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return fmt.Errorf("failed to resolve archive link target %q: %w", target, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("archive link target escapes the destination directory: %s", target)
	}

	return nil
}

// extractTracker records every path created while decompressing an archive
// so that a failed batch can be rolled back completely, leaving no partial
// output behind.
type extractTracker struct {
	// created holds the paths created by this batch, in creation order.
	// Parents always appear before their children.
	created []string
}

// mkdirAll creates path and any missing parents with the given permissions,
// recording only the directories that did not already exist.
func (t *extractTracker) mkdirAll(path string, perm os.FileMode) error {
	// Walk up to the deepest existing ancestor, collecting the directories
	// that will be created.
	var missing []string
	p := path
	for {
		if _, err := os.Stat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}

	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}

	// Record in creation order: top-most ancestor first.
	for i := len(missing) - 1; i >= 0; i-- {
		t.created = append(t.created, missing[i])
	}
	return nil
}

// track records a path created by this batch (a file, symlink, or hardlink).
func (t *extractTracker) track(path string) {
	t.created = append(t.created, path)
}

// cleanup removes everything recorded so far, in reverse creation order, on
// a best-effort basis. Children are removed before their parents so that
// directories are empty when their turn comes.
func (t *extractTracker) cleanup() {
	for i := len(t.created) - 1; i >= 0; i-- {
		_ = os.Remove(t.created[i])
	}
	t.created = nil
}
