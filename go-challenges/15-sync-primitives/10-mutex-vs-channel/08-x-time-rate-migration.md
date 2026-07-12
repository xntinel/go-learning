# Exercise 8: Replacing the Hand-Rolled Limiter with golang.org/x/time/rate

Every senior engineer eventually performs this migration: the hand-rolled
limiter that taught the team how token buckets work gets replaced by
`golang.org/x/time/rate`, which ships the same semantics plus the production
capabilities nobody wants to hand-maintain — a built-in `Wait(ctx)`,
`Reserve` for pre-booking, `SetLimit` for live tuning. Because Exercise 3
made the limiter a port with a conformance suite, the migration is one table
entry, and the suite proves behavioral equivalence instead of hoping for it.

## What you'll build

```text
xratemigrate/                   independent module: example.com/xratemigrate
  go.mod                        requires golang.org/x/time
  limiter/
    limiter.go                  Limiter port; var _ checks for MutexLimiter,
                                ChannelLimiter, and *rate.Limiter itself
    mutex.go                    hand-rolled MutexLimiter (the outgoing code)
    channel.go                  hand-rolled ChannelLimiter (the outgoing code)
    conformance_test.go         the shared contract run against ALL THREE
    xrate_test.go               Wait fail-fast, SetLimit retuning, Reserve,
                                rate.Every, Example
  cmd/
    demo/
      main.go                   burst/deny, Every, Wait, Reserve in one run
```

- Files: `limiter/limiter.go`, `limiter/mutex.go`, `limiter/channel.go`, `limiter/conformance_test.go`, `limiter/xrate_test.go`, `cmd/demo/main.go`.
- Implement: nothing new — that is the point. Prove `*rate.Limiter` satisfies the existing port at compile time and passes the existing conformance suite unchanged, then exercise the capabilities the hand-rolled versions lack.
- Test: three-backend conformance (burst/deny plus exact-N storm), fail-fast `Wait` on an unrefillable bucket, `SetLimit` observably changing admission, `Reserve` with delay and cancel-refund, `rate.Every` arithmetic.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module (this one has a real dependency):

```bash
mkdir -p go-solutions/15-sync-primitives/10-mutex-vs-channel/08-x-time-rate-migration/limiter go-solutions/15-sync-primitives/10-mutex-vs-channel/08-x-time-rate-migration/cmd/demo
cd go-solutions/15-sync-primitives/10-mutex-vs-channel/08-x-time-rate-migration
go get golang.org/x/time/rate
```

### The adapter that turned out to be unnecessary

The migration plan said "write an adapter from `rate.Limiter` to our
`Limiter` port". Then you read the library's method set:
`func (lim *Limiter) Allow() bool`. The signature matches the port exactly,
so the adapter is zero lines:

```
var _ Limiter = (*rate.Limiter)(nil)
```

This is the payoff of consumer-defined, minimal interfaces: define the port
by what call sites need, and third-party types satisfy it by accident.
`rate.NewLimiter(r Limit, b int)` maps onto the hand-rolled constructor
vocabulary directly — `b` is `maxTokens` (the burst; the limiter starts
full), and `r` is `refillRate` in tokens per second, expressed as the
`rate.Limit` float type. `rate.Every(interval)` converts "one token every
100ms" configuration into a `Limit` (10/s), which is how interval-minded
configs (like the channel limiter's) migrate without anyone doing division
in their head. `r = 0` means no refill, exactly like the hand-rolled rate 0
— which is why the conformance suite's exact-N storm needs no changes.

### What you gain, concretely

`Wait(ctx)` is Exercise 7 done by the library, with two upgrades. First,
fairness: internally `Wait` takes a *reservation* — it pre-books a specific
future token and sleeps until that token's time arrives — so waiters are
served in reservation order rather than racing on wakeup like the
hand-rolled loop. Second, fail-fast deadline math: if the deadline cannot
possibly be met (an empty bucket with `Limit(0)` can never refill), `Wait`
returns an error *immediately* instead of sleeping out the context — the
test below pins that it fails in microseconds, not 50 milliseconds.

`Reserve()` is the capability with no hand-rolled equivalent: it commits a
token now and tells you `Delay()` — how long to wait before acting on it.
That inverts control: instead of blocking inside the limiter, your event
loop schedules the work at the permitted time. `Reservation.Cancel` refunds
the token (as best it can) if plans change. `SetLimit` and `SetBurst`
retune a live limiter safely mid-flight, which is what makes dynamic config
("ops bumped the partner quota, no redeploy") a one-liner.

And the closing observation for the whole lesson: `rate.Limiter` is
internally a `sync.Mutex` around a `float64` token count with continuous
elapsed-time refill — structurally the Exercise 1 design, not the
Exercise 2 one. For guarded shared state, the standard tool chose the
mutex. Hand-roll to learn; import to ship.

Create `limiter/limiter.go`:

```go
// Package limiter defines the admission port, two hand-rolled token buckets,
// and the compile-time proof that golang.org/x/time/rate drops in for both.
package limiter

import "golang.org/x/time/rate"

// Limiter is the admission port every backend satisfies.
type Limiter interface {
	Allow() bool
}

// The migration in one line: rate.Limiter satisfies the port as-is.
// No adapter needed — minimal consumer-defined interfaces pay off here.
var (
	_ Limiter = (*MutexLimiter)(nil)
	_ Limiter = (*ChannelLimiter)(nil)
	_ Limiter = (*rate.Limiter)(nil)
)
```

Create `limiter/mutex.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// MutexLimiter is the outgoing hand-rolled bucket: continuous refill under
// one mutex — structurally the same design rate.Limiter uses internally.
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
```

Create `limiter/channel.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// ChannelLimiter is the outgoing ticker-refill bucket over a buffered channel.
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

// Close stops the refill goroutine. Idempotent.
func (cl *ChannelLimiter) Close() {
	cl.once.Do(func() { close(cl.stop) })
}
```

### The demo: the new capabilities in one deterministic run

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"
)

func main() {
	// Zero refill, burst 3: the limiter starts full, then denies.
	l := rate.NewLimiter(0, 3)
	fmt.Println("burst then deny:", l.Allow(), l.Allow(), l.Allow(), l.Allow())

	// Interval-style config migrates via rate.Every.
	fmt.Println("rate.Every(500ms) =", rate.Every(500*time.Millisecond), "tokens/s")

	// Live retuning: ops raised the quota, no redeploy. Wait blocks until
	// the newly-permitted token arrives (~10ms at 100/s).
	l.SetLimit(rate.Limit(100))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fmt.Println("Wait after SetLimit(100/s):", l.Wait(ctx))

	// Reserve pre-books the next token and reports when it may be used.
	r := l.Reserve()
	fmt.Printf("reservation: ok=%v delay>0=%v\n", r.OK(), r.Delay() > 0)
	r.Cancel() // plans changed: refund the pre-booked token
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
burst then deny: true true true false
rate.Every(500ms) = 2 tokens/s
Wait after SetLimit(100/s): <nil>
reservation: ok=true delay>0=true
```

### Tests: conformance first, then the new powers

The conformance suite is byte-for-byte the Exercise 3 contract with one new
table entry — that *is* the migration test. `TestWaitFailsFastWhenHopeless`
pins the fail-fast property: with `Limit(0)` the bucket can never refill,
so `Wait` must return an error well before the 50ms deadline would have
elapsed (the library detects up front that the reservation cannot land
inside the deadline). Note the error is the library's own — *not*
`context.DeadlineExceeded`, because the context never actually expired —
so the test asserts on behavior (non-nil, prompt) rather than a specific
sentinel.

Create `limiter/conformance_test.go`:

```go
package limiter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

type newLimiter func(burst int) (Limiter, func())

func conformanceBackends() map[string]newLimiter {
	return map[string]newLimiter{
		"mutex": func(burst int) (Limiter, func()) {
			return NewMutexLimiter(float64(burst), 0), func() {}
		},
		"channel": func(burst int) (Limiter, func()) {
			cl := NewChannelLimiter(burst, time.Hour)
			return cl, cl.Close
		},
		// The migration: one table entry, zero new test logic.
		"x/time/rate": func(burst int) (Limiter, func()) {
			return rate.NewLimiter(0, burst), func() {}
		},
	}
}

func testLimiterContract(t *testing.T, construct newLimiter) {
	t.Helper()

	t.Run("burst then deny", func(t *testing.T) {
		t.Parallel()
		l, cleanup := construct(4)
		defer cleanup()

		for i := range 4 {
			if !l.Allow() {
				t.Fatalf("Allow #%d denied during burst", i)
			}
		}
		if l.Allow() {
			t.Fatal("Allow succeeded on an empty bucket")
		}
	})

	t.Run("exact burst under concurrency", func(t *testing.T) {
		t.Parallel()
		l, cleanup := construct(300)
		defer cleanup()

		var allowed atomic.Int64
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				for range 20 {
					if l.Allow() {
						allowed.Add(1)
					}
				}
			})
		}
		wg.Wait()

		if got, want := allowed.Load(), int64(300); got != want {
			t.Fatalf("allowed = %d, want exactly %d", got, want)
		}
	})
}

func TestLimiterConformance(t *testing.T) {
	t.Parallel()
	for name, construct := range conformanceBackends() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testLimiterContract(t, construct)
		})
	}
}
```

Create `limiter/xrate_test.go`:

```go
package limiter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestWaitFailsFastWhenHopeless(t *testing.T) {
	t.Parallel()

	l := rate.NewLimiter(0, 1) // no refill, ever
	if !l.Allow() {
		t.Fatal("initial burst denied")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := l.Wait(ctx)
	if err == nil {
		t.Fatal("Wait succeeded on a bucket that can never refill")
	}
	// Fail-fast: the library proves the deadline unreachable up front and
	// returns without sleeping out the context.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Wait took %v; want a fast up-front refusal", elapsed)
	}
}

func TestSetLimitChangesAdmission(t *testing.T) {
	t.Parallel()

	l := rate.NewLimiter(0, 1)
	l.Allow()
	if l.Allow() {
		t.Fatal("Allow succeeded with zero refill on an empty bucket")
	}

	l.SetLimit(rate.Limit(1000)) // ops raised the quota at runtime

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l.Allow() {
			return // admission observably resumed after retuning
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no admission within 2s of SetLimit(1000)")
}

func TestReserveAndCancelRefunds(t *testing.T) {
	t.Parallel()

	l := rate.NewLimiter(rate.Every(50*time.Millisecond), 1)
	if !l.Allow() {
		t.Fatal("initial burst denied")
	}

	r := l.Reserve()
	if !r.OK() {
		t.Fatal("Reserve.OK = false for n=1 within burst")
	}
	if r.Delay() <= 0 {
		t.Fatal("Reserve on an empty bucket must report a positive delay")
	}
	r.Cancel() // refund: the pre-booked token goes back

	// After the refund, the next token still arrives on the refill
	// schedule; poll generously rather than asserting exact timing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l.Allow() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no token within 2s after Reserve+Cancel")
}

func TestRateEvery(t *testing.T) {
	t.Parallel()

	if got, want := rate.Every(100*time.Millisecond), rate.Limit(10); got != want {
		t.Fatalf("rate.Every(100ms) = %v, want %v", got, want)
	}
	if got, want := rate.Every(2*time.Second), rate.Limit(0.5); got != want {
		t.Fatalf("rate.Every(2s) = %v, want %v", got, want)
	}
}

func Example() {
	l := rate.NewLimiter(0, 2)
	fmt.Println(l.Allow(), l.Allow(), l.Allow())
	// Output: true true false
}
```

## Review

Judge the migration the way the tests do. Equivalence: the conformance
suite passing for `x/time/rate` with zero new test logic means every call
site coded against the port keeps its behavior. Superiority: fail-fast
`Wait`, reservation fairness, `Reserve`/`Cancel`, and `SetLimit` are
capabilities the hand-rolled versions would each need dozens of subtle
lines (and their own storm tests) to match. The mistakes to avoid are
migration-specific: do not assert `context.DeadlineExceeded` from a `Wait`
that refuses up front — the library returns its own error before the
context expires; do not forget that `Reserve` *commits* a token, so a
reservation you abandon without `Cancel` is quota silently spent; and do
not translate an interval config by hand when `rate.Every` exists to get
the division right.

Run `go test -count=1 -race ./...` and read the verbose conformance output
once: three backends, one contract, identical subtests — that symmetry is
what "swappable backend" means. Keep the hand-rolled implementations in the
tree during a real migration until the conformance suite has run in CI
against the library for a while; deleting them is the last commit, not the
first.

## Resources

- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — `NewLimiter`, `Every`, `Allow`, `Wait`, `Reserve`, `SetLimit`, `Tokens`.
- [rate.Limiter source](https://cs.opensource.google/go/x/time/+/master:rate/rate.go) — see the mutex and float64 tokens at the top of the struct: the lesson's closing argument.
- [Go wiki: Use a sync.Mutex or a channel?](https://go.dev/wiki/MutexOrChannel) — the guidance the library's internals confirm.

---

Prev: [07-context-aware-wait.md](07-context-aware-wait.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-weighted-semaphore-fanout.md](09-weighted-semaphore-fanout.md)
