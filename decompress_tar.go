// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// untar is a shared helper for untarring an archive. The reader should provide
// an uncompressed view of the tar archive.
//
// Every path an entry would create is normalized and verified to stay inside
// dst before it is written, and symbolic/hard link targets are constrained to
// resolve inside dst as well. All paths created by this call are recorded in
// tracker so the caller can roll the batch back on error.
func untar(input io.Reader, dst, src string, dir bool, umask os.FileMode, fileSizeLimit int64, filesLimit int, tracker *extractTracker) error {
	tarR := tar.NewReader(input)
	done := false
	type dirInfo struct {
		hdr  *tar.Header
		path string
	}
	dirHdrs := []dirInfo{}
	now := time.Now()

	// root is the directory all entries must stay within.
	root := dst
	if !dir {
		root = filepath.Dir(dst)
	}

	var (
		fileSize   int64
		filesCount int
	)

	for {
		if filesLimit > 0 {
			filesCount++
			if filesCount > filesLimit {
				return fmt.Errorf("tar archive contains too many files: %d > %d", filesCount, filesLimit)
			}
		}

		hdr, err := tarR.Next()
		if err == io.EOF {
			if !done {
				// Empty archive
				return fmt.Errorf("empty archive: %s", src)
			}

			break
		}
		if err != nil {
			return err
		}

		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			// don't unpack extended headers as files
			continue
		}

		path := dst
		if dir {
			// Normalize the entry name and verify the final path stays
			// inside the destination root.
			path, err = secureJoinPath(root, hdr.Name)
			if err != nil {
				return err
			}
		}

		fileInfo := hdr.FileInfo()

		fileSize += fileInfo.Size()

		if fileSizeLimit > 0 && fileSize > fileSizeLimit {
			return fmt.Errorf("tar archive larger than limit: %d", fileSizeLimit)
		}

		if fileInfo.IsDir() {
			if !dir {
				return fmt.Errorf("expected a single file: %s", src)
			}

			// A directory, just make the directory and continue unarchiving...
			if err := tracker.mkdirAll(path, mode(0755, umask)); err != nil {
				return err
			}

			// Record the directory information so that we may set its attributes
			// after all files have been extracted
			dirHdrs = append(dirHdrs, dirInfo{hdr: hdr, path: path})

			continue
		}

		// There is no ordering guarantee that a file in a directory is
		// listed before the directory, so make sure the parent exists.
		if dir {
			if err := tracker.mkdirAll(filepath.Dir(path), mode(0755, umask)); err != nil {
				return err
			}
		}

		if dir && hdr.Typeflag == tar.TypeSymlink {
			// The link target must resolve to a location inside the
			// destination root.
			if err := secureLinkTarget(root, path, hdr.Linkname); err != nil {
				return err
			}
			_ = os.Remove(path)
			if err := os.Symlink(hdr.Linkname, path); err != nil {
				return err
			}
			tracker.track(path)
			continue
		}

		if dir && hdr.Typeflag == tar.TypeLink {
			// Hard link targets are relative to the archive root; the
			// joined path must stay inside the destination root.
			targetPath, err := secureJoinPath(root, hdr.Linkname)
			if err != nil {
				return err
			}
			_ = os.Remove(path)
			if err := os.Link(targetPath, path); err != nil {
				return err
			}
			tracker.track(path)
			continue
		}

		// We have a file. If we already decoded, then it is an error
		if !dir && done {
			return fmt.Errorf("expected a single file, got multiple: %s", src)
		}

		// Mark that we're done so future in single file mode errors
		done = true

		// Sanitize file permissions to avoid permissions escalation attacks
		mode := hdr.FileInfo().Mode()
		mode &= 0o777

		// Size limit is tracked using the returned file info.
		err = copyReader(path, tarR, mode, umask, 0)
		if err != nil {
			return err
		}
		tracker.track(path)

		// Set the access and modification time if valid, otherwise default to current time
		aTime := now
		mTime := now
		if hdr.AccessTime.Unix() > 0 {
			aTime = hdr.AccessTime
		}
		if hdr.ModTime.Unix() > 0 {
			mTime = hdr.ModTime
		}
		if err := os.Chtimes(path, aTime, mTime); err != nil {
			return err
		}
	}

	// Perform a final pass over extracted directories to update metadata
	for _, d := range dirHdrs {
		// Chmod the directory since they might be created before we know the mode flags
		if err := os.Chmod(d.path, mode(d.hdr.FileInfo().Mode(), umask)); err != nil {
			return err
		}
		// Set the mtime/atime attributes since they would have been changed during extraction
		aTime := now
		mTime := now
		if d.hdr.AccessTime.Unix() > 0 {
			aTime = d.hdr.AccessTime
		}
		if d.hdr.ModTime.Unix() > 0 {
			mTime = d.hdr.ModTime
		}
		if err := os.Chtimes(d.path, aTime, mTime); err != nil {
			return err
		}
	}

	return nil
}

// TarDecompressor is an implementation of Decompressor that can
// unpack tar files.
type TarDecompressor struct {
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

func (d *TarDecompressor) Decompress(dst, src string, dir bool, umask os.FileMode) (err error) {
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

	// File first
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return untar(f, dst, src, dir, umask, d.FileSizeLimit, d.FilesLimit, tracker)
}
