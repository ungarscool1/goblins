package workflow

import (
	"fmt"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

const maxFeedback = 16 * 1024

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[... output truncated ...]\n" + s[len(s)-n:]
}

// RenderQuestions formats planner questions as a ticket comment.
func RenderQuestions(qs []string) string {
	var b strings.Builder
	b.WriteString("I have some questions before proposing a design:\n\n")
	for i, q := range qs {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(q))
	}
	b.WriteString("\nReply in a comment. Nothing will be implemented until a design is approved.")
	return b.String()
}

// RenderDesign formats a design revision with its approval instruction.
func RenderDesign(rev int, hash, body, approveCmd string) string {
	return fmt.Sprintf("## Proposed design, revision %d\n\n%s\n\n---\nTo approve this exact revision, comment `%s %d` (design hash `%s`). "+
		"Any other comment from a maintainer asks for a revision. Nothing is implemented before approval.",
		rev, strings.TrimSpace(body), approveCmd, rev, hash[:12])
}

// RenderFailure formats failed checks for the next implementation round. The
// content is repository output: it is passed on as data, never interpreted.
func RenderFailure(rep core.ValidationReport) string {
	var b strings.Builder
	b.WriteString(rep.Summary + "\n")
	for _, c := range rep.Checks {
		if c.Status == core.ValidationPassed {
			continue
		}
		fmt.Fprintf(&b, "\n### %s (exit %d)\n%s\n", c.Name, c.ExitCode, c.Output)
	}
	return tail(b.String(), maxFeedback)
}

func prLink(pr core.PullRequest) string {
	if pr.URL != "" {
		return pr.URL
	}
	return "#" + pr.Number
}

// RenderReport formats the final status report for the ticket.
func RenderReport(t *core.Task, rep core.ValidationReport) string {
	var b strings.Builder
	b.WriteString("## Status report\n\n")
	if t.PullRequest != nil {
		fmt.Fprintf(&b, "- Pull request: %s (not merged, it awaits your review)\n", prLink(*t.PullRequest))
	}
	if t.HeadSHA != "" {
		fmt.Fprintf(&b, "- Commit: `%s`\n", t.HeadSHA)
	}
	if t.Approval != nil {
		fmt.Fprintf(&b, "- Design: revision %d, approved by @%s\n", t.Approval.Revision, t.Approval.Approver.Login)
	}
	fmt.Fprintf(&b, "- Validation: %s. %s\n", rep.Status, rep.Summary)
	for _, c := range rep.Checks {
		fmt.Fprintf(&b, "  - %s: %s\n", c.Name, c.Status)
	}
	return b.String()
}
