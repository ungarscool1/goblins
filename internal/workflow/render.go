package workflow

import (
	"fmt"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
)

const (
	maxFeedback = 16 * 1024
	// A ticket comment has to stay readable and well under trackers' size limits.
	maxTicketCheckOutput = 4 * 1024
	maxTicketOutput      = 32 * 1024
)

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

// RenderFailedChecks formats failed checks for a person reading the ticket, who has no
// other way to see why validation failed once the containers are gone. The ticket can be
// public and the output is repository-controlled, so it is redacted first (a cut can
// remove the delimiter that makes a secret recognizable), fenced so that it cannot format
// the comment or mention anyone, and kept from carrying a comment marker.
func RenderFailedChecks(rep core.ValidationReport) string {
	var b strings.Builder
	for _, c := range rep.Checks {
		if c.Status == core.ValidationPassed {
			continue
		}
		if b.Len() >= maxTicketOutput {
			b.WriteString("Output of further failed checks is omitted.\n\n")
			break
		}
		output := tail(observability.RedactText(c.Output), maxTicketCheckOutput)
		output = strings.ReplaceAll(strings.ToValidUTF8(output, "\uFFFD"), "<!--", "<! --")
		fmt.Fprintf(&b, "**%s** (exit %d):\n\n%s\n\n", c.Name, c.ExitCode, fenced(strings.TrimSpace(output)))
	}
	return strings.TrimSpace(b.String())
}

// fenced wraps text in a code fence longer than any run of backticks inside it.
func fenced(s string) string {
	longest, run := 2, 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", longest+1)
	return fence + "\n" + s + "\n" + fence
}

// RenderReviewFeedback formats findings for the implementer. They are a reviewer's words:
// they are passed on as data, and the implementer's scope stays what the design says.
func RenderReviewFeedback(findings []core.ReviewFinding, sha string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Findings of the review of commit %s:\n", sha)
	for i, f := range findings {
		fmt.Fprintf(&b, "\n%d. [%s, %s confidence] %s", i+1, f.Severity, f.Confidence,
			findingLocation(f))
		b.WriteString("\n" + strings.TrimSpace(f.Body) + "\n")
	}
	return tail(b.String(), maxFeedback)
}

// RenderFindingList formats findings for a person reading the ticket. They are model
// output, so they are fenced: they cannot format the comment or mention anyone.
func RenderFindingList(findings []core.ReviewFinding) string {
	var b strings.Builder
	for _, f := range findings {
		body := strings.ReplaceAll(strings.ToValidUTF8(f.Body, "\uFFFD"), "<!--", "<! --")
		fmt.Fprintf(&b, "**%s** %s:\n\n%s\n\n", f.Severity, findingLocation(f),
			fenced(tail(strings.TrimSpace(body), maxTicketCheckOutput)))
		if b.Len() >= maxTicketOutput {
			b.WriteString("Further findings are omitted.\n\n")
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func findingLocation(f core.ReviewFinding) string {
	switch {
	case f.Path == "":
		return "(no location)"
	case f.Line > 0:
		return fmt.Sprintf("`%s:%d`", f.Path, f.Line)
	}
	return "`" + f.Path + "`"
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
