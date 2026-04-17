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

// agentsDir is the path to the production instruction files relative to this
// test file's package directory. Tests run with CWD set to the package dir,
// so this resolves to <repo>/agents/.
const agentsDir = "../../agents"

// productionAgents lists every instruction file the CLI's role registry
// expects. Keep in sync with roleInstructionFiles in config.go.
var productionAgents = []string{"OOMPA.md", "LOOMPA.md", "CHARLIE.md"}

// TestAgentInstructionFilesParse confirms ReadAgentPrompt accepts every
// production instruction file — frontmatter is well-formed, body is present,
// and the model field is empty per design decision D3 (let the preset default
// win; no per-role model pinning). If the frontmatter starts carrying a model
// name, update D3 in the design doc before removing this assertion — a pinned
// model in frontmatter overrides the preset's default and will age badly.
func TestAgentInstructionFilesParse(t *testing.T) {
	for _, name := range productionAgents {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(agentsDir, name)
			body, model, err := orch.ReadAgentPrompt(path)
			require.NoError(t, err, "ReadAgentPrompt must succeed")
			assert.Greater(t, len(body), 500,
				"instruction body suspiciously short (%d bytes); truncated?", len(body))
			assert.Empty(t, model,
				"model must be empty (D3): preset default wins. Update D3 first if pinning.")
		})
	}
}

// TestAgentInstructionFilesHaveRequiredSections enforces BVV spec §6.1,
// which lists five section headings an instruction file MUST contain.
// This is a structural conformance check — missing a section means the agent
// is under-specified on a dimension the spec mandates.
func TestAgentInstructionFilesHaveRequiredSections(t *testing.T) {
	required := []string{
		"## Phase 1",
		"## Decision Rules",
		"## Operating Rules",
		"## Completion Protocol",
		"## Memory Format",
	}
	for _, name := range productionAgents {
		t.Run(name, func(t *testing.T) {
			body := readBody(t, name)
			for _, section := range required {
				assert.Contains(t, body, section,
					"spec §6.1 requires section %q", section)
			}
		})
	}
}

// Forbidden command-invocation patterns. These regexes match `bd` invocations
// anchored to line-start (possibly after whitespace for indented code-block
// lines), NOT inline mentions like `` `bd close` `` inside prose where the
// file is *prohibiting* the command. The distinction matters: the instruction
// files legitimately name these commands when forbidding them, but an agent
// would invoke them as leading shell commands.
//
// Patterns broken out by role because BVV spec §7.4 (BVV-TG-02) permits the
// planner to reset `failed`/`blocked` tasks to `open` via `bd update --status`;
// builder and verifier never mutate status.
var (
	// `bd close` is never permitted for any role — orchestrator owns closure
	// (spec §8.3.1a BVV-DSP-09).
	forbiddenBdCloseRe = regexp.MustCompile(`(?m)^\s*bd\s+close\b`)
	// `bd update --claim` is never permitted — orchestrator atomically
	// assigns workers via the dispatcher, not the agent.
	forbiddenBdClaimRe = regexp.MustCompile(`(?m)^\s*bd\s+update\b[^\n]*--claim\b`)
	// `bd update --status` is permitted only for CHARLIE (planner) per
	// BVV-TG-02 reconciliation; forbidden for OOMPA and LOOMPA.
	forbiddenBdStatusRe = regexp.MustCompile(`(?m)^\s*bd\s+update\b[^\n]*--status\b`)
)

// TestAgentInstructionFilesForbiddenPatterns catches the most likely
// regression: someone pastes a fragment from the medivet prior art
// (ralph/RALPH.md, verify/VERIFY.md, vicky/VICKY.md) that contains
// pre-BVV beads-mutation commands as executable shell (not just prose).
// The BVV spec forbids these — the orchestrator owns status transitions
// (§8.3.1a BVV-DSP-09).
//
// Also enforces positive patterns: every file must reference the
// environment contract (ORCH_TASK_ID, ORCH_BRANCH, PROGRESS.md).
// Exit-3 handoff applies to OOMPA and LOOMPA; spec §6.2 forbids it
// for the planner — CHARLIE must NOT reference exit 3 as a valid outcome.
func TestAgentInstructionFilesForbiddenPatterns(t *testing.T) {
	// Patterns every file must contain — environment contract, memory path,
	// and the mandatory task-discovery command per spec §8.4.1.
	requiredForAll := []string{
		"ORCH_TASK_ID",
		"ORCH_BRANCH",
		"ORCH_PROJECT",
		"PROGRESS.md",
		"bd show", // §8.4.1: every agent discovers its task via bd show.
	}

	// Role-specific positive checks.
	roleRequired := map[string][]string{
		// OOMPA and LOOMPA commit code; their files must carry the D9 commit
		// template trailer so the Task:/Branch: audit trail is greppable.
		// exit 3 handoff allowed (spec §6.2).
		"OOMPA.md":  {"exit 3", "Task: ORCH_TASK_ID="},
		"LOOMPA.md": {"exit 3", "Task: ORCH_TASK_ID="},
		// CHARLIE writes only to beads (no code commits), hence no commit
		// template trailer expected. Must reference bd create since it's the
		// only agent that writes tasks.
		"CHARLIE.md": {"bd create"},
	}

	// CHARLIE must not present exit 3 as a valid outcome, but it MAY mention
	// it in prose explaining why it is forbidden. Check the Completion
	// Protocol table specifically — exit-3 row would contain `| 3 |` or
	// similar. The `exit 3 is forbidden` / `MUST NOT exit 3` prose is OK.
	charlieExit3ForbiddenAsOutcomeRe := regexp.MustCompile(`(?mi)^\|\s*\*?\*?\s*3\s*\*?\*?\s*\|`)

	for _, name := range productionAgents {
		t.Run(name, func(t *testing.T) {
			body := readBody(t, name)

			// bd close: forbidden for all.
			assert.False(t, forbiddenBdCloseRe.MatchString(body),
				"%s must not invoke `bd close` (orchestrator owns closure, §8.3.1a)", name)
			// bd update --claim: forbidden for all.
			assert.False(t, forbiddenBdClaimRe.MatchString(body),
				"%s must not invoke `bd update --claim` (dispatcher atomically assigns)", name)
			// bd update --status: forbidden for OOMPA/LOOMPA only.
			if name != "CHARLIE.md" {
				assert.False(t, forbiddenBdStatusRe.MatchString(body),
					"%s must not invoke `bd update --status` "+
						"(only planner may reset failed/blocked tasks, BVV-TG-02)", name)
			}

			// Required env-contract references.
			for _, pat := range requiredForAll {
				assert.Contains(t, body, pat,
					"every instruction file must reference the env contract %q", pat)
			}
			// Role-specific positive checks.
			for _, pat := range roleRequired[name] {
				assert.Contains(t, body, pat,
					"%s must reference %q", name, pat)
			}

			// CHARLIE must not list exit 3 as a valid outcome in the
			// Completion Protocol table — spec §6.2: planner has no handoff.
			if name == "CHARLIE.md" {
				assert.False(t, charlieExit3ForbiddenAsOutcomeRe.MatchString(body),
					"CHARLIE must not list exit 3 as a valid outcome row "+
						"(spec §6.2: planner has no handoff)")
			}
		})
	}
}

// readBody parses an instruction file via the production ReadAgentPrompt
// path and returns its frontmatter-stripped body for substring assertions.
// Using the production parser (not a raw file read) guarantees tests catch
// patterns that would actually reach the agent's system prompt.
func readBody(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(agentsDir, name)
	body, _, err := orch.ReadAgentPrompt(path)
	require.NoError(t, err, "ReadAgentPrompt(%s)", path)
	require.NotEmpty(t, strings.TrimSpace(body))
	return body
}
