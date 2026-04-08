------------------------------ MODULE BVVTypes ------------------------------
(***************************************************************************)
(* Constants, type aliases, and helper operators for the BVV formal model. *)
(* No variables or actions — pure definitions only.                        *)
(*                                                                         *)
(* Spec reference: BUILD_VERIFY_VALIDATE_SPEC.md                           *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences

CONSTANTS
    MaxTasks,       \* Upper bound on task IDs (superset for dynamic creation)
    Workers,        \* Set of worker IDs, e.g. {"w1", "w2"}
    Branches,       \* Set of branch names for concurrent lifecycles
    Orchestrators,  \* Set of orchestrator IDs, e.g. {"orch1", "orch2"}
    MaxRetries,     \* Maximum retry attempts per task (RECOMMENDED: 2)
    MaxHandoffs,    \* Maximum handoffs per task (RECOMMENDED: 5)
    GapTolerance,   \* Gap threshold before lifecycle abort (RECOMMENDED: 3)
    HumanActions,   \* BOOLEAN: enable/disable external human re-open
    None            \* Model value sentinel for empty fields

\* Task ID domain
TaskIDs == 1..MaxTasks

\* --------------------------------------------------------------------------
\* Status sets (Section 5.1a)
\* --------------------------------------------------------------------------

StatusSet == {"not_created", "open", "assigned", "in_progress",
              "completed", "failed", "blocked"}

TerminalSet == {"completed", "failed", "blocked"}

ActiveSet == {"assigned", "in_progress"}

RoleSet == {"builder", "verifier", "planner", "gate"}

WorkerStatusSet == {"idle", "busy", "suspended"}

LockStatusSet == {"free", "held", "stale"}

\* --------------------------------------------------------------------------
\* Type correctness predicate (used as TypeOK invariant)
\* --------------------------------------------------------------------------

\* Declared here as a template — actual TypeOK defined in BVV.tla
\* where variables are in scope.

\* --------------------------------------------------------------------------
\* Helper operators
\* --------------------------------------------------------------------------

\* Predicate: status is terminal
IsTerminal(s) == s \in TerminalSet

\* Predicate: task has been created (not in "not_created" state)
\* NOTE: requires taskStatus variable — used in modules that EXTEND this one
\* Defined as an operator template; actual usage depends on variable scope.

\* Set cardinality helper
Card(S) == Cardinality(S)

\* --------------------------------------------------------------------------
\* Graph helpers for acyclicity checking (BVV-TG-08)
\* --------------------------------------------------------------------------

\* Transitive closure of a dependency relation over TaskIDs.
\* deps is [TaskIDs -> SUBSET TaskIDs].
\* TC(deps)[t] = all tasks reachable from t via deps edges.
RECURSIVE ReachableFrom(_, _, _)
ReachableFrom(t, depRel, visited) ==
    LET directDeps == depRel[t] \ visited
        newVisited == visited \cup directDeps
    IN  directDeps \cup
        UNION {ReachableFrom(d, depRel, newVisited) : d \in directDeps}

\* Check: no task is reachable from itself (acyclic graph)
IsAcyclic(depRel) ==
    \A t \in TaskIDs : t \notin ReachableFrom(t, depRel, {})

=============================================================================
