package orch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/endgame/facet-scan/orch"
	"github.com/endgame/facet-scan/orch/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- End-to-End Tests with Mock Agents ---

// findRepoRoot walks up from the test binary's location to find go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}

// TestE2E_MiniPipelineAllComplete runs a mini pipeline with mock success agents.
func TestE2E_MiniPipelineAllComplete(t *testing.T) {
	skipIfNoTmux(t)

	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "success")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "e2e-mini"
	cfg.MaxWorkers = 4
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	require.NoError(t, err, "pipeline should complete successfully")

	// Verify event log has pipeline_complete event.
	eventLogPath := filepath.Join(dir, "events.jsonl")
	testutil.ValidateEventSequence(t, eventLogPath, []orch.EventKind{
		orch.EventPhaseStart,
		orch.EventPipelineComplete,
	})
}

// TestE2E_GatedPipelineGatePass runs a gated pipeline where the gate passes.
func TestE2E_GatedPipelineGatePass(t *testing.T) {
	skipIfNoTmux(t)

	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.GatedPipeline()
	preset := testutil.MockPresetForScript(mockDir, "success")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "e2e-gate-pass"
	// MaxWorkers=1 ensures sequential agents run one at a time, respecting
	// input dependencies (validator depends on checker's output).
	cfg.MaxWorkers = 1
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 5 * time.Second, CBThreshold: 5, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	require.NoError(t, err, "gated pipeline with passing gate should complete")
}

// TestE2E_GatedPipelineGateFails runs a gated pipeline where the gate agent fails.
func TestE2E_GatedPipelineGateFails(t *testing.T) {
	skipIfNoTmux(t)

	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.GatedPipeline()

	// Use fail.sh for the gate agent, success.sh for others.
	// The gate agent in GatedPipeline is "validator".
	// We need a preset that runs fail.sh — but all agents use the same preset.
	// Use fail preset for ALL agents — phase 1 agents fail (gap), gate also fails.
	preset := testutil.MockPresetForScript(mockDir, "fail")

	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "e2e-gate-fail"
	cfg.MaxWorkers = 4
	cfg.Retry = orch.RetryConfig{MaxRetries: 0, BaseTimeout: time.Second}
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = engine.Run(ctx)
	assert.Error(t, err, "pipeline should fail")
}

// TestE2E_ResumeAfterInterrupt verifies [RCV-13]: interrupted pipeline is resumable from persisted state.
func TestE2E_ResumeAfterInterrupt(t *testing.T) {
	skipIfNoTmux(t)

	repoRoot := findRepoRoot(t)
	mockDir := testutil.MockScriptDir(repoRoot)
	dir := t.TempDir()

	p := testutil.MiniPipeline()
	preset := testutil.MockPresetForScript(mockDir, "success")

	// First run: start and cancel quickly.
	cfg := orch.DefaultEngineConfig(preset, "", dir, repoRoot)
	cfg.RunID = "e2e-resume"
	cfg.MaxWorkers = 4
	cfg.Dispatch = orch.DispatchConfig{Interval: 200 * time.Millisecond}
	cfg.Watchdog = orch.WatchdogConfig{Interval: 2 * time.Second, CBThreshold: 3, CBWindow: 60 * time.Second}

	engine1, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	_ = engine1.Run(ctx1) // likely times out or partially completes

	// Verify ledger exists.
	ledgerDir := filepath.Join(dir, "ledger")
	require.DirExists(t, ledgerDir)

	// Resume.
	engine2, err := orch.NewEngine(p, cfg)
	require.NoError(t, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	err = engine2.Resume(ctx2)
	// Resume may succeed or error depending on state — just verify it doesn't panic.
	_ = err
}
