package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/endgame/wonka-factory/orch"
	"github.com/spf13/cobra"
)

// CLIFlags holds every flag value the CLI might pass into BuildEngineConfig.
// Keeping this cobra-independent means config_test.go can exercise the full
// config-assembly logic without spinning up cobra commands — the cobra
// layer is a thin flag-to-struct translator.
type CLIFlags struct {
	// Root persistent flags.
	Branch   string
	Ledger   string // "beads" or "fs"
	AgentDir string
	RunDir   string // empty => default ./.wonka/<sanitized-branch>/
	RepoPath string // empty => os.Getwd()

	// Lifecycle-only flags (run, resume).
	AgentPreset  string
	Workers      int
	GapTolerance int
	MaxRetries   int
	MaxHandoffs  int
	BaseTimeout  time.Duration
}

// Default values. Match BVV_IMPLEMENTATION_PLAN.md §7.2 flag defaults.
const (
	defaultLedger       = "beads"
	defaultAgentDir     = "agents"
	defaultAgentPreset  = "claude"
	defaultWorkers      = 4
	defaultGapTolerance = 3
	defaultMaxRetries   = 2
	defaultMaxHandoffs  = 5
	defaultBaseTimeout  = 30 * time.Minute

	defaultLockStaleness  = 2 * time.Minute
	defaultLockRetryCount = 3
	defaultLockRetryDelay = 2 * time.Second
)

// Role-to-instruction-file mapping. Fixed set for Phase 7; the "gate" role
// is deferred to Phase 8 (will be a shell wrapper or a subcommand).
var roleInstructionFiles = map[string]string{
	"builder":  "OOMPA.md",
	"verifier": "LOOMPA.md",
	"planner":  "CHARLIE.md",
}

// BuildEngineConfig assembles an orch.EngineConfig from CLI flag values.
// Returns the assembled config plus a slice of human-readable warnings that
// the caller should emit to stderr (missing-but-tolerated instruction files,
// ledger fallback hints, etc.). Returns an error for anything that should
// halt before engine init: unknown preset/ledger, missing agent directory,
// all instruction files absent, unsanitizable branch.
func BuildEngineConfig(flags CLIFlags) (orch.EngineConfig, []string, error) {
	var warnings []string

	branch := strings.TrimSpace(flags.Branch)
	if branch == "" {
		return orch.EngineConfig{}, nil, errors.New("branch is required")
	}
	// Match orch's internal sanitizer for nonsensical branch values.
	if branch == "." || branch == ".." || strings.ContainsRune(branch, 0) {
		return orch.EngineConfig{}, nil, fmt.Errorf("invalid branch name %q", branch)
	}

	ledger, err := parseLedgerKind(flags.Ledger)
	if err != nil {
		return orch.EngineConfig{}, nil, err
	}

	preset, err := LookupPreset(flags.AgentPreset)
	if err != nil {
		return orch.EngineConfig{}, nil, err
	}

	if flags.Workers < 1 {
		return orch.EngineConfig{}, nil, fmt.Errorf("workers must be >= 1, got %d", flags.Workers)
	}
	if flags.GapTolerance < 0 || flags.MaxRetries < 0 || flags.MaxHandoffs < 0 {
		return orch.EngineConfig{}, nil, errors.New("gap-tolerance, retry-count, and handoff-limit must be >= 0")
	}
	if flags.BaseTimeout <= 0 {
		return orch.EngineConfig{}, nil, fmt.Errorf("timeout must be > 0, got %s", flags.BaseTimeout)
	}

	repoPath, err := resolveRepoPath(flags.RepoPath)
	if err != nil {
		return orch.EngineConfig{}, nil, err
	}

	runDir := flags.RunDir
	if runDir == "" {
		runDir = filepath.Join(repoPath, ".wonka", sanitizeBranch(branch))
	}

	agentDir := flags.AgentDir
	if agentDir == "" {
		agentDir = defaultAgentDir
	}
	// Relative --agent-dir resolves under --repo, matching --run-dir's
	// default (<repo>/.wonka/…). Otherwise `wonka run --repo /elsewhere`
	// with the default "agents" would stat ./agents under the CLI's cwd
	// instead of under the target repo.
	if !filepath.IsAbs(agentDir) {
		agentDir = filepath.Join(repoPath, agentDir)
	}
	if info, err := os.Stat(agentDir); err != nil {
		return orch.EngineConfig{}, nil, fmt.Errorf("agent directory %q not found: %w", agentDir, err)
	} else if !info.IsDir() {
		return orch.EngineConfig{}, nil, fmt.Errorf("agent-dir %q is not a directory", agentDir)
	}

	roles, roleWarnings, err := buildRoleRegistry(agentDir, preset)
	if err != nil {
		return orch.EngineConfig{}, nil, err
	}
	warnings = append(warnings, roleWarnings...)

	lifecycle := &orch.LifecycleConfig{
		Branch:       branch,
		GapTolerance: flags.GapTolerance,
		MaxRetries:   flags.MaxRetries,
		MaxHandoffs:  flags.MaxHandoffs,
		BaseTimeout:  flags.BaseTimeout,
		Lock: orch.LockConfig{
			StalenessThreshold: defaultLockStaleness,
			RetryCount:         defaultLockRetryCount,
			RetryDelay:         defaultLockRetryDelay,
		},
		Roles: roles,
	}

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, repoPath)
	cfg.MaxWorkers = flags.Workers
	cfg.LedgerKind = ledger
	return cfg, warnings, nil
}

// parseLedgerKind validates --ledger input and returns the orch LedgerKind
// or an error listing valid values. Empty input defaults to beads.
func parseLedgerKind(raw string) (orch.LedgerKind, error) {
	switch strings.TrimSpace(raw) {
	case "", string(orch.LedgerBeads):
		return orch.LedgerBeads, nil
	case string(orch.LedgerFS):
		return orch.LedgerFS, nil
	default:
		return "", fmt.Errorf("unknown ledger %q (available: beads, fs)", raw)
	}
}

// resolveRepoPath returns the absolute repo path. Empty input falls back to
// os.Getwd(). Errors if getwd fails (unlikely but worth surfacing clearly).
func resolveRepoPath(raw string) (string, error) {
	if raw != "" {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", fmt.Errorf("resolve --repo: %w", err)
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return cwd, nil
}

// buildRoleRegistry stat-checks each role's instruction file under agentDir.
// - All three missing → error (operator clearly hasn't run Phase 8; fail fast
//   before any tmux/lock side effects).
// - Some missing → per-role warnings, partial registry returned so runs with
//   subset workloads (e.g. builders only) keep working during Phase 8 dev.
// - All present → silent.
// The preset is shared by every role; MaxTurns stays at zero (preset default)
// until a flag is added in a later phase.
func buildRoleRegistry(agentDir string, preset *orch.Preset) (map[string]orch.RoleConfig, []string, error) {
	roles := make(map[string]orch.RoleConfig, len(roleInstructionFiles))
	var warnings []string
	presentCount := 0

	// Sort role names for deterministic warning order (stable tests).
	names := make([]string, 0, len(roleInstructionFiles))
	for r := range roleInstructionFiles {
		names = append(names, r)
	}
	sort.Strings(names)

	for _, role := range names {
		basename := roleInstructionFiles[role]
		path := filepath.Join(agentDir, basename)
		if _, err := os.Stat(path); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"role %q instruction file %s missing; tasks with role=%s will escalate per BVV-DSP-03a until the file exists",
				role, path, role))
			continue
		}
		roles[role] = orch.RoleConfig{
			InstructionFile: path,
			Preset:          preset,
		}
		presentCount++
	}

	if presentCount == 0 {
		return nil, nil, fmt.Errorf(
			"agent directory %q contains none of %v; create them or pass --agent-dir",
			agentDir, instructionFileBasenames())
	}
	return roles, warnings, nil
}

// sanitizeBranch duplicates orch's internal sanitizeBranchForLock logic so
// RunDir and the engine-derived lock path agree on the fragment shape.
// TODO(phase-8): orch should export this helper (or an OpenLedgerForRun
// constructor) so the CLI doesn't hardcode the sanitization rule.
func sanitizeBranch(branch string) string {
	safe := strings.NewReplacer("/", "-", "\\", "-").Replace(strings.TrimSpace(branch))
	if safe == "" || safe == "." || safe == ".." {
		return "default"
	}
	return safe
}

func instructionFileBasenames() []string {
	out := make([]string, 0, len(roleInstructionFiles))
	for _, v := range roleInstructionFiles {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// addLifecycleFlags attaches run/resume-only flags to a cobra command. Keeps
// wonka status from declaring (and then rejecting mis-typed) flags it never
// consumes.
func addLifecycleFlags(cmd *cobra.Command, flags *CLIFlags) {
	cmd.Flags().StringVar(&flags.AgentPreset, "agent", defaultAgentPreset, "agent preset name (see presets registry)")
	cmd.Flags().IntVar(&flags.Workers, "workers", defaultWorkers, "concurrent worker count (>= 1)")
	cmd.Flags().IntVar(&flags.GapTolerance, "gap-tolerance", defaultGapTolerance, "non-critical failures tolerated before abort (BVV-ERR-04)")
	cmd.Flags().IntVar(&flags.MaxRetries, "retry-count", defaultMaxRetries, "retries per task on exit-code-1 failure (BVV-ERR-01)")
	cmd.Flags().IntVar(&flags.MaxHandoffs, "handoff-limit", defaultMaxHandoffs, "session restarts per task on exit-code-3 handoff (BVV-L-04)")
	cmd.Flags().DurationVar(&flags.BaseTimeout, "timeout", defaultBaseTimeout, "base session timeout, scales with retry attempt (BVV-ERR-02a)")
}
