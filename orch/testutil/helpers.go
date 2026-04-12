package testutil

import (
	"context"
	"sync/atomic"

	"github.com/endgame/wonka-factory/orch"
)

// MockPreset returns a minimal Preset suitable for unit tests. No flags are
// set — the preset's Command is "echo" (no-op in tests that bypass tmux).
func MockPreset() *orch.Preset {
	return &orch.Preset{
		Name:    "mock",
		Command: "echo",
	}
}

// MockRoleConfig returns a RoleConfig with a MockPreset and no instruction file.
func MockRoleConfig() orch.RoleConfig {
	return orch.RoleConfig{
		Preset: MockPreset(),
	}
}

// MockLifecycleConfig returns a LifecycleConfig for the given branch with
// roles auto-populated from the provided role names (each gets a MockRoleConfig).
func MockLifecycleConfig(branch string, roles ...string) *orch.LifecycleConfig {
	roleMap := make(map[string]orch.RoleConfig, len(roles))
	for _, r := range roles {
		roleMap[r] = MockRoleConfig()
	}
	return &orch.LifecycleConfig{
		Branch:       branch,
		GapTolerance: 3,
		MaxRetries:   2,
		MaxHandoffs:  3,
		Roles:        roleMap,
	}
}

// ImmediateSpawnFunc returns a SpawnFunc that immediately sends an outcome
// with the given exit code. Bypasses tmux entirely for unit tests.
func ImmediateSpawnFunc(exitCode int) orch.SpawnFunc {
	return func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(exitCode), exitCode, roleCfg)
	}
}

// ChannelSpawnFunc returns a SpawnFunc that blocks until a signal is sent on
// the returned channel. The int value sent is the exit code. Each invocation
// of the SpawnFunc blocks independently.
func ChannelSpawnFunc() (orch.SpawnFunc, chan<- int) {
	ch := make(chan int, 10)
	fn := func(ctx context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		select {
		case <-ctx.Done():
			return
		case code := <-ch:
			outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(code), code, roleCfg)
		}
	}
	return fn, ch
}

// SequenceSpawnFunc returns a SpawnFunc that yields exit codes from a slice
// in order. After the slice is exhausted, subsequent calls return exit code 0.
func SequenceSpawnFunc(codes []int) orch.SpawnFunc {
	var idx atomic.Int64
	return func(_ context.Context, task *orch.Task, worker *orch.Worker, roleCfg orch.RoleConfig, outcomes chan<- orch.TaskOutcome) {
		i := int(idx.Add(1) - 1)
		code := 0
		if i < len(codes) {
			code = codes[i]
		}
		outcomes <- orch.NewTaskOutcome(task, worker, orch.DetermineOutcome(code), code, roleCfg)
	}
}
