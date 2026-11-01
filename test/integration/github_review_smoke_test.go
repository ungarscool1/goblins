//go:build smoke

// Opt-in smoke test of review conversations against a real GitHub App and a disposable pull
// request. It writes one reply in a thread you name, once, however often it runs, and uses no
// model. It needs real credentials, so it never runs in CI.
//
//	GOBLINS_SMOKE_GITHUB=1 GOBLINS_SMOKE_APP_ID=... GOBLINS_SMOKE_APP_KEY_FILE=... \
//	GOBLINS_SMOKE_REPO=owner/disposable GOBLINS_SMOKE_PR=1 GOBLINS_SMOKE_THREAD=123 \
//	  go test -tags smoke -run GitHubReviewSmoke -v ./test/integration/
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
)

func TestGitHubReviewSmoke(t *testing.T) {
	if os.Getenv("GOBLINS_SMOKE_GITHUB") != "1" {
		t.Skip("set GOBLINS_SMOKE_GITHUB=1 and the variables above to run against a real App")
	}
	key, err := os.ReadFile(os.Getenv("GOBLINS_SMOKE_APP_KEY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := gh.NewClient(gh.ClientOptions{AppID: os.Getenv("GOBLINS_SMOKE_APP_ID"), PrivateKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := gh.NewSCM("smoke", c, "")
	repo := core.RepoRef{Slug: os.Getenv("GOBLINS_SMOKE_REPO")}
	number, thread := os.Getenv("GOBLINS_SMOKE_PR"), os.Getenv("GOBLINS_SMOKE_THREAD")
	if _, err := s.ListReviews(ctx, repo, number); err != nil {
		t.Fatal(err)
	}
	draft := core.CommentDraft{IdempotencyKey: "smoke/" + thread, Body: "Smoke test of review conversations."}
	for range 2 {
		if err := s.ReplyToThread(ctx, repo, number, thread, draft); err != nil {
			t.Fatal(err)
		}
	}
	threads, err := s.ReviewThreads(ctx, repo, number)
	if err != nil {
		t.Fatal(err)
	}
	replies := 0
	for _, th := range threads {
		if th.ThreadID == thread && th.Own && strings.Contains(th.Body, "Smoke test") {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("replies = %d, want 1", replies)
	}
}
