package scheduler

import (
	"context"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// volumeGrace is how long a finished task's volumes are left to its own cleanup job, which
// retries, before reconciliation treats them as left behind.
const volumeGrace = time.Hour

// ReconcileVolumes removes volumes no task will use again: those of completed or cancelled
// tasks whose cleanup never ran, after a crash or a lost job, and those of tasks this store has
// never heard of. A failed task keeps its volumes, as its work may still be recovered by hand.
func (s *Scheduler) ReconcileVolumes(ctx context.Context) {
	s.init()
	for _, rt := range s.Runtimes {
		lister, ok := rt.(core.VolumeLister)
		if !ok || !rt.Info().Has(core.CapRuntimeVolumeList) {
			continue
		}
		vols, err := lister.ListVolumes(ctx,
			map[string]string{LabelInstallation: s.Cfg.Installation})
		if err != nil {
			s.Log.Warn("cannot list volumes", "runtime", rt.Info().Name, "err", err)
			continue
		}
		for _, v := range vols {
			if s.volumeLeftBehind(ctx, v) {
				s.removeVolume(ctx, rt, v)
			}
		}
	}
}

func (s *Scheduler) volumeLeftBehind(ctx context.Context, v core.VolumeStatus) bool {
	taskID := v.Labels[LabelTask]
	if taskID == "" {
		return false
	}
	t, err := s.Store.GetTask(ctx, taskID)
	if core.IsKind(err, core.KindNotFound) {
		return true
	}
	if err != nil {
		return false
	}
	done := t.State == core.StateCompleted || t.State == core.StateCancelled
	return done && s.Now().Sub(t.UpdatedAt) >= volumeGrace
}

func (s *Scheduler) removeVolume(ctx context.Context, rt core.ExecutionRuntime,
	v core.VolumeStatus) {
	s.Log.Info("removing a volume left behind", "volume", v.Name, "task", v.Labels[LabelTask])
	if err := rt.RemoveVolume(ctx, v.Name); err != nil {
		s.Log.Warn("cannot remove volume", "volume", v.Name, "err", err)
		return
	}
	s.Metrics.Inc("goblins_volumes_reclaimed_total", nil)
}
