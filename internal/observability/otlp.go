package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// OTLP limits. A collector that is down must cost memory only up to the queue, and never slow
// down the work it observes: spans beyond it are dropped and counted.
const (
	otlpQueue       = 2048
	otlpBatch       = 256
	otlpMaxAttrSize = 1024
)

// OTLPOptions configures an OTLPExporter.
type OTLPOptions struct {
	// Endpoint is the collector's base URL, as in OTEL_EXPORTER_OTLP_ENDPOINT. Spans go to
	// its /v1/traces unless the URL already names that path.
	Endpoint string
	// Headers are sent with every request, for a collector that needs a login.
	Headers map[string]string
	// Interval is how long a span waits for others before a batch is sent.
	Interval time.Duration
	Timeout  time.Duration
	// Resource describes this process, as OpenTelemetry resource attributes.
	Resource map[string]string
	Client   *http.Client
	Metrics  *Metrics
	Log      *slog.Logger
}

// OTLPExporter sends spans to an OpenTelemetry collector over OTLP/HTTP with JSON encoding,
// which the standard library can speak without an SDK (ADR 0007).
type OTLPExporter struct {
	o       OTLPOptions
	url     string
	queue   chan Span
	dropped atomic.Int64
}

// TracesURL checks a collector endpoint and returns where spans are posted.
func TracesURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("must be an http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("cannot carry credentials, a query or a fragment; use headers_env")
	}
	if !strings.HasSuffix(u.Path, "/v1/traces") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/traces"
	}
	return u.String(), nil
}

// NewOTLPExporter returns an exporter. Nothing is sent until Run.
func NewOTLPExporter(o OTLPOptions) (*OTLPExporter, error) {
	target, err := TracesURL(o.Endpoint)
	if err != nil {
		return nil, err
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: o.Timeout}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &OTLPExporter{o: o, url: target, queue: make(chan Span, otlpQueue)}, nil
}

// Export queues a span. It never blocks: a full queue drops the span.
func (e *OTLPExporter) Export(s Span) {
	if s.SpanID == "" {
		s.SpanID = NewSpanID()
	}
	select {
	case e.queue <- s:
	default:
		e.dropped.Add(1)
		e.o.Metrics.Inc("goblins_spans_dropped_total", nil)
	}
}

// Dropped is how many spans were lost to a full queue or a failed export.
func (e *OTLPExporter) Dropped() int64 { return e.dropped.Load() }

// Run sends batches until ctx ends, then sends what is left within the timeout.
func (e *OTLPExporter) Run(ctx context.Context) {
	tick := time.NewTicker(e.o.Interval)
	defer tick.Stop()
	var batch []Span
	for {
		select {
		case s := <-e.queue:
			if batch = append(batch, s); len(batch) >= otlpBatch {
				e.send(context.WithoutCancel(ctx), batch)
				batch = nil
			}
		case <-tick.C:
			e.send(context.WithoutCancel(ctx), batch)
			batch = nil
		case <-ctx.Done():
			e.drain(context.WithoutCancel(ctx), batch)
			return
		}
	}
}

func (e *OTLPExporter) drain(ctx context.Context, batch []Span) {
	for {
		select {
		case s := <-e.queue:
			batch = append(batch, s)
		default:
			for len(batch) > 0 {
				n := min(len(batch), otlpBatch)
				e.send(ctx, batch[:n])
				batch = batch[n:]
			}
			return
		}
	}
}

func (e *OTLPExporter) send(ctx context.Context, batch []Span) {
	if len(batch) == 0 {
		return
	}
	err := e.post(ctx, batch)
	outcome := "ok"
	if err != nil {
		outcome = "error"
		e.dropped.Add(int64(len(batch)))
		e.o.Log.Warn("cannot export spans", "spans", len(batch), "err", err)
	}
	e.o.Metrics.Inc("goblins_span_exports_total", Labels{"outcome", outcome})
}

func (e *OTLPExporter) post(ctx context.Context, batch []Span) error {
	body, err := json.Marshal(otlpRequest(e.o.Resource, batch))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range e.o.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.o.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector answered %s", resp.Status)
	}
	return nil
}

// ParseHeaders reads headers in the OTEL_EXPORTER_OTLP_HEADERS form: key=value pairs,
// separated by commas, with values URL-encoded.
func ParseHeaders(s string) (map[string]string, error) {
	h := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("a header is not key=value")
		}
		value, err := url.QueryUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		h[http.CanonicalHeaderKey(k)] = value
	}
	return h, nil
}

type otlpValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
}

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId,omitempty"`
	Name         string     `json:"name"`
	Kind         int        `json:"kind"`
	Start        string     `json:"startTimeUnixNano"`
	End          string     `json:"endTimeUnixNano"`
	Attributes   []otlpAttr `json:"attributes,omitempty"`
	Status       otlpStatus `json:"status"`
}

// OTLP status codes and span kinds.
const (
	otlpStatusOK     = 1
	otlpStatusError  = 2
	otlpKindInternal = 1
)

func otlpRequest(resource map[string]string, batch []Span) map[string]any {
	var res []otlpAttr
	for _, k := range slices.Sorted(maps.Keys(resource)) {
		res = append(res, attr(k, resource[k]))
	}
	spans := make([]otlpSpan, 0, len(batch))
	for _, s := range batch {
		spans = append(spans, toOTLP(s))
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": res},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "goblins"},
			"spans": spans,
		}},
	}}}
}

// toOTLP converts a span. A task's spans share its trace ID and have no parent: the task's
// root span is never finished, so pointing at it would leave every span dangling.
func toOTLP(s Span) otlpSpan {
	o := otlpSpan{TraceID: TraceID(s.TaskID), SpanID: s.SpanID, Name: s.Name,
		Kind: otlpKindInternal, Start: strconv.FormatInt(s.Start.UnixNano(), 10),
		End:    strconv.FormatInt(s.Start.Add(s.Duration).UnixNano(), 10),
		Status: otlpStatus{Code: otlpStatusOK}}
	if s.Status != "" && s.Status != "ok" {
		o.Status = otlpStatus{Code: otlpStatusError, Message: s.Status}
	}
	o.Attributes = append(o.Attributes, attr("goblins.task", s.TaskID))
	for i := 0; i+1 < len(s.Attrs); i += 2 {
		k, ok := s.Attrs[i].(string)
		if !ok || k == "task" {
			continue
		}
		o.Attributes = append(o.Attributes, attrOf("goblins."+k, s.Attrs[i+1]))
	}
	return o
}

func attr(k, v string) otlpAttr {
	return otlpAttr{Key: k, Value: otlpValue{StringValue: &v}}
}

// attrOf converts a value with the redaction logs get, since a collector is one more place
// secrets must not reach.
func attrOf(k string, v any) otlpAttr {
	if IsSensitiveKey(k) {
		return attr(k, redacted)
	}
	switch x := v.(type) {
	case int:
		n := strconv.Itoa(x)
		return otlpAttr{Key: k, Value: otlpValue{IntValue: &n}}
	case int64:
		n := strconv.FormatInt(x, 10)
		return otlpAttr{Key: k, Value: otlpValue{IntValue: &n}}
	case bool:
		return otlpAttr{Key: k, Value: otlpValue{BoolValue: &x}}
	}
	s := RedactText(fmt.Sprint(v))
	if len(s) > otlpMaxAttrSize {
		s = strings.ToValidUTF8(s[:otlpMaxAttrSize], "")
	}
	return attr(k, s)
}
