package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestMetricsWriteThePrometheusFormat(t *testing.T) {
	m := NewMetrics()
	m.Inc("jobs_total", Labels{"kind", "plan", "outcome", "ok"})
	m.Inc("jobs_total", Labels{"kind", "plan", "outcome", "ok"})
	m.Observe("took_seconds", 2, Labels{"kind", "plan"})
	var b bytes.Buffer
	if err := m.Write(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# TYPE jobs_total counter\n", `jobs_total{kind="plan",outcome="ok"} 2`,
		"# TYPE took_seconds histogram\n", `took_seconds_bucket{kind="plan",le="1"} 0`,
		`took_seconds_bucket{kind="plan",le="5"} 1`, `took_seconds_bucket{kind="plan",le="+Inf"} 1`,
		`took_seconds_sum{kind="plan"} 2`, `took_seconds_count{kind="plan"} 1`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("%q is missing from:\n%s", want, b.String())
		}
	}
}

func TestNilMetricsRecordNothing(t *testing.T) {
	var m *Metrics
	m.Inc("a", nil)
	m.Observe("b", 1, nil)
	if err := m.Write(&bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestTraceIsStablePerTaskAndSpansAreRedacted(t *testing.T) {
	first := TraceID("t1")
	if first != TraceID("t1") || first == TraceID("t2") || len(first) != 32 {
		t.Fatalf("trace id %q", first)
	}
	var b bytes.Buffer
	log, err := NewLogger(&b, LogOptions{Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	LogSpan(t.Context(), log.With(slog.String("x", "y")), "t1", "job.plan", time.Now(), time.Second,
		"ok", "api_key", "s3cret")
	got := b.String()
	if !strings.Contains(got, TraceID("t1")) || !strings.Contains(got, RootSpanID("t1")) ||
		!strings.Contains(got, `"duration_ms":1000`) || strings.Contains(got, "s3cret") {
		t.Fatalf("span = %s", got)
	}
}
