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
	var errs []error
	for _, v := range []struct {
		runtime core.ExecutionRuntime
		purpose string
	}{{pr.Planner.Runtime, "plan"}, {pr.Implementer.Runtime, "impl"}, {pr.Implementer.Runtime,
		"scratch"}, {pr.Implementer.Runtime, "review"}} {
		if err := v.runtime.RemoveVolume(ctx, p.volume(jc.Task, v.purpose)); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, p.releaseEnvironments(ctx, jc, pr))
	if err := errors.Join(errs...); err != nil {
		return nil, core.Wrap(core.KindTransient, "pipeline.cleanup", err)
	}
	return struct{}{}, nil
}
