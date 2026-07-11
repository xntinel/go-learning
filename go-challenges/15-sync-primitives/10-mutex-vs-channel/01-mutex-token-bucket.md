# Exercise 1: Mutex-Guarded Token Bucket for an API Gateway

Your gateway calls an upstream partner API that contractually allows bursts of
N requests and a sustained rate of R per second; exceed it and the partner
starts returning 429s that count against your SLA. This exercise builds the
admission check as a token bucket guarded by a single `sync.Mutex`, with a
continuous time-based refill and an injectable clock so the tests never sleep.

## What you'll build

```text
mutexbucket/                    independent module: example.com/mutexbucket
  go.mod
  limiter/
    mutex.go                    type MutexLimiter; NewMutexLimiter,
                                NewMutexLimiterWithClock, Allow, Tokens
    mutex_test.go               fake-clock table tests, 50-goroutine exact-burst
                                storm, cap test, Example
  cmd/
    demo/
      main.go                   deterministic demo driven by a manual clock
```

- Files: `limiter/mutex.go`, `limiter/mutex_test.go`, `cmd/demo/main.go`.
- Implement: `MutexLimiter` with a float64 token count, continuous refill computed from the injected clock, refill-and-spend as one critical section, and a `Tokens()` inspection method.
- Test: table-driven refill tests against a fake clock (no sleeps), a 50-goroutine storm asserting exactly `maxTokens` allows with refill disabled, a cap-at-max test, and an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/mutexbucket/limiter ~/go-exercises/mutexbucket/cmd/demo
cd ~/go-exercises/mutexbucket
go mod init example.com/mutexbucket
```

### The design: one critical section, no check-then-act gap

A token bucket has two numbers that must stay mutually consistent: the token
count and the timestamp of the last refill. Every `Allow` call does a
read-modify-write across both — compute elapsed time, credit fractional
tokens, cap at the maximum, then try to spend one. The single most important
design decision is that *all of that happens under one `Lock`*. The broken
alternative shows up in real code reviews constantly: take the lock to read
the count, release it, decide, then take the lock again to decrement. Between
the two acquisitions another goroutine can spend the token you just observed,
and the bucket goes negative — the limiter over-admits under exactly the load
it exists to control. A mutex protects an *invariant*, and an invariant is
only protected if every read-modify-write sequence that relies on it is a
single critical section.

The refill is *continuous*: instead of a background goroutine adding tokens on
a timer, each call credits `elapsed.Seconds() * refillRate` fractional tokens
since the previous call. This has three consequences worth internalizing.
First, precision — a caller arriving 3.7ms after the last one credits exactly
0.0037·R tokens, so the sustained rate is enforced smoothly rather than in
tick-sized steps. Second, the type is a passive value: no goroutine, no
ticker, no `Close` method, no lifecycle to leak (contrast with the channel
implementation in Exercise 2, where lifecycle is half the work). Third, the
arithmetic runs inside the critical section — fine here because it is a few
float operations, but a reminder that everything under the lock is serialized
across all callers.

### The clock is a dependency; inject it

The original version of this limiter called `time.Now()` directly, and its
refill tests slept real milliseconds — slow, and flaky under a loaded CI
scheduler. The production-grade fix is to treat the clock as an injectable
dependency: the exported `NewMutexLimiter` wires in `time.Now`, and
`NewMutexLimiterWithClock` accepts any `func() time.Time`. Tests advance a
fake clock by exact durations and assert exact token counts — `advance 250ms
at 10 tokens/s, expect exactly 2 admissions` — with zero sleeps and zero
flakiness. (Go 1.25's `testing/synctest` offers an alternative that
virtualizes `time.Now` underneath un-instrumented code; clock injection
remains the portable pattern and also serves demos, simulations, and replay
tooling, so it is the one practiced here.)

Create `limiter/mutex.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// MutexLimiter is a token-bucket rate limiter guarded by a single mutex.
// Tokens refill continuously: each call credits elapsed*refillRate tokens,
// capped at maxTokens. The zero value is not usable; use a constructor.
type MutexLimiter struct {
	mu         sync.Mutex
	now        func() time.Time
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second; 0 disables refill
	lastRefill time.Time
}

// NewMutexLimiter returns a limiter that starts full (an initial burst of
// maxTokens) and refills at refillRate tokens per second, reading time.Now.
func NewMutexLimiter(maxTokens, refillRate float64) *MutexLimiter {
	return NewMutexLimiterWithClock(maxTokens, refillRate, time.Now)
}

// NewMutexLimiterWithClock is NewMutexLimiter with an injectable clock.
// Tests and simulations pass a fake now func; production passes time.Now.
func NewMutexLimiterWithClock(maxTokens, refillRate float64, now func() time.Time) *MutexLimiter {
	return &MutexLimiter{
		now:        now,
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: now(),
	}
}

// Allow reports whether one request may proceed, spending one token if so.
// Refill and spend happen in a single critical section: there is no gap in
// which another goroutine can observe or spend the tokens credited here.
func (l *MutexLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	elapsed := now.Sub(l.lastRefill).Seconds()
	l.tokens += elapsed * l.refillRate
	if l.tokens > l.maxTokens {
		l.tokens = l.maxTokens
	}
	l.lastRefill = now

	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// Tokens returns the token count as of the last refill. It does not itself
// refill; it exists for observability (metrics, debugging), not decisions.
func (l *MutexLimiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokens
}
```

Two details deserve a second look. `lastRefill = now` is written on *every*
call, even a denied one — the elapsed time was already converted into token
credit, so failing to advance the timestamp would double-count that interval
on the next call and over-admit. And `Tokens()` deliberately does not refill:
it is an observability accessor, and callers must never build a
check-then-act on top of it ("if Tokens() >= 1 then Allow()") — `Allow` is
the only atomic admission operation.

### The demo: a deterministic run on a manual clock

Because the clock is injected, the demo can script an exact scenario instead
of racing the wall clock: drain the burst, watch a denial, advance the clock
250ms (2.5 tokens at 10/s), and watch exactly two admissions come back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/mutexbucket/limiter"
)

// clock is a manual clock for a deterministic, scriptable demo run.
type clock struct {
	t time.Time
}

func (c *clock) Now() time.Time { return c.t }

func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func main() {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := limiter.NewMutexLimiterWithClock(5, 10, clk.Now)

	allowed := 0
	for range 5 {
		if l.Allow() {
			allowed++
		}
	}
	fmt.Printf("initial burst: allowed %d/5\n", allowed)
	fmt.Printf("bucket empty: allow=%v tokens=%.2f\n", l.Allow(), l.Tokens())

	clk.advance(250 * time.Millisecond) // 2.5 tokens at 10 tokens/s
	fmt.Printf("advance 250ms: allow=%v allow=%v allow=%v\n", l.Allow(), l.Allow(), l.Allow())
	fmt.Printf("tokens remaining: %.2f\n", l.Tokens())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial burst: allowed 5/5
bucket empty: allow=false tokens=0.00
advance 250ms: allow=true allow=true allow=false
tokens remaining: 0.50
```

### Tests: exact numbers, no sleeps, then a storm

The refill table pins the arithmetic with exact assertions (250ms at 10/s
admits exactly 2; 99ms admits 0; a long gap caps at `maxTokens`). The storm
test is the concurrency proof: 50 goroutines race 100 `Allow` calls each
against a bucket of 1000 with refill disabled, and the total admitted must be
*exactly* 1000 — not roughly. Combined with a silent race detector, the exact
count rules out both torn `tokens` updates and check-then-act over-admission.
The storm uses `sync.WaitGroup.Go` (Go 1.25), which wraps the Add/Done
bookkeeping.

Create `limiter/mutex_test.go`:

```go
package limiter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a mutex-protected manual clock safe for concurrent readers.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestMutexLimiterRefill(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		maxTokens   float64
		refillRate  float64
		advance     time.Duration
		wantAllowed int
	}{
		{"no refill without elapsed time", 5, 10, 0, 0},
		{"just under one token", 5, 10, 99 * time.Millisecond, 0},
		{"exactly 2.5 tokens admits 2", 5, 10, 250 * time.Millisecond, 2},
		{"one second refills the rate", 5, 10, time.Second, 5},
		{"long idle caps at maxTokens", 5, 10, time.Hour, 5},
		{"rate zero never refills", 5, 0, time.Hour, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clk := newFakeClock()
			l := NewMutexLimiterWithClock(tt.maxTokens, tt.refillRate, clk.Now)

			// Drain the initial burst completely.
			for range int(tt.maxTokens) {
				if !l.Allow() {
					t.Fatal("initial burst denied")
				}
			}
			if l.Allow() {
				t.Fatal("Allow succeeded on an empty bucket")
			}

			clk.Advance(tt.advance)

			allowed := 0
			for range int(tt.maxTokens) + 1 {
				if l.Allow() {
					allowed++
				}
			}
			if allowed != tt.wantAllowed {
				t.Fatalf("after advancing %v: allowed = %d, want exactly %d",
					tt.advance, allowed, tt.wantAllowed)
			}
		})
	}
}

func TestMutexLimiterConcurrentExactBurst(t *testing.T) {
	t.Parallel()

	// Refill disabled: across 50 goroutines x 100 attempts, exactly
	// maxTokens calls may succeed. Exact-N plus a silent race detector is
	// the correctness proof for the single-critical-section design.
	l := NewMutexLimiter(1000, 0)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 100 {
				if l.Allow() {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	if got, want := allowed.Load(), int64(1000); got != want {
		t.Fatalf("allowed = %d, want exactly %d (no refill, no over-allow)", got, want)
	}
}

func TestMutexLimiterCapsAtMax(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewMutexLimiterWithClock(5, 1000, clk.Now)

	for range 5 {
		l.Allow()
	}
	clk.Advance(time.Hour) // would credit 3.6 million tokens without the cap

	if !l.Allow() {
		t.Fatal("Allow denied after refill")
	}
	// Refill capped at 5, then one token spent: exactly 4 remain.
	if got := l.Tokens(); got != 4 {
		t.Fatalf("Tokens = %g, want exactly 4 (cap at max, minus one spent)", got)
	}
}

func ExampleMutexLimiter() {
	l := NewMutexLimiter(1, 0)
	fmt.Println(l.Allow())
	fmt.Println(l.Allow())
	// Output:
	// true
	// false
}
```

## Review

The heart of this module is that refill and spend form one atomic step. If
you ever find yourself releasing the lock between "compute the new token
count" and "decrement it", you have reintroduced the check-then-act gap that
the storm test exists to catch — run it under `-race` and watch the exact
count break. The second subtlety is `lastRefill`: it must advance on every
call, including denials, because the elapsed interval has already been
converted to credit; forgetting this double-counts time and quietly
over-admits. Third, keep `Tokens()` for dashboards only — any admission
decision built on it instead of `Allow` is racy by construction.

Confirm correctness with `go test -count=1 -race ./...` (all exact-count
assertions must hold with the race detector silent) and `go run ./cmd/demo`
(the output is fully deterministic because the demo owns its clock). Note
what this design did *not* need: no goroutine, no ticker, no `Close`.
Exercise 2 builds the same contract on a channel and inherits all three.

## Resources

- [Go wiki: Use a sync.Mutex or a channel?](https://go.dev/wiki/MutexOrChannel) — the decision table this lesson is built around.
- [sync package: Mutex](https://pkg.go.dev/sync#Mutex) — Lock/Unlock semantics and the fairness notes.
- [time package](https://pkg.go.dev/time) — `time.Since`, `Time.Sub`, `Duration.Seconds`, and the monotonic clock reading that makes elapsed-time math safe.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` catches and how to run it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-channel-semaphore-limiter.md](02-channel-semaphore-limiter.md)
