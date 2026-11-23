// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"errors"
	"fmt"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		Input  string
		Pwd    string
		Output string
		Err    bool
	}{
		{"./foo", "/foo", "file:///foo/foo", false},
		{"git::./foo", "/foo", "git::file:///foo/foo", false},
		{
			"git::github.com/hashicorp/foo",
			"",
			"git::https://github.com/hashicorp/foo.git",
			false,
		},
		{
			"./foo//bar",
			"/foo",
			"file:///foo/foo//bar",
			false,
		},
		{
			"git::github.com/hashicorp/foo//bar",
			"",
			"git::https://github.com/hashicorp/foo.git//bar",
			false,
		},
		{
			"git::https://github.com/hashicorp/consul.git",
			"",
			"git::https://github.com/hashicorp/consul.git",
			false,
		},
		{
			"git::https://person@someothergit.com/foo/bar",
			"",
			"git::https://person@someothergit.com/foo/bar",
			false,
		},
		{
			"git::https://person@someothergit.com/foo/bar",
			"/bar",
			"git::https://person@someothergit.com/foo/bar",
			false,
		},
		{
			"./foo/archive//*",
			"/bar",
			"file:///bar/foo/archive//*",
			false,
		},

		// https://github.com/hashicorp/go-getter/pull/124
		{
			"git::ssh://git@my.custom.git/dir1/dir2",
			"",
			"git::ssh://git@my.custom.git/dir1/dir2",
			false,
		},
		{
			"git::git@my.custom.git:dir1/dir2",
			"/foo",
			"git::ssh://git@my.custom.git/dir1/dir2",
			false,
		},
		{
			"git::git@my.custom.git:dir1/dir2",
			"",
			"git::ssh://git@my.custom.git/dir1/dir2",
			false,
		},
	}

	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d %s", i, tc.Input), func(t *testing.T) {
			output, err := Detect(tc.Input, tc.Pwd, Detectors)
			if err != nil != tc.Err {
				t.Fatalf("%d: bad err: %s", i, err)
			}
			if output != tc.Output {
				t.Fatalf("%d: bad output: %s\nexpected: %s", i, output, tc.Output)
			}
		})
	}
}

func TestDetect_encodedBoundaries(t *testing.T) {
	cases := []struct {
		Input  string
		Output string
	}{
		// Percent-encoded query values pass through detection
		// byte-for-byte: they are decoded exactly once, by the getter
		// that finally consumes the URL.
		{
			"github.com/hashicorp/go-getter//subdir?foo=bar%20baz",
			"git::https://github.com/hashicorp/go-getter.git//subdir?foo=bar%20baz",
		},
		{
			"github.com/hashicorp/go-getter//subdir?foo=a+b",
			"git::https://github.com/hashicorp/go-getter.git//subdir?foo=a+b",
		},
		// An encoded slash in the path must not collapse into a path
		// separator when the subdir is spliced back in.
		{
			"bucket.s3.amazonaws.com/foo%2Fbar//sub",
			"s3::https://s3.amazonaws.com/bucket/foo%2Fbar//sub",
		},
		// A getter forced on the source wins over the one the detector
		// would impose, and the result is not re-detected.
		{
			"http::github.com/hashicorp/foo",
			"http::https://github.com/hashicorp/foo.git",
		},
		{
			"http::github.com/hashicorp/foo//sub?x=1",
			"http::https://github.com/hashicorp/foo.git//sub?x=1",
		},
	}

	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d %s", i, tc.Input), func(t *testing.T) {
			output, err := Detect(tc.Input, "", Detectors)
			if err != nil {
				t.Fatalf("err: %s", err)
			}
			if output != tc.Output {
				t.Fatalf("bad output: %s\nexpected: %s", output, tc.Output)
			}
		})
	}
}

func TestDetect_noDetectorStableError(t *testing.T) {
	// With no detectors configured and a source that is not a valid URL,
	// Detect must return the stable ErrNoDetector error.
	_, err := Detect("github.com/hashicorp/foo", "", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNoDetector) {
		t.Fatalf("expected ErrNoDetector, got: %s", err)
	}
}
