package pipeline

import (
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

func describe(design string) string {
	task := core.Task{Approval: &core.Approval{Hash: "0123456789abcdef",
		Approver: core.Actor{Login: "alice"}}}
	issue := core.Issue{URL: "https://example.invalid/issues/3"}
	return pullRequestBody(task, design, nil, issue, workflow.PublishPayload{SHA: "abc123", Revision: 2})
}

func TestPullRequestBodyDescribesTheChange(t *testing.T) {
	body := describe("### Scope\n\nAdds a health check.\n\n### Architecture\n\nOne handler.\n\n" +
		"### Alternatives considered\n\nA framework.\n\n### Constraints\n\nStandard library.\n\n" +
		"### Implementation plan\n\n1. Write it.\n\n### Tests\n\nA table test.\n\n" +
		"### Security considerations\n\nNo input is read.\n\n### Out of scope\n\nMetrics.\n\n")
	order := []string{"## Summary\n\nAdds a health check.", "## How it works\n\nOne handler.",
		"## Tests\n\nA table test.", "## Security\n\nNo input is read.",
		"## Out of scope\n\nMetrics.", "https://example.invalid/issues/3", "revision 2",
		"`0123456789ab`", "@alice", "`abc123`", "automated agent"}
	rest := body
	for _, want := range order {
		_, after, found := strings.Cut(rest, want)
		if !found {
			t.Fatalf("%q is missing or out of order in:\n%s", want, body)
		}
		rest = after
	}
	for _, left := range []string{"A framework", "Write it", "Standard library"} {
		if strings.Contains(body, left) {
			t.Errorf("%q should stay on the ticket:\n%s", left, body)
		}
	}
}

func TestPullRequestBodyKeepsADesignWithoutKnownHeadings(t *testing.T) {
	body := describe("Just make it work.")
	if !strings.Contains(body, "## Design\n\nJust make it work.") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestPullRequestBodyCannotActOnAnything(t *testing.T) {
	body := describe("### Scope\n\nCloses #1, fixes: #2 and resolved owner/repo#3. " +
		"Ask @bob. <!-- x -->\n### Unknown\n\nStays here.\n\n### Tests\n\nA fixed value, and PR #2 is unrelated.\n")
	for _, bad := range []string{"Closes #1", "fixes: #2", "resolved owner/repo#3", "@bob", "<!-- x"} {
		if strings.Contains(body, bad) {
			t.Errorf("%q survived in:\n%s", bad, body)
		}
	}
	for _, want := range []string{"`Closes` #1", "Stays here", "A fixed value, and PR #2"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q is missing from:\n%s", want, body)
		}
	}
}

func describeFrom(c *changeDescription) string {
	task := core.Task{Approval: &core.Approval{Hash: "0123456789abcdef",
		Approver: core.Actor{Login: "alice"}}}
	return pullRequestBody(task, "### Scope\n\nFrom the design.", c,
		core.Issue{URL: "https://example.invalid/issues/3"},
		workflow.PublishPayload{SHA: "abc123", Revision: 2})
}

func TestPullRequestBodyPrefersWhatTheDiffShows(t *testing.T) {
	body := describeFrom(&changeDescription{Summary: "Added a helper.",
		Changes: []string{"Moved the check"}, Testing: "Added a test.",
		Files: []string{"main.go", "main_test.go"}})
	order := []string{"## Summary\n\nAdded a helper.", "## Changes\n\n- Moved the check",
		"## Testing\n\nAdded a test.", "## Files\n\n- `main.go`\n- `main_test.go`", "`abc123`"}
	rest := body
	for _, want := range order {
		_, after, found := strings.Cut(rest, want)
		if !found {
			t.Fatalf("%q is missing or out of order in:\n%s", want, body)
		}
		rest = after
	}
	if strings.Contains(body, "From the design") {
		t.Fatalf("the design should only stand in:\n%s", body)
	}
}

func TestPullRequestBodyFromADiffCannotActOnAnything(t *testing.T) {
	body := describeFrom(&changeDescription{Summary: "Closes #1 for @bob <!-- x -->",
		Changes: []string{"fixes owner/repo#3"}, Testing: "resolves #4", Partial: true})
	for _, bad := range []string{"Closes #1", "@bob", "<!-- x", "fixes owner", "resolves #4"} {
		if strings.Contains(body, bad) {
			t.Errorf("%q survived in:\n%s", bad, body)
		}
	}
	if !strings.Contains(body, diffCutDescription) {
		t.Errorf("a partial read is not mentioned:\n%s", body)
	}
}

func TestParseDescription(t *testing.T) {
	block := func(s string) string { return "```goblins-description\n" + s + "\n```" }
	d, err := parseDescription("Quoting " + block(`{"summary":"old"}`) + "\nthen\n" +
		block(`{"summary":" new ","changes":["a\nb",""," "],"testing":"t"}`))
	if err != nil || d.Summary != "new" || len(d.Changes) != 1 || d.Changes[0] != "a b" {
		t.Fatalf("%+v, %v", d, err)
	}
	for name, bad := range map[string]string{"no block": "just text", "not JSON": block("nope"),
		"no summary": block(`{"changes":["a"]}`)} {
		if _, err := parseDescription(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDiffFiles(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n+diff --git a/fake b/fake\n" +
		"diff --git a/dir with space/x.go b/dir with space/x.go\n--- a/y\n"
	got := strings.Join(diffFiles(diff), "|")
	if got != "main.go|dir with space/x.go" {
		t.Fatal(got)
	}
}
