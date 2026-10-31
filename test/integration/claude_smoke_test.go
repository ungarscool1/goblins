//go:build smoke

// Opt-in smoke test of the real Claude CLI. It CONSUMES SUBSCRIPTION USAGE, so it
// runs only with the smoke tag and GOBLINS_SMOKE_CLAUDE=1, and needs a login in the
// local CLI. It sends one tiny prompt with no workspace.
//
//	GOBLINS_SMOKE_CLAUDE=1 go test -tags smoke -run ClaudeSmoke -v ./test/integration/
package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestClaudeSmokeRestrictedStream(t *testing.T) {
	if os.Getenv("GOBLINS_SMOKE_CLAUDE") != "1" {
		t.Skip("set GOBLINS_SMOKE_CLAUDE=1 to spend subscription usage on this test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--output-format", "stream-json", "--verbose", "--restricted", "--strict-mcp-config",
		"--disable-slash-commands", "--tools", "Read", "--allowedTools", "Read", "--permission-mode", "dontAsk", "Reply with exactly: pong")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{`"subtype":"init"`, `"session_id"`, `"type":"result"`, `"apiKeySource":"none"`} {
		if !strings.Contains(s, want) {
			t.Errorf("stream lacks %s:\n%s", want, s)
		}
	}
}
