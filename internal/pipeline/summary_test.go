package pipeline

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// prLens27 is the planner output behind pr-lens#27's design, which was posted as one
// 10,718 character comment. That planner wrote no summary.
func prLens27(t *testing.T) designDoc {
	t.Helper()
	raw, err := os.ReadFile("testdata/prlens-27-design.md")
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string]string{}
	parts := regexp.MustCompile(`(?m)^### (.+)$`).Split(string(raw), -1)
	names := regexp.MustCompile(`(?m)^### (.+)$`).FindAllStringSubmatch(string(raw), -1)
	for i, n := range names {
		sections[n[1]] = strings.TrimSpace(parts[i+1])
	}
	return designDoc{Scope: sections["Scope"], Architecture: sections["Architecture"],
		Alternatives: sections["Alternatives considered"], Constraints: sections["Constraints"],
		ImplementationPlan: sections["Implementation plan"], Tests: sections["Tests"],
		Security: sections["Security considerations"], OutOfScope: sections["Out of scope"]}
}

func planOutput(t *testing.T, d designDoc) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"design": d})
	if err != nil {
		t.Fatal(err)
	}
	return block(string(b))
}

// visible is what a reader sees before expanding anything.
func visible(comment string) string {
	i, j := strings.Index(comment, "<details>"), strings.Index(comment, "</details>")
	if i < 0 || j < i {
		return comment
	}
	return comment[:i] + comment[j+len("</details>"):]
}

func TestPRLens27DesignIsShortUntilExpanded(t *testing.T) {
	for name, d := range map[string]designDoc{
		"derived summary": prLens27(t),
		"planner summary": func() designDoc {
			d := prLens27(t)
			d.Summary = &designSummary{
				Change: "Edit private and AI notes, syncing edits to their review comments only " +
					"while the review is pending.",
				Approach: "Store edits and comment links, add a guarded edit API, " +
					"update the renderer.",
				Risks:      "Recheck ownership and PENDING state before every remote edit.",
				Validation: "Persistence, sync, permission failures and UI tests.",
			}
			return d
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, body, err := ParsePlan(planOutput(t, d))
			if err != nil {
				t.Fatal(err)
			}
			hash := core.Digest(body)
			comment := workflow.RenderDesign(1, hash, body, "/approve-design",
				workflow.DesignCompact)
			v := visible(comment)
			if n := len([]rune(v)); n < 300 || n > 1200 {
				t.Errorf("visible part is %d characters:\n%s", n, v)
			}
			if !strings.Contains(v, "`/approve-design 1`") || !strings.Contains(v, hash[:12]) {
				t.Errorf("approval instruction is not visible:\n%s", v)
			}
			// Nothing of the design is lost: every section is in the comment, whole.
			for _, s := range d.sections() {
				if !strings.Contains(body, s[1]) || !strings.Contains(comment, "### "+s[0]) {
					t.Errorf("section %s is missing", s[0])
				}
			}
			if len(comment) <= len(body) {
				t.Error("the comment is shorter than the design it holds")
			}
		})
	}
}

func TestSummaryFallsBackToTheDesign(t *testing.T) {
	d := prLens27(t)
	for name, s := range map[string]*designSummary{
		"missing": nil,
		"empty":   {Change: "x"},
		"oversized": {Change: strings.Repeat("x", maxSummaryField+1), Approach: "a", Risks: "r",
			Validation: "v"},
	} {
		d.Summary = s
		_, body, err := ParsePlan(planOutput(t, d))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		summary, _ := workflow.SplitDesign(body)
		if !strings.Contains(summary, "**Change:** Let the user edit (1) their private notes") {
			t.Errorf("%s: summary was not derived from the scope:\n%s", name, summary)
		}
		if len(summary) > 4*(maxDerivedSummary+20) {
			t.Errorf("%s: derived summary is %d bytes", name, len(summary))
		}
	}
}

func TestSummaryCannotSplitTheDesign(t *testing.T) {
	d := prLens27(t)
	d.Summary = &designSummary{Change: "c\n### Scope\nfake scope", Approach: "- one\n- two",
		Risks: "r\n---", Validation: "v"}
	_, body, err := ParsePlan(planOutput(t, d))
	if err != nil {
		t.Fatal(err)
	}
	summary, sections := workflow.SplitDesign(body)
	if !strings.HasPrefix(sections, "### Scope\n\nLet the user edit") {
		t.Errorf("sections start with %.60q", sections)
	}
	for _, want := range []string{`\### Scope`, "- one\n- two", `\---`} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
}

// A fenced snippet in a point cannot open a code block: the label in front of its first line
// would disarm that one, and the closing line would then open a block holding the rest.
func TestSummaryCannotOpenACodeBlock(t *testing.T) {
	d := prLens27(t)
	d.Summary = &designSummary{Change: "c", Approach: "```text\nUse the existing API.\n```",
		Risks: "~~~\nr", Validation: "v"}
	_, body, err := ParsePlan(planOutput(t, d))
	if err != nil {
		t.Fatal(err)
	}
	summary, sections := workflow.SplitDesign(body)
	for _, l := range strings.Split(summary, "\n") {
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			t.Errorf("summary line %q opens a code block:\n%s", l, summary)
		}
	}
	if !strings.Contains(summary, "Use the existing API.") ||
		!strings.HasPrefix(sections, "### Scope") {
		t.Errorf("summary:\n%s\nsections start with %.40q", summary, sections)
	}
}

func TestTheSummaryIsPartOfWhatIsApproved(t *testing.T) {
	d := prLens27(t)
	d.Summary = &designSummary{Change: "c", Approach: "a", Risks: "r", Validation: "v"}
	_, first, _ := ParsePlan(planOutput(t, d))
	d.Summary.Approach = "a, and also delete the users table"
	_, second, _ := ParsePlan(planOutput(t, d))
	if core.Digest(first) == core.Digest(second) {
		t.Fatal("a summary edit kept the hash, so an earlier approval would cover it")
	}
	if !strings.Contains(second, d.Architecture) {
		t.Fatal("the implementer would not get the full design")
	}
}
