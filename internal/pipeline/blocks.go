package pipeline

import "strings"

// lastBlock returns what follows the opening line of the last fenced block with this info
// string in an agent's output, since earlier text may quote one. For JSON, a decoder reads one
// value from it and stops there, so the closing fence is never looked for: a snippet with its
// own fences inside a string, such as code in a design or a review, cannot cut the value short.
func lastBlock(output, info string) (string, bool) {
	open := "```" + info
	for end := len(output); ; {
		i := strings.LastIndex(output[:end], open)
		if i < 0 {
			return "", false
		}
		rest := output[i+len(open):]
		line, body, found := strings.Cut(rest, "\n")
		if found && strings.TrimSpace(line) == "" {
			return body, true
		}
		end = i // a longer info string, or text on the opening line: not this block
	}
}
