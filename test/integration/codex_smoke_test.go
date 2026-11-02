//go:build smoke

// Opt-in smoke test of the real Codex CLI. It CONSUMES SUBSCRIPTION USAGE, so it runs only with
// the smoke tag and GOBLINS_SMOKE_CODEX=1, and needs a ChatGPT login in the local CLI. It sends
// one tiny prompt, with the same restrictions as the adapter.
//
//	GOBLINS_SMOKE_CODEX=1 go test -tags smoke -run CodexSmoke -v ./test/integration/
package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCodexSmokeRestrictedStream(t *testing.T) {
	if os.Getenv("GOBLINS_SMOKE_CODEX") != "1" {
		t.Skip("set GOBLINS_SMOKE_CODEX=1 to spend subscription usage on this test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	status, err := exec.CommandContext(ctx, "codex", "login", "status").CombinedOutput()
	if err != nil || !strings.Contains(string(status), "ChatGPT") {
		t.Fatalf("not a ChatGPT login: %v\n%s", err, status)
	}
	cmd := exec.CommandContext(ctx, "codex", "exec", "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check",
		"--disable", "shell_tool", "--disable", "unified_exec", "-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`,
		"-c", `forced_login_method="chatgpt"`, "Reply with exactly: pong")
	cmd.Dir = t.TempDir()
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENAI_") && !strings.HasPrefix(kv, "CODEX_API") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{`"thread.started"`, `"agent_message"`, `"turn.completed"`, "pong"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"command_execution"`) {
		t.Fatalf("a command ran with the command tools off:\n%s", s)
	}
}
