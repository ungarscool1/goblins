package app

import (
	"context"
	"log/slog"
	"net"
	"sync"

	"github.com/ungarscool1/goblins/internal/api"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
)

// serveMetrics starts the metrics listener when one is configured. Metrics get a port of
// their own: whoever can scrape them reaches nothing else, and the port that takes webhooks
// serves no metrics. The listener stops with ctx, and wg tracks it.
func serveMetrics(ctx context.Context, cfg config.Server, sys *System, store core.StateStore,
	log *slog.Logger, wg *sync.WaitGroup) error {
	if cfg.MetricsListen == "" {
		return nil
	}
	ln, err := net.Listen("tcp", cfg.MetricsListen)
	if err != nil {
		return core.Wrap(core.KindInvalid, "app.metrics_listen", err)
	}
	log.Info("metrics listening", "listen", ln.Addr().String())
	h := api.MetricsOnly(api.MetricsHandler(sys.Metrics, store, sys.Scheduler, log))
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := api.ServeHandler(ctx, ln, h, cfg.ShutdownTimeout); err != nil {
			log.Error("metrics listener stopped", "err", err)
		}
	}()
	return nil
}
