package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return rec.Code, body
}

func TestHealthAndReadiness(t *testing.T) {
	failing := errors.New("db password=hunter2 unreachable")
	var checkErr error
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), "v1", []Check{{Name: "db", Run: func(context.Context) error { return checkErr }}})

	if code, body := get(t, s.Handler(), "/healthz"); code != 200 || body["version"] != "v1" {
		t.Fatalf("healthz: %d %v", code, body)
	}
	if code, _ := get(t, s.Handler(), "/readyz"); code != 503 {
		t.Fatalf("readyz before startup: %d", code)
	}
	s.SetReady(true)
	if code, _ := get(t, s.Handler(), "/readyz"); code != 200 {
		t.Fatalf("readyz when ready: %d", code)
	}
	checkErr = failing
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("readyz with failing check: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("readiness exposed check error details")
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), "v1", nil)
	s.SetReady(true)
	started := make(chan struct{})
	release := make(chan struct{})
	s.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, 5*time.Second) }()

	respc := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respc <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		respc <- string(b)
	}()
	<-started
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if got := <-respc; got != "done" {
		t.Fatalf("in-flight request not drained: %q", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
