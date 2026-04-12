package orch

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GateConfig controls gate handler behavior.
type GateConfig struct {
	// CITimeout is the maximum time to wait for CI checks to complete.
	// Default: 30 minutes.
	CITimeout time.Duration
}

// DefaultGateConfig returns sensible production defaults.
func DefaultGateConfig() GateConfig {
	return GateConfig{CITimeout: 30 * time.Minute}
}

// ExecuteGate implements the PR gate handler (BVV spec §8.3).
//
// The gate is a deterministic script (BVV-AI-02), not an AI agent. It:
//  1. Checks predecessor statuses (BVV-GT-03) — skips PR if any dep failed/blocked
//  2. Creates a PR via `gh pr create`
//  3. Polls CI via `gh pr checks --watch` with timeout
//  4. Returns exit code 0 (pass) or 1 (fail)
//
// BVV-GT-01: the gate MUST NOT merge the PR. PR creation is the terminal action.
// BVV-GT-02: gate failure does not block other lifecycles (enforced by lifecycle scoping).
func ExecuteGate(ctx context.Context, store Store, log *EventLog, taskID, repoPath, targetBranch, sourceBranch string, cfg GateConfig) int {
	// 1. Check predecessor statuses (BVV-GT-03).
	deps, err := store.GetDeps(taskID)
	if err != nil {
		emitGate(log, EventGateFailed, taskID, fmt.Sprintf("gate: get deps: %v", err))
		return 1
	}
	for _, depID := range deps {
		dep, err := store.GetTask(depID)
		if err != nil {
			emitGate(log, EventGateFailed, taskID, fmt.Sprintf("gate: get dep %s: %v", depID, err))
			return 1
		}
		if dep.Status == StatusFailed || dep.Status == StatusBlocked {
			emitGate(log, EventGateFailed, taskID,
				fmt.Sprintf("gate: predecessor %s has status %s — skipping PR creation", depID, dep.Status))
			return 1
		}
	}

	emitGate(log, EventGateCreated, taskID, fmt.Sprintf("gate: creating PR %s → %s", sourceBranch, targetBranch))

	// 2. Create PR.
	prArgs := []string{"pr", "create",
		"--base", targetBranch,
		"--head", sourceBranch,
		"--title", fmt.Sprintf("feat: %s", sourceBranch),
		"--body", "Automated PR created by wonka-factory gate handler.",
	}
	if err := runGH(ctx, repoPath, prArgs...); err != nil {
		// PR may already exist — treat as non-fatal and continue to CI check.
		if !strings.Contains(err.Error(), "already exists") {
			emitGate(log, EventGateFailed, taskID, fmt.Sprintf("gate: gh pr create: %v", err))
			return 1
		}
	}

	// 3. Poll CI checks with timeout.
	ciCtx, cancel := context.WithTimeout(ctx, cfg.CITimeout)
	defer cancel()

	checkArgs := []string{"pr", "checks", sourceBranch, "--watch"}
	if err := runGH(ciCtx, repoPath, checkArgs...); err != nil {
		emitGate(log, EventGateFailed, taskID, fmt.Sprintf("gate: CI checks failed: %v", err))
		return 1
	}

	// 4. All checks passed (BVV-GT-01: no auto-merge).
	emitGate(log, EventGatePassed, taskID, fmt.Sprintf("gate: CI passed for %s", sourceBranch))
	return 0
}

// runGH executes a `gh` CLI command in the given directory.
func runGH(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// emitGate writes a gate event to the log. The gate runs inside a tmux
// session without access to ProgressReporter — only the EventLog is available.
func emitGate(log *EventLog, kind EventKind, taskID, summary string) {
	if log == nil {
		return
	}
	_ = log.Emit(Event{Kind: kind, TaskID: taskID, Summary: summary})
}
