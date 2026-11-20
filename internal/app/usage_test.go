package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

func TestMeteredAgentCountsEveryRun(t *testing.T) {
	inner := fake.NewAgent("claude")
	inner.Script(
		fake.AgentStep{Output: "ok",
			Usage: core.AgentUsage{InputTokens: 1200, OutputTokens: 30, Turns: 2}},
		fake.AgentStep{Err: core.Errorf(core.KindRateLimited, "x", "limited"),
			Usage: core.AgentUsage{InputTokens: 800, OutputTokens: 5, Turns: 1}},
		fake.AgentStep{Output: "no usage reported"})
	m := observability.NewMetrics()
	agent := meteredAgent{AgentProvider: inner, name: "claude", metrics: m}
	ctx := context.Background()
	for _, req := range []core.AgentRequest{
		{Role: core.RolePlanner, Model: "opus"}, {Role: core.RolePlanner, Model: "opus"},
		{Role: core.RoleReviewer},
	} {
		_, _ = agent.Run(ctx, req, func(core.AgentEvent) {})
	}
	var b bytes.Buffer
	if err := m.Write(&b); err != nil {
		t.Fatal(err)
	}
	planner := `agent="claude",role="planner",model="opus"`
	for _, want := range []string{
		`goblins_agent_runs_total{` + planner + `} 2`,
		`goblins_agent_tokens_total{` + planner + `,direction="input"} 2000`,
		`goblins_agent_tokens_total{` + planner + `,direction="output"} 35`,
		`goblins_agent_turns_total{` + planner + `} 3`,
		`goblins_agent_runs_total{agent="claude",role="reviewer",model="default"} 1`,
	} {
		if !strings.Contains(b.String(), want+"\n") {
			t.Errorf("%q is missing from:\n%s", want, b.String())
		}
	}
}
