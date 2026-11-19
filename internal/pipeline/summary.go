package pipeline

import (
	"strings"
	"unicode"

	"github.com/ungarscool1/goblins/internal/workflow"
)

// A summary is read before the design, so each point stays short. Longer ones are not
// rejected: the summary is replaced by one derived from the design, which is always kept.
const (
	maxSummaryField   = 600
	maxDerivedSummary = 280
)

type designSummary struct {
	Change     string `json:"change"`
	Approach   string `json:"approach"`
	Risks      string `json:"risks"`
	Validation string `json:"validation"`
}

func (s designSummary) points() [][2]string {
	return [][2]string{{"Change", s.Change}, {"Approach", s.Approach}, {"Risks", s.Risks},
		{"Validation", s.Validation}}
}

func (s designSummary) usable() bool {
	for _, p := range s.points() {
		v := strings.TrimSpace(p[1])
		if v == "" || len(v) > maxSummaryField {
			return false
		}
	}
	return true
}

// renderSummary writes the summary section that opens a design. It uses the planner's own
// summary when it has a usable one and otherwise derives one from the design, so an older
// planner, a fake or a malformed summary still gets a short view without another model run.
func renderSummary(d designDoc) string {
	s := derivedSummary(d)
	if d.Summary != nil && d.Summary.usable() {
		s = *d.Summary
	}
	var b strings.Builder
	b.WriteString(workflow.DesignSummaryHeading)
	for _, p := range s.points() {
		b.WriteString("**" + p[0] + ":** " + flatten(p[1]) + "\n\n")
	}
	return b.String()
}

// derivedSummary takes the first sentence of the sections that answer the summary's
// questions. It is deterministic, and it only ever repeats what the design says.
func derivedSummary(d designDoc) designSummary {
	return designSummary{Change: firstSentence(d.Scope), Approach: firstSentence(d.Architecture),
		Risks: firstSentence(d.Security), Validation: firstSentence(d.Tests)}
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	for i, r := range s {
		if (r == '.' || r == '!' || r == '?') && (i+1 == len(s) || unicode.IsSpace(rune(s[i+1]))) {
			s = s[:i+1]
			break
		}
	}
	if len(s) > maxDerivedSummary {
		cut := strings.LastIndexByte(s[:maxDerivedSummary], ' ')
		if cut <= 0 {
			cut = maxDerivedSummary
		}
		s = strings.ToValidUTF8(s[:cut], "") + "..."
	}
	return s
}

// flatten keeps a summary point to one block that cannot start a heading: the design's
// sections begin at the first heading after the summary, so the summary must not hold one.
// Line breaks stay, so the approach can be a short list.
func flatten(s string) string {
	var kept []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
			continue
		case strings.HasPrefix(l, "#"), strings.Trim(l, "-=") == "":
			l = `\` + l
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
