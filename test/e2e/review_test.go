package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
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
