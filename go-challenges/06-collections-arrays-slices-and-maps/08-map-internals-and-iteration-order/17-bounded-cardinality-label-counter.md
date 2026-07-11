# Exercise 17: Bounded-Cardinality Label Counter Against Metrics Explosion

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A metrics aggregator sitting in front of a time-series backend -- the shape
every Prometheus client library or Datadog custom-metric pipeline takes --
counts requests by label combination before exposition: route, status code,
tenant. Each distinct combination becomes one time series downstream, and a
time-series database's cost is dominated by how many distinct series it
carries, not by how many events fed them. The moment one label component is
influenced by something outside your control -- a raw user ID leaking into
a "tenant" label, a URL slug with a UUID in it treated as a "route" label --
the number of distinct combinations stops being bounded by your product's
shape and starts being bounded only by how many different values an
attacker, or an ordinary buggy client, can send. This is the cardinality
explosion that has taken down real metrics backends: millions of one-off
series appear in minutes, memory on the aggregator and the downstream store
both blow up, and every other team's dashboards go dark along with yours.

The trap is structural, not a typo: `counts[labelKey(ev)]++` is exactly the
right code for a metrics counter, and it is also exactly the code that has
no size bound. A plain Go map never refuses an insert; it grows to hold
whatever keys you give it. Nothing about the syntax signals "this map's
domain is attacker-influenced" -- that has to be a design decision made
before the first line of the counter is written, not a fix applied after
the incident.

This module builds `BoundedCounter` as the package you drop into an
aggregation pipeline: a counter keyed by a comparable `Labels` struct that
caps the number of distinct real combinations it will ever track, and
collapses everything past that cap into a single `Other: true` overflow
bucket rather than growing without bound. The unbounded version never
appears in the type's API; it lives in the test file, isolated, as the
counter the tests prove would have grown forever.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
labelcounter/              module example.com/labelcounter
  go.mod                   go 1.24
  labelcounter.go          Labels, BoundedCounter; New, Record, Series, Overflow, Snapshot
  labelcounter_test.go     within-cap table, overflow boundary, snapshot aliasing,
                            unbounded-growth contrast, concurrency, ExampleBoundedCounter_Record
```

- Files: `labelcounter.go`, `labelcounter_test.go`.
- Implement: `New(maxSeries int) (*BoundedCounter, error)` rejecting a non-positive cap with `ErrInvalidMaxSeries`; `(*BoundedCounter).Record(labels Labels)` incrementing an existing series directly and collapsing any new series past the cap into the `Labels{Other: true}` overflow bucket; `Series() int`, `Overflow() int64`, and `Snapshot() map[Labels]int64` returning an independent copy.
- Test: the within-cap table; the exact boundary where a new label first overflows, including that already-tracked series keep incrementing normally afterward; `Snapshot` never aliasing the internal map; an `unboundedRecord` contrast proving a plain map's key count tracks event count 1:1 while `BoundedCounter.Series()` stays at the cap; concurrent `Record` calls from many goroutines; and `ExampleBoundedCounter_Record` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/labelcounter
cd ~/go-exercises/labelcounter
go mod init example.com/labelcounter
go mod edit -go=1.24
```

### A map has no size bound; the domain does

`counts[labels]++` on a plain `map[Labels]int64` is unconditionally correct
Go: it initializes an absent entry to its zero value and increments it, or
increments an existing one, in one expression. The property that makes it
dangerous here is not in that line at all -- it is in where `labels` comes
from. If every component of `labels` is drawn from a small, product-defined
set (a handful of routes, a handful of status codes), the map's size is
naturally bounded by that set's size and growing without a cap is fine. The
instant one component is copied from request data an external party
controls, the map's size becomes bounded by *their* behavior instead:

```go
// BUGGY: no cap. counts grows one entry per distinct label combination,
// and an attacker or a buggy client controls how many that is.
counts[labelKey(ev)]++
```

Nothing here is a bug in the conventional sense -- it does exactly what it
says. The defect is the absence of a decision: *what happens when the
number of distinct combinations exceeds what the downstream time-series
store, or this process's memory, can hold?* A map answers that question by
default with "grow forever", which is the one answer a metrics pipeline can
never afford.

The fix is comma-ok at the cap boundary. `Record` must distinguish "this
exact combination is already tracked, so just increment it" -- always
allowed, no cap applies -- from "this is a brand-new combination", which is
the only case the cap can refuse:

```go
if _, ok := c.counts[labels]; ok {
    c.counts[labels]++   // already tracked: never capped
    return
}
if c.series >= c.maxSeries {
    c.counts[overflow]++ // new, but the cap is full: collapse
    return
}
c.counts[labels] = 1      // new, and there is room
c.series++
```

The two-result map read is what makes that branch possible at all: `v :=
c.counts[labels]` alone cannot tell "zero because absent" from "zero because
this is genuinely the first event for an existing zero-count series" (which
never happens here, but the same idiom is what the config-merge and
metrics-aggregator exercises elsewhere in this lesson rely on for exactly
that reason).

Create `labelcounter.go`:

```go
// Package labelcounter counts events by label combination while capping how
// many distinct combinations it will ever track, so a label that carries
// unexpectedly high-cardinality input (a raw user ID, a URL slug) cannot grow
// the counter without bound.
//
// It exists to get one detail right that a hand-rolled counts[key]++ loop
// gets wrong by omission: a plain map has no built-in size limit, so if any
// component of the key is influenced by a user or an attacker, the map grows
// exactly as fast as they can vary that component. See the package tests for
// a side-by-side demonstration of the unbounded growth this type prevents.
package labelcounter

import (
	"errors"
	"fmt"
	"maps"
	"sync"
)

// ErrInvalidMaxSeries means the configured series cap was not positive.
var ErrInvalidMaxSeries = errors.New("labelcounter: maxSeries must be positive")

// Labels identifies one label combination. Other, when true, marks the
// single overflow bucket that absorbs every distinct combination recorded
// after the counter reaches its configured cap.
type Labels struct {
	Route  string
	Status string
	Tenant string
	Other  bool
}

// overflow is the sentinel key every over-cap combination collapses into.
var overflow = Labels{Other: true}

// BoundedCounter counts events by Labels, capping the number of distinct
// real (non-overflow) label combinations it will track. Once that cap is
// reached, every combination that has not already been seen is counted
// under a single overflow bucket instead of growing the underlying map
// further -- trading per-combination precision for a hard memory bound.
//
// BoundedCounter is safe for concurrent use by multiple goroutines.
type BoundedCounter struct {
	mu        sync.RWMutex
	maxSeries int
	series    int // count of distinct real keys, excluding the overflow bucket
	counts    map[Labels]int64
}

// New returns a BoundedCounter that tracks at most maxSeries distinct real
// label combinations before collapsing the rest into an overflow bucket. It
// returns ErrInvalidMaxSeries if maxSeries is not positive.
func New(maxSeries int) (*BoundedCounter, error) {
	if maxSeries <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxSeries, maxSeries)
	}
	return &BoundedCounter{
		maxSeries: maxSeries,
		counts:    make(map[Labels]int64),
	}, nil
}

// Record increments the counter for labels. If labels is a combination
// already being tracked, it is incremented directly. If labels is new and
// the counter is already at its configured cap, the increment is applied to
// the overflow bucket (Labels{Other: true}) instead of growing the map with
// a new distinct key.
func (c *BoundedCounter) Record(labels Labels) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// comma-ok distinguishes "labels already tracked, just increment" from
	// "labels is a brand-new distinct key" -- only the second case is
	// subject to the cap.
	if _, ok := c.counts[labels]; ok {
		c.counts[labels]++
		return
	}
	if c.series >= c.maxSeries {
		c.counts[overflow]++
		return
	}
	c.counts[labels] = 1
	c.series++
}

// Series reports the number of distinct real label combinations currently
// tracked, excluding the overflow bucket.
func (c *BoundedCounter) Series() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.series
}

// Overflow reports how many events have been collapsed into the overflow
// bucket so far. It is 0 until the cap is first reached.
func (c *BoundedCounter) Overflow() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[overflow]
}

// Snapshot returns an independent copy of the current counts, safe to read
// or mutate without affecting the counter or racing against concurrent
// Record calls. It never returns the counter's internal map.
func (c *BoundedCounter) Snapshot() map[Labels]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return maps.Clone(c.counts)
}
```

### Using it

Construct one `BoundedCounter` per exposition cycle, or once for the process
lifetime, with `maxSeries` set to whatever your downstream time-series store
can comfortably absorb from this one counter. Every request-handling
goroutine calls `Record` directly -- `BoundedCounter` takes its own lock, so
there is nothing for the caller to coordinate. When it is time to expose the
counters (a scrape, a flush), call `Snapshot`: it returns a fresh copy under
the read lock, so the exposition path never blocks a concurrent `Record`
for longer than the copy itself takes, and mutating the returned map can
never corrupt the counter's own state.

`ExampleBoundedCounter_Record` is this module's runnable demonstration: `go
test` executes it and diffs its stdout against the `// Output:` comment
below.

```go
func ExampleBoundedCounter_Record() {
	c, err := New(2)
	if err != nil {
		panic(err)
	}

	c.Record(Labels{Route: "/checkout", Tenant: "acme"})
	c.Record(Labels{Route: "/checkout", Tenant: "acme"})
	c.Record(Labels{Route: "/checkout", Tenant: "globex"})
	c.Record(Labels{Route: "/checkout", Tenant: "attacker-leaked-id"})

	fmt.Println("distinct real series:", c.Series())
	fmt.Println("overflow count:", c.Overflow())

	// Output:
	// distinct real series: 2
	// overflow count: 1
}
```

Two tenants (`acme`, `globex`) fill the cap of 2; the third tenant, standing
in for a value an attacker controls, is collapsed into the overflow bucket
instead of becoming a third series.

### Tests

`TestRecordCountsWithinCap` is the ordinary path: distinct labels each get
their own count, and repeats increment rather than re-inserting.
`TestRecordCollapsesIntoOverflowPastCap` is the boundary case the whole
module is about -- it fills the cap exactly, sends labels past it, and
checks both halves of the comma-ok branch: a brand-new label overflows, but
a label recorded before the cap filled keeps incrementing normally
afterward, because the cap only ever gates the creation of a new series.
`TestSnapshotDoesNotAliasInternalMap` mutates the returned map and confirms
the counter's own state is untouched.

`TestUnboundedRecordGrowsWithoutBound` is the module's core contrast.
`unboundedRecord` is unexported and unreachable from the package API; it is
the counter with no cap at all, run against the same five hundred
label combinations -- each with a distinct `Tenant`, standing in for a
leaked user ID -- that feed `BoundedCounter` in the same test. The plain map
ends up with exactly one key per event, five hundred of them;
`BoundedCounter.Series()` stays pinned at the configured cap regardless.
`TestBoundedCounterIsSafeForConcurrentUse` then drives `Record` from fifty
goroutines and checks the series count never exceeds the cap and every
event is still accounted for, either as a real series or in the overflow
bucket.

Create `labelcounter_test.go`:

```go
package labelcounter

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestNewRejectsNonPositiveMaxSeries(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -50} {
		if _, err := New(n); !errors.Is(err, ErrInvalidMaxSeries) {
			t.Errorf("New(%d) error = %v, want ErrInvalidMaxSeries", n, err)
		}
	}
}

func TestRecordCountsWithinCap(t *testing.T) {
	t.Parallel()

	c, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	a := Labels{Route: "/checkout", Status: "200", Tenant: "acme"}
	b := Labels{Route: "/checkout", Status: "500", Tenant: "acme"}
	for range 3 {
		c.Record(a)
	}
	c.Record(b)

	snap := c.Snapshot()
	if snap[a] != 3 {
		t.Errorf("snap[a] = %d, want 3", snap[a])
	}
	if snap[b] != 1 {
		t.Errorf("snap[b] = %d, want 1", snap[b])
	}
	if c.Series() != 2 {
		t.Errorf("Series() = %d, want 2", c.Series())
	}
	if c.Overflow() != 0 {
		t.Errorf("Overflow() = %d, want 0 (cap not reached)", c.Overflow())
	}
}

func TestRecordCollapsesIntoOverflowPastCap(t *testing.T) {
	t.Parallel()

	c, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := Labels{Route: "/a", Tenant: "t1"}
	second := Labels{Route: "/a", Tenant: "t2"}
	third := Labels{Route: "/a", Tenant: "t3"} // arrives after the cap is full
	fourth := Labels{Route: "/a", Tenant: "t4"}

	c.Record(first)
	c.Record(second)
	if c.Series() != 2 {
		t.Fatalf("Series() = %d, want 2 before any overflow", c.Series())
	}

	c.Record(third)
	c.Record(fourth)
	c.Record(third) // a label that already overflowed still just adds to the bucket

	if c.Series() != 2 {
		t.Fatalf("Series() = %d, want 2: the cap must not grow past maxSeries", c.Series())
	}
	if c.Overflow() != 3 {
		t.Fatalf("Overflow() = %d, want 3 (third, fourth, third again)", c.Overflow())
	}

	snap := c.Snapshot()
	if len(snap) != 3 { // first, second, and the overflow bucket
		t.Fatalf("len(snap) = %d, want 3 (2 real series + 1 overflow bucket)", len(snap))
	}
	if snap[first] != 1 || snap[second] != 1 {
		t.Fatalf("snap = %+v, want first and second each 1", snap)
	}

	// A label already tracked before the cap filled keeps incrementing
	// normally even after the cap is in effect for new keys.
	c.Record(first)
	if got := c.Snapshot()[first]; got != 2 {
		t.Fatalf("snap[first] = %d, want 2: existing series must not be capped", got)
	}
}

func TestSnapshotDoesNotAliasInternalMap(t *testing.T) {
	t.Parallel()

	c, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Record(Labels{Route: "/x"})

	snap := c.Snapshot()
	snap[Labels{Route: "/x"}] = 999
	snap[Labels{Route: "/injected"}] = 1

	fresh := c.Snapshot()
	if fresh[Labels{Route: "/x"}] != 1 {
		t.Fatalf("mutating a snapshot changed the counter's internal state")
	}
	if _, ok := fresh[Labels{Route: "/injected"}]; ok {
		t.Fatal("mutating a snapshot injected a key into the counter")
	}
}

// unboundedRecord is the counter almost everyone writes first: increment the
// label key with no cap at all. It is never exported and never reached from
// the package API; it exists so the test below can pin the failure mode
// BoundedCounter exists to prevent.
func unboundedRecord(counts map[Labels]int64, labels Labels) {
	counts[labels]++
}

// TestUnboundedRecordGrowsWithoutBound is the whole point of the module. A
// single high-cardinality field -- here Tenant standing in for a raw user ID
// or URL slug that leaked into a label -- makes the unbounded map grow one
// entry per event, while BoundedCounter, given the identical event stream,
// holds its distinct-key count at exactly maxSeries and routes the rest into
// one overflow bucket.
func TestUnboundedRecordGrowsWithoutBound(t *testing.T) {
	t.Parallel()

	const events = 500
	const maxSeries = 10

	unbounded := make(map[Labels]int64)
	bounded, err := New(maxSeries)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := range events {
		labels := Labels{Route: "/checkout", Status: "200", Tenant: "user-" + strconv.Itoa(i)}
		unboundedRecord(unbounded, labels)
		bounded.Record(labels)
	}

	if len(unbounded) != events {
		t.Fatalf("unbounded map holds %d keys, want %d: one per distinct tenant", len(unbounded), events)
	}
	if bounded.Series() != maxSeries {
		t.Fatalf("BoundedCounter.Series() = %d, want %d", bounded.Series(), maxSeries)
	}
	if want := int64(events - maxSeries); bounded.Overflow() != want {
		t.Fatalf("BoundedCounter.Overflow() = %d, want %d", bounded.Overflow(), want)
	}
	if !(bounded.Series() < len(unbounded)) {
		t.Fatalf("bounded series count %d must stay below the unbounded map's %d keys", bounded.Series(), len(unbounded))
	}
}

func TestBoundedCounterIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	c, err := New(20)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for g := range 50 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			labels := Labels{Route: "/a", Tenant: "shared"}
			if g%2 == 0 {
				labels = Labels{Route: "/a", Tenant: strconv.Itoa(g)}
			}
			for range 10 {
				c.Record(labels)
			}
		}(g)
	}
	wg.Wait()

	if c.Series() > 20 {
		t.Fatalf("Series() = %d, want <= 20 under concurrent load", c.Series())
	}
	snap := c.Snapshot()
	var total int64
	for _, n := range snap {
		total += n
	}
	if total != 500 { // 50 goroutines * 10 records each
		t.Fatalf("total recorded = %d, want 500", total)
	}
}

// ExampleBoundedCounter_Record is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment
// below.
func ExampleBoundedCounter_Record() {
	c, err := New(2)
	if err != nil {
		panic(err)
	}

	c.Record(Labels{Route: "/checkout", Tenant: "acme"})
	c.Record(Labels{Route: "/checkout", Tenant: "acme"})
	c.Record(Labels{Route: "/checkout", Tenant: "globex"})
	c.Record(Labels{Route: "/checkout", Tenant: "attacker-leaked-id"})

	fmt.Println("distinct real series:", c.Series())
	fmt.Println("overflow count:", c.Overflow())

	// Output:
	// distinct real series: 2
	// overflow count: 1
}
```

## Review

`BoundedCounter` is correct when `Series()` never exceeds `maxSeries`
regardless of how many distinct label combinations arrive, and when every
already-tracked combination keeps incrementing normally even after the cap
takes effect for new ones -- the comma-ok branch in `Record` is what keeps
those two rules from interfering with each other. The failure this design
prevents is structural rather than a coding mistake: a plain
`counts[key]++` is correct Go and still has no size bound, so the moment any
component of the key carries attacker- or user-influenced input, the map's
growth is bounded by their behavior instead of your product's shape.
Collapsing overflow into one `Labels{Other: true}` bucket keeps that shape
observable -- you still see "how much cardinality did we suppress" via
`Overflow()` -- without ever letting the underlying map grow past
`maxSeries` real entries. `Snapshot` returns an independent copy so the
exposition path can read and even mutate a copy freely, and `BoundedCounter`
is safe to share across every request-handling goroutine. Run
`go test -count=1 -race ./...` to confirm the boundary, the aliasing
contract, the unbounded-growth contrast, and the concurrency test.

## Resources

- [Prometheus: cardinality](https://prometheus.io/docs/practices/naming/#labels) — why unbounded label values are the canonical cause of a metrics-backend cardinality explosion.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the independent copy `Snapshot` returns.
- [Comma-ok idiom](https://go.dev/ref/spec#Index_expressions) — the map-index form that distinguishes absence from a zero value.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the default concurrency primitive for a shared read/write map, used here over the counter's internal state.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-lazy-pattern-compile-cache.md](16-lazy-pattern-compile-cache.md) | Next: [18-blocklist-membership-scanner.md](18-blocklist-membership-scanner.md)
