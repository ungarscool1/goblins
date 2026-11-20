# ADR 0007: OTLP traces without the OpenTelemetry SDK

**Status:** accepted

**Context.** Issue #21 asks for OpenTelemetry-compatible traces. Goblins already gives every
job attempt a span, logged with W3C-format trace and span IDs derived from the task. Sending
them to a collector with the OpenTelemetry Go SDK would add the SDK, its exporters, gRPC and
protobuf, which is far more code than Goblins' own dependencies together. ADR 0001 keeps third
parties to what would be unreasonable to write.

**Decision.** Export spans with OTLP/HTTP and its JSON encoding, which every OpenTelemetry
collector accepts on `/v1/traces`, written with `net/http` and `encoding/json`. Spans are
batched, the queue is bounded and a full queue drops spans rather than block a job. The
exporter is off unless `tracing.otlp_endpoint` is set. Metrics stay Prometheus text.

**Consequences.**

- No new module. The wire format is a few structs, kept to what Goblins produces: internal
  spans, string, integer and boolean attributes, and an OK or error status.
- There is no context propagation into agents or other services, and no gRPC or protobuf
  transport. A deployment needing those can put a collector in front.
- Spans of a task share its trace ID and have no parent, since the task's root span never
  finishes. The trace ID is the one in logs and `goblins audit`.
- If Goblins later needs more of OpenTelemetry, such as propagation or OTLP metrics, this
  decision should be revisited rather than the hand-written encoding grown.
