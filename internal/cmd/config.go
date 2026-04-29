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
	RunDir   string // empty => default <RepoPath>/.wonka/<sanitized-branch>/
	RepoPath string // empty => os.Getwd()

	// Lifecycle-only flags (run, resume).
	AgentPreset     string
	Workers         int
	GapTolerance    int
	MaxRetries      int
	MaxHandoffs     int
	BaseTimeout     time.Duration
	NoValidateGraph bool // when true, disables BVV-TG-07..10 post-planner validation

	// Run-only positional argument. Path to a work-package directory containing
	// functional-spec.md and vv-spec.md. When set, the CLI seeds a deterministic
	// plan-<branch> planner task (encapsulating the prior `bd create` step) and
	// hashes the spec files into a work-order-hash label so re-runs detect
	// content changes and reopen the planner for idempotent reconciliation.
	// Empty for resume/status; resume reads the work-order from the existing
	// task body, status doesn't need it.
	WorkOrder string

	// Observability — OBS-04. Empty OTelEndpoint means no-op telemetry; no
	// network connection is attempted and no data leaves the process.
	OTelEndpoint string // host:port of OTLP receiver (e.g. "localhost:14317")
	OTelProtocol string // "grpc" (default) or "http"
	OTelInsecure bool   // when true, skip TLS for OTLP transport (default: false; must be explicit for loopback dev)
}

// Default values for CLI flags — chosen to match the BVV spec's reference
// defaults for gap tolerance, retry/handoff budgets, and session timeout.
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

// Role-to-instruction-file mapping. Tasks labeled role:gate are not
// dispatched by the CLI today; they escalate via BVV-DSP-03a until a gate
// role is registered here.
var roleInstructionFiles = map[string]string{
	orch.RoleBuilder:  "OOMPA.md",
	orch.RoleVerifier: "LOOMPA.md",
	orch.RolePlanner:  "CHARLIE.md",
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
	// Reject two classes of unusable branch names:
	//   - "." and ".." — orch's lock-path sanitizer flattens both to "default",
	//     which would silently merge two typo'd --branch values into one lifecycle
	//     lock. Surface the invalid name instead.
	//   - NUL bytes — not filesystem-safe; the OS rejects them downstream, but
	//     erroring here yields a clearer message than the eventual syscall error.
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

	runDir := resolveRunDir(repoPath, branch, flags.RunDir)

	agentDir := flags.AgentDir
	if agentDir == "" {
		agentDir = defaultAgentDir
	}
	agentDir = resolveUnderRepo(repoPath, agentDir)
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

	// Environment precondition: any beads-shared kind (LedgerBeads or
	// LedgerBDCLI) needs `bd` on PATH — both auto-init <repo>/.beads/ via
	// `bd init`, and BDCLIStore additionally shells out for every operation.
	// Checked after structural flag validation so a user passing --workers=0
	// sees the workers error first; environment errors come last.
	if (ledger == orch.LedgerBeads || ledger == orch.LedgerBDCLI) && !orch.BeadsCLIAvailable() {
		return orch.EngineConfig{}, nil, fmt.Errorf("--ledger=%s requires the bd CLI on PATH: %w", ledger, orch.ErrBeadsCLIMissing)
	}

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
		// BVV-TG-07..10: default-on at Level 2. --no-validate-graph flips
		// this off for Level 1 compatibility against pre-populated ledgers.
		ValidateGraph: !flags.NoValidateGraph,
	}

	cfg := orch.DefaultEngineConfig(lifecycle, runDir, repoPath)
	cfg.MaxWorkers = flags.Workers
	cfg.LedgerKind = ledger
	return cfg, warnings, nil
}

// ResolveWorkOrder converts a CLI-supplied work-order path to an absolute path
// under repoPath, then verifies the directory and the two required spec files
// (functional-spec.md, vv-spec.md) exist and are non-empty. Per the design,
// technical specs live in the target repo's CLAUDE.md — only the WHAT
// (functional) and PROOF (vv) belong in a per-feature work order.
//
// Kept separate from BuildEngineConfig so BuildEngineConfig's signature stays
// orch-agnostic — orch must not learn what a "work order" is.
func ResolveWorkOrder(repoPath, raw string) (string, error) {
	abs, err := filepath.Abs(resolveUnderRepo(repoPath, raw))
	if err != nil {
		return "", fmt.Errorf("resolve work-order path %q: %w", raw, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("work-order %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("work-order %q is not a directory", abs)
	}
	for _, name := range WorkOrderRequiredFiles {
		st, err := os.Stat(filepath.Join(abs, name))
		if err != nil {
			return "", fmt.Errorf("work-order missing %s: %w", name, err)
		}
		// Reject non-regular entries (most commonly a directory named like the
		// spec, e.g. functional-spec.md/). Without this guard, validation passes
		// — directory FileInfo.Size() is non-zero on most filesystems — and the
		// failure surfaces later inside hashWorkOrder under the lifecycle lock,
		// turning a config error into a runtime error after side effects.
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("work-order %s is not a regular file", name)
		}
		if st.Size() == 0 {
			return "", fmt.Errorf("work-order %s is empty", name)
		}
	}
	return abs, nil
}

// resolveUnderRepo joins a relative path against repoPath, matching the
// project convention that operator-supplied paths (--agent-dir, work-order
// positional) resolve under --repo so `wonka run --repo /elsewhere foo`
// reads /elsewhere/foo, not <cwd>/foo. Absolute paths pass through unchanged.
func resolveUnderRepo(repoPath, raw string) string {
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(repoPath, raw)
}

// WorkOrderRequiredFiles are the two spec files Charlie reads during ORIENT.
// Order matters for hashing in seed.go: changing this order invalidates every
// previously-stored work-order-hash label, forcing spurious replan on the next
// `wonka run`. Treat as append-only — and document any addition as a hash
// scheme bump in the changelog.
var WorkOrderRequiredFiles = []string{
	"functional-spec.md",
	"vv-spec.md",
}

// parseLedgerKind validates --ledger input and returns the orch LedgerKind
// or an error listing valid values. Empty input defaults to beads.
func parseLedgerKind(raw string) (orch.LedgerKind, error) {
	switch strings.TrimSpace(raw) {
	case "", string(orch.LedgerBeads):
		return orch.LedgerBeads, nil
	case string(orch.LedgerFS):
		return orch.LedgerFS, nil
	case string(orch.LedgerBDCLI):
		return orch.LedgerBDCLI, nil
	default:
		return "", fmt.Errorf("unknown ledger %q (available: beads, bd-cli, fs)", raw)
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
// Tasks whose role label has no entry in the returned registry get escalated
// per BVV-DSP-03a (the dispatcher's "no handler for role" path).
//
//   - All three missing → error (operator clearly hasn't set up the agents
//     directory; fail fast before any tmux/lock side effects).
//   - Some missing (ENOENT) → per-role warnings, partial registry returned so
//     runs with subset workloads (e.g. builders only) keep working.
//   - Stat fails for reasons other than ENOENT (EACCES, ENOTDIR, ELOOP) →
//     hard error. Flattening these to "missing" would hide fixable operator
//     problems (wrong mode, symlink loop) behind a benign-sounding warning.
//   - All present → silent.
//
// The preset is shared by every role; MaxTurns stays at zero (preset default)
// until a flag is added later.
func buildRoleRegistry(agentDir string, preset *orch.Preset) (map[string]orch.RoleConfig, []string, error) {
	roles := make(map[string]orch.RoleConfig, len(roleInstructionFiles))
	var warnings []string

	// Sort role names for deterministic warning order (stable tests).
	names := make([]string, 0, len(roleInstructionFiles))
	for r := range roleInstructionFiles {
		names = append(names, r)
	}
	sort.Strings(names)

	for _, role := range names {
		basename := roleInstructionFiles[role]
		path := filepath.Join(agentDir, basename)
		_, err := os.Stat(path)
		switch {
		case err == nil:
			roles[role] = orch.RoleConfig{
				InstructionFile: path,
				Preset:          preset,
			}
		case os.IsNotExist(err):
			warnings = append(warnings, fmt.Sprintf(
				"role %q instruction file %s missing; tasks with role=%s will escalate per BVV-DSP-03a until the file exists",
				role, path, role))
		default:
			return nil, nil, fmt.Errorf("stat role %q instruction file %s: %w", role, path, err)
		}
	}

	if len(roles) == 0 {
		return nil, nil, fmt.Errorf(
			"agent directory %q contains none of %v; create them or pass --agent-dir",
			agentDir, instructionFileBasenames())
	}
	return roles, warnings, nil
}

// resolveRunDir returns the user-specified --run-dir when set, else the
// default <repo>/.wonka/<sanitized-branch>. Shared by BuildEngineConfig
// and showStatus so the two derive the same path for a given branch.
func resolveRunDir(repoPath, branch, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return filepath.Join(repoPath, ".wonka", sanitizeBranch(branch))
}

// sanitizeBranch duplicates orch's internal sanitizeBranchForLock logic so
// RunDir and the engine-derived lock path agree on the fragment shape.
// TODO(orch-seam): export sanitizeBranchForLock from orch (or better, an
// OpenLedgerForRun constructor that owns path derivation end-to-end) so
// the two copies can't drift. If orch's rule changes and this doesn't,
// the CLI's run-dir and the engine's lock path stop agreeing.
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
	cmd.Flags().BoolVar(&flags.NoValidateGraph, "no-validate-graph", false, "disable post-planner task-graph validation (BVV-TG-07..10); required for Level 1 operation against pre-populated ledgers")
	cmd.Flags().StringVar(&flags.OTelEndpoint, "otel-endpoint", "", "OTLP receiver endpoint (host:port). Empty = no telemetry emitted. Example: localhost:14317")
	cmd.Flags().StringVar(&flags.OTelProtocol, "otel-protocol", "grpc", "OTLP transport: grpc or http. Only consulted when --otel-endpoint is set")
	cmd.Flags().BoolVar(&flags.OTelInsecure, "otel-insecure", false, "skip TLS on the OTLP connection. Only allowed for loopback endpoints (localhost / 127.0.0.1 / ::1); refused against any non-loopback endpoint. Required for the local docker-compose stack (localhost:14317)")
}
