package workflow_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/storage"
	"github.com/ungarscool1/goblins/internal/workflow"
)

type perms map[string]core.Permission

func (p perms) Permission(_ context.Context, _ string, _ core.Subject, a core.Actor) (core.Permission, error) {
	if _, known := p[a.Login]; !known && a.Login == "flaky" {
		return 0, core.Errorf(core.KindTransient, "test", "tracker unavailable")
	}
	return p[a.Login], nil
}

type harness struct {
	t      *testing.T
	file   string
	store  *storage.Store
	engine *workflow.Engine
	now    time.Time
}

var (
	alice = core.Actor{ID: "a", Login: "alice", Type: core.ActorHuman}
	mal   = core.Actor{ID: "m", Login: "mallory", Type: core.ActorHuman}
	subj  = core.Subject{Kind: core.SubjectIssue, Provider: "t", Container: "acme/app", ExternalID: "42"}
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, file: filepath.Join(t.TempDir(), "g.db"), now: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	h.restart()
	t.Cleanup(func() { _ = h.store.Close() })
	return h
}

// restart simulates a process crash: all in-memory state is gone, only the
// database file survives.
func (h *harness) restart() {
	h.t.Helper()
	if h.store != nil {
		_ = h.store.Close()
	}
	s, err := storage.OpenSQLite(context.Background(), h.file)
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = s
	h.engine = &workflow.Engine{
		Store: s, Auth: perms{"alice": core.PermissionWrite, "mallory": core.PermissionRead}, Owner: "engine",
		Policies: func(string) (workflow.Policy, bool) { return workflow.DefaultPolicy(), true },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return h.now },
	}
}

func (h *harness) tick(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) issue(key string) core.Event {
	return core.Event{Project: "p", Source: "s", Type: core.EventIssueOpened, Subject: subj, DedupKey: key, Actor: alice, OccurredAt: h.now,
		Issue: &core.Issue{Title: "Add feature", Body: "Please", Open: true, Author: alice}}
}

func (h *harness) comment(key string, a core.Actor, body string) core.Event {
	return core.Event{Project: "p", Source: "s", Type: core.EventCommentCreated, Subject: subj, DedupKey: key, Actor: a, OccurredAt: h.now,
		Comment: &core.Comment{ID: key, Author: a, Body: body, CreatedAt: h.now, UpdatedAt: h.now}}
}

func (h *harness) deliver(evs ...core.Event) {
	h.t.Helper()
	for _, ev := range evs {
		if _, _, err := h.store.AppendEvent(context.Background(), ev); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.engine.Drain(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) task() core.Task {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), workflow.TaskID("p", subj))
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *harness) wantState(s core.State) {
	h.t.Helper()
	if got := h.task(); got.State != s {
		h.t.Fatalf("state = %s (%s), want %s", got.State, got.Reason, s)
	}
}

// claim returns a runnable job of kind, or nil if none is queued. Side jobs that
// come first are completed along the way.
func (h *harness) claim(kind core.JobKind) *core.Job {
	h.t.Helper()
	ctx := context.Background()
	if h.countActive(kind) == 0 {
		return nil
	}
	for {
		j, err := h.store.ClaimJob(ctx, core.JobClaim{Owner: "w", LeaseFor: time.Minute, Now: h.now})
		if err != nil {
			h.t.Fatal(err)
		}
		if j == nil {
			return nil
		}
		if j.Kind == kind {
			return j
		}
		h.finish(*j, true, "", "", nil)
	}
}

func (h *harness) countActive(kind core.JobKind) int {
	h.t.Helper()
	jobs, _ := h.store.ListJobs(context.Background(), h.task().ID)
	n := 0
	for _, j := range jobs {
		if j.Kind == kind && (j.State == core.JobPending || j.State == core.JobRunning) {
			n++
		}
	}
	return n
}

func (h *harness) finish(j core.Job, ok bool, kind core.ErrorKind, msg string, res []byte) {
	h.t.Helper()
	ctx := context.Background()
	task, err := h.store.GetTask(ctx, j.TaskID)
	if err != nil {
		h.t.Fatal(err)
	}
	ev := workflow.JobOutcomeEvent(task, j, core.JobOutcome{OK: ok, ErrorKind: kind, Message: msg, Result: res}, h.now)
	state := core.JobSucceeded
	if !ok {
		state = core.JobFailed
	}
	if err := h.store.FinishJob(ctx, core.JobFinish{JobID: j.ID, Owner: j.LeaseOwner, Attempt: j.Attempt, State: state, Event: &ev, Now: h.now}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.engine.Drain(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) comments() []string {
	h.t.Helper()
	jobs, err := h.store.ListJobs(context.Background(), h.task().ID)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, j := range jobs {
		if j.Kind == workflow.JobComment {
			out = append(out, string(j.Payload))
		}
	}
	return out
}

func (h *harness) countJobs(kind core.JobKind) int {
	h.t.Helper()
	jobs, _ := h.store.ListJobs(context.Background(), h.task().ID)
	n := 0
	for _, j := range jobs {
		if j.Kind == kind {
			n++
		}
	}
	return n
}

func jsonOf(t *testing.T, s string) []byte { t.Helper(); return []byte(s) }

func design(body string) []byte {
	return []byte(fmt.Sprintf(`{"session_id":"plan-1","design":%q}`, body))
}

func TestDuplicateDeliveriesStartOneJob(t *testing.T) {
	h := newHarness(t)
	ev := h.issue("issue-opened-42")
	h.deliver(ev, ev)
	ev.DeliveryID = "redelivery"
	h.deliver(ev)
	if n := h.countJobs(workflow.JobPlan); n != 1 {
		t.Fatalf("plan jobs = %d", n)
	}
	h.wantState(core.StateInvestigating)
	trs, _ := h.store.ListTransitions(context.Background(), h.task().ID)
	if len(trs) != 1 || trs[0].From != core.StateDiscovered {
		t.Fatalf("transitions = %+v", trs)
	}
}

func TestDesignApprovalFlowAndReapproval(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliver(h.issue("i"))

	// Questions first, then an answer, then a design.
	h.finish(*h.claim(workflow.JobPlan), true, "", "", jsonOf(t, `{"session_id":"plan-1","questions":["Which database?"]}`))
	h.wantState(core.StateWaitingForUser)
	if h.claim(workflow.JobImplement) != nil {
		t.Fatal("implementation before approval")
	}

	h.tick(time.Hour)
	h.deliver(h.comment("c1", alice, "Postgres please"))
	h.wantState(core.StateInvestigating)
	h.finish(*h.claim(workflow.JobPlan), true, "", "", design("Use Postgres."))
	h.wantState(core.StateDesignProposed)
	first := h.task()
	if first.DesignRevision != 1 || first.Sessions[core.RolePlanner] != "plan-1" {
		t.Fatalf("task = %+v", first)
	}
	if h.claim(workflow.JobImplement) != nil {
		t.Fatal("implementation before approval")
	}

	// Rejected approvals leave everything in place.
	h.tick(time.Minute)
	h.deliver(h.comment("c2", mal, "/approve-design 1"))
	h.wantState(core.StateDesignProposed)
	h.deliver(h.comment("c3", alice, "/approve-design 9"))
	h.wantState(core.StateDesignProposed)
	if h.task().Approval != nil || h.claim(workflow.JobImplement) != nil {
		t.Fatal("rejected approval had an effect")
	}

	// Feedback yields revision 2, and approving revision 1 no longer works.
	h.tick(time.Minute)
	h.deliver(h.comment("c4", alice, "Also add an index"))
	h.finish(*h.claim(workflow.JobPlan), true, "", "", design("Use Postgres with an index."))
	second := h.task()
	if second.DesignRevision != 2 || second.DesignHash == first.DesignHash {
		t.Fatalf("task = %+v", second)
	}
	h.tick(time.Minute)
	h.deliver(h.comment("c5", alice, "/approve-design 1"))
	h.wantState(core.StateDesignProposed)

	h.deliver(h.comment("c6", alice, "/approve-design 2"))
	h.wantState(core.StateAgentReady)
	approved := h.task()
	if approved.Approval == nil || approved.Approval.Revision != 2 || approved.Approval.Hash != second.DesignHash || approved.Approval.Approver.Login != "alice" {
		t.Fatalf("approval = %+v", approved.Approval)
	}
	if d, err := h.store.GetDesign(ctx, approved.ID, 1); err != nil || d.Body != "Use Postgres." {
		t.Fatalf("design history = %+v, %v", d, err)
	}

	// A material requirement change reopens approval and cancels queued work.
	h.tick(time.Minute)
	edit := core.Event{Project: "p", Source: "s", Type: core.EventIssueEdited, Subject: subj, DedupKey: "edit", Actor: alice, OccurredAt: h.now,
		Issue: &core.Issue{Title: "Add feature", Body: "Scope changed", Open: true}}
	h.deliver(edit)
	h.wantState(core.StateInvestigating)
	if h.task().Approval != nil {
		t.Fatal("approval survived a requirement change")
	}
	if h.claim(workflow.JobImplement) != nil {
		t.Fatal("cancelled implementation still runs")
	}
	h.finish(*h.claim(workflow.JobPlan), true, "", "", design("Revised for the new scope."))
	if got := h.task(); got.DesignRevision != 3 || got.State != core.StateDesignProposed {
		t.Fatalf("task = %+v", got)
	}
	h.tick(time.Minute)
	h.deliver(h.comment("c7", alice, "/approve-design 2"))
	h.wantState(core.StateDesignProposed)
	h.deliver(h.comment("c8", alice, "/approve-design 3"))
	h.wantState(core.StateAgentReady)

	all := strings.Join(h.comments(), "\n")
	for _, want := range []string{"Which database?", "/approve-design 3", "needs write access", "not the current revision"} {
		if !strings.Contains(all, want) {
			t.Errorf("comments lack %q:\n%s", want, all)
		}
	}
}

func TestReorderedComments(t *testing.T) {
	h := newHarness(t)
	h.deliver(h.issue("i"))
	h.finish(*h.claim(workflow.JobPlan), true, "", "", design("D"))
	h.tick(time.Minute)
	feedback := h.comment("feedback", alice, "wait, change X") // written first
	h.tick(time.Minute)
	approve := h.comment("approve", alice, "/approve-design 1") // written second

	// Delivered in the opposite order: the approval is honored, since it was
	// written after the proposal, and the older comment is not new input.
	h.deliver(approve, feedback)
	h.wantState(core.StateAgentReady)
	if h.countJobs(workflow.JobPlan) != 1 {
		t.Fatal("stale comment triggered a replan")
	}
}

func TestReorderedFeedbackBeforeApprovalNeverApprovesStaleDesign(t *testing.T) {
	h := newHarness(t)
	h.deliver(h.issue("i"))
	h.finish(*h.claim(workflow.JobPlan), true, "", "", design("D"))
	h.tick(time.Minute)
	feedback := h.comment("feedback", alice, "change X")
	h.tick(time.Minute)
	approve := h.comment("approve", alice, "/approve-design 1")

	h.deliver(feedback, approve)
	// The design is being revised, so the approval cannot apply to the revision.
	h.wantState(core.StateInvestigating)
	if h.task().Approval != nil {
		t.Fatal("approved while investigating")
	}
}

func TestApprovalWrittenBeforeRevisionExistedIsRefused(t *testing.T) {
	h := newHarness(t)
	h.deliver(h.issue("i"))
	early := h.comment("early", alice, "/approve-design 1")
	h.tick(time.Minute)
	job := h.claim(workflow.JobPlan)
	h.finish(*job, true, "", "", design("D"))
	// The comment says revision 1, but it was written before revision 1 existed:
	// it delivers late and must not approve what its author never saw.
	h.deliver(early)
	h.wantState(core.StateDesignProposed)
	if h.task().Approval != nil {
		t.Fatal("pre-written approval accepted")
	}
}

func TestCrashWhileRunningResumesWithoutDoubleRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliver(h.issue("i"))
	job := h.claim(workflow.JobPlan)
	if err := h.store.SaveCheckpoint(ctx, job.ID, job.LeaseOwner, job.Attempt, []byte(`{"session_id":"sess-before-crash"}`)); err != nil {
		t.Fatal(err)
	}

	h.restart() // the process dies mid-job

	// While the lease is valid nobody else takes it.
	if again := h.claim(workflow.JobPlan); again != nil {
		t.Fatal("job double-run during its lease")
	}
	h.tick(2 * time.Minute)
	again := h.claim(workflow.JobPlan)
	if again == nil || again.ID != job.ID || again.Attempt != 2 || !strings.Contains(string(again.Checkpoint), "sess-before-crash") {
		t.Fatalf("recovered job = %+v", again)
	}
	// The dead worker's late result is fenced off.
	if err := h.store.FinishJob(ctx, core.JobFinish{JobID: job.ID, Owner: job.LeaseOwner, Attempt: job.Attempt, State: core.JobSucceeded, Now: h.now}); !core.IsKind(err, core.KindConflict) {
		t.Fatalf("fenced finish: %v", err)
	}
	h.finish(*again, true, "", "", design("D"))
	h.wantState(core.StateDesignProposed)
}

func TestCrashBetweenClaimAndCommitReprocessesEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.store.AppendEvent(ctx, h.issue("i")); err != nil {
		t.Fatal(err)
	}
	if ev, _ := h.store.ClaimEvent(ctx, "crashed", time.Minute, h.now); ev == nil {
		t.Fatal("no event")
	}
	h.restart()
	if err := h.engine.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.FindTask(ctx, "p", subj); !core.IsKind(err, core.KindNotFound) {
		t.Fatal("event processed during another worker's lease")
	}
	h.tick(2 * time.Minute)
	if err := h.engine.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(core.StateInvestigating)
	if h.countJobs(workflow.JobPlan) != 1 {
		t.Fatal("reprocessing duplicated effects")
	}
}

func TestQuotaPauseSurvivesRestartAndResumesSession(t *testing.T) {
	h := newHarness(t)
	h.deliver(h.issue("i"))
	h.finish(*h.claim(workflow.JobPlan), false, core.KindQuotaExhausted, "usage limit reached", nil)
	h.wantState(core.StateQuotaPaused)
	if h.claim(workflow.JobPlan) != nil {
		t.Fatal("work continued during pause")
	}
	h.restart()
	h.wantState(core.StateQuotaPaused)
	due := func() []core.Task {
		tasks, err := h.store.DueTasks(context.Background(), h.now)
		if err != nil {
			t.Fatal(err)
		}
		return tasks
	}
	if len(due()) != 0 {
		t.Fatal("due before the pause ended")
	}
	h.tick(time.Hour)
	if len(due()) != 1 {
		t.Fatal("not due after the pause")
	}
	h.deliver(core.Event{Project: "p", Source: "engine", Type: core.EventTimerFired, Subject: subj, DedupKey: "timer", OccurredAt: h.now})
	h.wantState(core.StateInvestigating)
	if h.claim(workflow.JobPlan) == nil {
		t.Fatal("work did not resume")
	}
}

func TestWaitingForDaysConsumesNothing(t *testing.T) {
	h := newHarness(t)
	h.deliver(h.issue("i"))
	h.finish(*h.claim(workflow.JobPlan), true, "", "", jsonOf(t, `{"session_id":"s","questions":["Q?"]}`))
	h.tick(10 * 24 * time.Hour)
	h.restart()
	if h.claim(workflow.JobPlan) != nil {
		t.Fatal("silence caused planner work")
	}
	h.wantState(core.StateWaitingForUser)
	h.deliver(h.comment("c", alice, "answer"))
	if h.claim(workflow.JobPlan) == nil {
		t.Fatal("reply did not resume planning")
	}
	if got := h.task().Sessions[core.RolePlanner]; got != "s" {
		t.Fatalf("session lost: %q", got)
	}
}

func TestPermissionLookupOutageRetriesInsteadOfRejecting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliver(h.issue("i"))
	h.finish(*h.claim(workflow.JobPlan), true, "", "", design("D"))
	h.tick(time.Minute)
	flaky := core.Actor{ID: "f", Login: "flaky", Type: core.ActorHuman}
	if _, _, err := h.store.AppendEvent(ctx, h.comment("c", flaky, "/approve-design 1")); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Drain(ctx); !core.IsKind(err, core.KindTransient) {
		t.Fatalf("err = %v", err)
	}
	h.wantState(core.StateDesignProposed)
	h.engine.Auth = perms{"flaky": core.PermissionWrite}
	if err := h.engine.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(core.StateDesignProposed) // the retry is delayed
	h.tick(10 * time.Second)
	if err := h.engine.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(core.StateAgentReady)
}

func TestConcurrentEnginesProduceOneEffect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const issues = 10
	subject := func(i int) core.Subject {
		return core.Subject{Kind: core.SubjectIssue, Provider: "t", Container: "acme/app", ExternalID: fmt.Sprint(i)}
	}
	for i := range issues {
		for d := range 3 {
			ev := h.issue(fmt.Sprintf("opened-%d", i))
			ev.Subject = subject(i)
			ev.DeliveryID = fmt.Sprint(d)
			if _, _, err := h.store.AppendEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := *h.engine
			e.Owner = fmt.Sprintf("engine-%d", w)
			for range 100 {
				if _, err := e.ProcessNext(ctx); err != nil && !core.IsKind(err, core.KindConflict) {
					t.Errorf("worker %d: %v", w, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	tasks, err := h.store.ListTasks(ctx, "p")
	if err != nil || len(tasks) != issues {
		t.Fatalf("tasks = %d, %v", len(tasks), err)
	}
	for _, task := range tasks {
		jobs, _ := h.store.ListJobs(ctx, task.ID)
		if len(jobs) != 1 {
			t.Fatalf("task %s has %d jobs", task.ID, len(jobs))
		}
	}
}

func TestUnknownProjectIsDroppedNotRetried(t *testing.T) {
	h := newHarness(t)
	ev := h.issue("x")
	ev.Project = "other"
	h.engine.Policies = func(p string) (workflow.Policy, bool) { return workflow.DefaultPolicy(), p == "p" }
	h.deliver(ev)
	if tasks, _ := h.store.ListTasks(context.Background(), ""); len(tasks) != 0 {
		t.Fatal("task for unknown project")
	}
}
