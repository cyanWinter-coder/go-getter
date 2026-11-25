// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package getter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gitRecoveryMetadataName is the recovery journal stored inside a staged
// Git repository's Git directory. It is deliberately not part of the working
// tree, so it is never published as a source file.
const gitRecoveryMetadataName = "go-getter-recovery.json"

const gitRecoveryMetadataVersion = 1

var fullGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gitRecoveryState describes how far an unpublished recoverable clone has
// progressed.
type gitRecoveryState string

const (
	// gitRecoveryFetched means the object database has been populated and the
	// resolved commit has been recorded. Checkout has not completed.
	gitRecoveryFetched gitRecoveryState = "fetched"

	// gitRecoveryCheckingOut means a checkout attempt is in progress. The
	// working tree may be partial, so recovery must reset/checkout from the
	// recorded object database rather than trust the worktree.
	gitRecoveryCheckingOut gitRecoveryState = "checking-out"

	// gitRecoveryCheckedOut means the exact recorded commit is checked out.
	gitRecoveryCheckedOut gitRecoveryState = "checked-out"

	// gitRecoverySubmodules means submodules are being prepared.
	gitRecoverySubmodules gitRecoveryState = "submodules"

	// gitRecoveryReady means checkout and submodules succeeded and the stage
	// may be published.
	gitRecoveryReady gitRecoveryState = "ready"
)

// gitRecoveryMetadata is the durable snapshot used to resume an interrupted
// Git fetch without reusing a stale or mixed worktree.
type gitRecoveryMetadata struct {
	Version int `json:"version"`

	// Source is the canonical getter URL without go-getter's ref, depth and
	// sshkey query parameters.
	Source string `json:"source"`

	// Ref is the ref exactly as requested. An empty string means the remote
	// default branch.
	Ref string `json:"ref,omitempty"`

	// Commit is the full, resolved commit that this recovery attempt must
	// finish and publish.
	Commit string `json:"commit"`

	Depth int `json:"depth,omitempty"`

	State     gitRecoveryState `json:"state"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// CleanupError reports that downloaded content was safely published but a
// temporary or backup path could not be removed. The published destination is
// valid; the path retained in Path makes the leftover diagnosable.
type CleanupError struct {
	Path string
	Err  error
}

func (e *CleanupError) Error() string {
	if e == nil {
		return "git getter cleanup error"
	}
	if e.Err == nil {
		return fmt.Sprintf("failed to clean up %q", e.Path)
	}
	return fmt.Sprintf("failed to clean up %q: %v", e.Path, e.Err)
}

func (e *CleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// recoverableGitClone prepares a complete repository in an unpublished
// sibling directory and only then replaces dst. A deterministic recovery
// directory allows a later Get to reuse a successfully fetched object
// database after cancellation or process interruption.
type recoverableGitClone struct {
	dst        string
	dir        string
	sourceText string
	ref        string
	depth      int
	sshKeyFile string

	metadata gitRecoveryMetadata
}

func newRecoverableGitClone(dst string, u *url.URL, ref string, depth int, sshKeyFile string) (*recoverableGitClone, error) {
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return nil, err
	}
	absDst = filepath.Clean(absDst)

	sourceText := ""
	if u != nil {
		sourceText = u.String()
	}

	return &recoverableGitClone{
		dst:        absDst,
		dir:        gitRecoveryDir(absDst),
		sourceText: sourceText,
		ref:        ref,
		depth:      depth,
		sshKeyFile: sshKeyFile,
	}, nil
}

func gitRecoveryDir(dst string) string {
	return dst + ".recover"
}

func gitRecoveryMetadataPath(repoDir string) string {
	return filepath.Join(repoDir, ".git", gitRecoveryMetadataName)
}

// run executes the complete recoverable clone and atomic publication.
func (r *recoverableGitClone) run(ctx context.Context, g *GitGetter, client *Client) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.dst), 0o755); err != nil {
		return err
	}

	if err := r.prepareObjects(ctx); err != nil {
		return err
	}
	if err := r.checkout(ctx); err != nil {
		return err
	}

	if err := r.updateState(ctx, gitRecoverySubmodules); err != nil {
		return err
	}
	if err := g.fetchSubmodulesForClient(ctx, r.dir, r.sshKeyFile, r.depth, client); err != nil {
		return err
	}

	if err := r.updateState(ctx, gitRecoveryReady); err != nil {
		return err
	}
	return r.publish()
}

// prepareObjects reuses a journaled, successfully cloned object cache or
// creates and journals a fresh no-checkout clone.
func (r *recoverableGitClone) prepareObjects(ctx context.Context) error {
	meta, err := readGitRecoveryMetadata(r.dir)
	if err == nil && r.canReuseObjects(ctx, meta) {
		r.metadata = meta
		return nil
	}

	if err := r.cloneFresh(ctx); err != nil {
		return err
	}

	resolveRef := r.ref
	if resolveRef == "" {
		resolveRef = "HEAD"
	}
	commit, err := resolveCheckoutRef(ctx, r.dir, resolveRef)
	if err != nil {
		return fmt.Errorf("resolve fetched Git commit: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if !fullGitCommitPattern.MatchString(commit) {
		return fmt.Errorf("resolved Git ref %q to invalid commit %q", r.ref, commit)
	}

	r.metadata = gitRecoveryMetadata{
		Version:   gitRecoveryMetadataVersion,
		Source:    r.sourceText,
		Ref:       r.ref,
		Commit:    commit,
		Depth:     r.depth,
		State:     gitRecoveryFetched,
		UpdatedAt: time.Now().UTC(),
	}
	return writeGitRecoveryMetadata(r.dir, r.metadata)
}

func (r *recoverableGitClone) canReuseObjects(ctx context.Context, meta gitRecoveryMetadata) bool {
	if meta.Version != gitRecoveryMetadataVersion {
		return false
	}
	if meta.Source != r.sourceText || meta.Ref != r.ref || meta.Depth != r.depth {
		return false
	}
	if !fullGitCommitPattern.MatchString(meta.Commit) {
		return false
	}
	switch meta.State {
	case gitRecoveryFetched, gitRecoveryCheckingOut, gitRecoveryCheckedOut,
		gitRecoverySubmodules, gitRecoveryReady:
	default:
		return false
	}

	// Every accepted state after the atomic "fetched" record was written has
	// complete fetch. Verify the object actually survived before reusing it.
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", "--end-of-options", meta.Commit+"^{commit}")
	cmd.Dir = r.dir
	return cmd.Run() == nil
}

// cloneFresh discards an untrusted/unjournaled recovery directory and makes
// a no-checkout clone. Separating fetch from checkout is what allows an
// interrupted checkout to resume from the same object database.
func (r *recoverableGitClone) cloneFresh(ctx context.Context) error {
	if err := os.RemoveAll(r.dir); err != nil {
		return errors.Join(
			fmt.Errorf("reset incomplete Git recovery directory %q: %w", r.dir, err),
			&CleanupError{Path: r.dir, Err: err},
		)
	}
	if err := os.MkdirAll(filepath.Dir(r.dir), 0o755); err != nil {
		return err
	}

	args := []string{"clone", "--no-checkout"}
	if r.depth > 0 {
		args = append(args, "--depth", strconv.Itoa(r.depth))
		if r.ref != "" {
			args = append(args, "--branch", r.ref)
		}
	}
	args = append(args, "--", r.sourceText, r.dir)

	cmd := exec.CommandContext(ctx, "git", args...)
	setupGitEnv(cmd, r.sshKeyFile)
	err := runGitCommand(ctx, cmd)
	if err != nil && r.depth > 0 && r.ref != "" && gitCommitIDRegex.MatchString(r.ref) {
		return fmt.Errorf("%w (note that setting 'depth' requires 'ref' to be a branch or tag name)", err)
	}
	return err
}

// checkout forces a pristine worktree at the exact recorded commit. Existing
// worktree contents are never accepted as proof of the checked-out revision.
func (r *recoverableGitClone) checkout(ctx context.Context) error {
	if err := r.updateState(ctx, gitRecoveryCheckingOut); err != nil {
		return err
	}

	// Remove files left by an interrupted checkout or failed submodule. The
	// second -f also removes nested Git repositories left by submodules. The
	// journal lives in .git and is therefore retained by git clean.
	if err := r.git(ctx, "", "clean", "-ffdx"); err != nil {
		return err
	}
	if err := r.git(ctx, "", "checkout", "--force", "--detach", "--end-of-options", r.metadata.Commit); err != nil {
		return err
	}
	return r.updateState(ctx, gitRecoveryCheckedOut)
}

func (r *recoverableGitClone) updateState(_ context.Context, state gitRecoveryState) error {
	r.metadata.State = state
	r.metadata.UpdatedAt = time.Now().UTC()
	return writeGitRecoveryMetadata(r.dir, r.metadata)
}

func (r *recoverableGitClone) git(ctx context.Context, sshKeyFile string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	setupGitEnv(cmd, sshKeyFile)
	return runGitCommand(ctx, cmd)
}

// publish atomically replaces dst with the completed stage. The previous
// destination is moved aside first and restored if publication fails.
func (r *recoverableGitClone) publish() error {
	parent := filepath.Dir(r.dst)

	backup, err := temporarySiblingPath(parent, ".go-getter-backup-*")
	if err != nil {
		return err
	}

	_, statErr := os.Lstat(r.dst)
	dstExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	if dstExists {
		if err := os.Rename(r.dst, backup); err != nil {
			return fmt.Errorf("move existing Git destination aside: %w", err)
		}
	}

	if err := os.Rename(r.dir, r.dst); err != nil {
		if dstExists {
			if restoreErr := os.Rename(backup, r.dst); restoreErr != nil {
				return errors.Join(
					fmt.Errorf("publish recovered Git clone %q: %w", r.dst, err),
					fmt.Errorf("restore previous destination from %q: %w", backup, restoreErr),
				)
			}
		}
		return fmt.Errorf("publish recovered Git clone %q: %w", r.dst, err)
	}

	if dstExists {
		if err := os.RemoveAll(backup); err != nil {
			return &CleanupError{Path: backup, Err: err}
		}
	}
	return nil
}

// temporarySiblingPath returns a unique non-existent path in parent.
func temporarySiblingPath(parent, pattern string) (string, error) {
	f, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func readGitRecoveryMetadata(repoDir string) (gitRecoveryMetadata, error) {
	var meta gitRecoveryMetadata
	b, err := os.ReadFile(gitRecoveryMetadataPath(repoDir))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, fmt.Errorf("parse Git recovery metadata: %w", err)
	}
	return meta, nil
}

func writeGitRecoveryMetadata(repoDir string, meta gitRecoveryMetadata) error {
	path := gitRecoveryMetadataPath(repoDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".metadata-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(meta); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runGitCommand preserves command diagnostics while making caller
// cancellation detectable with errors.Is(err, context.Canceled).
func runGitCommand(ctx context.Context, cmd *exec.Cmd) error {
	err := getRunCommand(cmd)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}

// gitDestinationLocks serializes operations targeting the same local path.
// The buffered channel is a cancellable mutex.
var gitDestinationLocks sync.Map

type gitDestinationLock struct {
	ch chan struct{}
}

func lockGitDestination(ctx context.Context, dst string) (func(), error) {
	value, _ := gitDestinationLocks.LoadOrStore(dst, &gitDestinationLock{ch: make(chan struct{}, 1)})
	lock := value.(*gitDestinationLock)

	select {
	case lock.ch <- struct{}{}:
		return func() { <-lock.ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// gitCloneFlights coalesces concurrent gets for exactly the same destination
// and source snapshot request. All callers therefore observe one result
// instead of racing two publications at the same path.
var gitCloneFlights sync.Map

type gitCloneFlight struct {
	done chan struct{}
	err  error
}

func runGitCloneFlight(ctx context.Context, key string, fn func(context.Context) error) (retErr error) {
	candidate := &gitCloneFlight{done: make(chan struct{})}
	actual, loaded := gitCloneFlights.LoadOrStore(key, candidate)
	if loaded {
		flight := actual.(*gitCloneFlight)
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	flight := candidate
	defer func() {
		flight.err = retErr
		close(flight.done)
		gitCloneFlights.Delete(key)
	}()

	return fn(ctx)
}

func gitCloneFlightKey(dst, source, ref string, depth int, sshKey string, timeout time.Duration) string {
	return strings.Join([]string{
		"v1",
		filepath.Clean(dst),
		source,
		ref,
		strconv.Itoa(depth),
		sshKey,
		timeout.String(),
	}, "\x00")
}

// removeAllContinue removes every path even if an earlier removal fails. The
// returned error joins all *CleanupError values so one source's cleanup
// failure neither hides the others nor blocks unrelated sources.
func removeAllContinue(paths ...string) error {
	var errs []error
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, &CleanupError{Path: path, Err: err})
		}
	}
	return errors.Join(errs...)
}
