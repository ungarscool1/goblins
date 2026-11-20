package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

// taskStore answers GetTask from a map. Nothing else is called.
type taskStore struct {
	core.StateStore
	tasks map[string]core.Task
}

func (s taskStore) GetTask(_ context.Context, id string) (core.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return core.Task{}, core.Errorf(core.KindNotFound, "test", "no task %s", id)
	}
	return t, nil
}

func TestVolumesLeftBehindAreReclaimed(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	old, recent := now.Add(-2*time.Hour), now.Add(-time.Minute)
	store := taskStore{tasks: map[string]core.Task{
		"done":      {ID: "done", State: core.StateCompleted, UpdatedAt: old},
		"cancelled": {ID: "cancelled", State: core.StateCancelled, UpdatedAt: old},
		"just-done": {ID: "just-done", State: core.StateCompleted, UpdatedAt: recent},
		"failed":    {ID: "failed", State: core.StateFailed, UpdatedAt: old},
		"blocked":   {ID: "blocked", State: core.StateBlocked, UpdatedAt: old},
	}}
	rt := fake.NewRuntime("rt")
	ctx := context.Background()
	for _, v := range []struct{ name, installation, task string }{
		{"v-done", "inst", "done"}, {"v-cancelled", "inst", "cancelled"},
		{"v-just-done", "inst", "just-done"}, {"v-failed", "inst", "failed"},
		{"v-blocked", "inst", "blocked"}, {"v-gone", "inst", "gone"},
		{"v-foreign", "elsewhere", "done"}, {"v-no-task", "inst", ""},
	} {
		labels := map[string]string{scheduler.LabelInstallation: v.installation}
		if v.task != "" {
			labels[scheduler.LabelTask] = v.task
		}
		if err := rt.EnsureVolume(ctx, v.name, labels); err != nil {
			t.Fatal(err)
		}
	}
	m := observability.NewMetrics()
	s := &scheduler.Scheduler{Store: store, Handlers: map[core.JobKind]scheduler.Handler{},
		Runtimes: []core.ExecutionRuntime{rt}, Log: discard, Metrics: m,
		Now: func() time.Time { return now }, Cfg: scheduler.Config{Installation: "inst"}}
	s.ReconcileVolumes(ctx)
	for name, kept := range map[string]bool{"v-done": false, "v-cancelled": false, "v-gone": false,
		"v-just-done": true, "v-failed": true, "v-blocked": true, "v-foreign": true,
		"v-no-task": true} {
		if rt.HasVolume(name) != kept {
			t.Errorf("%s: kept = %v, want %v", name, rt.HasVolume(name), kept)
		}
	}
}

// A runtime that cannot list volumes is left alone.
func TestVolumeReconciliationNeedsTheCapability(t *testing.T) {
	rt := fake.NewRuntime("rt")
	rt.Caps = []core.Capability{core.CapRuntimeVolumes, core.CapRuntimeNetworkNone}
	ctx := context.Background()
	_ = rt.EnsureVolume(ctx, "v", map[string]string{scheduler.LabelInstallation: "inst",
		scheduler.LabelTask: "gone"})
	s := &scheduler.Scheduler{Store: taskStore{}, Handlers: map[core.JobKind]scheduler.Handler{},
		Runtimes: []core.ExecutionRuntime{rt}, Log: discard, Now: time.Now,
		Cfg: scheduler.Config{Installation: "inst"}}
	s.ReconcileVolumes(ctx)
	if !rt.HasVolume("v") {
		t.Fatal("removed a volume through a runtime that does not list them")
	}
}
