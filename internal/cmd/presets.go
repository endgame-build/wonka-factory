// Package cmd wires the wonka CLI to the orch library. presets.go holds
// the registry of agent launch presets selectable via --agent.
package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/endgame/wonka-factory/orch"
)

// jqExtractText pulls assistant text from claude's stream-json output for
// the .txt sidecar log.
const jqExtractText = `select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text // empty`

// SystemPromptFlag must be --append-system-prompt (body value), not
// --append-system-prompt-file (path) — orch.BuildCommand passes the
// instruction body literally to this flag (see its doc-comment for the
// contract). --dangerously-skip-permissions is required: without it
// claude blocks on tool-use prompts and orchestrated sessions hang.
// --print is required for non-interactive operation: claude refuses
// --output-format stream-json without it. The KickoffPrompt is the
// trailing user message --print mode demands; agents read their actual
// task from $ORCH_TASK_ID per the role instruction file injected as
// the system prompt, so a one-word nudge is sufficient.
var presets = map[string]*orch.Preset{
	"claude": {
		Name:             "claude",
		Command:          "claude",
		Args:             []string{"--dangerously-skip-permissions", "--print", "--output-format", "stream-json", "--verbose"},
		SystemPromptFlag: "--append-system-prompt",
		ModelFlag:        "--model",
		ProcessNames:     []string{"node", "claude"},
		Env:              map[string]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "99"},
		TextFilter:       jqExtractText,
		KickoffPrompt:    "Begin.",
	},
}

// LookupPreset returns the preset matching name, or an error listing the
// available keys. Used by config.BuildEngineConfig and the --agent flag
// validator in root.go.
func LookupPreset(name string) (*orch.Preset, error) {
	if p, ok := presets[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown agent preset %q (available: %s)", name, strings.Join(presetNames(), ", "))
}

func presetNames() []string {
	names := make([]string, 0, len(presets))
	for k := range presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
