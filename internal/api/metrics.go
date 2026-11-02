package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
)

// StallCounter says how many tasks should be moving and are not.
type StallCounter interface{ StalledTasks() int64 }

// MetricsHandler serves Prometheus metrics: what was recorded, and what the store says now.
func MetricsHandler(m *observability.Metrics, store core.StateStore, stalls StallCounter,
	log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		stats, err := store.Stats(ctx)
		if err != nil {
			log.Warn("cannot read metrics", "err", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		tasks, jobs := map[string]int64{}, map[string]int64{}
		for _, c := range stats.Tasks {
			tasks[observability.Labels{"state", string(c.State)}.String()] = c.Count
		}
		for _, c := range stats.Jobs {
			l := observability.Labels{"kind", string(c.Kind), "state", string(c.State)}
			jobs[l.String()] = c.Count
		}
		gauges := []struct {
			name   string
			values map[string]int64
		}{{"goblins_tasks", tasks}, {"goblins_jobs", jobs},
			{"goblins_tasks_stalled", map[string]int64{"": stalls.StalledTasks()}}}
		for _, g := range gauges {
			if err := observability.WriteGauge(w, g.name, g.values); err != nil {
				log.Warn("cannot write metrics", "err", fmt.Errorf("%s: %w", g.name, err))
				return
			}
		}
		if err := m.Write(w); err != nil {
			log.Warn("cannot write metrics", "err", err)
		}
	})
}
