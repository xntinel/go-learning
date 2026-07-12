# Exercise 2: Counting errors by category for dashboards and alerts

A log line answers "what happened to this one request." It cannot answer "how
often, and in which category." That is the metric's job. This module builds a
`Metrics` type that classifies each error with `errors.Is` into per-category
atomic counters and exposes a `Snapshot` map that a `/metrics` scrape can read —
the aggregate a dashboard graphs and an alert fires on.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
errmetrics/                  independent module: example.com/errmetrics
  go.mod                     go 1.25
  metrics.go                 sentinels; Metrics with atomic.Int64 counters; Record, Snapshot
  cmd/
    demo/
      main.go                runnable demo: record a mix, print the snapshot
  metrics_test.go            per-category counts + concurrent -race Record test
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: a `Metrics` type that categorizes each error with `errors.Is` into `not_found`, `invalid`, and `other` `atomic.Int64` counters, and a `Snapshot() map[string]int64`.
- Test: drive a mix of not-found, invalid, and unknown errors; assert `Snapshot` returns the exact per-category counts; run `Record` from parallel goroutines under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/14-error-observability/02-error-metrics-by-category/cmd/demo
cd go-solutions/10-error-handling/14-error-observability/02-error-metrics-by-category
go mod edit -go=1.25
```

### Why categorize, and why atomic

A single `errors_total` counter is nearly useless on a dashboard: a spike could
be a harmless flood of `not_found` from a crawler hitting dead links, or a
database that just fell over. You cannot tell, so you cannot alert intelligently.
Classifying with `errors.Is` into separate counters (`not_found`, `invalid`, and
a catch-all `other`) lets each category be graphed and alerted independently —
`not_found` you might ignore, a rising `other` you page on.

`Record` is called from many request-handling goroutines at once, so the counters
must be concurrency-safe without a lock on the hot path. `atomic.Int64` gives
exactly that: `Add(1)` is a single atomic increment, `Load()` a single atomic
read, no mutex. This is the standard shape for a metric counter — cheap enough to
call on every request. `Snapshot` reads all three with `Load()` into a plain map
the caller can serialize; note that under concurrent writes the three loads are
not a single atomic instant (the snapshot can catch counter A after an increment
and counter B before a paired one), which is fine for monitoring — counters are
monotone and eventually consistent, and no dashboard needs a cross-counter
transactional read.

Classification order matters: `errors.Is` walks the wrap chain, so if an error
could match more than one sentinel the first `case` wins. Here the sentinels are
disjoint, but keeping the most specific categories first and `default` last is
the habit that scales to a real taxonomy.

Create `metrics.go`:

```go
package errmetrics

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// Sentinels the service layer wraps with %w. Metrics classifies by matching them.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

// Metrics counts errors by category using lock-free atomic counters, so Record
// is cheap enough to call on every failed request from any goroutine.
type Metrics struct {
	notFound atomic.Int64
	invalid  atomic.Int64
	other    atomic.Int64
}

// Record classifies err with errors.Is and increments the matching counter.
// A nil error is ignored so callers can pass the result of an operation directly.
func (m *Metrics) Record(err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, ErrNotFound):
		m.notFound.Add(1)
	case errors.Is(err, ErrInvalid):
		m.invalid.Add(1)
	default:
		m.other.Add(1)
	}
}

// Snapshot reads the counters into a map suitable for a /metrics scrape.
func (m *Metrics) Snapshot() map[string]int64 {
	return map[string]int64{
		"not_found": m.notFound.Load(),
		"invalid":   m.invalid.Load(),
		"other":     m.other.Load(),
	}
}

// helpers used by the demo to fabricate wrapped errors of each category.
func notFoundErr(id string) error { return fmt.Errorf("get %q: %w", id, ErrNotFound) }
func invalidErr() error           { return fmt.Errorf("put: %w", ErrInvalid) }
```

### The runnable demo

The demo records a realistic mix — several misses, one bad request, one unknown
failure — and prints the snapshot in a stable key order so the Expected-output
block is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/errmetrics"
)

func main() {
	var m errmetrics.Metrics

	m.Record(fmt.Errorf("get %q: %w", "a", errmetrics.ErrNotFound))
	m.Record(fmt.Errorf("get %q: %w", "b", errmetrics.ErrNotFound))
	m.Record(fmt.Errorf("get %q: %w", "c", errmetrics.ErrNotFound))
	m.Record(fmt.Errorf("put: %w", errmetrics.ErrInvalid))
	m.Record(errors.New("connection reset by peer"))
	m.Record(nil) // ignored

	snap := m.Snapshot()
	for _, k := range []string{"not_found", "invalid", "other"} {
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
not_found=3
invalid=1
other=1
```

### Tests

The exact-count test drives a fixed mix and asserts every category. The
concurrency test fires `Record` from many parallel goroutines under `-race` to
prove the atomic counters are safe and the totals are exact — a mutex-free
counter that miscounts under contention would show up here.

Create `metrics_test.go`:

```go
package errmetrics

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestSnapshotExactCounts(t *testing.T) {
	t.Parallel()
	var m Metrics

	m.Record(fmt.Errorf("get: %w", ErrNotFound))
	m.Record(fmt.Errorf("get: %w", ErrNotFound))
	m.Record(fmt.Errorf("put: %w", ErrInvalid))
	m.Record(errors.New("boom"))
	m.Record(nil)

	snap := m.Snapshot()
	want := map[string]int64{"not_found": 2, "invalid": 1, "other": 1}
	for k, v := range want {
		if snap[k] != v {
			t.Fatalf("Snapshot()[%q] = %d, want %d", k, snap[k], v)
		}
	}
}

func TestRecordConcurrent(t *testing.T) {
	t.Parallel()
	var m Metrics
	const perCat = 1000

	var wg sync.WaitGroup
	fire := func(mk func() error) {
		defer wg.Done()
		for range perCat {
			m.Record(mk())
		}
	}
	wg.Add(3)
	go fire(func() error { return fmt.Errorf("x: %w", ErrNotFound) })
	go fire(func() error { return fmt.Errorf("x: %w", ErrInvalid) })
	go fire(func() error { return errors.New("other") })
	wg.Wait()

	snap := m.Snapshot()
	for k, v := range map[string]int64{"not_found": perCat, "invalid": perCat, "other": perCat} {
		if snap[k] != v {
			t.Fatalf("after concurrent Record, %q = %d, want %d", k, snap[k], v)
		}
	}
}

func Example() {
	var m Metrics
	m.Record(fmt.Errorf("get: %w", ErrNotFound))
	m.Record(fmt.Errorf("put: %w", ErrInvalid))
	snap := m.Snapshot()
	fmt.Println(snap["not_found"], snap["invalid"], snap["other"])
	// Output: 1 1 0
}
```

## Review

The type is correct when `Snapshot` returns the exact per-category counts for any
mix of inputs and stays exact under concurrency. `TestSnapshotExactCounts` pins
the classification (including that `nil` is ignored and unknown errors land in
`other`), and `TestRecordConcurrent` under `-race` proves the atomic counters do
not lose increments across 3000 concurrent `Record` calls — the reason to reach
for `atomic.Int64` over a plain `int64` you would have to guard with a mutex.

The mistake to avoid is one counter for everything: it makes the dashboard blind
to which category is failing. The second, subtler one is labeling by an unbounded
key (the id, the full message) instead of a bounded category — that is fine here
because the categories are a fixed small set, but it is exactly the cardinality
trap the RED middleware exercise addresses. Keep the category set small and
closed; wrap with `%w` at the service layer so `errors.Is` can classify here.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, its `Add` and `Load`, the lock-free counter primitive.
- [`errors`](https://pkg.go.dev/errors) — `errors.Is` walking the `%w` chain to classify.
- [Prometheus: metric and label naming](https://prometheus.io/docs/practices/naming/) — why categories are labels and why they must stay low-cardinality.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-context-scoped-correlation-fields.md](03-context-scoped-correlation-fields.md)
