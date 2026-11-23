// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
)

// TarGzipDecompressor is an implementation of Decompressor that can
// decompress tar.gzip files.
type TarGzipDecompressor struct {
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

func (d *TarGzipDecompressor) Decompress(dst, src string, dir bool, umask os.FileMode) (err error) {
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

	// Gzip compression is second
	gzipR, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("Error opening a gzip reader for %s: %w", src, err)
	}
	defer func() { _ = gzipR.Close() }()

	return untar(gzipR, dst, src, dir, umask, d.FileSizeLimit, d.FilesLimit, tracker)
}
