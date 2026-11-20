package app

import (
	"cmp"
	"context"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
)

// meteredAgent counts what every run of an agent reported using, whichever stage ran it:
// planning, implementing, triage, describing and reviewing alike, a fallback model's run as
// its own, and a run that failed part way. Nothing is estimated: a CLI that reports no usage
// adds only to the run count. Labels come from the configuration, never from a ticket.
type meteredAgent struct {
	core.AgentProvider
	name    string
	metrics *observability.Metrics
}

func (a meteredAgent) Run(ctx context.Context, req core.AgentRequest,
	sink func(core.AgentEvent)) (core.AgentResult, error) {
	res, err := a.AgentProvider.Run(ctx, req, sink)
	l := observability.Labels{"agent", a.name, "role", string(req.Role),
		"model", cmp.Or(req.Model, "default")}
	a.metrics.Inc("goblins_agent_runs_total", l)
	tokens := func(direction string, n int64) {
		a.metrics.Add("goblins_agent_tokens_total",
			slices.Concat(l, observability.Labels{"direction", direction}), float64(n))
	}
	tokens("input", res.Usage.InputTokens)
	tokens("output", res.Usage.OutputTokens)
	a.metrics.Add("goblins_agent_turns_total", l, float64(res.Usage.Turns))
	return res, err
}
