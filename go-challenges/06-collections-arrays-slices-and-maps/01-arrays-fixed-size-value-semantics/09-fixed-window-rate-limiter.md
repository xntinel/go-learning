# Exercise 9: A Fixed-Window Rate Limiter with a [60]int Circular Second-Bucket Array

A per-client rate limiter that counts requests in one-second buckets over a
sliding minute is a classic fixed-size-array use. A `[60]int` holds one counter per
second; the array IS the entire state — no allocation, cache-resident, and cheap to
copy for a lock-free snapshot. This exercise builds that limiter with modular
indexing and a deterministic clock.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod
  limiter.go                 Limiter{buckets [60]int, ...}; Allow(now); Count(now); Snapshot() [60]int
  cmd/
    demo/
      main.go                runnable demo: burst to the limit, deny, advance, reset
  limiter_test.go            over-limit denied, window reset, snapshot independent, second-59-to-0 wrap
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a `Limiter` holding `buckets [60]int` and a limit, with `Allow(now time.Time) bool` that zeros stale slots and counts within the current minute, plus `Snapshot() [60]int`.
- Test: N requests fill the window then the (N+1)th is denied; advancing past the window resets; `Snapshot` is an independent copy; wraparound at second 59 to 0.
- Verify: `go test -count=1 -race ./...`

### Why the [60]int array is the whole state

The limiter caps requests over a trailing 60-second window by keeping one counter
per second: `buckets[s]` is the number of requests seen in the second whose value
mod 60 is `s`. A `[60]int` is exactly the right shape — the size is fixed and known,
so the array lives inline in the struct with no heap allocation, and the whole
limiter state is cache-resident. Summing the sixty counters gives the current
window total.

The subtlety is *staleness*. Second `s` in the array is reused every 60 seconds, so
before you trust `buckets[s]` you must know whether it belongs to the current minute
or a previous one. The limiter tracks the last epoch-second it saw (`lastSec`). On
each `Allow(now)`, it computes `sec := now.Unix()` and, for every second that has
elapsed since `lastSec` (capped at 60, since more than a full window means every
bucket is stale), it zeros the bucket for that second — reclaiming the slot before
reuse. Then it increments `buckets[sec%60]`, sums the window, and allows the request
only if the total is within the limit. Modular indexing (`sec%60`) is what makes the
fixed array behave as a circular buffer of seconds.

`Snapshot() [60]int` returns the buckets array *by value*. Because arrays are copied
on return, the caller gets an independent `[60]int` that the limiter's ongoing
mutations cannot touch — a lock-free, immutable picture of the state at that instant.
This is the "value copy as immutability" idea made concrete: a metrics endpoint can
grab a snapshot and read it at leisure while the limiter keeps serving.

The clock is passed in as `now time.Time` rather than read from `time.Now()`, so the
tests are fully deterministic without any clock abstraction machinery — they just
pass explicit instants and assert exact boundary behavior.

Create `limiter.go`:

```go
package ratelimit

import (
	"time"
)

// WindowSize is the number of one-second buckets in the trailing window.
const WindowSize = 60

// Limiter is a fixed-window rate limiter. Its entire state is a [60]int of
// per-second counters plus the last second observed; nothing is heap-allocated.
type Limiter struct {
	buckets [WindowSize]int
	limit   int
	lastSec int64
	primed  bool
}

// New returns a Limiter allowing up to limit requests per trailing 60 seconds.
func New(limit int) *Limiter {
	return &Limiter{limit: limit}
}

// advance zeros every bucket for the seconds elapsed between lastSec and sec, so
// stale slots do not count toward the current window.
func (l *Limiter) advance(sec int64) {
	if !l.primed {
		l.lastSec = sec
		l.primed = true
		return
	}
	elapsed := sec - l.lastSec
	if elapsed <= 0 {
		return
	}
	if elapsed > WindowSize {
		elapsed = WindowSize
	}
	for s := l.lastSec + 1; s <= l.lastSec+elapsed; s++ {
		l.buckets[s%WindowSize] = 0
	}
	l.lastSec = sec
}

// Allow records a request at now and reports whether it is within the limit.
func (l *Limiter) Allow(now time.Time) bool {
	sec := now.Unix()
	l.advance(sec)
	l.buckets[sec%WindowSize]++
	return l.total() <= l.limit
}

// Count returns the current window total at now, after aging out stale buckets,
// without recording a request.
func (l *Limiter) Count(now time.Time) int {
	l.advance(now.Unix())
	return l.total()
}

func (l *Limiter) total() int {
	sum := 0
	for _, c := range l.buckets {
		sum += c
	}
	return sum
}

// Snapshot returns an independent copy of the per-second buckets. Because the
// array is returned by value, mutating the result does not affect the limiter.
func (l *Limiter) Snapshot() [WindowSize]int {
	return l.buckets
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	l := ratelimit.New(3)
	base := time.Unix(1_000_000, 0)

	for i := 1; i <= 4; i++ {
		ok := l.Allow(base)
		fmt.Printf("request %d at t=0s: allowed=%v\n", i, ok)
	}

	// Advance a full window: stale buckets age out, budget is restored.
	later := base.Add(61 * time.Second)
	fmt.Printf("after 61s, allowed=%v\n", l.Allow(later))

	// A snapshot is independent of ongoing mutation.
	snap := l.Snapshot()
	snap[0] = 999
	fmt.Printf("snapshot mutation isolated: %v\n", l.Snapshot()[0] != 999)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1 at t=0s: allowed=true
request 2 at t=0s: allowed=true
request 3 at t=0s: allowed=true
request 4 at t=0s: allowed=false
after 61s, allowed=true
snapshot mutation isolated: true
```

### Tests

`TestOverLimitDenied` fires the limit's worth of requests in one second and asserts
the next is denied. `TestWindowResets` fills the window, advances past 60 seconds,
and asserts the count is back to a single request. `TestSnapshotIndependent` mutates
a snapshot and asserts the limiter is unaffected. `TestSecond59Wraps` crosses the
second 59 to 0 boundary and asserts the modular indexing keeps counting correctly
rather than aliasing bucket 59 with bucket 0.

Create `limiter_test.go`:

```go
package ratelimit

import (
	"testing"
	"time"
)

func TestOverLimitDenied(t *testing.T) {
	t.Parallel()

	l := New(3)
	now := time.Unix(2_000_000, 0)
	for i := 0; i < 3; i++ {
		if !l.Allow(now) {
			t.Fatalf("request %d within limit should be allowed", i+1)
		}
	}
	if l.Allow(now) {
		t.Fatal("the 4th request over a limit of 3 must be denied")
	}
}

func TestWindowResets(t *testing.T) {
	t.Parallel()

	l := New(2)
	base := time.Unix(3_000_000, 0)
	l.Allow(base)
	l.Allow(base)
	if l.Allow(base) {
		t.Fatal("3rd request over limit 2 should be denied")
	}

	// Advance past the whole window; all stale buckets age out.
	later := base.Add(61 * time.Second)
	if !l.Allow(later) {
		t.Fatal("after the window elapses, requests should be allowed again")
	}
	if got := l.Count(later); got != 1 {
		t.Fatalf("window count after reset = %d, want 1", got)
	}
}

func TestSnapshotIndependent(t *testing.T) {
	t.Parallel()

	l := New(10)
	now := time.Unix(4_000_000, 0)
	l.Allow(now)

	snap := l.Snapshot()
	for i := range snap {
		snap[i] = 777
	}
	if l.Snapshot() == snap {
		t.Fatal("Snapshot must return an independent copy, not a shared array")
	}
}

func TestSecond59Wraps(t *testing.T) {
	t.Parallel()

	l := New(100)
	// A base whose Unix second mod 60 is 59, then step into the next minute.
	start := time.Unix(59, 0) // 59 % 60 == 59
	if start.Unix()%WindowSize != 59 {
		t.Fatalf("test setup wrong: %d %% 60 = %d", start.Unix(), start.Unix()%WindowSize)
	}
	l.Allow(start)                      // bucket 59
	l.Allow(start.Add(1 * time.Second)) // bucket 0 (60 % 60)
	l.Allow(start.Add(2 * time.Second)) // bucket 1

	if got := l.Count(start.Add(2 * time.Second)); got != 3 {
		t.Fatalf("count across the 59->0 wrap = %d, want 3", got)
	}
}
```

## Review

The limiter is correct when the trailing-window total counts exactly the requests
in the last 60 seconds: stale buckets are zeroed as time advances, so a request at
second `s` never sees a count left over from the previous minute's second `s`. The
`[60]int` array is the whole state — no allocation, and `Snapshot` returns it by
value for a lock-free independent copy, which `TestSnapshotIndependent` proves. The
modular indexing (`sec%60`) makes the fixed array a circular second-buffer;
`TestSecond59Wraps` confirms crossing the minute boundary keeps distinct counts
rather than aliasing. The design choice to pass `now` in keeps the tests
deterministic with no clock abstraction. Run `go test -race` to confirm the
over-limit denial, the window reset, and the wraparound.

## Resources

- [Go Specification: Array types](https://go.dev/ref/spec#Array_types) — fixed-length arrays as inline struct state.
- [time.Time.Unix](https://pkg.go.dev/time#Time.Unix) — the epoch-second used for bucket indexing.
- [Rate limiting algorithms (Cloudflare blog)](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — fixed and sliding window counters in practice.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-ip-allowlist-netip-fixed-array.md](08-ip-allowlist-netip-fixed-array.md) | Next: [10-binary-header-slice-to-array.md](10-binary-header-slice-to-array.md)
