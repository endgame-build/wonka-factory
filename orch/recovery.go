package orch

import (
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// RetryConfig configures the retry protocol (BVV-ERR-01).
type RetryConfig struct {
	MaxRetries  int           // maximum retry attempts per task (recommended: 2)
	BaseTimeout time.Duration // base timeout for scaled timeout calculation (BVV-ERR-02a)
}

// DefaultRetryConfig returns sensible defaults for CLI flag binding.
// BaseTimeout = 30m matches the BVV-ERR-02a RECOMMENDED default.
// MaxRetries = 2 reflects the BVV-ERR-01 expectation of bounded retries
// without runaway reassignment loops.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:  2,
		BaseTimeout: 30 * time.Minute,
	}
}

// RetryState tracks per-task retry counters (BVV-ERR-01).
//
// BVV retries reset the same task (stable ID) — the fork's pattern of
// creating retry-task clones with suffixed IDs is gone. The dispatcher
// transitions exit-code-1 tasks back to StatusOpen for re-dispatch without
// creating new task entities.
//
// Not thread-safe. Only the dispatcher's outcome-processing step mutates
// RetryState, and that runs on a single goroutine (the dispatch tick drains
// the outcomes channel serially).
type RetryState struct {
	attempts map[string]int
}

// NewRetryState creates an empty retry state.
func NewRetryState() *RetryState {
	return &RetryState{attempts: make(map[string]int)}
}

// CanRetry returns true if the task has retries remaining (BVV-ERR-01).
func (rs *RetryState) CanRetry(taskID string, cfg RetryConfig) bool {
	return rs.attempts[taskID] < cfg.MaxRetries
}

// RecordAttempt increments the attempt count for a task.
func (rs *RetryState) RecordAttempt(taskID string) {
	rs.attempts[taskID]++
}

// AttemptCount returns the number of attempts used for a task.
func (rs *RetryState) AttemptCount(taskID string) int {
	return rs.attempts[taskID]
}

// ScaledTimeout computes the timeout for a retry attempt (BVV-ERR-02a):
//
//	timeout(attempt) = base_timeout * (1.0 + 0.5 * attempt_number)
func ScaledTimeout(base time.Duration, attempt int) time.Duration {
	scale := 1.0 + 0.5*float64(attempt)
	return time.Duration(float64(base) * scale)
}

// RetryJitter adds random jitter to a duration: uniform [0, d/4].
// Returns 0 for non-positive input; returns d (no jitter) when d/4 underflows
// to 0 for very small durations.
func RetryJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	maxJitter := d / 4
	if maxJitter <= 0 {
		return d
	}
	jitter := time.Duration(rand.Int64N(int64(maxJitter)))
	return d + jitter
}

// GapTracker tracks non-critical task failures and enforces the gap tolerance
// bound (BVV-ERR-03, BVV-ERR-04, BVV-ERR-05, BVV-S-07).
//
// NOT thread-safe — the dispatcher's outcome-processing step is the sole
// writer, and that runs serially on the dispatch goroutine via the outcomes
// channel. This matches the TLA+ ApplyGap label atomicity requirement.
type GapTracker struct {
	gaps      int
	tolerance int
	taskIDs   []string // task IDs that contributed gaps (monotonic per BVV-ERR-05)
}

// NewGapTracker creates a tracker with the given tolerance (from LifecycleConfig.GapTolerance).
func NewGapTracker(tolerance int) *GapTracker {
	return &GapTracker{tolerance: tolerance}
}

// IncrementAndCheck atomically increments the gap count and checks tolerance.
// Returns true if gaps >= tolerance (lifecycle must abort, BVV-ERR-04).
// The taskID is recorded for audit purposes (BVV-ERR-05 monotonic accumulation).
func (gt *GapTracker) IncrementAndCheck(taskID string) (abort bool) {
	gt.gaps++
	gt.taskIDs = append(gt.taskIDs, taskID)
	return gt.gaps >= gt.tolerance
}

// Count returns the current gap count.
func (gt *GapTracker) Count() int {
	return gt.gaps
}

// TaskIDs returns the list of task IDs that contributed gaps.
func (gt *GapTracker) TaskIDs() []string {
	return gt.taskIDs
}

// SetGaps restores the tracker from a prior session's event-log replay
// (BVV-ERR-05 monotonic across sessions). The gap count is derived from
// len(taskIDs) — a single source of truth eliminates the class of replay
// bugs where count and the slice disagree. The caller's slice is cloned
// so a post-Resume IncrementAndCheck append cannot alias into memory the
// caller still holds.
func (gt *GapTracker) SetGaps(taskIDs []string) {
	gt.taskIDs = slices.Clone(taskIDs)
	gt.gaps = len(taskIDs)
}

// HandoffState tracks per-task handoff counters for BVV-L-04 (max handoffs)
// and BVV-ERR-11a (watchdog-triggered handoff). Thread-safe via sync.Mutex —
// concurrent writers are the dispatcher tick (exit-code-3 handoff processing)
// and the watchdog goroutine (dead-session restart). Both paths must use
// TryRecord so the BVV-L-04 limit applies to the combined count atomically.
//
// Counters are NOT reset on retry (BVV-L-04: handoff counter monotonic within
// lifecycle). They reset only on human re-open, detected by Phase 5 Resume
// scanning the event log for terminal-then-open task transitions and calling
// Reset(taskID) during the single-goroutine Resume phase.
type HandoffState struct {
	mu       sync.Mutex
	counts   map[string]int
	maxLimit int
}

// NewHandoffState creates a handoff tracker with the given per-task limit.
// A limit of 0 means "no handoffs allowed" — TryRecord refuses on the first
// call and CanHandoff returns false.
func NewHandoffState(maxHandoffs int) *HandoffState {
	return &HandoffState{
		counts:   make(map[string]int),
		maxLimit: maxHandoffs,
	}
}

// CanHandoff reports whether the given task has handoff budget remaining.
// Returns true when the current count is strictly less than maxLimit.
//
// WARNING: this is a non-atomic read. Callers that intend to act on the
// result (i.e. record a handoff) MUST use TryRecord instead — otherwise
// two writers can both observe budget remaining and both increment,
// overshooting maxLimit and violating BVV-L-04. CanHandoff is safe only
// for diagnostic/tests and for purely read-only query paths.
func (h *HandoffState) CanHandoff(taskID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[taskID] < h.maxLimit
}

// RecordHandoffUnchecked increments without enforcing BVV-L-04. The
// "Unchecked" suffix flags every call site: production writers (dispatcher
// exit-3, watchdog dead-session) MUST use TryRecord so the check and
// increment happen atomically. Only Resume replay and test fixtures may
// bypass the budget.
func (h *HandoffState) RecordHandoffUnchecked(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[taskID]++
}

// TryRecord atomically checks the handoff budget and, if budget remains,
// increments the counter under a single mutex acquisition. This is the
// only handoff-mutation API safe to call from the two production writers
// (dispatcher exit-3 + watchdog dead-session) — splitting the check and
// the increment across two lock acquisitions lets both writers pass the
// check and both increment, violating BVV-L-04.
//
// Returns (post-increment count, true) on success; (current count
// unchanged, false) when count >= maxLimit. On false, callers must not
// proceed with a restart — per BVV-ERR-11a the watchdog emits
// EventHandoffLimitReached and the dispatcher fails the task next tick.
func (h *HandoffState) TryRecord(taskID string) (count int, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts[taskID] >= h.maxLimit {
		return h.counts[taskID], false
	}
	h.counts[taskID]++
	return h.counts[taskID], true
}

// Count returns the current handoff count for a task. Safe to call from any
// goroutine.
func (h *HandoffState) Count(taskID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[taskID]
}

// Reset zeroes the handoff counter for a task (BVV-S-02a: human re-open of a
// terminal task resets retry and handoff counters). Called by Phase 5 Resume
// during event-log replay, before the watchdog goroutine starts.
func (h *HandoffState) Reset(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.counts, taskID)
}

// SetCounts replaces the entire counter map. Intended for Phase 5 Resume
// replay from the event log. Designed to be called at init time before any
// other goroutine can access the state — the mutex is held as defensive
// insurance against contract drift rather than as a correctness requirement.
// The lock cost is negligible on the one-shot init path.
//
// A nil input is normalised to an empty map so subsequent in-place
// mutation calls (TryRecord, RecordHandoffUnchecked) don't panic.
func (h *HandoffState) SetCounts(counts map[string]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts = maps.Clone(counts)
	if h.counts == nil {
		h.counts = make(map[string]int)
	}
}
