package config

import (
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

func knownFake(_ providers.Kind, typ string) bool { return typ == "fake" }

const valid = `
version: 1
installation: {name: personal, data_dir: /tmp/goblins}
server: {listen: "127.0.0.1:9090"}
trackers: {t1: {type: fake, options: {token_env: MY_TOKEN, url: "https://user:pw12345@example.com"}}}
scms: {s1: {type: fake}}
agents: {a1: {type: fake}}
runtimes: {r1: {type: fake}}
projects:
  - id: demo
    tracker: t1
    scm: s1
    repository: acme/demo
    stages:
      planner: {agent: a1, runtime: r1}
      implementer: {agent: a1, runtime: r1, model: big, effort: high}
`

func noEnv(string) (string, bool) { return "", false }

func TestLoadValid(t *testing.T) {
	cfg, err := LoadBytes([]byte(valid), noEnv, knownFake)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9090" || cfg.Server.ShutdownTimeout != 20*time.Second || cfg.Storage.Driver != "sqlite" {
		t.Fatalf("defaults not merged: %+v", cfg)
	}
}

func TestEnvOverrides(t *testing.T) {
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"GOBLINS_LISTEN": "0.0.0.0:1", "GOBLINS_LOG_LEVEL": "debug", "GOBLINS_SHUTDOWN_TIMEOUT": "3s"}[k]
		return v, ok
	}
	cfg, err := LoadBytes([]byte(valid), env, knownFake)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:1" || cfg.Log.Level != "debug" || cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	badEnv := func(k string) (string, bool) { return "soon", k == "GOBLINS_SHUTDOWN_TIMEOUT" }
	if _, err := LoadBytes([]byte(valid), badEnv, knownFake); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("bad duration: %v", err)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct{ name, cfg, want string }{
		{"no version", "installation: {name: x, data_dir: y}", "version"},
		{"future version", "version: 2", "unsupported value 2"},
		{"unknown field", "version: 1\nbogus: true", "bogus"},
		{"two documents", "version: 1\n---\nversion: 1", "single YAML document"},
		{"unknown provider", "version: 1\ntrackers: {t: {type: smoke-signals}}", `unknown tracker provider "smoke-signals"`},
		{"missing type", "version: 1\nagents: {a: {}}", "agents.a.type: required"},
		{"inline secret", "version: 1\ntrackers: {t: {type: fake, options: {api_token: abc}}}", "inline secrets"},
		{"nested inline secret", "version: 1\ntrackers: {t: {type: fake, options: {auth: {password: abc}}}}", "inline secrets"},
		{"reserved source name", "version: 1\nevent_sources: {operator: {type: fake}}", "reserved"},
		{"weak approval permission", strings.Replace(valid, "repository: acme/demo", "repository: acme/demo\n    policy: {min_approve_permission: triage}", 1), "approval needs at least write"},
		{"bad storage", "version: 1\nstorage: {driver: mongo}", "unknown driver"},
		{"postgres dsn", "version: 1\nstorage: {driver: postgres}", "dsn_env"},
		{"bad level", "version: 1\nlog: {level: loud}", "invalid log level"},
		{"dangling reference", strings.Replace(valid, "tracker: t1", "tracker: nope", 1), `undefined instance "nope"`},
		{"duplicate project", valid + `  - {id: demo, tracker: t1, scm: s1, repository: a/b, stages: {planner: {agent: a1, runtime: r1}, implementer: {agent: a1, runtime: r1}}}`, "duplicate id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.cfg), noEnv, knownFake)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if !core.IsKind(err, core.KindInvalid) {
				t.Fatalf("kind = %s", core.KindOf(err))
			}
		})
	}
}

func TestRenderRedacts(t *testing.T) {
	cfg, err := LoadBytes([]byte(valid), noEnv, knownFake)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents["a1"] = Provider{Type: "fake", Options: map[string]any{"api_token": "leak-me", "nested": map[string]any{"secret": 42}}}
	out, err := cfg.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, leak := range []string{"leak-me", "pw12345", "42"} {
		if strings.Contains(s, leak) {
			t.Errorf("render leaked %q:\n%s", leak, s)
		}
	}
	if !strings.Contains(s, "token_env: MY_TOKEN") {
		t.Errorf("secret reference must stay visible:\n%s", s)
	}
	// Render must not mutate the original.
	if cfg.Agents["a1"].Options["api_token"] != "leak-me" {
		t.Error("render mutated configuration")
	}
}
