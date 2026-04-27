//go:build verify

package orch_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireBd skips the test when the bd CLI is not on PATH. The auto-init
// path requires `bd init`; we don't want CI runners without bd to fail
// the suite, but we do want the tests to run wherever bd is present.
func requireBd(t *testing.T) {
	t.Helper()
	if !orch.BeadsCLIAvailable() {
		t.Skip("bd CLI not on PATH")
	}
}

// initBdRepo creates a fresh bd-initialised .beads/ directory under a temp
// directory and returns its path. Encapsulates the git-init + bd-init
// dance every BDCLIStore test needs (bd init's --stealth setup expects to
// run inside a git repo even when no hooks are installed). Takes
// testing.TB so contract tests, benchmarks, and the differential test can
// all reuse it.
func initBdRepo(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	gitInit := exec.Command("git", "init", "-q")
	gitInit.Dir = dir
	require.NoError(tb, gitInit.Run())
	bdInit := exec.Command("bd", "init", "--stealth", "--non-interactive", "--quiet")
	bdInit.Dir = dir
	out, err := bdInit.CombinedOutput()
	require.NoError(tb, err, "bd init: %s", out)
	return filepath.Join(dir, ".beads")
}

// TestResolveLedgerDir pins the path-resolution contract. Three input
// classes (LedgerBeads, LedgerFS, empty) map to deterministic outputs.
// A regression that flipped beads to runDir or fs to repoPath would route
// the dispatcher to a different store than the planner writes to —
// exactly the failure mode this whole change set fixes.
func TestResolveLedgerDir(t *testing.T) {
	const repoPath = "/repo"
	const runDir = "/run"

	cases := []struct {
		name     string
		kind     orch.LedgerKind
		override string
		want     string
	}{
		{"beads_routes_to_repo_dot_beads", orch.LedgerBeads, "", "/repo/.beads"},
		{"fs_routes_to_run_dir_ledger", orch.LedgerFS, "", "/run/ledger"},
		{"empty_routes_to_run_dir_ledger", "", "", "/run/ledger"},
		{"override_wins_over_kind", orch.LedgerBeads, "/test/ledger", "/test/ledger"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orch.ResolveLedgerDir(repoPath, runDir, tc.kind, tc.override)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEnsureBeadsInitialised_AlreadyExists pins the no-op short-circuit:
// when <repoPath>/.beads/ already exists, EnsureBeadsInitialised returns
// (false, nil) without invoking bd. Runs in any environment because the
// short-circuit happens before exec.LookPath.
func TestEnsureBeadsInitialised_AlreadyExists(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".beads"), 0o755))

	created, err := orch.EnsureBeadsInitialised(repo)
	require.NoError(t, err)
	assert.False(t, created, "must return created=false when .beads/ already exists")
}

// A regular file at <repo>/.beads must be rejected, not silently treated
// as initialised.
func TestEnsureBeadsInitialised_RegularFileRejected(t *testing.T) {
	repo := t.TempDir()
	beadsPath := filepath.Join(repo, ".beads")
	require.NoError(t, os.WriteFile(beadsPath, []byte("not a dir"), 0o644))

	created, err := orch.EnsureBeadsInitialised(repo)
	assert.False(t, created)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// TestEnsureBeadsInitialised_NoBdReturnsCLIMissing pins the fail-fast
// behavior when bd is not on PATH: a clear sentinel rather than a confusing
// downstream error from beads.Open. PATH is cleared for the test so the
// outcome is deterministic regardless of the host.
func TestEnsureBeadsInitialised_NoBdReturnsCLIMissing(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("PATH", "")

	created, err := orch.EnsureBeadsInitialised(repo)
	assert.False(t, created)
	require.Error(t, err)
	assert.True(t, errors.Is(err, orch.ErrBeadsCLIMissing),
		"missing bd must surface as ErrBeadsCLIMissing, got: %v", err)
}

// TestEnsureBeadsInitialised_CreatesIfMissing exercises the happy path
// when bd is on PATH. Gated by requireBd so CI runners without bd skip
// this test rather than fail.
func TestEnsureBeadsInitialised_CreatesIfMissing(t *testing.T) {
	requireBd(t)
	repo := t.TempDir()
	// bd init operates inside a git repo; --stealth suppresses git hooks
	// but the working tree must still be a git repo.
	gitInit := exec.Command("git", "init", "-q")
	gitInit.Dir = repo
	require.NoError(t, gitInit.Run())

	created, err := orch.EnsureBeadsInitialised(repo)
	require.NoError(t, err)
	assert.True(t, created, "fresh repo must report created=true")
	assert.DirExists(t, filepath.Join(repo, ".beads"), ".beads/ must exist after init")
}

// TestBeadsCLIAvailable_EmptyPath sanity-checks the precondition helper.
// With PATH cleared, BeadsCLIAvailable returns false; the inverse is host-
// dependent so we don't pin it.
func TestBeadsCLIAvailable_EmptyPath(t *testing.T) {
	t.Setenv("PATH", "")
	assert.False(t, orch.BeadsCLIAvailable())
}

// TestNewStore_ExplicitBeadsDoesNotFallback pins the tightened semantics:
// an explicit --ledger beads request is strict — when beads fails, the
// error surfaces rather than silently writing FS-store JSON into a
// directory bd manages. Empty kind retains fallback (covered above).
func TestNewStore_ExplicitBeadsDoesNotFallback(t *testing.T) {
	// Probe whether beads is reachable; if it is, the strict path is not
	// exercised and the assertion would be vacuous.
	if _, kind, err := orch.NewStore("", filepath.Join(t.TempDir(), "probe")); err == nil && kind == orch.LedgerBeads {
		t.Skip("Beads backend reachable — strict-no-fallback path cannot be exercised")
	}
	_, _, err := orch.NewStore(orch.LedgerBeads, t.TempDir())
	require.Error(t, err, "explicit beads with unreachable backend must error, not fall back")
}

// TestNewStore_ExplicitFS creates an FSStore explicitly by kind.
func TestNewStore_ExplicitFS(t *testing.T) {
	store, kind, err := orch.NewStore(orch.LedgerFS, t.TempDir())
	require.NoError(t, err)
	defer store.Close()
	assert.Equal(t, orch.LedgerFS, kind)

	// Smoke: create and get a task.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "smoke", Status: orch.StatusOpen}))
	got, err := store.GetTask("smoke")
	require.NoError(t, err)
	assert.Equal(t, "smoke", got.ID)
}

// TestNewStore_DefaultsToBeads verifies the default kind triggers beads (which
// falls back to FS when Dolt is unavailable). The returned kind tells us which
// backend is actually active.
func TestNewStore_DefaultsToBeads(t *testing.T) {
	store, kind, err := orch.NewStore("", t.TempDir())
	require.NoError(t, err)
	defer store.Close()

	// kind is either LedgerBeads (Dolt present) or LedgerFS (fallback).
	assert.Contains(t, []orch.LedgerKind{orch.LedgerBeads, orch.LedgerFS}, kind)

	// Smoke: the store is functional regardless of backend.
	require.NoError(t, store.CreateTask(&orch.Task{ID: "fallback", Status: orch.StatusOpen}))
	got, err := store.GetTask("fallback")
	require.NoError(t, err)
	assert.Equal(t, "fallback", got.ID)
}

// TestNewStore_UnknownKind verifies that an unknown kind returns an error.
func TestNewStore_UnknownKind(t *testing.T) {
	_, _, err := orch.NewStore("sqlite", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown ledger kind")
}
