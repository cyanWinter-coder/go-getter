// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxLinkTargetSize bounds the size of a symbolic link target stored in a
// zip entry. Anything larger is rejected as bogus.
const maxLinkTargetSize = 8192

// ZipDecompressor is an implementation of Decompressor that can
// decompress zip files.
type ZipDecompressor struct {
	// FileSizeLimit limits the total size of all
	// decompressed files.
	//
	// The zero value means no limit.
	FileSizeLimit int64

	// FilesLimit limits the number of files that are
	// allowed to be decompressed.
	//
	// The zero value means no limit.
	FilesLimit int
}

func (d *ZipDecompressor) Decompress(dst, src string, dir bool, umask os.FileMode) (err error) {
	// Any security or integrity error aborts the batch and removes
	// everything this batch created.
	tracker := new(extractTracker)
	defer func() {
		if err != nil {
			tracker.cleanup()
		}
	}()

	// If we're going into a directory we should make that first
	mkdir := dst
	if !dir {
		mkdir = filepath.Dir(dst)
	}
	if err = tracker.mkdirAll(mkdir, mode(0755, umask)); err != nil {
		return err
	}

	// Open the zip
	zipR, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = zipR.Close() }()

	// Check the zip integrity
	if len(zipR.File) == 0 {
		// Empty archive
		return fmt.Errorf("empty archive: %s", src)
	}
	if !dir && len(zipR.File) > 1 {
		return fmt.Errorf("expected a single file: %s", src)
	}

	if d.FilesLimit > 0 && len(zipR.File) > d.FilesLimit {
		return fmt.Errorf("zip archive contains too many files: %d > %d", len(zipR.File), d.FilesLimit)
	}

	var fileSizeTotal int64

	// Go through and unarchive
	for _, f := range zipR.File {
		path := dst
		if dir {
			// Normalize the entry name and verify the final path stays
			// inside the destination root.
			path, err = secureJoinPath(dst, f.Name)
			if err != nil {
				return err
			}
		}

		fileInfo := f.FileInfo()

		fileSizeTotal += fileInfo.Size()

		if d.FileSizeLimit > 0 && fileSizeTotal > d.FileSizeLimit {
			return fmt.Errorf("zip archive larger than limit: %d", d.FileSizeLimit)
		}

		if fileInfo.IsDir() {
			if !dir {
				return fmt.Errorf("expected a single file: %s", src)
			}

			// A directory, just make the directory and continue unarchiving...
			if err := tracker.mkdirAll(path, mode(0755, umask)); err != nil {
				return err
			}

			continue
		}

		// Create the enclosing directories if we must. ZIP files aren't
		// required to contain entries for just the directories so this
		// can happen.
		if dir {
			if err := tracker.mkdirAll(filepath.Dir(path), mode(0755, umask)); err != nil {
				return err
			}
		}

		// Open the file for reading
		srcF, err := f.Open()
		if err != nil {
			if srcF != nil {
				_ = srcF.Close()
			}
			return err
		}

		// A symlink entry stores its target as the entry contents. The
		// target must resolve to a location inside the destination root.
		if dir && f.Mode()&os.ModeSymlink != 0 {
			target, err := readLinkTarget(srcF)
			_ = srcF.Close()
			if err != nil {
				return err
			}
			if err := secureLinkTarget(dst, path, target); err != nil {
				return err
			}
			_ = os.Remove(path)
			if err := os.Symlink(target, path); err != nil {
				return err
			}
			tracker.track(path)
			continue
		}

		// Size limit is tracked using the returned file info.

		// Sanitize file permissions to avoid permissions escalation attacks
		mode := f.Mode()
		mode &= 0o777

		err = copyReader(path, srcF, mode, umask, 0)
		_ = srcF.Close()
		if err != nil {
			return err
		}
		tracker.track(path)
	}

	return nil
}

// readLinkTarget reads a symbolic link target from a zip entry, bounding the
// size so a bogus entry cannot exhaust memory.
func readLinkTarget(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxLinkTargetSize+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxLinkTargetSize {
		return "", fmt.Errorf("archive link target too long: %d > %d", len(b), maxLinkTargetSize)
	}
	return string(b), nil
}
