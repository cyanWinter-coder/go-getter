// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// resumeStateVersion is the version of the on-disk resume metadata format.
const resumeStateVersion = 1

// resumeStateSuffix is appended to the destination file name to store the
// validator and resource identity of a partial download. It is removed once
// the download completes.
const resumeStateSuffix = ".getter-resume"

// resumeState is the persisted identity of a partial download.
//
// It exists so that a subsequent GetFile call can prove that the bytes left in
// the destination file are a prefix of the same representation the server is
// about to send. Without it, a server that ignores a Range request, or a
// redirect to a different resource, could make the getter concatenate two
// unrelated payloads into a corrupt but seemingly complete file.
type resumeState struct {
	Version int `json:"version"`

	// URL is the canonicalized final URL (after redirects) that the partial
	// bytes were downloaded from.
	URL string `json:"url,omitempty"`

	// ETag and LastModified are the representation validators reported by the
	// server, when available.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`

	// TotalSize is the full representation size in bytes, when known.
	TotalSize int64 `json:"total_size,omitempty"`
}

func resumeStatePath(dst string) string {
	return filepath.Clean(dst) + resumeStateSuffix
}

// loadResumeState reads the resume metadata for dst. A missing or malformed
// state is treated as "no state": callers fall back to a full download rather
// than failing, because the destination may simply be a user-supplied partial
// file.
func loadResumeState(dst string) *resumeState {
	b, err := os.ReadFile(resumeStatePath(dst))
	if err != nil {
		return nil
	}
	var st resumeState
	if err := json.Unmarshal(b, &st); err != nil || st.Version != resumeStateVersion {
		return nil
	}
	return &st
}

// save writes the resume metadata atomically so a cancellation can never leave
// a half-written state file.
func (st *resumeState) save(dst string) error {
	if st == nil {
		return nil
	}
	st.Version = resumeStateVersion

	target := resumeStatePath(dst)
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".getter-resume-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	data, err := json.Marshal(st)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return err
	}
	return nil
}

// removeResumeState deletes any resume metadata for dst. It is called once the
// download has completed. A missing state is not an error.
func removeResumeState(dst string) {
	_ = os.Remove(resumeStatePath(dst))
}

// discardPartial truncates the partial destination back to zero bytes and
// drops any incompatible resume metadata.
func discardPartial(f *os.File, dst string) error {
	removeResumeState(dst)
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	return nil
}

// canonicalURL returns a normalized representation of u used to decide whether
// two responses refer to the same resource. It only performs semantics-preserving
// normalization: lower-cased scheme/host, default ports removed, fragments
// dropped, dot segments resolved and query parameters canonically encoded.
func canonicalURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.Fragment = ""
	c.RawFragment = ""
	c.Scheme = strings.ToLower(c.Scheme)
	if host := strings.ToLower(c.Hostname()); host != "" {
		port := c.Port()
		if (c.Scheme == "http" && port == "80") || (c.Scheme == "https" && port == "443") {
			c.Host = host
		} else if port != "" {
			c.Host = host + ":" + port
		} else {
			c.Host = host
		}
	}
	if p := path.Clean(c.Path); p == "." || p == "" {
		c.Path = "/"
	} else {
		c.Path = p
	}
	// Query().Encode sorts parameters and deterministically re-encodes them.
	if c.RawQuery != "" {
		c.RawQuery = c.Query().Encode()
	}
	return c.String()
}

// validatorsDiffer reports whether the response validators contradict the
// ones recorded for the partial download. ETag takes precedence over
// Last-Modified. If neither side provides a comparable validator the result is
// false (cannot prove a difference).
func validatorsDiffer(st *resumeState, etag, lastModified string) bool {
	if st == nil {
		return false
	}
	if st.ETag != "" && etag != "" {
		return st.ETag != etag
	}
	if st.LastModified != "" && lastModified != "" {
		return st.LastModified != lastModified
	}
	return false
}

// ifRangeValidator returns the validator to send in the If-Range header,
// preferring a recorded ETag and falling back to Last-Modified. An empty
// string means no validator is available.
func ifRangeValidator(st *resumeState, headETag, headLastModified string) string {
	if st != nil {
		if st.ETag != "" {
			return st.ETag
		}
		if st.LastModified != "" {
			return st.LastModified
		}
	}
	if headETag != "" {
		return headETag
	}
	return headLastModified
}

// contentRange is a parsed HTTP Content-Range header.
type contentRange struct {
	start int64
	end   int64
	total int64
	// complete is true when both start and end byte offsets are known.
	complete bool
}

// parseContentRange parses a Content-Range header value such as
// "bytes 0-99/200", "bytes 0-99/*" or the 416 form "bytes */200".
// It returns nil when the value is absent or cannot be parsed.
func parseContentRange(v string) *contentRange {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	const prefix = "bytes "
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return nil
	}
	rest := strings.TrimSpace(v[len(prefix):])
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return nil
	}

	cr := &contentRange{total: -1}
	if total := strings.TrimSpace(parts[1]); total != "*" {
		n, err := strconv.ParseInt(total, 10, 64)
		if err != nil || n < 0 {
			return nil
		}
		cr.total = n
	}

	rangePart := strings.TrimSpace(parts[0])
	if rangePart == "*" {
		// Unsatisfiable range form: "bytes */123".
		return cr
	}
	se := strings.SplitN(rangePart, "-", 2)
	if len(se) != 2 {
		return nil
	}
	start, err := strconv.ParseInt(strings.TrimSpace(se[0]), 10, 64)
	if err != nil || start < 0 {
		return nil
	}
	end, err := strconv.ParseInt(strings.TrimSpace(se[1]), 10, 64)
	if err != nil || end < start {
		return nil
	}
	cr.start = start
	cr.end = end
	cr.complete = true
	return cr
}

// headerValidator extracts the ETag and Last-Modified validators from a
// response/HEAD header set.
func headerValidator(h http.Header) (etag, lastModified string) {
	return h.Get("ETag"), h.Get("Last-Modified")
}
