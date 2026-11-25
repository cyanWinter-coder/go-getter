// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func uid424GitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

func uid424WriteMarkerDestination(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "original.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func uid424AssertMarkerDestinationUnchanged(t *testing.T, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dst, "original.txt"))
	if err != nil {
		t.Fatalf("original destination was modified or removed: %v", err)
	}
	if string(b) != "original" {
		t.Fatalf("original destination content = %q, want %q", b, "original")
	}
}

func TestUID424GitRecoveryMetadataRecordsSnapshot(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "recovery-metadata")
	repo.git("checkout", "-b", "snapshot")
	repo.commitFile("snapshot.txt", "one")
	wantCommit, err := repo.latestCommit()
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "target")
	rc, err := newRecoverableGitClone(dst, repo.url, "snapshot", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := rc.prepareObjects(context.Background()); err != nil {
		t.Fatalf("prepareObjects: %v", err)
	}

	meta, err := readGitRecoveryMetadata(rc.dir)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta.Source != repo.url.String() {
		t.Fatalf("Source = %q, want %q", meta.Source, repo.url.String())
	}
	if meta.Ref != "snapshot" {
		t.Fatalf("Ref = %q, want snapshot", meta.Ref)
	}
	if meta.Commit != wantCommit {
		t.Fatalf("Commit = %q, want full commit %q", meta.Commit, wantCommit)
	}
	if !fullGitCommitPattern.MatchString(meta.Commit) {
		t.Fatalf("Commit %q is not a full lowercase commit", meta.Commit)
	}
	if meta.State != gitRecoveryFetched {
		t.Fatalf("State = %q, want %q", meta.State, gitRecoveryFetched)
	}
}

func TestUID424GitRecoveryReusesObjectCacheAndPinsCommitAfterRefMove(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "recovery-ref-move")
	repo.git("checkout", "-b", "moving")
	repo.commitFile("a.txt", "a")
	firstCommit, err := repo.latestCommit()
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "target")
	rc, err := newRecoverableGitClone(dst, repo.url, "moving", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rc.prepareObjects(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rc.checkout(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted/mixed checkout, then move the remote ref.
	if err := os.Remove(filepath.Join(rc.dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	repo.commitFile("b.txt", "b")
	movedCommit, err := repo.latestCommit()
	if err != nil {
		t.Fatal(err)
	}
	if firstCommit == movedCommit {
		t.Fatal("test setup did not move the ref")
	}

	// Make the source unavailable. Recovery must finish the already resolved
	// snapshot from its object cache rather than contact or trust the source.
	movedRepoDir := repo.dir + "-moved"
	if err := os.Rename(repo.dir, movedRepoDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Rename(movedRepoDir, repo.dir) }()

	rc2, err := newRecoverableGitClone(dst, repo.url, "moving", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc2.prepareObjects(ctx); err != nil {
		t.Fatalf("reuse prepared object cache: %v", err)
	}
	if rc2.metadata.Commit != firstCommit {
		t.Fatalf("recovery commit changed to %q after ref move; want pinned %q", rc2.metadata.Commit, firstCommit)
	}
	if err := rc2.checkout(ctx); err != nil {
		t.Fatalf("checkout from reused object cache: %v", err)
	}
	if got := uid424GitHead(t, rc2.dir); got != firstCommit {
		t.Fatalf("HEAD = %q, want pinned commit %q", got, firstCommit)
	}
	if _, err := os.Stat(filepath.Join(rc2.dir, "a.txt")); err != nil {
		t.Fatalf("interrupted worktree was not restored: %v", err)
	}
}

func TestUID424GitRecoveryDoesNotReuseUnsuccessfulFetch(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "recovery-incomplete-fetch")
	repo.commitFile("a.txt", "a")
	wantCommit, err := repo.latestCommit()
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "target")
	rc, err := newRecoverableGitClone(dst, repo.url, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rc.prepareObjects(ctx); err != nil {
		t.Fatal(err)
	}

	// A fetching journal means the fetch transaction did not durably succeed.
	rc.metadata.State = gitRecoveryState("fetching")
	if err := writeGitRecoveryMetadata(rc.dir, rc.metadata); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(rc.dir, "partial-fetch-marker")
	if err := os.WriteFile(marker, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rc.prepareObjects(ctx); err != nil {
		t.Fatalf("fresh fetch: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("partial fetch directory was reused; marker stat err=%v", err)
	}
	if rc.metadata.State != gitRecoveryFetched || rc.metadata.Commit != wantCommit {
		t.Fatalf("unexpected metadata after fresh fetch: %+v", rc.metadata)
	}
}

func TestUID424GitGetter_CheckoutFailureDoesNotPublish(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell git wrapper")
	}

	repo := testGitRepo(t, "checkout-failure")
	repo.commitFile("ok.txt", "ok")
	dst := uid424WriteMarkerDestination(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "checkout" ]; then
  echo "injected checkout failure" >&2
  exit 1
fi
exec %q "$@"
`, realGit)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	g := new(GitGetter)
	err = g.Get(dst, repo.url)
	if err == nil || !strings.Contains(err.Error(), "injected checkout failure") {
		t.Fatalf("Get error = %v, want injected checkout failure", err)
	}
	uid424AssertMarkerDestinationUnchanged(t, dst)
}

func TestUID424GitGetter_RetryAfterCheckoutFailureReusesObjectCache(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell git wrapper")
	}

	repo := testGitRepo(t, "checkout-retry")
	repo.commitFile("ok.txt", "ok")
	dst := filepath.Join(t.TempDir(), "target")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	failCheckout := filepath.Join(bin, "fail-checkout")
	cloneCount := filepath.Join(bin, "clone-count")
	if err := os.WriteFile(failCheckout, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(bin, "git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "clone" ]; then
  echo clone >> %[2]q
fi
if [ "$1" = "checkout" ] && [ -f %[1]q ]; then
  rm -f %[1]q
  echo "injected first checkout failure" >&2
  exit 1
fi
exec %[3]q "$@"
`, failCheckout, cloneCount, realGit)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	g := new(GitGetter)
	if err := g.Get(dst, repo.url); err == nil ||
		!strings.Contains(err.Error(), "injected first checkout failure") {
		t.Fatalf("first Get error = %v, want injected checkout failure", err)
	}
	count, err := os.ReadFile(cloneCount)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(count), "clone\n"); got != 1 {
		t.Fatalf("clone count after first attempt = %d, want 1", got)
	}

	if err := g.Get(dst, repo.url); err != nil {
		t.Fatalf("retry Get: %v", err)
	}
	count, err = os.ReadFile(cloneCount)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(count), "clone\n"); got != 1 {
		t.Fatalf("clone count after retry = %d, want object cache reuse and count 1", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "ok.txt")); err != nil {
		t.Fatalf("published destination missing ok.txt: %v", err)
	}
}

func TestUID424GitGetter_SubmoduleFailureDoesNotPublish(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	child := testGitRepo(t, "failed-child")
	child.commitFile("child.txt", "child")

	parent := testGitRepo(t, "failed-parent")
	parent.commitFile("parent.txt", "parent")
	rel, err := filepath.Rel(parent.dir, child.dir)
	if err != nil {
		t.Fatal(err)
	}
	rel = strings.ReplaceAll(rel, `\`, `/`)
	parent.git("-c", "protocol.file.allow=always", "submodule", "add", "-f", rel, "child")
	parent.git("commit", "-m", "add child submodule")

	badSubmodule := filepath.Join(t.TempDir(), "missing-submodule")
	parent.git("config", "-f", ".gitmodules", "submodule.child.url", badSubmodule)
	parent.git("add", ".gitmodules")
	parent.git("commit", "-m", "break submodule URL")

	dst := uid424WriteMarkerDestination(t)
	g := new(GitGetter)
	err = g.Get(dst, parent.url)
	if err == nil {
		t.Fatal("expected submodule Get to fail")
	}
	if !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("error %q does not mention submodule", err)
	}
	uid424AssertMarkerDestinationUnchanged(t, dst)
}

func TestUID424GitGetter_RefMovePublishesCompleteNewSnapshot(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "atomic-ref-move")
	repo.git("checkout", "-b", "snapshot")
	repo.commitFile("old.txt", "old")

	g := new(GitGetter)
	dst := filepath.Join(t.TempDir(), "target")
	q := repo.url.Query()
	q.Set("ref", "snapshot")
	u := *repo.url
	u.RawQuery = q.Encode()

	if err := g.Get(dst, &u); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "old.txt")); err != nil {
		t.Fatal(err)
	}
	oldHead := uid424GitHead(t, dst)

	repo.git("rm", "old.txt")
	repo.commitFile("new.txt", "new")
	newHead, err := repo.latestCommit()
	if err != nil {
		t.Fatal(err)
	}

	if err := g.Get(dst, &u); err != nil {
		t.Fatalf("second Get after ref move: %v", err)
	}
	if got := uid424GitHead(t, dst); got != newHead {
		t.Fatalf("published HEAD = %s, want %s (old was %s)", got, newHead, oldHead)
	}
	if _, err := os.Stat(filepath.Join(dst, "new.txt")); err != nil {
		t.Fatalf("new snapshot file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old file remains after atomic snapshot replacement (err=%v)", err)
	}
}

func TestUID424GitGetter_ConcurrentSameDestination(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "concurrent")
	repo.commitFile("shared.txt", "shared")

	g := new(GitGetter)
	dst := filepath.Join(t.TempDir(), "target")
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := g.Get(dst, repo.url); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Get: %v", err)
		}
	}

	wantHead, err := repo.latestCommit()
	if err != nil {
		t.Fatal(err)
	}
	if got := uid424GitHead(t, dst); got != wantHead {
		t.Fatalf("concurrent result HEAD = %q, want %q", got, wantHead)
	}
	if _, err := os.Stat(filepath.Join(dst, "shared.txt")); err != nil {
		t.Fatalf("consistent worktree missing shared file: %v", err)
	}
}

func TestUID424Get_ConcurrentGitSameDestination(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "concurrent-package-get")
	repo.commitFile("shared.txt", "shared")
	dst := filepath.Join(t.TempDir(), "target")

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := Get(dst, "git::"+repo.url.String()); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent package Get: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "shared.txt")); err != nil {
		t.Fatalf("shared destination missing file: %v", err)
	}
}

func TestUID424GitGetter_CancelBeforeStartKeepsDestinationAndCanRetry(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}

	repo := testGitRepo(t, "cancel-before-start")
	repo.commitFile("ok.txt", "ok")
	dst := uid424WriteMarkerDestination(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := new(GitGetter)
	g.SetClient(&Client{Ctx: ctx})
	err := g.Get(dst, repo.url)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error = %v, want context.Canceled", err)
	}
	uid424AssertMarkerDestinationUnchanged(t, dst)

	retry := new(GitGetter)
	if err := retry.Get(dst, repo.url); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "ok.txt")); err != nil {
		t.Fatalf("retried destination missing ok.txt: %v", err)
	}
}

func TestUID424GitGetter_CancelDuringCloneAndRetry(t *testing.T) {
	if !testHasGit {
		t.Skip("git not found, skipping")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell git wrapper")
	}

	repo := testGitRepo(t, "cancel-during-clone")
	repo.commitFile("ok.txt", "ok")
	dst := uid424WriteMarkerDestination(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	block := filepath.Join(bin, "block")
	started := filepath.Join(bin, "started")
	if err := os.WriteFile(block, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(bin, "git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "clone" ] && [ -f %[1]q ]; then
  : > %[2]q
  exec sleep 30
fi
exec %[3]q "$@"
`, block, started, realGit)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := new(GitGetter)
	g.SetClient(&Client{Ctx: ctx})

	errCh := make(chan error, 1)
	go func() { errCh <- g.Get(dst, repo.url) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Get error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled git clone did not return")
	}
	uid424AssertMarkerDestinationUnchanged(t, dst)

	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	retry := new(GitGetter)
	if err := retry.Get(dst, repo.url); err != nil {
		t.Fatalf("retry after mid-clone cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "ok.txt")); err != nil {
		t.Fatalf("retried destination missing ok.txt: %v", err)
	}
}

func TestUID424RemoveAllContinueReportsFailureAndRemovesOtherPaths(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}

	badParent := filepath.Join(root, "bad-parent")
	bad := filepath.Join(badParent, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badParent, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(badParent, 0o755) }()

	err := removeAllContinue(good, bad)
	if err == nil {
		t.Fatal("expected cleanup error for permission-denied path")
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error %v does not contain *CleanupError", err)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("cleanup error %q does not identify path %q", err, bad)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatalf("other path was not cleaned; stat err=%v", err)
	}
}
