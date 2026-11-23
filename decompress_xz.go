// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ulikunitz/xz"
)

// XzDecompressor is an implementation of Decompressor that can
// decompress xz files.
type XzDecompressor struct {
	// FileSizeLimit limits the size of a decompressed file.
	//
	// The zero value means no limit.
	FileSizeLimit int64
}

func (d *XzDecompressor) Decompress(dst, src string, dir bool, umask os.FileMode) (err error) {
	// Directory isn't supported at all
	if dir {
		return fmt.Errorf("xz-compressed files can only unarchive to a single file")
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

	// xz compression is second
	xzR, err := xz.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}

	// Copy it out, potentially using a file size limit.
	return copyReader(dst, xzR, 0622, umask, d.FileSizeLimit)
}
