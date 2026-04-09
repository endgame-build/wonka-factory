package testutil

import (
	"path/filepath"

	"github.com/endgame/facet-scan/orch"
)

// MockScriptDir returns the absolute path to testdata/mock-agents/ relative
// to the repo root. Tests should call this to locate mock scripts.
func MockScriptDir(repoRoot string) string {
	return filepath.Join(repoRoot, "testdata", "mock-agents")
}

// MockPreset returns a Preset that runs a specific mock agent script via bash.
// The script receives the output path as its first argument ($1).
func MockPreset(scriptPath string) *orch.Preset {
	return &orch.Preset{
		Name:         "mock",
		Command:      "bash",
		Args:         []string{scriptPath},
		ProcessNames: []string{"bash"},
		PromptFlag:   "",
		AgentFlag:    "",
		PluginFlag:   "",
		Env:          map[string]string{},
	}
}

// MockPresetForScript returns a Preset that runs a named mock script from the
// given mock directory (e.g., "success", "fail", "invalid-output", "hang", "crash").
func MockPresetForScript(mockDir, scriptName string) *orch.Preset {
	return MockPreset(filepath.Join(mockDir, scriptName+".sh"))
}

// PresetRouter maps agentIDs to specific mock scripts for targeted fault injection.
// Agents not in the map use the default script.
type PresetRouter struct {
	MockDir   string
	Default   string            // default script name (e.g., "success")
	Overrides map[string]string // agentID → script name
}

// PresetFor returns the mock Preset for a given agentID.
func (r *PresetRouter) PresetFor(agentID string) *orch.Preset {
	script := r.Default
	if override, ok := r.Overrides[agentID]; ok {
		script = override
	}
	return MockPresetForScript(r.MockDir, script)
}
