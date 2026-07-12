# Exercise 16: Token Bucket Rate Limiter — State-Driven `iter.Seq[bool]` Controlling Per-Item Access

**Nivel: Intermedio** — validacion rapida (un test corto).

Every public API gateway needs to answer one question per incoming request:
allow it through, or reject it before it touches the backend. A token bucket
is the standard answer -- a pool of tokens that refills at a constant rate and
is spent one-per-request -- and expressing it as an `iter.Seq[bool]` makes the
limiter itself a pure transformation from a stream of request timestamps to a
stream of allow/deny decisions, with the bucket's state (current tokens, last
request time) living entirely in the iterator's closure. This exercise is an
independent module with its own `go mod init`.

## What you'll build

```text
ratelimit/                independent module: example.com/token-bucket-rate-limiter
  go.mod                   module example.com/token-bucket-rate-limiter
  ratelimit.go             Allow
  cmd/
    demo/
      main.go              runnable demo: a burst of requests against a small bucket
  ratelimit_test.go        burst-then-deny, refill-over-time, early-stop, invalid-args panic
```

Implement: `Allow(capacity int, refillEvery time.Duration, requests iter.Seq[time.Time]) iter.Seq[bool]` yielding one allow/deny decision per request timestamp; panics if `capacity < 1` or `refillEvery <= 0`.
Test: 4 simultaneous requests against a bucket of 3 yield `true,true,true,false`; a bucket of 1 exhausted then queried 1s later (with a 1s refill period) yields `true,false,true`; a consumer break after two decisions stops there; invalid capacity panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/16-token-bucket-rate-limiter/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/16-token-bucket-rate-limiter
go mod edit -go=1.24
```

The state that must survive across yields is exactly two numbers: the
current token count (a `float64`, not an `int`, because refill happens
continuously rather than in whole-token ticks) and the timestamp of the last
request. On every request, `Allow` first credits the bucket for however much
simulated time elapsed since the previous request -- `elapsed / refillEvery`
tokens -- capped at `capacity` so an idle bucket cannot accumulate an
unbounded backlog of favors. Only after crediting does it check whether at
least one token is available; if so it spends one and yields `true`,
otherwise it yields `false` without touching the count. Driving the clock off
the timestamps carried in `requests` rather than calling `time.Now()` inside
`Allow` is what makes the whole limiter replayable in a test: a table of
`time.Time` values built by hand exercises any refill schedule instantly.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"iter"
	"time"
)

// Allow returns an iter.Seq[bool] that yields one allow/deny decision per
// timestamp in requests, modeling a token bucket of the given capacity that
// refills at a constant rate of one token every refillEvery. The bucket
// state -- the current token count and the timestamp of the last request --
// lives in the iterator's closure and carries across yields, which is what
// lets each decision depend on how much simulated time elapsed since the
// previous request instead of on a real wall clock. capacity must be >= 1
// and refillEvery must be > 0.
func Allow(capacity int, refillEvery time.Duration, requests iter.Seq[time.Time]) iter.Seq[bool] {
	if capacity < 1 {
		panic("ratelimit: capacity must be >= 1")
	}
	if refillEvery <= 0 {
		panic("ratelimit: refillEvery must be > 0")
	}
	return func(yield func(bool) bool) {
		tokens := float64(capacity)
		var last time.Time
		first := true
		for t := range requests {
			if first {
				first = false
			} else if elapsed := t.Sub(last); elapsed > 0 {
				tokens += float64(elapsed) / float64(refillEvery)
				if tokens > float64(capacity) {
					tokens = float64(capacity)
				}
			}
			last = t

			allowed := tokens >= 1
			if allowed {
				tokens--
			}
			if !yield(allowed) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/token-bucket-rate-limiter"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	requests := []time.Time{
		base,
		base.Add(100 * time.Millisecond),
		base.Add(200 * time.Millisecond),
		base.Add(300 * time.Millisecond),
		base.Add(1100 * time.Millisecond),
	}

	seq := func(yield func(time.Time) bool) {
		for _, t := range requests {
			if !yield(t) {
				return
			}
		}
	}

	n := 0
	for allowed := range ratelimit.Allow(3, time.Second, seq) {
		fmt.Printf("request %d: allowed=%v\n", n, allowed)
		n++
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
request 0: allowed=true
request 1: allowed=true
request 2: allowed=true
request 3: allowed=false
request 4: allowed=true
```

Five requests hit a bucket of capacity 3: the first three drain it, the
fourth (100ms later, not enough time to refill even a tenth of a token to
matter) is denied, and the fifth arrives 800ms after the fourth -- enough
simulated time for 0.8 of a token to refill on top of the 0.3 left over,
crossing the 1-token threshold again.

### Tests

The table below is expressed as four separate test functions rather than one
data table, because each one probes a materially different part of the
contract -- bursting, refilling, early termination, and the panic guard --
and keeping them separate keeps each failure message unambiguous about which
behavior broke.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"testing"
	"time"
)

func timesFrom(base time.Time, offsets ...time.Duration) []time.Time {
	ts := make([]time.Time, len(offsets))
	for i, off := range offsets {
		ts[i] = base.Add(off)
	}
	return ts
}

func sliceValues(ts []time.Time) func(func(time.Time) bool) {
	return func(yield func(time.Time) bool) {
		for _, t := range ts {
			if !yield(t) {
				return
			}
		}
	}
}

func decisions(capacity int, refillEvery time.Duration, ts []time.Time) []bool {
	var got []bool
	for allowed := range Allow(capacity, refillEvery, sliceValues(ts)) {
		got = append(got, allowed)
	}
	return got
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAllowBurstThenDeny(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// capacity 3, refill every 1s: 4 requests at t=0 exhaust the bucket on
	// the 4th.
	ts := timesFrom(base, 0, 0, 0, 0)
	got := decisions(3, time.Second, ts)
	want := []bool{true, true, true, false}
	if !equalBools(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestAllowRefillsOverTime(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// capacity 1, refill every 1s: exhaust immediately, then a request 1s
	// later is allowed again because exactly one token refilled.
	ts := timesFrom(base, 0, 0, time.Second)
	got := decisions(1, time.Second, ts)
	want := []bool{true, false, true}
	if !equalBools(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestAllowStopsEarly(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := timesFrom(base, 0, 0, 0, 0, 0)
	var got []bool
	for allowed := range Allow(2, time.Second, sliceValues(ts)) {
		got = append(got, allowed)
		if len(got) == 2 {
			break
		}
	}
	want := []bool{true, true}
	if !equalBools(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestAllowPanicsOnInvalidCapacity(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for capacity < 1")
		}
	}()
	Allow(0, time.Second, sliceValues(nil))
}
```

## Review

The correctness of `Allow` hinges on crediting refill *before* checking
whether a token is available, and on capping the credited amount at
`capacity` -- otherwise a bucket left idle for a long time would accumulate an
unbounded backlog of allowed bursts instead of the intended constant refill
rate. The common mistake is refilling in whole-token ticks (an integer
counter incremented once per elapsed `refillEvery`), which under-refills for
any request that lands between ticks; carrying a `float64` token count
sidesteps that entirely and makes partial credit exact. Driving the clock
from the timestamps embedded in `requests` rather than `time.Now()` is what
keeps the whole limiter deterministic and instantly testable.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Token bucket algorithm — Wikipedia](https://en.wikipedia.org/wiki/Token_bucket)
- [Stripe: rate limiters](https://stripe.com/blog/rate-limiters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-offset-limit-fanout-splitter.md](15-offset-limit-fanout-splitter.md) | Next: [17-connection-pool-lease-iterator.md](17-connection-pool-lease-iterator.md)
