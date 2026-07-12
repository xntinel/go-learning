# Exercise 5: Lock-Free Token Bucket Rate Limiter

An API gateway calls `Allow()` on every request for every client; the limiter's
fast path is one of the hottest shared-state touches in the process. This
exercise builds a token bucket whose entire state lives behind one
`atomic.Pointer` to an immutable snapshot, updated with the read-compute-CAS
pattern — the same loop as the Treiber stack, applied to real business state.

## What you'll build

```text
tokenbucket/                     independent module: example.com/tokenbucket
  go.mod
  limiter.go                     state{micro, last}; Limiter: New, NewWithClock, Allow, Tokens
  limiter_test.go                deterministic refill via injected clock; over-admission
                                 storm test; Example
  cmd/
    demo/
      main.go                    burst of 4 against capacity 3
```

- Files: `limiter.go`, `limiter_test.go`, `cmd/demo/main.go`.
- Implement: `Limiter` with capacity and refill rate in micro-tokens, state behind `atomic.Pointer[state]`, `Allow` as a CAS loop with a read-only deny path, clock injected as a `func() time.Time`.
- Test: burst-of-capacity allowed and capacity+1 denied; refill after a clock advance; partial refill denied; G goroutines with a frozen clock never over-admit.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/11-lock-free-data-structures/05-cas-token-bucket-limiter/cmd/demo
cd go-solutions/15-sync-primitives/11-lock-free-data-structures/05-cas-token-bucket-limiter
```

### The state is a value; the pointer is the variable

A token bucket has two pieces of state that must change *together*: the token
count and the timestamp of the last refill. Two separate atomics cannot do that
— a goroutine could observe a new count with an old timestamp and refill twice.
A mutex can, and that version ships first. The lock-free version keeps both
fields in one immutable struct and makes the `atomic.Pointer[state]` the only
mutable thing in the type:

```go
for {
	old := l.st.Load()          // read: one snapshot, internally consistent
	next := refillAndSpend(old) // compute: privately, off to the side
	if l.st.CompareAndSwap(old, next) {
		return true             // publish: all-or-nothing
	}
}
```

Immutability after publication is the load-bearing rule. `Allow` never writes
through `old` — it builds a fresh `state` and swaps the pointer. Any goroutine
that loaded the old pointer keeps reading a frozen, consistent snapshot. This is
also what makes ABA harmless here: even if two states carry identical values,
they are distinct allocations, and Go's GC guarantees `old`'s address cannot be
recycled while the loop still references it.

Tokens are stored in *micro-tokens* (millionths) so refill math is integer:
`elapsed * rate / time.Second` with nanosecond `elapsed` never loses sub-token
progress to truncation the way whole-token math would at high call rates. Two
guards matter in that expression: a long idle period would overflow
`elapsed * rate`, so any elapsed time that would fill the bucket on its own
short-circuits to "full"; and a clock that appears to go backwards (VM
migrations, NTP steps) clamps to zero elapsed rather than minting negative
tokens.

One more production-shaped decision: the *deny* path does not CAS. Refill is a
pure function of `(old, now)`, so a denied request can simply return false —
no retry loop, no write traffic from clients that are already over their limit.
That matters: a limiter that CASes on denial lets a rejected flood keep the
cache line hot and slow down the requests you *are* admitting.

The clock is a `func() time.Time` field injected at construction.
`time.Now` in production; a fake in tests. Determinism in the tests below comes
entirely from this seam.

Create `limiter.go`:

```go
package tokenbucket

import (
	"sync/atomic"
	"time"
)

// microToken is the internal resolution: one token = 1e6 micro-tokens,
// so refill math stays in integers without losing sub-token progress.
const microToken = 1_000_000

// state is an immutable snapshot of the bucket. Never mutated after
// publication; Allow replaces the whole struct via CAS.
type state struct {
	micro int64     // available micro-tokens
	last  time.Time // instant the snapshot was computed
}

// Limiter is a lock-free token bucket. Allow is safe for concurrent
// use; the deny path is read-only.
type Limiter struct {
	capacity int64 // micro-tokens
	rate     int64 // micro-tokens per second
	now      func() time.Time
	st       atomic.Pointer[state]
}

// New returns a bucket holding capacity tokens, refilled at perSecond
// tokens per second, starting full.
func New(capacity, perSecond int) *Limiter {
	return NewWithClock(capacity, perSecond, time.Now)
}

// NewWithClock is New with an injected clock, the seam tests use to
// make refill deterministic.
func NewWithClock(capacity, perSecond int, now func() time.Time) *Limiter {
	l := &Limiter{
		capacity: int64(capacity) * microToken,
		rate:     int64(perSecond) * microToken,
		now:      now,
	}
	l.st.Store(&state{micro: l.capacity, last: now()})
	return l
}

// refilled returns the micro-token balance of old at instant now,
// capped at capacity and safe against overflow and backward clocks.
func (l *Limiter) refilled(old *state, now time.Time) int64 {
	elapsed := now.Sub(old.last)
	if elapsed <= 0 {
		return old.micro // clock frozen or stepped back: no refill
	}
	// If the idle time alone would fill the bucket, skip the
	// multiplication that could overflow int64.
	if fill := (l.capacity - old.micro) / l.rate; elapsed/time.Second > time.Duration(fill) {
		return l.capacity
	}
	micro := old.micro + int64(elapsed)*l.rate/int64(time.Second)
	if micro > l.capacity {
		micro = l.capacity
	}
	return micro
}

// Allow spends one token if available. The grant path is a CAS loop;
// the deny path performs no writes at all.
func (l *Limiter) Allow() bool {
	for {
		old := l.st.Load()
		now := l.now()
		micro := l.refilled(old, now)
		if micro < microToken {
			return false
		}
		if !now.After(old.last) {
			now = old.last
		}
		next := &state{micro: micro - microToken, last: now}
		if l.st.CompareAndSwap(old, next) {
			return true
		}
	}
}

// Tokens reports the whole tokens currently available (monitoring).
func (l *Limiter) Tokens() int64 {
	st := l.st.Load()
	return l.refilled(st, l.now()) / microToken
}
```

### Tests

The fake clock is an atomic nanosecond offset from a fixed base, so concurrent
`Allow` calls can read it while the test advances it. The deterministic cases
pin the business contract: a full burst of `capacity` is admitted, request
`capacity+1` is denied, a 2-second advance at 1 token/s admits exactly 2 more, a
500 ms advance admits none. `TestConcurrentNeverOverAdmit` is the invariant
test: with the clock frozen, 32 goroutines racing `Allow` must be granted
exactly `capacity` tokens in total — if two goroutines could both spend the last
token, the CAS (comparing the snapshot pointer, not the values) would have to
admit both, and the count catches it.

Create `limiter_test.go`:

```go
package tokenbucket

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic clock: a base instant plus an atomic
// offset, safe to advance while other goroutines read it.
type fakeClock struct {
	base   time.Time
	offset atomic.Int64 // nanoseconds
}

func newFakeClock() *fakeClock {
	return &fakeClock{base: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	return c.base.Add(time.Duration(c.offset.Load()))
}

func (c *fakeClock) Advance(d time.Duration) {
	c.offset.Add(int64(d))
}

func TestBurstThenDeny(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewWithClock(5, 1, clk.Now)

	for i := range 5 {
		if !l.Allow() {
			t.Fatalf("request %d denied within burst capacity", i+1)
		}
	}
	if l.Allow() {
		t.Fatal("request 6 allowed with an empty bucket and frozen clock")
	}
}

func TestRefillAfterAdvance(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewWithClock(5, 1, clk.Now)
	for range 5 {
		l.Allow()
	}

	clk.Advance(2 * time.Second)
	for i := range 2 {
		if !l.Allow() {
			t.Fatalf("refilled request %d denied after 2s at 1 token/s", i+1)
		}
	}
	if l.Allow() {
		t.Fatal("third request allowed after only 2 tokens refilled")
	}
}

func TestPartialRefillDenied(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewWithClock(1, 1, clk.Now)
	l.Allow()

	clk.Advance(500 * time.Millisecond)
	if l.Allow() {
		t.Fatal("allowed with only half a token refilled")
	}
	clk.Advance(500 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("denied after a full token refilled")
	}
}

func TestBackwardClockMintsNothing(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewWithClock(1, 1000, clk.Now)
	l.Allow()

	clk.Advance(-time.Hour)
	if l.Allow() {
		t.Fatal("backward clock step minted tokens")
	}
}

func TestLongIdleCapsAtCapacity(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewWithClock(3, 1000, clk.Now)
	for range 3 {
		l.Allow()
	}

	clk.Advance(1000 * time.Hour) // would overflow naive elapsed*rate
	if got := l.Tokens(); got != 3 {
		t.Fatalf("Tokens after long idle = %d, want capacity 3", got)
	}
	granted := 0
	for range 10 {
		if l.Allow() {
			granted++
		}
	}
	if granted != 3 {
		t.Fatalf("granted after long idle = %d, want 3", granted)
	}
}

func TestConcurrentNeverOverAdmit(t *testing.T) {
	t.Parallel()

	const capacity = 100
	const goroutines = 32
	const attempts = 100

	clk := newFakeClock()
	l := NewWithClock(capacity, 1, clk.Now)

	var granted atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range attempts {
				if l.Allow() {
					granted.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := granted.Load(); got != capacity {
		t.Fatalf("granted = %d, want exactly %d (frozen clock)", got, capacity)
	}
}

func ExampleLimiter_Allow() {
	clk := newFakeClock()
	l := NewWithClock(2, 1, clk.Now)
	fmt.Println(l.Allow(), l.Allow(), l.Allow())
	// Output: true true false
}
```

### The demo

Four back-to-back requests against a capacity-3 bucket refilled at 1 token/s:
the burst is admitted, the fourth is limited. (The microseconds between calls
refill on the order of a millionth of a token, so the output is stable.)

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tokenbucket"
)

func main() {
	l := tokenbucket.New(3, 1)
	for i := 1; i <= 4; i++ {
		if l.Allow() {
			fmt.Printf("request %d: allowed\n", i)
		} else {
			fmt.Printf("request %d: rate limited\n", i)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1: allowed
request 2: allowed
request 3: allowed
request 4: rate limited
```

## Review

The invariant that makes this limiter correct is that spending and refilling are
one atomic publication: a snapshot is internally consistent, the successor is
built privately, and the CAS admits exactly one spender per snapshot — which is
why the frozen-clock storm grants exactly `capacity`. The mistakes to look for
in review: mutating `old.micro` in place (races with every concurrent reader;
the state must be immutable), retrying the CAS without re-loading (livelock),
whole-token integer math (starves refill at high call rates), and doing the
refill multiplication before checking whether idle time alone fills the bucket
(int64 overflow after long idle — `TestLongIdleCapsAtCapacity` exists for that).
Also respect what the deny path is: read-only by design, so rejected floods do
not create write contention. In real deployments you would hold one `Limiter`
per client in a map guarded elsewhere; this exercise optimizes the per-client
fast path.

## Resources

- [Token bucket](https://en.wikipedia.org/wiki/Token_bucket) — the algorithm and its burst-plus-rate semantics.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — Load/CompareAndSwap on a typed pointer.
- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the production limiter (mutex-based) to benchmark against before shipping your own.
- [time package](https://pkg.go.dev/time) — `Time.Sub`, `Duration`, and the monotonic clock reading that makes `Sub` safe here.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-sharded-request-counters.md](04-sharded-request-counters.md) | Next: [06-atomic-circuit-breaker.md](06-atomic-circuit-breaker.md)
