package github_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
	"github.com/ungarscool1/goblins/internal/providers/github/fakegithub"
)

func scmSetup(t *testing.T) (*fakegithub.Server, *gh.SCM) {
	t.Helper()
	srv, c := setup(t)
	srv.SetBranch(repo, "main", "aaa")
	srv.SetBranch(repo, "goblins/t1", "bbb")
	return srv, gh.NewSCM("gh", c, "")
}

func TestSCMContract(t *testing.T) {
	contract.SourceControl(t, func(t *testing.T) contract.SCMHarness {
		_, s := scmSetup(t)
		return contract.SCMHarness{SCM: s, Repo: core.RepoRef{Slug: repo}, BaseBranch: "main", BaseSHA: "aaa", WorkBranch: "goblins/t1"}
	})
}

func TestGitAccessIsScopedAndKeepsTokenOutOfURL(t *testing.T) {
	srv, s := scmSetup(t)
	a, err := s.GitAccess(ctx, core.RepoRef{Slug: repo}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.RemoteURL != "https://github.com/"+repo+".git" || a.Username != "x-access-token" || a.Password.Empty() {
		t.Fatalf("access = %+v", a)
	}
	if strings.Contains(a.RemoteURL, a.Password.Reveal()) {
		t.Fatal("token in URL")
	}
	if a.ExpiresAt.IsZero() {
		t.Fatal("no expiry")
	}
	if got := srv.TokenRepos[len(srv.TokenRepos)-1]; len(got) != 1 || got[0] != "app" {
		t.Fatalf("token repositories = %v", got)
	}
}

func draft() core.PullRequestDraft {
	return core.PullRequestDraft{IdempotencyKey: "k", Repo: core.RepoRef{Slug: repo}, Head: "goblins/t1", Base: "main", Title: "Add feature", Body: "Closes #1", Draft: true}
}

func TestDraftPullRequestThenSameOneIsUpdated(t *testing.T) {
	srv, s := scmSetup(t)
	pr, created, err := s.EnsurePullRequest(ctx, draft())
	if err != nil || !created || !pr.Draft || pr.HeadSHA != "bbb" || pr.Base != "main" {
		t.Fatalf("create = %+v, %v, %v", pr, created, err)
	}
	if pr.Author.Type != core.ActorBot {
		t.Fatalf("author = %+v", pr.Author)
	}

	srv.SetBranch(repo, "goblins/t1", "ccc")
	d := draft()
	d.Title = "Add feature, second iteration"
	again, created, err := s.EnsurePullRequest(ctx, d)
	if err != nil || created || again.Number != pr.Number || again.Title != d.Title || again.HeadSHA != "ccc" {
		t.Fatalf("update = %+v, %v, %v", again, created, err)
	}
	if got := srv.PullRequests(repo); len(got) != 1 {
		t.Fatalf("pull requests = %d", len(got))
	}
}

func TestLostResponseDoesNotDuplicate(t *testing.T) {
	srv, s := scmSetup(t)
	// Another attempt wins the race between the lookup and the create.
	if _, _, err := s.EnsurePullRequest(ctx, draft()); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.EnsurePullRequest(ctx, draft()); err != nil || created {
		t.Fatalf("retry: %v %v", created, err)
	}
	if got := srv.PullRequests(repo); len(got) != 1 {
		t.Fatalf("pull requests = %d", len(got))
	}
}

func TestMissingPermissionIsExplicit(t *testing.T) {
	srv, s := scmSetup(t)
	srv.DenyPullRequests = true
	_, _, err := s.EnsurePullRequest(ctx, draft())
	if !core.IsKind(err, core.KindForbidden) || !strings.Contains(core.UserMessage(err), "permissions") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "ghs_") {
		t.Fatalf("token in error: %v", err)
	}
}

func TestMissingBranchIsInvalid(t *testing.T) {
	_, s := scmSetup(t)
	d := draft()
	d.Head = "goblins/does-not-exist"
	if _, _, err := s.EnsurePullRequest(ctx, d); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestReviewsNeverApproveAndAreIdempotent(t *testing.T) {
	srv, s := scmSetup(t)
	pr, _, err := s.EnsurePullRequest(ctx, draft())
	if err != nil {
		t.Fatal(err)
	}
	r := core.RepoRef{Slug: repo}
	review := core.ReviewDraft{IdempotencyKey: "rev", Verdict: core.VerdictApprove, Body: "looks good", Comments: []core.InlineComment{{Path: "a.go", Line: 3, Body: "nit"}}}
	for range 2 {
		if err := s.PostReview(ctx, r, pr.Number, review); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	_, _ = fmt.Sscan(pr.Number, &n)
	reviews := srv.Reviews(repo, n)
	if len(reviews) != 1 {
		t.Fatalf("reviews = %d", len(reviews))
	}
	if reviews[0]["event"] != "COMMENT" {
		t.Fatalf("event = %v: Goblins must not approve", reviews[0]["event"])
	}
	threads, err := s.ReviewThreads(ctx, r, pr.Number)
	if err != nil || len(threads) != 1 || threads[0].Path != "a.go" || threads[0].Line != 3 {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	if err := s.PostReview(ctx, r, pr.Number, core.ReviewDraft{IdempotencyKey: "rev2", Verdict: core.VerdictChangesRequested, Body: "no"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Reviews(repo, n)[1]["event"]; got != "REQUEST_CHANGES" {
		t.Fatalf("event = %v", got)
	}
	if err := s.PostPullRequestComment(ctx, r, pr.Number, core.CommentDraft{IdempotencyKey: "c", Body: "status"}); err != nil {
		t.Fatal(err)
	}
}

func TestStatusChecksMapping(t *testing.T) {
	srv, s := scmSetup(t)
	srv.SetCheckRun(repo, "bbb", "build", "completed", "success")
	srv.SetCheckRun(repo, "bbb", "lint", "completed", "failure")
	srv.SetCheckRun(repo, "bbb", "e2e", "in_progress", "")
	got, err := s.StatusChecks(ctx, core.RepoRef{Slug: repo}, "bbb")
	if err != nil || len(got) != 3 {
		t.Fatalf("checks = %+v, %v", got, err)
	}
	want := map[string]core.CheckState{"build": core.CheckPassed, "lint": core.CheckFailed, "e2e": core.CheckPending}
	for _, c := range got {
		if want[c.Name] != c.State {
			t.Errorf("%s = %s", c.Name, c.State)
		}
	}
	c, err := s.GetCommit(ctx, core.RepoRef{Slug: repo}, "bbb")
	if err != nil || c.SHA != "bbb" {
		t.Fatalf("commit = %+v, %v", c, err)
	}
}

func TestSCMFactory(t *testing.T) {
	srv, _ := scmSetup(t)
	r := providers.NewRegistry()
	gh.Register(r)
	deps := providers.Deps{Getenv: func(string) (string, bool) { return string(srv.KeyPEM()), true }}
	s, err := providers.As[core.SourceControl](r, providers.KindSCM, "github", "s", deps, providers.Options{"app_id": 1, "private_key_env": "K", "api_url": srv.URL, "git_url": "file:///git"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.GitAccess(ctx, core.RepoRef{Slug: repo}, false)
	if err != nil || a.RemoteURL != "file:///git/acme/app.git" {
		t.Fatalf("access = %+v, %v", a, err)
	}
}
