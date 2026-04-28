package orch

import (
	"fmt"
	"slices"
	"strings"
)

// TGRequirement names a specific BVV-TG-* requirement reported by
// ValidateLifecycleGraph. A typed value (rather than a free-form string)
// gives us compile-time protection against typos in call sites and lets
// downstream code switch exhaustively over the closed set.
//
// Constants are declared untyped below so they remain assignable to both
// TGRequirement-typed fields and string contexts (e.g. fmt.Sprintf with
// %s). The underlying type is string and %s formatting renders the
// requirement ID verbatim in audit-trail events and error messages.
type TGRequirement string

// TG-07..10 requirement IDs from BVV spec §7.5.
const (
	ReqTG07 = "BVV-TG-07" // every non-escalation task carries a configured role
	ReqTG08 = "BVV-TG-08" // task graph is acyclic
	ReqTG09 = "BVV-TG-09" // exactly one role:gate task depends on every role:verifier task
	ReqTG10 = "BVV-TG-10" // every non-escalation task is reachable from the plan task
)

// ReqTG00 tags orchestrator operational failures during graph validation
// (e.g. store read errors) — not a spec requirement, so excluded from
// AllTGRequirements. Matches the BVV-TG-NN shape pinned by
// TestValidate_GraphValidationErrorFormat so it parses consistently alongside
// real spec violations on the abort-reason field.
const ReqTG00 = "BVV-TG-00"

// AllTGRequirements enumerates every spec-defined BVV-TG-* requirement ID.
// Tests iterate this to pin the closed set; adding a new requirement
// without updating this slice yields a test failure at the TGRequirement
// boundary rather than a silent drift. ReqTG00 is intentionally excluded.
var AllTGRequirements = []TGRequirement{ReqTG07, ReqTG08, ReqTG09, ReqTG10}

// GraphValidationError reports a BVV-TG-07..10 violation discovered by
// ValidateLifecycleGraph. Carries the requirement ID (e.g. "BVV-TG-09"),
// a human-readable reason, and the offending task IDs so callers can
// surface actionable diagnostics to operators.
type GraphValidationError struct {
	Requirement TGRequirement // one of ReqTG07..ReqTG10
	Reason      string        // one-line human explanation
	TaskIDs     []string      // offending task IDs (may be empty for structural errors)
}

func (e *GraphValidationError) Error() string {
	if len(e.TaskIDs) == 0 {
		return fmt.Sprintf("[%s] %s", e.Requirement, e.Reason)
	}
	return fmt.Sprintf("[%s] %s: %s", e.Requirement, e.Reason, strings.Join(e.TaskIDs, ", "))
}

// ValidateLifecycleGraph checks BVV-TG-07..10 against all tasks carrying
// label "branch:<branch>" in the given store. Returns nil on a well-formed
// graph. Returns *GraphValidationError identifying the first failed
// requirement when malformed. Other errors wrap store failures.
//
// See docs/BVV_PHASE_9_PLAN.md §"Open Questions" for the rationale behind
// abort-on-failure vs. retry, and the default-on-at-Level-2 policy.
//
// Early skip (returns nil, no error): the branch has zero role:planner tasks.
// This is the legitimate Level 1 pre-populated-ledger path — validation is
// a Level 2 concern, and the absence of a planner task means no planner
// ever ran. Callers that want Level 1 operation with pre-populated ledgers
// should additionally gate this call via LifecycleConfig.ValidateGraph.
//
// Escalation tasks (role == RoleEscalation) are exempt from TG-07 role-map
// validation and TG-10 reachability — they're orchestrator-created
// human-inbox artifacts, not planner output.
//
// Implementation notes:
//   - Store exposes only forward dep edges (GetDeps). A reverse adjacency map
//     is built in a single pass to support TG-10 (plan→dependents traversal).
//   - TG-08 (acyclic) is already enforced by AddDep (LDG-06). The redundant
//     DFS here catches raw-DB tampering that bypasses AddDep. ~20 extra LoC
//     for defense-in-depth on a spec invariant.
func ValidateLifecycleGraph(store Store, branch string, roles map[string]RoleConfig) error {
	tasks, err := store.ListTasks(LabelBranch + ":" + branch)
	if err != nil {
		return fmt.Errorf("validate: list tasks for branch %q: %w", branch, err)
	}

	// Partition by role for cardinality checks.
	var planners, gates, verifiers []string
	taskByID := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		taskByID[t.ID] = t
		switch t.Role() {
		case RolePlanner:
			planners = append(planners, t.ID)
		case RoleGate:
			gates = append(gates, t.ID)
		case RoleVerifier:
			verifiers = append(verifiers, t.ID)
		}
	}

	// Early skip: pre-populated Level 1 ledger has no planner task.
	// Not a validation failure — the Level 2 well-formedness properties
	// don't apply when the graph wasn't assembled by a planner run.
	if len(planners) == 0 {
		return nil
	}

	// Multi-planner guard: BVV-TG-10 references "the plan task" (singular).
	// Covered under TG-10 rather than a separate requirement because
	// reachability-from-plan is ill-defined when there are multiple plans.
	if len(planners) > 1 {
		return &GraphValidationError{
			Requirement: ReqTG10,
			Reason:      fmt.Sprintf("exactly one role:planner task required, got %d", len(planners)),
			TaskIDs:     planners,
		}
	}
	planID := planners[0]

	// --- BVV-TG-07: every non-escalation task's role must be recognized. ---
	// A "recognized" role is either configured in lifecycle.Roles (handler
	// wired up, dispatchable normally) or a known closed-set value (e.g.
	// RoleGate before its handler ships). Known-but-unconfigured roles
	// flow through the dispatcher's BVV-DSP-03a escalation path at runtime
	// rather than failing the graph here — the alternative would block any
	// planner that emits a role:gate task while wonka still ships without
	// a default GATE.md, which is the documented out-of-the-box state.
	// Truly unknown roles (typos, deprecated values) still fail TG-07.
	var badRoles []string
	for _, t := range tasks {
		role := t.Role()
		if role == RoleEscalation {
			continue
		}
		if role == "" {
			badRoles = append(badRoles, t.ID)
			continue
		}
		if _, ok := roles[string(role)]; ok {
			continue
		}
		if !role.Valid() {
			badRoles = append(badRoles, t.ID)
		}
	}
	if len(badRoles) > 0 {
		return &GraphValidationError{
			Requirement: ReqTG07,
			Reason:      "tasks carry unrecognized role labels (not in lifecycle.Roles and not a closed-set Role value)",
			TaskIDs:     badRoles,
		}
	}

	// --- Build forward and reverse dep maps for reachability/cycle checks. ---
	forward := make(map[string][]string, len(tasks))
	reverse := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		deps, err := store.GetDeps(t.ID)
		if err != nil {
			return fmt.Errorf("validate: get deps for %s: %w", t.ID, err)
		}
		// Keep only intra-branch edges. An out-of-branch dep would violate
		// lifecycle scoping — surface as TG-10 orphaning downstream.
		var intra []string
		for _, dep := range deps {
			if _, ok := taskByID[dep]; ok {
				intra = append(intra, dep)
			}
		}
		forward[t.ID] = intra
		for _, dep := range intra {
			reverse[dep] = append(reverse[dep], t.ID)
		}
	}

	// --- BVV-TG-08: acyclic (DFS with 3-color marking). ---
	// AddDep already enforces acyclicity, but raw-DB edits could bypass it.
	// This catches that path and pins BVV-TG-08 as an independent spec test.
	if cyc := firstCycle(taskByID, forward); cyc != nil {
		return &GraphValidationError{
			Requirement: ReqTG08,
			Reason:      "dependency cycle detected",
			TaskIDs:     cyc,
		}
	}

	// --- BVV-TG-09: exactly one role:gate, reachable from gate must cover verifiers. ---
	if len(gates) != 1 {
		return &GraphValidationError{
			Requirement: ReqTG09,
			Reason:      fmt.Sprintf("exactly one role:gate task required, got %d", len(gates)),
			TaskIDs:     gates,
		}
	}
	gateID := gates[0]
	reachableFromGate := bfsReach(gateID, forward)
	var unreachedVerifiers []string
	for _, v := range verifiers {
		if !reachableFromGate[v] {
			unreachedVerifiers = append(unreachedVerifiers, v)
		}
	}
	if len(unreachedVerifiers) > 0 {
		return &GraphValidationError{
			Requirement: ReqTG09,
			Reason:      "gate task does not depend (directly or transitively) on all role:verifier tasks",
			TaskIDs:     unreachedVerifiers,
		}
	}

	// --- BVV-TG-10: every task reachable from plan via dependency edges. ---
	// "Reachable from plan" = traverse reverse edges (dependents). The plan
	// is the root (no deps); everything else should depend on it directly
	// or via a chain.
	reachableFromPlan := bfsReach(planID, reverse)
	var orphans []string
	for _, t := range tasks {
		if t.ID == planID {
			continue
		}
		if t.Role() == RoleEscalation {
			// Escalation tasks are orchestrator-created mid-lifecycle and
			// intentionally off-graph — exempt from the reachability rule.
			continue
		}
		if !reachableFromPlan[t.ID] {
			orphans = append(orphans, t.ID)
		}
	}
	if len(orphans) > 0 {
		return &GraphValidationError{
			Requirement: ReqTG10,
			Reason:      "tasks not reachable from plan task via dependency edges",
			TaskIDs:     orphans,
		}
	}

	return nil
}

// firstCycle returns a task-ID slice naming at least one node on a cycle,
// or nil if the graph (restricted to nodes in taskByID) is acyclic.
// Uses the classic white/gray/black DFS coloring.
func firstCycle(taskByID map[string]*Task, forward map[string][]string) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(taskByID))
	var cycle []string
	// Iterate in sorted order for deterministic cycle reporting — two
	// equally-valid cycles shouldn't produce alternating test outputs.
	ids := sortedIDs(taskByID)

	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		for _, dep := range forward[id] {
			switch color[dep] {
			case gray:
				cycle = []string{id, dep}
				return true
			case white:
				if visit(dep) {
					// Cycle found deeper in the recursion — bubble up
					// without extending `cycle`. The two back-edge
					// endpoints set in the gray branch above are enough
					// evidence for operator diagnostics; a full cycle
					// reconstruction would require tracking the recursion
					// stack, which is out of scope for this defensive
					// check (AddDep catches the common case).
					return true
				}
			}
		}
		color[id] = black
		return false
	}
	for _, id := range ids {
		if color[id] == white {
			if visit(id) {
				return cycle
			}
		}
	}
	return nil
}

// bfsReach returns the set of node IDs reachable from start via the given
// adjacency map. Uses BFS to avoid stack growth on long dependency chains.
func bfsReach(start string, adj map[string][]string) map[string]bool {
	reached := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if reached[next] {
				continue
			}
			reached[next] = true
			queue = append(queue, next)
		}
	}
	return reached
}

// sortedIDs returns task IDs in lexicographic order. Keeps DFS cycle
// detection deterministic across runs with the same graph shape.
func sortedIDs(taskByID map[string]*Task) []string {
	ids := make([]string, 0, len(taskByID))
	for id := range taskByID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
