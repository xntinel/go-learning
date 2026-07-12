# Exercise 1: Lock-Free Metrics Pipeline

Metrics are the cheapest of the three observability signals: one atomic instruction per request, scraped as a few numbers that summarize millions of requests. This exercise builds the whole Prometheus-compatible pipeline a proxy needs — atomic counters and gauges, a bucketed histogram, a cardinality-limited labeled counter, and a registry that serializes everything to the text exposition format over HTTP — with no lock on the observation hot path.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
metrics.go           Counter, Gauge, Histogram, LabeledCounter, Registry; Collector interface
cmd/
  demo/
    main.go          register the four golden-signal metrics, feed deterministic traffic, expose
metrics_test.go      counter/gauge/histogram contracts, cardinality limit, HTTP handler, race tests
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `Counter`, `Gauge`, `Histogram`, `LabeledCounter` (with `ErrCardinalityLimit`), and `Registry` with an HTTP `Handler`, all behind an unexported `Collector` interface.
- Test: atomic increment under 1000 goroutines, histogram `le` boundary placement, cardinality-limit sentinel error, and the `/metrics` HTTP endpoint.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/08-observability/01-metrics-pipeline/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/08-observability/01-metrics-pipeline
```

### Why atomics, and why a float stored as Uint64

The observation path runs on every proxied request, so its cost is multiplied by the proxy's entire throughput. A `sync.Mutex` around a counter would serialize every increment: with a thousand goroutines contending, the metric becomes the bottleneck and the proxy's throughput collapses to one goroutine's worth. The lock-free alternative stores the value as the IEEE 754 bit pattern of a `float64` inside an `atomic.Uint64`. Reads (`Load`) and the natively-atomic `Add` on integers never block; the only place a loop appears is the float addition, because the hardware has no atomic float add. There the code does a compare-and-swap loop: read the current bits, compute `Float64bits(Float64frombits(old) + v)`, and try to swap them in; if another goroutine changed the value in between, the swap fails and the loop re-reads and retries. Under normal load the swap succeeds on the first attempt, so the loop is effectively a single instruction; contention only adds iterations, and it never parks the goroutine. A gauge is the same machinery with subtraction allowed (`Add(-1)`), because the CAS loop re-reads the live value each iteration.

Histogram observation is cheaper than a counter add. Each bucket is its own `atomic.Uint64` incremented with the native `Add(1)` — no CAS loop — and the only CAS loop is on the running sum. Finding the right bucket is a binary search with `le` semantics: `sort.SearchFloat64s` returns the first index whose bound is `>= v`, which is exactly the bucket a value at or below that bound belongs to, so a value equal to a boundary lands in that boundary's bucket rather than the next one. The counts are stored non-cumulatively (one increment per observation) and turned into the cumulative `le` buckets Prometheus requires only at collection time, by accumulating a running total as the buckets are written.

### Cardinality limiting without a TOCTOU race

A labeled counter fans out to a child `*Counter` per unique label combination, kept in a `sync.Map` keyed by a canonical join of the label values. Left unbounded this is a memory leak waiting on a high-cardinality label, so the type carries a limit and an `atomic.Int64` of how many distinct sets it has admitted. The ordering is what makes it correct under concurrency: it `LoadOrStore`s the new child first (an atomic insert), and only then increments the count and checks the limit. If two goroutines race to add the same label set, exactly one wins the `LoadOrStore` and the other gets the existing child; if adding pushes the count over the limit, the loser of the limit check deletes the entry it just stored, decrements, and returns a shared no-op counter. Checking the count *before* storing would be the classic time-of-check/time-of-use bug — two goroutines both read "one below the limit" and both proceed. `WithResult` surfaces the breach as `ErrCardinalityLimit` (wrapped with the metric name, so `errors.Is` still matches) for callers that want to alert; `With` is the branch-free hot-path variant that just hands back the no-op.

### The registry and exposition

The `Collector` interface has a single unexported method, `collect(io.Writer)`, which keeps the interface package-private: only types defined here can satisfy it, so the registry is a closed set. The `Registry` holds a slice of collectors under an `RWMutex`; `Register` takes the write lock briefly, and `Expose` takes the read lock only long enough to copy the slice, then writes each collector's text outside the lock so a slow client cannot block registration. `Handler` wraps `Expose` in an `http.Handler` and sets the `text/plain; version=0.0.4; charset=utf-8` content type that identifies the Prometheus text exposition format.

Create `metrics.go`:

```go
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Collector is satisfied by every metric type in this package.
// The unexported method keeps the interface package-private.
type Collector interface {
	collect(w io.Writer)
}

// Counter is a monotonically increasing metric.
// All operations are safe for concurrent use without locks.
type Counter struct {
	bits     atomic.Uint64 // IEEE 754 bits of a float64; updated via CAS loop
	name     string
	help     string
	labelStr string // Prometheus label string, e.g. {method="GET"}, or ""
	noop     bool   // if true, Add and Inc are silent no-ops (cardinality limit)
}

// NewCounter returns a new unlabeled Counter.
func NewCounter(name, help string) *Counter {
	return &Counter{name: name, help: help}
}

// Inc increments the counter by one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds v to the counter. It panics if v is negative.
func (c *Counter) Add(v float64) {
	if v < 0 {
		panic("metrics: Counter.Add requires a non-negative value")
	}
	if c.noop {
		return
	}
	for {
		old := c.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if c.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Value returns the current counter value.
func (c *Counter) Value() float64 {
	return math.Float64frombits(c.bits.Load())
}

func (c *Counter) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", c.name, c.help)
	fmt.Fprintf(w, "# TYPE %s counter\n", c.name)
	fmt.Fprintf(w, "%s%s %g\n", c.name, c.labelStr, c.Value())
}

// Gauge is a metric that can go up or down.
type Gauge struct {
	bits atomic.Uint64
	name string
	help string
}

// NewGauge returns a new Gauge.
func NewGauge(name, help string) *Gauge {
	return &Gauge{name: name, help: help}
}

// Set sets the gauge to exactly v.
func (g *Gauge) Set(v float64) {
	g.bits.Store(math.Float64bits(v))
}

// Add adds v to the gauge (v may be negative).
func (g *Gauge) Add(v float64) {
	for {
		old := g.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if g.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Inc increments the gauge by one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec decrements the gauge by one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current gauge value.
func (g *Gauge) Value() float64 {
	return math.Float64frombits(g.bits.Load())
}

func (g *Gauge) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", g.name, g.help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", g.name)
	fmt.Fprintf(w, "%s %g\n", g.name, g.Value())
}

// DefBuckets are the default upper bounds for latency histograms, in seconds.
// They match the Prometheus client Go defaults.
var DefBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0}

// Histogram tracks the distribution of observed values using configurable buckets.
type Histogram struct {
	name    string
	help    string
	bounds  []float64       // sorted upper bounds; len = N
	buckets []atomic.Uint64 // non-cumulative counts; len = N+1 (last = +Inf)
	count   atomic.Uint64
	sumBits atomic.Uint64 // CAS-updated float64 sum stored as IEEE 754 bits
}

// NewHistogram returns a Histogram with the given upper-bound bucket boundaries.
// bounds is sorted internally; duplicates are harmless.
func NewHistogram(name, help string, bounds []float64) *Histogram {
	b := make([]float64, len(bounds))
	copy(b, bounds)
	sort.Float64s(b)
	return &Histogram{
		name:    name,
		help:    help,
		bounds:  b,
		buckets: make([]atomic.Uint64, len(b)+1),
	}
}

// Observe records a single observation.
func (h *Histogram) Observe(v float64) {
	// sort.SearchFloat64s returns the first i where bounds[i] >= v.
	// Prometheus le (less-than-or-equal) semantics: a value equal to a bound
	// must fall into that bound's bucket. SearchFloat64s satisfies this exactly.
	idx := sort.SearchFloat64s(h.bounds, v)
	h.buckets[idx].Add(1)
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sumBits.CompareAndSwap(old, next) {
			break
		}
	}
}

// Percentile returns an estimate of the p-th percentile (0.0-1.0) by linear
// interpolation within the bucket containing that rank. Returns 0 for empty
// histograms.
func (h *Histogram) Percentile(p float64) float64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(float64(total) * p))
	var cumulative uint64
	for i := range h.buckets {
		c := h.buckets[i].Load()
		cumulative += c
		if cumulative >= target {
			lower := 0.0
			if i > 0 {
				lower = h.bounds[i-1]
			}
			if i >= len(h.bounds) {
				// +Inf bucket: best estimate is the last finite bound.
				return lower
			}
			upper := h.bounds[i]
			prev := cumulative - c
			fraction := 0.0
			if c > 0 {
				fraction = float64(target-prev) / float64(c)
			}
			return lower + (upper-lower)*fraction
		}
	}
	return h.bounds[len(h.bounds)-1]
}

func (h *Histogram) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
	var cumulative uint64
	for i, bound := range h.bounds {
		cumulative += h.buckets[i].Load()
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", h.name, bound, cumulative)
	}
	cumulative += h.buckets[len(h.bounds)].Load()
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, cumulative)
	fmt.Fprintf(w, "%s_count %d\n", h.name, h.count.Load())
	fmt.Fprintf(w, "%s_sum %g\n", h.name, math.Float64frombits(h.sumBits.Load()))
}

// ErrCardinalityLimit is returned by LabeledCounter.WithResult when adding a new
// label combination would exceed the configured limit.
var ErrCardinalityLimit = fmt.Errorf("metrics: cardinality limit exceeded")

// noopCounter absorbs all operations silently. It is returned by With when the
// cardinality limit is reached, keeping the hot path branch-free at the call site.
var noopCounter = &Counter{noop: true}

// LabeledCounter is a Counter family keyed by label combinations.
type LabeledCounter struct {
	name       string
	help       string
	labelNames []string
	children   sync.Map // label key (string) -> *Counter
	limit      int
	count      atomic.Int64 // number of stored unique label sets
}

// NewLabeledCounter returns a LabeledCounter. limit is the maximum number of
// distinct label combinations it will track; further combinations are silently
// dropped.
func NewLabeledCounter(name, help string, labelNames []string, limit int) *LabeledCounter {
	return &LabeledCounter{
		name:       name,
		help:       help,
		labelNames: labelNames,
		limit:      limit,
	}
}

// With returns the Counter for the given label values. It returns noopCounter
// (a counter that silently ignores all operations) when the cardinality limit
// is exceeded.
func (lc *LabeledCounter) With(labels map[string]string) *Counter {
	c, _ := lc.WithResult(labels)
	return c
}

// WithResult is the observable variant: it returns ErrCardinalityLimit wrapped
// with the metric name when the limit is exceeded, so callers can detect and
// alert on cardinality breaches.
func (lc *LabeledCounter) WithResult(labels map[string]string) (*Counter, error) {
	key := lc.labelKey(labels)

	if v, ok := lc.children.Load(key); ok {
		return v.(*Counter), nil
	}

	c := &Counter{
		name:     lc.name,
		help:     lc.help,
		labelStr: lc.formatLabelStr(labels),
	}
	if actual, loaded := lc.children.LoadOrStore(key, c); loaded {
		return actual.(*Counter), nil
	}

	// We stored a new child. Check the cardinality limit.
	if int(lc.count.Add(1)) > lc.limit {
		// Over the limit: remove what we just stored and return the noop counter.
		lc.children.Delete(key)
		lc.count.Add(-1)
		return noopCounter, fmt.Errorf("%w: metric %s", ErrCardinalityLimit, lc.name)
	}
	return c, nil
}

// labelKey returns a canonical map key for a label set.
func (lc *LabeledCounter) labelKey(labels map[string]string) string {
	pairs := make([]string, len(lc.labelNames))
	for i, n := range lc.labelNames {
		pairs[i] = n + "=" + labels[n]
	}
	return strings.Join(pairs, "\x00")
}

// formatLabelStr returns the Prometheus label string, e.g. {method="GET",status="200"}.
func (lc *LabeledCounter) formatLabelStr(labels map[string]string) string {
	pairs := make([]string, len(lc.labelNames))
	for i, n := range lc.labelNames {
		pairs[i] = n + `="` + labels[n] + `"`
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

func (lc *LabeledCounter) collect(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", lc.name, lc.help)
	fmt.Fprintf(w, "# TYPE %s counter\n", lc.name)
	// Snapshot and sort the children so the exposition order is stable.
	var lines []string
	lc.children.Range(func(_, v any) bool {
		c := v.(*Counter)
		if !c.noop {
			lines = append(lines, fmt.Sprintf("%s%s %g\n", lc.name, c.labelStr, c.Value()))
		}
		return true
	})
	sort.Strings(lines)
	for _, l := range lines {
		io.WriteString(w, l)
	}
}

// Registry holds all registered Collectors and serves them as Prometheus text.
type Registry struct {
	mu         sync.RWMutex
	collectors []Collector
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds c to the registry. It is safe to call concurrently.
func (r *Registry) Register(c Collector) {
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
}

// Expose writes all metrics in Prometheus text format to w.
// The name avoids the io.WriterTo interface (which requires (int64, error)).
func (r *Registry) Expose(w io.Writer) {
	r.mu.RLock()
	cs := make([]Collector, len(r.collectors))
	copy(cs, r.collectors)
	r.mu.RUnlock()
	for _, c := range cs {
		c.collect(w)
	}
}

// Handler returns an http.Handler that serves the Prometheus metrics endpoint.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Expose(w)
	})
}
```

Read `Observe` and `collect` on the histogram as the two halves of the cumulative-bucket contract: `Observe` increments exactly one bucket (the non-cumulative store), and `collect` adds a running `cumulative` as it walks the bounds so the emitted `le` series is monotonic. Read `WithResult` for the ordering that defeats the cardinality race: store first with `LoadOrStore`, then `count.Add(1)` and compare; on a breach, undo the store and return the shared `noopCounter`. The labeled `collect` sorts its lines so the exposition is byte-for-byte stable across runs despite `sync.Map`'s unordered iteration.

### The runnable demo

The demo registers one metric for each of the four golden signals — a labeled request counter (traffic), an active-connections gauge (saturation), a latency histogram (latency), and the error status folded into the request counter's `status` label — then feeds a fixed, deterministic batch of traffic and writes the exposition to stdout. Fixed input makes the output reproducible, which is what lets the expected block below be exact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/metrics-pipeline"
)

func main() {
	reg := metrics.NewRegistry()

	// Traffic: request counter labeled by upstream, method and status.
	requests := metrics.NewLabeledCounter(
		"proxy_requests_total",
		"Total proxy requests partitioned by upstream, method and status.",
		[]string{"upstream", "method", "status"},
		500,
	)
	reg.Register(requests)

	// Saturation: active connections gauge.
	activeConns := metrics.NewGauge(
		"proxy_active_connections",
		"Number of currently open proxy connections.",
	)
	reg.Register(activeConns)

	// Latency: distribution across a small, explicit set of buckets.
	latency := metrics.NewHistogram(
		"proxy_request_duration_seconds",
		"Proxy request latency distribution in seconds.",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1},
	)
	reg.Register(latency)

	// A fixed, deterministic batch of traffic so the output is reproducible.
	type sample struct {
		upstream, method, status string
		latSec                   float64
	}
	traffic := []sample{
		{"svc-a", "GET", "200", 0.004},
		{"svc-a", "GET", "200", 0.008},
		{"svc-a", "GET", "200", 0.030},
		{"svc-a", "POST", "200", 0.060},
		{"svc-b", "GET", "200", 0.012},
		{"svc-b", "GET", "500", 0.090},
	}
	for _, s := range traffic {
		activeConns.Inc()
		latency.Observe(s.latSec)
		requests.With(map[string]string{
			"upstream": s.upstream,
			"method":   s.method,
			"status":   s.status,
		}).Inc()
		activeConns.Dec()
	}

	reg.Expose(os.Stdout)
	fmt.Fprintf(os.Stdout, "# p50 latency estimate: %.4fs\n", latency.Percentile(0.50))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
# HELP proxy_requests_total Total proxy requests partitioned by upstream, method and status.
# TYPE proxy_requests_total counter
proxy_requests_total{upstream="svc-a",method="GET",status="200"} 3
proxy_requests_total{upstream="svc-a",method="POST",status="200"} 1
proxy_requests_total{upstream="svc-b",method="GET",status="200"} 1
proxy_requests_total{upstream="svc-b",method="GET",status="500"} 1
# HELP proxy_active_connections Number of currently open proxy connections.
# TYPE proxy_active_connections gauge
proxy_active_connections 0
# HELP proxy_request_duration_seconds Proxy request latency distribution in seconds.
# TYPE proxy_request_duration_seconds histogram
proxy_request_duration_seconds_bucket{le="0.005"} 1
proxy_request_duration_seconds_bucket{le="0.01"} 2
proxy_request_duration_seconds_bucket{le="0.025"} 3
proxy_request_duration_seconds_bucket{le="0.05"} 4
proxy_request_duration_seconds_bucket{le="0.1"} 6
proxy_request_duration_seconds_bucket{le="+Inf"} 6
proxy_request_duration_seconds_count 6
proxy_request_duration_seconds_sum 0.204
# p50 latency estimate: 0.0250s
```

### Tests

The tests pin every contract: the counter/gauge arithmetic and the negative-`Add` panic, the histogram's `le` boundary placement and cumulative buckets, the cardinality-limit sentinel, the `/metrics` HTTP endpoint and its content type, and — under `-race` — that 1000 goroutines incrementing a counter and 500 goroutines balancing `Inc`/`Dec` on a gauge produce the exact arithmetic result with no data race. The two `Example` functions double as documentation and as auto-verified output assertions.

Create `metrics_test.go`:

```go
package metrics

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ExampleCounter shows the basic counter lifecycle.
func ExampleCounter() {
	c := NewCounter("requests_total", "Total requests.")
	c.Inc()
	c.Add(4)
	fmt.Printf("%.0f\n", c.Value())
	// Output: 5
}

// ExampleRegistry_Expose shows the Prometheus text output for a single counter.
func ExampleRegistry_Expose() {
	reg := NewRegistry()
	c := NewCounter("hits_total", "Cache hits.")
	reg.Register(c)
	c.Add(7)
	var sb strings.Builder
	reg.Expose(&sb)
	fmt.Print(sb.String())
	// Output:
	// # HELP hits_total Cache hits.
	// # TYPE hits_total counter
	// hits_total 7
}

func TestCounterInc(t *testing.T) {
	t.Parallel()
	c := NewCounter("x", "")
	c.Inc()
	c.Inc()
	if got := c.Value(); got != 2 {
		t.Fatalf("Value() = %g, want 2", got)
	}
}

func TestCounterAddSequential(t *testing.T) {
	t.Parallel()
	steps := []struct {
		add  float64
		want float64
	}{
		{0.5, 0.5},
		{1.5, 2.0},
		{0.0, 2.0},
	}
	c := NewCounter("x", "")
	for _, s := range steps {
		c.Add(s.add)
		if got := c.Value(); got != s.want {
			t.Errorf("after Add(%g): Value() = %g, want %g", s.add, got, s.want)
		}
	}
}

func TestCounterAddNegativePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for negative Add")
		}
	}()
	NewCounter("x", "").Add(-1)
}

func TestGaugeSetIncDec(t *testing.T) {
	t.Parallel()
	g := NewGauge("active", "")
	g.Set(10)
	g.Inc()
	g.Dec()
	if got := g.Value(); got != 10 {
		t.Fatalf("Value() = %g, want 10", got)
	}
	g.Add(-5)
	if got := g.Value(); got != 5 {
		t.Fatalf("Value() = %g, want 5", got)
	}
}

func TestHistogramObserve(t *testing.T) {
	t.Parallel()
	h := NewHistogram("lat", "", []float64{0.1, 0.5, 1.0})
	// bucket 0 (le=0.1): 0.05
	// bucket 1 (le=0.5): 0.3
	// bucket 2 (le=1.0): 0.7
	// bucket 3 (+Inf):   2.0
	for _, v := range []float64{0.05, 0.3, 0.7, 2.0} {
		h.Observe(v)
	}
	if got := h.count.Load(); got != 4 {
		t.Fatalf("count = %d, want 4", got)
	}
	want := []uint64{1, 1, 1, 1}
	for i, w := range want {
		if got := h.buckets[i].Load(); got != w {
			t.Errorf("buckets[%d] = %d, want %d", i, got, w)
		}
	}
}

func TestHistogramBoundaryObservation(t *testing.T) {
	t.Parallel()
	// A value exactly equal to a bucket boundary must land in that bucket,
	// not in the next one (Prometheus le semantics).
	h := NewHistogram("lat", "", []float64{0.1, 0.5})
	h.Observe(0.1)
	if got := h.buckets[0].Load(); got != 1 {
		t.Fatalf("buckets[0] = %d, want 1 (le=0.1 boundary)", got)
	}
	if got := h.buckets[1].Load(); got != 0 {
		t.Fatalf("buckets[1] = %d, want 0", got)
	}
}

func TestHistogramPercentile(t *testing.T) {
	t.Parallel()
	h := NewHistogram("lat", "", []float64{0.1, 0.5, 1.0})
	// All 100 observations fall in bucket 0 (le=0.1).
	for range 100 {
		h.Observe(0.05)
	}
	p50 := h.Percentile(0.50)
	if p50 < 0 || p50 > 0.1 {
		t.Fatalf("p50 = %g, want in [0, 0.1]", p50)
	}
	p99 := h.Percentile(0.99)
	if p99 < 0 || p99 > 0.1 {
		t.Fatalf("p99 = %g, want in [0, 0.1]", p99)
	}
}

func TestHistogramPercentileEmpty(t *testing.T) {
	t.Parallel()
	if got := NewHistogram("lat", "", DefBuckets).Percentile(0.99); got != 0 {
		t.Fatalf("empty histogram p99 = %g, want 0", got)
	}
}

func TestLabeledCounterWith(t *testing.T) {
	t.Parallel()
	lc := NewLabeledCounter("rpc_total", "RPC calls.", []string{"method", "status"}, 100)

	lc.With(map[string]string{"method": "GET", "status": "200"}).Inc()
	lc.With(map[string]string{"method": "GET", "status": "200"}).Inc()
	lc.With(map[string]string{"method": "POST", "status": "201"}).Add(3)

	if got := lc.With(map[string]string{"method": "GET", "status": "200"}).Value(); got != 2 {
		t.Fatalf("GET/200 = %g, want 2", got)
	}
	if got := lc.With(map[string]string{"method": "POST", "status": "201"}).Value(); got != 3 {
		t.Fatalf("POST/201 = %g, want 3", got)
	}
}

func TestLabeledCounterCardinalityLimit(t *testing.T) {
	t.Parallel()
	lc := NewLabeledCounter("m", "", []string{"id"}, 2)

	_, err1 := lc.WithResult(map[string]string{"id": "a"})
	_, err2 := lc.WithResult(map[string]string{"id": "b"})
	_, err3 := lc.WithResult(map[string]string{"id": "c"})

	if err1 != nil || err2 != nil {
		t.Fatalf("first two label sets should succeed: %v, %v", err1, err2)
	}
	if !errors.Is(err3, ErrCardinalityLimit) {
		t.Fatalf("err3 = %v, want ErrCardinalityLimit", err3)
	}
}

func TestLabeledCounterSameLabelSetReturnsExisting(t *testing.T) {
	t.Parallel()
	lc := NewLabeledCounter("m", "", []string{"k"}, 10)
	c1 := lc.With(map[string]string{"k": "v"})
	c2 := lc.With(map[string]string{"k": "v"})
	if c1 != c2 {
		t.Fatal("same label set must return the same *Counter pointer")
	}
}

func TestRegistryHTTPHandler(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	c := NewCounter("test_total", "Test counter.")
	reg.Register(c)
	c.Add(42)

	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "test_total 42") {
		t.Fatalf("body missing expected metric line:\n%s", body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain prefix", ct)
	}
}

func TestCounterConcurrency(t *testing.T) {
	t.Parallel()
	const goroutines = 1000
	const incsEach = 100
	c := NewCounter("concurrent_total", "")
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range incsEach {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	if got := c.Value(); got != goroutines*incsEach {
		t.Fatalf("concurrent value = %g, want %d", got, goroutines*incsEach)
	}
}

func TestGaugeConcurrency(t *testing.T) {
	t.Parallel()
	const goroutines = 500
	const iters = 200
	g := NewGauge("balance", "")
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range iters {
				g.Inc()
				g.Dec()
			}
		}()
	}
	wg.Wait()
	if got := g.Value(); got != 0 {
		t.Fatalf("balanced Inc/Dec gauge = %g, want 0", got)
	}
}
```

## Review

The pipeline is correct when no observation takes a lock and every contract holds under `-race`. Confirm the counter and gauge use a CAS loop over `atomic.Uint64` bits (never a mutex), and that `TestCounterConcurrency` and `TestGaugeConcurrency` produce exact arithmetic — `goroutines*incsEach` and `0` respectively — which is what proves the loop is atomic rather than merely fast. The most common metrics bug is the histogram exposition: the buckets are stored non-cumulatively but must be emitted cumulatively, so check `collect` accumulates a running total and that `TestHistogramBoundaryObservation` pins a value equal to a bound into that bound's bucket. The second is the cardinality race: the limit must be checked *after* `LoadOrStore`, not before, or two goroutines can both slip past a check-then-store and overshoot; `TestLabeledCounterCardinalityLimit` asserts the third distinct label set returns `ErrCardinalityLimit` via `errors.Is`, and `TestLabeledCounterSameLabelSetReturnsExisting` asserts an identical set returns the same pointer rather than a second child. The library has no program to run for correctness — the `Example` functions and `go test -race` are the whole verification — but the demo's exposition output is a useful sanity check that the text format is well-formed.

## Resources

- [Prometheus exposition formats](https://prometheus.io/docs/instrumenting/exposition_formats/) — the authoritative specification for the text format, label rules, and the cumulative-`le` histogram semantics this exercise implements.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Uint64`, `atomic.Int64`, and the `CompareAndSwap` / `Add` methods that make the observation path lock-free.
- [The Four Golden Signals (Google SRE Book)](https://sre.google/sre-book/monitoring-distributed-systems/#xref_monitoring_golden-signals) — the canonical definition of latency, traffic, errors, and saturation that motivates the four metric families in the demo.
- [`sync.Map`](https://pkg.go.dev/sync#Map) — the concurrent map and its `LoadOrStore`, the atomic check-and-insert the cardinality limiter is built on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-structured-access-logs.md](02-structured-access-logs.md)
