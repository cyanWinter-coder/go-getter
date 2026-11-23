// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// osRename is os.Rename. It is a variable so that tests can simulate
// publish failures.
var osRename = os.Rename

// stagedPath is a staging area for content that is downloaded (or
// decompressed) next to its final destination and only published once it
// is complete and verified. Because the staging area is a sibling of the
// destination, publishing is a rename within the same filesystem and a
// failed or canceled download never modifies the destination.
type stagedPath struct {
	// dir is the staging directory, created next to dst.
	dir string
	// path is the staged content inside dir, mirroring dst's base name.
	path string
	// dst is the final destination the content is published to.
	dst string
}

// stage creates a staging area for content that will eventually be
// published to dst. The staging directory is created next to dst so that
// publishing is guaranteed to be a rename on the same filesystem.
func stage(dst string) (*stagedPath, error) {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, fmt.Errorf("error creating destination directory: %w", err)
	}
	dir, err := os.MkdirTemp(parent, ".getter-staging-")
	if err != nil {
		return nil, fmt.Errorf("error creating staging directory: %w", err)
	}
	return &stagedPath{
		dir:  dir,
		path: filepath.Join(dir, filepath.Base(dst)),
		dst:  dst,
	}, nil
}

// cleanup removes the staging area. It is safe to call after a successful
// publish, in which case only the empty staging directory is removed.
func (s *stagedPath) cleanup() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
		s.dir = ""
	}
}

// seed copies an existing regular file at the destination into the staging
// area, so that getters which support resuming an interrupted download
// (e.g. http range requests) can continue where they left off. The
// destination itself is only read, never modified; anything that is not a
// regular file (symlinks, directories, ...) is left for publish to
// replace wholesale.
func (s *stagedPath) seed() error {
	fi, err := os.Lstat(s.dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("error examining destination %s: %w", s.dst, err)
	}
	if !fi.Mode().IsRegular() {
		return nil
	}

	src, err := os.Open(s.dst)
	if err != nil {
		return fmt.Errorf("error opening existing destination %s: %w", s.dst, err)
	}
	defer func() { _ = src.Close() }()

	out, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return fmt.Errorf("error seeding staging area from %s: %w", s.dst, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, src); err != nil {
		return fmt.Errorf("error seeding staging area from %s: %w", s.dst, err)
	}
	return nil
}

// publish moves the staged content to its destination using renames within
// the same filesystem. If the destination already exists it is first moved
// aside and restored if the swap fails, so a failed publish always
// preserves the previous destination.
func (s *stagedPath) publish() error {
	if _, err := os.Lstat(s.dst); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("error examining destination %s: %w", s.dst, err)
		}
		// Nothing at the destination: a single rename publishes.
		if err := osRename(s.path, s.dst); err != nil {
			return fmt.Errorf("error publishing %s: %w", s.dst, err)
		}
		return nil
	}

	// The destination exists: move it aside so it can be restored if
	// publishing the staged content fails.
	backupDir, err := os.MkdirTemp(filepath.Dir(s.dst), ".getter-backup-")
	if err != nil {
		return fmt.Errorf("error creating backup directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(backupDir) }()

	backup := filepath.Join(backupDir, filepath.Base(s.dst))
	if err := osRename(s.dst, backup); err != nil {
		return fmt.Errorf("error moving aside existing destination %s: %w", s.dst, err)
	}

	if err := osRename(s.path, s.dst); err != nil {
		// Restore the previous destination.
		if rerr := osRename(backup, s.dst); rerr != nil {
			return fmt.Errorf(
				"error publishing %s: %w; additionally failed to restore the previous destination: %v",
				s.dst, err, rerr)
		}
		return fmt.Errorf("error publishing %s: %w", s.dst, err)
	}

	return nil
}
