package contract

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func sprint(v any) string { return fmt.Sprintf("%+v %#v %v", v, v, v) }

func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	s := strings.ToLower(strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(t.Name()))
	if len(s) > 30 {
		s = s[len(s)-30:]
	}
	return fmt.Sprintf("goblins-test-%s-%s-%d", prefix, strings.Trim(s, "-"), time.Now().UnixNano()%1_000_000_000)
}
