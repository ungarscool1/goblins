package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/storage"
	"github.com/ungarscool1/goblins/internal/workflow"
)

type allow struct{}

func (allow) Permission(context.Context, string, core.Subject, core.Actor) (core.Permission, error) {
	return core.PermissionWrite, nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t     *testing.T
	file  string
	store *storage.Store
	now   atomic.Int64
	sched *scheduler.Scheduler
	src   *fake.EventSource
	plans atomic.Int64
}

func (e *env) clock() time.Time { return time.Unix(0, e.now.Load()).UTC() }

func (e *env) tick(d time.Duration) { e.now.Add(int64(d)) }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, file: filepath.Join(t.TempDir(), "g.db"), src: fake.NewEventSource("src")}
	e.now.Store(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC).UnixNano())
	e.boot()
	t.Cleanup(func() { _ = e.store.Close() })
	return e
}

// boot simulates a fresh process on the same database.
func (e *env) boot() {
	e.t.Helper()
	if e.store != nil {
		_ = e.store.Close()
	}
	s, err := storage.OpenSQLite(context.Background(), e.file)
	if err != nil {
		e.t.Fatal(err)
	}
	e.store = s
	engine := &workflow.Engine{Store: s, Auth: allow{}, Owner: "engine", Log: discard, Now: e.clock,
		Policies: func(string) (workflow.Policy, bool) {
			p := workflow.DefaultPolicy()
			p.PlannerLimitKey = "agent-a"
			return p, true
		}}
	inbox := scheduler.NewInbox(s, discard, []scheduler.Route{{Source: "src", Container: "acme/app", Project: "p", Tracker: "t", SCM: "s"}})
	inbox.Now = e.clock
	e.sched = &scheduler.Scheduler{
		Store: s, Engine: engine, Inbox: inbox, Log: discard, Now: e.clock,
		Handlers: map[core.JobKind]scheduler.Handler{
			workflow.JobPlan: func(context.Context, scheduler.JobContext) (any, error) {
				e.plans.Add(1)
				return workflow.PlanResult{Questions: []string{"Why?"}}, nil
			},
			workflow.JobComment: func(context.Context, scheduler.JobContext) (any, error) { return struct{}{}, nil },
		},
		Cfg: scheduler.Config{Owner: "w", Installation: "inst", Heartbeat: 10 * time.Millisecond, ReconcileEvery: time.Hour, TickInterval: 2 * time.Millisecond},
	}
}

func issueEvent(id string) core.Event {
	actor := core.Actor{Login: "alice", Type: core.ActorHuman}
	return core.Event{Type: core.EventIssueOpened, DedupKey: "opened-" + id, Actor: actor,
		Subject: core.Subject{Kind: core.SubjectIssue, Container: "acme/app", ExternalID: id},
		Issue:   &core.Issue{Title: "T" + id, Body: "B", Open: true}, OccurredAt: time.Unix(1, 0)}
}

func (e *env) task(id string) core.Task {
	e.t.Helper()
	sub := core.Subject{Kind: core.SubjectIssue, Provider: "t", Container: "acme/app", ExternalID: id}
	task, err := e.store.FindTask(context.Background(), "p", sub)
	if err != nil {
		e.t.Fatal(err)
	}
	return task
}

func TestDuplicateDeliveriesAndRestartsStartOneJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev := issueEvent("1")
	for range 3 {
		if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{ev}); err != nil {
			t.Fatal(err)
		}
	}
	e.sched.Step(ctx)
	if got := e.plans.Load(); got != 1 {
		t.Fatalf("plans = %d", got)
	}
	e.boot()
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{ev}); err != nil {
		t.Fatal(err)
	}
	e.sched.Step(ctx)
	if got := e.plans.Load(); got != 1 {
		t.Fatalf("plans after restart = %d", got)
	}
	if st := e.task("1").State; st != core.StateWaitingForUser {
		t.Fatalf("state = %s", st)
	}
}

func TestInboxDropsUnroutedContainers(t *testing.T) {
	e := newEnv(t)
	ev := issueEvent("1")
	ev.Subject.Container = "evil/repo"
	n, err := e.sched.Inbox.Accept(context.Background(), "src", []core.Event{ev})
	if err != nil || n != 0 {
		t.Fatalf("accepted %d, %v", n, err)
	}
	if e.sched.Inbox.Allowed("src", "evil/repo") || !e.sched.Inbox.Allowed("src", "acme/app") {
		t.Fatal("allow-list wrong")
	}
}

func TestReconciliationPollRecoversLostWebhook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sched.Sources = []scheduler.PollSource{{Source: e.src, Container: "acme/app", Interval: time.Minute}}
	e.sched.Step(ctx)
	if e.plans.Load() != 0 {
		t.Fatal("idle poll caused work")
	}
	// The webhook never arrived, but the polling source reports the same fact.
	e.src.Push(issueEvent("7"))
	e.sched.Step(ctx)
	if e.plans.Load() != 0 {
		t.Fatal("poll interval not honored")
	}
	e.tick(time.Minute)
	e.sched.Step(ctx)
	if e.plans.Load() != 1 {
		t.Fatalf("plans = %d", e.plans.Load())
	}
	cur, _ := e.store.GetCursor(ctx, "poll:src:acme/app")
	if cur != "1" {
		t.Fatalf("cursor = %q", cur)
	}
	// A webhook redelivery of the same fact is absorbed.
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("7")}); err != nil {
		t.Fatal(err)
	}
	e.tick(time.Minute)
	e.sched.Step(ctx)
	if e.plans.Load() != 1 {
		t.Fatalf("plans after replay = %d", e.plans.Load())
	}
}

func TestLimitsBoundABurstOfIssues(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running, peak, done atomic.Int64
	release := make(chan struct{})
	e.sched.Cfg.Limits = scheduler.Limits{MaxPerKey: map[string]int{"agent-a": 2}}
	e.sched.Handlers[workflow.JobPlan] = func(ctx context.Context, _ scheduler.JobContext) (any, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		done.Add(1)
		return workflow.PlanResult{Questions: []string{"Q"}}, nil
	}
	var evs []core.Event
	for i := range 10 {
		evs = append(evs, issueEvent(fmt.Sprint(i)))
	}
	if _, err := e.sched.Inbox.Accept(ctx, "src", evs); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { _ = e.sched.Run(ctx); close(stopped) }()
	waitFor(t, func() bool { return running.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak = %d with limit 2", p)
	}
	close(release)
	waitFor(t, func() bool { return done.Load() == 10 })
	cancel()
	<-stopped
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak = %d", p)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestTransientFailureRetriesWithBackoffThenFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sched.Cfg.RetryBase = time.Minute
	var calls atomic.Int64
	e.sched.Handlers[workflow.JobPlan] = func(context.Context, scheduler.JobContext) (any, error) {
		calls.Add(1)
		return nil, core.Errorf(core.KindTransient, "x", "try later")
	}
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	e.sched.Step(ctx)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	e.sched.Step(ctx)
	if calls.Load() != 1 {
		t.Fatal("retried before backoff")
	}
	e.tick(time.Minute)
	e.sched.Step(ctx)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	e.tick(2 * time.Minute)
	e.sched.Step(ctx)
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if st := e.task("1").State; st != core.StateFailed {
		t.Fatalf("state = %s, want failed after attempts ran out", st)
	}
}

func TestQuotaErrorPausesAndResumesOnTimer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var calls atomic.Int64
	e.sched.Handlers[workflow.JobPlan] = func(context.Context, scheduler.JobContext) (any, error) {
		if calls.Add(1) == 1 {
			return nil, core.Errorf(core.KindQuotaExhausted, "x", "limit reached")
		}
		return workflow.PlanResult{Questions: []string{"Q"}}, nil
	}
	e.sched.Handlers[workflow.JobNotify] = func(context.Context, scheduler.JobContext) (any, error) { return struct{}{}, nil }
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	e.sched.Step(ctx)
	if st := e.task("1").State; st != core.StateQuotaPaused {
		t.Fatalf("state = %s", st)
	}
	e.boot() // the pause outlives the process
	e.sched.Handlers[workflow.JobPlan] = func(context.Context, scheduler.JobContext) (any, error) {
		calls.Add(1)
		return workflow.PlanResult{Questions: []string{"Q"}}, nil
	}
	e.sched.Handlers[workflow.JobNotify] = func(context.Context, scheduler.JobContext) (any, error) { return struct{}{}, nil }
	e.sched.Step(ctx)
	if calls.Load() != 1 {
		t.Fatal("ran while paused")
	}
	e.tick(31 * time.Minute)
	e.sched.Step(ctx)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if st := e.task("1").State; st != core.StateWaitingForUser {
		t.Fatalf("state = %s", st)
	}
}

func TestCrashedJobCannotDoubleRunAndContinuesFromCheckpoint(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	if err := e.sched.Engine.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	// Another process holds the job when it dies.
	job, err := e.store.ClaimJob(ctx, core.JobClaim{Owner: "dead", LeaseFor: 2 * time.Minute, Now: e.clock()})
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	if err := e.store.SaveCheckpoint(ctx, job.ID, "dead", 1, []byte(`{"session":"s9"}`)); err != nil {
		t.Fatal(err)
	}
	e.boot()
	var seen atomic.Value
	e.sched.Handlers[workflow.JobPlan] = func(_ context.Context, jc scheduler.JobContext) (any, error) {
		e.plans.Add(1)
		seen.Store(string(jc.Job.Checkpoint))
		return workflow.PlanResult{Questions: []string{"Q"}}, nil
	}
	e.sched.Step(ctx)
	if e.plans.Load() != 0 {
		t.Fatal("ran during a live lease")
	}
	e.tick(3 * time.Minute)
	e.sched.Step(ctx)
	if e.plans.Load() != 1 || seen.Load() != `{"session":"s9"}` {
		t.Fatalf("plans = %d, checkpoint = %v", e.plans.Load(), seen.Load())
	}
}

func TestCancelStopsARunningAttempt(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	stopped := make(chan error, 1)
	var once sync.Once
	e.sched.Handlers[workflow.JobPlan] = func(hctx context.Context, _ scheduler.JobContext) (any, error) {
		once.Do(func() { close(started) })
		<-hctx.Done()
		stopped <- hctx.Err()
		return nil, hctx.Err()
	}
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = e.sched.Run(ctx); close(done) }()
	<-started
	actor := core.Actor{Login: "alice", Type: core.ActorHuman}
	ev := core.Event{Type: core.EventCommentCreated, DedupKey: "cancel", Actor: actor, Subject: core.Subject{Kind: core.SubjectIssue, Container: "acme/app", ExternalID: "1"},
		Comment: &core.Comment{ID: "c", Author: actor, Body: "/goblins cancel", CreatedAt: e.clock()}, OccurredAt: e.clock()}
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{ev}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("attempt kept running after cancel")
	}
	cancel()
	<-done
	if st := e.task("1").State; st != core.StateCancelled {
		t.Fatalf("state = %s", st)
	}
}

func TestShutdownRequeuesWithoutFailing(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	e.sched.Handlers[workflow.JobPlan] = func(hctx context.Context, _ scheduler.JobContext) (any, error) {
		close(started)
		<-hctx.Done()
		return nil, hctx.Err()
	}
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = e.sched.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done

	e.boot()
	bg := context.Background()
	e.sched.Step(bg)
	if e.plans.Load() != 1 {
		t.Fatalf("job was not picked up after restart: plans=%d", e.plans.Load())
	}
	if st := e.task("1").State; st != core.StateWaitingForUser {
		t.Fatalf("state = %s", st)
	}
}

func TestTranscriptIsBoundedAndRedacted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sched.Cfg.MaxTranscript = 5
	e.sched.Cfg.MaxTranscriptLine = 50
	e.sched.Handlers[workflow.JobPlan] = func(_ context.Context, jc scheduler.JobContext) (any, error) {
		jc.Log("text", "using Bearer abcdefghijklmnopqrstuvwxyz")
		jc.Log("text", strings.Repeat("x", 500))
		for range 100 {
			jc.Log("text", "line")
		}
		return workflow.PlanResult{Questions: []string{"Q"}}, nil
	}
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	e.sched.Step(ctx)
	entries, err := e.store.ListTranscript(ctx, e.task("1").ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d", len(entries))
	}
	if strings.Contains(entries[0].Text, "abcdefghijkl") {
		t.Fatalf("secret stored: %q", entries[0].Text)
	}
	if len(entries[1].Text) > 70 {
		t.Fatalf("line not truncated: %d", len(entries[1].Text))
	}
	if entries[4].Kind != "system" {
		t.Fatalf("limit not announced: %+v", entries[4])
	}
}

func TestHandlerPanicAndMissingHandlerAreContained(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sched.Cfg.RetryBase = time.Millisecond
	e.sched.Handlers[workflow.JobPlan] = func(context.Context, scheduler.JobContext) (any, error) { panic("boom") }
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.sched.Step(ctx)
		e.tick(time.Minute)
	}
	if st := e.task("1").State; st != core.StateFailed {
		t.Fatalf("state = %s", st)
	}

	delete(e.sched.Handlers, workflow.JobPlan)
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("2")}); err != nil {
		t.Fatal(err)
	}
	e.sched.Step(ctx)
	task := e.task("2")
	if task.State != core.StateBlocked || !strings.Contains(task.Reason, "no handler") {
		t.Fatalf("task = %s %q", task.State, task.Reason)
	}
}

func TestJobTimeoutStopsStuckWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.sched.Cfg.JobTimeout = map[core.JobKind]time.Duration{workflow.JobPlan: 10 * time.Millisecond}
	e.sched.Cfg.RetryBase = time.Millisecond
	var calls atomic.Int64
	e.sched.Handlers[workflow.JobPlan] = func(hctx context.Context, _ scheduler.JobContext) (any, error) {
		calls.Add(1)
		<-hctx.Done()
		return nil, hctx.Err()
	}
	if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		e.sched.Step(ctx)
		e.tick(time.Minute)
	}
	task := e.task("1")
	if calls.Load() != 3 || task.State != core.StateFailed || !strings.Contains(task.Reason, "time limit") {
		t.Fatalf("calls = %d, task = %s %q", calls.Load(), task.State, task.Reason)
	}
}

// The same reconciliation works whatever the backend is called: the runtime is
// only ever seen through its contract.
func TestOrphanWorkloadsAreReconciledOnAnyRuntime(t *testing.T) {
	for _, typ := range []string{"fake", "fake-cluster"} {
		t.Run(typ, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			rt := fake.NewRuntime("rt")
			rt.TypeName = typ
			rt.Behavior = func(core.WorkloadSpec) fake.WorkloadResult { return fake.WorkloadResult{Hang: true} }
			e.sched.Runtimes = []core.ExecutionRuntime{rt}

			if _, err := e.sched.Inbox.Accept(ctx, "src", []core.Event{issueEvent("1")}); err != nil {
				t.Fatal(err)
			}
			if err := e.sched.Engine.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ := e.store.ClaimJob(ctx, core.JobClaim{Owner: "other", LeaseFor: time.Minute, Now: e.clock()})
			labels := func(jobID, attempt string) map[string]string {
				return map[string]string{scheduler.LabelInstallation: "inst", scheduler.LabelJob: jobID, scheduler.LabelAttempt: attempt}
			}
			mk := func(name string, l map[string]string) {
				if _, err := rt.Create(ctx, core.WorkloadSpec{Name: name, Network: core.NetworkNone, Labels: l}); err != nil {
					t.Fatal(err)
				}
			}
			mk("live", labels(job.ID, "1"))
			mk("older-attempt", labels(job.ID, "0"))
			mk("unknown-job", labels("j-gone", "1"))
			other := labels("j-gone", "1")
			other[scheduler.LabelInstallation] = "another-installation"
			mk("foreign", other)
			mk("no-job-label", map[string]string{scheduler.LabelInstallation: "inst"})

			e.sched.ReconcileWorkloads(ctx)
			left, _ := rt.List(ctx, nil)
			names := map[string]bool{}
			for _, w := range left {
				names[w.Name] = true
			}
			if !names["live"] || !names["foreign"] || !names["no-job-label"] || names["older-attempt"] || names["unknown-job"] {
				t.Fatalf("after reconcile: %v", names)
			}
			// Once the owner's lease lapses, its workload is an orphan too.
			e.tick(2 * time.Minute)
			e.sched.ReconcileWorkloads(ctx)
			if _, err := rt.Status(ctx, "live"); !core.IsKind(err, core.KindNotFound) {
				t.Fatalf("expired workload kept: %v", err)
			}
		})
	}
}
