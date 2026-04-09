package orch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentOutcome represents the result of an agent invocation (OPS-05).
type AgentOutcome int

const (
	// OutcomeCompleted — valid output, task transitions to completed.
	OutcomeCompleted AgentOutcome = iota
	// OutcomeRetry — invalid output + critical agent → invoke retry protocol.
	OutcomeRetry
	// OutcomeGap — invalid output + non-critical agent → record gap.
	OutcomeGap
	// OutcomeFailed — non-zero exit code → task failed.
	OutcomeFailed
)

// String returns a human-readable label for the outcome.
func (o AgentOutcome) String() string {
	switch o {
	case OutcomeCompleted:
		return "completed"
	case OutcomeRetry:
		return "retry"
	case OutcomeGap:
		return "gap"
	case OutcomeFailed:
		return "failed"
	default:
		return fmt.Sprintf("AgentOutcome(%d)", int(o))
	}
}

// BuildCommand constructs the command-line invocation for an agent from preset
// configuration and agent metadata. The command is fully determined by these
// static inputs — no content-derived or content-predicated arguments (DSN-01, DSN-02).
func BuildCommand(preset *Preset, agentDef AgentDef, pluginDir, prompt string) []string {
	cmd := []string{preset.Command}
	cmd = append(cmd, preset.Args...)

	if preset.PromptFlag != "" && prompt != "" {
		cmd = append(cmd, preset.PromptFlag, prompt)
	}
	if preset.AgentFlag != "" && agentDef.ID != "" {
		cmd = append(cmd, preset.AgentFlag, agentDef.ID)
	}
	if agentDef.MaxTurns > 0 {
		// OPS-15: --max-turns is universal across all supported presets (claude, codex, goose).
		cmd = append(cmd, "--max-turns", strconv.Itoa(agentDef.MaxTurns))
	}
	if preset.PluginFlag != "" && pluginDir != "" {
		cmd = append(cmd, preset.PluginFlag, pluginDir)
	}

	return cmd
}

// BuildPrompt constructs a minimal prompt directing the agent to write output
// to the declared path. The orchestrator provides the path, not content
// instructions — the agent's system prompt comes from the embedded .md file
// via the --agent flag (ZFC).
//
// taskOutput is the task-level output path (e.g., "MANIFEST_A.md" for consensus
// instances), which may differ from agentDef.Output ("MANIFEST.md"). The prompt
// must use the task-level path so agents write to the correct location.
func BuildPrompt(taskOutput string, inputs []string, outputDir string) string {
	outputPath := filepath.Join(outputDir, taskOutput)
	var sb strings.Builder
	sb.WriteString("Write your output to: ")
	sb.WriteString(outputPath)
	if len(inputs) > 0 {
		sb.WriteString("\n\nInput files:\n")
		for _, input := range inputs {
			sb.WriteString("- ")
			sb.WriteString(filepath.Join(outputDir, input))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// BuildEnv constructs the environment variable map for an agent invocation (ITF-01).
// Environment variables are the sole mechanism for injecting identity context (ITF-02).
//
// Note: ORCH_ROLE is set to agentID (e.g., "00-scout"), not the worker's full
// address as the formal spec ITF-01 defines. This is an intentional narrowing —
// agents need to know their role, not their worker identity.
func BuildEnv(workerName, runID, outputDir, repoPath, agentID, branch string, presetEnv map[string]string) map[string]string {
	env := make(map[string]string, len(presetEnv)+6)
	for k, v := range presetEnv {
		env[k] = v
	}
	env["ORCH_WORKER_NAME"] = workerName
	env["ORCH_RUN_ID"] = runID
	env["ORCH_WORKSPACE"] = outputDir
	env["ORCH_PROJECT"] = repoPath
	env["ORCH_ROLE"] = agentID
	env["ORCH_BRANCH"] = branch
	return env
}

// ValidateInputs checks that all declared input files exist and are non-empty
// before invoking an agent (OPS-03).
func ValidateInputs(inputs []string, baseDir string) error {
	var missing []string
	for _, input := range inputs {
		path := filepath.Join(baseDir, input)
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				missing = append(missing, input)
				continue
			}
			return fmt.Errorf("stat input %s: %w", input, err)
		}
		if info.Size() == 0 {
			missing = append(missing, input)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrInputMissing, strings.Join(missing, ", "))
	}
	return nil
}

// ValidateOutput performs structural format checks on an agent's output file (OPS-04).
// This is purely structural — no content interpretation (DSN-03).
//
// Format-specific checks:
//   - md:    file exists, size > 100 bytes, first line starts with '#' or '---'
//   - jsonl: file exists, first line parses as valid JSON
//   - json:  file exists, content parses as valid JSON
//   - yaml:  file exists, content parses as valid YAML
func ValidateOutput(path string, format Format) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrOutputMissing, path)
		}
		return fmt.Errorf("stat output %s: %w", path, err)
	}

	switch format {
	case FormatMd:
		return validateMd(path, info.Size())
	case FormatJsonl:
		return validateJsonl(path)
	case FormatJson:
		return validateJson(path)
	case FormatYaml:
		return validateYaml(path)
	default:
		return fmt.Errorf("%w: unknown format %q", ErrOutputInvalid, format)
	}
}

// DetermineOutcome maps exit code + validation error + criticality to an outcome (OPS-05).
// Pure function with no content inspection.
func DetermineOutcome(exitCode int, outputErr error, criticality Criticality) AgentOutcome {
	// Non-zero exit → failed regardless of output.
	if exitCode != 0 {
		return OutcomeFailed
	}
	// Zero exit + valid output → completed.
	if outputErr == nil {
		return OutcomeCompleted
	}
	// Zero exit + invalid output → retry (critical) or gap (non-critical).
	if criticality == Critical {
		return OutcomeRetry
	}
	return OutcomeGap
}

// LogPath returns the canonical log file path for an agent task's stdout/stderr (OPS-19).
func LogPath(runDir, taskID string) string {
	return filepath.Join(runDir, "logs", taskID+".stdout")
}

// ReadAgentPrompt reads an agent definition from the plugin directory, strips
// YAML frontmatter, and returns the content body and model name. The body is
// suitable for injection via --append-system-prompt; the model maps to --model.
//
// Returns empty strings (no error) when the file doesn't exist — the agent
// runs without custom instructions, which is acceptable for generic presets.
func ReadAgentPrompt(pluginDir, agentID string) (body, model string, err error) {
	path := filepath.Join(pluginDir, "agents", agentID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read agent prompt %s: %w", agentID, err)
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

// --- format-specific validators ---

func validateMd(path string, size int64) error {
	if size <= 100 {
		return fmt.Errorf("%w: md file %s is %d bytes (need >100)", ErrOutputInvalid, path, size)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutputInvalid, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		first := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(first, "#") {
			return nil
		}
		if strings.HasPrefix(first, "---") {
			// YAML frontmatter — scan for agent-level crash markers.
			// The crash-detection-protocol skill writes "status: IN_PROGRESS"
			// or "crash_marker: true" before analysis. These files pass the
			// size and header checks but are not valid agent output.
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "---" {
					break // end of frontmatter
				}
				if line == "crash_marker: true" || line == "status: IN_PROGRESS" {
					return fmt.Errorf("%w: md file %s is an agent crash marker (not completed output)", ErrOutputInvalid, path)
				}
			}
			return nil
		}
		return fmt.Errorf("%w: md file %s first line has no header or frontmatter", ErrOutputInvalid, path)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: md file %s: %v", ErrOutputInvalid, path, err)
	}
	return fmt.Errorf("%w: md file %s is empty", ErrOutputInvalid, path)
}

func validateJsonl(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutputInvalid, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		var v json.RawMessage
		if json.Unmarshal([]byte(scanner.Text()), &v) == nil {
			return nil
		}
		return fmt.Errorf("%w: jsonl file %s first line is not valid JSON", ErrOutputInvalid, path)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: jsonl file %s: %v", ErrOutputInvalid, path, err)
	}
	return fmt.Errorf("%w: jsonl file %s is empty", ErrOutputInvalid, path)
}

func validateJson(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutputInvalid, err)
	}
	var v json.RawMessage
	if json.Unmarshal(data, &v) != nil {
		return fmt.Errorf("%w: json file %s is not valid JSON", ErrOutputInvalid, path)
	}
	return nil
}

func validateYaml(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOutputInvalid, err)
	}
	var v any
	if yaml.Unmarshal(data, &v) != nil {
		return fmt.Errorf("%w: yaml file %s is not valid YAML", ErrOutputInvalid, path)
	}
	return nil
}
