package jira

import (
	"regexp"
	"testing"
)

func TestToWiki(t *testing.T) {
	for in, want := range map[string]string{
		"## Proposed design, revision 2":           "h2. Proposed design, revision 2",
		"**Approve:** comment `/approve-design 2`": "*Approve:* comment {{/approve-design 2}}",
		"- one\n  - two\n1. three":                 "* one\n** two\n# three",
		"---":                                      "----",
		"see [the PR](https://x.test/pr/1)":        "see [the PR|https://x.test/pr/1]",
		"```go\nfmt.Println(\"{code}\")\n```":      "{code}\nfmt.Println(\"{ code}\")\n{code}",
		"unclosed\n```\nx":                         "unclosed\n{code}\nx\n{code}",
	} {
		if got := ToWiki(in); got != want {
			t.Errorf("ToWiki(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Text from a ticket or a model cannot notify anyone or run a macro.
func TestToWikiEscapesWhatJiraWouldActOn(t *testing.T) {
	out := ToWiki("hi [~accountid:5b10] and [~admin] {html}<script>{html} | !https://x.test/p.png!")
	unescaped := regexp.MustCompile(`(^|[^\\])(\[~|\{html|!h)`)
	if unescaped.MatchString(out) {
		t.Errorf("markup survived: %q", out)
	}
}
