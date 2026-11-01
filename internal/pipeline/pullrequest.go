package pipeline

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// The parts of a design that tell a reviewer what a pull request is, under the headings they
// get there. Alternatives and the plan stay on the ticket, with the rest of the design.
var pullRequestSections = []struct{ design, heading string }{
	{"Scope", "Summary"},
	{"Architecture", "How it works"},
	{"Tests", "Tests"},
	{"Security considerations", "Security"},
	{"Out of scope", "Out of scope"},
}

// An SCM can act on these when a pull request merges, such as closing another ticket.
var closingReference = regexp.MustCompile(
	`(?i)\b(close[sd]?|fix(?:e[sd])?|resolve[sd]?)(:?\s*(?:#\d|[\w.-]+/[\w.-]+#\d|https?://))`)

// pullRequestBody describes the change from the approved design, with where it came from last.
func pullRequestBody(task core.Task, design string, issue core.Issue,
	payload workflow.PublishPayload) string {
	var b strings.Builder
	sections := designSections(design)
	for _, s := range pullRequestSections {
		if text := sections[s.design]; text != "" {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.heading, safeDescription(text))
		}
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "## Design\n\n%s\n\n", safeDescription(design))
	}
	link := issue.URL
	if link == "" {
		link = issue.Ref.ExternalID
	}
	fmt.Fprintf(&b, "---\n\nFor %s. Design revision %d (`%s`), approved by @%s. ", link,
		payload.Revision, task.Approval.Hash[:12], task.Approval.Approver.Login)
	fmt.Fprintf(&b, "Validated commit: `%s`.\n\nWritten by an automated agent. "+
		"Goblins does not merge pull requests.", payload.SHA)
	return b.String()
}

// designSections splits a rendered design by its known headings. A heading the model wrote
// inside a section stays part of that section.
func designSections(design string) map[string]string {
	known := map[string]bool{"Alternatives considered": true, "Constraints": true,
		"Implementation plan": true}
	for _, s := range pullRequestSections {
		known[s.design] = true
	}
	sections := map[string]string{}
	current := ""
	for line := range strings.Lines(design) {
		name, isHeading := strings.CutPrefix(strings.TrimSpace(line), "### ")
		if isHeading && known[name] {
			current = name
			continue
		}
		if current != "" {
			sections[current] += line
		}
	}
	for name, text := range sections {
		sections[name] = strings.TrimSpace(text)
	}
	return sections
}

// safeDescription keeps model output from mentioning anyone, carrying a marker or acting on
// another ticket when the pull request merges.
func safeDescription(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "<!--", "<! --")
	s = strings.ReplaceAll(s, "@", "@\u200b")
	return closingReference.ReplaceAllString(s, "`${1}`${2}")
}
