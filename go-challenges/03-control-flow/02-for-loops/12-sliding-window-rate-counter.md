# Exercise 12: A Sliding-Window Rate Counter over a Ring Buffer

**Nivel: Intermedio** — validacion rapida (un test corto).

A token bucket answers "how many requests can I allow right now" with a
formula. A sliding-window counter answers a different question — "how many
events actually happened in the last N milliseconds" — by keeping every event
timestamp in a fixed-size ring buffer and evicting the ones that aged out.
This module builds that counter and proves its eviction loop never keeps a
stale entry around.

This module is fully self-contained: its own `go mod init` and one test file.

## What you'll build

```text
slidingwindow/               module example.com/slidingwindow
  go.mod                     go 1.24
  window.go                  ErrCapacityExceeded; Counter with Record/Count
  window_test.go             full/partial eviction, capacity, empty counter
```

- Files: `window.go`, `window_test.go`.
- Implement: a `Counter` backed by `buf []int64`, `head`, `size`, `capacity`; `Record(now, windowMs int64) error` and `Count(now, windowMs int64) int`, both driven by a private `evict` condition-only loop `for c.size > 0 && c.buf[c.head] <= cutoff { ... }` that advances `head` around the ring and decrements `size`.
- Test: fill to capacity and confirm `ErrCapacityExceeded`; advance `now` so a narrow window evicts everything and confirm the count drops to zero and slots free up; a partial eviction that frees exactly two of four slots; an empty counter reporting zero.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/12-sliding-window-rate-counter
cd go-solutions/03-control-flow/02-for-loops/12-sliding-window-rate-counter
go mod edit -go=1.24
```

### Why eviction is a condition-only loop, not a counted one

The ring buffer's oldest entry always sits at `head`. Eviction has no natural
"how many times" — it depends entirely on how stale the data is, so the loop
condition is the predicate itself: keep dropping the oldest entry as long as
one exists (`size > 0`) and it is older than the cutoff
(`buf[head] <= now-windowMs`). This is the same *provable termination* shape
as a readiness poll, except here the predicate is guaranteed to become false
within `size` iterations — the loop cannot run away, because every iteration
strictly shrinks `size`.

`Record` runs `evict` first, then checks whether a free slot exists: if
`size == capacity` even after evicting everything that could be evicted, the
buffer is genuinely saturated and `ErrCapacityExceeded` is the honest answer —
not "wait," since this counter has no notion of blocking. `Count` runs the
same eviction and simply reports the resulting `size`, so a caller can poll
the current rate without recording a new event.

Create `window.go`:

```go
package slidingwindow

import "errors"

// ErrCapacityExceeded means the ring buffer has no free slot for a new event
// even after evicting everything older than the window.
var ErrCapacityExceeded = errors.New("slidingwindow: capacity exceeded")

// Counter tracks event timestamps (in milliseconds) in a fixed-size ring
// buffer and reports how many fall within a trailing window ending at "now".
// It is the classic sliding-window rate counter: unlike a token bucket, which
// derives a count from an elapsed-time formula, this counter stores every
// event and evicts the ones that have aged out of the window.
type Counter struct {
	buf      []int64
	head     int
	size     int
	capacity int
}

// New builds a Counter backed by a ring buffer of the given capacity.
func New(capacity int) *Counter {
	return &Counter{buf: make([]int64, capacity), capacity: capacity}
}

// Record evicts events older than windowMs relative to now, then tries to
// record a new event at time now. If the buffer is still full after
// eviction, it returns ErrCapacityExceeded and does not record the event.
func (c *Counter) Record(now, windowMs int64) error {
	c.evict(now, windowMs)
	if c.size == c.capacity {
		return ErrCapacityExceeded
	}
	tail := (c.head + c.size) % c.capacity
	c.buf[tail] = now
	c.size++
	return nil
}

// Count evicts events older than windowMs relative to now and returns how
// many events remain in the trailing window.
func (c *Counter) Count(now, windowMs int64) int {
	c.evict(now, windowMs)
	return c.size
}

// evict walks the ring buffer from its oldest entry, dropping every event at
// or before the cutoff. It is a condition-only loop: it has no counter of its
// own, it runs exactly as long as the oldest entry is stale.
func (c *Counter) evict(now, windowMs int64) {
	cutoff := now - windowMs
	for c.size > 0 && c.buf[c.head] <= cutoff {
		c.head = (c.head + 1) % c.capacity
		c.size--
	}
}
```

### Tests

`TestCounterRecordAndEvict` fills a capacity-3 counter, confirms a fourth
`Record` is rejected while the window still covers every entry, then advances
`now` far enough that a 100ms window evicts all three and confirms both
`Count` and a fresh `Record` reflect the freed capacity.
`TestCounterPartialEviction` is the more realistic case: only the two oldest
of four entries fall outside the window, so exactly two slots free up — not
zero, not all four. `TestCounterEmptyIsZero` covers the base case.

Create `window_test.go`:

```go
package slidingwindow

import (
	"errors"
	"testing"
)

func TestCounterRecordAndEvict(t *testing.T) {
	t.Parallel()

	c := New(3)

	for _, ts := range []int64{0, 10, 20} {
		if err := c.Record(ts, 1000); err != nil {
			t.Fatalf("Record(%d) = %v, want nil", ts, err)
		}
	}

	// Buffer is full and none of the events are outside a 1000ms window yet.
	if err := c.Record(25, 1000); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Record(25) = %v, want ErrCapacityExceeded", err)
	}

	// Advance now far enough that a 100ms window evicts every prior event.
	if got := c.Count(500, 100); got != 0 {
		t.Fatalf("Count after eviction = %d, want 0", got)
	}

	// The buffer has free slots again after eviction.
	if err := c.Record(500, 100); err != nil {
		t.Fatalf("Record(500) after eviction = %v, want nil", err)
	}
	if got := c.Count(500, 100); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
}

func TestCounterPartialEviction(t *testing.T) {
	t.Parallel()

	c := New(4)
	for _, ts := range []int64{0, 100, 200, 300} {
		if err := c.Record(ts, 10_000); err != nil {
			t.Fatalf("Record(%d) = %v, want nil", ts, err)
		}
	}

	// A window of 150ms at now=300 keeps only events after cutoff=150: 200, 300.
	if got := c.Count(300, 150); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}

	// The two oldest slots are now free; two more events should fit.
	if err := c.Record(310, 150); err != nil {
		t.Fatalf("Record(310) = %v, want nil", err)
	}
	if err := c.Record(320, 150); err != nil {
		t.Fatalf("Record(320) = %v, want nil", err)
	}
	if err := c.Record(330, 150); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Record(330) = %v, want ErrCapacityExceeded", err)
	}
}

func TestCounterEmptyIsZero(t *testing.T) {
	t.Parallel()

	c := New(2)
	if got := c.Count(1000, 500); got != 0 {
		t.Fatalf("Count on empty counter = %d, want 0", got)
	}
}
```

## Review

The counter is correct when the ring buffer never holds an entry older than
the window after `evict` runs, and `evict`'s condition-only loop is what
guarantees that: it keeps dropping the oldest entry until either the buffer
is empty or the oldest entry is fresh enough, and it terminates because
`size` strictly decreases each pass. `TestCounterPartialEviction` is the
sharpest proof — it shows eviction stops exactly at the boundary between
stale and fresh entries, not before and not past it. Run
`go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the condition-only form used by `evict`.
- [Effective Go: Data](https://go.dev/doc/effective_go#data) — slices as the backing store for a ring buffer.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-paginated-drain-safety-cap.md](11-paginated-drain-safety-cap.md) | Next: [13-backoff-schedule-plateau.md](13-backoff-schedule-plateau.md)
