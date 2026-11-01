package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/github/fakegithub"
)

const withReviewer = `
agents:
  codex: {type: codex-cli, options: {image: agent, home_volume: codex-home}}
  claude:`

// useCodex declares the Codex agent; with review it also reviews. Without it, the project
// only has the agent on its stages, which is how an installation without Codex looks.
func (e *env) useCodex(review bool) {
	e.t.Helper()
	e.cfgYAML = bytes.Replace(e.cfgYAML, []byte("\nagents:\n  claude:"), []byte(withReviewer), 1)
	if review {
		e.cfgYAML = bytes.Replace(e.cfgYAML, []byte("      implementer: {agent: claude"), []byte("      reviewers: [{agent: codex, runtime: local, model: review-model}]\n      implementer: {agent: claude"), 1)
	}
	e.boot()
}

func (e *env) codexFile(name string) string {
	return filepath.Join(e.rt.VolumePath("codex-home"), name)
}

func (e *env) codexScenario(s string) {
	e.t.Helper()
	if err := os.MkdirAll(e.rt.VolumePath("codex-home"), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.codexFile("scenario"), []byte(s), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) codexRuns() int {
	b, err := os.ReadFile(e.codexFile("prompts"))
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "=== ")
}

func (e *env) approve() {
	e.t.Helper()
	e.toDesign()
	e.comment(maintainer, fmt.Sprintf("/approve-design %d", e.task().DesignRevision))
	e.settle()
	e.boot()
	e.settle()
}

// The personal setup: Codex reviews what Claude writes, finds something, Claude corrects it on
// the same pull request, the checks run again, and the new commit passes review.
func TestCodexReviewSendsClaudeBackOnTheSamePullRequest(t *testing.T) {
	e := setup(t)
	e.useCodex(true)
	e.approve()
	task := e.wantState(core.StateCompleted)

	prs := e.gh.PullRequests(repo)
	if len(prs) != 1 {
		t.Fatalf("pull requests = %d", len(prs))
	}
	if got := gitIn(t, e.bare, "show", "refs/heads/goblins/"+task.ID+":feature.txt"); got != "fixed and reviewed" {
		t.Fatalf("feature.txt = %q", got)
	}
	if e.codexRuns() != 2 {
		t.Fatalf("codex runs = %d", e.codexRuns())
	}
	// One failed validation, one pass, one correction.
	if got := e.agentRuns(implementerTools); got != 3 {
		t.Fatalf("implementer rounds = %d", got)
	}
	if task.ValidatedSHA != task.HeadSHA {
		t.Fatalf("validated %s, head %s", task.ValidatedSHA, task.HeadSHA)
	}
	reviews := e.gh.Reviews(repo, prs[0].Number)
	if len(reviews) != 2 {
		t.Fatalf("reviews = %+v", reviews)
	}
	first := reviews[0]
	if first["event"] != "COMMENT" || first["commit_id"] == task.HeadSHA || !strings.Contains(fmt.Sprint(first["body"]), "does not prove") {
		t.Fatalf("first review = %+v", first)
	}
	if !strings.Contains(fmt.Sprint(first["comments"]), "the feature is not reviewed") {
		t.Fatalf("finding not inline: %+v", first)
	}
	if reviews[1]["commit_id"] != task.HeadSHA {
		t.Fatalf("second review = %+v", reviews[1])
	}
	// The reviewer got no write access, no tokens and no way to run commands.
	got, err := os.ReadFile(e.codexFile("invocations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--disable shell_tool", "--disable unified_exec", `sandbox_mode="read-only"`, "--model review-model", "API_KEY=[] GITPW=[]"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("invocations lack %q", want)
		}
	}
	for _, r := range e.gh.Requests {
		if strings.HasSuffix(r, "/merge") {
			t.Fatalf("merge attempted: %s", r)
		}
	}
	if left, _ := e.rt.List(ctx, nil); len(left) != 0 {
		t.Fatalf("workloads left: %+v", left)
	}
	if _, err := os.Stat(e.rt.VolumePath("goblins-e2e-" + task.ID + "-review")); err == nil {
		t.Fatal("review volume left behind")
	}
}

func TestCodexUnavailableFallsBackSafely(t *testing.T) {
	for _, scenario := range []string{"logged_out", "quota"} {
		t.Run(scenario, func(t *testing.T) {
			e := setup(t)
			e.useCodex(true)
			e.codexScenario(scenario)
			e.approve()
			e.wantState(core.StateCompleted)
			prs := e.gh.PullRequests(repo)
			if len(prs) != 1 || len(e.gh.Reviews(repo, prs[0].Number)) != 0 {
				t.Fatalf("pull requests = %+v", prs)
			}
		})
	}
}

// The work setup: Codex is not used, so it is neither needed nor touched, even when defined.
func TestProjectWithoutCodexNeverRunsIt(t *testing.T) {
	e := setup(t)
	e.useCodex(false)
	e.approve()
	e.wantState(core.StateCompleted)
	if _, err := os.Stat(e.codexFile("invocations")); err == nil {
		t.Fatal("codex was run")
	}
	if strconv.Itoa(e.agentRuns(implementerTools)) != "2" {
		t.Fatalf("implementer rounds = %d", e.agentRuns(implementerTools))
	}
}

// followsPullRequest makes tasks go on past their report, which is the default.
func (e *env) followsPullRequest() {
	e.t.Helper()
	e.cfgYAML = bytes.Replace(e.cfgYAML, []byte(", stop_at_report: true"), []byte(", skip_review_triage: true"), 1)
	e.boot()
}

func (e *env) pullRequest() fakegithub.PullRequest {
	e.t.Helper()
	prs := e.gh.PullRequests(repo)
	if len(prs) != 1 {
		e.t.Fatalf("pull requests = %+v", prs)
	}
	return prs[0]
}

func (e *env) requestChanges(id int, body string, sha string) {
	e.t.Helper()
	pr := e.pullRequest()
	e.gh.AddReview(repo, pr.Number, maintainer, sha, "CHANGES_REQUESTED", body)
	e.deliverReview(id, pr, sha, body)
}

func (e *env) deliverReview(id int, pr fakegithub.PullRequest, sha, body string) {
	e.deliver("pull_request_review", map[string]any{"action": "submitted", "pull_request": pr, "sender": maintainer,
		"review": map[string]any{"id": id, "state": "changes_requested", "commit_id": sha, "body": body, "user": maintainer}})
}

// The acceptance scenario of review orchestration: a person's review and then a test failure
// each cause one correction on the same pull request, a replay causes nothing, a change to
// sensitive code stays with a person after approval, and only the merge ends the task.
func TestReviewAndCIFailureDriveCorrectionsUntilAPersonMerges(t *testing.T) {
	e := setup(t)
	e.followsPullRequest()
	e.approve()
	task := e.wantState(core.StateAwaitingReview)
	if task.Gate == nil || task.Gate.State != core.GateWaiting {
		t.Fatalf("gate = %+v", task.Gate)
	}
	pr := e.pullRequest()
	first := task.HeadSHA
	runs := e.agentRuns(implementerTools)

	// A change request becomes a correction on the same pull request.
	e.requestChanges(501, "please handle the empty case", first)
	e.settle()
	e.boot()
	e.settle()
	task = e.wantState(core.StateAwaitingReview)
	if task.HeadSHA == first || e.agentRuns(implementerTools) != runs+1 {
		t.Fatalf("head %s, implementer rounds %d", task.HeadSHA, e.agentRuns(implementerTools))
	}
	if !strings.Contains(e.prompts(), "please handle the empty case") {
		t.Fatal("the request did not reach the implementer")
	}
	if pushed := gitIn(t, e.bare, "rev-parse", "refs/heads/goblins/"+task.ID); pushed != task.HeadSHA {
		t.Fatalf("pushed %s, task at %s", pushed, task.HeadSHA)
	}
	if len(e.gh.PullRequests(repo)) != 1 {
		t.Fatal("a second pull request was opened")
	}

	// Replays cause no more work, and no more words.
	runs = e.agentRuns(implementerTools)
	comments := e.botComments()
	for range 3 {
		e.deliverReview(501, pr, first, "please handle the empty case")
	}
	e.settle()
	if e.agentRuns(implementerTools) != runs || e.botComments() != comments {
		t.Fatalf("a replay had an effect: %d rounds", e.agentRuns(implementerTools))
	}

	// The change request by that person still stands, whatever was pushed since.
	if got := strings.Join(task.Gate.Reasons, "\n"); !strings.Contains(got, "requested changes") {
		t.Fatalf("reasons = %s", got)
	}

	// A failing check on the new commit causes one correction.
	e.gh.SetCheckRun(repo, task.HeadSHA, "unit", "completed", "failure")
	checkEvent := map[string]any{"action": "completed", "sender": fakegithub.BotUser, "check_run": map[string]any{
		"id": 77, "name": "unit", "head_sha": task.HeadSHA, "conclusion": "failure",
		"pull_requests": []map[string]any{{"number": pr.Number}}}}
	e.deliver("check_run", checkEvent)
	e.settle()
	e.boot()
	e.settle()
	second := e.wantState(core.StateAwaitingReview)
	if second.HeadSHA == task.HeadSHA || e.agentRuns(implementerTools) != runs+1 {
		t.Fatalf("head %s, implementer rounds %d", second.HeadSHA, e.agentRuns(implementerTools))
	}
	if !strings.Contains(e.prompts(), "These checks failed on commit "+task.HeadSHA) {
		t.Fatalf("the failure did not reach the implementer:\n%s", e.prompts())
	}

	// Sensitive code stays with a person, even once everything is approved and green.
	e.gh.SetFiles(repo, pr.Number, "internal/auth/token.go")
	e.gh.SetCheckRun(repo, second.HeadSHA, "unit", "completed", "success")
	e.gh.AddReview(repo, pr.Number, maintainer, second.HeadSHA, "APPROVED", "fine by me")
	e.deliver("pull_request_review", map[string]any{"action": "submitted", "pull_request": e.pullRequest(), "sender": maintainer,
		"review": map[string]any{"id": 502, "state": "approved", "commit_id": second.HeadSHA, "user": maintainer}})
	e.settle()
	third := e.wantState(core.StateAwaitingReview)
	if third.Gate == nil || !slices.Contains(third.Gate.Categories, "authentication") {
		t.Fatalf("gate = %+v", third.Gate)
	}
	if !strings.Contains(e.botComments(), "a person has to look at") {
		t.Fatalf("people were not told:\n%s", e.botComments())
	}
	for _, r := range e.gh.Requests {
		if strings.HasSuffix(r, "/merge") {
			t.Fatalf("merge attempted: %s", r)
		}
	}

	// The merge by a person ends the task, and everything temporary with it.
	closed := e.gh.ClosePullRequest(repo, pr.Number, true)
	e.deliver("pull_request", map[string]any{"action": "closed", "pull_request": closed, "sender": maintainer})
	e.settle()
	e.wantState(core.StateCompleted)
	if _, err := os.Stat(e.rt.VolumePath("goblins-e2e-" + task.ID + "-impl")); err == nil {
		t.Fatal("workspace volume left behind")
	}
}

func TestPushBySomeoneElseIsCheckedOutReviewedAndValidated(t *testing.T) {
	e := setup(t)
	e.followsPullRequest()
	e.useCodex(true)
	e.approve()
	task := e.wantState(core.StateAwaitingReview)
	pr := e.pullRequest()
	runs := e.codexRuns()

	// Someone pushes to the branch themselves.
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, e.root, "clone", "-q", "-b", "goblins/"+task.ID, e.bare, clone)
	if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("fixed by hand, reviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", "-A")
	gitIn(t, clone, "-c", "user.name=A", "-c", "user.email=a@example.invalid", "commit", "-qm", "by hand")
	gitIn(t, clone, "push", "-q", "origin", "goblins/"+task.ID)
	pushed := gitIn(t, clone, "rev-parse", "HEAD")

	moved := e.pullRequest()
	moved.Head.SHA = pushed
	for range 2 {
		e.deliver("pull_request", map[string]any{"action": "synchronize", "pull_request": moved, "sender": maintainer})
	}
	e.settle()
	task = e.wantState(core.StateAwaitingReview)
	if task.HeadSHA != pushed || task.ValidatedSHA != pushed {
		t.Fatalf("head %s, validated %s, pushed %s", task.HeadSHA, task.ValidatedSHA, pushed)
	}
	if e.codexRuns() != runs+1 {
		t.Fatalf("reviews = %d, want one more than %d", e.codexRuns(), runs)
	}
	reviews := e.gh.Reviews(repo, pr.Number)
	if got := reviews[len(reviews)-1]["commit_id"]; got != pushed {
		t.Fatalf("reviewed %v", got)
	}
}

// converses makes tasks follow pull requests with review feedback being judged, the default.
func (e *env) converses() {
	e.t.Helper()
	e.cfgYAML = bytes.Replace(e.cfgYAML, []byte(", stop_at_report: true"), nil, 1)
	e.boot()
}

// The acceptance scenario of review conversations, over the real adapter: two remarks are
// judged, corrected on the same pull request, and answered in their own threads, while the
// person's verdict and threads stay exactly as that person left them.
func TestChangeRequestWithTwoRemarksIsAnsweredInItsThreads(t *testing.T) {
	e := setup(t)
	e.converses()
	e.approve()
	task := e.wantState(core.StateAwaitingReview)
	pr := e.pullRequest()
	first := task.HeadSHA
	runs := e.agentRuns(implementerTools)

	e.gh.AddReview(repo, pr.Number, maintainer, first, "CHANGES_REQUESTED", "",
		map[string]any{"path": "feature.txt", "line": 1, "body": "this should say more"},
		map[string]any{"path": "feature.txt", "line": 1, "body": "and be tested"})
	e.deliverReview(601, pr, first, "")
	e.settle()
	e.boot()
	e.settle()

	task = e.wantState(core.StateAwaitingReview)
	if task.HeadSHA == first || e.agentRuns(implementerTools) != runs+1 || len(e.gh.PullRequests(repo)) != 1 {
		t.Fatalf("head %s, implementer rounds %d, pull requests %d", task.HeadSHA, e.agentRuns(implementerTools), len(e.gh.PullRequests(repo)))
	}
	for _, root := range []int64{1, 2} {
		replies := e.gh.ThreadReplies(repo, pr.Number, root)
		if len(replies) != 1 || !strings.Contains(replies[0], task.HeadSHA) || !strings.Contains(replies[0], "left the thread open") {
			t.Fatalf("replies in %d = %q", root, replies)
		}
	}
	if got := strings.Join(task.Gate.Reasons, "\n"); !strings.Contains(got, "@maintainer requested changes") || !strings.Contains(got, "2 remarks by people are unresolved") {
		t.Fatalf("reasons = %s", got)
	}

	// Replaying the review, and our own replies coming back, change nothing.
	comments, replies := e.botComments(), e.gh.ThreadReplies(repo, pr.Number, 1)
	turns := e.agentRuns("Read,Glob,Grep")
	e.deliverReview(601, pr, first, "")
	e.deliver("pull_request_review_comment", map[string]any{"action": "created", "pull_request": e.pullRequest(), "sender": fakegithub.BotUser,
		"comment": map[string]any{"id": 999, "body": "echo", "user": fakegithub.BotUser, "created_at": time.Now()}})
	e.settle()
	if e.agentRuns("Read,Glob,Grep") != turns || e.botComments() != comments || !slices.Equal(e.gh.ThreadReplies(repo, pr.Number, 1), replies) {
		t.Fatal("a replay or an echo had an effect")
	}
	for _, r := range e.gh.Requests {
		if strings.HasSuffix(r, "/merge") || strings.Contains(r, "resolveReviewThread") || strings.Contains(r, "/dismissals") {
			t.Fatalf("Goblins touched a person's decision: %s", r)
		}
	}
}

func TestFalsePositiveIsArguedThenCorrectedWhenThePersonInsists(t *testing.T) {
	e := setup(t)
	e.converses()
	e.approve()
	task := e.wantState(core.StateAwaitingReview)
	pr := e.pullRequest()
	head := task.HeadSHA
	runs := e.agentRuns(implementerTools)

	e.gh.AddReview(repo, pr.Number, maintainer, head, "COMMENTED", "", map[string]any{"path": "feature.txt", "line": 1, "body": "false positive trap"})
	e.deliver("pull_request_review_comment", map[string]any{"action": "created", "pull_request": pr, "sender": maintainer,
		"comment": map[string]any{"id": 1, "body": "false positive trap", "user": maintainer, "created_at": time.Now()}})
	e.settle()
	task = e.wantState(core.StateAwaitingReview)
	if task.HeadSHA != head || e.agentRuns(implementerTools) != runs {
		t.Fatalf("the code changed: head %s", task.HeadSHA)
	}
	replies := e.gh.ThreadReplies(repo, pr.Number, 1)
	if len(replies) != 1 || !strings.Contains(replies[0], "feature.txt:1") || !strings.Contains(replies[0], "confirm, or propose") {
		t.Fatalf("replies = %q", replies)
	}

	// Days later, after a restart, the person answers in the thread.
	e.boot()
	e.gh.AddHumanReply(repo, pr.Number, 5000, 1, maintainer, "I insist, please change it")
	body := map[string]any{"action": "created", "pull_request": pr, "sender": maintainer,
		"comment": map[string]any{"id": 5000, "in_reply_to_id": 1, "body": "I insist, please change it", "user": maintainer, "created_at": time.Now()}}
	for range 2 {
		e.deliver("pull_request_review_comment", body)
	}
	e.settle()
	e.boot()
	e.settle()
	task = e.wantState(core.StateAwaitingReview)
	if task.HeadSHA == head || e.agentRuns(implementerTools) != runs+1 {
		t.Fatalf("head %s, implementer rounds %d", task.HeadSHA, e.agentRuns(implementerTools))
	}
	if !strings.Contains(e.prompts(), "I insist") || !strings.Contains(e.prompts(), "this workflow replied") {
		t.Fatalf("the conversation did not reach the judge:\n%s", e.prompts())
	}
	if got := e.gh.ThreadReplies(repo, pr.Number, 1); len(got) != 3 {
		t.Fatalf("thread = %q", got)
	}
}
