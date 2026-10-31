package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

type sink struct {
	events []core.Event
	err    error
}

func (s *sink) Accept(_ context.Context, _ string, evs []core.Event) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.events = append(s.events, evs...)
	return len(evs), nil
}

func TestWebhookHandler(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := fake.NewEventSource("src")
	s := &sink{}
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/{source}", WebhookHandler(map[string]core.EventSource{"src": src}, s, log))

	do := func(path, token string, body []byte) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("X-Fake-Token", token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do("/webhooks/src", "fake-token", []byte("x")); code != 202 || len(s.events) != 1 {
		t.Fatalf("valid: %d, %d events", code, len(s.events))
	}
	if code := do("/webhooks/src", "wrong", []byte("x")); code != 401 {
		t.Fatalf("forged: %d", code)
	}
	if code := do("/webhooks/nope", "fake-token", []byte("x")); code != 404 {
		t.Fatalf("unknown source: %d", code)
	}
	if code := do("/webhooks/src", "fake-token", make([]byte, MaxWebhookBytes+1)); code != 413 {
		t.Fatalf("oversized: %d", code)
	}
	s.err = errors.New("db down")
	if code := do("/webhooks/src", "fake-token", []byte("x")); code != 503 {
		t.Fatalf("storage failure must ask for redelivery: %d", code)
	}
	if got := webhookStatus(core.Errorf(core.KindForbidden, "x", "y")); got != 403 {
		t.Fatalf("forbidden: %d", got)
	}
	if got := webhookStatus(core.Errorf(core.KindInvalid, "x", "y")); got != 400 {
		t.Fatalf("invalid: %d", got)
	}
}
