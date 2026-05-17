// Package metrics implements the hand-rolled Prometheus text exposition
// for Posthorn's /metrics endpoint (FR55, ADR-15).
//
// We don't use prometheus/client_golang — its transitive dep tree is
// disproportionate to our needs (counters and histograms with fixed
// buckets and operator-controlled labels). The text exposition format is
// stable and well-documented; emitting it ourselves is ~200 LOC.
//
// NFR24 enforcement: label values come from operator-controlled config
// (endpoint paths, transport types) or our own enum-shaped values (error
// classes from ErrorClass.String(), status codes from HTTP). Submitter
// content (recipients, subjects, body fragments) never enters the label
// space — cardinality-explosion attacks against the scraper are
// structurally prevented because there's no code path that would inject
// a request-side value as a label.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds the collection of metrics emitted on /metrics. One
// Registry per Posthorn process. Goroutine-safe.
type Registry struct {
	mu      sync.RWMutex
	metrics []collector
}

// collector is the minimal interface for things that emit Prometheus
// exposition lines. Counter and Histogram both satisfy it.
type collector interface {
	// Name returns the metric's family name (used for sort stability).
	Name() string
	// Emit emits the metric's lines (HELP, TYPE, samples) to w.
	Emit(w io.Writer) error
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{}
}

// Register adds a metric to the registry. The metric will be emitted on
// the next /metrics scrape. Registering the same metric twice is a
// programmer error (panic).
func (r *Registry) Register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.metrics {
		if existing.Name() == c.Name() {
			panic("metrics.Register: duplicate metric name " + c.Name())
		}
	}
	r.metrics = append(r.metrics, c)
}

// Emit emits all registered metrics in name-sorted order. Returns the
// first error encountered; the writer position is undefined on error.
func (r *Registry) Emit(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Snapshot + sort for deterministic output.
	snapshot := make([]collector, len(r.metrics))
	copy(snapshot, r.metrics)
	sort.SliceStable(snapshot, func(i, j int) bool {
		return snapshot[i].Name() < snapshot[j].Name()
	})
	for _, m := range snapshot {
		if err := m.Emit(w); err != nil {
			return err
		}
	}
	return nil
}

// --- Counter -----------------------------------------------------------

// Counter is a monotonically-increasing metric with operator-controlled
// label keys. Each unique combination of label values has its own
// counter (a "label series"). Resetting is intentionally not supported —
// Prometheus counters represent cumulative totals.
type Counter struct {
	name       string
	help       string
	labelNames []string

	mu     sync.Mutex
	values map[string]int64 // key = encoded label values
}

// NewCounter constructs a counter. labelNames are the keys that
// observers will provide values for (in the same order). help is the
// operator-facing description shown in /metrics output.
func NewCounter(name, help string, labelNames []string) *Counter {
	return &Counter{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		values:     make(map[string]int64),
	}
}

// Name returns the counter's name (collector contract).
func (c *Counter) Name() string { return c.name }

// Inc increments the counter for the given label values. The number of
// values must match labelNames; mismatches panic (programmer error).
func (c *Counter) Inc(labelValues ...string) {
	c.Add(1, labelValues...)
}

// Add adds delta to the counter for the given label values. delta must
// be non-negative (Prometheus counter contract); negative deltas panic.
func (c *Counter) Add(delta int64, labelValues ...string) {
	if delta < 0 {
		panic("metrics.Counter: negative delta")
	}
	if len(labelValues) != len(c.labelNames) {
		panic(fmt.Sprintf("metrics.Counter %s: got %d label values, want %d", c.name, len(labelValues), len(c.labelNames)))
	}
	key := encodeLabelValues(labelValues)
	c.mu.Lock()
	c.values[key] += delta
	c.mu.Unlock()
}

// Emit emits the counter's exposition lines (collector contract).
func (c *Counter) Emit(w io.Writer) error {
	c.mu.Lock()
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	snapshot := make(map[string]int64, len(c.values))
	for _, k := range keys {
		snapshot[k] = c.values[k]
	}
	c.mu.Unlock()

	sort.Strings(keys)
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n", c.name, escapeHelp(c.help)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s counter\n", c.name); err != nil {
		return err
	}
	for _, k := range keys {
		labelStr := renderLabels(c.labelNames, k)
		if _, err := fmt.Fprintf(w, "%s%s %d\n", c.name, labelStr, snapshot[k]); err != nil {
			return err
		}
	}
	return nil
}

// --- Histogram ---------------------------------------------------------

// Histogram observes numeric values into a fixed set of buckets. Each
// label series tracks the per-bucket counts, the sum of observations,
// and the total observation count.
type Histogram struct {
	name       string
	help       string
	labelNames []string
	buckets    []float64 // sorted ascending; +Inf is implicit

	mu     sync.Mutex
	series map[string]*histogramSeries
}

type histogramSeries struct {
	bucketCounts []int64 // one per bucket; cumulative
	sum          float64
	count        int64
}

// NewHistogram constructs a histogram with the given upper-bound buckets.
// Buckets must be sorted ascending; the implicit +Inf bucket is added
// automatically. labelNames are the per-observation label keys.
func NewHistogram(name, help string, buckets []float64, labelNames []string) *Histogram {
	// Validate buckets are sorted ascending; panic on programmer error.
	for i := 1; i < len(buckets); i++ {
		if buckets[i] <= buckets[i-1] {
			panic(fmt.Sprintf("metrics.NewHistogram %s: buckets must be sorted ascending", name))
		}
	}
	return &Histogram{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		buckets:    append([]float64(nil), buckets...),
		series:     make(map[string]*histogramSeries),
	}
}

// Name returns the histogram's name (collector contract).
func (h *Histogram) Name() string { return h.name }

// Observe records value with the given label values. Negative values
// are valid; histograms don't require monotonicity.
func (h *Histogram) Observe(value float64, labelValues ...string) {
	if len(labelValues) != len(h.labelNames) {
		panic(fmt.Sprintf("metrics.Histogram %s: got %d label values, want %d", h.name, len(labelValues), len(h.labelNames)))
	}
	key := encodeLabelValues(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histogramSeries{bucketCounts: make([]int64, len(h.buckets))}
		h.series[key] = s
	}
	s.count++
	s.sum += value
	for i, ub := range h.buckets {
		if value <= ub {
			s.bucketCounts[i]++
		}
	}
}

// Emit emits the histogram's exposition lines (collector contract).
func (h *Histogram) Emit(w io.Writer) error {
	h.mu.Lock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	snapshot := make(map[string]histogramSeries, len(h.series))
	for _, k := range keys {
		s := h.series[k]
		snapshot[k] = histogramSeries{
			bucketCounts: append([]int64(nil), s.bucketCounts...),
			sum:          s.sum,
			count:        s.count,
		}
	}
	h.mu.Unlock()

	sort.Strings(keys)
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n", h.name, escapeHelp(h.help)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s histogram\n", h.name); err != nil {
		return err
	}
	for _, k := range keys {
		s := snapshot[k]
		labelValues := decodeLabelValues(k)
		// Emit each bucket as <name>_bucket{<labels>,le="<bound>"} <count>.
		for i, ub := range h.buckets {
			labelStr := renderLabelsWithExtra(h.labelNames, labelValues, "le", strconv.FormatFloat(ub, 'g', -1, 64))
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelStr, s.bucketCounts[i]); err != nil {
				return err
			}
		}
		// +Inf bucket — equals total count by definition.
		labelStr := renderLabelsWithExtra(h.labelNames, labelValues, "le", "+Inf")
		if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelStr, s.count); err != nil {
			return err
		}
		// _sum and _count summaries.
		baseLabel := renderLabels(h.labelNames, k)
		if _, err := fmt.Fprintf(w, "%s_sum%s %g\n", h.name, baseLabel, s.sum); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_count%s %d\n", h.name, baseLabel, s.count); err != nil {
			return err
		}
	}
	return nil
}

// --- Label encoding ----------------------------------------------------

// encodeLabelValues serializes label values into a single string suitable
// for use as a map key. Separator (\x00) is illegal in any operator-
// supplied label value (paths start with "/", transport types are
// lowercase ASCII identifiers, error classes are stable strings).
const labelSeparator = "\x00"

func encodeLabelValues(values []string) string {
	return strings.Join(values, labelSeparator)
}

func decodeLabelValues(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, labelSeparator)
}

// renderLabels formats the {<name>="<value>",...} exposition fragment.
// Returns "" if there are no labels.
func renderLabels(names []string, encodedValues string) string {
	if len(names) == 0 {
		return ""
	}
	values := decodeLabelValues(encodedValues)
	return renderLabelsWithValues(names, values)
}

// renderLabelsWithValues is the same as renderLabels but takes already-
// decoded values.
func renderLabelsWithValues(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// renderLabelsWithExtra renders labels with one additional key=value pair
// appended (used for histogram bucket `le` labels).
func renderLabelsWithExtra(names, values []string, extraKey, extraValue string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range names {
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteString(`",`)
	}
	b.WriteString(extraKey)
	b.WriteString(`="`)
	b.WriteString(escapeLabelValue(extraValue))
	b.WriteString(`"}`)
	return b.String()
}

// escapeLabelValue applies Prometheus exposition escaping rules to a
// label value: " → \", \ → \\, newline → \n.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, `"\`+"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// escapeHelp escapes HELP text per the same rules (without quotes around
// the value).
func escapeHelp(v string) string {
	if !strings.ContainsAny(v, `\`+"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
