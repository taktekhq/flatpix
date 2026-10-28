package server

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics are the counters behind /metrics, in Prometheus text format.
// Nothing here needs a client library.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64 // keyed by name{labels}
	hist     map[string]*histogram
	gauges   map[string]func() float64
}

type histogram struct {
	mu      sync.Mutex
	bounds  []float64
	buckets []int64
	sum     float64
	count   int64
}

// NewMetrics makes an empty set.
func NewMetrics() *Metrics {
	return &Metrics{counters: map[string]*atomic.Int64{}, hist: map[string]*histogram{}, gauges: map[string]func() float64{}}
}

// Inc adds one to the counter name with the given label pairs ("k","v",...).
func (m *Metrics) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }

// Add adds n to a counter.
func (m *Metrics) Add(name string, n int64, labels ...string) {
	key := name + labelString(labels)
	m.mu.Lock()
	c := m.counters[key]
	if c == nil {
		c = new(atomic.Int64)
		m.counters[key] = c
	}
	m.mu.Unlock()
	c.Add(n)
}

// Get reads a counter (used by tests).
func (m *Metrics) Get(name string, labels ...string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.counters[name+labelString(labels)]; c != nil {
		return c.Load()
	}
	return 0
}

// Observe records a value in the histogram name.
func (m *Metrics) Observe(name string, v float64) {
	m.mu.Lock()
	h := m.hist[name]
	if h == nil {
		b := []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
		h = &histogram{bounds: b, buckets: make([]int64, len(b))}
		m.hist[name] = h
	}
	m.mu.Unlock()
	h.mu.Lock()
	for i, b := range h.bounds {
		if v <= b {
			h.buckets[i]++
		}
	}
	h.sum += v
	h.count++
	h.mu.Unlock()
}

// Gauge registers a value that is read when /metrics is scraped.
func (m *Metrics) Gauge(name string, f func() float64) {
	m.mu.Lock()
	m.gauges[name] = f
	m.mu.Unlock()
}

func labelString(l []string) string {
	if len(l) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(l); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s=%q`, l[i], l[i+1]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var help = map[string]string{
	"flatpix_requests_total":           "Image requests by HTTP status.",
	"flatpix_renders_total":            "Renders finished, by output format.",
	"flatpix_render_errors_total":      "Renders that failed, by error kind. Clients that left are not counted.",
	"flatpix_cache_hits_total":         "Requests answered without rendering, by tier (derived, client, shared).",
	"flatpix_cache_misses_total":       "Requests that had to render.",
	"flatpix_queue_waits_total":        "Renders that had to wait for a free seat.",
	"flatpix_queue_wait_seconds":       "Time spent waiting for a seat.",
	"flatpix_rejections_total":         "Requests turned away with 503, by reason.",
	"flatpix_render_seconds":           "Time spent rendering.",
	"flatpix_download_seconds":         "Time spent downloading an original to scratch.",
	"flatpix_clients_gone_total":       "Requests abandoned by the client before the answer.",
	"flatpix_seats":                    "Render seats in total.",
	"flatpix_seats_in_use":             "Render seats busy now.",
	"flatpix_queue_depth":              "Renders waiting for a seat now.",
	"flatpix_downloads_in_use":         "Downloads running now.",
	"flatpix_scratch_budget_bytes":     "Scratch disk budget for downloads.",
	"flatpix_scratch_used_bytes":       "Scratch bytes reserved or in use now.",
	"flatpix_groups_in_flight":         "Distinct outputs being rendered or finishing now.",
	"flatpix_cache_put_errors_total":   "Derived cache uploads that failed.",
	"flatpix_ignored_parameters_total": "Parameters ignored in lenient mode.",
}

func (m *Metrics) metricType(name string) string {
	switch {
	case strings.HasSuffix(name, "_total"):
		return "counter"
	case strings.HasSuffix(name, "_seconds"):
		return "histogram"
	}
	return "gauge"
}

// WriteText writes the exposition text.
func (m *Metrics) WriteText(w io.Writer) {
	m.mu.Lock()
	byName := map[string][]string{}
	for k := range m.counters {
		n, _, _ := strings.Cut(k, "{")
		byName[n] = append(byName[n], k)
	}
	counters := map[string]int64{}
	for k, c := range m.counters {
		counters[k] = c.Load()
	}
	hists := map[string]*histogram{}
	for k, h := range m.hist {
		hists[k] = h
	}
	gauges := map[string]func() float64{}
	for k, g := range m.gauges {
		gauges[k] = g
	}
	m.mu.Unlock()

	var names []string
	for n := range byName {
		names = append(names, n)
	}
	for n := range hists {
		names = append(names, n)
	}
	for n := range gauges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, help[n], n, m.metricType(n))
		switch {
		case hists[n] != nil:
			h := hists[n]
			h.mu.Lock()
			for i, b := range h.bounds {
				fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", n, b, h.buckets[i])
			}
			fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", n, h.count, n, h.sum, n, h.count)
			h.mu.Unlock()
		case gauges[n] != nil:
			fmt.Fprintf(w, "%s %g\n", n, gauges[n]())
		default:
			keys := byName[n]
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "%s %d\n", k, counters[k])
			}
		}
	}
}
