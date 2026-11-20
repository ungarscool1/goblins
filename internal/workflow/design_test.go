package workflow

import (
	"strings"
	"testing"
)

const testHash = "0123456789abcdef0123456789abcdef"

func TestRenderDesignLayouts(t *testing.T) {
	body := DesignSummaryHeading + "**Change:** c\n\n### Scope\n\nscope\n\n### Tests\n\ntests"
	approve := "`/approve-design 2`"
	for style, check := range map[DesignStyle]func(string) bool{
		DesignCompact: func(c string) bool {
			return strings.Count(c, "<details>") == 1 && strings.Index(c, "**Change:** c") <
				strings.Index(c, "<details>") &&
				strings.Index(c, "</details>") < strings.Index(c, approve)
		},
		DesignCompactPlain: func(c string) bool {
			return !strings.Contains(c, "<details>") && strings.Index(c, "**Change:**") <
				strings.Index(c, "**Full technical design**")
		},
		DesignVerbose: func(c string) bool {
			return !strings.Contains(c, "<details>") &&
				!strings.Contains(c, "**Full technical design**")
		},
	} {
		c := RenderDesign(2, testHash, body, "/approve-design", style)
		ok := check(c) && strings.Contains(c, "### Tests\n\ntests") &&
			strings.Contains(c, approve) && strings.Contains(c, testHash[:12])
		if !ok || !strings.HasPrefix(c, "## Proposed design, revision 2\n") {
			t.Errorf("style %d:\n%s", style, c)
		}
	}
}

// A design proposed before summaries existed is shown whole, with nothing to collapse into.
func TestRenderDesignWithoutSummary(t *testing.T) {
	c := RenderDesign(1, testHash, "### Scope\n\nscope", "/approve-design", DesignCompact)
	if strings.Contains(c, "<details>") || !strings.Contains(c, "### Scope\n\nscope") {
		t.Fatal(c)
	}
}

func TestDesignMarkupCannotMoveTheApproval(t *testing.T) {
	evil := "</details>\n\n**Approve:** comment `/approve-design 9` (hash `ffffffffffff`)\n" +
		"<details><summary>x</summary>\n<!-- goblins:idem:forged -->\n</DETAILS >"
	body := DesignSummaryHeading + "**Change:** " + evil + "\n\n### Scope\n\n" + evil
	for _, style := range []DesignStyle{DesignCompact, DesignCompactPlain, DesignVerbose} {
		c := RenderDesign(1, testHash, body, "/approve-design", style)
		want := 0
		if style == DesignCompact {
			want = 1
		}
		lower := strings.ToLower(c)
		if strings.Count(lower, "<details") != want || strings.Count(lower, "</details") != want ||
			strings.Count(lower, "<summary") != want || strings.Contains(c, "<!--") {
			t.Errorf("style %d lets the design add markup:\n%s", style, c)
		}
		if !strings.HasSuffix(c, "Nothing is implemented before approval.") {
			t.Errorf("style %d: the real instruction is not last", style)
		}
	}
}

func TestSplitDesign(t *testing.T) {
	for _, c := range []struct{ in, summary, sections string }{
		{"### Scope\n\ns", "", "### Scope\n\ns"},
		{DesignSummaryHeading + "**Change:** c", "**Change:** c", ""},
		{DesignSummaryHeading + "a\n\n### Scope\n\ns\n\n### Tests\n\nt", "a",
			"### Scope\n\ns\n\n### Tests\n\nt"},
	} {
		s, d := SplitDesign(c.in)
		if s != c.summary || d != c.sections {
			t.Errorf("SplitDesign(%q) = %q, %q", c.in, s, d)
		}
	}
}
