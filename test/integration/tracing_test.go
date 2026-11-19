//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A mock task's jobs reach an OTLP collector as spans of one trace, carrying what ties the
// ticket to its design and commit.
func TestMockWorkflowExportsTraces(t *testing.T) {
	var mu sync.Mutex
	var spans []map[string]any
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if r.URL.Path != "/v1/traces" || json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range body.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
	}))
	defer collector.Close()

	s := &service{t: t, bin: buildBinary(t), addr: freeAddr(t), data: t.TempDir(),
		extra: []string{"GOBLINS_OTLP_ENDPOINT=" + collector.URL}}
	s.start()
	created := s.post("/mock/demo/issues", `{"title":"Try it","body":"Do the thing"}`)
	issue := created["issue"].(string)
	s.eventually(func() bool { return strings.Contains(s.status(), "design-proposed") })
	s.post("/mock/demo/issues/"+issue+"/comments", `{"body":"/approve-design 1"}`)
	s.eventually(func() bool { return strings.Contains(s.status(), "completed") })
	// Stopping sends what is queued.
	s.stop()

	mu.Lock()
	defer mu.Unlock()
	names := map[string]bool{}
	traces := map[any]bool{}
	for _, sp := range spans {
		names[sp["name"].(string)] = true
		traces[sp["traceId"]] = true
	}
	for _, want := range []string{"job.plan", "job.implement", "job.publish"} {
		if !names[want] {
			t.Errorf("no %s span among %v", want, names)
		}
	}
	if len(traces) != 1 {
		t.Errorf("one task made %d traces", len(traces))
	}
	if !strings.Contains(s.logs.String(), "exporting spans over OTLP") {
		t.Errorf("logs:\n%s", s.logs)
	}
}
