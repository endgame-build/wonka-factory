package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/endgame/wonka-factory/orch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const agentsDir = "../../agents"

// Forbidden `bd` command invocations. The line-start anchor prevents inline
// code mentions and blockquoted prose from matching; fenced code blocks DO
// match, which is the intent — fences are copy-bait for an LLM.
//
// `bd update --status` is permitted only for CHARLIE per BVV-TG-02; builders
// and verifiers never mutate status. CHARLIE's own permitted usage is further
// constrained below to `--status open` only (see forbiddenCharlieStatusRe).
var (
	forbiddenBdCloseRe  = regexp.MustCompile(`(?m)^\s*bd\s+close\b`)
	forbiddenBdClaimRe  = regexp.MustCompile(`(?m)^\s*bd\s+update\b.*--claim\b`)
	forbiddenBdStatusRe = regexp.MustCompile(`(?m)^\s*bd\s+update\b.*--status\b`)

	// CHARLIE may explain in prose why exit 3 is forbidden, but MUST NOT list
	// it as a row in its Completion Protocol table (spec §6.2). A table row
	// looks like `| 3 |` or `| **3** |`.
	charlieExit3AsOutcomeRe = regexp.MustCompile(`(?mi)^\|\s*\*?\*?\s*3\s*\*?\*?\s*\|`)

	// CHARLIE's only permitted `--status` target is `open` (BVV-TG-02). Any
	// other target violates BVV-TG-03 or re-terminalizes a terminal task.
	forbiddenCharlieStatusRe = regexp.MustCompile(
		`(?m)^\s*bd\s+update\b[^\n]*--status\s+(?:closed|completed|in_progress|failed|blocked)\b`)

	frontmatterNameRe = regexp.MustCompile(`(?m)^name:\s*(\S+)`)

	// Spec §6.1 mandated section headings. The trailing `\b` distinguishes
	// `## Phase 1:` (1 followed by non-word `:`) from `## Phase 10:`.
	sectionHeadingREs = map[string]*regexp.Regexp{
		"Phase 1":             regexp.MustCompile(`(?m)^## Phase 1\b`),
		"Decision Rules":      regexp.MustCompile(`(?m)^## Decision Rules\b`),
		"Operating Rules":     regexp.MustCompile(`(?m)^## Operating Rules\b`),
		"Completion Protocol": regexp.MustCompile(`(?m)^## Completion Protocol\b`),
		"Memory Format":       regexp.MustCompile(`(?m)^## Memory Format\b`),
	}

	// Completion Protocol table-row regexes for each exit code. Matches
	// `| 0 |` or `| **0** |` at line start.
	exitCodeRowREs = map[int]*regexp.Regexp{
		0: regexp.MustCompile(`(?m)^\|\s*\*?\*?\s*0\s*\*?\*?\s*\|`),
		1: regexp.MustCompile(`(?m)^\|\s*\*?\*?\s*1\s*\*?\*?\s*\|`),
		2: regexp.MustCompile(`(?m)^\|\s*\*?\*?\s*2\s*\*?\*?\s*\|`),
		3: regexp.MustCompile(`(?m)^\|\s*\*?\*?\s*3\s*\*?\*?\s*\|`),
	}
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
			// Promoted to require: a truncated file should produce one clear
			// diagnostic, not a cascade of fifteen section-missing asserts.
			require.Greater(t, len(body), 500,
				"instruction body suspiciously short (%d bytes) — corruption or truncation", len(body))
			// ReadAgentPrompt's frontmatter parser silently falls through if
			// the closing `\n---` delimiter is missing, returning the entire
			// file (including the YAML block) as the body. Catch that here.
			require.False(t, strings.HasPrefix(body, "---"),
				"body starts with `---` — frontmatter parser fell through; check delimiter shape")

			// D3: model stays empty so preset default wins. Pinning a model
			// in frontmatter ages badly — update the design doc before
			// removing this line.
			assert.Empty(t, model,
				"model must be empty per D3; update the design doc first if pinning")

			// Frontmatter `name:` must match the lowercased basename stem.
			// ReadAgentPrompt discards the name field, so we re-read the raw
			// file to inspect it.
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			m := frontmatterNameRe.FindStringSubmatch(string(raw))
			require.NotNil(t, m, "frontmatter `name:` key not found")
			wantName := strings.ToLower(strings.TrimSuffix(name, ".md"))
			assert.Equal(t, wantName, m[1],
				"frontmatter name %q does not match basename stem %q", m[1], wantName)

			// Spec §6.1 mandates these five section headings. Count exactly
			// one of each to catch duplicate-section regressions (bad merges)
			// and `## Phase 1` vs `## Phase 10` prefix collisions.
			for heading, re := range sectionHeadingREs {
				matches := re.FindAllStringIndex(body, -1)
				require.Equal(t, 1, len(matches),
					"spec §6.1: expected exactly one `## %s` heading, got %d", heading, len(matches))
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

			// Instruction-file rule (stricter than BVV-DSP-09, which requires
			// orchestrator authority but permits agent-side closure for
			// backward compatibility): no agent invokes these at all.
			assert.False(t, forbiddenBdCloseRe.MatchString(body),
				"%s must not invoke `bd close` (orchestrator owns closure)", name)
			assert.False(t, forbiddenBdClaimRe.MatchString(body),
				"%s must not invoke `bd update --claim` (dispatcher assigns)", name)

			switch name {
			case "OOMPA.md", "LOOMPA.md":
				// BVV-TG-02: only the planner may reset task status.
				assert.False(t, forbiddenBdStatusRe.MatchString(body),
					"%s must not invoke `bd update --status`", name)
				// §6.2: builder and verifier have four valid exit codes —
				// every row must appear in the Completion Protocol table. A
				// dropped row would leave the agent without guidance for
				// that failure class.
				for code := 0; code <= 3; code++ {
					assert.Regexp(t, exitCodeRowREs[code], body,
						"%s must list exit %d as a Completion Protocol row", name, code)
				}
				// D9: commits carry the Task: trailer for audit grep.
				assert.Contains(t, body, "Task: ORCH_TASK_ID=",
					"%s must carry the D9 commit template trailer", name)

			case "CHARLIE.md":
				// CHARLIE writes tasks, not code.
				assert.Contains(t, body, "bd create",
					"CHARLIE must reference `bd create`")
				// §6.2: planner has 0/1/2 only — exit 3 must not be a row,
				// and 0/1/2 must each be a row.
				assert.False(t, charlieExit3AsOutcomeRe.MatchString(body),
					"CHARLIE must not list exit 3 as a Completion Protocol row")
				for code := 0; code <= 2; code++ {
					assert.Regexp(t, exitCodeRowREs[code], body,
						"CHARLIE must list exit %d as a Completion Protocol row", code)
				}
				// BVV-TG-02: CHARLIE's only permitted --status target is
				// `open`. Any other target violates BVV-TG-03 or re-
				// terminalizes terminal state.
				assert.False(t, forbiddenCharlieStatusRe.MatchString(body),
					"CHARLIE may only set --status open; found an invalid target")

			default:
				t.Fatalf("no role-specific rules defined for %s — update agents_test.go switch", name)
			}
		})
	}
}
