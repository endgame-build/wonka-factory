package orch

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BuildCommand constructs the command-line invocation for an agent from
// preset configuration and role metadata. The command is fully determined by
// these static inputs — no content-derived or content-predicated arguments
// (BVV-DSN-04 phase-agnostic, BVV-DSP-04 exit-code-only outcome).
//
// systemPromptValue is what gets passed verbatim to preset.SystemPromptFlag —
// for the claude preset that's a path to a sidecar prompt file written by
// SpawnSession (see PromptPath), keeping large instruction bodies out of the
// tmux command buffer. Other presets that prefer the body-form flag can pass
// the body directly. model overrides the preset default via preset.ModelFlag.
// maxTurns appends --max-turns when > 0; zero means "preset default".
//
// Per the BVV plan, agent identity no longer flows through a dedicated CLI
// flag — the task's role label is resolved to a RoleConfig by the dispatcher,
// and the instruction body for that role is injected via the system-prompt
// flag. Agents discover their task via the ORCH_TASK_ID env var (see
// BuildEnv) and read the task body from the ledger.
func BuildCommand(preset *Preset, systemPromptValue, model string, maxTurns int) []string {
	cmd := []string{preset.Command}
	cmd = append(cmd, preset.Args...)

	if preset.SystemPromptFlag != "" && systemPromptValue != "" {
		cmd = append(cmd, preset.SystemPromptFlag, systemPromptValue)
	}
	if preset.ModelFlag != "" && model != "" {
		cmd = append(cmd, preset.ModelFlag, model)
	}
	if maxTurns > 0 {
		// --max-turns is universal across supported presets (claude, codex, goose).
		cmd = append(cmd, "--max-turns", strconv.Itoa(maxTurns))
	}
	// Kickoff prompt last, so it's the trailing positional. Required by CLIs
	// like claude in --print mode where a positional prompt or stdin is
	// mandatory; presets that don't need one leave it empty.
	if preset.KickoffPrompt != "" {
		cmd = append(cmd, preset.KickoffPrompt)
	}
	return cmd
}

// BuildEnv constructs the environment variable map for an agent invocation
// (BVV-ITF-01, BVV-DSP-06). Environment variables are the sole mechanism for
// injecting identity context; the agent reads its task from the ledger via
// `bd show $ORCH_TASK_ID`.
//
// BVV drops the fork's ORCH_ROLE and ORCH_WORKSPACE: roles are encoded in
// the instruction file (injected via --append-system-prompt), and BVV agents
// commit to the branch instead of writing to a workspace directory.
func BuildEnv(workerName, runID, repoPath, taskID, branch string, presetEnv map[string]string) map[string]string {
	env := make(map[string]string, len(presetEnv)+5)
	for k, v := range presetEnv {
		env[k] = v
	}
	env["ORCH_WORKER_NAME"] = workerName
	env["ORCH_RUN_ID"] = runID
	env["ORCH_PROJECT"] = repoPath
	env["ORCH_TASK_ID"] = taskID
	env["ORCH_BRANCH"] = branch
	return env
}

// LogPath returns the canonical log file path for a task's stdout/stderr.
// BVV-DSP-04: the sidecar exit-code file lives at {LogPath()}.exitcode and
// is read by ReadExitCode (see tmux.go).
func LogPath(runDir, taskID string) string {
	return filepath.Join(runDir, "logs", taskID+".stdout")
}

// PromptPath returns the canonical sidecar file path for an agent's system
// prompt. SpawnSession writes the role instruction body here and passes the
// path to claude via --append-system-prompt-file rather than inlining the
// content into the tmux command — large prompts (CHARLIE.md is ~16 KB)
// overflow tmux's command-parsing buffer on macOS otherwise (`tmux: command
// too long`). The threshold depends on tmux build options and platform
// ARG_MAX, so we side-step it entirely by always using a file.
func PromptPath(runDir, taskID string) string {
	return filepath.Join(runDir, "logs", taskID+".prompt.md")
}

// ReadAgentPrompt reads a role instruction file, strips YAML frontmatter,
// and returns the body (suitable for --append-system-prompt injection) plus
// the model name from the frontmatter (suitable for --model).
//
// An empty instructionFile short-circuits with ("", "", nil) — the caller
// explicitly opted out of custom instructions, and generic presets that
// carry their own defaults can run as-is. A non-empty path that does NOT
// exist is a misconfiguration (typo, renamed file, missing deploy), so we
// return the os.ErrNotExist wrapped with context. Silently defaulting to
// an empty body in that case would launch the agent with no role prompt
// and let BVV-DSP-04 mark the task complete on a normal exit — by the time
// the mistake surfaces the terminal state is irreversible.
//
// BVV change from fork: the signature takes the full instructionFile path
// directly (from RoleConfig.InstructionFile), not a (pluginDir, agentID)
// pair. Roles are now declared in LifecycleConfig.Roles, not inferred from
// file naming conventions.
func ReadAgentPrompt(instructionFile string) (body, model string, err error) {
	if instructionFile == "" {
		return "", "", nil
	}
	data, err := os.ReadFile(instructionFile)
	if err != nil {
		return "", "", fmt.Errorf("read agent prompt %s: %w", instructionFile, err)
	}

	content := string(data)

	// Strip YAML frontmatter delimited by "---\n" ... "\n---".
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---")
		if end >= 0 {
			frontmatter := content[4 : 4+end]
			body = strings.TrimSpace(content[4+end+4:])

			// Extract model from frontmatter (simple line scan, no full YAML parse).
			for _, line := range strings.Split(frontmatter, "\n") {
				if strings.HasPrefix(line, "model:") {
					model = strings.TrimSpace(strings.TrimPrefix(line, "model:"))
				}
			}
			return body, model, nil
		}
	}

	// No frontmatter — return entire content as body.
	return strings.TrimSpace(content), "", nil
}

// DetermineOutcome maps a process exit code to an AgentOutcome (BVV-DSP-04).
// The orchestrator MUST determine task outcome from the exit code alone — no
// output content inspection (BVV-DSN-04, BVV-S-05).
//
// BVV simplification from fork: the fork's DetermineOutcome took (exitCode,
// outputErr, criticality) and mixed outcome determination with retry/gap
// routing. BVV separates concerns — this is a pure exit-code switch; the
// dispatcher's processOutcome handles retry/gap/criticality routing.
func DetermineOutcome(exitCode int) AgentOutcome {
	switch exitCode {
	case 0:
		return OutcomeSuccess
	case 1:
		return OutcomeFailure
	case 2:
		return OutcomeBlocked
	case 3:
		return OutcomeHandoff
	default:
		return OutcomeFailure
	}
}
