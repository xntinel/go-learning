# Exercise 5: Race-Detector Contract Suite for Both Limiters

Admission control that over-admits under load is worse than none — it fails
exactly when it matters. This module is about *testing concurrent code*: it
carries both limiter implementations plus the complete behavioral test suite,
and its subject is the methodology — why exact-N (not "roughly N") is
assertable, how to test refill without flaking, and why a silent race
detector plus exact counts together constitute a correctness proof.

## What you'll build

```text
limitercontract/                independent module: example.com/limitercontract
  go.mod
  limiter/
    limiter.go                  Limiter port + compile-time satisfaction checks
    mutex.go                    MutexLimiter with Tokens()
    channel.go                  ChannelLimiter with Close
    limiter_test.go             the full contract: bursts, refills (both
                                implementations), 50-goroutine exact-N storms,
                                idempotent Close, cap-at-max, Example
  cmd/
    demo/
      main.go                   the storm as a deterministic runnable
```

- Files: `limiter/limiter.go`, `limiter/mutex.go`, `limiter/channel.go`, `limiter/limiter_test.go`, `cmd/demo/main.go`.
- Implement: both limiters (self-contained copies) and the complete test suite.
- Test: initial-burst for both, refill-over-time for both, exact-N concurrency storms with `atomic.Int64`, idempotent `Close`, cap-at-max, and `ExampleMutexLimiter` with `// Output:`.
- Verify: `go test -count=1 -race ./...` — the `-race` flag is the point of the module.

Set up the module:

```bash
mkdir -p ~/go-exercises/limitercontract/limiter ~/go-exercises/limitercontract/cmd/demo
cd ~/go-exercises/limitercontract
go mod init example.com/limitercontract
```

### Why exact-N is assertable, and why it matters

The central trick: whenever a test wants to assert a count, it first makes
the system deterministic by *disabling refill* — `refillRate = 0` for the
mutex limiter, `refillInterval = time.Hour` for the channel one. With refill
off, a bucket of 1000 tokens hammered by 50 goroutines making 100 attempts
each must admit *exactly* 1000 requests. Not "about 1000" — exactly. Every
admission consumes precisely one token, no tokens are created, so the total
is fixed regardless of interleaving. Any deviation is a bug with a name:
1001+ means a torn read-modify-write or a check-then-act gap let two
goroutines spend the same token; 999- means an admission was lost or a token
double-spent on the accounting side.

An approximate assertion ("allowed should be within 5% of 1000") would pass
in both failure cases and *still* flake if the tolerance were misjudged.
Approximate concurrency assertions are simultaneously weak and fragile —
the worst combination. When you cannot make a system deterministic enough
for exact assertions, that is a design smell in the code under test, not a
reason to loosen the test.

The second half of the proof is `-race`. The exact count shows the *logic*
excludes over-admission; the race detector shows no data race occurred while
producing it — that every access to `tokens`/`lastRefill` was ordered by the
mutex, and every channel operation synchronized properly. Each check has
blind spots the other covers: a program can be race-free yet logically
over-admit (check-then-act with correct locking around each half), and a
racy program can produce the right count by luck. Silent `-race` plus exact
N covers both axes. The storm results themselves are accumulated in an
`atomic.Int64` — the test must not introduce its own race while measuring.

The refill tests use the opposite discipline. Time-dependent behavior is
asserted either against guarantees (`time.Sleep(150ms)` sleeps *at least*
150ms, so at 10 tokens/s at least one token must exist afterwards — a
one-sided bound that cannot flake) or by polling with a deadline (the
channel refill test loops on `Allow` until success or a 2s cap). Never
assert that something happens *within* a fixed short window; only assert
that it has happened *after* a guaranteed minimum, or *eventually* before a
generous cap.

Create `limiter/limiter.go`:

```go
// Package limiter contains two token-bucket implementations and the
// behavioral contract suite that pins them to identical semantics.
package limiter

// Limiter is the admission port. Implementations must be concurrency-safe.
type Limiter interface {
	Allow() bool
}

// Interface drift fails the build here, not at test time.
var (
	_ Limiter = (*MutexLimiter)(nil)
	_ Limiter = (*ChannelLimiter)(nil)
)
```

Create `limiter/mutex.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// MutexLimiter refills continuously from elapsed wall time under one mutex.
type MutexLimiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second; 0 disables refill
	lastRefill time.Time
}

func NewMutexLimiter(maxTokens, refillRate float64) *MutexLimiter {
	return &MutexLimiter{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// Allow refills from elapsed time and spends one token, all in a single
// critical section: the invariant spans tokens and lastRefill together.
func (l *MutexLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.tokens += now.Sub(l.lastRefill).Seconds() * l.refillRate
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

// Tokens reports the count as of the last Allow; observability only.
func (l *MutexLimiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokens
}
```

Create `limiter/channel.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// ChannelLimiter holds tokens in a pre-filled buffered channel; a ticker
// goroutine refills one token per tick and drops ticks when full.
type ChannelLimiter struct {
	tokens chan struct{}
	stop   chan struct{}
	once   sync.Once
}

func NewChannelLimiter(maxTokens int, refillInterval time.Duration) *ChannelLimiter {
	cl := &ChannelLimiter{
		tokens: make(chan struct{}, maxTokens),
		stop:   make(chan struct{}),
	}
	for range maxTokens {
		cl.tokens <- struct{}{}
	}
	go func() {
		ticker := time.NewTicker(refillInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case cl.tokens <- struct{}{}:
				default:
				}
			case <-cl.stop:
				return
			}
		}
	}()
	return cl
}

func (cl *ChannelLimiter) Allow() bool {
	select {
	case <-cl.tokens:
		return true
	default:
		return false
	}
}

// Close stops the refill goroutine; idempotent via sync.Once.
func (cl *ChannelLimiter) Close() {
	cl.once.Do(func() { close(cl.stop) })
}
```

### The demo: the storm as an executable claim

The demo runs the exact-N storm outside the test harness, so you can watch
the invariant hold interactively (and break it on purpose — remove the
mutex's `defer l.mu.Unlock()` re-lock discipline or widen the channel's
buffer mid-run — to see what a violation looks like).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/limitercontract/limiter"
)

func storm(l limiter.Limiter, goroutines, attempts int) int64 {
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range attempts {
				if l.Allow() {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	return allowed.Load()
}

func main() {
	ml := limiter.NewMutexLimiter(1000, 0)
	fmt.Printf("mutex  : 50 goroutines x 100 attempts -> allowed=%d/5000\n",
		storm(ml, 50, 100))

	cl := limiter.NewChannelLimiter(500, time.Hour)
	defer cl.Close()
	fmt.Printf("channel: 50 goroutines x  50 attempts -> allowed=%d/2500\n",
		storm(cl, 50, 50))
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```
mutex  : 50 goroutines x 100 attempts -> allowed=1000/5000
channel: 50 goroutines x  50 attempts -> allowed=500/2500
```

### The full contract suite

Every test that constructs its own limiter runs `t.Parallel()` — nothing is
shared between tests, and parallel execution increases the scheduler
interleavings `-race` gets to observe.

Create `limiter/limiter_test.go`:

```go
package limiter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutexLimiterInitialBurst(t *testing.T) {
	t.Parallel()

	l := NewMutexLimiter(5, 0)
	for i := range 5 {
		if !l.Allow() {
			t.Fatalf("Allow #%d returned false", i)
		}
	}
	if l.Allow() {
		t.Fatal("Allow after burst returned true")
	}
}

func TestMutexLimiterRefills(t *testing.T) {
	t.Parallel()

	// 10 tokens/sec -> one token every 100ms. Sleep guarantees a MINIMUM
	// elapsed time, so after 150ms at least 1.5 tokens must have accrued:
	// a one-sided bound that cannot flake on a slow scheduler.
	l := NewMutexLimiter(1, 10)

	if !l.Allow() {
		t.Fatal("first Allow returned false")
	}
	if l.Allow() {
		t.Fatal("second immediate Allow returned true (no refill yet)")
	}
	time.Sleep(150 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("Allow after 150ms returned false (refill should have happened)")
	}
}

func TestChannelLimiterRefillsOverTime(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(1, 50*time.Millisecond)
	defer l.Close()

	if !l.Allow() {
		t.Fatal("initial Allow returned false")
	}
	if l.Allow() {
		t.Fatal("second immediate Allow returned true (no tick yet)")
	}

	// Eventually-style assertion with a generous cap: poll until the
	// ticker lands a token or 2s pass. Normally succeeds on the first
	// ~50ms tick; never flakes under CI load.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l.Allow() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no token refilled within 2s")
}

func TestMutexLimiterConcurrentAllow(t *testing.T) {
	t.Parallel()

	l := NewMutexLimiter(1000, 0) // refill off: exact-N is assertable

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

func TestChannelLimiterInitialBurst(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(3, time.Hour) // refill essentially off
	defer l.Close()

	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("Allow #%d returned false", i)
		}
	}
	if l.Allow() {
		t.Fatal("Allow after burst returned true")
	}
}

func TestChannelLimiterConcurrentAllow(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(500, time.Hour) // refill off, test only the burst
	defer l.Close()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 50 {
				if l.Allow() {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	if got, want := allowed.Load(), int64(500); got != want {
		t.Fatalf("allowed = %d, want exactly %d", got, want)
	}
}

func TestChannelLimiterCloseIdempotent(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(1, time.Hour)
	l.Close()
	l.Close() // must not panic
}

func TestMutexLimiterCapsAtMax(t *testing.T) {
	t.Parallel()

	l := NewMutexLimiter(5, 1000)
	for range 5 {
		l.Allow()
	}
	// Sleep long enough that the bucket would overflow without the cap.
	time.Sleep(50 * time.Millisecond)
	l.Allow() // trigger a refill, which must cap at maxTokens
	if got := l.Tokens(); got > 5 {
		t.Fatalf("Tokens = %g, want <= 5 (cap at max)", got)
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

Read the suite as a taxonomy. Exact-count tests (`ConcurrentAllow` for both
backends) require manufactured determinism — refill off — and prove the
admission invariant under contention. One-sided-bound tests
(`MutexLimiterRefills`, `CapsAtMax`) assert only what `time.Sleep`'s
minimum-duration guarantee entails. Eventually-tests
(`ChannelLimiterRefillsOverTime`) poll under a generous deadline for
behavior driven by a background goroutine. Lifecycle tests
(`CloseIdempotent`) pin API safety properties that have nothing to do with
counting. Matching the assertion style to the mechanism under test is the
entire craft; the classic failures — asserting an exact count while refill
is on, or a fixed 60ms window for a 50ms ticker — come from mixing the
categories.

Always run this suite as `go test -count=1 -race ./...`. The `-count=1`
defeats the test cache (a cached pass exercises nothing), and `-race` turns
every run into a dynamic race audit. If you modify either limiter and the
exact-N storm still passes under `-race`, you have real evidence — not
vibes — that the change preserved the admission contract.

## Resources

- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how the detector instruments synchronization and what a report means.
- [sync package: WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 method the storms use.
- [sync/atomic: Int64](https://pkg.go.dev/sync/atomic#Int64) — race-free counting inside the measurement itself.
- [testing package: Examples](https://pkg.go.dev/testing#hdr-Examples) — how `// Output:` blocks are verified by `go test`.

---

Prev: [04-limiter-demo-cli.md](04-limiter-demo-cli.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-per-client-limiter-registry.md](06-per-client-limiter-registry.md)
