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

// LogSpan records a finished span as a log line. Attributes go through the logger's redaction.
func LogSpan(ctx context.Context, log *slog.Logger, taskID, name string, start time.Time,
	d time.Duration, status string, attrs ...any) {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	all := append([]any{"trace_id", TraceID(taskID), "span_id", hex.EncodeToString(id),
		"parent_span_id", RootSpanID(taskID), "span", name, "start", start.UTC(),
		"duration_ms", d.Milliseconds(), "status", status}, attrs...)
	log.InfoContext(ctx, "span", all...)
}
