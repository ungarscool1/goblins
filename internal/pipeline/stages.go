package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// prepareStage applies a stage's settings to a run, and records what executes it: the
// blueprint, the agent, model, effort, tools and permissions, in the task's transcript.
func (p *Pipeline) prepareStage(ctx context.Context, jc scheduler.JobContext, stage Stage,
	role core.AgentRole, req *core.AgentRequest) error {
	req.Role, req.Model, req.Effort = role, stage.Model, stage.Effort
	req.Runtime, req.Timeout, req.Tools = stage.Runtime, stage.Timeout, stage.Tools
	info := stage.Agent.Info()
	tools := "none"
	if len(stage.Tools) > 0 {
		tools = strings.Join(stage.Tools, ", ")
	}
	jc.Log("stage", fmt.Sprintf("blueprint %s, %s: agent %s (%s), model %q, effort %q, "+
		"write files %v, web %v, allow-listed tools: %s, runtime %s", workflow.BlueprintVersion,
		role, info.Name, info.Type, stage.Model, stage.Effort, req.Policy.WriteFiles,
		req.Policy.Web, tools, stage.Runtime.Info().Name))
	return p.addMemory(ctx, jc, role, req)
}
