package pipeline_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// The run behind #47, replayed on fakes: questions, two designs, an approval, a failed check, a
// pull request, an unchanged look, a change request with one inline remark and a summary of
// it, a correction, and an approval. On goblins-testing#3 that came to 23 comments by Goblins on
// the issue. What has to stay: the questions, each design, and the inline answer. Everything
// else is one status, edited in place.
func TestNoisyRunStaysQuiet(t *testing.T) {
	w := setup(t)
	w.policy.StopAtReport, w.policy.SkipReviewTriage = false, false
	w.w.SetPermission(repo, "alice", core.PermissionWrite)
	w.val.Reports = []core.ValidationReport{{Status: core.ValidationFailed, Summary: "unit failed"},
		{Status: core.ValidationPassed, Summary: "unit passed"}}
	w.rt.Behavior = func(core.WorkloadSpec) fake.WorkloadResult {
		sha := strings.Repeat("0", 39) + string(rune('0'+w.impl.RunCount()))
		w.w.SetBranch(repo, "goblins/"+w.taskID(), sha)
		return fake.WorkloadResult{Stdout: `GOBLINS_RESULT {"prepared":true,"base_sha":"` + baseSHA +
			`","head_sha":"` + sha + `","changed":true}` + "\n"}
	}
	w.agent.Script(fake.AgentStep{Output: questionsJSON("Which status code?")},
		fake.AgentStep{Output: designJSON("first")}, fake.AgentStep{Output: designJSON("second")})
	w.open()
	w.say(maintainer, "200, please")
	w.say(maintainer, "Make it plain text instead")
	w.say(maintainer, "/approve-design 2")
	task := w.want(core.StateAwaitingReview)
	number, first := task.PullRequest.Number, task.HeadSHA

	// Looking again changes nothing.
	edits := w.w.Edits()
	w.pr(number, core.Event{Type: core.EventPRSynchronized, DedupKey: "again", OccurredAt: w.clock()})
	if w.w.Edits() != edits {
		t.Fatal("an unchanged look edited the status")
	}

	// A change request: one inline remark, and a summary that says nothing more.
	th := w.w.AddThread(repo, number, core.ReviewThread{Path: "a.go", Line: 3,
		Body: "this panics on empty input", Author: alice, CommitSHA: first})
	w.w.SubmitReview(repo, number, core.Review{ID: "r1", Author: alice, CommitSHA: first,
		Verdict: core.VerdictChangesRequested, Body: "One small change.",
		Permission: core.PermissionWrite})
	w.impl.Script(fake.AgentStep{Output: triageOutput(
		workflow.TriageItem{ID: "thread:" + th.ID, Decision: workflow.TriageValid,
			Plan: "guard against empty input, with a test"},
		workflow.TriageItem{ID: "review:r1", Decision: workflow.TriageUnclear,
			Reply: "Which change do you mean, beyond the inline remark?"},
	)})
	w.advance(time.Minute)
	w.pr(number, core.Event{Type: core.EventPRReviewed, DedupKey: "r1", Actor: alice,
		Verdict: core.VerdictChangesRequested, OccurredAt: w.clock()})
	second := w.want(core.StateAwaitingReview).HeadSHA
	if second == first {
		t.Fatal("no correction")
	}

	// The reviewer approves the correction.
	w.w.DismissReview(repo, number, "r1")
	w.w.SubmitReview(repo, number, core.Review{ID: "r2", Author: alice, CommitSHA: second,
		Verdict: core.VerdictApprove, Permission: core.PermissionWrite})
	w.advance(time.Minute)
	w.pr(number, core.Event{Type: core.EventPRReviewed, DedupKey: "r2", Actor: alice,
		Verdict: core.VerdictApprove, OccurredAt: w.clock()})
	w.want(core.StateAwaitingReview)

	posted := w.posted()
	// The questions, two designs and one status: nothing else on the issue.
	if got := w.botComments(); got != 4 {
		t.Fatalf("%d comments by Goblins on the issue, want 4:\n%s", got, posted)
	}
	for _, want := range []string{"Which status code?", "revision 1", "revision 2",
		"## Goblins status", second, "Corrections after review: 1 of"} {
		if !strings.Contains(posted, want) {
			t.Errorf("%q is missing from:\n%s", want, posted)
		}
	}
	// One answer, in the thread that asked for it. No top-level reply about the summary.
	if got := w.w.Replies(repo, number, th.ID); len(got) != 1 || !strings.Contains(got[0], second) {
		t.Fatalf("thread replies = %q", got)
	}
	if got := w.w.PullRequestComments(repo, number); len(got) != 0 {
		t.Fatalf("top-level comments on the pull request: %q", got)
	}
	summary := w.finding("review:r1")
	if !slices.ContainsFunc(summary.History, func(e core.FindingEvent) bool {
		return e.Status == core.FindingSuperseded
	}) {
		t.Fatalf("summary = %+v", summary)
	}
	t.Logf("issue: %d comments by Goblins, %d edits of its status; pull request: %d replies, "+
		"%d top-level comments", w.botComments(), w.w.Edits(),
		len(w.w.Replies(repo, number, th.ID)), len(w.w.PullRequestComments(repo, number)))
}

// The status comment is found again after a restart, its own edits start nothing, and if someone
// deletes it, the next change posts it again.
func TestStatusCommentSurvivesRestartsAndDeletion(t *testing.T) {
	w, number := live(t)
	status := w.statusComment()
	comments, runs := w.botComments(), w.agent.RunCount()+w.impl.RunCount()

	w.advance(time.Minute)
	w.accept(core.Event{Type: core.EventCommentEdited, DedupKey: "own-edit", Actor: w.w.Bot,
		Comment: &status, OccurredAt: w.clock()})
	if w.agent.RunCount()+w.impl.RunCount() != runs || w.statusComment().Body != status.Body {
		t.Fatal("an edit of the status comment by Goblins started something")
	}

	w.restart()
	edits := w.w.Edits()
	w.w.SubmitReview(repo, number, core.Review{ID: "ok", Author: alice, CommitSHA: w.task().HeadSHA,
		Verdict: core.VerdictApprove, Permission: core.PermissionWrite})
	w.pr(number, core.Event{Type: core.EventPRReviewed, DedupKey: "ok", Actor: alice,
		Verdict: core.VerdictApprove, OccurredAt: w.clock()})
	if w.botComments() != comments || w.w.Edits() == edits || w.statusComment().ID != status.ID {
		t.Fatalf("after a restart: %d comments, was %d; %d edits, was %d", w.botComments(),
			comments, w.w.Edits(), edits)
	}

	w.w.DeleteComment(w.issue.Ref, status.ID)
	w.w.SetPullRequest(repo, number, func(pr *core.PullRequest) { pr.Draft = false })
	w.pr(number, core.Event{Type: core.EventPRSynchronized, DedupKey: "ready", OccurredAt: w.clock()})
	again := w.statusComment()
	if again.ID == status.ID || w.botComments() != comments {
		t.Fatalf("not posted again: %+v, %d comments", again, w.botComments())
	}
}

// statusComment returns the status comment Goblins posted on the ticket.
func (w *world) statusComment() core.Comment {
	w.t.Helper()
	for _, c := range w.w.Comments(w.issue.Ref) {
		if c.Author.Type == core.ActorBot && strings.HasPrefix(c.Body, "## Goblins status") {
			return c
		}
	}
	w.t.Fatalf("no status comment in:\n%s", w.posted())
	return core.Comment{}
}
