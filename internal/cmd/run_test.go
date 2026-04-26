package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireExitCode asserts err's chain contains *exitError with code want.
func requireExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var ex *exitError
	require.True(t, errors.As(err, &ex), "expected *exitError in chain, got %T: %v", err, err)
	assert.Equal(t, want, ex.code)
}

// seedRepoWithAgents creates a temp "repo" containing an agents dir seeded
// with all three instruction files — lets cobra tests run the full flag
// validation without triggering a real engine.
func seedRepoWithAgents(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	agents := filepath.Join(repo, "agents")
	require.NoError(t, os.Mkdir(agents, 0o755))
	for _, name := range []string{"OOMPA.md", "LOOMPA.md", "CHARLIE.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(agents, name), []byte("# placeholder\n"), 0o644))
	}
	return repo
}

// runCobra is the standard harness: fresh root, captured streams, returns
// the error and stderr contents for assertions. Tests never share a root
// command — flag state would leak across parallel cases.
func runCobra(t *testing.T, args ...string) (error, string) {
	t.Helper()
	var stderr bytes.Buffer
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(&stderr)
	err := root.Execute()
	return err, stderr.String()
}

// seedRepoWithWorkOrder extends seedRepoWithAgents by also dropping a valid
// work-package under the repo. Tests that get past argument parsing into
// runLifecycle need a real path to point at — without one, ResolveWorkOrder
// would fail and mask the validation we're actually testing.
func seedRepoWithWorkOrder(t *testing.T) (repo, woRel string) {
	t.Helper()
	repo = seedRepoWithAgents(t)
	woRel = "wp"
	wo := filepath.Join(repo, woRel)
	require.NoError(t, os.Mkdir(wo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wo, "functional-spec.md"), []byte("# CAP-1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wo, "vv-spec.md"), []byte("# V-1\n"), 0o644))
	return repo, woRel
}

// TestRunCmd_RequiresBranch verifies cobra's MarkPersistentFlagRequired
// fires before any lifecycle side effects — no tmux, no lock, no store.
// The error must name the missing flag so operators know what to add. Pass a
// dummy positional so we test the --branch missing path specifically rather
// than tripping ExactArgs first.
func TestRunCmd_RequiresBranch(t *testing.T) {
	err, stderr := runCobra(t, "run", "anything")
	require.Error(t, err)
	assert.Contains(t, stderr+err.Error(), "branch")
}

// TestRunCmd_RequiresWorkOrder pins the new positional contract: `wonka run`
// without a work-package argument must fail at cobra arg parsing, before any
// engine init or store open. Operators get a clear "accepts 1 arg(s)" message
// rather than a confusing config-validation failure deeper in the call.
func TestRunCmd_RequiresWorkOrder(t *testing.T) {
	repo := seedRepoWithAgents(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
	)
	require.Error(t, err)
	// cobra's ExactArgs(1) message phrasing: "accepts 1 arg(s), received 0".
	// Don't pin the exact text (cobra version drift); pin the shape — the
	// error must mention argument count so the operator knows what to fix.
	assert.Contains(t, stderr+err.Error(), "arg")
}

// TestRunCmd_WorkOrderValidationFailsFast proves ResolveWorkOrder runs before
// engine init: a non-existent work-package path errors with exitConfigError
// without touching the lock. If this regresses, operators with typo'd paths
// would acquire the lifecycle lock for a doomed run.
func TestRunCmd_WorkOrderValidationFailsFast(t *testing.T) {
	repo := seedRepoWithAgents(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--ledger", "fs",
		"work-packages/nope",
	)
	require.Error(t, err)
	requireExitCode(t, err, exitConfigError)
	assert.Contains(t, stderr, "work-order", "error must name the failing input")
}

// TestRunCmd_InvalidLedger exercises the unknown-ledger path through
// BuildEngineConfig. Provides a positional so ExactArgs passes — the test is
// about ledger validation, not argument count.
func TestRunCmd_InvalidLedger(t *testing.T) {
	repo, wo := seedRepoWithWorkOrder(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--ledger", "dolt",
		wo,
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "dolt")
	assert.Contains(t, stderr, "beads")
	requireExitCode(t, err, exitConfigError)
}

// TestRunCmd_InvalidWorkers proves the --workers >= 1 guard in
// BuildEngineConfig fires for explicit zero (cobra's IntVar happily accepts
// zero at parse time; the semantic check is ours).
func TestRunCmd_InvalidWorkers(t *testing.T) {
	repo, wo := seedRepoWithWorkOrder(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--workers", "0",
		wo,
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "workers")
}

// TestBuildEngineConfig_ValidateGraphDefault verifies BVV-TG-07..10 validation
// is ON by default — Level 2 conformance requires it.
func TestBuildEngineConfig_ValidateGraphDefault(t *testing.T) {
	repo := seedRepoWithAgents(t)
	flags := CLIFlags{
		Branch: "feat/x", Ledger: "fs", RepoPath: repo,
		AgentDir: filepath.Join(repo, "agents"), AgentPreset: defaultAgentPreset,
		Workers: defaultWorkers, GapTolerance: defaultGapTolerance,
		MaxRetries: defaultMaxRetries, MaxHandoffs: defaultMaxHandoffs,
		BaseTimeout: defaultBaseTimeout,
		// NoValidateGraph left at zero value (false) — default-on path.
	}
	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.True(t, cfg.Lifecycle.ValidateGraph, "default must enable graph validation (Level 2)")
}

// TestBuildEngineConfig_NoValidateGraph verifies --no-validate-graph plumbs
// through as ValidateGraph=false (Level 1 compatibility escape hatch).
func TestBuildEngineConfig_NoValidateGraph(t *testing.T) {
	repo := seedRepoWithAgents(t)
	flags := CLIFlags{
		Branch: "feat/x", Ledger: "fs", RepoPath: repo,
		AgentDir: filepath.Join(repo, "agents"), AgentPreset: defaultAgentPreset,
		Workers: defaultWorkers, GapTolerance: defaultGapTolerance,
		MaxRetries: defaultMaxRetries, MaxHandoffs: defaultMaxHandoffs,
		BaseTimeout:     defaultBaseTimeout,
		NoValidateGraph: true,
	}
	cfg, _, err := BuildEngineConfig(flags)
	require.NoError(t, err)
	assert.False(t, cfg.Lifecycle.ValidateGraph, "--no-validate-graph must disable validation")
}

// TestRunCmd_NoValidateGraphFlag exercises the cobra path end-to-end by
// parsing --no-validate-graph through a real root command. Exits with a
// non-zero code because we don't actually run the engine, but flag parsing
// must succeed (no "unknown flag" error).
func TestRunCmd_NoValidateGraphFlag(t *testing.T) {
	repo, wo := seedRepoWithWorkOrder(t)
	// Use an unrecognized ledger to short-circuit before engine init; we only
	// care that --no-validate-graph parses cleanly.
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--no-validate-graph",
		"--ledger", "dolt", // triggers exitConfigError — flag parsing happened first
		wo,
	)
	require.Error(t, err)
	assert.NotContains(t, stderr, "unknown flag", "--no-validate-graph must parse")
}

// TestOBS04_BuildTelemetry_EmptyEndpointReturnsNil verifies the no-op path: with
// no --otel-endpoint, BuildTelemetry returns (nil, noop-shutdown, nil) so
// the engine attaches a nil *Telemetry and the whole observability surface
// stays dormant. This is the default posture — running wonka without an
// OTel collector MUST NOT fail or block.
func TestOBS04_BuildTelemetry_EmptyEndpointReturnsNil(t *testing.T) {
	telem, shutdown, err := BuildTelemetry(CLIFlags{})
	require.NoError(t, err)
	assert.Nil(t, telem, "no endpoint => nil telemetry")
	require.NotNil(t, shutdown, "shutdown func must always be callable")
	// noop shutdown must not panic or error even when telemetry is disabled.
	assert.NoError(t, shutdown(nil))
}

// TestOBS04_BuildTelemetry_UnknownProtocol rejects a bad --otel-protocol before
// any network I/O, so operators see a clear error rather than a misleading
// "connection refused" from an OTLP exporter attempting to dial with the
// wrong wire format.
func TestOBS04_BuildTelemetry_UnknownProtocol(t *testing.T) {
	_, _, err := BuildTelemetry(CLIFlags{
		OTelEndpoint: "localhost:14317",
		OTelProtocol: "thrift", // not supported
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thrift")
}

// TestOBS04_BuildTelemetry_RefusesInsecureRemote verifies the non-loopback
// guard: an operator who sets --otel-insecure against a non-local endpoint
// is rejected at startup. Without this guard, branch names, task IDs, and
// error text would transmit in cleartext to any remote collector — the
// insecure flag is a local-dev convenience, not a production toggle.
func TestOBS04_BuildTelemetry_RefusesInsecureRemote(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		loopback bool
	}{
		{"localhost", "localhost:14317", true},
		{"localhost-mixed-case", "LocalHost:4317", true},
		{"ipv4-loopback", "127.0.0.1:4317", true},
		{"ipv4-loopback-range", "127.1.2.3:4317", true},
		{"ipv6-loopback", "[::1]:4317", true},
		{"ipv6-loopback-bare-bracket", "[::1]", true},
		// Zone IDs like "::1%lo0" classify correctly in isLoopbackEndpoint
		// (netip.ParseAddr accepts them, unlike net.ParseIP) but the gRPC
		// URL resolver rejects "%" as an invalid escape before any dial
		// happens, so no end-to-end endpoint value is viable. Not tested.
		{"remote-host", "collector.example.com:4317", false},
		{"remote-ipv4", "10.0.0.1:4317", false},
		{"remote-ipv4-unspecified", "0.0.0.0:4317", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, shutdown, err := BuildTelemetry(CLIFlags{
				OTelEndpoint: tc.endpoint,
				OTelProtocol: "grpc",
				OTelInsecure: true,
			})
			if tc.loopback {
				require.NoError(t, err, "loopback + insecure must be accepted")
				// Providers were built; shut them down to avoid leaked
				// PeriodicReader / BatchSpanProcessor goroutines.
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					_ = shutdown(ctx)
				})
				return
			}
			require.Error(t, err, "non-loopback + insecure must be refused")
			assert.Contains(t, err.Error(), "loopback",
				"error must name the loopback requirement so operators know the fix")
		})
	}
}

// TestOBS04_BuildTelemetry_SecureRemoteAllowed: the loopback guard only
// fires on insecure+remote; secure+remote must pass startup validation.
// Shutdown uses a tight timeout because no collector is listening.
func TestOBS04_BuildTelemetry_SecureRemoteAllowed(t *testing.T) {
	telem, shutdown, err := BuildTelemetry(CLIFlags{
		OTelEndpoint: "collector.example.com:4317",
		OTelProtocol: "grpc",
		OTelInsecure: false,
	})
	require.NoError(t, err, "secure remote endpoint must be accepted")
	require.NotNil(t, telem)
	require.NotNil(t, shutdown)
	// Shutdown will likely error (no collector reachable); we only
	// assert it returns within the timeout rather than hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}

// TestOBS04_RunCmd_OTelFlagsParse confirms --otel-endpoint, --otel-protocol, and
// --otel-insecure all parse through cobra. Short-circuits before engine
// init via an invalid ledger so the test stays hermetic (no collector
// needed).
func TestOBS04_RunCmd_OTelFlagsParse(t *testing.T) {
	repo, wo := seedRepoWithWorkOrder(t)
	err, stderr := runCobra(t,
		"run",
		"--branch", "test",
		"--repo", repo,
		"--agent-dir", filepath.Join(repo, "agents"),
		"--otel-endpoint", "localhost:14317",
		"--otel-protocol", "grpc",
		"--otel-insecure=true",
		"--ledger", "dolt",
		wo,
	)
	require.Error(t, err)
	assert.NotContains(t, stderr, "unknown flag", "OTel flags must parse")
}
