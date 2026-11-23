// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNoStagingLeftovers fails the test if the given directory contains
// staging or backup directories left behind by a Get operation.
func assertNoStagingLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".getter-staging-") ||
			strings.HasPrefix(e.Name(), ".getter-backup-") {
			t.Fatalf("staging leftover in %s: %s", dir, e.Name())
		}
	}
}

// serveContent starts a test HTTP server serving load at /file.
func serveContent(t *testing.T, load []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(load)
	}))
}

func sha256Hex(load []byte) string {
	sum := sha256.Sum256(load)
	return hex.EncodeToString(sum[:])
}

// A checksum mismatch must not modify an existing destination, and the
// staging area must be cleaned up. This exercises the shared verification
// terminal state through the http getter.
func TestGetFile_checksumMismatchPreservesDst_http(t *testing.T) {
	load := []byte("new content\n")
	srv := serveContent(t, load)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "test-file")
	if err := os.WriteFile(dst, []byte("original content\n"), 0644); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Corrupt the expected digest so verification fails.
	badChecksum := sha256Hex([]byte("something else"))
	u := fmt.Sprintf("%s/file?checksum=sha256:%s", srv.URL, badChecksum)

	err := GetFile(dst, u)
	if err == nil {
		t.Fatal("should error")
	}
	var cerr *ChecksumError
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a ChecksumError, got: %T %s", err, err)
	}

	assertContents(t, dst, "original content\n")
	assertNoStagingLeftovers(t, dir)
}

// The same terminal state must hold for the file getter.
func TestGetFile_checksumMismatchPreservesDst_file(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "test-file")
	if err := os.WriteFile(dst, []byte("original content\n"), 0644); err != nil {
		t.Fatalf("err: %s", err)
	}

	badChecksum := sha256Hex([]byte("something else"))
	u := testModule("basic-file/foo.txt") + "?checksum=sha256:" + badChecksum

	client := &Client{
		Src: u,
		Dst: dst,
		Getters: map[string]Getter{
			"file": &FileGetter{Copy: true},
		},
	}

	err := client.Get()
	if err == nil {
		t.Fatal("should error")
	}
	var cerr *ChecksumError
	if !errors.As(err, &cerr) {
		t.Fatalf("expected a ChecksumError, got: %T %s", err, err)
	}

	assertContents(t, dst, "original content\n")
	assertNoStagingLeftovers(t, dir)
}

// Without a checksum (compatibility mode) downloads are still staged and
// published, replacing any previous destination.
func TestGetFile_noChecksumCompatMode(t *testing.T) {
	load := []byte("new content\n")
	srv := serveContent(t, load)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "test-file")

	// Initial download.
	if err := GetFile(dst, srv.URL+"/file"); err != nil {
		t.Fatalf("err: %s", err)
	}
	assertContents(t, dst, "new content\n")

	// A second download over an existing destination replaces it.
	if err := os.WriteFile(dst, []byte("stale\n"), 0644); err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := GetFile(dst, srv.URL+"/file"); err != nil {
		t.Fatalf("err: %s", err)
	}
	assertContents(t, dst, "new content\n")
	assertNoStagingLeftovers(t, dir)
}

// A failing final rename must surface an error and preserve the previous
// destination.
func TestGetFile_publishRenameFailure(t *testing.T) {
	load := []byte("new content\n")
	srv := serveContent(t, load)
	defer srv.Close()

	// Fail any rename moving staged content, i.e. the final publish step.
	orig := osRename
	osRename = func(oldpath, newpath string) error {
		if strings.Contains(oldpath, ".getter-staging-") {
			return errors.New("injected rename failure")
		}
		return orig(oldpath, newpath)
	}
	defer func() { osRename = orig }()

	t.Run("existing destination is preserved", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "test-file")
		if err := os.WriteFile(dst, []byte("original content\n"), 0644); err != nil {
			t.Fatalf("err: %s", err)
		}

		err := GetFile(dst, srv.URL+"/file")
		if err == nil {
			t.Fatal("should error")
		}
		if !strings.Contains(err.Error(), "error publishing") {
			t.Fatalf("unexpected err: %s", err)
		}

		assertContents(t, dst, "original content\n")
		assertNoStagingLeftovers(t, dir)
	})

	t.Run("no destination stays absent", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "test-file")

		err := GetFile(dst, srv.URL+"/file")
		if err == nil {
			t.Fatal("should error")
		}
		if !strings.Contains(err.Error(), "error publishing") {
			t.Fatalf("unexpected err: %s", err)
		}

		if _, err := os.Lstat(dst); !os.IsNotExist(err) {
			t.Fatalf("destination should not exist after failed publish")
		}
		assertNoStagingLeftovers(t, dir)
	})
}

// A canceled download must clean up its staging area and leave any
// previous destination untouched.
func TestGetFile_cancelCleansStaging(t *testing.T) {
	load := []byte("new content\n")
	srv := serveContent(t, load)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "test-file")
	if err := os.WriteFile(dst, []byte("original content\n"), 0644); err != nil {
		t.Fatalf("err: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := &Client{
		Ctx: ctx,
		Src: srv.URL + "/file",
		Dst: dst,
	}
	if err := client.Get(); err == nil {
		t.Fatal("should error")
	}

	assertContents(t, dst, "original content\n")
	assertNoStagingLeftovers(t, dir)
}

// A checksum mismatch on an archive must not modify an existing
// destination directory.
func TestGet_archiveChecksumMismatchPreservesDst(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "test-dir")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatalf("err: %s", err)
	}
	sentinel := filepath.Join(dst, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("original\n"), 0644); err != nil {
		t.Fatalf("err: %s", err)
	}

	u := testModule(
		"basic-file-archive/archive.tar.gz?checksum=md5:00000000000000000000000000000000")

	if err := Get(dst, u); err == nil {
		t.Fatal("should error")
	}

	assertContents(t, sentinel, "original\n")
	assertNoStagingLeftovers(t, dir)
}

// A corrupt archive (no checksum, compatibility mode) must fail without
// modifying an existing destination directory.
func TestGet_archiveDecompressFailurePreservesDst(t *testing.T) {
	// Not a valid gzip stream.
	srv := serveContent(t, []byte("this is not an archive"))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "test-dir")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatalf("err: %s", err)
	}
	sentinel := filepath.Join(dst, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("original\n"), 0644); err != nil {
		t.Fatalf("err: %s", err)
	}

	if err := Get(dst, srv.URL+"/archive.tar.gz"); err == nil {
		t.Fatal("should error")
	}

	assertContents(t, sentinel, "original\n")
	assertNoStagingLeftovers(t, dir)
}

// A successful archive extraction is staged and published as a whole.
func TestGet_archivePublish(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "test-dir")

	u := testModule(
		"basic-file-archive/archive.tar.gz?checksum=md5:fbd90037dacc4b1ab40811d610dde2f0")

	if err := Get(dst, u); err != nil {
		t.Fatalf("err: %s", err)
	}

	assertContents(t, filepath.Join(dst, "file"), "Hello\n")
	assertNoStagingLeftovers(t, dir)
}
