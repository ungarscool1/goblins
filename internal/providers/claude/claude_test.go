package claude_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/claude"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

var ctx = context.Background()

func fakeBinDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func setup(t *testing.T, scenario string) (*claude.Agent, *fake.ProcessRuntime) {
	t.Helper()
	rt := fake.NewProcessRuntime("proc", t.TempDir(), fakeBinDir(t))
	a := claude.New("claude", claude.Options{Image: "img:" + scenario, ConfigVolume: "config"})
	return a, rt
}

func request(rt core.ExecutionRuntime, scenario, name string) core.AgentRequest {
	return core.AgentRequest{
		IdempotencyKey: name, WorkloadName: name, Role: core.RolePlanner, Prompt: "hello", Runtime: rt,
		Labels: map[string]string{"goblins.scenario": scenario},
	}
}

// withScenario makes workloads see FAKE_SCENARIO through a wrapper executable,
// because the adapter builds the environment itself.
func withScenario(t *testing.T, a *claude.Agent, scenario string) *claude.Agent {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nFAKE_SCENARIO=" + scenario + " FAKE_KEY_SOURCE=" + os.Getenv("FAKE_KEY_SOURCE") + " exec " + filepath.Join(fakeBinDir(t), "claude") + " \"$@\"\n"
	exe := filepath.Join(dir, "claude-"+scenario)
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return claude.New("claude", claude.Options{Image: "img", ConfigVolume: "config", Executable: exe})
}

func TestContract(t *testing.T) {
	contract.AgentProvider(t, func(t *testing.T) (core.AgentProvider, core.ExecutionRuntime, string) {
		a, rt := setup(t, "ok")
		return a, rt, "echo: p"
	})
}

func TestRunStreamsAndResumes(t *testing.T) {
	a, rt := setup(t, "ok")
	var evs []core.AgentEvent
	sink := func(e core.AgentEvent) { evs = append(evs, e) }
	res, err := a.Run(ctx, request(rt, "ok", "w1"), sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "fake-session-1" || res.Output != "echo: hello" {
		t.Fatalf("result = %+v", res)
	}
	var kinds []core.AgentEventKind
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []core.AgentEventKind{core.AgentEventSession, core.AgentEventText, core.AgentEventTool, core.AgentEventResult}
	if strings.Join(asStrings(kinds), ",") != strings.Join(asStrings(want), ",") {
		t.Fatalf("events = %v", kinds)
	}

	req := request(rt, "ok", "w2")
	req.ResumeSessionID = "earlier-session"
	res, err = a.Run(ctx, req, func(core.AgentEvent) {})
	if err != nil || res.SessionID != "earlier-session" {
		t.Fatalf("resume = %+v, %v", res, err)
	}
	if got := invocations(t, rt); !strings.Contains(got, "--resume earlier-session") {
		t.Fatalf("invocations:\n%s", got)
	}
}

func asStrings(k []core.AgentEventKind) []string {
	out := make([]string, len(k))
	for i, v := range k {
		out[i] = string(v)
	}
	return out
}

func invocations(t *testing.T, rt *fake.ProcessRuntime) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rt.VolumePath("config"), "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lastArgs(t *testing.T, rt *fake.ProcessRuntime) string {
	t.Helper()
	var last string
	for _, l := range strings.Split(invocations(t, rt), "\n") {
		if strings.HasPrefix(l, "ARGS ") {
			last = l
		}
	}
	return last
}

func TestToolPolicyNeverGrantsCommandExecution(t *testing.T) {
	cases := []struct {
		policy core.ToolPolicy
		want   string
	}{
		{core.ToolPolicy{}, "--tools Read,Glob,Grep "},
		{core.ToolPolicy{WriteFiles: true}, "--tools Read,Glob,Grep,Edit,Write "},
		{core.ToolPolicy{Web: true}, "--tools Read,Glob,Grep,WebFetch,WebSearch "},
	}
	for i, tc := range cases {
		a, rt := setup(t, "ok")
		req := request(rt, "ok", "policy")
		req.Policy = tc.policy
		req.Model = "some-model"
		req.Effort = "high"
		if _, err := a.Run(ctx, req, func(core.AgentEvent) {}); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		got := lastArgs(t, rt)
		for _, want := range []string{tc.want, "--restricted", "--strict-mcp-config", "--permission-mode dontAsk", "--model some-model", "--effort high"} {
			if !strings.Contains(got, want) {
				t.Errorf("%d: missing %q in %s", i, want, got)
			}
		}
		for _, banned := range []string{"Bash", "bypassPermissions", "dangerously", "PowerShell"} {
			if strings.Contains(got, banned) {
				t.Errorf("%d: %q present in %s", i, banned, got)
			}
		}
	}
}

func TestReadOnlyPolicyMountsWorkspaceReadOnly(t *testing.T) {
	a, rt := setup(t, "ok")
	req := request(rt, "ok", "ro")
	req.Workspace = core.Mount{Volume: "ws", Target: "/workspace"}
	if _, err := a.Run(ctx, req, func(core.AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	spec := rt.Specs[len(rt.Specs)-1]
	var mount core.Mount
	for _, m := range spec.Mounts {
		if m.Volume == "ws" {
			mount = m
		}
	}
	if !mount.ReadOnly {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}
	if !spec.Security.ReadOnlyRoot || spec.Security.Privileged {
		t.Fatalf("security = %+v", spec.Security)
	}
}

func TestPreflightWorkloadCarriesTheRequestLabels(t *testing.T) {
	a, rt := setup(t, "ok")
	if _, err := a.Run(ctx, request(rt, "ok", "labels"), func(core.AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	preflight := rt.Specs[0]
	if !strings.HasPrefix(preflight.Name, "goblins-preflight-") || preflight.Labels["goblins.scenario"] != "ok" {
		t.Fatalf("name = %s, labels = %v", preflight.Name, preflight.Labels)
	}
}

func TestRuntimeWithoutReadOnlyRootIsRefused(t *testing.T) {
	a, _ := setup(t, "ok")
	rt := fake.NewRuntime("r")
	rt.Caps = []core.Capability{core.CapRuntimeVolumes, core.CapRuntimeNetworkNone, core.CapRuntimeNetworkEgress}
	_, err := a.Run(ctx, request(rt, "ok", "no-root"), func(core.AgentEvent) {})
	if !core.IsKind(err, core.KindUnsupported) {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if len(rt.Created) != 0 {
		t.Fatalf("started despite the refusal: %+v", rt.Created)
	}
}

func TestAPIKeysNeverReachTheCLI(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-orchestrator-env")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-from-orchestrator-env")
	a, rt := setup(t, "ok")
	if _, err := a.Run(ctx, request(rt, "ok", "env"), func(core.AgentEvent) {}); err != nil {
		t.Fatal(err)
	}
	got := invocations(t, rt)
	if strings.Contains(got, "orchestrator-env") {
		t.Fatalf("API key leaked to the CLI:\n%s", got)
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY=[] ANTHROPIC_AUTH_TOKEN=[]") {
		t.Fatalf("invocations:\n%s", got)
	}
	// Even a value set by the workload itself, say by an image, is cleared first.
	spec := rt.Specs[len(rt.Specs)-1]
	if !strings.Contains(strings.Join(spec.Command, " "), "unset ANTHROPIC_API_KEY") {
		t.Fatalf("command does not clear billing variables: %v", spec.Command)
	}
	for k := range spec.Secrets {
		if strings.Contains(k, "API_KEY") {
			t.Fatalf("secret %s", k)
		}
	}
}

func TestAPIBilledSessionIsRefused(t *testing.T) {
	t.Setenv("FAKE_KEY_SOURCE", "ANTHROPIC_API_KEY")
	base, rt := setup(t, "ok")
	a := withScenario(t, base, "ok")
	_, err := a.Run(ctx, request(rt, "ok", "billing"), func(core.AgentEvent) {})
	if !core.IsKind(err, core.KindAuthRequired) || !strings.Contains(err.Error(), "instead of a subscription") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailuresAreClassified(t *testing.T) {
	cases := map[string]core.ErrorKind{
		"rate_limit":    core.KindQuotaExhausted,
		"status_429":    core.KindQuotaExhausted,
		"invalid_login": core.KindAuthRequired,
		"stderr_login":  core.KindAuthRequired,
		"gone":          core.KindNotFound,
		"crash":         core.KindTransient,
		"no_result":     core.KindTransient,
	}
	for scenario, want := range cases {
		t.Run(scenario, func(t *testing.T) {
			base, rt := setup(t, scenario)
			a := withScenario(t, base, scenario)
			res, err := a.Run(ctx, request(rt, scenario, "w-"+scenario), func(core.AgentEvent) {})
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

func TestOldCLIFailsClosed(t *testing.T) {
	base, rt := setup(t, "old")
	a := withScenario(t, base, "old_cli")
	_, err := a.Run(ctx, request(rt, "old", "w"), func(core.AgentEvent) {})
	if !core.IsKind(err, core.KindUnsupported) || !strings.Contains(err.Error(), "--restricted") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(invocations(t, rt), "-p --output-format") {
		t.Fatal("a CLI without the security options was run")
	}
}

func TestCancellationStopsAndCleansUp(t *testing.T) {
	base, rt := setup(t, "hang")
	a := withScenario(t, base, "hang")
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(cctx, request(rt, "hang", "w-hang"), func(core.AgentEvent) {})
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
	cases := map[string]core.AuthState{"ok": core.AuthOK, "logged_out": core.AuthLoginRequired, "api_key": core.AuthLoginRequired, "bedrock": core.AuthLoginRequired}
	for scenario, want := range cases {
		t.Run(scenario, func(t *testing.T) {
			base, rt := setup(t, scenario)
			got, err := withScenario(t, base, scenario).AuthStatus(ctx, rt)
			if err != nil || got.State != want {
				t.Fatalf("status = %+v, %v", got, err)
			}
		})
	}
}

func TestFactoryRefusesAPIKeys(t *testing.T) {
	r := providers.NewRegistry()
	claude.Register(r)
	deps := providers.Deps{Getenv: func(string) (string, bool) { return "x", true }}
	for _, opts := range []providers.Options{
		{"image": "i", "api_key_env": "ANTHROPIC_API_KEY"},
		{"image": "i", "api_key_file": "/secret"},
		{"image": "i", "api_key": "sk-x"},
		{},
	} {
		if _, err := providers.As[core.AgentProvider](r, providers.KindAgent, "claude-cli", "c", deps, opts); !core.IsKind(err, core.KindInvalid) {
			t.Fatalf("%v accepted: %v", opts, err)
		}
	}
	a, err := providers.As[core.AgentProvider](r, providers.KindAgent, "claude-cli", "c", deps, providers.Options{"image": "i", "oauth_token_env": "T"})
	if err != nil || !a.Info().Has(core.CapAgentResume) {
		t.Fatalf("valid config: %v", err)
	}
}
