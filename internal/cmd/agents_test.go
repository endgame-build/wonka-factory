package cmd

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const agentsDir = "../../agents"

// Forbidden `bd` command invocations, anchored to line-start so prose mentions
// like `` `bd close` `` inside an instruction *forbidding* the command don't
// match. `bd update --status` is permitted only for CHARLIE per BVV-TG-02
// (planner may reset failed/blocked tasks to open); builders and verifiers
// never mutate status.
var (
	forbiddenBdCloseRe  = regexp.MustCompile(`(?m)^\s*bd\s+close\b`)
	forbiddenBdClaimRe  = regexp.MustCompile(`(?m)^\s*bd\s+update\b.*--claim\b`)
	forbiddenBdStatusRe = regexp.MustCompile(`(?m)^\s*bd\s+update\b.*--status\b`)

	// CHARLIE may explain in prose why exit 3 is forbidden, but MUST NOT list
	// it as a row in its Completion Protocol table (spec §6.2). A table row
	// looks like `| 3 |` or `| **3** |`.
	charlieExit3AsOutcomeRe = regexp.MustCompile(`(?mi)^\|\s*\*?\*?\s*3\s*\*?\*?\s*\|`)
)

// TestAgentInstructionFiles exercises every production instruction file
// through the live ReadAgentPrompt path, enforcing BVV spec §6.1 structural
// requirements plus the forbidden/required pattern matrices.
func TestAgentInstructionFiles(t *testing.T) {
	// Use the production source of role→file mappings so renaming a role in
	// config.go propagates here automatically — the historical drift trap.
	for _, name := range instructionFileBasenames() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(agentsDir, name)
			body, model, err := orch.ReadAgentPrompt(path)
			require.NoError(t, err)
			require.NotEmpty(t, strings.TrimSpace(body))
			assert.Greater(t, len(body), 500,
				"instruction body suspiciously short (%d bytes)", len(body))

			// D3: model stays empty so preset default wins. If a model gets
			// pinned in frontmatter, revisit D3 before removing this line.
			assert.Empty(t, model,
				"model must be empty per D3; update the design doc first if pinning")

			// Spec §6.1 mandates these five section headings.
			for _, section := range []string{
				"## Phase 1",
				"## Decision Rules",
				"## Operating Rules",
				"## Completion Protocol",
				"## Memory Format",
			} {
				assert.Contains(t, body, section,
					"spec §6.1 requires %q", section)
			}

			// Environment contract + task-discovery protocol (spec §8.4.1).
			for _, pat := range []string{
				"ORCH_TASK_ID",
				"ORCH_BRANCH",
				"ORCH_PROJECT",
				"PROGRESS.md",
				"bd show",
			} {
				assert.Contains(t, body, pat,
					"env contract requires %q", pat)
			}

			// §8.3.1a BVV-DSP-09: no agent closes tasks.
			assert.False(t, forbiddenBdCloseRe.MatchString(body),
				"%s must not invoke `bd close` (orchestrator owns closure)", name)
			assert.False(t, forbiddenBdClaimRe.MatchString(body),
				"%s must not invoke `bd update --claim` (dispatcher assigns)", name)

			switch name {
			case "OOMPA.md", "LOOMPA.md":
				// BVV-TG-02: only the planner may reset task status.
				assert.False(t, forbiddenBdStatusRe.MatchString(body),
					"%s must not invoke `bd update --status`", name)
				// §6.2: builder and verifier may handoff.
				assert.Contains(t, body, "exit 3",
					"%s must reference exit 3 handoff", name)
				// D9: commits carry the Task: trailer for audit grep.
				assert.Contains(t, body, "Task: ORCH_TASK_ID=",
					"%s must carry the D9 commit template trailer", name)

			case "CHARLIE.md":
				// CHARLIE writes tasks, not code.
				assert.Contains(t, body, "bd create",
					"CHARLIE must reference bd create")
				// §6.2: planner has no handoff; exit 3 must not be a valid
				// outcome row in the Completion Protocol table.
				assert.False(t, charlieExit3AsOutcomeRe.MatchString(body),
					"CHARLIE must not list exit 3 as a Completion Protocol row")
			}
		})
	}
}
