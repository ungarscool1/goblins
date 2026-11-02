package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/codex"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

var ctx = context.Background()

func bin(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// setup returns an agent whose CLI sees FAKE_SCENARIO through a wrapper executable,
// because the adapter builds the environment itself.
func setup(t *testing.T, scenario string) (*codex.Agent, *fake.ProcessRuntime) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nFAKE_SCENARIO=" + scenario + " exec " + filepath.Join(bin(t), "codex") + " \"$@\"\n"
	exe := filepath.Join(dir, "codex-"+scenario)
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return codex.New("codex", codex.Options{Image: "img", HomeVolume: "home", Executable: exe}), fake.NewProcessRuntime("proc", t.TempDir(), bin(t))
}

func request(rt core.ExecutionRuntime, name string) core.AgentRequest {
	return core.AgentRequest{IdempotencyKey: name, WorkloadName: name, Role: core.RolePlanner, Prompt: "hello", Runtime: rt}
}

func invocations(t *testing.T, rt *fake.ProcessRuntime) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rt.VolumePath("home"), "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lastRun(t *testing.T, rt *fake.ProcessRuntime) string {
	t.Helper()
	var last string
	for l := range strings.SplitSeq(invocations(t, rt), "\n") {
		if strings.HasPrefix(l, "ARGS exec") && strings.HasSuffix(l, " -") {
			last = l
		}
	}
	return last
}

func TestContract(t *testing.T) {
	contract.AgentProvider(t, func(t *testing.T) (core.AgentProvider, core.ExecutionRuntime, string) {
		a, rt := setup(t, "ok")
		return a, rt, "echo: p"
	})
}

func TestRunStreamsAndResumes(t *testing.T) {
	a, rt := setup(t, "ok")
	var kinds []string
	res, err := a.Run(ctx, request(rt, "w1"), func(e core.AgentEvent) {
		kinds = append(kinds, string(e.Kind))
		if strings.Contains(e.Text, "secret thoughts") {
			t.Error("reasoning must not be forwarded")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "fake-thread-1" || res.Output != "echo: hello" {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.Join(kinds, ","); got != "session,tool,text,result" {
		t.Fatalf("events = %s", got)
	}
	if a.Info().Has(core.CapAgentResume) {
		req := request(rt, "w2")
		req.ResumeSessionID = "earlier-thread"
		res, err = a.Run(ctx, req, func(core.AgentEvent) {})
		if err != nil || res.SessionID != "earlier-thread" {
			t.Fatalf("resume = %+v, %v", res, err)
		}
		if got := lastRun(t, rt); !strings.HasPrefix(got, "ARGS exec resume ") || !strings.HasSuffix(got, " earlier-thread -") {
			t.Fatalf("invocation: %s", got)
		}
	}
}

func TestPolicyTurnsOffEveryCommandTool(t *testing.T) {
	cases := []struct {
		policy core.ToolPolicy
		want   []string
	}{
		{core.ToolPolicy{}, []string{`sandbox_mode="read-only"`, `web_search="disabled"`}},
		{core.ToolPolicy{WriteFiles: true}, []string{`sandbox_mode="workspace-write"`, `web_search="disabled"`}},
		{core.ToolPolicy{Web: true}, []string{`sandbox_mode="read-only"`, `web_search="live"`}},
	}
	for i, tc := range cases {
		a, rt := setup(t, "ok")
		req := request(rt, "policy")
		req.Policy = tc.policy
		req.Model, req.Effort, req.SystemPrompt = "gpt-fake", "high", `say "hi"`
		if _, err := a.Run(ctx, req, func(core.AgentEvent) {}); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		got := lastRun(t, rt)
		want := append(tc.want, "--ignore-user-config", "--ignore-rules", `forced_login_method="chatgpt"`, `approval_policy="never"`, "project_doc_max_bytes=0", "mcp_servers={}",
			"--disable shell_tool", "--disable unified_exec", "--disable browser_use", "--disable new_tool", "--model gpt-fake", `model_reasoning_effort="high"`, `developer_instructions="say \"hi\""`)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%d: missing %q in %s", i, w, got)
			}
		}
		for _, banned := range []string{"dangerously", "danger-full-access", "--disable item_ids", "--with-api-key", "--oss"} {
			if strings.Contains(got, banned) {
				t.Errorf("%d: %q present in %s", i, banned, got)
			}
		}
	}
}

func TestReadOnlyPolicyMountsWorkspaceReadOnly(t *testing.T) {
	a, rt := setup(t, "ok")
	req := request(rt, "ro")
	req.Workspace = core.Mount{Volume: "ws", Target: "/workspace"}
	if _, err := a.Run(ctx, req, func(core.AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	spec := rt.Specs[len(rt.Specs)-1]
	for _, m := range spec.Mounts {
		if m.Volume == "ws" && !m.ReadOnly {
			t.Fatalf("mounts = %+v", spec.Mounts)
		}
	}
	if !spec.Security.ReadOnlyRoot || spec.Security.Privileged {
		t.Fatalf("security = %+v", spec.Security)
	}
}

func TestACommandRunIsDiscarded(t *testing.T) {
	a, rt := setup(t, "ran_command")
	res, err := a.Run(ctx, request(rt, "cmd"), func(core.AgentEvent) {})
	if !core.IsKind(err, core.KindUnsupported) || res.Output != "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestAPIKeysNeverReachTheCLI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-orchestrator-env")
	t.Setenv("CODEX_API_KEY", "sk-from-orchestrator-env")
	a, rt := setup(t, "ok")
	if _, err := a.Run(ctx, request(rt, "env"), func(core.AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	got := invocations(t, rt)
	if strings.Contains(got, "orchestrator-env") || !strings.Contains(got, "OPENAI_API_KEY=[] CODEX_API_KEY=[]") {
		t.Fatalf("invocations:\n%s", got)
	}
	spec := rt.Specs[len(rt.Specs)-1]
	if !strings.Contains(strings.Join(spec.Command, " "), "unset OPENAI_API_KEY CODEX_API_KEY") {
		t.Fatalf("command does not clear billing variables: %v", spec.Command)
	}
	if len(spec.Secrets) != 0 {
		t.Fatalf("secrets: %v", spec.Secrets)
	}
}

func TestLoginProblemsStopTheRunBeforeAnyModelCall(t *testing.T) {
	for _, scenario := range []string{"logged_out", "api_key"} {
		t.Run(scenario, func(t *testing.T) {
			a, rt := setup(t, scenario)
			_, err := a.Run(ctx, request(rt, "w"), func(core.AgentEvent) {})
			if !core.IsKind(err, core.KindAuthRequired) || !strings.Contains(core.UserMessage(err), "codex-login") {
				t.Fatalf("err = %v", err)
			}
			if lastRun(t, rt) != "" {
				t.Fatal("the model was called")
			}
			if strings.Contains(core.UserMessage(err), "sk-proj") {
				t.Fatalf("key in message: %v", err)
			}
		})
	}
}

func TestFailuresAreClassified(t *testing.T) {
	cases := map[string]core.ErrorKind{
		"rate_limit": core.KindQuotaExhausted,
		"expired":    core.KindAuthRequired,
		"gone":       core.KindNotFound,
		"crash":      core.KindTransient,
		"no_result":  core.KindTransient,
	}
	for scenario, want := range cases {
		t.Run(scenario, func(t *testing.T) {
			a, rt := setup(t, scenario)
			res, err := a.Run(ctx, request(rt, "w-"+scenario), func(core.AgentEvent) {})
			if !core.IsKind(err, want) {
				t.Fatalf("err = %v, want %s", err, want)
			}
			if strings.Contains(core.UserMessage(err), "segfault") {
				t.Fatalf("raw CLI output in message: %v", err)
			}
			if scenario == "rate_limit" && res.SessionID == "" {
				t.Fatal("session lost on failure, so it could not be resumed")
			}
		})
	}
}

func TestCLIThatCannotBeRestrictedIsRefused(t *testing.T) {
	for scenario, want := range map[string]string{"old_cli": "--ignore-user-config", "no_feature": "shell_tool"} {
		t.Run(scenario, func(t *testing.T) {
			a, rt := setup(t, scenario)
			_, err := a.Run(ctx, request(rt, "w"), func(core.AgentEvent) {})
			if !core.IsKind(err, core.KindUnsupported) || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v", err)
			}
			if lastRun(t, rt) != "" {
				t.Fatal("a CLI without the restrictions was run")
			}
		})
	}
}

func TestModelCapabilitiesComeFromTheCatalog(t *testing.T) {
	a, rt := setup(t, "ok")
	if m, err := a.Models(ctx); err != nil || len(m) != 0 {
		t.Fatalf("models before a preflight = %v, %v", m, err)
	}
	if _, err := a.Run(ctx, request(rt, "first"), func(core.AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	m, err := a.Models(ctx)
	if err != nil || len(m) != 1 || m[0].ID != "gpt-fake" || strings.Join(m[0].Efforts, ",") != "low,high" {
		t.Fatalf("models = %+v, %v", m, err)
	}
	req := request(rt, "bad-effort")
	req.Model, req.Effort = "gpt-fake", "xhigh"
	if _, err := a.Run(ctx, req, func(core.AgentEvent) {}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("err = %v", err)
	}
	req = request(rt, "unknown-model")
	req.Model, req.Effort = "gpt-newer", "xhigh"
	if _, err := a.Run(ctx, req, func(core.AgentEvent) {}); err != nil {
		t.Fatalf("a model the catalog lags behind must pass: %v", err)
	}
}

func TestCancellationStopsAndCleansUp(t *testing.T) {
	a, rt := setup(t, "hang")
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(cctx, request(rt, "w-hang"), func(core.AgentEvent) {})
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := rt.Status(ctx, "w-hang"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !core.IsKind(err, core.KindCancelled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not stop")
	}
	if _, err := rt.Status(ctx, "w-hang"); !core.IsKind(err, core.KindNotFound) {
		t.Fatalf("workload not cleaned up: %v", err)
	}
}

func TestAuthStatus(t *testing.T) {
	for scenario, want := range map[string]core.AuthState{"ok": core.AuthOK, "logged_out": core.AuthLoginRequired, "api_key": core.AuthLoginRequired} {
		t.Run(scenario, func(t *testing.T) {
			a, rt := setup(t, scenario)
			got, err := a.AuthStatus(ctx, rt)
			if err != nil || got.State != want {
				t.Fatalf("status = %+v, %v", got, err)
			}
		})
	}
}

func TestFactoryRefusesAPICredentials(t *testing.T) {
	r := providers.NewRegistry()
	codex.Register(r)
	deps := providers.Deps{Getenv: func(string) (string, bool) { return "x", true }}
	for _, opts := range []providers.Options{
		{"image": "i", "api_key_env": "OPENAI_API_KEY"},
		{"image": "i", "api_key_file": "/secret"},
		{"image": "i", "api_key": "sk-x"},
		{"image": "i", "access_token_env": "CODEX_ACCESS_TOKEN"},
		{},
	} {
		if _, err := providers.As[core.AgentProvider](r, providers.KindAgent, "codex-cli", "c", deps, opts); !core.IsKind(err, core.KindInvalid) {
			t.Fatalf("%v accepted: %v", opts, err)
		}
	}
	a, err := providers.As[core.AgentProvider](r, providers.KindAgent, "codex-cli", "c", deps, providers.Options{"image": "i"})
	if err != nil || !a.Info().Has(core.CapAgentResume) {
		t.Fatalf("valid config: %v", err)
	}
}
