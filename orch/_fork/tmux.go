package orch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// TmuxClient wraps tmux CLI operations with socket isolation.
// Each pipeline run uses its own socket ("orch-{runID}") to prevent
// collision with the user's tmux or other orchestrator runs.
type TmuxClient struct {
	Socket string
	runID  string
}

// NewTmuxClient creates a TmuxClient with socket isolation for the given run.
func NewTmuxClient(runID string) *TmuxClient {
	return &TmuxClient{Socket: "orch-" + runID, runID: runID}
}

// RunID returns the run identifier this client was created with.
func (tc *TmuxClient) RunID() string {
	return tc.runID
}

// Available reports whether the tmux binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// StartServer starts the tmux server and sets exit-empty off so the server
// persists even when all sessions die. This prevents a race where rapid
// session death causes the server to shut down between back-to-back
// CreateSession calls — a race observed on CI where mock agents finish
// in <10ms.
//
// Safe to call if the server is already running (skips if bootstrap session exists).
// Must be paired with KillServer in cleanup.
func (tc *TmuxClient) StartServer() error {
	bootstrapName := tc.runID + "-bootstrap"

	// If the bootstrap session already exists, the server is running.
	if exists, err := tc.HasSession(bootstrapName); err == nil && exists {
		return nil
	}

	// Create a bootstrap session + set exit-empty off in a single tmux
	// invocation. The ";" separator chains commands atomically within the
	// same server process, eliminating the TOCTOU window where the bootstrap
	// session could die between new-session and set-option.
	out, err := exec.Command("tmux", "-L", tc.Socket, //nolint:gosec // args are controlled
		"new-session", "-d", "-s", bootstrapName, "sleep", "infinity",
		";",
		"set-option", "-g", "exit-empty", "off",
	).CombinedOutput()
	if err != nil {
		_ = tc.KillServer() // clean up partial state
		return fmt.Errorf("tmux start-server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateSession starts a new detached tmux session.
//
// shellCmd is a complete shell command string (may include redirections,
// env exports, pipes). It is passed to bash -c, so it must be a valid
// shell expression. Use BuildShellCommand to construct it.
//
// workDir sets the starting directory for the session (tmux -c flag).
// When non-empty, the session's shell starts in that directory, which
// affects CLAUDE.md auto-discovery and file tool paths. Pass the target
// repository path so agents analyse the correct codebase.
func (tc *TmuxClient) CreateSession(name, shellCmd, workDir string) error {
	if shellCmd == "" {
		return errors.New("tmux: empty command")
	}

	args := []string{
		"-L", tc.Socket,
		"new-session",
		"-d",
		"-s", name,
	}
	if workDir != "" {
		args = append(args, "-c", workDir)
	}
	args = append(args, "bash", "-c", shellCmd)

	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-session %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// validEnvKey matches POSIX-compliant environment variable names.
var validEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// BuildShellCommand constructs a shell command string that exports env vars,
// runs the given command, and writes the exit code to a sidecar file.
// Uses shell export instead of tmux -e (available only in tmux 3.2+) for broader compatibility.
//
// The command runs as a child process (not exec) so that bash can write the exit code
// to {logPath}.exitcode after the command exits. The tmux session ends when bash exits.
//
// When textFilter is non-empty, the command output is piped through tee (to capture
// raw output in logPath) and then through jq (to extract filtered text into a .txt file).
// This enables stream-json logging: the .stdout file gets full JSONL, the .txt file gets
// human-readable assistant text. The exit code uses PIPESTATUS[0] to capture the agent's
// exit code (not jq's). If jq is unavailable, the .txt file is empty but .stdout and
// .exitcode are unaffected.
//
// Returns an error if any env key is not a valid POSIX identifier.
func BuildShellCommand(cmd []string, env map[string]string, logPath, textFilter string) (string, error) {
	// Sort keys for deterministic output (aids debugging and testing).
	keys := make([]string, 0, len(env))
	for k := range env {
		if !validEnvKey.MatchString(k) {
			return "", fmt.Errorf("%w: %q", ErrEnvKeyInvalid, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "export %s=%s; ", k, shellQuote(env[k]))
	}

	for i, part := range cmd {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(shellQuote(part))
	}

	if logPath != "" {
		if textFilter != "" {
			// Stream-json mode: tee raw JSONL to .stdout, pipe through jq to .txt.
			// PIPESTATUS[0] captures the agent's exit code, not jq's.
			// Note: PIPESTATUS is bash-specific; this works because CreateSession uses "bash -c".
			textPath := strings.TrimSuffix(logPath, ".stdout") + ".txt"
			sb.WriteString(" | tee ")
			sb.WriteString(shellQuote(logPath))
			sb.WriteString(" | jq -r --unbuffered ")
			sb.WriteString(shellQuote(textFilter))
			sb.WriteString(" > ")
			sb.WriteString(shellQuote(textPath))
			sb.WriteString(" 2>/dev/null; echo ${PIPESTATUS[0]} > ")
			sb.WriteString(shellQuote(logPath + ".exitcode"))
		} else {
			sb.WriteString(" > ")
			sb.WriteString(shellQuote(logPath))
			sb.WriteString(" 2>&1; echo $? > ")
			sb.WriteString(shellQuote(logPath + ".exitcode"))
		}
	}

	return sb.String(), nil
}

// ReadExitCode reads the exit code from a sidecar file written by BuildShellCommand.
// Returns -1 if the file does not exist (agent was killed before bash wrote it).
// Callers must treat -1 as "unknown" — distinct from exit 0 (success).
func ReadExitCode(logPath string) (int, error) {
	data, err := os.ReadFile(logPath + ".exitcode")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return -1, nil
		}
		return -1, fmt.Errorf("read exit code: %w", err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return -1, nil // empty sidecar → unknown exit code (partial write / abrupt kill)
	}
	code, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse exit code %q: %w", s, err)
	}
	return code, nil
}

// isSessionNotFound returns true if the tmux output indicates the session
// (or server) does not exist. Covers tmux version message variants.
func isSessionNotFound(msg string) bool {
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "no current") ||
		strings.Contains(msg, "error connecting")
}

// HasSession reports whether a session with the given name is alive.
// Returns (false, nil) if the session does not exist or no server is running.
// Returns a non-nil error for infrastructure failures (exec, permission, etc.).
func (tc *TmuxClient) HasSession(name string) (bool, error) {
	out, err := exec.Command("tmux", "-L", tc.Socket, "has-session", "-t", name).CombinedOutput() //nolint:gosec // args are controlled
	if err == nil {
		return true, nil
	}
	if isSessionNotFound(string(out)) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session %q: %w: %s", name, err, strings.TrimSpace(string(out)))
}

// KillSession terminates a specific session.
func (tc *TmuxClient) KillSession(name string) error {
	out, err := exec.Command("tmux", "-L", tc.Socket, "kill-session", "-t", name).CombinedOutput() //nolint:gosec // args are controlled
	if err != nil {
		return fmt.Errorf("tmux kill-session %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListSessions returns the names of all sessions on this socket.
// Returns an empty slice (not error) if no server is running.
func (tc *TmuxClient) ListSessions() ([]string, error) {
	out, err := exec.Command( //nolint:gosec // args are controlled
		"tmux", "-L", tc.Socket,
		"list-sessions", "-F", "#{session_name}",
	).CombinedOutput()
	if err != nil {
		if isSessionNotFound(string(out)) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %w: %s", err, strings.TrimSpace(string(out)))
	}

	text := strings.TrimSpace(string(out))
	if text == "" {
		return []string{}, nil
	}
	return strings.Split(text, "\n"), nil
}

// KillServer terminates all sessions on this socket. Safe to call if
// no server is running.
func (tc *TmuxClient) KillServer() error {
	out, err := exec.Command("tmux", "-L", tc.Socket, "kill-server").CombinedOutput() //nolint:gosec // args are controlled
	if err != nil {
		if isSessionNotFound(string(out)) {
			return nil // idempotent cleanup
		}
		return fmt.Errorf("tmux kill-server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SessionName builds the canonical session name for a worker in this run.
func SessionName(runID, workerName string) string {
	return runID + "-" + workerName
}

// KillSessionIfExists kills a session, suppressing "not found" and "no server"
// errors. All other errors (permission denied, exec failure) are propagated.
func (tc *TmuxClient) KillSessionIfExists(name string) error {
	err := tc.KillSession(name)
	if err == nil {
		return nil
	}
	if isSessionNotFound(err.Error()) {
		return nil
	}
	return err
}

// shellQuote wraps a value in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
