// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	getter "github.com/hashicorp/go-getter"
)

// TestUID425RangeNotSatisfiableRestarts verifies that a 416 on the resume
// request leads to a single unconditional full GET and a correct file.
func TestUID425RangeNotSatisfiableRestarts(t *testing.T) {
	content := uid425Pattern(4096)

	var mu sync.Mutex
	rangeGets, fullGets := 0, 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		hasRange := r.Header.Get("Range") != ""
		if hasRange {
			rangeGets++
		} else {
			fullGets++
		}
		mu.Unlock()

		if hasRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(dst, content[:len(content)/2], 0666); err != nil {
		t.Fatal(err)
	}

	if err := (&getter.HttpGetter{}).GetFile(dst, uid425MustURL(t, srv.URL+"/blob")); err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytesEqualUID425(got, content) {
		t.Fatalf("content mismatch after 416 restart: len=%d want=%d", len(got), len(content))
	}

	mu.Lock()
	defer mu.Unlock()
	if rangeGets != 1 || fullGets != 1 {
		t.Fatalf("expected one range GET and one full GET, got range=%d full=%d", rangeGets, fullGets)
	}
}

// TestUID425FailedGetPreservesPartial verifies that a failed request does not
// truncate a pre-existing partial file, so its length remains a resume point.
func TestUID425FailedGetPreservesPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "blob")
	partial := uid425Pattern(1234)
	if err := os.WriteFile(dst, partial, 0666); err != nil {
		t.Fatal(err)
	}

	g := &getter.HttpGetter{DoNotCheckHeadFirst: true}
	err := g.GetFile(dst, uid425MustURL(t, srv.URL+"/blob"))
	if err == nil {
		t.Fatal("expected status error")
	}
	var statusErr *getter.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected HTTPStatusError 500, got %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, partial) {
		t.Fatalf("partial file modified after failed request (len=%d want %d)", len(got), len(partial))
	}
}

// TestUID425Misaligned206Restarts verifies that a 206 whose Content-Range does
// not continue at the persisted offset is not appended to the old prefix; the
// getter re-fetches unconditionally.
func TestUID425Misaligned206Restarts(t *testing.T) {
	content := uid425Pattern(4096)
	cut := len(content) / 2

	var mu sync.Mutex
	fullGets := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"misaligned"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Header.Get("Range") != "" {
			// Misbehaving server: answer as if starting at 0 even though the
			// client asked for cut-. Validator matches, offset must protect us.
			segment := content[:cut]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", cut-1, len(content)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(segment)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(segment)
			return
		}

		mu.Lock()
		fullGets++
		mu.Unlock()
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "blob")
	// Persist matching validator state so the first attempt trusts its offset
	// basis, plus a correct prefix.
	if err := os.WriteFile(dst, content[:cut], 0666); err != nil {
		t.Fatal(err)
	}
	st := fmt.Sprintf(`{"version":1,"url":%q,"etag":%q,"total_size":%d}`,
		srv.URL+"/blob", `"misaligned"`, len(content))
	if err := os.WriteFile(dst+".getter-resume", []byte(st), 0600); err != nil {
		t.Fatal(err)
	}

	if err := (&getter.HttpGetter{}).GetFile(dst, uid425MustURL(t, srv.URL+"/blob")); err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytesEqualUID425(got, content) {
		t.Fatalf("hybrid/incorrect content after misaligned 206: len=%d want=%d", len(got), len(content))
	}

	mu.Lock()
	defer mu.Unlock()
	if fullGets != 1 {
		t.Fatalf("expected exactly one unconditional full GET, got %d", fullGets)
	}
}
