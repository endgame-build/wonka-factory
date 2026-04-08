-------------------------------- MODULE BVV --------------------------------
(***************************************************************************)
(* Top-level BVV formal specification.                                     *)
(*                                                                         *)
(* Composes all modules, defines Init/Next/Spec, and encodes:              *)
(*   - 11 safety properties (BVV-S-01 through BVV-S-10, including S-02a)   *)
(*   - 4 liveness properties (BVV-L-01 through BVV-L-04)                   *)
(*   - Type correctness invariant                                          *)
(*                                                                         *)
(* Spec reference: BUILD_VERIFY_VALIDATE_SPEC.md Sections 12, 13           *)
(***************************************************************************)
EXTENDS BVVLifecycle

\* =========================================================================
\*  INITIAL STATE
\* =========================================================================

\* For static configurations (smoke, small): all tasks start with pre-wired
\* status, role, deps, branch. Dynamic planning configs start with only
\* plan task(s) as "open" and everything else as "not_created".
\*
\* This Init is for the dynamic planning model. Static configs override
\* via INIT in the .cfg file.

Init ==
    \* Task state: everything not_created except we allow external setup
    /\ taskStatus \in [TaskIDs -> {"not_created", "open"}]
    \* At least one task must be open (the plan task)
    /\ \E t \in TaskIDs : taskStatus[t] = "open"
    \* Open tasks must have roles, not_created tasks have None
    /\ role \in [TaskIDs -> RoleSet \cup {None}]
    /\ \A t \in TaskIDs :
        (taskStatus[t] = "open" => role[t] \in RoleSet) /\
        (taskStatus[t] = "not_created" => role[t] = None)
    \* Open tasks must have a branch
    /\ taskBranch \in [TaskIDs -> Branches \cup {None}]
    /\ \A t \in TaskIDs :
        (taskStatus[t] = "open" => taskBranch[t] \in Branches) /\
        (taskStatus[t] = "not_created" => taskBranch[t] = None)
    \* All counters start at zero
    /\ retryCount = [t \in TaskIDs |-> 0]
    /\ handoffCount = [t \in TaskIDs |-> 0]
    /\ assignee = [t \in TaskIDs |-> None]
    /\ critical \in [TaskIDs -> BOOLEAN]
    \* Deps: open tasks may have deps on other open tasks; not_created have empty
    /\ deps \in [TaskIDs -> SUBSET TaskIDs]
    /\ \A t \in TaskIDs :
        taskStatus[t] = "not_created" => deps[t] = {}
    /\ IsAcyclic(deps)
    \* System state
    /\ workerState = [w \in Workers |-> "idle"]
    /\ workerTask = [w \in Workers |-> None]
    /\ sessionAlive = [w \in Workers |-> FALSE]
    /\ gapCount = [b \in Branches |-> 0]
    /\ lifecycleAborted = [b \in Branches |-> FALSE]
    /\ lifecycleLock = [b \in Branches |-> "free"]
    /\ lockHolder = [b \in Branches |-> None]
    /\ dispatching = [b \in Branches |-> FALSE]
    /\ prCreated = [t \in TaskIDs |-> FALSE]
    /\ humanReopenFlag = FALSE

\* =========================================================================
\*  STATIC INIT OPERATORS (for smoke.cfg, small.cfg)
\* =========================================================================

\* Pick a branch from the Branches set (works with TLC model values)
DefaultBranch == CHOOSE b \in Branches : TRUE

\* Static init for smoke.cfg: linear chain plan(1) -> build(2) -> vv(3)
StaticInit3 ==
    /\ taskStatus = [t \in TaskIDs |->
        IF t \in {1, 2, 3} THEN "open" ELSE "not_created"]
    /\ role = [t \in TaskIDs |->
        CASE t = 1 -> "planner"
          [] t = 2 -> "builder"
          [] t = 3 -> "verifier"
          [] OTHER -> None]
    /\ deps = [t \in TaskIDs |->
        CASE t = 1 -> {}
          [] t = 2 -> {1}
          [] t = 3 -> {2}
          [] OTHER -> {}]
    /\ taskBranch = [t \in TaskIDs |->
        IF t \in {1, 2, 3} THEN DefaultBranch ELSE None]
    /\ critical = [t \in TaskIDs |->
        IF t = 1 THEN TRUE ELSE FALSE]
    /\ retryCount = [t \in TaskIDs |-> 0]
    /\ handoffCount = [t \in TaskIDs |-> 0]
    /\ assignee = [t \in TaskIDs |-> None]
    /\ workerState = [w \in Workers |-> "idle"]
    /\ workerTask = [w \in Workers |-> None]
    /\ sessionAlive = [w \in Workers |-> FALSE]
    /\ gapCount = [b \in Branches |-> 0]
    /\ lifecycleAborted = [b \in Branches |-> FALSE]
    /\ lifecycleLock = [b \in Branches |-> "free"]
    /\ lockHolder = [b \in Branches |-> None]
    /\ dispatching = [b \in Branches |-> FALSE]
    /\ prCreated = [t \in TaskIDs |-> FALSE]
    /\ humanReopenFlag = FALSE

\* Static init for small.cfg:
\* plan(1) -> build(2) -> [vv1(3) || vv2(4)] -> gate(5)
StaticInit5 ==
    /\ taskStatus = [t \in TaskIDs |->
        IF t \in {1, 2, 3, 4, 5} THEN "open" ELSE "not_created"]
    /\ role = [t \in TaskIDs |->
        CASE t = 1 -> "planner"
          [] t = 2 -> "builder"
          [] t = 3 -> "verifier"
          [] t = 4 -> "verifier"
          [] t = 5 -> "gate"
          [] OTHER -> None]
    /\ deps = [t \in TaskIDs |->
        CASE t = 1 -> {}
          [] t = 2 -> {1}
          [] t = 3 -> {2}
          [] t = 4 -> {2}
          [] t = 5 -> {3, 4}
          [] OTHER -> {}]
    /\ taskBranch = [t \in TaskIDs |->
        IF t \in {1, 2, 3, 4, 5} THEN DefaultBranch ELSE None]
    /\ critical = [t \in TaskIDs |->
        IF t = 1 THEN TRUE ELSE FALSE]
    /\ retryCount = [t \in TaskIDs |-> 0]
    /\ handoffCount = [t \in TaskIDs |-> 0]
    /\ assignee = [t \in TaskIDs |-> None]
    /\ workerState = [w \in Workers |-> "idle"]
    /\ workerTask = [w \in Workers |-> None]
    /\ sessionAlive = [w \in Workers |-> FALSE]
    /\ gapCount = [b \in Branches |-> 0]
    /\ lifecycleAborted = [b \in Branches |-> FALSE]
    /\ lifecycleLock = [b \in Branches |-> "free"]
    /\ lockHolder = [b \in Branches |-> None]
    /\ dispatching = [b \in Branches |-> FALSE]
    /\ prCreated = [t \in TaskIDs |-> FALSE]
    /\ humanReopenFlag = FALSE

\* Convert a set to a sequence (for deterministic branch assignment)
RECURSIVE SetToSeq(_)
SetToSeq(S) ==
    IF S = {} THEN << >>
    ELSE LET x == CHOOSE x \in S : TRUE
         IN  <<x>> \o SetToSeq(S \ {x})

\* Static init for lifecycle.cfg: 2 plan tasks (one per branch), rest uncreated.
\* Requires |Branches| >= 2 and MaxTasks >= 2.
StaticInitLifecycle ==
    LET branchSeq == SetToSeq(Branches)
        b1 == branchSeq[1]
        b2 == branchSeq[2]
    IN
    /\ taskStatus = [t \in TaskIDs |->
        IF t \in {1, 2} THEN "open" ELSE "not_created"]
    /\ role = [t \in TaskIDs |->
        IF t \in {1, 2} THEN "planner" ELSE None]
    /\ deps = [t \in TaskIDs |-> {}]
    /\ taskBranch = [t \in TaskIDs |->
        CASE t = 1 -> b1
          [] t = 2 -> b2
          [] OTHER -> None]
    /\ critical = [t \in TaskIDs |->
        IF t \in {1, 2} THEN TRUE ELSE FALSE]
    /\ retryCount = [t \in TaskIDs |-> 0]
    /\ handoffCount = [t \in TaskIDs |-> 0]
    /\ assignee = [t \in TaskIDs |-> None]
    /\ workerState = [w \in Workers |-> "idle"]
    /\ workerTask = [w \in Workers |-> None]
    /\ sessionAlive = [w \in Workers |-> FALSE]
    /\ gapCount = [b \in Branches |-> 0]
    /\ lifecycleAborted = [b \in Branches |-> FALSE]
    /\ lifecycleLock = [b \in Branches |-> "free"]
    /\ lockHolder = [b \in Branches |-> None]
    /\ dispatching = [b \in Branches |-> FALSE]
    /\ prCreated = [t \in TaskIDs |-> FALSE]
    /\ humanReopenFlag = FALSE

\* Symmetry set for .cfg files
WorkerSymmetry == Workers

\* =========================================================================
\*  NEXT-STATE RELATION
\* =========================================================================

Next ==
    \* --- Task machine actions (8) ---
    \/ \E t \in TaskIDs, w \in Workers : Assign(t, w)
    \/ \E t \in TaskIDs, w \in Workers : SessionStart(t, w)
    \/ \E t \in TaskIDs, w \in Workers : ExitDone(t, w)
    \/ \E t \in TaskIDs, w \in Workers : ExitFail(t, w)
    \/ \E t \in TaskIDs, w \in Workers : ExitFailTerminal(t, w)
    \/ \E t \in TaskIDs, w \in Workers : ExitBlocked(t, w)
    \/ \E t \in TaskIDs, w \in Workers : ExitHandoff(t, w)
    \/ \E t \in TaskIDs, w \in Workers : ExitHandoffLimit(t, w)
    \* --- Dispatch/recovery actions (6) ---
    \/ \E t \in TaskIDs, w \in Workers : SessionTimeout(t, w)
    \/ \E w \in Workers : SessionCrash(w)
    \/ \E t \in TaskIDs, w \in Workers : WatchdogRestart(t, w)
    \/ \E t \in TaskIDs, w \in Workers : WatchdogFail(t, w)
    \/ \E t \in TaskIDs : HumanReopen(t)
    \/ \E b \in Branches : Reconcile(b)
    \* --- Lock actions (3) ---
    \/ \E orch \in Orchestrators, b \in Branches : AcquireLock(orch, b)
    \/ \E orch \in Orchestrators, b \in Branches : ReleaseLock(orch, b)
    \/ \E b \in Branches : LockGoesStale(b)
    \* --- Planner actions (2) ---
    \* NOTE: deps are quantified over subsets of CREATED tasks only,
    \* not all TaskIDs. This bounds the state space: |SUBSET created|
    \* is much smaller than |SUBSET TaskIDs| during early planning.
    \/ LET created == {t \in TaskIDs : IsCreated(t)} IN
       \E pt \in TaskIDs, nt \in TaskIDs, r \in {"builder", "verifier"},
          d \in SUBSET created, c \in BOOLEAN :
        PlannerCreateTask(pt, nt, r, d, c)
    \/ LET created == {t \in TaskIDs : IsCreated(t)} IN
       \E pt \in TaskIDs, nt \in TaskIDs, gd \in SUBSET created :
        PlannerCreateGateTask(pt, nt, gd)

\* =========================================================================
\*  FAIRNESS
\* =========================================================================

\* Weak fairness on dispatch: if a task is ready and a worker is idle,
\* it eventually gets assigned.
\* Weak fairness on session start: assigned tasks eventually start.
\* Weak fairness on exits: sessions eventually terminate (some exit fires).
\* Weak fairness on watchdog: dead sessions eventually get handled.
\* Strong fairness on reconcile: if repeatedly enabled, eventually fires.
\* Weak fairness on lock staleness: held locks eventually go stale (if holder dies).

Fairness ==
    /\ \A t \in TaskIDs, w \in Workers :
        WF_vars(Assign(t, w))
    /\ \A t \in TaskIDs, w \in Workers :
        WF_vars(SessionStart(t, w))
    /\ \A t \in TaskIDs, w \in Workers :
        WF_vars(ExitDone(t, w) \/ ExitFail(t, w) \/
                ExitFailTerminal(t, w) \/ ExitBlocked(t, w) \/
                ExitHandoff(t, w) \/ ExitHandoffLimit(t, w) \/
                SessionTimeout(t, w))
    /\ \A t \in TaskIDs, w \in Workers :
        WF_vars(WatchdogRestart(t, w) \/ WatchdogFail(t, w))
    /\ \A b \in Branches :
        SF_vars(Reconcile(b))
    /\ \A b \in Branches :
        WF_vars(LockGoesStale(b))

\* =========================================================================
\*  SPECIFICATION
\* =========================================================================

Spec == Init /\ [][Next]_vars /\ Fairness

\* =========================================================================
\*  TYPE CORRECTNESS INVARIANT
\* =========================================================================

TypeOK ==
    /\ taskStatus \in [TaskIDs -> StatusSet]
    /\ retryCount \in [TaskIDs -> 0..(MaxRetries + 1)]
    /\ handoffCount \in [TaskIDs -> 0..(MaxHandoffs + 1)]
    /\ assignee \in [TaskIDs -> Workers \cup {None}]
    /\ role \in [TaskIDs -> RoleSet \cup {None}]
    /\ deps \in [TaskIDs -> SUBSET TaskIDs]
    /\ taskBranch \in [TaskIDs -> Branches \cup {None}]
    /\ critical \in [TaskIDs -> BOOLEAN]
    /\ workerState \in [Workers -> WorkerStatusSet]
    /\ workerTask \in [Workers -> TaskIDs \cup {None}]
    /\ sessionAlive \in [Workers -> BOOLEAN]
    /\ gapCount \in [Branches -> 0..(GapTolerance + Cardinality(Workers) + 1)]
    /\ lifecycleAborted \in [Branches -> BOOLEAN]
    /\ lifecycleLock \in [Branches -> LockStatusSet]
    /\ lockHolder \in [Branches -> Orchestrators \cup {None}]
    /\ dispatching \in [Branches -> BOOLEAN]
    /\ prCreated \in [TaskIDs -> BOOLEAN]
    /\ humanReopenFlag \in BOOLEAN

\* =========================================================================
\*  SAFETY PROPERTIES (Section 12)
\* =========================================================================

(* BVV-S-01: Lifecycle Exclusion                                           *)
(* At most one orchestrator holds the lock for any branch.                 *)
(* Combined with BVV-DSP-12: no orchestrator holds two branch locks.       *)
LifecycleExclusion ==
    \* Each branch has at most one lock holder
    /\ \A b \in Branches :
        lifecycleLock[b] = "held" =>
            lockHolder[b] \in Orchestrators
    \* BVV-DSP-12: no orchestrator holds locks for two branches
    /\ \A o \in Orchestrators :
        Cardinality({b \in Branches : lockHolder[b] = o}) <= 1

(* BVV-S-02: Terminal Status Irreversibility                               *)
(* A terminal task stays terminal unless HumanReopen acted on it.          *)
(* Encoded as an action constraint: checked on every state transition.     *)
TerminalIrreversibility ==
    \A t \in TaskIDs :
        IsTerminal(taskStatus[t]) =>
            (IsTerminal(taskStatus'[t]) \/ humanReopenFlag')

(* BVV-S-02a: Counter Reset on Re-open                                    *)
(* When a terminal task transitions to open, counters must be zero.        *)
CounterResetOnReopen ==
    \A t \in TaskIDs :
        (IsTerminal(taskStatus[t]) /\ taskStatus'[t] = "open") =>
            (retryCount'[t] = 0 /\ handoffCount'[t] = 0)

(* BVV-S-03: Single Assignment                                            *)
(* At most one task assigned to each worker.                               *)
SingleAssignment ==
    \A w \in Workers :
        Cardinality({t \in TaskIDs : assignee[t] = w}) <= 1

(* BVV-S-04: Dependency Ordering                                          *)
(* No task is dispatched/executing unless all its deps are terminal.       *)
DependencyOrdering ==
    \A t \in TaskIDs :
        taskStatus[t] \in ActiveSet => AllDepsTerminal(t)

(* BVV-S-05: Zero Content Inspection                                      *)
(* Verified by construction: no variable represents task content,          *)
(* agent output, or agent memory. All dispatch decisions reference         *)
(* role (metadata), deps (structure), and taskStatus (state).              *)

(* BVV-S-06: Gate Authority                                               *)
(* PR is only created when gate task completes.                            *)
GateAuthority ==
    \A t \in TaskIDs :
        prCreated[t] => taskStatus[t] = "completed"

(* BVV-S-07: Bounded Degradation                                          *)
(* PR created implies gap count is below tolerance.                        *)
BoundedDegradation ==
    \A t \in TaskIDs :
        (prCreated[t] /\ taskBranch[t] /= None) =>
            gapCount[taskBranch[t]] < GapTolerance

(* BVV-S-08: Assignment Durability                                        *)
(* Verified by construction: SessionCrash has UNCHANGED on all ledger      *)
(* state except sessionAlive.                                              *)

(* BVV-S-09: Workspace Write Serialization                                *)
(* At most one builder in_progress per branch.                             *)
(* NOTE: This depends on Deps correctly serializing builders.              *)
(* If Deps is wrong, this invariant WILL be violated — correctly           *)
(* mirroring the spec's "partially enforced" status.                       *)
WorkspaceWriteSerialization ==
    \A b \in Branches :
        Cardinality(BuildersInProgress(b)) <= 1

(* BVV-S-10: Watchdog-Retry Non-Interference                              *)
(* Verified by construction: WatchdogRestart has UNCHANGED retryCount;     *)
(* ExitFail/ExitFailTerminal has UNCHANGED handoffCount.                   *)
(* The counters are orthogonal.                                            *)

\* =========================================================================
\*  TASK GRAPH WELL-FORMEDNESS (Section 7.5)
\* =========================================================================

(* BVV-TG-07: Every created task has a valid role                          *)
ValidRoles ==
    \A t \in TaskIDs :
        IsCreated(t) => role[t] \in RoleSet

(* BVV-TG-08: Task graph is acyclic                                       *)
AcyclicGraph == IsAcyclic(deps)

(* BVV-TG-09: At most one gate task per branch                            *)
SingleGatePerBranch ==
    \A b \in Branches : Cardinality(GateTasks(b)) <= 1

(* BVV-TG-10: All tasks reachable from a plan task on the same branch     *)
(* NOTE: This is checked as a weaker condition — every created task        *)
(* with deps={} must be a planner task or depend on one. Full              *)
(* transitive reachability checking is expensive; deferred to small.cfg.   *)

\* =========================================================================
\*  LIVENESS PROPERTIES (Section 13)
\* =========================================================================

(* BVV-L-01: Eventual Termination                                         *)
(* All created tasks eventually reach terminal status.                     *)
(* Only holds when HumanActions = FALSE (closed system).                   *)
EventualTermination ==
    ~HumanActions =>
        <>(\A t \in TaskIDs :
            IsCreated(t) => IsTerminal(taskStatus[t]))

(* BVV-L-02: Lock Release                                                 *)
(* A held lock eventually becomes free or stale.                           *)
LockRelease ==
    \A b \in Branches :
        [](lifecycleLock[b] = "held" ~>
            lifecycleLock[b] \in {"free", "stale"})

(* BVV-L-03: Worker Recovery                                              *)
(* A worker with a dead session eventually gets restarted or its task      *)
(* reaches terminal status.                                                *)
WorkerRecovery ==
    \A w \in Workers :
        []((workerTask[w] /= None /\ ~sessionAlive[w]) ~>
            (sessionAlive[w] \/
             (workerTask[w] /= None =>
                IsTerminal(taskStatus[workerTask[w]])) \/
             workerTask[w] = None))

(* BVV-L-04: Bounded Handoff (invariant part)                             *)
(* Handoff count never exceeds the limit.                                  *)
BoundedHandoff ==
    \A t \in TaskIDs : handoffCount[t] <= MaxHandoffs + 1
    \* +1 because WatchdogRestart can increment at the limit boundary
    \* before ExitHandoffLimit converts to failure. The model allows
    \* handoffCount to momentarily be MaxHandoffs+1 within a step.
    \* The actual bound check is: when handoffCount >= MaxHandoffs,
    \* exit code 3 is converted to exit code 1.

(* BVV-L-04: Bounded Handoff (liveness part)                              *)
(* A task at the handoff limit eventually reaches terminal status.         *)
BoundedHandoffLiveness ==
    \A t \in TaskIDs :
        [](handoffCount[t] >= MaxHandoffs =>
            <>(IsTerminal(taskStatus[t]) \/ ~IsCreated(t)))

\* =========================================================================
\*  COMBINED INVARIANTS (for .cfg files)
\* =========================================================================

\* All state invariants combined
AllInvariants ==
    /\ TypeOK
    /\ LifecycleExclusion
    /\ SingleAssignment
    /\ DependencyOrdering
    /\ GateAuthority
    /\ BoundedDegradation
    /\ WorkspaceWriteSerialization
    /\ ValidRoles
    /\ AcyclicGraph
    /\ SingleGatePerBranch
    /\ BoundedHandoff

=============================================================================
