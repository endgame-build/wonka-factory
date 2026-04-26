package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAgentDir creates a temp dir populated with the three instruction files.
// Missing files can be suppressed by removing them before BuildEngineConfig.
func seedAgentDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"OOMPA.md", "LOOMPA.md", "CHARLIE.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("# placeholder\n"), 0o644))
	}
	return dir
}

// validFlags returns a CLIFlags with all fields populated to defaults and
// pointed at a fully-seeded agent dir. Tests mutate a single field to probe
// validation paths.
func validFlags(t *testing.T) CLIFlags {
	t.Helper()
	return CLIFlags{
		Branch:       "feat-x",
		Ledger:       "fs",
		AgentDir:     seedAgentDir(t),
		RunDir:       t.TempDir(),
		RepoPath:     t.TempDir(),
		AgentPreset:  defaultAgentPreset,
		Workers:      defaultWorkers,
		GapTolerance: defaultGapTolerance,
		MaxRetries:   defaultMaxRetries,
		MaxHandoffs:  defaultMaxHandoffs,
		BaseTimeout:  defaultBaseTimeout,
	}
}

// TestBuildEngineConfig_Defaults confirms the spec-defined default values
// propagate into EngineConfig / LifecycleConfig. Guards against silent
// regressions where a constant rename breaks the CLI's contract.
func TestBuildEngineConfig_Defaults(t *testing.T) {
	cfg, warnings, err := BuildEngineConfig(validFlags(t))
	require.NoError(t, err)
	assert.Empty(t, warnings, "all instruction files present, no warnings expected")

	assert.Equal(t, defaultWorkers, cfg.MaxWorkers)
	require.NotNil(t, cfg.Lifecycle)
	assert.Equal(t, "feat-x", cfg.Lifecycle.Branch)
	assert.Equal(t, defaultGapTolerance, cfg.Lifecycle.GapTolerance)
	assert.Equal(t, defaultMaxRetries, cfg.Lifecycle.MaxRetries)
	assert.Equal(t, defaultMaxHandoffs, cfg.Lifecycle.MaxHandoffs)
	assert.Equal(t, defaultBaseTimeout, cfg.Lifecycle.BaseTimeout)
	assert.Equal(t, defaultLockStaleness, cfg.Lifecycle.Lock.StalenessThreshold)
	assert.Equal(t, orch.LedgerFS, cfg.LedgerKind)
}

// TestBuildEngineConfig_BranchSanitization verifies the raw branch label is
// preserved in Lifecycle.Branch (so label filters keep finding tasks created
// with the slashed name) while the RunDir uses the sanitized fragment. Pins
// the agreement between this CLI's sanitizeBranch and orch's internal
// sanitizeBranchForLock — if they drift, the lock file and the run dir
// target different paths.
func TestBuildEngineConfig_BranchSanitization(t *testing.T) {
	flags := validFlags(t)
	flags.Branch = "feat/x"
	flags.RunDir = "" // force default-derivation from repo + branch

	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.Equal(t, "feat/x", cfg.Lifecycle.Branch, "raw branch must survive for label filtering")
	assert.Contains(t, cfg.RunDir, "feat-x", "RunDir must use sanitized fragment")
	assert.NotContains(t, cfg.RunDir, "feat/x", "RunDir must not contain the unsanitized slash form")
}

// TestBuildEngineConfig_RejectsEmptyBranch ensures the required-branch check
// fires before any side effects (temp dir stat, preset lookup, etc.).
func TestBuildEngineConfig_RejectsEmptyBranch(t *testing.T) {
	flags := validFlags(t)
	flags.Branch = "  "
	_, _, err := BuildEngineConfig(flags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch")
}

// TestBuildEngineConfig_RejectsDotBranch covers the "." / ".." / NUL guards
// that prevent a sanitized RunDir from collapsing into a parent traversal.
func TestBuildEngineConfig_RejectsDotBranch(t *testing.T) {
	for _, b := range []string{".", "..", "bad\x00branch"} {
		flags := validFlags(t)
		flags.Branch = b
		_, _, err := BuildEngineConfig(flags)
		require.Error(t, err, "branch %q should be rejected", b)
	}
}

// TestBuildEngineConfig_UnknownAgent proves preset validation happens inside
// BuildEngineConfig, not inside orch — catches typos before engine init.
func TestBuildEngineConfig_UnknownAgent(t *testing.T) {
	flags := validFlags(t)
	flags.AgentPreset = "gpt4"
	_, _, err := BuildEngineConfig(flags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gpt4")
	assert.Contains(t, err.Error(), "claude")
}

// TestBuildEngineConfig_UnknownLedger surfaces the parseLedgerKind error path.
func TestBuildEngineConfig_UnknownLedger(t *testing.T) {
	flags := validFlags(t)
	flags.Ledger = "dolt"
	_, _, err := BuildEngineConfig(flags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dolt")
	assert.Contains(t, err.Error(), "beads")
}

// TestBuildEngineConfig_InvalidWorkers guards the >= 1 constraint that would
// otherwise produce a runtime divide-by-zero or zero-goroutine deadlock
// inside the dispatcher.
func TestBuildEngineConfig_InvalidWorkers(t *testing.T) {
	flags := validFlags(t)
	flags.Workers = 0
	_, _, err := BuildEngineConfig(flags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workers")
}

// TestBuildEngineConfig_InvalidTimeout guards against a zero or negative
// timeout, which would make ScaledTimeout return an immediately-fired timer.
// Split into subtests so a zero-duration regression doesn't mask a separate
// negative-duration regression.
func TestBuildEngineConfig_InvalidTimeout(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
	}{
		{"zero", 0},
		{"negative", -1 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := validFlags(t)
			flags.BaseTimeout = tc.timeout
			_, _, err := BuildEngineConfig(flags)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "timeout")
		})
	}
}

// TestBuildEngineConfig_NegativeBudgets covers the three ">= 0" guards in a
// single test — all three share the same error wording.
func TestBuildEngineConfig_NegativeBudgets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CLIFlags)
	}{
		{"gap", func(f *CLIFlags) { f.GapTolerance = -1 }},
		{"retries", func(f *CLIFlags) { f.MaxRetries = -1 }},
		{"handoffs", func(f *CLIFlags) { f.MaxHandoffs = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := validFlags(t)
			tc.mutate(&flags)
			_, _, err := BuildEngineConfig(flags)
			require.Error(t, err)
		})
	}
}

// TestBuildEngineConfig_RolesPopulated proves all three roles land in the
// Lifecycle.Roles map when their files exist, with the correct InstructionFile
// paths — the dispatcher reads this map to route role labels to agents.
func TestBuildEngineConfig_RolesPopulated(t *testing.T) {
	flags := validFlags(t)
	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	require.NotNil(t, cfg.Lifecycle)

	require.Len(t, cfg.Lifecycle.Roles, 3)
	assert.Equal(t, filepath.Join(flags.AgentDir, "OOMPA.md"), cfg.Lifecycle.Roles["builder"].InstructionFile)
	assert.Equal(t, filepath.Join(flags.AgentDir, "LOOMPA.md"), cfg.Lifecycle.Roles["verifier"].InstructionFile)
	assert.Equal(t, filepath.Join(flags.AgentDir, "CHARLIE.md"), cfg.Lifecycle.Roles["planner"].InstructionFile)

	for role, rc := range cfg.Lifecycle.Roles {
		assert.NotNil(t, rc.Preset, "role %s must have a non-nil preset pointer", role)
	}
}

// TestBuildEngineConfig_MissingInstructionFile_Warns proves the partial-set
// path: one file missing produces a warning and the role is absent from the
// registry, but the other roles are still usable.
func TestBuildEngineConfig_MissingInstructionFile_Warns(t *testing.T) {
	flags := validFlags(t)
	require.NoError(t, os.Remove(filepath.Join(flags.AgentDir, "LOOMPA.md")))

	cfg, warnings, err := BuildEngineConfig(flags)
	require.NoError(t, err, "missing verifier file must not block the run")

	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "verifier")
	assert.Contains(t, warnings[0], "LOOMPA.md")
	assert.Contains(t, warnings[0], "BVV-DSP-03a")

	_, hasVerifier := cfg.Lifecycle.Roles["verifier"]
	assert.False(t, hasVerifier, "missing role must not be in the registry")
	assert.Len(t, cfg.Lifecycle.Roles, 2)
}

// TestBuildEngineConfig_AllInstructionsMissing_Errors is the fail-fast path:
// zero role files → halt before any side effects, clearer than letting the
// dispatcher escalate every task to BVV-DSP-03a.
func TestBuildEngineConfig_AllInstructionsMissing_Errors(t *testing.T) {
	flags := validFlags(t)
	for _, name := range []string{"OOMPA.md", "LOOMPA.md", "CHARLIE.md"} {
		require.NoError(t, os.Remove(filepath.Join(flags.AgentDir, name)))
	}

	_, _, err := BuildEngineConfig(flags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none of")
	assert.Contains(t, err.Error(), "OOMPA.md")
}

// TestBuildEngineConfig_MissingAgentDir halts before role-checking with a
// clearer error ("agent directory not found" beats three synthesized
// per-role "file not found" warnings).
func TestBuildEngineConfig_MissingAgentDir(t *testing.T) {
	flags := validFlags(t)
	flags.AgentDir = filepath.Join(t.TempDir(), "does-not-exist")
	_, _, err := BuildEngineConfig(flags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent directory")
}

// TestBuildEngineConfig_DefaultRunDir exercises the RunDir-derivation path:
// when --run-dir is empty, RunDir = <repo>/.wonka/<sanitized-branch>.
func TestBuildEngineConfig_DefaultRunDir(t *testing.T) {
	flags := validFlags(t)
	flags.RunDir = ""
	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(flags.RepoPath, ".wonka", "feat-x"), cfg.RunDir)
}

// TestBuildEngineConfig_RelativeAgentDirResolvesUnderRepo pins the resolution
// rule: `--agent-dir agents` with `--repo /elsewhere` must stat
// /elsewhere/agents, not <cwd>/agents. Without this rule, operators running
// wonka outside the repo root see confusing "agent directory not found"
// errors despite the files existing where they expect.
func TestBuildEngineConfig_RelativeAgentDirResolvesUnderRepo(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "agents")
	require.NoError(t, os.Mkdir(agents, 0o755))
	for _, name := range []string{"OOMPA.md", "LOOMPA.md", "CHARLIE.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(agents, name), []byte("# x\n"), 0o644))
	}

	flags := validFlags(t)
	flags.RepoPath = repo
	flags.AgentDir = "agents" // relative — must resolve under RepoPath

	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err, "relative agent-dir must resolve under --repo")

	require.NotNil(t, cfg.Lifecycle)
	assert.Equal(t, filepath.Join(agents, "OOMPA.md"), cfg.Lifecycle.Roles["builder"].InstructionFile)
}

// TestResolveWorkOrder_HappyPath confirms a well-formed work-order resolves to
// an absolute path. Production callers (runLifecycle) pass the result into
// SeedPlannerTask, which rejects relative paths — this test guards the
// contract from the other side.
func TestResolveWorkOrder_HappyPath(t *testing.T) {
	repo := t.TempDir()
	wo := filepath.Join(repo, "wp")
	require.NoError(t, os.Mkdir(wo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wo, "functional-spec.md"), []byte("# CAP-1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wo, "vv-spec.md"), []byte("# V-1\n"), 0o644))

	abs, err := ResolveWorkOrder(repo, "wp")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(abs))
	assert.Equal(t, wo, abs, "relative path must resolve under repo")
}

// TestResolveWorkOrder_AbsolutePassthrough verifies an absolute work-order
// path bypasses the repo-rooted join. This matters when operators script
// against work packages stored outside the target repo (a shared "specs"
// repo, for instance).
func TestResolveWorkOrder_AbsolutePassthrough(t *testing.T) {
	repo := t.TempDir()
	external := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(external, "functional-spec.md"), []byte("# x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(external, "vv-spec.md"), []byte("# v\n"), 0o644))

	abs, err := ResolveWorkOrder(repo, external)
	require.NoError(t, err)
	assert.Equal(t, external, abs)
}

// TestResolveWorkOrder_FailureModes is a single function rather than N tests
// because each case is one-line and they share the same fail-fast contract:
// any failure returns an error before any side effect, so runLifecycle can
// die() with exitConfigError without touching the lifecycle lock.
func TestResolveWorkOrder_FailureModes(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		repo := t.TempDir()
		_, err := ResolveWorkOrder(repo, "nope")
		require.Error(t, err)
	})

	t.Run("path is a file not a directory", func(t *testing.T) {
		repo := t.TempDir()
		f := filepath.Join(repo, "wp")
		require.NoError(t, os.WriteFile(f, []byte("not a dir"), 0o644))
		_, err := ResolveWorkOrder(repo, "wp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})

	t.Run("missing functional-spec", func(t *testing.T) {
		repo := t.TempDir()
		wo := filepath.Join(repo, "wp")
		require.NoError(t, os.Mkdir(wo, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wo, "vv-spec.md"), []byte("# v\n"), 0o644))
		_, err := ResolveWorkOrder(repo, "wp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "functional-spec.md")
	})

	t.Run("missing vv-spec", func(t *testing.T) {
		repo := t.TempDir()
		wo := filepath.Join(repo, "wp")
		require.NoError(t, os.Mkdir(wo, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wo, "functional-spec.md"), []byte("# f\n"), 0o644))
		_, err := ResolveWorkOrder(repo, "wp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vv-spec.md")
	})

	t.Run("empty functional-spec", func(t *testing.T) {
		repo := t.TempDir()
		wo := filepath.Join(repo, "wp")
		require.NoError(t, os.Mkdir(wo, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wo, "functional-spec.md"), []byte{}, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(wo, "vv-spec.md"), []byte("# v\n"), 0o644))
		_, err := ResolveWorkOrder(repo, "wp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})
}

// TestBuildEngineConfig_ProductionAgents exercises BuildEngineConfig against
// the real agents/ directory rather than a synthetic fixture. Closes the
// drift gap between roleInstructionFiles (internal/cmd/config.go) and the
// files actually checked into agents/ — a rename that updates only one side
// would otherwise produce silent BVV-DSP-03a escalations at runtime with no
// failing test.
func TestBuildEngineConfig_ProductionAgents(t *testing.T) {
	realAgents, err := filepath.Abs(filepath.Join("..", "..", "agents"))
	require.NoError(t, err)

	flags := validFlags(t)
	flags.AgentDir = realAgents

	cfg, warnings, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.Empty(t, warnings, "every role in config.roleInstructionFiles must resolve to a real file in agents/")

	require.NotNil(t, cfg.Lifecycle)
	for role, basename := range roleInstructionFiles {
		rc, ok := cfg.Lifecycle.Roles[role]
		require.Truef(t, ok, "role %q missing from registry", role)
		want := filepath.Join(realAgents, basename)
		assert.Equal(t, want, rc.InstructionFile,
			"role %q → %s drift between roleInstructionFiles and agents/", role, basename)
		_, statErr := os.Stat(rc.InstructionFile)
		assert.NoError(t, statErr, "registered instruction file must exist on disk")
	}
}
