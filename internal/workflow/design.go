package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// DesignSummaryHeading opens a design's summary. The planner stage writes it first, and the
// design's own sections start at the next heading, so a summary never holds a heading.
const DesignSummaryHeading = "### Summary\n\n"

// DesignStyle says how a design comment is laid out. It changes what people see first, never
// what is stored, hashed, approved or handed to the implementer: that is always the full text.
type DesignStyle int

const (
	// DesignCompact shows the summary and puts the full design in a collapsed section.
	DesignCompact DesignStyle = iota
	// DesignCompactPlain shows the summary first and the full design after it, for trackers
	// that cannot collapse a section.
	DesignCompactPlain
	// DesignVerbose shows everything expanded, as one document.
	DesignVerbose
)

// SplitDesign separates a design's summary from its sections. A design written before
// summaries existed has none, and is all sections.
func SplitDesign(body string) (summary, sections string) {
	body = strings.TrimSpace(body)
	rest, ok := strings.CutPrefix(body, DesignSummaryHeading)
	if !ok {
		return "", body
	}
	i := strings.Index(rest, "\n### ")
	if i < 0 {
		return strings.TrimSpace(rest), ""
	}
	return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+1:])
}

// The design is model output. These would let it close the collapsed section early, open
// one around the approval instruction, or pose as a comment marker.
var designMarkup = regexp.MustCompile(`(?i)<(/?)(details|summary)\b`)

func inertDesign(s string) string {
	s = designMarkup.ReplaceAllString(strings.ToValidUTF8(s, "�"), "&lt;$1$2")
	return strings.ReplaceAll(s, "<!--", "<! --")
}

func (m *machine) renderDesign(hash, body string) string {
	return RenderDesign(m.t.DesignRevision, hash, body, m.p.ApproveCommand, m.p.DesignStyle)
}

// RenderDesign formats a design revision with its approval instruction. The instruction,
// revision and hash are always outside the collapsed part, after all model text.
func RenderDesign(rev int, hash, body, approveCmd string, style DesignStyle) string {
	summary, sections := SplitDesign(inertDesign(body))
	var b strings.Builder
	fmt.Fprintf(&b, "## Proposed design, revision %d\n\n", rev)
	switch {
	case summary == "" || style == DesignVerbose:
		if summary != "" {
			b.WriteString(summary + "\n\n")
		}
		b.WriteString(sections + "\n\n")
	case style == DesignCompactPlain:
		b.WriteString(summary + "\n\n---\n**Full technical design**\n\n" + sections + "\n\n")
	default:
		b.WriteString(summary + "\n\n<details>\n<summary>Full technical design</summary>\n\n" +
			sections + "\n\n</details>\n\n")
	}
	fmt.Fprintf(&b, "---\n**Approve:** comment `%s %d` to approve this exact revision, "+
		"full design included (design hash `%s`). Any other comment from a maintainer asks for "+
		"a revision. Nothing is implemented before approval.", approveCmd, rev, hash[:12])
	return b.String()
}
