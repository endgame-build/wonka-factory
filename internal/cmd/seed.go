package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/endgame/wonka-factory/orch"
)

// SeedPlannerTask ensures a planner task exists for the given branch with a
// body pointing at workOrderAbs and a hash label matching the current work-
// order content. Idempotent: if a matching task already exists in the right
// state, this is a no-op.
//
// Decision matrix:
//
//	existing state             hash match  action
//	-------------------------  ----------  ------------------
//	not found                  —           CreateTask(seed)
//	open / assigned / in_prog  any         no-op (already in flight)
//	completed/failed/blocked   match       no-op (nothing to do)
//	completed/failed/blocked   differ      reopen + update body+hash
//
// Caller must hold the per-branch lifecycle lock. Engine.Run wires this via
// cfg.Seed under the lock, so the deterministic plan-<branch> ID is race-safe.
// Test callers must serialize manually.
//
// workOrderAbs must be an absolute path — runLifecycle pre-validates via
// ResolveWorkOrder. The check below is for direct test callers that bypass
// runLifecycle: a relative path here would bind the planner task to a path
// interpreted under the agent's $ORCH_PROJECT, which may differ from the
// operator's cwd.
func SeedPlannerTask(store orch.Store, branch, workOrderAbs string) error {
	if branch == "" {
		return errors.New("seed: branch is required")
	}
	if workOrderAbs == "" {
		return errors.New("seed: work-order path is required")
	}
	if !filepath.IsAbs(workOrderAbs) {
		return fmt.Errorf("seed: work-order path %q must be absolute", workOrderAbs)
	}

	hash, err := hashWorkOrder(workOrderAbs)
	if err != nil {
		return fmt.Errorf("seed: hash work-order: %w", err)
	}

	taskID := plannerTaskID(branch)
	existing, err := store.GetTask(taskID)
	if err != nil {
		// Either ErrNotFound (create path) or a real I/O error (abort).
		if errors.Is(err, orch.ErrNotFound) {
			return createSeed(store, taskID, branch, workOrderAbs, hash)
		}
		return fmt.Errorf("seed: lookup %s: %w", taskID, err)
	}

	// Existing task — decide based on status and hash.
	switch {
	case !existing.Status.Terminal():
		// open / assigned / in_progress — leave it alone. BVV-TG-03 forbids
		// touching in-progress tasks; open tasks already need no nudge to be
		// dispatched. Either way, our work here is done.
		return nil

	case existing.Labels[orch.LabelWorkOrderHash] == hash:
		// Terminal but content unchanged — re-running is a no-op. The user
		// presumably re-invoked `wonka run` for resume-like reasons; nothing
		// for us to mutate.
		return nil

	default:
		// Terminal + content changed → reopen so Charlie reconciles the graph
		// against the new spec. BVV-TG-02 (Charlie idempotent) handles the
		// downstream reconciliation; we just unblock the dispatch.
		return reopenSeed(store, existing, workOrderAbs, hash)
	}
}

// plannerTaskID derives the deterministic ID used for the seeded planner task.
// sanitizeBranch strips slashes so the ID is filesystem-safe (FSStore writes
// tasks/<id>.json) and matches the run-dir naming convention. The branch
// label on the task carries the raw branch unchanged so ReadyTasks(branch)
// keeps finding it across the slash boundary.
func plannerTaskID(branch string) string {
	return "plan-" + sanitizeBranch(branch)
}

// createSeed inserts a fresh planner task. Labels carry the role/branch/
// criticality contract orch dispatches on, plus our work-order-hash for
// future change detection. Body is the absolute work-order path so Charlie's
// `bd show $ORCH_TASK_ID` reads a path it can stat directly.
func createSeed(store orch.Store, id, branch, workOrderAbs, hash string) error {
	now := time.Now().UTC()
	t := &orch.Task{
		ID:    id,
		Title: id,
		Body:  workOrderAbs,
		// Status open + zero priority = first thing dispatched once Run enters
		// its loop. Charlie has no architectural prerequisites; planning is
		// the lifecycle's root.
		Status:   orch.StatusOpen,
		Priority: 0,
		Labels: map[string]string{
			orch.LabelRole:          orch.RolePlanner,
			orch.LabelBranch:        branch,
			orch.LabelCriticality:   string(orch.Critical),
			orch.LabelWorkOrderHash: hash,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateTask(t); err != nil {
		return fmt.Errorf("seed: create %s: %w", id, err)
	}
	return nil
}

// reopenSeed flips a terminal planner task back to open with refreshed body
// and hash. We do NOT reset retry/handoff counters — that's BVV-S-02a (human
// re-open) territory, not the seed path.
func reopenSeed(store orch.Store, t *orch.Task, workOrderAbs, hash string) error {
	t.Body = workOrderAbs
	t.Status = orch.StatusOpen
	t.Labels[orch.LabelWorkOrderHash] = hash
	t.UpdatedAt = time.Now().UTC()
	if err := store.UpdateTask(t); err != nil {
		return fmt.Errorf("seed: reopen %s: %w", t.ID, err)
	}
	return nil
}

// hashWorkOrder produces a SHA-256 over WorkOrderRequiredFiles, NUL-separated
// so content shifting between files doesn't collide with the same bytes
// appearing at a boundary. Callers must have already validated the files exist
// (via ResolveWorkOrder); I/O errors here indicate a concurrent mutation.
func hashWorkOrder(workOrderAbs string) (string, error) {
	h := sha256.New()
	for i, name := range WorkOrderRequiredFiles {
		if i > 0 {
			h.Write([]byte{0})
		}
		f, err := os.Open(filepath.Join(workOrderAbs, name))
		if err != nil {
			return "", fmt.Errorf("open %s: %w", name, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("read %s: %w", name, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close %s: %w", name, closeErr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
