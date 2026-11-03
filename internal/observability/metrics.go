package observability

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Durations of jobs span seconds to hours, as agent runs do.
var durationBuckets = []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200}

// Labels are name and value pairs. Values must come from a closed set such as a state or a
// kind, never from a ticket or a repository, as metrics are unauthenticated and cheap to read.
type Labels []string

func (l Labels) String() string {
	parts := make([]string, 0, len(l)/2)
	for i := 0; i+1 < len(l); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%s", l[i], strconv.Quote(l[i+1])))
	}
	return strings.Join(parts, ",")
}

type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

// Metrics holds counters and histograms in the Prometheus text format, starting over with each
// process as Prometheus expects. A nil Metrics records nothing, so tests and tools need none.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]map[string]float64
	hists    map[string]map[string]*histogram
}

// NewMetrics returns empty metrics.
func NewMetrics() *Metrics {
	return &Metrics{counters: map[string]map[string]float64{},
		hists: map[string]map[string]*histogram{}}
}

// Inc adds one to a counter.
func (m *Metrics) Inc(name string, l Labels) { m.Add(name, l, 1) }

// Add adds v, which must not be negative, to a counter.
func (m *Metrics) Add(name string, l Labels, v float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters[name] == nil {
		m.counters[name] = map[string]float64{}
	}
	m.counters[name][l.String()] += v
}

// Observe records a duration in seconds.
func (m *Metrics) Observe(name string, seconds float64, l Labels) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hists[name] == nil {
		m.hists[name] = map[string]*histogram{}
	}
	h := m.hists[name][l.String()]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(durationBuckets))}
		m.hists[name][l.String()] = h
	}
	for i, b := range durationBuckets {
		if seconds <= b {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.total++
}

// Write renders everything recorded so far.
func (m *Metrics) Write(w io.Writer) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(m.counters)) {
		fmt.Fprintf(&b, "# TYPE %s counter\n", name)
		for _, l := range slices.Sorted(maps.Keys(m.counters[name])) {
			// Plain digits, as token counts in exponent form are hard to read.
			v := strconv.FormatFloat(m.counters[name][l], 'f', -1, 64)
			fmt.Fprintf(&b, "%s %s\n", series(name, l), v)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m.hists)) {
		fmt.Fprintf(&b, "# TYPE %s histogram\n", name)
		for _, l := range slices.Sorted(maps.Keys(m.hists[name])) {
			writeHistogram(&b, name, l, m.hists[name][l])
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeHistogram(b *strings.Builder, name, labels string, h *histogram) {
	join := func(extra string) string {
		if labels == "" {
			return extra
		}
		return labels + "," + extra
	}
	for i, bound := range durationBuckets {
		le := join(fmt.Sprintf(`le="%g"`, bound))
		fmt.Fprintf(b, "%s %d\n", series(name+"_bucket", le), h.counts[i])
	}
	fmt.Fprintf(b, "%s %d\n", series(name+"_bucket", join(`le="+Inf"`)), h.total)
	fmt.Fprintf(b, "%s %g\n", series(name+"_sum", labels), h.sum)
	fmt.Fprintf(b, "%s %d\n", series(name+"_count", labels), h.total)
}

// WriteGauge renders a value read at scrape time, such as the depth of a queue.
func WriteGauge(w io.Writer, name string, values map[string]int64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
	for _, l := range slices.Sorted(maps.Keys(values)) {
		fmt.Fprintf(&b, "%s %d\n", series(name, l), values[l])
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func series(name, labels string) string {
	if labels == "" {
		return name
	}
	return name + "{" + labels + "}"
}
