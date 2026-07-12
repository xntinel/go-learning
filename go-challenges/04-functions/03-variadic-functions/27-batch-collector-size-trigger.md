# Exercise 27: Batch Collector with Size and Time Triggers

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

An ingestion pipeline never wants to write one event at a time — too much
per-write overhead — nor wait forever for a batch to fill — too much
latency if traffic is slow. The standard fix is a collector that flushes
whenever *either* threshold fires first: enough items have piled up, or
the oldest pending item has waited too long. `Add(events ...Event)` lets a
producer push one or many events in a single call and immediately learn
whether that push crossed a threshold.

## What you'll build

```text
batchcol/                  independent module: example.com/batchcol
  go.mod                   go 1.24
  batchcol.go              package batchcol; type Event struct{ID string}; type Clock func() time.Time; type Collector (mutex-protected); New, Add, Flush
  cmd/
    demo/
      main.go              runnable demo: size-triggered flush, then a time-triggered flush via a fake clock
  batchcol_test.go          table tests: size trigger, age trigger, forced Flush, concurrent Add with -race
```

- Files: `batchcol.go`, `cmd/demo/main.go`, `batchcol_test.go`.
- Implement: `Event`, `Clock func() time.Time`, `Collector` with `New(maxSize int, maxAge time.Duration, now Clock) *Collector`, `(*Collector).Add(events ...Event) ([]Event, bool)`, `(*Collector).Flush() ([]Event, bool)`.
- Test: three events with `maxSize=3` flush on the third `Add`; two events with `maxAge=5s` flush only once the injected clock has advanced past it; concurrent `Add` calls from many goroutines never lose or duplicate an event across all flushed batches (`-race`).
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/27-batch-collector-size-trigger/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/27-batch-collector-size-trigger
go mod edit -go=1.24
```

### Why the clock is injected, and why check-then-flush is one critical section

`Collector` takes a `Clock func() time.Time` instead of calling
`time.Now()` directly so that a test can freeze or fast-forward time
without a real `time.Sleep`. A test that actually slept 5 seconds to prove
the age trigger works would be slow and still flaky under load; a test
that hands in `func() time.Time { return now }` and then mutates the local
`now` variable between calls is instant and exact. Production code just
passes `time.Now` and gets real wall-clock behavior for free — the
indirection costs nothing at the call site.

`Add` appends the incoming events, then checks "is `len(pending) >=
maxSize` or has `maxAge` elapsed since the oldest pending item" and, if
so, flushes — all under one `c.mu.Lock()` held for the whole method. This
matters because "check, then act" is a classic race if the check and the
act are two separate critical sections: two goroutines could both observe
"not yet at threshold" right before each pushes an event that individually
would have tipped it over, and neither one flushes, or worse, both compute
overlapping batches from a `pending` slice that got mutated in between. By
holding the lock across append-check-flush as a single section, the
`Collector` guarantees that whichever goroutine's `Add` call happens to
tip the batch over threshold is exactly the one (and the only one) that
observes `flushed == true` and takes ownership of that batch.

Create `batchcol.go`:

```go
// batchcol.go
package batchcol

import (
	"sync"
	"time"
)

// Event is one item accumulated into a batch.
type Event struct {
	ID string
}

// Clock returns the current time. Production code passes time.Now;
// tests pass a fake clock so flush-on-timeout is deterministic.
type Clock func() time.Time

// Collector accumulates events and flushes a batch once it reaches maxSize
// items or the oldest pending item has waited maxAge, whichever comes
// first. A Collector must not be copied after first use.
type Collector struct {
	mu      sync.Mutex
	maxSize int
	maxAge  time.Duration
	now     Clock

	pending    []Event
	start      time.Time
	hasPending bool
}

// New builds a Collector that flushes at maxSize items or maxAge elapsed
// since the first item currently pending, using now to read the time.
func New(maxSize int, maxAge time.Duration, now Clock) *Collector {
	return &Collector{
		maxSize: maxSize,
		maxAge:  maxAge,
		now:     now,
	}
}

// Add appends events to the pending batch and, if the size or age
// threshold is now met, flushes and returns the flushed batch and true.
// Otherwise it returns nil, false. The size-then-age check and the flush
// itself happen under a single lock, so two goroutines calling Add
// concurrently can never both observe "not yet full" and each flush a
// partial, overlapping batch.
func (c *Collector) Add(events ...Event) ([]Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	for _, ev := range events {
		if !c.hasPending {
			c.start = now
			c.hasPending = true
		}
		c.pending = append(c.pending, ev)
	}

	if !c.shouldFlushLocked(now) {
		return nil, false
	}
	return c.flushLocked(), true
}

// Flush forces out whatever is pending, regardless of threshold, and
// reports whether there was anything to flush.
func (c *Collector) Flush() ([]Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.hasPending {
		return nil, false
	}
	return c.flushLocked(), true
}

// shouldFlushLocked must be called with c.mu held.
func (c *Collector) shouldFlushLocked(now time.Time) bool {
	if len(c.pending) >= c.maxSize {
		return true
	}
	return c.hasPending && now.Sub(c.start) >= c.maxAge
}

// flushLocked must be called with c.mu held.
func (c *Collector) flushLocked() []Event {
	batch := c.pending
	c.pending = nil
	c.hasPending = false
	c.start = time.Time{}
	return batch
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"time"

	"example.com/batchcol"
)

func main() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	c := batchcol.New(3, 5*time.Second, clock)

	batch, flushed := c.Add(batchcol.Event{ID: "e1"}, batchcol.Event{ID: "e2"})
	fmt.Printf("after 2 events: flushed=%v batch=%v\n", flushed, batch)

	batch, flushed = c.Add(batchcol.Event{ID: "e3"})
	fmt.Printf("after 3rd event (size trigger): flushed=%v batch=%v\n", flushed, batch)

	c.Add(batchcol.Event{ID: "e4"})
	now = now.Add(6 * time.Second)
	batch, flushed = c.Add(batchcol.Event{ID: "e5"})
	fmt.Printf("after timeout elapsed (time trigger): flushed=%v batch=%v\n", flushed, batch)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after 2 events: flushed=false batch=[]
after 3rd event (size trigger): flushed=true batch=[{e1} {e2} {e3}]
after timeout elapsed (time trigger): flushed=true batch=[{e4} {e5}]
```

### Tests

`TestAddConcurrentNoLostOrDuplicatedEvents` is the load-bearing one: it
freezes the clock (so only the size trigger ever fires), fires many
concurrent `Add` calls from multiple goroutines, collects every flushed
batch plus one final forced `Flush`, and asserts the total count and the
exact set of event IDs match — proving no event was ever dropped or handed
out in two different batches.

Create `batchcol_test.go`:

```go
// batchcol_test.go
package batchcol

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestAddFlushesOnSize(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(3, time.Hour, func() time.Time { return base })

	if _, flushed := c.Add(Event{ID: "e1"}, Event{ID: "e2"}); flushed {
		t.Fatal("expected no flush before reaching maxSize")
	}
	batch, flushed := c.Add(Event{ID: "e3"})
	if !flushed {
		t.Fatal("expected a flush at maxSize")
	}
	if len(batch) != 3 {
		t.Fatalf("batch len = %d, want 3", len(batch))
	}
}

func TestAddFlushesOnAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := New(10, 5*time.Second, clock)

	if _, flushed := c.Add(Event{ID: "e1"}); flushed {
		t.Fatal("expected no flush immediately")
	}
	now = now.Add(6 * time.Second)
	batch, flushed := c.Add(Event{ID: "e2"})
	if !flushed {
		t.Fatal("expected a flush once maxAge elapsed")
	}
	if len(batch) != 2 {
		t.Fatalf("batch len = %d, want 2", len(batch))
	}
}

func TestFlushForcesPending(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(100, time.Hour, func() time.Time { return base })

	c.Add(Event{ID: "e1"})
	batch, ok := c.Flush()
	if !ok || len(batch) != 1 {
		t.Fatalf("Flush() = %v, %v, want 1 event, true", batch, ok)
	}
	_, ok = c.Flush()
	if ok {
		t.Fatal("second Flush on empty collector should report false")
	}
}

func TestAddConcurrentNoLostOrDuplicatedEvents(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return base } // frozen: only size triggers flushes
	c := New(4, time.Hour, clock)

	const goroutines = 8
	const perGoroutine = 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allFlushed []string

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				id := eventID(g, i)
				if batch, flushed := c.Add(Event{ID: id}); flushed {
					mu.Lock()
					for _, ev := range batch {
						allFlushed = append(allFlushed, ev.ID)
					}
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()

	if rest, ok := c.Flush(); ok {
		for _, ev := range rest {
			allFlushed = append(allFlushed, ev.ID)
		}
	}

	if want := goroutines * perGoroutine; len(allFlushed) != want {
		t.Fatalf("total flushed events = %d, want %d (lost or duplicated)", len(allFlushed), want)
	}

	sort.Strings(allFlushed)
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			id := eventID(g, i)
			idx := sort.SearchStrings(allFlushed, id)
			if idx >= len(allFlushed) || allFlushed[idx] != id {
				t.Fatalf("event %s missing from flushed output", id)
			}
		}
	}
}

func eventID(g, i int) string {
	return fmt.Sprintf("g%d-i%d", g, i)
}
```

## Review

`Collector` is correct when every event passed to `Add` ends up in
exactly one flushed batch (from `Add` or from a later `Flush`), the flush
fires the instant either threshold is crossed, and concurrent callers
never observe a torn or duplicated batch. The senior points are the
injected `Clock`, which turns a would-be flaky sleep-based timing test
into an instant, deterministic one, and locking the entire
check-then-flush sequence as one critical section instead of locking only
the append or only the flush — a partial lock here is the textbook
check-then-act race, and it would only show up under real concurrent load,
which is exactly why the test suite exercises it with `-race` and many
goroutines instead of trusting it by inspection.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex)
- [Go blog: race detector](https://go.dev/blog/race-detector)
- [`time.Time` and durations](https://pkg.go.dev/time#Time)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-worker-pool-task-enqueuer.md](26-worker-pool-task-enqueuer.md) | Next: [28-payload-validation-rule-engine.md](28-payload-validation-rule-engine.md)
