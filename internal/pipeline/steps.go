package pipeline

import (
	"regexp"
	"strings"

	"github.com/ungarscool1/goblins/internal/observability"
)

var stepName = regexp.MustCompile(`^goblins-j-[0-9a-f]+-(.+)-\d+$`)

// stepOf is what a workspace step does, from its workload name. The whole name is no use to a
// person, and its job ID reads as a token to the redaction of messages.
func stepOf(name string) string {
	if m := stepName.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return "workspace"
}

// lastLine is the end of what a failed step wrote, such as Git's own error, redacted and short,
// so that the person told it failed is also told why.
func lastLine(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if line == "" {
		return ""
	}
	if len(line) > 200 {
		line = strings.ToValidUTF8(line[:200], "") + "..."
	}
	return ": " + observability.RedactText(line)
}
