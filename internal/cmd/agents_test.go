package cmd

import (
	"fmt"
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

// Forbidden `bd` invocations. The line-start anchor permits blockquoted
// callouts ("> **Never** run `bd close`...") to survive while catching any
// copy-bait fenced shell line.
//
// `bd update --status` is permitted only for CHARLIE per BVV-TG-02 (further
// constrained to `--status open` by forbiddenCharlieStatusRe below).
var (
	forbiddenBdCloseRe  = regexp.MustCompile(`(?m)^\s*bd\s+close\b`)
	forbiddenBdClaimRe  = regexp.MustCompile(`(?m)^\s*bd\s+update\b.*--claim\b`)
	forbiddenBdStatusRe = regexp.MustCompile(`(?m)^\s*bd\s+update\b.*--status\b`)

	// CHARLIE's only permitted `--status` target is `open`. Any other target
	// re-terminalizes a terminal task (BVV-TG-03) or grabs status ownership
	// from the orchestrator.
	forbiddenCharlieStatusRe = regexp.MustCompile(
		`(?m)^\s*bd\s+update\b[^\n]*--status\s+(?:closed|completed|in_progress|failed|blocked)\b`)

	frontmatterNameRe = regexp.MustCompile(`(?m)^name:\s*(\S+)`)
)

// requiredSections are the spec §6.1 mandated top-level headings. Each must
// appear exactly once per file — duplicates signal a bad merge.
var requiredSections = []string{
	"## Phase 1",
	"## Decision Rules",
	"## Operating Rules",
	"## Completion Protocol",
	"## Memory Format",
}

// exitCodeRows[n] matches a Completion Protocol table row for exit code n,
// e.g. `| 0 |` or `| **0** |` at line start.
var exitCodeRows = func() map[int]*regexp.Regexp {
	m := make(map[int]*regexp.Regexp, 4)
	for code := 0; code <= 3; code++ {
		m[code] = regexp.MustCompile(fmt.Sprintf(`(?m)^\|\s*\*?\*?\s*%d\s*\*?\*?\s*\|`, code))
	}
	return m
}()

func TestAgentInstructionFiles(t *testing.T) {
	t.Parallel()

	// Iterate the production role→file registry so a rename propagates to
	// the test automatically; the switch below still keys on role, not
	// basename, so a BUILDER.md rename doesn't fall through to default.
	for role, basename := range roleInstructionFiles {
		t.Run(basename, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(agentsDir, basename)
			body, model, err := orch.ReadAgentPrompt(path)
			require.NoError(t, err)
			require.Greater(t, len(body), 500,
				"instruction body suspiciously short (%d bytes) — corruption or truncation", len(body))
			// ReadAgentPrompt's frontmatter parser silently falls through
			// when the closing `\n---` delimiter is missing, returning the
			// entire file (including the YAML block) as the body.
			require.False(t, strings.HasPrefix(body, "---"),
				"body starts with `---` — frontmatter parser fell through; check delimiter shape")

			// D3: model must be empty so preset default wins. Pinning a
			// model in frontmatter ages badly — update the design doc
			// before removing this assertion.
			assert.Empty(t, model,
				"model must be empty per D3; update the design doc first if pinning")

			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			m := frontmatterNameRe.FindStringSubmatch(string(raw))
			require.NotNil(t, m, "frontmatter `name:` key not found")
			wantName := strings.ToLower(strings.TrimSuffix(basename, ".md"))
			assert.Equal(t, wantName, m[1],
				"frontmatter name %q does not match basename stem %q", m[1], wantName)

			for _, heading := range requiredSections {
				// Trailing `\b` distinguishes `## Phase 1` from `## Phase 10`.
				re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(heading) + `\b`)
				matches := re.FindAllStringIndex(body, 2)
				require.Equal(t, 1, len(matches),
					"spec §6.1: expected exactly one `%s` heading, got %d", heading, len(matches))
			}

			// Environment contract + task-discovery protocol (spec §8.4.1).
			for _, pat := range []string{
				"ORCH_TASK_ID",
				"ORCH_BRANCH",
				"ORCH_PROJECT",
				"PROGRESS.md",
				"bd show",
			} {
				assert.Contains(t, body, pat, "env contract requires %q", pat)
			}

			// Stricter than BVV-DSP-09 (which allows agent-side closure for
			// backward compatibility): no production agent invokes these.
			assert.False(t, forbiddenBdCloseRe.MatchString(body),
				"%s must not invoke `bd close` (orchestrator owns closure)", basename)
			assert.False(t, forbiddenBdClaimRe.MatchString(body),
				"%s must not invoke `bd update --claim` (dispatcher assigns)", basename)

			switch role {
			case "builder", "verifier":
				// BVV-TG-02: only the planner may reset task status.
				assert.False(t, forbiddenBdStatusRe.MatchString(body),
					"%s must not invoke `bd update --status`", basename)
				// §6.2: builder/verifier have four valid exit codes — each
				// must appear in the Completion Protocol table.
				for code := 0; code <= 3; code++ {
					assert.Regexp(t, exitCodeRows[code], body,
						"%s must list exit %d as a Completion Protocol row", basename, code)
				}
				// D9: commits carry the Task: trailer for audit grep.
				assert.Contains(t, body, "Task: ORCH_TASK_ID=",
					"%s must carry the D9 commit template trailer", basename)

			case "planner":
				assert.Contains(t, body, "bd create",
					"CHARLIE must reference `bd create`")
				// §6.2: planner exits 0/1/2 only — exit 3 must not be a row.
				assert.False(t, exitCodeRows[3].MatchString(body),
					"CHARLIE must not list exit 3 as a Completion Protocol row")
				for code := 0; code <= 2; code++ {
					assert.Regexp(t, exitCodeRows[code], body,
						"CHARLIE must list exit %d as a Completion Protocol row", code)
				}
				// BVV-TG-02: CHARLIE's only permitted --status target is `open`.
				assert.False(t, forbiddenCharlieStatusRe.MatchString(body),
					"CHARLIE may only set --status open; found an invalid target")

			default:
				t.Fatalf("no role-specific rules defined for role %q — update agents_test.go switch", role)
			}
		})
	}
}
