package pipeline

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"github.com/ungarscool1/goblins/internal/core"
)

const (
	maxHeaderBytes = 100
	fallbackType   = "chore"
)

const commitInstruction = `

When you finish, end your reply with exactly one fenced block tagged goblins-commit holding the
subject of the commit, as a Conventional Commit: type(optional scope): what this change does, in
the imperative and in lower case. Describe this change, not the whole ticket. The type is one of
feat, fix, docs, style, refactor, perf, test, build, ci, chore or revert. Only if this change
breaks what existing users of the project rely on, add ! after the type or scope, as in feat!: .
` + "```goblins-commit" + `
feat(api): add a health check endpoint
` + "```"

var (
	commitBlock = regexp.MustCompile("(?s)```goblins-commit\\s*\\n(.*?)\\n?```")
	// An upper case type or a missing space is rejected, not repaired: a model that gets the
	// form wrong may have got the rest wrong too, and the ticket's title is there instead.
	conventionalHeader = regexp.MustCompile(
		`^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)` +
			`(\([a-z0-9][a-z0-9._/-]*\))?!?: (\S.*)$`)
	// A commit subject carrying one of these keeps CI from running on that commit.
	ciDirective = regexp.MustCompile(`(?i)\[[^\]]*\b(skip|no ci)\b[^\]]*\]|\*\*\*no_ci\*\*\*`)
)

// implementerInstructions asks the implementer to word its commit, when it has a say in that.
func implementerInstructions(pr *Project) string {
	if pr.PlainCommits {
		return implementerSystem
	}
	return implementerSystem + commitInstruction
}

// commitHeader words the subject of a commit: what the implementer reports having done or,
// failing that, the ticket's title. The implementer's reply is untrusted output.
func commitHeader(pr *Project, title, output string) string {
	if pr.PlainCommits {
		return safeHeader(title)
	}
	blocks := commitBlock.FindAllStringSubmatch(output, -1)
	if len(blocks) > 0 {
		if header, ok := parseHeader(blocks[len(blocks)-1][1]); ok {
			return header
		}
	}
	return titleHeader(title)
}

// pullRequestTitle words a pull request's title, which becomes the commit subject when a pull
// request is squashed. It covers the whole diff, so it comes from the description.
func pullRequestTitle(pr *Project, title string, change *changeDescription) string {
	if pr.PlainCommits {
		return safeHeader(title)
	}
	if change != nil {
		if header, ok := parseHeader(change.Title); ok {
			return header
		}
	}
	return titleHeader(title)
}

// publishedTitle is the title a push leaves on its pull request. An open pull request keeps its
// title, which a person may have edited, since the same ticket and design are behind every push.
// Only the fallback gives way, once a description words a real title.
func publishedTitle(ctx context.Context, pr *Project, task core.Task,
	change *changeDescription) (string, error) {
	title := pullRequestTitle(pr, task.Title, change)
	if task.PullRequest == nil {
		return title, nil
	}
	current, err := pr.SCM.GetPullRequest(ctx, pr.Repo, task.PullRequest.Number)
	if core.IsKind(err, core.KindNotFound) {
		return title, nil
	}
	if err != nil {
		return "", err
	}
	if !pr.PlainCommits && current.Title == titleHeader(task.Title) {
		return title, nil
	}
	return current.Title, nil
}

// titleHeader makes a header of a ticket's title, which says nothing about the kind of change
// that came of it. The fallback type does not pass for a feature or a fix, which release tools
// act on.
func titleHeader(title string) string {
	if header, ok := parseHeader(title); ok {
		return header
	}
	return fallbackType + ": " + lowerFirst(safeHeader(title))
}

// parseHeader validates a single line against the Conventional Commits form.
func parseHeader(s string) (string, bool) {
	header := safeHeader(s)
	m := conventionalHeader.FindStringSubmatch(header)
	if m == nil || len(header) > maxHeaderBytes || strings.Contains(strings.TrimSpace(s), "\n") {
		return "", false
	}
	return header[:len(header)-len(m[3])] + lowerFirst(m[3]), true
}

// safeHeader makes text fit to be a commit subject: one line that cannot turn off CI, mention
// anyone or act on another ticket.
func safeHeader(s string) string {
	s = strings.Join(strings.Fields(ciDirective.ReplaceAllString(s, "")), " ")
	return strings.TrimRight(safeDescription(s), ".")
}

// lowerFirst lowers a leading capital, except in what looks like an acronym, such as "API".
func lowerFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 || (len(r) > 1 && unicode.IsUpper(r[1])) {
		return s
	}
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
