# Exercise 21: Time-Windowed Metrics Aggregation with Ring Buffer

**Nivel: Intermedio** — validacion rapida (un test corto).

A dashboard that shows "requests in the last 4 minutes" cannot keep every
individual event around — it only needs the *count* in each fixed-width time
bucket (one bucket per minute, say), and buckets older than the window
should disappear on their own as time moves forward. This module builds that
ring buffer of buckets: eviction is a condition-only loop bounded by how many
buckets are actually live, and aggregation walks exactly that live portion.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
metrics/                       module example.com/metrics
  go.mod                       go 1.24
  metrics.go                   Bucket; RingMetrics; New/Record/Total; evict
  metrics_test.go                same-bucket aggregation, new bucket, partial/full eviction, empty ring, capacity rollover
  cmd/demo/
    main.go                     five records across a 4-second window, aggregated at three points in time
```

- Files: `metrics.go`, `metrics_test.go`, `cmd/demo/main.go`.
- Implement: `RingMetrics` backed by `buckets []Bucket`, `head`, `size`, `capacity`, `bucketMs`; `Record(nowMs, n int64)` and `Total(nowMs int64) int64`, both driven by a private `evict` condition-only loop `for m.size > 0 && nowMs-m.buckets[m.head].StartMs >= windowMs { ... }`.
- Test: two records landing in the same bucket aggregate; a record past the bucket boundary starts a new one; a long-idle `Total` evicts everything; a full-window `Total` sees everything; the ring rolling over at capacity.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why eviction is bounded by `size`, not by wall-clock time

`evict` looks almost identical to the sliding-window rate counter's eviction
loop from Exercise 12, and that similarity is the point: whether the ring
holds raw timestamps or per-bucket counts, "drop everything at the front that
has aged out of the window" is the same condition-only shape, because it has
no natural iteration count of its own — it depends entirely on how stale the
oldest entry is. What is different here is `Total`'s aggregation step: after
`evict` runs, the loop that sums counts is bounded by `size`, the number of
buckets *actually live right now*, walking `(head+i) % capacity` for
`i` from `0` to `size-1`. That is the ring buffer's version of `for range` —
Go cannot range directly over a slice with wraparound, so the loop expresses
the same guarantee by hand: touch exactly the live elements, in age order,
and never the stale or unused slots sitting in the rest of the backing array.

`Record` evicts first, then either adds to the existing tail bucket (if
`nowMs` still falls in the same bucket window) or opens a new one. The
capacity check right before opening a new bucket (`if m.size == m.capacity`)
is a defensive backstop, not the primary eviction mechanism: under the normal
assumption that `Record` is called with non-decreasing `nowMs`, `evict` will
always have already made room by the time a genuinely new bucket is needed,
because a full ring's oldest bucket is by construction exactly `capacity *
bucketMs` old the instant a new, distinct bucket is due. The backstop exists
so the ring can never silently grow past `capacity` even if that assumption
is ever violated.

Create `metrics.go`:

```go
package metrics

// Bucket is one fixed-width time window's running count.
type Bucket struct {
	StartMs int64
	Count   int64
}

// RingMetrics aggregates counts into fixed-width time buckets (for example,
// one bucket per minute) kept in a ring buffer, so old buckets are evicted
// automatically instead of the window growing without bound.
type RingMetrics struct {
	buckets  []Bucket
	head     int
	size     int
	capacity int
	bucketMs int64
}

// New builds a RingMetrics holding at most capacity buckets, each spanning
// bucketMs milliseconds.
func New(capacity int, bucketMs int64) *RingMetrics {
	return &RingMetrics{
		buckets:  make([]Bucket, capacity),
		capacity: capacity,
		bucketMs: bucketMs,
	}
}

// Record evicts buckets that have aged out of the ring's total span, then
// adds n to the bucket covering nowMs -- starting a new bucket if the tail
// bucket does not cover nowMs yet.
func (m *RingMetrics) Record(nowMs int64, n int64) {
	m.evict(nowMs)

	bucketStart := nowMs - (nowMs % m.bucketMs)
	if m.size > 0 {
		tail := (m.head + m.size - 1) % m.capacity
		if m.buckets[tail].StartMs == bucketStart {
			m.buckets[tail].Count += n
			return
		}
	}

	if m.size == m.capacity {
		// Ring is full of buckets that are all still within the window; the
		// oldest one must roll off to make room for the new one.
		m.head = (m.head + 1) % m.capacity
		m.size--
	}
	next := (m.head + m.size) % m.capacity
	m.buckets[next] = Bucket{StartMs: bucketStart, Count: n}
	m.size++
}

// Total evicts aged-out buckets and sums the counts of everything still live.
// The aggregation is bounded by size -- the number of buckets actually in the
// window right now -- which is the ring's analogue of ranging over a slice:
// it walks exactly the live portion, never the whole backing array.
func (m *RingMetrics) Total(nowMs int64) int64 {
	m.evict(nowMs)

	var total int64
	for i := 0; i < m.size; i++ {
		idx := (m.head + i) % m.capacity
		total += m.buckets[idx].Count
	}
	return total
}

// evict walks the ring from its oldest bucket, dropping every bucket whose
// entire span is older than the ring's total window (capacity * bucketMs).
// This is a condition-only loop: it has no counter of its own, it runs
// exactly as long as the oldest bucket is stale, and it terminates because
// every iteration strictly shrinks size.
func (m *RingMetrics) evict(nowMs int64) {
	windowMs := int64(m.capacity) * m.bucketMs
	for m.size > 0 && nowMs-m.buckets[m.head].StartMs >= windowMs {
		m.head = (m.head + 1) % m.capacity
		m.size--
	}
}
```

### The runnable demo

The demo uses a 4-bucket, 1-second-per-bucket ring (a 4-second window) and
records five events, then reads the total at three points in time: while
everything is still in the window, after enough time has passed that the
oldest bucket has aged out, and after a long idle period that empties the
ring entirely.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func main() {
	// 4 one-second buckets -> a 4-second rolling window.
	m := metrics.New(4, 1000)

	m.Record(0, 10)
	m.Record(500, 5)
	m.Record(1200, 7)
	m.Record(2300, 3)
	m.Record(3400, 8)

	fmt.Printf("total at 3400ms: %d\n", m.Total(3400))

	// Advance 3s: the [0,1000) bucket (10+5=15) ages out of the 4s window.
	fmt.Printf("total at 6400ms: %d\n", m.Total(6400))

	// Advance far past everything: window is empty.
	fmt.Printf("total at 20000ms: %d\n", m.Total(20_000))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
total at 3400ms: 33
total at 6400ms: 8
total at 20000ms: 0
```

### Tests

`TestRecordAggregatesWithinSameBucket` and `TestRecordStartsNewBucketAcrossBoundary`
cover `Record`'s two branches; `TestEvictDropsBucketsOlderThanWindow` is the
sharpest eviction check — it lands exactly on the window edge to confirm one
stale bucket is dropped while two fresher ones survive.
`TestEvictDropsEverythingWhenIdleLongEnough` and `TestEmptyRingTotalIsZero`
cover the two empty-result shapes, and `TestRingRollsOverAtCapacity` confirms
the ring never holds more than `capacity` buckets.

Create `metrics_test.go`:

```go
package metrics

import "testing"

func TestRecordAggregatesWithinSameBucket(t *testing.T) {
	t.Parallel()

	m := New(3, 1000)
	m.Record(0, 5)
	m.Record(500, 3) // same 1000ms bucket as t=0

	if got := m.Total(600); got != 8 {
		t.Fatalf("Total = %d, want 8", got)
	}
}

func TestRecordStartsNewBucketAcrossBoundary(t *testing.T) {
	t.Parallel()

	m := New(3, 1000)
	m.Record(0, 5)
	m.Record(1200, 2) // new bucket (starts at 1000)

	if got := m.Total(1200); got != 7 {
		t.Fatalf("Total = %d, want 7", got)
	}
}

func TestEvictDropsBucketsOlderThanWindow(t *testing.T) {
	t.Parallel()

	// capacity 3, bucketMs 1000 -> total window 3000ms.
	m := New(3, 1000)
	m.Record(0, 5)    // bucket [0, 1000)
	m.Record(1200, 2) // bucket [1000, 2000)
	m.Record(2200, 4) // bucket [2000, 3000)

	if got := m.Total(2200); got != 11 {
		t.Fatalf("Total before eviction = %d, want 11", got)
	}

	// Advance to 3200: the [0,1000) bucket is now exactly at the 3000ms
	// window edge and must be evicted; the other two remain.
	if got := m.Total(3200); got != 6 {
		t.Fatalf("Total after partial eviction = %d, want 6 (2 + 4)", got)
	}
}

func TestEvictDropsEverythingWhenIdleLongEnough(t *testing.T) {
	t.Parallel()

	m := New(2, 1000)
	m.Record(0, 5)
	m.Record(500, 3)

	if got := m.Total(10_000); got != 0 {
		t.Fatalf("Total after long idle period = %d, want 0", got)
	}
}

func TestEmptyRingTotalIsZero(t *testing.T) {
	t.Parallel()

	m := New(4, 1000)
	if got := m.Total(0); got != 0 {
		t.Fatalf("Total on empty ring = %d, want 0", got)
	}
}

func TestRingRollsOverAtCapacity(t *testing.T) {
	t.Parallel()

	// capacity 2, bucketMs 1000 -> window 2000ms. A third distinct bucket
	// must evict the oldest so the ring never holds more than 2 buckets.
	m := New(2, 1000)
	m.Record(0, 1)
	m.Record(1000, 1)
	m.Record(2000, 1)

	if got := m.Total(2000); got != 2 {
		t.Fatalf("Total = %d, want 2 (oldest bucket rolled off)", got)
	}
}
```

## Review

`RingMetrics` is correct when `Total` never includes a bucket whose entire
span is older than `capacity * bucketMs`, and never misses one that is still
within it. The common mistake this design avoids is aggregating with a plain
`for _, b := range m.buckets` over the whole backing array — that would sum
every unused zero-value slot as well as any stale bucket `evict` has not yet
physically overwritten, silently double-counting or including garbage.
Bounding the aggregation loop by `size` and indexing through `head` is what
keeps the sum limited to exactly the live buckets. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the condition-only form used by `evict`.
- [Prometheus: Histograms and summaries](https://prometheus.io/docs/practices/histograms/) — the bucketed-aggregation model this module's `Bucket` type mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-stream-frame-decoder-bounded.md](20-stream-frame-decoder-bounded.md) | Next: [22-two-phase-commit-coordinator.md](22-two-phase-commit-coordinator.md)
