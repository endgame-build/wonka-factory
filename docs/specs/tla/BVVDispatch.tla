----------------------------- MODULE BVVDispatch ----------------------------
(***************************************************************************)
(* Orchestrator dispatch, crash recovery, reconciliation, and locking.     *)
(*                                                                         *)
(* Actions:                                                                *)
(*   SessionTimeout, SessionCrash, WatchdogRestart, WatchdogFail,          *)
(*   HumanReopen, Reconcile, AcquireLock, ReleaseLock, LockGoesStale       *)
(*                                                                         *)
(* Spec reference: BUILD_VERIFY_VALIDATE_SPEC.md Sections 8.5, 9, 11, 11a *)
(***************************************************************************)
EXTENDS BVVTaskMachine

\* --------------------------------------------------------------------------
\* Action: SessionTimeout (BVV-ERR-02a)
\* --------------------------------------------------------------------------
\* Orchestrator enforces base session timeout. Treated as exit code 1.
\* Nondeterministic: can fire at any point while session is alive.
\* This is a sound overapproximation of the real timed behavior.

SessionTimeout(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = TRUE
    \* Treated as exit code 1 — same as ExitFail or ExitFailTerminal
    /\ IF retryCount[t] < MaxRetries
       THEN /\ taskStatus' = [taskStatus EXCEPT ![t] = "open"]
            /\ retryCount' = [retryCount EXCEPT ![t] = retryCount[t] + 1]
            /\ ClearAssignee(t)
            /\ ReleaseWorker(w)
            /\ NoGapUpdate
       ELSE /\ taskStatus' = [taskStatus EXCEPT ![t] = "failed"]
            /\ retryCount' = retryCount
            /\ ClearAssignee(t)
            /\ ReleaseWorker(w)
            /\ UpdateGap(t)
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<handoffCount, role, deps, taskBranch, critical,
                   lifecycleLock, lockHolder, dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action: SessionCrash (environmental fault)
\* --------------------------------------------------------------------------
\* A tmux session dies unexpectedly. Only sessionAlive changes.
\* BVV-S-08: Assignment durability — crash preserves all ledger state.

SessionCrash(w) ==
    /\ sessionAlive[w] = TRUE
    /\ sessionAlive' = [sessionAlive EXCEPT ![w] = FALSE]
    /\ humanReopenFlag' = FALSE
    \* BVV-S-08: UNCHANGED everything else
    /\ UNCHANGED <<taskStatus, retryCount, handoffCount, assignee, role,
                   deps, taskBranch, critical, workerState, workerTask,
                   gapCount, lifecycleAborted, lifecycleLock, lockHolder,
                   dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action: WatchdogRestart (BVV-ERR-11a, handoff budget remains)
\* --------------------------------------------------------------------------
\* Watchdog detects dead session, restarts it. Counts as handoff.
\* BVV-S-10: increments handoffCount, NOT retryCount.

WatchdogRestart(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = FALSE   \* Session is dead
    /\ workerTask[w] = t
    /\ handoffCount[t] < MaxHandoffs
    \* Postcondition: restart session, increment handoff
    /\ sessionAlive' = [sessionAlive EXCEPT ![w] = TRUE]
    /\ handoffCount' = [handoffCount EXCEPT ![t] = handoffCount[t] + 1]
    /\ humanReopenFlag' = FALSE
    \* Task stays in_progress, worker stays busy
    /\ UNCHANGED <<taskStatus, retryCount, assignee, role, deps,
                   taskBranch, critical, workerState, workerTask,
                   gapCount, lifecycleAborted, lifecycleLock, lockHolder,
                   dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action: WatchdogFail (BVV-ERR-11a, handoff limit reached)
\* --------------------------------------------------------------------------
\* Watchdog detects dead session but handoff budget exhausted.
\* Treated as exit code 1.

WatchdogFail(t, w) ==
    /\ taskStatus[t] = "in_progress"
    /\ assignee[t] = w
    /\ sessionAlive[w] = FALSE
    /\ workerTask[w] = t
    /\ handoffCount[t] >= MaxHandoffs
    \* Treated as exit code 1
    /\ IF retryCount[t] < MaxRetries
       THEN /\ taskStatus' = [taskStatus EXCEPT ![t] = "open"]
            /\ retryCount' = [retryCount EXCEPT ![t] = retryCount[t] + 1]
            /\ ClearAssignee(t)
            /\ ReleaseWorker(w)
            /\ NoGapUpdate
       ELSE /\ taskStatus' = [taskStatus EXCEPT ![t] = "failed"]
            /\ retryCount' = retryCount
            /\ ClearAssignee(t)
            /\ ReleaseWorker(w)
            /\ UpdateGap(t)
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<handoffCount, role, deps, taskBranch, critical,
                   lifecycleLock, lockHolder, dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action: HumanReopen (BVV-S-02a)
\* --------------------------------------------------------------------------
\* Human operator re-opens a terminal task via CLI.
\* External action — not an orchestrator action. Resets counters.

HumanReopen(t) ==
    /\ HumanActions                        \* Toggle for model checking
    /\ IsTerminal(taskStatus[t])
    /\ IsCreated(t)
    \* Postcondition: reset to open with fresh budgets
    /\ taskStatus' = [taskStatus EXCEPT ![t] = "open"]
    /\ retryCount' = [retryCount EXCEPT ![t] = 0]
    /\ handoffCount' = [handoffCount EXCEPT ![t] = 0]
    /\ assignee' = [assignee EXCEPT ![t] = None]
    /\ humanReopenFlag' = TRUE             \* Signal for BVV-S-02 constraint
    /\ UNCHANGED <<role, deps, taskBranch, critical,
                   workerState, workerTask, sessionAlive, gapCount,
                   lifecycleAborted, lifecycleLock, lockHolder,
                   dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action: Reconcile (Section 11a.2, BVV-ERR-07, BVV-ERR-08)
\* --------------------------------------------------------------------------
\* Per-branch: reset stale assignments, then enable dispatch.
\* BVV-ERR-07: no dispatch during reconciliation.
\* BVV-ERR-08: don't reset tasks with live sessions.

Reconcile(b) ==
    /\ lifecycleLock[b] = "held"
    /\ dispatching[b] = FALSE              \* BVV-ERR-07
    \* Stale assignment reset (11a.2 step 1):
    \* Tasks that are assigned/in_progress but have no live session
    /\ LET staleTasks ==
            {t \in TaskIDs :
                /\ taskStatus[t] \in {"assigned", "in_progress"}
                /\ taskBranch[t] = b
                /\ assignee[t] /= None
                /\ sessionAlive[assignee[t]] = FALSE}
       IN
       /\ taskStatus' = [t \in TaskIDs |->
            IF t \in staleTasks
            THEN "open"
            ELSE taskStatus[t]]
       /\ assignee' = [t \in TaskIDs |->
            IF t \in staleTasks
            THEN None
            ELSE assignee[t]]
       \* Release workers for stale tasks
       /\ LET staleWorkers ==
                {assignee[t] : t \in staleTasks} \ {None}
          IN
          /\ workerState' = [w \in Workers |->
                IF w \in staleWorkers
                THEN "idle"
                ELSE workerState[w]]
          /\ workerTask' = [w \in Workers |->
                IF w \in staleWorkers
                THEN None
                ELSE workerTask[w]]
    \* Enable dispatching after reconciliation
    /\ dispatching' = [dispatching EXCEPT ![b] = TRUE]
    /\ humanReopenFlag' = FALSE
    \* Reconciliation is idempotent. No counter changes.
    /\ UNCHANGED <<retryCount, handoffCount, role, deps, taskBranch,
                   critical, sessionAlive, gapCount, lifecycleAborted,
                   lifecycleLock, lockHolder, prCreated>>

\* --------------------------------------------------------------------------
\* Action: AcquireLock (BVV-ERR-06, BVV-S-01)
\* --------------------------------------------------------------------------
\* Orchestrator acquires lifecycle lock for a branch.
\* Can acquire if free or if stale (previous holder assumed dead).

AcquireLock(orch, b) ==
    /\ lifecycleLock[b] \in {"free", "stale"}
    \* No orchestrator holds two branch locks simultaneously
    /\ \A b2 \in Branches : b2 /= b => lockHolder[b2] /= orch
    /\ lifecycleLock' = [lifecycleLock EXCEPT ![b] = "held"]
    /\ lockHolder' = [lockHolder EXCEPT ![b] = orch]
    /\ dispatching' = [dispatching EXCEPT ![b] = FALSE]  \* Must reconcile first
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<taskStatus, retryCount, handoffCount, assignee, role,
                   deps, taskBranch, critical, workerState, workerTask,
                   sessionAlive, gapCount, lifecycleAborted, prCreated>>

\* --------------------------------------------------------------------------
\* Action: ReleaseLock (BVV-ERR-10)
\* --------------------------------------------------------------------------
\* Orchestrator releases lifecycle lock. BVV-ERR-09: no status modification.

ReleaseLock(orch, b) ==
    /\ lifecycleLock[b] = "held"
    /\ lockHolder[b] = orch
    \* Section 8.1.3 + BVV-ERR-09: orchestrator releases lock only when
    \* all tasks are terminal (lifecycle complete) or lifecycle is aborted
    \* and no active sessions remain.
    /\ ~\E w \in Workers :
        /\ sessionAlive[w] = TRUE
        /\ workerTask[w] /= None
        /\ taskBranch[workerTask[w]] = b
    /\ \/ lifecycleAborted[b] = TRUE
       \/ \A t \in TaskIDs :
            IsCreated(t) /\ taskBranch[t] = b => IsTerminal(taskStatus[t])
    /\ lifecycleLock' = [lifecycleLock EXCEPT ![b] = "free"]
    /\ lockHolder' = [lockHolder EXCEPT ![b] = None]
    /\ dispatching' = [dispatching EXCEPT ![b] = FALSE]
    /\ humanReopenFlag' = FALSE
    \* BVV-ERR-09: UNCHANGED all task state
    /\ UNCHANGED <<taskStatus, retryCount, handoffCount, assignee, role,
                   deps, taskBranch, critical, workerState, workerTask,
                   sessionAlive, gapCount, lifecycleAborted, prCreated>>

\* --------------------------------------------------------------------------
\* Action: LockGoesStale (BVV-L-02)
\* --------------------------------------------------------------------------
\* Models the passage of time: a held lock exceeds staleness threshold.
\* Enables a new orchestrator to acquire it.

LockGoesStale(b) ==
    /\ lifecycleLock[b] = "held"
    \* Lock staleness models the holder crashing. An active orchestrator
    \* (dispatching=TRUE) refreshes the lock — it only goes stale when
    \* the holder is dead (stopped dispatching).
    /\ dispatching[b] = FALSE
    /\ lifecycleLock' = [lifecycleLock EXCEPT ![b] = "stale"]
    \* Lock holder is now dead — they don't know the lock is stale
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<taskStatus, retryCount, handoffCount, assignee, role,
                   deps, taskBranch, critical, workerState, workerTask,
                   sessionAlive, gapCount, lifecycleAborted, lockHolder,
                   dispatching, prCreated>>

\* --------------------------------------------------------------------------
\* Action: AbortCleanup (implied by BVV-ERR-04 + Section 8.1.3)
\* --------------------------------------------------------------------------
\* When lifecycle is aborted, mark all remaining open tasks on that branch
\* as blocked. Without this, undispatched tasks are stranded and
\* EventualTermination (BVV-L-01) is violated.
\*
\* Justification: Section 8.1.3 requires "ALL tasks in scope terminal" for
\* dispatch loop termination. BVV-ERR-04 stops dispatch on abort. This
\* action bridges the gap — the orchestrator terminates stranded tasks
\* so the dispatch loop can exit cleanly.

AbortCleanup(b) ==
    /\ lifecycleAborted[b] = TRUE
    /\ lifecycleLock[b] = "held"
    \* At least one open task exists on this branch
    /\ \E t \in TaskIDs :
        /\ taskStatus[t] = "open"
        /\ taskBranch[t] = b
    \* Mark all open tasks on this branch as blocked
    /\ taskStatus' = [t \in TaskIDs |->
        IF taskStatus[t] = "open" /\ taskBranch[t] = b
        THEN "blocked"
        ELSE taskStatus[t]]
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<retryCount, handoffCount, assignee, role, deps,
                   taskBranch, critical, workerState, workerTask,
                   sessionAlive, gapCount, lifecycleAborted,
                   lifecycleLock, lockHolder, dispatching, prCreated>>

=============================================================================
