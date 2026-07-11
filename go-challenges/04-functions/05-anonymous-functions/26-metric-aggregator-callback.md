# Exercise 26: Metrics Collection Pipeline Built from Anonymous Callback Functions

**Nivel: Intermedio** — validacion rapida (un test corto).

A metrics pipeline that exports a named `RecordRequest`, `RecordError`, and
`RecordBytes` function for every call site that wants to report something
grows one exported function per metric forever. This module builds the
alternative: a single `Counter.Run` that hands every collector one private
`record` closure, and callers write their own one-off collector as an
anonymous function inline, right where the metric is produced.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
metrics/                      module example.com/metrics
  go.mod
  metrics.go                   Counter, private record closure, Collector, Run, Snapshot
  metrics_test.go               aggregation, cross-call persistence, concurrent Run callers
  cmd/demo/main.go              three inline collectors reporting into one Counter
```

- Files: `metrics.go`, `metrics_test.go`, `cmd/demo/main.go`.
- Implement: `Counter` with a private `record(name string, delta int64)` method; `Collector` as `func(record func(string, int64))`; `Run(collectors ...Collector)` that hands each collector the `record` closure; `Snapshot()` returning a safe copy.
- Test: aggregation across several collectors and several `Run` calls; a concurrent-callers test proving `Run` is race-free under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metrics/cmd/demo
cd ~/go-exercises/metrics
go mod init example.com/metrics
go mod edit -go=1.24
```

### One private closure funnels every anonymous collector

`Counter` never exports `record` — it's a method used only through the
closure `Run` passes into each collector. That is the whole point of the
design: a `Collector` is just `func(record func(name string, delta int64))`,
so a call site never needs a named, exported reporting function at all. It
writes its increments as an anonymous function literal right where the
event happens — a request handler, a batch loop, a log parser — closing
over whatever local context it already has (a status code, a byte count)
and calling `record` as many times, under as many names, as it needs. `Run`
itself knows nothing about any collector's internal logic; it only owns
locking `record` around the map write, so every collector, however many
there are, funnels through the one place a mutex is needed.

Create `metrics.go`:

```go
package metrics

import "sync"

// Counter aggregates named integer values reported by collector callbacks.
type Counter struct {
	mu     sync.Mutex
	values map[string]int64
}

// NewCounter returns an empty Counter ready to accumulate.
func NewCounter() *Counter {
	return &Counter{values: make(map[string]int64)}
}

// record is passed into every collector as its only argument. It is never
// exported: collectors are anonymous functions closed over whatever local
// state they need, and this closure is the single funnel every one of them
// writes through, so the mutex only has to live in one place.
func (c *Counter) record(name string, delta int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[name] += delta
}

// Collector is the shape every callback passed to Run must have: given a
// record function, it reports zero or more increments. Callers write these
// inline as anonymous functions -- there is deliberately no named,
// exported function type implementing this pipeline stage, because each
// collector is a one-off closing over whatever the call site already has
// in scope (a request struct, a batch of rows, a parsed log line).
type Collector func(record func(name string, delta int64))

// Run executes every collector in order, handing each one the private
// record closure. Nothing about a collector's own logic -- how many times
// it calls record, under what names -- is known to Run; it only owns the
// aggregation.
func (c *Counter) Run(collectors ...Collector) {
	for _, collect := range collectors {
		collect(c.record)
	}
}

// Snapshot returns a stable copy of the current values, safe to read after
// Run has returned (or concurrently, since it takes the same lock).
func (c *Counter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.values))
	for k, v := range c.values {
		out[k] = v
	}
	return out
}
```

### The runnable demo

The demo reports three metrics through three inline collectors — none of
them named functions.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/metrics"
)

func main() {
	counter := metrics.NewCounter()

	counter.Run(
		func(record func(string, int64)) {
			record("requests", 1)
			record("requests", 1)
		},
		func(record func(string, int64)) {
			record("errors", 1)
		},
		func(record func(string, int64)) {
			record("bytes_in", 512)
		},
	)

	snap := counter.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d\n", k, snap[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bytes_in=512
errors=1
requests=2
```

### Tests

`TestCounterRunAggregates` checks a batch of collectors adds up correctly
in one `Run` call. `TestCounterRunAcrossMultipleCalls` checks values persist
and keep accumulating across separate `Run` calls. `TestCounterRunConcurrentCallersAreRaceFree`
launches many goroutines each calling `Run` with its own collector and
verifies the final count is exact under `-race` — proof the single `record`
funnel is really serializing every write.

Create `metrics_test.go`:

```go
package metrics

import (
	"sync"
	"testing"
)

func TestCounterRunAggregates(t *testing.T) {
	t.Parallel()
	c := NewCounter()

	c.Run(
		func(record func(string, int64)) {
			record("requests", 1)
			record("requests", 1)
			record("requests", 1)
		},
		func(record func(string, int64)) {
			record("errors", 1)
		},
	)

	snap := c.Snapshot()
	if snap["requests"] != 3 {
		t.Fatalf("requests = %d, want 3", snap["requests"])
	}
	if snap["errors"] != 1 {
		t.Fatalf("errors = %d, want 1", snap["errors"])
	}
	if _, ok := snap["bytes_in"]; ok {
		t.Fatalf("snapshot has unexpected key bytes_in")
	}
}

func TestCounterRunAcrossMultipleCalls(t *testing.T) {
	t.Parallel()
	c := NewCounter()

	c.Run(func(record func(string, int64)) { record("hits", 5) })
	c.Run(func(record func(string, int64)) { record("hits", 2) })

	if got := c.Snapshot()["hits"]; got != 7 {
		t.Fatalf("hits = %d, want 7 (aggregation must persist across Run calls)", got)
	}
}

func TestCounterRunConcurrentCallersAreRaceFree(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Run(func(record func(string, int64)) { record("concurrent", 1) })
		}()
	}
	wg.Wait()

	if got := c.Snapshot()["concurrent"]; got != 50 {
		t.Fatalf("concurrent = %d, want 50", got)
	}
}
```

## Review

The pipeline is correct when `Snapshot` always reflects exactly the sum of
every `record` call any collector made, no matter how many collectors ran,
across how many `Run` calls, from how many goroutines. The design's whole
value is negative: it is the *absence* of exported per-metric functions.
The moment a collector needs a fourth or fifth metric, or a metric only one
call site ever reports, nothing in `Counter`'s public surface has to change
— the new collector is just another anonymous function literal passed to
`Run`. The one discipline that must hold is that every collector writes
only through the `record` closure it was given, never by reaching into
`Counter`'s fields directly (which isn't even possible from another
package, since `values` and `record` are unexported) — that unexported
funnel is what keeps the mutex correct without collectors needing to know
it exists.

## Resources

- [Go Language Specification: Function types](https://go.dev/ref/spec#Function_types)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Effective Go: Closures](https://go.dev/doc/effective_go#closures)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-deferred-cleanup-guard.md](25-deferred-cleanup-guard.md) | Next: [27-canary-flag-iife.md](27-canary-flag-iife.md)
