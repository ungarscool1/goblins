package observability

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"
)

// TraceID names everything that happened for a task. It is derived from the task, so a
// restart, a retry and a person reading the audit log all arrive at the same one. Its form
// follows W3C Trace Context, as do span IDs, so spans can be turned into OpenTelemetry ones.
func TraceID(taskID string) string {
	sum := sha256.Sum256([]byte("goblins-trace/" + taskID))
	return hex.EncodeToString(sum[:16])
}

// RootSpanID is the span every span of a task hangs from.
func RootSpanID(taskID string) string {
	sum := sha256.Sum256([]byte("goblins-span/" + taskID))
	return hex.EncodeToString(sum[:8])
}

// Span is one finished piece of work on a task.
type Span struct {
	TaskID   string
	Name     string
	Start    time.Time
	Duration time.Duration
	Status   string
	// Attrs are key and value pairs. They go through the logger's redaction.
	Attrs []any
	// SpanID is set by NewSpanID, so a log line and an exported span are the same span.
	SpanID string
}

// SpanExporter sends finished spans somewhere outside the process. It must not block.
type SpanExporter interface {
	Export(Span)
}

// NewSpanID returns a random span ID in W3C form.
func NewSpanID() string {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	return hex.EncodeToString(id)
}

// LogSpan records a finished span as a log line.
func LogSpan(ctx context.Context, log *slog.Logger, s Span) {
	if s.SpanID == "" {
		s.SpanID = NewSpanID()
	}
	all := append([]any{"trace_id", TraceID(s.TaskID), "span_id", s.SpanID,
		"parent_span_id", RootSpanID(s.TaskID), "span", s.Name, "start", s.Start.UTC(),
		"duration_ms", s.Duration.Milliseconds(), "status", s.Status}, s.Attrs...)
	log.InfoContext(ctx, "span", all...)
}
