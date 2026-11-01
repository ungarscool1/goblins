package github_test

import (
	"fmt"
	"maps"
	"strconv"
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

func TestStatusChecksFollowPagination(t *testing.T) {
	srv, s := scmSetup(t)
	for i := range 150 {
		srv.SetCheckRun(repo, "bbb", fmt.Sprintf("ok-%d", i), "completed", "success")
	}
	srv.SetCheckRun(repo, "bbb", "last", "completed", "failure")
	got, err := s.StatusChecks(ctx, core.RepoRef{Slug: repo}, "bbb")
	if err != nil || len(got) != 151 {
		t.Fatalf("checks = %d, %v", len(got), err)
	}
	if last := got[150]; last.Name != "last" || last.State != core.CheckFailed {
		t.Fatalf("a failure on the last page was lost: %+v", last)
	}
}

func TestReviewThreadsReportResolution(t *testing.T) {
	srv, s := scmSetup(t)
	r := core.RepoRef{Slug: repo}
	pr, _, err := s.EnsurePullRequest(ctx, draft())
	if err != nil {
		t.Fatal(err)
	}
	review := core.ReviewDraft{IdempotencyKey: "rev", Verdict: core.VerdictComment, Body: "b", Comments: []core.InlineComment{
		{Path: "a.go", Line: 1, Body: "first"}, {Path: "b.go", Line: 2, Body: "second"},
	}}
	if err := s.PostReview(ctx, r, pr.Number, review); err != nil {
		t.Fatal(err)
	}
	var n int64
	_, _ = fmt.Sscan(pr.Number, &n)
	srv.AddReviewReply(repo, n, 99, 1, "a reply")
	srv.ResolveThread(repo, n, 1)

	got, err := s.ReviewThreads(ctx, r, pr.Number)
	if err != nil || len(got) != 3 {
		t.Fatalf("threads = %+v, %v", got, err)
	}
	want := map[string]bool{"first": true, "second": false, "a reply": true}
	for _, th := range got {
		if th.Resolved != want[th.Body] {
			t.Errorf("%q resolved = %v", th.Body, th.Resolved)
		}
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

func TestNativeReviewReadsOnlyTheExactCommit(t *testing.T) {
	const sha, older = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bot := fakegithub.User{ID: 9, Login: "reviewer[bot]", Type: "Bot"}
	build := func(t *testing.T, opts providers.Options) (*fakegithub.Server, core.ReviewProvider, core.ReviewRequest) {
		t.Helper()
		srv := fakegithub.New(t)
		srv.AddRepo("acme/app")
		pr := srv.AddPullRequest("acme/app", "t", fakegithub.Human(1, "alice"))
		r := providers.NewRegistry()
		gh.Register(r)
		all := providers.Options{"app_id": 42, "private_key_env": "K", "api_url": srv.URL, "reviewer_login": bot.Login, "trigger_comment": "@reviewer review", "wait_seconds": 0}
		maps.Copy(all, opts)
		deps := providers.Deps{Getenv: func(string) (string, bool) { return string(srv.KeyPEM()), true }}
		p, err := providers.As[core.ReviewProvider](r, providers.KindReview, "github-native-review", "native", deps, all)
		if err != nil {
			t.Fatal(err)
		}
		return srv, p, core.ReviewRequest{Repo: core.RepoRef{Slug: "acme/app"}, PullRequest: core.PullRequest{Number: strconv.FormatInt(pr.Number, 10)}, HeadSHA: sha}
	}

	t.Run("unverified does nothing and is unavailable", func(t *testing.T) {
		srv, p, req := build(t, nil)
		_, err := p.Review(ctx, req)
		if !core.IsKind(err, core.KindUnsupported) {
			t.Fatalf("err = %v", err)
		}
		if n, _ := strconv.ParseInt(req.PullRequest.Number, 10, 64); len(srv.Comments("acme/app", n)) != 0 {
			t.Fatal("asked for a review without having been told that works")
		}
	})
	t.Run("a silent reviewer is unavailable, after asking once per commit", func(t *testing.T) {
		srv, p, req := build(t, providers.Options{"verified": true})
		for range 2 {
			if _, err := p.Review(ctx, req); !core.IsKind(err, core.KindUnsupported) {
				t.Fatalf("err = %v", err)
			}
		}
		n, _ := strconv.ParseInt(req.PullRequest.Number, 10, 64)
		if got := srv.Comments("acme/app", n); len(got) != 1 || !strings.Contains(got[0].Body, "@reviewer review") {
			t.Fatalf("comments = %+v", got)
		}
	})
	t.Run("only its review of this commit counts, and approval means nothing", func(t *testing.T) {
		srv, p, req := build(t, providers.Options{"verified": true})
		n, _ := strconv.ParseInt(req.PullRequest.Number, 10, 64)
		srv.AddReview("acme/app", n, bot, older, "CHANGES_REQUESTED", "old")
		srv.AddReview("acme/app", n, fakegithub.Human(2, "bob"), sha, "CHANGES_REQUESTED", "a person")
		srv.AddReview("acme/app", n, bot, sha, "APPROVED", "fine", map[string]any{"path": "a.go", "line": 4, "body": "**P1** this breaks\nmore"}, map[string]any{"path": "b.go", "line": 1, "body": "no tag"})
		res, err := p.Review(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Verdict != core.VerdictComment || !res.Published || len(res.Findings) != 2 {
			t.Fatalf("res = %+v", res)
		}
		if f := res.Findings[0]; f.Severity != core.ReviewHigh || f.HeadSHA != sha || f.Provenance != bot.Login || f.Path != "a.go" {
			t.Fatalf("finding = %+v", f)
		}
		if res.Findings[1].Severity != core.ReviewMedium {
			t.Fatalf("finding = %+v", res.Findings[1])
		}
		if got := srv.Comments("acme/app", n); len(got) != 0 {
			t.Fatalf("asked although a review exists: %+v", got)
		}
	})
	t.Run("configuration is checked", func(t *testing.T) {
		r := providers.NewRegistry()
		gh.Register(r)
		srv := fakegithub.New(t)
		deps := providers.Deps{Getenv: func(string) (string, bool) { return string(srv.KeyPEM()), true }}
		_, err := providers.As[core.ReviewProvider](r, providers.KindReview, "github-native-review", "n", deps, providers.Options{"app_id": 42, "private_key_env": "K", "api_url": srv.URL})
		if !core.IsKind(err, core.KindInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
}
