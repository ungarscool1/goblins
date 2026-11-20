package pipeline

import (
	"encoding/base64"
	"strings"
)

// changedPaths decodes the paths a commit step reported. They are repository content, so
// anything unreadable yields none, which no scope accepts.
func changedPaths(b64 string) []string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
