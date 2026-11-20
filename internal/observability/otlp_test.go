package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type collector struct {
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	status   int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.requests = append(c.requests, body)
	c.headers = append(c.headers, r.Header.Clone())
	if c.status != 0 {
		w.WriteHeader(c.status)
	}
}

func (c *collector) spans() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, r := range c.requests {
		for _, rs := range r["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, s.(map[string]any))
				}
			}
		}
	}
	return out
}

func attrs(span map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, a := range span["attributes"].([]any) {
		m := a.(map[string]any)
		out[m["key"].(string)] = m["value"].(map[string]any)
	}
	return out
}

func TestOTLPExportsSpansInTheWireFormat(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	m := NewMetrics()
	e, err := NewOTLPExporter(OTLPOptions{Endpoint: srv.URL, Interval: time.Hour,
		Headers:  map[string]string{"Authorization": "Bearer collector-login"},
		Resource: map[string]string{"service.name": "goblins"}, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	start := time.Unix(1700000000, 5)
	e.Export(Span{TaskID: "task-1", Name: "job.implement", Start: start, Duration: time.Second,
		Status: "ok", Attrs: []any{"task", "task-1", "attempt", 2, "head_sha", "abc",
			"api_token", "hunter2", "note", "Bearer abcdefghijklmnop"}})
	e.Export(Span{TaskID: "task-1", Name: "job.validate", Start: start, Status: "transient"})
	cancel()
	<-done

	spans := c.spans()
	if len(spans) != 2 {
		t.Fatalf("collector got %d spans", len(spans))
	}
	s := spans[0]
	if s["traceId"] != TraceID("task-1") || len(s["spanId"].(string)) != 16 ||
		s["parentSpanId"] != nil || s["name"] != "job.implement" {
		t.Errorf("span = %v", s)
	}
	if s["startTimeUnixNano"] != "1700000000000000005" ||
		s["endTimeUnixNano"] != "1700000001000000005" {
		t.Errorf("times = %v, %v", s["startTimeUnixNano"], s["endTimeUnixNano"])
	}
	a := attrs(s)
	if a["goblins.task"]["stringValue"] != "task-1" || a["goblins.attempt"]["intValue"] != "2" ||
		a["goblins.head_sha"]["stringValue"] != "abc" {
		t.Errorf("attributes = %v", a)
	}
	if a["goblins.api_token"]["stringValue"] != redacted ||
		strings.Contains(a["goblins.note"]["stringValue"].(string), "abcdefghijklmnop") {
		t.Errorf("a secret reached the collector: %v", a)
	}
	if st := s["status"].(map[string]any); st["code"] != float64(otlpStatusOK) {
		t.Errorf("status = %v", st)
	}
	if st := spans[1]["status"].(map[string]any); st["code"] != float64(otlpStatusError) ||
		st["message"] != "transient" {
		t.Errorf("failed span status = %v", st)
	}
	if c.headers[0].Get("Authorization") != "Bearer collector-login" {
		t.Error("headers were not sent")
	}
	res := c.requests[0]["resourceSpans"].([]any)[0].(map[string]any)["resource"].(map[string]any)
	if !strings.Contains(mustJSON(t, res), `"service.name"`) {
		t.Errorf("resource = %v", res)
	}
	var out strings.Builder
	_ = m.Write(&out)
	if !strings.Contains(out.String(), `goblins_span_exports_total{outcome="ok"} 1`) {
		t.Errorf("metrics:\n%s", out.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOTLPNeverBlocksTheWork(t *testing.T) {
	e, err := NewOTLPExporter(OTLPOptions{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing drains the queue, as with a collector that is down.
	finished := make(chan struct{})
	go func() {
		for range otlpQueue + 10 {
			e.Export(Span{TaskID: "t", Name: "x"})
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Export blocked")
	}
	if e.Dropped() != 10 {
		t.Fatalf("dropped = %d", e.Dropped())
	}
}

func TestOTLPCountsRejectedBatches(t *testing.T) {
	c := &collector{status: http.StatusServiceUnavailable}
	srv := httptest.NewServer(c)
	defer srv.Close()
	m := NewMetrics()
	e, _ := NewOTLPExporter(OTLPOptions{Endpoint: srv.URL + "/", Interval: time.Hour, Metrics: m})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	e.Export(Span{TaskID: "t", Name: "x"})
	cancel()
	<-done
	if e.Dropped() != 1 || len(c.spans()) != 1 {
		t.Fatalf("dropped = %d, delivered = %d", e.Dropped(), len(c.spans()))
	}
	var b strings.Builder
	if err := m.Write(&b); err != nil || !strings.Contains(b.String(), "goblins_spans_dropped_total 1") {
		t.Fatalf("metrics = %s, %v", b.String(), err)
	}
}

// A collector that stalls gets one timeout for everything left at exit, not one per batch.
func TestOTLPShutdownHasOneDeadline(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stall)
	e, _ := NewOTLPExporter(OTLPOptions{Endpoint: srv.URL, Interval: time.Hour,
		Timeout: 200 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	for range 8 * otlpBatch {
		e.Export(Span{TaskID: "t", Name: "x"})
	}
	cancel()
	start := time.Now()
	go func() { defer close(done); e.Run(ctx) }()
	<-done
	if took := time.Since(start); took > time.Second {
		t.Fatalf("shutdown took %s", took)
	}
	if e.Dropped() != 8*otlpBatch {
		t.Fatalf("dropped = %d", e.Dropped())
	}
}

func TestTracesURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://c:4318":           "http://c:4318/v1/traces",
		"https://c/otlp/":         "https://c/otlp/v1/traces",
		"http://c:4318/v1/traces": "http://c:4318/v1/traces",
		"ftp://c":                 "",
		"http://user:pw@c:4318":   "",
		"http://c:4318?token=abc": "",
		"c:4318":                  "",
	} {
		got, err := TracesURL(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("TracesURL(%q) = %q, %v", in, got, err)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	h, err := ParseHeaders("authorization=Bearer%20abc, x-scope-orgid = team ,")
	if err != nil || h["Authorization"] != "Bearer abc" || h["X-Scope-Orgid"] != "team" {
		t.Fatalf("headers = %v, %v", h, err)
	}
	if _, err := ParseHeaders("novalue"); err == nil {
		t.Fatal("a header without a value was accepted")
	}
}
