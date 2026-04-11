package orch

import (
	"errors"
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
// instructionBody is the frontmatter-stripped content of the role's .md file
// (returned by ReadAgentPrompt). It is passed as the literal argument to
// preset.SystemPromptFlag — matching the Claude/codex/goose CLI contract
// where --append-system-prompt takes a prompt string, not a file path.
// model overrides the preset default via preset.ModelFlag. maxTurns appends
// --max-turns when > 0; zero means "preset default".
//
// Per the BVV plan, agent identity no longer flows through a dedicated CLI
// flag — the task's role label is resolved to a RoleConfig by the dispatcher,
// and the instruction body for that role is injected directly via the
// system-prompt flag. Agents discover their task via the ORCH_TASK_ID env
// var (see BuildEnv) and read the task body from the ledger.
func BuildCommand(preset *Preset, instructionBody, model string, maxTurns int) []string {
	cmd := []string{preset.Command}
	cmd = append(cmd, preset.Args...)

	if preset.SystemPromptFlag != "" && instructionBody != "" {
		cmd = append(cmd, preset.SystemPromptFlag, instructionBody)
	}
	if preset.ModelFlag != "" && model != "" {
		cmd = append(cmd, preset.ModelFlag, model)
	}
	if maxTurns > 0 {
		// --max-turns is universal across supported presets (claude, codex, goose).
		cmd = append(cmd, "--max-turns", strconv.Itoa(maxTurns))
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

// ReadAgentPrompt reads a role instruction file, strips YAML frontmatter,
// and returns the body (suitable for --append-system-prompt injection) plus
// the model name from the frontmatter (suitable for --model).
//
// Returns empty strings (no error) when the file doesn't exist — the agent
// runs without custom instructions, which is acceptable for generic presets
// that carry their own defaults.
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
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
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
