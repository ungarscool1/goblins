package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

func TestCommitHeader(t *testing.T) {
	block := func(s string) string { return "Done.\n```goblins-commit\n" + s + "\n```" }
	cases := []struct{ name, title, output, want string }{
		{"the implementer's words", "Add a thing", block("feat(api): add it"), "feat(api): add it"},
		{"the last block counts", "T", block("fix: first") + block("test: second"), "test: second"},
		{"a capital is lowered", "T", block("fix: Handle nil."), "fix: handle nil"},
		{"breaking", "T", block("feat!: drop v1"), "feat!: drop v1"},
		{"no block", "Add a thing", "Done.", "chore: add a thing"},
		{"unknown type", "Add a thing", block("wip: stuff"), "chore: add a thing"},
		{"upper case type", "Add a thing", block("Feat: stuff"), "chore: add a thing"},
		{"two lines", "Add a thing", block("feat: a\n\nbody"), "chore: add a thing"},
		{"long", "Add a thing", block("feat: " + strings.Repeat("a", 99)), "chore: add a thing"},
		{"a conventional title is kept", "fix(db): Close the pool", "", "fix(db): close the pool"},
		{"an acronym stays", "API errors are wrong", "", "chore: API errors are wrong"},
		{"CI stays on", "T", block("feat: add a thing [skip ci]"), "feat: add a thing"},
		{"CI stays on from a title", "Add a thing [ci skip]", "", "chore: add a thing"},
		{"no closing reference", "T", block("fix: closes #12"), "fix: `closes` #12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := commitHeader(&Project{}, c.title, c.output); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestPlainCommitsKeepTheTitle(t *testing.T) {
	pr := &Project{PlainCommits: true}
	got := commitHeader(pr, "Add a thing", "```goblins-commit\nfeat: other\n```")
	title := pullRequestTitle(pr, "Add a thing", &changeDescription{Title: "feat: other"})
	if got != "Add a thing" || title != "Add a thing" {
		t.Fatalf("commit %q, title %q", got, title)
	}
	if strings.Contains(implementerInstructions(pr), "goblins-commit") {
		t.Fatal("the implementer was asked to word a commit nobody will use")
	}
}

func TestPullRequestTitle(t *testing.T) {
	pr := &Project{}
	cases := []struct {
		name   string
		change *changeDescription
		want   string
	}{
		{"from the description", &changeDescription{Title: "feat: add it"}, "feat: add it"},
		{"an unusable one", &changeDescription{Title: "Added a thing"}, "chore: add a thing"},
		{"no description", nil, "chore: add a thing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pullRequestTitle(pr, "Add a thing", c.change); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAnOpenPullRequestKeepsItsTitle(t *testing.T) {
	ctx := context.Background()
	world := fake.NewWorld()
	repo := core.RepoRef{Slug: "o/r"}
	scm := fake.NewSCM(world, "s")
	worded := &changeDescription{Title: "feat: other words"}
	cases := []struct {
		name, current, want string
		plain               bool
	}{
		{"a worded title stays", "feat: add a thing", "feat: add a thing", false},
		{"so does a person's edit", "My own title", "My own title", false},
		{"the fallback gives way", "chore: add a thing", "feat: other words", false},
		{"plain stays too", "My own title", "My own title", true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pull, _, err := scm.EnsurePullRequest(ctx, core.PullRequestDraft{Repo: repo,
				IdempotencyKey: c.name, Head: "h" + string(rune('a'+i)), Base: "main",
				Title: c.current})
			if err != nil {
				t.Fatal(err)
			}
			pr := &Project{Repo: repo, SCM: scm, PlainCommits: c.plain}
			task := core.Task{Title: "Add a thing", PullRequest: &pull}
			got, err := publishedTitle(ctx, pr, task, worded)
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v, want %q", got, err, c.want)
			}
		})
	}
	t.Run("a first push words one", func(t *testing.T) {
		pr := &Project{Repo: repo, SCM: scm}
		got, err := publishedTitle(ctx, pr, core.Task{Title: "Add a thing"}, worded)
		if err != nil || got != "feat: other words" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("a pull request that is gone gets one", func(t *testing.T) {
		pr := &Project{Repo: repo, SCM: scm}
		task := core.Task{Title: "Add a thing", PullRequest: &core.PullRequest{Number: "999"}}
		got, err := publishedTitle(ctx, pr, task, worded)
		if err != nil || got != "feat: other words" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}

func TestAgentsAreAskedToMarkBreakingChanges(t *testing.T) {
	for name, prompt := range map[string]string{
		"implementer": implementerInstructions(&Project{}), "describer": describerSystem} {
		if !strings.Contains(prompt, "feat!:") {
			t.Errorf("%s: %s", name, prompt)
		}
	}
}
