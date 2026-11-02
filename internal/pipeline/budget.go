package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

var errToolBudget = errors.New("tool budget used up")

// runBudgeted runs an agent within its stage's budget, and on the stage's fallback model when
// the configured one is limited. Both continue the same session.
func runBudgeted(ctx context.Context, jc scheduler.JobContext, stage Stage, req core.AgentRequest,
	sink func(core.AgentEvent)) (core.AgentResult, error) {
	res, err := runOnce(ctx, jc, stage, req, sink)
	limited := core.IsKind(err, core.KindQuotaExhausted) || core.IsKind(err, core.KindRateLimited)
	if !limited || stage.FallbackModel == "" || stage.FallbackModel == req.Model {
		return res, err
	}
	jc.Log("system", "the model is limited, continuing on "+stage.FallbackModel)
	req.Model = stage.FallbackModel
	req.WorkloadName = workloadName(jc, "agent-fallback")
	if res.SessionID != "" {
		req.ResumeSessionID = res.SessionID
	}
	return runOnce(ctx, jc, stage, req, sink)
}

// runOnce counts tool calls from the event stream, which every agent reports alike, rather
// than asking a CLI to limit itself. A budget is per run: a person resuming grants a new one.
func runOnce(ctx context.Context, jc scheduler.JobContext, stage Stage, req core.AgentRequest,
	sink func(core.AgentEvent)) (core.AgentResult, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	calls := 0
	res, err := stage.Agent.Run(ctx, req, func(e core.AgentEvent) {
		if e.Kind == core.AgentEventTool {
			calls++
			if stage.MaxToolCalls > 0 && calls > stage.MaxToolCalls {
				cancel(errToolBudget)
			}
		}
		sink(e)
	})
	if errors.Is(context.Cause(ctx), errToolBudget) {
		return res, core.Errorf(core.KindInvalid, "pipeline.budget",
			"the agent went over its budget of %d tool calls", stage.MaxToolCalls)
	}
	if u := res.Usage; u != (core.AgentUsage{}) {
		// Only what the CLI reported: nothing here estimates what is left of a subscription.
		jc.Log("usage", fmt.Sprintf("%d input tokens, %d output tokens, %d turns",
			u.InputTokens, u.OutputTokens, u.Turns))
	}
	return res, err
}
