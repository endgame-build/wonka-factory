--------------------------- MODULE BVVTaskMachine ---------------------------
(***************************************************************************)
(* Per-task state machine: status transitions, retry, handoff, gap mgmt.   *)
(*                                                                         *)
(* 8 actions model all task-level transitions from the BVV spec:           *)
(*   Assign, SessionStart, ExitDone, ExitFail, ExitFailTerminal,           *)
(*   ExitBlocked, ExitHandoff, ExitHandoffLimit                            *)
(*                                                                         *)
(* Spec reference: BUILD_VERIFY_VALIDATE_SPEC.md Sections 7.1, 8.3, 11    *)
(***************************************************************************)
EXTENDS BVVTypes

VARIABLES
    taskStatus,     \* [TaskIDs -> StatusSet]
    retryCount,     \* [TaskIDs -> 0..MaxRetries+1]
    handoffCount,   \* [TaskIDs -> 0..MaxHandoffs+1]
    assignee,       \* [TaskIDs -> Workers \cup {None}]
    role,           \* [TaskIDs -> RoleSet \cup {None}]
    deps,           \* [TaskIDs -> SUBSET TaskIDs]
    taskBranch,     \* [TaskIDs -> Branches \cup {None}]
    critical,       \* [TaskIDs -> BOOLEAN]

    \* System-level variables (declared here for UNCHANGED clauses)
    workerState,    \* [Workers -> WorkerStatusSet]
    workerTask,     \* [Workers -> TaskIDs \cup {None}]
    sessionAlive,   \* [Workers -> BOOLEAN]
    gapCount,       \* [Branches -> Nat]
    lifecycleAborted, \* [Branches -> BOOLEAN]
    lifecycleLock,  \* [Branches -> LockStatusSet]
    lockHolder,     \* [Branches -> Orchestrators \cup {None}]
    dispatching,    \* [Branches -> BOOLEAN]
    prCreated,      \* [TaskIDs -> BOOLEAN]
    humanReopenFlag \* BOOLEAN — auxiliary for BVV-S-02 action constraint

\* Tuple of all task-level variables
taskVars == <<taskStatus, retryCount, handoffCount, assignee,
              role, deps, taskBranch, critical>>

\* Tuple of all system-level variables
sysVars == <<workerState, workerTask, sessionAlive, gapCount,
             lifecycleAborted, lifecycleLock, lockHolder, dispatching,
             prCreated, humanReopenFlag>>

\* Tuple of all variables
vars == <<taskStatus, retryCount, handoffCount, assignee,
          role, deps, taskBranch, critical,
          workerState, workerTask, sessionAlive, gapCount,
          lifecycleAborted, lifecycleLock, lockHolder, dispatching,
          prCreated, humanReopenFlag>>

\* --------------------------------------------------------------------------
\* Helper operators (require variables in scope)
\* --------------------------------------------------------------------------

\* Task has been created (BVV-TG-02)
IsCreated(t) == taskStatus[t] /= "not_created"

\* All dependencies of t have reached terminal status (Section 7.1)
AllDepsTerminal(t) ==
    \A d \in deps[t] : IsTerminal(taskStatus[d])

\* All dependencies of t completed (not just terminal) — for gate (BVV-GT-03)
AllDepsCompleted(t) ==
    \A d \in deps[t] : taskStatus[d] = "completed"

\* Tasks ready for dispatch on a given branch (Section 5.2, 8.1)
ReadyTasks(b) ==
    {t \in TaskIDs : /\ taskStatus[t] = "open"
                     /\ taskBranch[t] = b
                     /\ AllDepsTerminal(t)
                     /\ assignee[t] = None}

\* Idle workers available for assignment
IdleWorkers == {w \in Workers : workerState[w] = "idle"}

\* Builder tasks currently in_progress on a branch (for BVV-S-09)
BuildersInProgress(b) ==
    {t \in TaskIDs : /\ role[t] = "builder"
                     /\ taskBranch[t] = b
                     /\ taskStatus[t] = "in_progress"}

\* Gate tasks on a branch (for BVV-TG-09)
GateTasks(b) ==
    {t \in TaskIDs : /\ role[t] = "gate"
                     /\ taskBranch[t] = b
                     /\ IsCreated(t)}

\* --------------------------------------------------------------------------
\* Gap management (Section 11.3, BVV-ERR-03, BVV-ERR-04)
\* --------------------------------------------------------------------------

\* Update gap counter when a task reaches terminal failure.
\* Critical tasks abort immediately; non-critical increment the counter.
UpdateGap(t) ==
    LET b == taskBranch[t] IN
    IF critical[t]
    THEN \* BVV-ERR-03: critical failure → immediate lifecycle abort
         /\ lifecycleAborted' = [lifecycleAborted EXCEPT ![b] = TRUE]
         /\ UNCHANGED gapCount
    ELSE \* BVV-ERR-04: non-critical → increment gap, abort if threshold
         LET newGap == gapCount[b] + 1 IN
         /\ gapCount' = [gapCount EXCEPT ![b] = newGap]
         /\ lifecycleAborted' =
                [lifecycleAborted EXCEPT ![b] = newGap >= GapTolerance]

\* No gap update (used in non-failure exit actions)
NoGapUpdate ==
    /\ UNCHANGED gapCount
    /\ UNCHANGED lifecycleAborted

\* --------------------------------------------------------------------------
\* Worker release helper
\* --------------------------------------------------------------------------

ReleaseWorker(w) ==
    /\ workerState' = [workerState EXCEPT ![w] = "idle"]
    /\ workerTask' = [workerTask EXCEPT ![w] = None]
    /\ sessionAlive' = [sessionAlive EXCEPT ![w] = FALSE]

ClearAssignee(t) ==
    assignee' = [assignee EXCEPT ![t] = None]

\* --------------------------------------------------------------------------
\* Action 1: Assign (BVV-DSP-01, LDG-08)
\* --------------------------------------------------------------------------
\* Orchestrator assigns a ready task to an idle worker.
\* Precondition: task open, deps terminal, worker idle, lifecycle not aborted,
\*               dispatching enabled for this branch.

Assign(t, w) ==
    LET b == taskBranch[t] IN
    /\ taskStatus[t] = "open"
    /\ AllDepsTerminal(t)
    /\ assignee[t] = None
    /\ workerState[w] = "idle"
    /\ lifecycleAborted[b] = FALSE
    /\ dispatching[b] = TRUE
    /\ lifecycleLock[b] = "held"
    \* Postcondition
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "assigned"]
    /\ assignee' = [assignee EXCEPT ![t] = w]
    /\ workerState' = [workerState EXCEPT ![w] = "busy"]
    /\ workerTask' = [workerTask EXCEPT ![w] = t]
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<retryCount, handoffCount, role, deps, taskBranch,
                   critical, sessionAlive, gapCount, lifecycleAborted,
                   lifecycleLock, lockHolder, dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action 2: SessionStart (LDG-14a)
\* --------------------------------------------------------------------------
\* Worker session begins executing the assigned task.

SessionStart(t, w) ==
    /\ taskStatus[t] = "assigned"
    /\ assignee[t] = w
    /\ workerState[w] = "busy"
    \* Postcondition
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "in_progress"]
    /\ sessionAlive' = [sessionAlive EXCEPT ![w] = TRUE]
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<retryCount, handoffCount, assignee, role, deps,
                   taskBranch, critical, workerState, workerTask,
                   gapCount, lifecycleAborted, lifecycleLock, lockHolder,
                   dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action 3: ExitDone — exit code 0 (Section 8.3.1)
\* --------------------------------------------------------------------------
\* Agent completed the task successfully.
\* For gate tasks: requires AllDepsCompleted (BVV-GT-03).

ExitDone(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    \* Gate-specific: all deps must be completed, not just terminal (BVV-GT-03)
    /\ IF role[t] = "gate"
       THEN AllDepsCompleted(t)
       ELSE TRUE
    \* Postcondition
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "completed"]
    /\ ClearAssignee(t)
    /\ ReleaseWorker(w)
    \* If gate task, mark PR as created (BVV-S-06, BVV-S-07)
    /\ prCreated' = IF role[t] = "gate"
                    THEN [prCreated EXCEPT ![t] = TRUE]
                    ELSE prCreated
    /\ humanReopenFlag' = FALSE
    /\ NoGapUpdate
    /\ UNCHANGED <<retryCount, handoffCount, role, deps, taskBranch,
                   critical, lifecycleLock, lockHolder, dispatching>>

\* --------------------------------------------------------------------------
\* Action 4: ExitFail — exit code 1, retries remain (Section 11.1)
\* --------------------------------------------------------------------------
\* Agent failed but retry budget allows re-dispatch.
\* Task resets to open (non-terminal cycle).

ExitFail(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    /\ retryCount[t] < MaxRetries
    \* Postcondition: reset to open for re-dispatch
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "open"]
    /\ retryCount' = [retryCount EXCEPT ![t] = retryCount[t] + 1]
    /\ ClearAssignee(t)
    /\ ReleaseWorker(w)
    /\ humanReopenFlag' = FALSE
    /\ NoGapUpdate
    \* BVV-S-10: handoffCount NOT changed by retry
    /\ UNCHANGED <<handoffCount, role, deps, taskBranch, critical,
                   lifecycleLock, lockHolder, dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action 5: ExitFailTerminal — exit code 1, retries exhausted (Section 11.1)
\* --------------------------------------------------------------------------
\* Agent failed and no retries remain. Task enters terminal failed status.

ExitFailTerminal(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    /\ retryCount[t] >= MaxRetries
    \* Postcondition
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "failed"]
    /\ ClearAssignee(t)
    /\ ReleaseWorker(w)
    /\ UpdateGap(t)
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<retryCount, handoffCount, role, deps, taskBranch,
                   critical, lifecycleLock, lockHolder, dispatching,
                   prCreated>>

\* --------------------------------------------------------------------------
\* Action 6: ExitBlocked — exit code 2 (Section 11.2)
\* --------------------------------------------------------------------------
\* Agent determined the task is blocked. Terminal, no retry.

ExitBlocked(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    \* Postcondition
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "blocked"]
    /\ ClearAssignee(t)
    /\ ReleaseWorker(w)
    /\ UpdateGap(t)
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<retryCount, handoffCount, role, deps, taskBranch,
                   critical, lifecycleLock, lockHolder, dispatching,
                   prCreated>>

\* --------------------------------------------------------------------------
\* Action 7: ExitHandoff — exit code 3, within limit (BVV-DSP-14, BVV-L-04)
\* --------------------------------------------------------------------------
\* Agent signals context pressure. New session spawned, same task.
\* Task status stays in_progress. Handoff count incremented.

ExitHandoff(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    /\ handoffCount[t] < MaxHandoffs
    \* Postcondition: task stays in_progress (BVV-DSP-14)
    /\ UNCHANGED taskStatus
    /\ handoffCount' = [handoffCount EXCEPT ![t] = handoffCount[t] + 1]
    \* Old session dies, new session spawns atomically.
    \* Modeled as sessionAlive staying TRUE — the orchestrator spawns
    \* the replacement session immediately (Section 10.1 step 3).
    \* If we set sessionAlive=FALSE, the watchdog would fire and
    \* double-count the handoff. The atomic respawn avoids this.
    /\ UNCHANGED sessionAlive
    \* Worker stays busy, same task
    /\ humanReopenFlag' = FALSE
    \* BVV-S-10: retryCount NOT changed by handoff
    /\ UNCHANGED <<retryCount, assignee, role, deps, taskBranch, critical,
                   workerState, workerTask, gapCount, lifecycleAborted,
                   lifecycleLock, lockHolder, dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action 8: ExitHandoffLimit — exit code 3 at limit (BVV-L-04)
\* --------------------------------------------------------------------------
\* Agent signals handoff but limit reached. Convert to exit code 1.
\* Delegates to ExitFail or ExitFailTerminal depending on retry budget.

ExitHandoffLimit(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    /\ handoffCount[t] >= MaxHandoffs
    \* Converted to exit code 1 behavior:
    /\ IF retryCount[t] < MaxRetries
       THEN \* Retries remain: reset to open (like ExitFail)
            /\ taskStatus' = [taskStatus EXCEPT ![t] = "open"]
            /\ retryCount' = [retryCount EXCEPT ![t] = retryCount[t] + 1]
            /\ ClearAssignee(t)
            /\ ReleaseWorker(w)
            /\ NoGapUpdate
            /\ humanReopenFlag' = FALSE
            /\ UNCHANGED <<handoffCount, role, deps, taskBranch, critical,
                           lifecycleLock, lockHolder, dispatching, prCreated>>
       ELSE \* Retries exhausted: terminal failure (like ExitFailTerminal)
            /\ taskStatus' = [taskStatus EXCEPT ![t] = "failed"]
            /\ retryCount' = retryCount
            /\ ClearAssignee(t)
            /\ ReleaseWorker(w)
            /\ UpdateGap(t)
            /\ humanReopenFlag' = FALSE
            /\ UNCHANGED <<handoffCount, role, deps, taskBranch, critical,
                           lifecycleLock, lockHolder, dispatching, prCreated>>

=============================================================================
