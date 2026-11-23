//go:build !windows

// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestSubdirGlob_symlinkEscape(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	// A subdir naming the symlink directly must be rejected.
	if _, err := SubdirGlob(root, "link"); !errors.Is(err, ErrSubdirOutsideRoot) {
		t.Fatalf("expected ErrSubdirOutsideRoot, got: %v", err)
	}

	// A glob that matches only the symlink must be rejected as well.
	if _, err := SubdirGlob(root, "l*"); !errors.Is(err, ErrSubdirOutsideRoot) {
		t.Fatalf("expected ErrSubdirOutsideRoot, got: %v", err)
	}

	// A real directory inside the root still resolves.
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	got, err := SubdirGlob(root, "real")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if got != real {
		t.Fatalf("bad: %s", got)
	}
}

// testSymlinkGetter is a Getter whose Get plants a symlink named "link"
// inside the download directory, pointing at target.
type testSymlinkGetter struct {
	target string
}

func (g *testSymlinkGetter) Get(dst string, _ *url.URL) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	return os.Symlink(g.target, filepath.Join(dst, "link"))
}

func (g *testSymlinkGetter) GetFile(string, *url.URL) error {
	return fmt.Errorf("file mode not supported")
}

func (g *testSymlinkGetter) ClientMode(*url.URL) (ClientMode, error) {
	return ClientModeDir, nil
}

func (g *testSymlinkGetter) SetClient(*Client) {}

func TestClientGet_subdirSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")

	client := &Client{
		Src:  "test::https://example.com//link",
		Dst:  dst,
		Mode: ClientModeDir,
		Getters: map[string]Getter{
			"test": &testSymlinkGetter{target: outside},
		},
	}

	err := client.Get()
	if !errors.Is(err, ErrSubdirOutsideRoot) {
		t.Fatalf("expected ErrSubdirOutsideRoot, got: %v", err)
	}

	// The destination must not have been created or modified.
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst should not exist, stat err: %v", err)
	}
}
