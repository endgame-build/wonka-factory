---------------------------- MODULE BVVLifecycle ----------------------------
(***************************************************************************)
(* Dynamic planning agent, task graph creation, concurrent lifecycles.      *)
(*                                                                         *)
(* Models the Level 2 behavior: planner decomposes work packages into      *)
(* task graphs with build, V&V, and gate tasks.                            *)
(*                                                                         *)
(* Actions:                                                                *)
(*   PlannerCreateTask, PlannerCreateGateTask, PlannerFinish               *)
(*                                                                         *)
(* Spec reference: BUILD_VERIFY_VALIDATE_SPEC.md Sections 7.3-7.5         *)
(***************************************************************************)
EXTENDS BVVDispatch

\* --------------------------------------------------------------------------
\* Planner state: which planner tasks have finished planning
\* --------------------------------------------------------------------------

\* A planner task is "planning" when it's in_progress and its role is planner.
\* It becomes "done planning" when it exits (ExitDone transitions it to completed).
\* During planning, it creates tasks via the actions below.

IsPlannerActive(t) ==
    /\ taskStatus[t] = "in_progress"
    /\ role[t] = "planner"

\* --------------------------------------------------------------------------
\* Helper: find an unused task slot
\* --------------------------------------------------------------------------

UnusedTaskSlots == {t \in TaskIDs : taskStatus[t] = "not_created"}

\* --------------------------------------------------------------------------
\* Action: PlannerCreateTask (BVV-TG-01, BVV-TG-02, BVV-TG-03)
\* --------------------------------------------------------------------------
\* Active planner creates a build or verifier task.
\* The planner picks an unused task slot and assigns role, deps, branch.
\*
\* BVV-TG-02: idempotent — only creates if slot is "not_created"
\* BVV-TG-03: cannot modify in_progress or completed tasks
\* BVV-TG-08: deps must not create cycles (checked via precondition)

PlannerCreateTask(plannerTask, newTask, newRole, newDeps, isCritical) ==
    /\ IsPlannerActive(plannerTask)
    /\ taskStatus[newTask] = "not_created"       \* BVV-TG-02: unused slot
    /\ newRole \in {"builder", "verifier"}        \* Only build/VV tasks
    /\ newDeps \subseteq TaskIDs                  \* Valid dependency set
    \* All deps must be created tasks (can't depend on uncreated)
    /\ \A d \in newDeps : taskStatus[d] /= "not_created"
    \* BVV-TG-08: adding this task must not create a cycle
    /\ LET proposedDeps == [deps EXCEPT ![newTask] = newDeps]
       IN  newTask \notin ReachableFrom(newTask, proposedDeps, {})
    \* BVV-TG-03: don't touch in_progress or completed tasks
    \* (ensured because newTask is "not_created")
    \* Postcondition: create the task
    /\ taskStatus' = [taskStatus EXCEPT ![newTask] = "open"]
    /\ role' = [role EXCEPT ![newTask] = newRole]
    /\ deps' = [deps EXCEPT ![newTask] = newDeps]
    /\ taskBranch' = [taskBranch EXCEPT ![newTask] = taskBranch[plannerTask]]
    /\ critical' = [critical EXCEPT ![newTask] = isCritical]
    /\ assignee' = [assignee EXCEPT ![newTask] = None]
    /\ retryCount' = [retryCount EXCEPT ![newTask] = 0]
    /\ handoffCount' = [handoffCount EXCEPT ![newTask] = 0]
    /\ prCreated' = [prCreated EXCEPT ![newTask] = FALSE]
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<workerState, workerTask, sessionAlive, gapCount,
                   lifecycleAborted, lifecycleLock, lockHolder, dispatching>>

\* --------------------------------------------------------------------------
\* Action: PlannerCreateGateTask (BVV-TG-09)
\* --------------------------------------------------------------------------
\* Active planner creates the single gate task for this lifecycle.
\* BVV-TG-09: exactly one gate per lifecycle.

PlannerCreateGateTask(plannerTask, newTask, gateDeps) ==
    LET b == taskBranch[plannerTask] IN
    /\ IsPlannerActive(plannerTask)
    /\ taskStatus[newTask] = "not_created"
    \* BVV-TG-09: no gate task already exists for this branch
    /\ Cardinality(GateTasks(b)) = 0
    /\ gateDeps \subseteq TaskIDs
    /\ \A d \in gateDeps : taskStatus[d] /= "not_created"
    \* Gate should depend on V&V tasks (not enforced structurally — planner's job)
    \* Postcondition
    /\ taskStatus' = [taskStatus EXCEPT ![newTask] = "open"]
    /\ role' = [role EXCEPT ![newTask] = "gate"]
    /\ deps' = [deps EXCEPT ![newTask] = gateDeps]
    /\ taskBranch' = [taskBranch EXCEPT ![newTask] = b]
    /\ critical' = [critical EXCEPT ![newTask] = FALSE]  \* Gate is not critical
    /\ assignee' = [assignee EXCEPT ![newTask] = None]
    /\ retryCount' = [retryCount EXCEPT ![newTask] = 0]
    /\ handoffCount' = [handoffCount EXCEPT ![newTask] = 0]
    /\ prCreated' = [prCreated EXCEPT ![newTask] = FALSE]
    /\ humanReopenFlag' = FALSE
    /\ UNCHANGED <<workerState, workerTask, sessionAlive, gapCount,
                   lifecycleAborted, lifecycleLock, lockHolder, dispatching>>

\* --------------------------------------------------------------------------
\* Note: PlannerFinish is modeled by ExitDone(plannerTask, w)
\* --------------------------------------------------------------------------
\* The planner completes by exiting with code 0, which transitions the
\* planner task to "completed" via the standard ExitDone action.
\* No separate PlannerFinish action is needed — it's just ExitDone.

=============================================================================
