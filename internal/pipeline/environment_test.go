package pipeline

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/storage"
)

type envWorld struct {
	p     *Pipeline
	pr    *Project
	env   *fake.Environment
	store *storage.Store
	tasks map[string]core.Task
}

// envStore is the real lease store, with tasks from a map.
type envStore struct {
	*storage.Store
	tasks map[string]core.Task
}

func (s envStore) GetTask(_ context.Context, id string) (core.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return core.Task{}, core.Errorf(core.KindNotFound, "test", "no task")
	}
	return t, nil
}

func newEnvWorld(t *testing.T, targets ...string) *envWorld {
	t.Helper()
	st, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	w := &envWorld{store: st, tasks: map[string]core.Task{},
		env: &fake.Environment{Name: "dev", TargetsV: targets}}
	w.pr = &Project{ID: "p", Environment: w.env,
		Implementer: Stage{Runtime: fake.NewRuntime("rt")}}
	w.p = &Pipeline{Installation: "inst", Projects: map[string]*Project{"p": w.pr},
		Environments: envStore{st, w.tasks}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return w
}

func (w *envWorld) jc(id string) scheduler.JobContext {
	if _, ok := w.tasks[id]; !ok {
		w.tasks[id] = core.Task{ID: id, Project: "p", State: core.StateImplementing}
	}
	return scheduler.JobContext{Task: w.tasks[id], Job: core.Job{ID: "j-" + id, Attempt: 1},
		Log: func(string, string) {}}
}

func (w *envWorld) check(t *testing.T, id string) ([]core.ValidationCheck, string, error) {
	t.Helper()
	return w.p.checkInEnvironment(context.Background(), w.jc(id), w.pr, "abc123", "scratch")
}

// Two tasks never use one target at once. The second waits, at no cost, and gets it once the
// first released it.
func TestASharedTargetIsUsedByOneTaskAtATime(t *testing.T) {
	w := newEnvWorld(t, "dev-1")
	w.env.Fail = func(step, _ string) bool { return step == "smoke" }
	if _, failure, err := w.check(t, "a"); err != nil || !strings.Contains(failure, "smoke") {
		t.Fatalf("a: %q, %v", failure, err)
	}
	// a failed, so it keeps the target for its repair round.
	_, _, err := w.check(t, "b")
	if !core.IsKind(err, core.KindConflict) || core.RetryAtOf(err).IsZero() ||
		!strings.Contains(err.Error(), "dev-1 (task a)") {
		t.Fatalf("b while a holds it: %v", err)
	}
	w.env.Fail = nil
	if _, failure, err := w.check(t, "a"); err != nil || failure != "" {
		t.Fatalf("a repaired: %q, %v", failure, err)
	}
	if _, failure, err := w.check(t, "b"); err != nil || failure != "" {
		t.Fatalf("b after a released: %q, %v", failure, err)
	}
	ran := w.env.Ran()
	if !slices.Equal(ran[:4], []string{"provision@dev-1", "deploy@dev-1", "smoke@dev-1",
		"logs@dev-1"}) || slices.Index(ran, "reset@dev-1") < 0 {
		t.Fatalf("ran = %v", ran)
	}
}

// A target held by a task that finished without releasing it is reset, then reused.
func TestATargetLeftByAFinishedTaskIsResetFirst(t *testing.T) {
	w := newEnvWorld(t, "dev-1")
	w.env.Fail = func(step, _ string) bool { return step == "smoke" }
	_, _, _ = w.check(t, "gone")
	w.tasks["gone"] = core.Task{ID: "gone", State: core.StateCancelled}
	w.env.Fail = nil
	checks, failure, err := w.check(t, "next")
	if err != nil || failure != "" || !strings.HasPrefix(checks[0].Name, "reset on dev/dev-1") {
		t.Fatalf("checks = %+v, %q, %v", checks, failure, err)
	}
}

// A reset that fails keeps the target from everyone until a person restores it.
func TestABrokenResetReservesTheTarget(t *testing.T) {
	w := newEnvWorld(t, "dev-1")
	w.env.Fail = func(step, _ string) bool { return step == "reset" }
	_, _, err := w.check(t, "a")
	if !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("failed reset: %v", err)
	}
	l, _ := w.store.GetLease(context.Background(), "env:dev:dev-1")
	if l.Holder != "a" || l.Data[noteBroken] == "" || !l.ExpiresAt.Equal(forever) {
		t.Fatalf("lease = %+v", l)
	}
	w.tasks["a"] = core.Task{ID: "a", State: core.StateCompleted}
	checks, failure, err := w.check(t, "b")
	if err != nil || !strings.Contains(failure, "needs a person") ||
		checks[0].Status != core.ValidationError {
		t.Fatalf("b: %+v, %q, %v", checks, failure, err)
	}
}

// A step that cannot run twice, interrupted, is never run again on its own.
func TestAnInterruptedStepIsNotRepeated(t *testing.T) {
	w := newEnvWorld(t)
	w.env.Err = map[string]error{"deploy": core.Errorf(core.KindTransient, "test", "worker lost")}
	if _, _, err := w.check(t, "a"); !core.IsKind(err, core.KindTransient) {
		t.Fatalf("interrupted: %v", err)
	}
	w.env.Err = nil
	checks, failure, err := w.check(t, "a")
	if err != nil || !strings.Contains(failure, "deploy") ||
		checks[0].Status != core.ValidationError {
		t.Fatalf("after the interruption: %+v, %q, %v", checks, failure, err)
	}
	if n := strings.Count(strings.Join(w.env.Ran(), " "), "deploy@"); n != 1 {
		t.Fatalf("deploy ran %d times", n)
	}
}

// A cancelled task's cleanup releases what it holds.
func TestCleanupReleasesTheEnvironment(t *testing.T) {
	w := newEnvWorld(t)
	w.env.Fail = func(step, _ string) bool { return step == "smoke" }
	_, _, _ = w.check(t, "a")
	if leases, _ := w.store.ListLeases(context.Background(), "env:"); len(leases) != 1 {
		t.Fatalf("leases = %+v", leases)
	}
	if err := w.p.releaseEnvironments(context.Background(), w.jc("a"), w.pr); err != nil {
		t.Fatal(err)
	}
	if leases, _ := w.store.ListLeases(context.Background(), "env:"); len(leases) != 0 {
		t.Fatalf("leases after cleanup = %+v", leases)
	}
	if !slices.Contains(w.env.Ran(), "reset@"+ephemeralTarget("a")) {
		t.Fatalf("ran = %v", w.env.Ran())
	}
}
