# Exercise 3: Exposing Channel Saturation as an Observability Gauge (len/cap)

When a buffered work channel is your queue, its fullness is a signal an SRE wants on
a dashboard: `len(ch)/cap(ch)` is the queue-saturation gauge that drives alerts and
autoscaling. This exercise builds a small `QueueDepth` helper that publishes that
utilization, and — just as importantly — nails down that `len`/`cap` are advisory
metrics you must never use as a control-flow gate.

This module is fully self-contained.

## What you'll build

```text
queuedepth/                  module: example.com/queuedepth
  go.mod                     go 1.26
  queuedepth.go              type Queue[T]; Enqueue, TryDequeue, Len, Cap, Utilization
  cmd/
    demo/
      main.go                fill a queue partway and print the saturation gauge
  queuedepth_test.go         len/cap/utilization assertions, drain, advisory-only note
```

- Files: `queuedepth.go`, `cmd/demo/main.go`, `queuedepth_test.go`.
- Implement: a `Queue[T]` wrapping a buffered channel with `Len`, `Cap`, `Utilization() float64`, blocking `Enqueue`, and non-blocking `TryDequeue`.
- Test: fill to K without a receiver and assert `len==K`, `cap==N`, `utilization==K/N`; drain and assert `len` returns toward 0.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/queuedepth/cmd/demo
cd ~/go-exercises/queuedepth
go mod init example.com/queuedepth
go mod edit -go=1.26
```

### Why len/cap are a legitimate metric and an illegitimate predicate

`len(ch)` on a buffered channel returns the number of elements currently in the
buffer; `cap(ch)` returns the fixed buffer size. Both are cheap, non-blocking reads.
Their ratio, `len(ch)/cap(ch)`, is a *saturation* number in `[0, 1]`: 0 means the
queue is empty (consumers keeping up), 1 means it is full (producers are about to
block — backpressure imminent). This is exactly what you want on a dashboard and
exactly what an autoscaler consumes to decide whether to add consumers. Publishing it
as a gauge is a first-class, idiomatic use of `len`/`cap`.

The catch is that the number is a *racy snapshot*. By the time `Utilization` has read
`len` and divided by `cap`, another goroutine may have enqueued or dequeued several
items — the value you return was true for an instant that has already passed. For a
metric that is completely fine: a dashboard showing "queue was 70% full a moment ago"
is informative, and an autoscaler smooths over noise anyway. Monotonicity of the
underlying trend (rising when producers outpace consumers, falling otherwise) is what
makes it useful, not instantaneous exactness.

The same raciness makes `len` catastrophic as *control flow*. `if q.Len() == 0`
before a dequeue, or `if q.Len() < q.Cap()` before an enqueue, is a check-then-act
race: the condition can flip between the check and the act, so you get a blocked
dequeue on a now-empty queue or a blocked enqueue on a now-full one — the exact stall
the guard was meant to prevent. The correct control-flow primitives are the blocking
send/receive (let the channel do the synchronization) or `select`+`default` for a
non-blocking try. `Enqueue` here blocks; `TryDequeue` uses `select`+`default`. Neither
consults `len`. `Len`/`Cap`/`Utilization` exist *only* to feed the gauge.

Create `queuedepth.go`:

```go
package queuedepth

// Queue is a bounded work queue backed by a buffered channel. Len/Cap/Utilization
// are advisory saturation metrics; Enqueue and TryDequeue are the control-flow
// operations and never consult len.
type Queue[T any] struct {
	ch chan T
}

// New returns a queue whose buffer holds up to capacity items.
func New[T any](capacity int) *Queue[T] {
	return &Queue[T]{ch: make(chan T, capacity)}
}

// Enqueue blocks until the item is buffered (or a consumer takes it). It relies on
// the channel for synchronization, not on len.
func (q *Queue[T]) Enqueue(item T) {
	q.ch <- item
}

// TryDequeue removes and returns an item without blocking. ok is false when the
// queue is momentarily empty.
func (q *Queue[T]) TryDequeue() (item T, ok bool) {
	select {
	case v := <-q.ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}

// Len is the current buffered depth: an advisory, racy snapshot for metrics only.
func (q *Queue[T]) Len() int { return len(q.ch) }

// Cap is the fixed buffer capacity.
func (q *Queue[T]) Cap() int { return cap(q.ch) }

// Utilization reports len/cap in [0,1] as a saturation gauge. Advisory only.
func (q *Queue[T]) Utilization() float64 {
	c := cap(q.ch)
	if c == 0 {
		return 0
	}
	return float64(len(q.ch)) / float64(c)
}
```

### The runnable demo

The demo fills 3 of 4 slots (no consumer running) and prints the gauge, then drains
one and prints again — a stand-in for what a metrics scrape would observe.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/queuedepth"
)

func main() {
	q := queuedepth.New[string](4)
	for _, job := range []string{"a", "b", "c"} {
		q.Enqueue(job)
	}
	fmt.Printf("depth=%d cap=%d utilization=%.2f\n", q.Len(), q.Cap(), q.Utilization())

	q.TryDequeue()
	fmt.Printf("depth=%d cap=%d utilization=%.2f\n", q.Len(), q.Cap(), q.Utilization())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
depth=3 cap=4 utilization=0.75
depth=2 cap=4 utilization=0.50
```

### Tests

`TestSaturationGauge` fills K of N slots with no consumer and asserts `Len==K`,
`Cap==N`, and `Utilization==K/N` exactly — deterministic because there is no
concurrent activity to make the snapshot stale. `TestDrainLowersDepth` dequeues all
buffered items and asserts `Len` returns to 0. `TestUtilizationBounds` checks the
empty (0.0) and full (1.0) endpoints. These reads are observational, so `-race` sees
nothing to complain about even when a real system reads the gauge concurrently.

Create `queuedepth_test.go`:

```go
package queuedepth

import (
	"fmt"
	"testing"
)

func TestSaturationGauge(t *testing.T) {
	t.Parallel()

	const n, k = 8, 5
	q := New[int](n)
	for i := range k {
		q.Enqueue(i) // no consumer: values sit in the buffer
	}

	if q.Len() != k {
		t.Fatalf("Len = %d, want %d", q.Len(), k)
	}
	if q.Cap() != n {
		t.Fatalf("Cap = %d, want %d", q.Cap(), n)
	}
	if got, want := q.Utilization(), float64(k)/float64(n); got != want {
		t.Fatalf("Utilization = %v, want %v", got, want)
	}
}

func TestDrainLowersDepth(t *testing.T) {
	t.Parallel()

	q := New[int](4)
	for i := range 4 {
		q.Enqueue(i)
	}
	for range 4 {
		if _, ok := q.TryDequeue(); !ok {
			t.Fatal("TryDequeue returned ok=false while buffer non-empty")
		}
	}
	if q.Len() != 0 {
		t.Fatalf("Len after full drain = %d, want 0", q.Len())
	}
	if _, ok := q.TryDequeue(); ok {
		t.Fatal("TryDequeue returned ok=true on an empty queue")
	}
}

func TestUtilizationBounds(t *testing.T) {
	t.Parallel()

	q := New[int](3)
	if got := q.Utilization(); got != 0.0 {
		t.Fatalf("empty Utilization = %v, want 0.0", got)
	}
	for i := range 3 {
		q.Enqueue(i)
	}
	if got := q.Utilization(); got != 1.0 {
		t.Fatalf("full Utilization = %v, want 1.0", got)
	}
}

func ExampleQueue_Utilization() {
	q := New[int](4)
	q.Enqueue(1)
	q.Enqueue(2)
	fmt.Printf("%.2f\n", q.Utilization())
	// Output: 0.50
}
```

## Review

The gauge is correct when `Utilization` is `len/cap` guarded against a zero-capacity
divide, and when the *control-flow* operations (`Enqueue`, `TryDequeue`) never read
`len` — they let the channel and `select`+`default` do the synchronizing. The
assertions are exact only because the tests run with no concurrent producer/consumer;
in production the same read is a slightly-stale snapshot, which is acceptable for a
metric and unacceptable for a predicate. The mistake to avoid is graduating the gauge
into a gate: `if q.Len() < q.Cap()` before `Enqueue` is a check-then-act race that
reintroduces the stall it pretends to prevent. Keep `len`/`cap` on the dashboard, and
keep the blocking op or `select`+`default` in the code path.

## Resources

- [Go spec: Length and capacity](https://go.dev/ref/spec#Length_and_capacity) — `len`/`cap` on channels and their meaning.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking receive via `default`.
- [pkg.go.dev: expvar](https://pkg.go.dev/expvar) — how a real Go service publishes such gauges for scraping.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-buffered-worker-pool.md](02-buffered-worker-pool.md) | Next: [04-load-shedding-nonblocking-enqueue.md](04-load-shedding-nonblocking-enqueue.md)
