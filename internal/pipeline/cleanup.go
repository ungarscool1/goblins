package pipeline

import (
	"context"
	"errors"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/scheduler"
)

func (p *Pipeline) cleanup(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	// Environments first: their release steps run in the scratch workspace, which still holds
	// the tested checkout. A release to try again keeps it; a broken one needs a person, and
	// keeping the workspace would not help them.
	errs := []error{p.releaseEnvironments(ctx, jc, pr)}
	if errs[0] != nil && !core.IsKind(errs[0], core.KindForbidden) {
		return nil, core.Wrap(core.KindTransient, "pipeline.cleanup", errs[0])
	}
	for _, v := range []struct {
		runtime core.ExecutionRuntime
		purpose string
	}{{pr.Planner.Runtime, "plan"}, {pr.Implementer.Runtime, "impl"}, {pr.Implementer.Runtime,
		"scratch"}, {pr.Implementer.Runtime, "review"}} {
		if err := v.runtime.RemoveVolume(ctx, p.volume(jc.Task, v.purpose)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, core.Wrap(core.KindTransient, "pipeline.cleanup", err)
	}
	return struct{}{}, nil
}
