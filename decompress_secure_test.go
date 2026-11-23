// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeTarBytes builds a tar archive in memory from the given headers and
// bodies. Each entry is a header optionally paired with body content.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Typeflag == tar.TypeReg && hdr.Size == 0 && len(e.body) > 0 {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("err: %s", err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("err: %s", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("err: %s", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatalf("err: %s", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("err: %s", err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name string
	body []byte
	mode os.FileMode
}

func buildGzip(t *testing.T, body []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(body); err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("err: %s", err)
	}
	return buf.Bytes()
}

// writeArchive writes the archive bytes to a new file in dir and returns its path.
func writeArchive(t *testing.T, dir, name string, b []byte) string {
	t.Helper()

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0666); err != nil {
		t.Fatalf("err: %s", err)
	}
	return p
}

// assertEmptyDir verifies that dir contains no entries, i.e. a failed batch
// left nothing behind.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected no leftover content in %s, found: %s", dir, strings.Join(names, ", "))
	}
}

func TestTarTraversal(t *testing.T) {
	cases := map[string]tarEntry{
		"parent traversal":  {hdr: tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("x")},
		"nested traversal":  {hdr: tar.Header{Name: "sub/../../evil.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("x")},
		"absolute path":     {hdr: tar.Header{Name: "/tmp/evil.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("x")},
		"parent dir entry":  {hdr: tar.Header{Name: "..", Typeflag: tar.TypeDir, Mode: 0o755}},
		"dotdot suffix dir": {hdr: tar.Header{Name: "sub/..", Typeflag: tar.TypeDir, Mode: 0o755}},
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			td := t.TempDir()

			// A benign entry first, so we can verify it gets cleaned up
			// when the malicious entry aborts the batch.
			archive := buildTar(t, []tarEntry{
				{hdr: tar.Header{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("ok")},
				entry,
			})
			src := writeArchive(t, td, "input.tar", archive)

			// Extract into a fresh subdirectory of a scratch dir so we can
			// check nothing is left behind.
			scratch := filepath.Join(td, "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatalf("err: %s", err)
			}
			dst := filepath.Join(scratch, "subdir", "result")

			d := new(TarDecompressor)
			err := d.Decompress(dst, src, true, 0o022)
			if err == nil {
				t.Fatal("expected traversal entry to error")
			}

			assertEmptyDir(t, scratch)
		})
	}
}

func TestTarSymlinkTraversal(t *testing.T) {
	cases := map[string]tarEntry{
		"relative escape": {hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd", Mode: 0o777}},
		"absolute target": {hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}},
		"nested escape":   {hdr: tar.Header{Name: "sub/link", Typeflag: tar.TypeSymlink, Linkname: "../../outside", Mode: 0o777}},
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			td := t.TempDir()
			archive := buildTar(t, []tarEntry{
				{hdr: tar.Header{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("ok")},
				entry,
			})
			src := writeArchive(t, td, "input.tar", archive)

			scratch := filepath.Join(td, "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatalf("err: %s", err)
			}
			dst := filepath.Join(scratch, "result")

			d := new(TarDecompressor)
			err := d.Decompress(dst, src, true, 0o022)
			if err == nil {
				t.Fatal("expected escaping symlink to error")
			}

			assertEmptyDir(t, scratch)
		})
	}
}

func TestTarHardlinkTraversal(t *testing.T) {
	td := t.TempDir()
	archive := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("ok")},
		{hdr: tar.Header{Name: "evil", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd", Mode: 0o600}},
	})
	src := writeArchive(t, td, "input.tar", archive)

	scratch := filepath.Join(td, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("err: %s", err)
	}
	dst := filepath.Join(scratch, "result")

	d := new(TarDecompressor)
	err := d.Decompress(dst, src, true, 0o022)
	if err == nil {
		t.Fatal("expected escaping hardlink to error")
	}

	assertEmptyDir(t, scratch)
}

func TestTarLinksValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on windows")
	}

	td := t.TempDir()
	archive := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "dir/target.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("hello")},
		{hdr: tar.Header{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "target.txt", Mode: 0o777}},
		{hdr: tar.Header{Name: "link2", Typeflag: tar.TypeSymlink, Linkname: "dir/target.txt", Mode: 0o777}},
		{hdr: tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "dir/target.txt", Mode: 0o600}},
	})
	src := writeArchive(t, td, "input.tar", archive)

	dst := filepath.Join(td, "result")

	d := new(TarDecompressor)
	if err := d.Decompress(dst, src, true, 0o022); err != nil {
		t.Fatalf("err: %s", err)
	}

	// The symlinks must have been created as links and resolve to the
	// extracted content.
	for _, link := range []string{"dir/link", "link2"} {
		p := filepath.Join(dst, filepath.FromSlash(link))
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to be a symlink", link)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		if string(b) != "hello" {
			t.Fatalf("bad content through %s: %q", link, string(b))
		}
	}

	// The hardlink must share the content of its target.
	b, err := os.ReadFile(filepath.Join(dst, "hard"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if string(b) != "hello" {
		t.Fatalf("bad hardlink content: %q", string(b))
	}
}

func TestZipTraversal(t *testing.T) {
	cases := map[string]zipEntry{
		"parent traversal": {name: "../evil.txt", body: []byte("x")},
		"nested traversal": {name: "sub/../../evil.txt", body: []byte("x")},
		"absolute path":    {name: "/tmp/evil.txt", body: []byte("x")},
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			td := t.TempDir()
			archive := buildZip(t, []zipEntry{
				{name: "ok.txt", body: []byte("ok")},
				entry,
			})
			src := writeArchive(t, td, "input.zip", archive)

			scratch := filepath.Join(td, "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatalf("err: %s", err)
			}
			dst := filepath.Join(scratch, "subdir", "result")

			d := new(ZipDecompressor)
			err := d.Decompress(dst, src, true, 0o022)
			if err == nil {
				t.Fatal("expected traversal entry to error")
			}

			assertEmptyDir(t, scratch)
		})
	}
}

func TestZipSymlinkTraversal(t *testing.T) {
	cases := map[string]zipEntry{
		"relative escape": {name: "link", body: []byte("../../etc/passwd"), mode: os.ModeSymlink | 0o777},
		"absolute target": {name: "link", body: []byte("/etc/passwd"), mode: os.ModeSymlink | 0o777},
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			td := t.TempDir()
			archive := buildZip(t, []zipEntry{
				{name: "ok.txt", body: []byte("ok")},
				entry,
			})
			src := writeArchive(t, td, "input.zip", archive)

			scratch := filepath.Join(td, "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatalf("err: %s", err)
			}
			dst := filepath.Join(scratch, "result")

			d := new(ZipDecompressor)
			err := d.Decompress(dst, src, true, 0o022)
			if err == nil {
				t.Fatal("expected escaping symlink to error")
			}

			assertEmptyDir(t, scratch)
		})
	}
}

func TestZipSymlinkValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on windows")
	}

	td := t.TempDir()
	archive := buildZip(t, []zipEntry{
		{name: "target.txt", body: []byte("hello")},
		{name: "link", body: []byte("target.txt"), mode: os.ModeSymlink | 0o777},
	})
	src := writeArchive(t, td, "input.zip", archive)

	dst := filepath.Join(td, "result")

	d := new(ZipDecompressor)
	if err := d.Decompress(dst, src, true, 0o022); err != nil {
		t.Fatalf("err: %s", err)
	}

	p := filepath.Join(dst, "link")
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected link to be a symlink")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if string(b) != "hello" {
		t.Fatalf("bad content through link: %q", string(b))
	}
}

func TestTarLimitCleanup(t *testing.T) {
	archive := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("aaa")},
		{hdr: tar.Header{Name: "b.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("bbb")},
		{hdr: tar.Header{Name: "c.txt", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("ccc")},
	})

	t.Run("file size limit", func(t *testing.T) {
		td := t.TempDir()
		src := writeArchive(t, td, "input.tar", archive)
		scratch := filepath.Join(td, "scratch")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatalf("err: %s", err)
		}

		d := &TarDecompressor{FileSizeLimit: 5}
		err := d.Decompress(filepath.Join(scratch, "result"), src, true, 0o022)
		if err == nil {
			t.Fatal("expected file size limit to error")
		}
		assertEmptyDir(t, scratch)
	})

	t.Run("files limit", func(t *testing.T) {
		td := t.TempDir()
		src := writeArchive(t, td, "input.tar", archive)
		scratch := filepath.Join(td, "scratch")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatalf("err: %s", err)
		}

		d := &TarDecompressor{FilesLimit: 2}
		err := d.Decompress(filepath.Join(scratch, "result"), src, true, 0o022)
		if err == nil {
			t.Fatal("expected files limit to error")
		}
		assertEmptyDir(t, scratch)
	})
}

func TestZipLimitCleanup(t *testing.T) {
	archive := buildZip(t, []zipEntry{
		{name: "a.txt", body: []byte("aaa")},
		{name: "b.txt", body: []byte("bbb")},
		{name: "c.txt", body: []byte("ccc")},
	})

	t.Run("file size limit", func(t *testing.T) {
		td := t.TempDir()
		src := writeArchive(t, td, "input.zip", archive)
		scratch := filepath.Join(td, "scratch")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatalf("err: %s", err)
		}

		d := &ZipDecompressor{FileSizeLimit: 5}
		err := d.Decompress(filepath.Join(scratch, "result"), src, true, 0o022)
		if err == nil {
			t.Fatal("expected file size limit to error")
		}
		assertEmptyDir(t, scratch)
	})

	t.Run("files limit", func(t *testing.T) {
		td := t.TempDir()
		src := writeArchive(t, td, "input.zip", archive)
		scratch := filepath.Join(td, "scratch")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatalf("err: %s", err)
		}

		d := &ZipDecompressor{FilesLimit: 2}
		err := d.Decompress(filepath.Join(scratch, "result"), src, true, 0o022)
		if err == nil {
			t.Fatal("expected files limit to error")
		}
		assertEmptyDir(t, scratch)
	})
}

func TestGzipLimitCleanup(t *testing.T) {
	td := t.TempDir()
	src := writeArchive(t, td, "input.gz", buildGzip(t, []byte("hello world, this is too long")))

	scratch := filepath.Join(td, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("err: %s", err)
	}
	dst := filepath.Join(scratch, "subdir", "result")

	d := &GzipDecompressor{FileSizeLimit: 5}
	err := d.Decompress(dst, src, false, 0o022)
	if err == nil {
		t.Fatal("expected file size limit to error")
	}
	if !strings.Contains(err.Error(), "file larger than limit: 5") {
		t.Fatalf("unexpected error: %q", err.Error())
	}

	// The partial output file and the directories created for it must be gone.
	assertEmptyDir(t, scratch)
}

func TestGzipWithinLimit(t *testing.T) {
	td := t.TempDir()
	src := writeArchive(t, td, "input.gz", buildGzip(t, []byte("hello")))

	dst := filepath.Join(td, "result")

	d := &GzipDecompressor{FileSizeLimit: 5}
	if err := d.Decompress(dst, src, false, 0o022); err != nil {
		t.Fatalf("err: %s", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if string(b) != "hello" {
		t.Fatalf("bad content: %q", string(b))
	}
}
