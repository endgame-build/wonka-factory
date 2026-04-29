package orch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// StoreConstructor creates a Store backed by the given directory.
type StoreConstructor func(dir string) (Store, error)

// storeRegistry maps ledger kind names to their constructors.
var storeRegistry = map[LedgerKind]StoreConstructor{
	LedgerFS:    func(dir string) (Store, error) { return NewFSStore(dir) },
	LedgerBeads: func(dir string) (Store, error) { return NewBeadsStore(dir, defaultActor) },
	LedgerBDCLI: func(dir string) (Store, error) { return NewBDCLIStore(dir, defaultActor) },
}

// beadsFallbackOnce ensures the Beads→FS fallback warning prints only once per process.
var beadsFallbackOnce sync.Once

// ErrBeadsCLIMissing signals that `--ledger beads` was requested but the `bd`
// binary is not on PATH. Auto-init requires the bd CLI (the beads SDK does
// not expose a programmatic Init); callers should fail fast rather than
// silently degrade because Charlie's contract pins the ledger location.
var ErrBeadsCLIMissing = errors.New("bd CLI not found on PATH — install bd or run `bd init` manually before `wonka run --ledger beads`")

// ResolveLedgerDir returns the directory wonka opens for a given ledger kind.
//
//	LedgerBeads → <repoPath>/.beads/   (shared with bd; see BVV-DSN-04)
//	LedgerBDCLI → <repoPath>/.beads/   (same database, accessed via bd CLI)
//	LedgerFS    → <runDir>/ledger/     (per-run dev convenience)
//	empty kind  → <runDir>/ledger/     (test/legacy default; CLI sets kind explicitly)
//
// The override parameter is non-empty only in tests, where it is wired to
// Engine.testLedgerDir via Engine.SetTestLedgerDir (see testhooks_test.go).
// Production callers must pass "" — there is no public EngineConfig field
// for this seam.
//
// Empty kind routes to the FS path so tests using DefaultEngineConfig (which
// leaves LedgerKind unset) continue working without bd on PATH. The CLI always
// resolves LedgerKind explicitly via parseLedgerKind before reaching here, so
// production code never enters the empty branch.
func ResolveLedgerDir(repoPath, runDir string, kind LedgerKind, override string) string {
	if override != "" {
		return override
	}
	if kind == LedgerBeads || kind == LedgerBDCLI {
		return filepath.Join(repoPath, ".beads")
	}
	return filepath.Join(runDir, "ledger")
}

// NewStore creates a Store of the given kind and returns the kind actually used.
// An empty kind defaults to LedgerBeads (BVV-DSP-16). Beads falls back to
// LedgerFS only when the caller did not explicitly request beads (i.e., when
// the input kind was empty); explicit `--ledger beads` is strict — a beads
// failure surfaces as an error so a misconfigured operator does not silently
// write FS-store JSON into a directory bd manages.
//
// The returned LedgerKind tells the caller which backend is active, so silent
// fallback (when permitted) is detectable.
func NewStore(kind LedgerKind, dir string) (Store, LedgerKind, error) {
	originalKind := kind
	if kind == "" {
		kind = LedgerBeads
	}
	fallback := originalKind == "" && kind == LedgerBeads
	ctor, ok := storeRegistry[kind]
	if !ok {
		return nil, "", fmt.Errorf("unknown ledger kind %q (available: beads, bd-cli, fs)", kind)
	}
	store, err := ctor(dir)
	if err != nil && fallback {
		beadsFallbackOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: beads store unavailable (%v), falling back to filesystem store\n", err)
		})
		beadsErr := err
		fsStore, fsErr := storeRegistry[LedgerFS](dir)
		if fsErr != nil {
			return nil, "", fmt.Errorf("beads unavailable (%v), fs fallback also failed: %w", beadsErr, fsErr)
		}
		return fsStore, LedgerFS, nil
	}
	return store, kind, err
}

// BeadsCLIAvailable reports whether the `bd` binary is on PATH. Used by the
// CLI's BuildEngineConfig precondition so a missing bd surfaces a clear error
// before the lifecycle lock is acquired.
func BeadsCLIAvailable() bool {
	_, err := exec.LookPath("bd")
	return err == nil
}

// EnsureBeadsInitialised creates <repoPath>/.beads/ via `bd init` if missing.
// Returns true when init was actually invoked (caller may want to warn the
// operator that wonka mutated the working tree). Returns ErrBeadsCLIMissing
// if `bd` is not on PATH and init is needed.
//
// The init invocation passes --stealth so bd does not install git hooks in
// the operator's repository — wonka's contract is "shared ledger," not
// "share Charlie's full bd workflow." Operators who want bd hooks active can
// run `bd init` manually with their own flags before running wonka.
func EnsureBeadsInitialised(repoPath string) (bool, error) {
	if repoPath == "" {
		return false, fmt.Errorf("ensure beads: empty repo path")
	}
	beadsDir := filepath.Join(repoPath, ".beads")
	// Stat returns nil for both files and directories; reject non-directories
	// so a stray file at .beads doesn't masquerade as an initialised store.
	if info, err := os.Stat(beadsDir); err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("ensure beads: %s exists but is not a directory", beadsDir)
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat beads dir %s: %w", beadsDir, err)
	}
	if !BeadsCLIAvailable() {
		return false, ErrBeadsCLIMissing
	}
	// 30s is generous: a fresh `bd init --stealth` is sub-second on dev
	// hardware. The cap is here to make a hung init (e.g. waiting on a
	// network FS lock) surface as an error instead of blocking `wonka run`
	// indefinitely before the lifecycle lock is acquired.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", "init", "--stealth", "--non-interactive", "--quiet") //nolint:gosec // args are programmer-controlled
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Concurrent multi-branch init race: two `wonka run --branch …`
		// invocations from the same repo can both stat-miss above and race
		// to bd init. The loser sees bd's "Aborting" exit-1; re-stat after
		// the failure — if .beads/ now exists as a directory, the winner
		// already created it and our work is done.
		if info, statErr := os.Stat(beadsDir); statErr == nil && info.IsDir() {
			return false, nil
		}
		return false, fmt.Errorf("bd init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}
