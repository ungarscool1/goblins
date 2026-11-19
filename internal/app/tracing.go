package app

import (
	"context"
	"log/slog"
	"os"

	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
)

// newTraceExporter builds the OTLP exporter when tracing has an endpoint. Its headers can
// hold a login, so they come from the environment, like every other secret.
func newTraceExporter(cfg config.Config, metrics *observability.Metrics,
	log *slog.Logger) (*observability.OTLPExporter, error) {
	t := cfg.Tracing
	if t.OTLPEndpoint == "" {
		return nil, nil
	}
	var headers map[string]string
	if t.HeadersEnv != "" {
		raw, ok := os.LookupEnv(t.HeadersEnv)
		if !ok {
			return nil, core.Errorf(core.KindInvalid, "app.tracing",
				"tracing.headers_env: %s is not set", t.HeadersEnv)
		}
		h, err := observability.ParseHeaders(raw)
		if err != nil {
			return nil, core.Errorf(core.KindInvalid, "app.tracing", "%s: %v", t.HeadersEnv, err)
		}
		headers = h
	}
	return observability.NewOTLPExporter(observability.OTLPOptions{
		Endpoint: t.OTLPEndpoint, Headers: headers, Interval: t.Interval,
		Resource: map[string]string{"service.name": "goblins", "service.version": Version,
			"goblins.installation": cfg.Installation.Name},
		Metrics: metrics, Log: log})
}

// startTracing runs the exporter until the returned stop, which sends what is queued. It is
// stopped after the scheduler, so the spans of the last jobs still go out.
func startTracing(e *observability.OTLPExporter, log *slog.Logger) (stop func()) {
	if e == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	log.Info("exporting spans over OTLP")
	return func() {
		cancel()
		<-done
		if n := e.Dropped(); n > 0 {
			log.Warn("spans were dropped", "spans", n)
		}
	}
}
