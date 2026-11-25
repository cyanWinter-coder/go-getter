// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

// These tests cover safe resumption of large HTTP downloads: validator
// handling, 200-vs-206 semantics, persisted-byte progress, cancellation,
// redirect/resource identity checks and the checksum gate. They live in the
// external test package so they only exercise the public API and cannot
// collide with package-internal test symbols.

package getter_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	getter "github.com/hashicorp/go-getter"
)

// ---------------------------------------------------------------------------
// Configurable range/validator HTTP server
// ---------------------------------------------------------------------------

type uid425RecordedRequest struct {
	method      string
	rangeHeader string
	ifRange     string
	path        string
}

type uid425FileServer struct {
	mu sync.Mutex

	content      []byte
	etag         string
	lastModified string

	// always200 makes the server ignore Range and always answer 200 full.
	always200 bool
	// ignoreIfRange disables If-Range validation.
	ignoreIfRange bool

	// blockAfter > 0 makes GET responses flush the first blockAfter bytes and
	// then block until the request context is canceled or release is closed.
	blockAfter int
	release    chan struct{}

	requests []uid425RecordedRequest
	getCount int
}

func newUID425FileServer(content []byte, etag string) *uid425FileServer {
	return &uid425FileServer{
		content:    content,
		etag:       etag,
		release:    make(chan struct{}),
		blockAfter: 0,
	}
}

func (s *uid425FileServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serve)
	return mux
}

func (s *uid425FileServer) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

func (s *uid425FileServer) snapshot() ([]uid425RecordedRequest, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]uid425RecordedRequest(nil), s.requests...)
	return out, s.getCount
}

func (s *uid425FileServer) setContent(content []byte, etag, lastModified string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content = content
	s.etag = etag
	s.lastModified = lastModified
}

func (s *uid425FileServer) disableBlocking() {
	s.mu.Lock()
	s.blockAfter = 0
	s.mu.Unlock()
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

func (s *uid425FileServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	content := s.content
	etag := s.etag
	lm := s.lastModified
	always200 := s.always200
	ignoreIfRange := s.ignoreIfRange
	blockAfter := s.blockAfter
	s.requests = append(s.requests, uid425RecordedRequest{
		method:      r.Method,
		rangeHeader: r.Header.Get("Range"),
		ifRange:     r.Header.Get("If-Range"),
		path:        r.URL.Path,
	})
	if r.Method == http.MethodGet {
		s.getCount++
	}
	s.mu.Unlock()

	w.Header().Set("Accept-Ranges", "bytes")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lm != "" {
		w.Header().Set("Last-Modified", lm)
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		return
	}

	start := 0
	end := len(content)
	status := http.StatusOK

	if !always200 {
		if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
			spec := strings.TrimPrefix(rh, "bytes=")
			if dash := strings.Index(spec, "-"); dash >= 0 {
				if n, err := strconv.Atoi(spec[:dash]); err == nil && n < len(content) {
					// Enforce If-Range like a standards-compliant server.
					validatorOK := true
					if !ignoreIfRange {
						if ir := r.Header.Get("If-Range"); ir != "" {
							if strings.HasPrefix(ir, `"`) || strings.HasPrefix(ir, `W/`) {
								validatorOK = ir == etag
							} else if lm != "" {
								validatorOK = ir == lm
							}
						}
					}
					if validatorOK {
						start = n
						end = len(content)
						status = http.StatusPartialContent
					}
				}
			}
		}
	}

	body := content[start:end]
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(content)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)

	if blockAfter > 0 && r.Method == http.MethodGet {
		n := blockAfter
		if n > len(body) {
			n = len(body)
		}
		_, _ = w.Write(body[:n])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-s.release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(body[n:])
		return
	}

	_, _ = w.Write(body)
}

// uid425Pattern builds deterministic, incompressible-ish content.
func uid425Pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + i/257) % 251)
	}
	return b
}

func uid425MustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}

func uid425WaitSize(t *testing.T, dst string, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(dst)
		if err == nil && fi.Size() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fi, err := os.Stat(dst)
	got := int64(-1)
	if err == nil {
		got = fi.Size()
	}
	t.Fatalf("timed out waiting for dst size %d, got %d", want, got)
}

func uid425SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Requirement 2: 200 truncates, 206 appends the declared range
// ---------------------------------------------------------------------------

func TestUID425Resume200FullTruncatesOldPart(t *testing.T) {
	content := uid425Pattern(8192)
	srv := newUID425FileServer(content, `"etag-200"`)
	srv.always200 = true // server ignores Range and sends the full entity
	ln := srv.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	partial := bytesRepeatUID425('Z', len(content)/2)
	if err := os.WriteFile(dst, partial, 0666); err != nil {
		t.Fatal(err)
	}

	g := &getter.HttpGetter{}
	if err := g.GetFile(dst, uid425MustURL(t, ln.URL+"/blob")); err != nil {
		t.Fatalf("GetFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, content) {
		t.Fatalf("content mismatch: len got=%d want=%d (hybrid/concatenated?)", len(got), len(content))
	}

	reqs, _ := srv.snapshot()
	var sawRange bool
	for _, r := range reqs {
		if r.method == http.MethodGet && strings.HasPrefix(r.rangeHeader, "bytes=") {
			sawRange = true
		}
	}
	if !sawRange {
		t.Fatalf("expected a resume Range request, requests: %+v", reqs)
	}
}

func TestUID425Resume206AppendsDeclaredRange(t *testing.T) {
	content := uid425Pattern(8192)
	srv := newUID425FileServer(content, `"etag-206"`)
	ln := srv.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	cut := len(content) / 2
	if err := os.WriteFile(dst, content[:cut], 0666); err != nil {
		t.Fatal(err)
	}

	g := &getter.HttpGetter{}
	if err := g.GetFile(dst, uid425MustURL(t, ln.URL+"/blob")); err != nil {
		t.Fatalf("GetFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, content) {
		t.Fatalf("content mismatch: len got=%d want=%d", len(got), len(content))
	}

	reqs, _ := srv.snapshot()
	var get uid425RecordedRequest
	found := false
	for _, r := range reqs {
		if r.method == http.MethodGet {
			get = r
			found = true
		}
	}
	if !found {
		t.Fatal("missing GET request")
	}
	if want := fmt.Sprintf("bytes=%d-", cut); get.rangeHeader != want {
		t.Fatalf("Range = %q, want %q", get.rangeHeader, want)
	}
	if get.ifRange != `"etag-206"` {
		t.Fatalf("If-Range = %q, want %q", get.ifRange, `"etag-206"`)
	}
}

// ---------------------------------------------------------------------------
// Requirements 1 + 3: validator persisted across cancellation and mismatch
// triggers a clean restart
// ---------------------------------------------------------------------------

func TestUID425CancelThenResumeSameResource(t *testing.T) {
	content := uid425Pattern(64 * 1024)
	srv := newUID425FileServer(content, `"etag-cancel"`)
	srv.blockAfter = 4096
	ln := srv.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- getter.GetFile(dst, ln.URL+"/blob", getter.WithContext(ctx))
	}()

	uid425WaitSize(t, dst, 4096)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GetFile did not return after cancel")
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("partial destination missing after cancel: %v", err)
	}
	if fi.Size() != 4096 {
		t.Fatalf("persisted size after cancel = %d, want 4096", fi.Size())
	}

	// Resume against the same resource with blocking disabled.
	srv.disableBlocking()
	g := &getter.HttpGetter{}
	if err := g.GetFile(dst, uid425MustURL(t, ln.URL+"/blob")); err != nil {
		t.Fatalf("resume GetFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, content) {
		t.Fatalf("resumed content mismatch: len=%d want=%d", len(got), len(content))
	}
	if _, err := os.Stat(dst + ".getter-resume"); !os.IsNotExist(err) {
		t.Fatalf("resume state should be removed after completion, stat err=%v", err)
	}

	reqs, _ := srv.snapshot()
	var sawResume bool
	for _, r := range reqs {
		if r.method == http.MethodGet && r.rangeHeader == "bytes=4096-" && r.ifRange == `"etag-cancel"` {
			sawResume = true
		}
	}
	if !sawResume {
		t.Fatalf("expected validator-backed resume from 4096, requests: %+v", reqs)
	}
}

func TestUID425ValidatorChangeRestartsFromZero(t *testing.T) {
	oldContent := uid425Pattern(64 * 1024)
	newContent := uid425Pattern(64 * 1024)
	// Make sure the representations actually differ.
	newContent[0] ^= 0xFF

	srv := newUID425FileServer(oldContent, `"etag-v1"`)
	srv.blockAfter = 4096
	ln := srv.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- getter.GetFile(dst, ln.URL+"/blob", getter.WithContext(ctx)) }()
	uid425WaitSize(t, dst, 4096)
	cancel()
	<-errCh

	// Server now serves a new representation with a different validator.
	srv.disableBlocking()
	srv.setContent(newContent, `"etag-v2"`, "")

	g := &getter.HttpGetter{}
	if err := g.GetFile(dst, uid425MustURL(t, ln.URL+"/blob")); err != nil {
		t.Fatalf("GetFile after validator change: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, newContent) {
		t.Fatalf("expected restart onto new content, len=%d", len(got))
	}

	reqs, _ := srv.snapshot()
	var carriedOldValidator bool
	for _, r := range reqs {
		if r.method == http.MethodGet && r.ifRange == `"etag-v1"` && strings.HasPrefix(r.rangeHeader, "bytes=4096-") {
			carriedOldValidator = true
		}
	}
	if !carriedOldValidator {
		t.Fatalf("resume request did not carry the stored validator: %+v", reqs)
	}
}

func TestUID425LastModifiedChangeRestarts(t *testing.T) {
	oldContent := uid425Pattern(32 * 1024)
	newContent := append([]byte(nil), oldContent...)
	newContent[len(newContent)-1] ^= 0xFF

	srv := newUID425FileServer(oldContent, "")
	srv.lastModified = "Mon, 01 Jan 2024 00:00:00 GMT"
	srv.blockAfter = 4096
	ln := srv.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- getter.GetFile(dst, ln.URL+"/blob", getter.WithContext(ctx)) }()
	uid425WaitSize(t, dst, 4096)
	cancel()
	<-errCh

	srv.disableBlocking()
	srv.setContent(newContent, "", "Tue, 02 Jan 2024 00:00:00 GMT")

	if err := (&getter.HttpGetter{}).GetFile(dst, uid425MustURL(t, ln.URL+"/blob")); err != nil {
		t.Fatalf("GetFile after Last-Modified change: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, newContent) {
		t.Fatal("expected restart onto modified content")
	}
}

// ---------------------------------------------------------------------------
// Requirement 3: progress starts at actually persisted bytes
// ---------------------------------------------------------------------------

type uid425Progress struct {
	mu          sync.Mutex
	calls       int
	currentSize int64
	totalSize   int64
	streamReads int64
	lastSrc     string
}

type uid425CountingStream struct {
	rc io.ReadCloser
	p  *uid425Progress
}

func (c *uid425CountingStream) Read(b []byte) (int, error) {
	n, err := c.rc.Read(b)
	if n > 0 {
		c.p.mu.Lock()
		c.p.streamReads += int64(n)
		c.p.mu.Unlock()
	}
	return n, err
}

func (c *uid425CountingStream) Close() error { return c.rc.Close() }

func (p *uid425Progress) TrackProgress(src string, currentSize, totalSize int64, stream io.ReadCloser) io.ReadCloser {
	p.mu.Lock()
	p.calls++
	p.lastSrc = src
	p.currentSize = currentSize
	p.totalSize = totalSize
	p.mu.Unlock()
	return &uid425CountingStream{rc: stream, p: p}
}

func TestUID425ProgressUsesPersistedBytes(t *testing.T) {
	content := uid425Pattern(4096)
	srv := newUID425FileServer(content, `"etag-progress"`)
	ln := srv.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	cut := len(content) / 2
	if err := os.WriteFile(dst, content[:cut], 0666); err != nil {
		t.Fatal(err)
	}

	tracker := &uid425Progress{}
	if err := getter.GetFile(dst, ln.URL+"/blob", getter.WithProgress(tracker)); err != nil {
		t.Fatalf("GetFile: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.calls == 0 {
		t.Fatal("TrackProgress never called")
	}
	if tracker.currentSize != int64(cut) {
		t.Fatalf("currentSize = %d, want persisted %d", tracker.currentSize, cut)
	}
	if tracker.totalSize != int64(len(content)) {
		t.Fatalf("totalSize = %d, want %d", tracker.totalSize, len(content))
	}
	if tracker.streamReads != int64(len(content)-cut) {
		t.Fatalf("stream reads = %d, want remaining %d", tracker.streamReads, len(content)-cut)
	}
}

// ---------------------------------------------------------------------------
// Requirement 4: redirect to a different canonical URL discards state
// ---------------------------------------------------------------------------

func TestUID425CrossOriginRedirectDiscardsState(t *testing.T) {
	contentA := uid425Pattern(64 * 1024)
	srvA := newUID425FileServer(contentA, `"same-etag"`)
	srvA.blockAfter = 4096
	lnA := srvA.server(t)

	dst := filepath.Join(t.TempDir(), "blob")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- getter.GetFile(dst, lnA.URL+"/a/file", getter.WithContext(ctx)) }()
	uid425WaitSize(t, dst, 4096)
	cancel()
	<-errCh
	srvA.disableBlocking()

	// Different origin (distinct test server port), different payload, but
	// deliberately the SAME etag so the URL identity check is what must fire.
	contentB := uid425Pattern(48 * 1024)
	srvB := newUID425FileServer(contentB, `"same-etag"`)
	lnB := srvB.server(t)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, lnB.URL+"/b/resource", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	if err := (&getter.HttpGetter{}).GetFile(
		dst, uid425MustURL(t, redirector.URL+"/start"),
	); err != nil {
		t.Fatalf("GetFile through cross-origin redirect: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqualUID425(got, contentB) {
		t.Fatalf("expected destination B content after redirect, len=%d want=%d", len(got), len(contentB))
	}

	reqsB, getsB := srvB.snapshot()
	if getsB == 0 {
		t.Fatal("origin B received no GET")
	}
	var sawFullUnconditional bool
	for _, r := range reqsB {
		if r.method == http.MethodGet && r.rangeHeader == "" {
			sawFullUnconditional = true
		}
	}
	if !sawFullUnconditional {
		t.Fatalf("expected an unconditional full GET at B after discarding state: %+v", reqsB)
	}
}

// ---------------------------------------------------------------------------
// Requirement 5: checksum gate before accepting the published file
// ---------------------------------------------------------------------------

func TestUID425ChecksumGateAcceptsAndRejects(t *testing.T) {
	content := []byte("uid425 checksum gated content\n")

	t.Run("valid checksum succeeds despite range-ignoring server", func(t *testing.T) {
		srv := newUID425FileServer(content, `"etag-cs"`)
		srv.always200 = true
		ln := srv.server(t)

		dst := filepath.Join(t.TempDir(), "blob")
		if err := os.WriteFile(dst, bytesRepeatUID425('?', len(content)/2), 0666); err != nil {
			t.Fatal(err)
		}

		raw := fmt.Sprintf("%s/blob?checksum=%s", ln.URL, uid425SHA256(content))
		if err := getter.GetFile(dst, raw); err != nil {
			t.Fatalf("expected valid checksum to pass, got: %v", err)
		}
		got, _ := os.ReadFile(dst)
		if !bytesEqualUID425(got, content) {
			t.Fatalf("published content mismatch: %q", got)
		}
	})

	t.Run("invalid checksum is reported", func(t *testing.T) {
		srv := newUID425FileServer(content, `"etag-cs2"`)
		ln := srv.server(t)

		dst := filepath.Join(t.TempDir(), "blob")
		bad := uid425SHA256(append([]byte(nil), content...))
		// Flip a hex character to produce a well-formed but wrong digest.
		if bad[0] == '0' {
			bad = "1" + bad[1:]
		} else {
			bad = "0" + bad[1:]
		}

		raw := fmt.Sprintf("%s/blob?checksum=%s", ln.URL, bad)
		err := getter.GetFile(dst, raw)
		if err == nil {
			t.Fatal("expected checksum mismatch error")
		}
		var csErr *getter.ChecksumError
		if !errors.As(err, &csErr) {
			t.Fatalf("expected *getter.ChecksumError, got %T: %v", err, err)
		}
	})
}

// ---------------------------------------------------------------------------
// tiny local byte helpers (avoid importing bytes just for two calls is not a
// goal; these keep failure messages explicit)
// ---------------------------------------------------------------------------

func bytesEqualUID425(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bytesRepeatUID425(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}
