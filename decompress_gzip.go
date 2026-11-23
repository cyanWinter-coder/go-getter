// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
)

// GzipDecompressor is an implementation of Decompressor that can
// decompress gzip files.
type GzipDecompressor struct {
	// FileSizeLimit limits the size of a decompressed file.
	//
	// The zero value means no limit.
	FileSizeLimit int64
}

func (d *GzipDecompressor) Decompress(dst, src string, dir bool, umask os.FileMode) (err error) {
	// Directory isn't supported at all
	if dir {
		return fmt.Errorf("gzip-compressed files can only unarchive to a single file")
	}

	// Any error aborts the batch and removes everything this batch created.
	tracker := new(extractTracker)
	defer func() {
		if err != nil {
			tracker.cleanup()
		}
	}()

	// If we're going into a directory we should make that first
	if err = tracker.mkdirAll(filepath.Dir(dst), mode(0755, umask)); err != nil {
		return err
	}

	// File first
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// gzip compression is second
	gzipR, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gzipR.Close() }()

	// Copy it out
	return copyReader(dst, gzipR, 0622, umask, d.FileSizeLimit)
}
