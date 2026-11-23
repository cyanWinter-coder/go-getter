// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SourceDirSubdir takes a source URL and returns a tuple of the URL without
// the subdir and the subdir.
//
// ex:
//
//	dom.com/path/?q=p               => dom.com/path/?q=p, ""
//	proto://dom.com/path//*?q=p     => proto://dom.com/path?q=p, "*"
//	proto://dom.com/path//path2?q=p => proto://dom.com/path?q=p, "path2"
func SourceDirSubdir(src string) (string, string) {

	// URL might contains another url in query parameters
	stop := len(src)
	if idx := strings.Index(src, "?"); idx > -1 {
		stop = idx
	}

	// Calculate an offset to avoid accidentally marking the scheme
	// as the dir.
	var offset int
	if idx := strings.Index(src[:stop], "://"); idx > -1 {
		offset = idx + 3
	}

	// First see if we even have an explicit subdir
	idx := strings.Index(src[offset:stop], "//")
	if idx == -1 {
		return src, ""
	}

	idx += offset
	subdir := src[idx+2:]
	src = src[:idx]

	// Next, check if we have query parameters and push them onto the
	// URL.
	if idx = strings.Index(subdir, "?"); idx > -1 {
		query := subdir[idx:]
		subdir = subdir[:idx]
		src += query
	}

	return src, subdir
}

// addSubdirToSource splices a subdir component back into a source string,
// in front of any query string or fragment.
//
// This is deliberately string surgery rather than a net/url round trip:
// parsing and re-serializing the URL would decode percent-encoded
// characters in the path and query (collapsing e.g. "%2F" into a literal
// slash) and re-encode them, corrupting sources whose meaning depends on
// the exact encoding. Splicing keeps the source byte-for-byte intact so
// that decoding happens exactly once, in the getter that consumes the URL.
func addSubdirToSource(src, subdir string) string {
	// Insert before the first query or fragment delimiter, whichever
	// comes first.
	idx := len(src)
	if i := strings.IndexAny(src, "?#"); i > -1 {
		idx = i
	}
	return src[:idx] + "//" + subdir + src[idx:]
}

// ErrSubdirOutsideRoot is returned (wrapped) by SubdirGlob when the
// requested subdir resolves to a path outside of the download root, for
// example through a symlink or a glob matching one.
var ErrSubdirOutsideRoot = errors.New("subdir points outside the download root")

// SubdirGlob returns the actual subdir with globbing processed.
//
// dst should be a destination directory that is already populated (the
// download is complete) and subDir should be the set subDir. If subDir
// is an empty string, this returns an empty string.
//
// The returned path is the full absolute path.
//
// The result is guaranteed to live inside dst: dot segments are resolved
// by the glob and any symlink encountered along the matched path must not
// point outside of dst.
func SubdirGlob(dst, subDir string) (string, error) {
	pattern := filepath.Join(dst, subDir)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("subdir %q not found", subDir)
	}

	if len(matches) > 1 {
		return "", fmt.Errorf("subdir %q matches multiple paths", subDir)
	}

	match := matches[0]

	// The match must stay within the download root. A subdir that reaches
	// a symlink pointing outside of dst (or a glob matching one) would
	// otherwise let the caller copy or read files outside of the download.
	root, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return "", fmt.Errorf("error resolving download root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(match)
	if err != nil {
		return "", fmt.Errorf("error resolving subdir %q: %w", subDir, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrSubdirOutsideRoot, subDir)
	}

	return match, nil
}
