# Exercise 7: Metrics aggregator — accumulate counters in place through a pointer

A per-endpoint stats aggregator must share one counter instance across many
`Record` calls, or updates land on throwaway copies and vanish. This module builds
that aggregator with `map[string]*Stats`, shows the value-passing variant silently
losing updates, and explains why the map must hold pointers.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
metrics/                   independent module: example.com/metrics
  go.mod                   module example.com/metrics
  metrics.go               Stats{Count,TotalLatencyNS,MaxNS}; Record(*Stats, int64); Collector with map[string]*Stats
  cmd/
    demo/
      main.go              records several latencies, prints accumulated stats
  metrics_test.go          pointer accumulates; value variant loses updates; per-endpoint isolation
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `Record(s *Stats, latencyNS int64)` that mutates the shared accumulator, and a `Collector` holding `map[string]*Stats` that hands out the same `*Stats` per key.
- Test: recording several latencies through one `*Stats` accumulates `Count`, `TotalLatencyNS`, and `MaxNS`; a value-passing variant loses updates; two endpoints stay isolated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/01-pointer-basics/07-in-place-metrics-aggregation/cmd/demo
cd go-solutions/09-pointers/01-pointer-basics/07-in-place-metrics-aggregation
```

### Why the accumulator must be shared by pointer

`Stats` holds `Count`, `TotalLatencyNS`, and `MaxNS`. `Record(s *Stats, latencyNS
int64)` increments the count, adds the latency, and raises the max — all through the
pointer, so every call mutates the *same* instance. If `Record` took `Stats` by
value it would increment a copy and the caller's accumulator would never move: the
value-passing variant in the test records ten latencies and ends with `Count == 0`,
which is the exact bug this module warns about. This is the production shape of the
`incrementValue` vs `incrementPointer` contrast from Exercise 1.

The `Collector` stores `map[string]*Stats`. This is a deliberate design choice
forced by addressability: you cannot take `&m[k]` on a `map[string]Stats` (map
elements are not addressable, so `&collector.stats["GET /users"]` is a compile
error), and `m[k].Count++` on a value map does not compile either. Storing pointers
sidesteps both: `Get(endpoint)` looks up the key, lazily creates a `&Stats{}` on a
miss, stores it, and returns the pointer — so every caller for that endpoint shares
one accumulator and `Record(c.Get(ep), latency)` accumulates correctly. If the map
held `Stats`, each `Get` would return a copy and updates would be lost on write-back
unless you carefully re-stored the whole value every time.

Create `metrics.go`:

```go
package metrics

// Stats is a per-endpoint accumulator.
type Stats struct {
	Count          int64
	TotalLatencyNS int64
	MaxNS          int64
}

// Record folds one observation into the shared accumulator through the pointer.
// Passing *Stats (not Stats) is required: a value copy would drop the update.
func Record(s *Stats, latencyNS int64) {
	s.Count++
	s.TotalLatencyNS += latencyNS
	if latencyNS > s.MaxNS {
		s.MaxNS = latencyNS
	}
}

// Collector holds one *Stats per endpoint. The map stores pointers because a
// map element is not addressable (&m[k] is illegal), so the only way to mutate
// a stored Stats in place is to store a pointer to it.
type Collector struct {
	stats map[string]*Stats
}

// NewCollector builds an empty collector.
func NewCollector() *Collector {
	return &Collector{stats: make(map[string]*Stats)}
}

// Get returns the shared *Stats for endpoint, creating it on first use. Every
// caller for the same endpoint gets the same pointer.
func (c *Collector) Get(endpoint string) *Stats {
	s, ok := c.stats[endpoint]
	if !ok {
		s = &Stats{}
		c.stats[endpoint] = s
	}
	return s
}

// Observe records a latency for an endpoint in one step.
func (c *Collector) Observe(endpoint string, latencyNS int64) {
	Record(c.Get(endpoint), latencyNS)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func main() {
	c := metrics.NewCollector()
	for _, ns := range []int64{100, 250, 90, 300} {
		c.Observe("GET /users", ns)
	}
	c.Observe("POST /users", 500)

	s := c.Get("GET /users")
	fmt.Printf("GET /users: count=%d total=%dns max=%dns\n", s.Count, s.TotalLatencyNS, s.MaxNS)

	p := c.Get("POST /users")
	fmt.Printf("POST /users: count=%d total=%dns max=%dns\n", p.Count, p.TotalLatencyNS, p.MaxNS)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /users: count=4 total=740ns max=300ns
POST /users: count=1 total=500ns max=500ns
```

### Tests

`TestPointerAccumulates` records several latencies through one `*Stats` and asserts
all three fields accumulate. `TestValueVariantLosesUpdates` defines a deliberately
wrong `recordByValue(s Stats, ...)` inside the test, feeds it the same latencies,
and asserts the caller's `Stats` stayed zero — proving the pointer is required.
`TestEndpointsIsolated` records on two endpoints and asserts their accumulators do
not bleed into each other.

Create `metrics_test.go`:

```go
package metrics

import (
	"fmt"
	"testing"
)

// recordByValue is the buggy value-passing variant kept only to prove updates
// are lost. Production code must never do this.
func recordByValue(s Stats, latencyNS int64) {
	s.Count++
	s.TotalLatencyNS += latencyNS
}

func TestPointerAccumulates(t *testing.T) {
	t.Parallel()

	var s Stats
	for _, ns := range []int64{100, 250, 90, 300} {
		Record(&s, ns)
	}
	if s.Count != 4 {
		t.Fatalf("Count = %d, want 4", s.Count)
	}
	if s.TotalLatencyNS != 740 {
		t.Fatalf("TotalLatencyNS = %d, want 740", s.TotalLatencyNS)
	}
	if s.MaxNS != 300 {
		t.Fatalf("MaxNS = %d, want 300", s.MaxNS)
	}
}

func TestValueVariantLosesUpdates(t *testing.T) {
	t.Parallel()

	var s Stats
	for _, ns := range []int64{100, 250, 90, 300} {
		recordByValue(s, ns) // mutates a copy; s never changes
	}
	if s.Count != 0 || s.TotalLatencyNS != 0 {
		t.Fatalf("value variant unexpectedly mutated s = %+v; want zero (updates should be lost)", s)
	}
}

func TestEndpointsIsolated(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	c.Observe("a", 10)
	c.Observe("a", 20)
	c.Observe("b", 5)

	if a := c.Get("a"); a.Count != 2 || a.TotalLatencyNS != 30 {
		t.Fatalf("endpoint a = %+v, want count 2 total 30", *a)
	}
	if b := c.Get("b"); b.Count != 1 || b.TotalLatencyNS != 5 {
		t.Fatalf("endpoint b = %+v, want count 1 total 5", *b)
	}
}

func TestGetReturnsSamePointer(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	if c.Get("x") != c.Get("x") {
		t.Fatal("Get returned different pointers for the same endpoint; the accumulator is not shared")
	}
}

func Example() {
	c := NewCollector()
	c.Observe("GET /", 100)
	c.Observe("GET /", 200)
	s := c.Get("GET /")
	fmt.Println(s.Count, s.TotalLatencyNS)
	// Output: 2 300
}
```

## Review

The aggregator is correct when many `Record` calls fold into one accumulator: the
pointer variant drives `Count` to 4 and `TotalLatencyNS` to 740, while the value
variant leaves the caller at zero — the whole point. The `map[string]*Stats` choice
is not stylistic: `&collector.stats[ep]` does not compile because map elements are
unaddressable, so pointers are the mechanism that lets a stored `Stats` be mutated
in place. `TestGetReturnsSamePointer` pins that a given endpoint always yields the
same instance, which is what makes accumulation across calls work. This module is
single-goroutine; a real collector shared across request handlers would guard the
map and the counters with a mutex or use `sync/atomic` — that concurrency layer is a
later lesson, but the pointer-sharing that makes it necessary is exactly what you
built here. Run `go test -race`.

## Resources

- [Go Language Specification: Address operators](https://go.dev/ref/spec#Address_operators) — why `&m[k]` on a map is illegal.
- [Effective Go: Maps](https://go.dev/doc/effective_go#maps) — map value semantics.
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — when to share by pointer.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-large-request-copy-cost.md](06-large-request-copy-cost.md) | Next: [08-range-copy-address-trap.md](08-range-copy-address-trap.md)
