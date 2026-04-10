package orch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTMX01_SocketPrefixIsolation locks in the BVV socket rename from "orch-"
// to "wonka-". This is a unit check on the constructor — no tmux binary
// required — that guards against accidental regression during future
// refactors.
func TestTMX01_SocketPrefixIsolation(t *testing.T) {
	tc := NewTmuxClient("run-abc")
	if got, want := tc.Socket, "wonka-run-abc"; got != want {
		t.Errorf("socket prefix: got %q, want %q", got, want)
	}
	if strings.HasPrefix(tc.Socket, "orch-") {
		t.Errorf("socket %q must not use the fork's 'orch-' prefix", tc.Socket)
	}
	if tc.RunID() != "run-abc" {
		t.Errorf("RunID: got %q, want %q", tc.RunID(), "run-abc")
	}
}

// TestBuildShellCommand_Deterministic verifies that env var export order is
// deterministic (sorted keys) so the generated shell command is stable across
// runs. This is load-bearing for sidecar-based exit-code capture (BVV
// Appendix A): the same inputs must produce byte-identical shell commands so
// test fixtures remain reliable.
func TestBuildShellCommand_Deterministic(t *testing.T) {
	env := map[string]string{
		"ORCH_WORKER_NAME": "w1",
		"ORCH_TASK_ID":     "task-001",
		"ORCH_BRANCH":      "feature-x",
	}
	cmd := []string{"echo", "hello"}

	out1, err := BuildShellCommand(cmd, env, "/tmp/log.stdout", "")
	if err != nil {
		t.Fatalf("BuildShellCommand: %v", err)
	}
	out2, err := BuildShellCommand(cmd, env, "/tmp/log.stdout", "")
	if err != nil {
		t.Fatalf("BuildShellCommand (second call): %v", err)
	}
	if out1 != out2 {
		t.Errorf("BuildShellCommand is non-deterministic:\n  1: %s\n  2: %s", out1, out2)
	}

	// Must include the exit-code sidecar per BVV Appendix A.
	if !strings.Contains(out1, "echo $? > '/tmp/log.stdout.exitcode'") {
		t.Errorf("sidecar exit-code capture missing from shell command:\n%s", out1)
	}
}

// TestBuildShellCommand_InvalidEnvKey verifies that the env-key validator
// rejects non-POSIX identifiers and returns ErrInvalidEnvKey via errors.Is.
func TestBuildShellCommand_InvalidEnvKey(t *testing.T) {
	for _, bad := range []string{"1BAD", "has-dash", "has space", ""} {
		t.Run(bad, func(t *testing.T) {
			_, err := BuildShellCommand(
				[]string{"echo"},
				map[string]string{bad: "v"},
				"", "",
			)
			if err == nil {
				t.Fatalf("expected error for env key %q", bad)
			}
			if !errors.Is(err, ErrInvalidEnvKey) {
				t.Errorf("expected ErrInvalidEnvKey, got: %v", err)
			}
		})
	}
}

// TestSessionName_Canonical locks in the session naming scheme consumed by
// the watchdog (BVV-ERR-11) and session.go (WKR-04).
func TestSessionName_Canonical(t *testing.T) {
	if got, want := SessionName("run-1", "w-001"), "run-1-w-001"; got != want {
		t.Errorf("SessionName: got %q, want %q", got, want)
	}
}

// TestReadExitCode_MissingSidecar verifies that a missing sidecar file
// returns (-1, nil), not an error — callers treat -1 as "unknown exit code,
// likely killed before bash could write". BVV dispatcher maps -1 to failure.
func TestReadExitCode_MissingSidecar(t *testing.T) {
	code, err := ReadExitCode("/nonexistent/path/nolog.stdout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != -1 {
		t.Errorf("missing sidecar: got exit code %d, want -1", code)
	}
}

// TestReadExitCode_EmptySidecar covers the partial-write edge case: the
// sidecar file exists (bash opened it) but has no content (bash was killed
// before the write completed). Must return (-1, nil) — the same "unknown
// exit code" signal as a missing file. BVV-DSP-04 treats unknown as failure.
func TestReadExitCode_EmptySidecar(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "task-x.stdout")
	if err := os.WriteFile(logPath+".exitcode", []byte{}, 0o644); err != nil {
		t.Fatalf("write empty sidecar: %v", err)
	}

	code, err := ReadExitCode(logPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != -1 {
		t.Errorf("empty sidecar: got exit code %d, want -1", code)
	}
}

// TestReadExitCode_WhitespaceOnlySidecar covers another partial-write case:
// bash wrote whitespace (e.g., a stray newline) but no integer. The parser
// must treat this as unknown (-1), not as a parse error.
func TestReadExitCode_WhitespaceOnlySidecar(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "task-y.stdout")
	if err := os.WriteFile(logPath+".exitcode", []byte("   \n"), 0o644); err != nil {
		t.Fatalf("write whitespace sidecar: %v", err)
	}

	code, err := ReadExitCode(logPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != -1 {
		t.Errorf("whitespace sidecar: got exit code %d, want -1", code)
	}
}
