package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/ungarscool1/goblins/internal/core"
)

// MaxWebhookBytes bounds inbound deliveries before any authentication.
const MaxWebhookBytes = 10 << 20

// EventSink stores normalized events. It never starts work directly.
type EventSink interface {
	Accept(ctx context.Context, source string, events []core.Event) (int, error)
}

// WebhookHandler authenticates, normalizes and stores deliveries for named sources.
// Responses reveal only the broad category of a refusal.
func WebhookHandler(sources map[string]core.EventSource, sink EventSink, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("source")
		src, ok := sources[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "delivery too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "cannot read delivery", http.StatusBadRequest)
			return
		}
		events, err := src.HandleWebhook(r.Context(), core.WebhookRequest{Headers: r.Header, Body: body})
		if err != nil {
			log.Warn("webhook refused", "source", name, "error_kind", core.KindOf(err))
			http.Error(w, http.StatusText(webhookStatus(err)), webhookStatus(err))
			return
		}
		n, err := sink.Accept(r.Context(), name, events)
		if err != nil {
			// A 5xx asks the sender to redeliver, and dedup makes that safe.
			log.Error("cannot store webhook events", "source", name, "err", err)
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"accepted": n})
	})
}

func webhookStatus(err error) int {
	switch core.KindOf(err) {
	case core.KindUnauthorized:
		return http.StatusUnauthorized
	case core.KindForbidden:
		return http.StatusForbidden
	case core.KindInvalid:
		return http.StatusBadRequest
	case core.KindUnsupported:
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
