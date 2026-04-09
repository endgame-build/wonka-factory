package orch

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// GateVerdict represents the result of quality gate evaluation.
type GateVerdict int

const (
	GatePass    GateVerdict = iota // gate agent completed → phase proceeds
	GateFail                       // gate agent failed → pipeline terminates (S6, PC-07)
	GateNone                       // no gate configured for this phase
	GatePending                    // gate agent not yet terminal
)

// String returns a human-readable label for the verdict.
func (v GateVerdict) String() string {
	switch v {
	case GatePass:
		return "pass"
	case GateFail:
		return "fail"
	case GateNone:
		return "none"
	case GatePending:
		return "pending"
	default:
		return fmt.Sprintf("GateVerdict(%d)", int(v))
	}
}

// EvaluateGate checks the quality gate for a phase (EXP-10, S6).
//
// Logic (per conformance profile §4.5):
//   - gate == nil                 → GateNone (phase proceeds)
//   - gate agent task completed   → GatePass (phase proceeds)
//   - gate agent task failed      → GateFail (pipeline terminates, PC-07)
//   - gate agent task not terminal → GatePending
//
// When retries create multiple tasks with the same AgentID, this function
// scans all matching children and applies latest-attempt-wins semantics:
// any completed attempt → GatePass; any non-terminal attempt → GatePending;
// all attempts failed → GateFail.
//
// The gate agent is identified by matching AgentID among the provided child tasks.
// Callers pass the already-loaded children slice to avoid redundant store reads.
func EvaluateGate(children []*Task, gate *QualityGate) GateVerdict {
	if gate == nil {
		return GateNone
	}

	found := false
	allFailed := true
	for _, child := range children {
		if child.AgentID != gate.Agent {
			continue
		}
		found = true
		switch child.Status {
		case StatusCompleted:
			return GatePass // any successful attempt suffices
		case StatusFailed:
			// continue scanning — a retry may have succeeded or still be running
		default:
			allFailed = false
		}
	}

	if !found {
		// Gate agent task not found — GateNone is safe (expansion guarantees presence).
		return GateNone
	}
	if allFailed {
		return GateFail
	}
	return GatePending
}

// EvaluateZeroFindings checks for CON-05: zero findings → pipeline terminates.
// This is a structural check (file size / line count), not content interpretation (ZFC, DSN-03).
//
// For md: returns true if the file has no non-header, non-empty lines after frontmatter.
// For jsonl: returns true if the file has zero data lines.
// For json/yaml: returns true if the file size is below a minimal threshold.
func EvaluateZeroFindings(outputPath string, format Format) (bool, error) {
	info, err := os.Stat(outputPath)
	if err != nil {
		return false, fmt.Errorf("evaluate zero findings: %w", err)
	}

	switch format {
	case FormatMd:
		return mdHasNoContent(outputPath)
	case FormatJsonl:
		return jsonlHasNoDataLines(outputPath)
	case FormatJson, FormatYaml:
		// Minimal structural heuristic: files under 50 bytes are "empty".
		return info.Size() < 50, nil
	default:
		return false, nil
	}
}

// mdHasNoContent returns true if a markdown file has only headers, frontmatter, and blank lines.
func mdHasNoContent(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			inFrontmatter = !inFrontmatter
			continue
		}
		if inFrontmatter || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return false, nil // found content
	}
	return true, scanner.Err()
}

// jsonlHasNoDataLines returns true if a JSONL file has zero non-empty lines.
func jsonlHasNoDataLines(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			return false, nil // found a data line
		}
	}
	return true, scanner.Err()
}
