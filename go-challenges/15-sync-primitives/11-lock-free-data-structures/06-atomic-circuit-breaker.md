# Exercise 6: CAS State Machine: Circuit Breaker Transitions

A circuit breaker guards a shaky downstream: after enough consecutive failures
it opens and fails fast, after a cooldown it lets exactly one probe through, and
the probe's outcome decides whether to close again. Every one of those "exactly
one" claims is a CAS: this exercise builds the state machine where
`CompareAndSwap` *is* the transition, so a hundred goroutines racing the same
edge produce exactly one winner.

## What you'll build

```text
circuitbreaker/                  independent module: example.com/circuitbreaker
  go.mod
  breaker.go                     State (Closed/Open/HalfOpen); Breaker: Allow,
                                 ReportSuccess, ReportFailure, State, Trips
  breaker_test.go                deterministic transitions with injected clock;
                                 100 goroutines race for the single probe;
                                 concurrent-failure single-trip test; Example
  cmd/
    demo/
      main.go                    trip, fail fast, cool down, probe, recover
```

- Files: `breaker.go`, `breaker_test.go`, `cmd/demo/main.go`.
- Implement: state in an `atomic.Int32`, consecutive failures in an `atomic.Int64`, trip time as UnixNano in an `atomic.Int64`, transitions Closed to Open, Open to HalfOpen, HalfOpen to Closed/Open, all via `CompareAndSwap`.
- Test: injected clock drives deterministic transitions; the hard test counts CAS winners among 100 racing goroutines and demands exactly one probe; concurrent `ReportFailure` trips exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/11-lock-free-data-structures/06-atomic-circuit-breaker/cmd/demo
cd go-solutions/15-sync-primitives/11-lock-free-data-structures/06-atomic-circuit-breaker
```

### CAS as an exactly-once transition

The breaker's states fit in an `int32`: Closed (requests flow, failures are
counted), Open (fail fast until the cooldown elapses), HalfOpen (one probe is in
flight; everyone else fails fast). The transitions are where concurrency bites:

- Closed to Open: many requests can fail *simultaneously*. Each increments the
  failure counter, and every goroutine that observes the threshold crossed will
  try to trip — but only one `CompareAndSwap(Closed, Open)` can succeed, so the
  trip happens exactly once. Losers simply move on; the state they wanted is
  already there.
- Open to HalfOpen: after the cooldown, a stampede of requests arrives at once.
  Whoever wins `CompareAndSwap(Open, HalfOpen)` *is* the probe — the CAS result
  doubles as the probe token. There is no separate flag to hand out, so there is
  no window where two goroutines both hold it.
- HalfOpen to Closed (probe succeeded) or back to Open (probe failed): again a
  CAS, so a stale `ReportSuccess` from a request admitted before the trip cannot
  close an Open breaker — the CAS from HalfOpen fails and the report is
  correctly ignored.

Note the design grammar: *check* state with `Load`, *change* state with
`CompareAndSwap` from the exact state you observed. A `Store` would trample
whatever transition raced in between; every write to `state` in this type is a
CAS for that reason.

The trip timestamp rides in a separate `atomic.Int64` (UnixNano). It is written
just *before* the trip CAS, so any goroutine that observes state Open also
observes a trip time at least as fresh as that transition (the memory model's
sequentially consistent ordering gives us that). Racing trip candidates may each
write a timestamp a few nanoseconds apart and only one will win the CAS — the
losers' writes harmlessly nudge the trip time by nanoseconds. What the ordering
rules out is the bad case: a goroutine seeing Open with a *stale or zero* trip
time and starting the cooldown from the wrong instant.

Failure counting is *consecutive* failures: any success in Closed resets the
count. That is the standard breaker semantic — ten failures spread over a day of
successes should not trip anything.

Create `breaker.go`:

```go
package circuitbreaker

import (
	"sync/atomic"
	"time"
)

// State is the breaker's position.
type State int32

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker is a lock-free circuit breaker. Transitions are CAS
// operations, so each edge is taken exactly once no matter how many
// goroutines race it. Breaker must not be copied after first use.
type Breaker struct {
	threshold int64 // consecutive failures that trip the breaker
	cooldown  time.Duration
	now       func() time.Time

	state     atomic.Int32
	failures  atomic.Int64
	trippedAt atomic.Int64 // UnixNano of the last trip
	trips     atomic.Int64 // total Closed/HalfOpen -> Open transitions
}

// New returns a Closed breaker that opens after threshold consecutive
// failures and probes again cooldown after tripping.
func New(threshold int, cooldown time.Duration) *Breaker {
	return NewWithClock(threshold, cooldown, time.Now)
}

// NewWithClock is New with an injected clock for deterministic tests.
func NewWithClock(threshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	return &Breaker{threshold: int64(threshold), cooldown: cooldown, now: now}
}

// State reports the current state.
func (b *Breaker) State() State {
	return State(b.state.Load())
}

// Trips reports how many times the breaker has opened (monitoring).
func (b *Breaker) Trips() int64 {
	return b.trips.Load()
}

// Allow reports whether this request may proceed. In HalfOpen, only
// the goroutine whose CAS won the Open -> HalfOpen transition gets
// true: the CAS result is the probe token.
func (b *Breaker) Allow() bool {
	switch State(b.state.Load()) {
	case StateClosed:
		return true
	case StateOpen:
		if b.now().UnixNano()-b.trippedAt.Load() < int64(b.cooldown) {
			return false
		}
		return b.state.CompareAndSwap(int32(StateOpen), int32(StateHalfOpen))
	default: // StateHalfOpen: a probe is already in flight
		return false
	}
}

// ReportSuccess records a successful call. It closes a half-open
// breaker (the probe passed) and clears the consecutive-failure
// count in Closed.
func (b *Breaker) ReportSuccess() {
	if b.state.CompareAndSwap(int32(StateHalfOpen), int32(StateClosed)) {
		b.failures.Store(0)
		return
	}
	if State(b.state.Load()) == StateClosed {
		b.failures.Store(0)
	}
}

// ReportFailure records a failed call. In Closed it trips the breaker
// once the consecutive-failure threshold is reached; in HalfOpen the
// failed probe re-opens the breaker and restarts the cooldown.
func (b *Breaker) ReportFailure() {
	switch State(b.state.Load()) {
	case StateHalfOpen:
		b.trippedAt.Store(b.now().UnixNano())
		if b.state.CompareAndSwap(int32(StateHalfOpen), int32(StateOpen)) {
			b.trips.Add(1)
		}
	case StateClosed:
		if b.failures.Add(1) >= b.threshold {
			b.trippedAt.Store(b.now().UnixNano())
			if b.state.CompareAndSwap(int32(StateClosed), int32(StateOpen)) {
				b.trips.Add(1)
				b.failures.Store(0)
			}
		}
	}
}
```

### Tests

The deterministic suite walks the full lifecycle with a fake clock: trip at the
threshold (not before), fail fast while Open, deny before the cooldown and probe
after it, close on probe success, re-open on probe failure. The two hard tests
are the exactly-once claims. `TestExactlyOneProbe` trips the breaker, advances
the clock past the cooldown, releases 100 goroutines against `Allow`, and counts
grants: the answer must be exactly 1, because the probe token *is* the CAS win.
`TestConcurrentFailuresTripOnce` fires 100 concurrent `ReportFailure` calls with
a threshold of 5 and asserts `Trips() == 1` — many goroutines observe the
threshold crossed, one CAS wins.

Create `breaker_test.go`:

```go
package circuitbreaker

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	base   time.Time
	offset atomic.Int64
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

func TestLifecycle(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	b := NewWithClock(3, time.Minute, clk.Now)

	// Failures below the threshold keep the breaker closed.
	b.ReportFailure()
	b.ReportFailure()
	if got := b.State(); got != StateClosed {
		t.Fatalf("state after 2 failures = %v, want closed", got)
	}

	// A success resets the consecutive count.
	b.ReportSuccess()
	b.ReportFailure()
	b.ReportFailure()
	if got := b.State(); got != StateClosed {
		t.Fatalf("state after reset + 2 failures = %v, want closed", got)
	}

	// The third consecutive failure trips it.
	b.ReportFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("state at threshold = %v, want open", got)
	}
	if b.Allow() {
		t.Fatal("Allow granted while open, before cooldown")
	}

	// Cooldown not yet elapsed: still failing fast.
	clk.Advance(30 * time.Second)
	if b.Allow() {
		t.Fatal("Allow granted 30s into a 60s cooldown")
	}

	// Cooldown elapsed: exactly one probe.
	clk.Advance(31 * time.Second)
	if !b.Allow() {
		t.Fatal("probe denied after cooldown")
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state during probe = %v, want half-open", got)
	}
	if b.Allow() {
		t.Fatal("second Allow granted while half-open")
	}

	// Probe succeeds: closed again, failure count cleared.
	b.ReportSuccess()
	if got := b.State(); got != StateClosed {
		t.Fatalf("state after successful probe = %v, want closed", got)
	}
	if !b.Allow() {
		t.Fatal("Allow denied after recovery")
	}
}

func TestFailedProbeReopens(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	b := NewWithClock(1, time.Minute, clk.Now)

	b.ReportFailure()
	clk.Advance(2 * time.Minute)
	if !b.Allow() {
		t.Fatal("probe denied after cooldown")
	}
	b.ReportFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("state after failed probe = %v, want open", got)
	}
	if got := b.Trips(); got != 2 {
		t.Fatalf("Trips = %d, want 2 (initial trip + failed probe)", got)
	}
	// The failed probe restarted the cooldown.
	clk.Advance(30 * time.Second)
	if b.Allow() {
		t.Fatal("Allow granted before the restarted cooldown elapsed")
	}
}

func TestExactlyOneProbe(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	b := NewWithClock(1, time.Minute, clk.Now)
	b.ReportFailure()
	clk.Advance(2 * time.Minute)

	const goroutines = 100
	var granted atomic.Int64
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if b.Allow() {
				granted.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	if got := granted.Load(); got != 1 {
		t.Fatalf("probes granted = %d, want exactly 1", got)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state after probe race = %v, want half-open", got)
	}
}

func TestConcurrentFailuresTripOnce(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	b := NewWithClock(5, time.Minute, clk.Now)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ReportFailure()
		}()
	}
	wg.Wait()

	if got := b.State(); got != StateOpen {
		t.Fatalf("state after failure storm = %v, want open", got)
	}
	if got := b.Trips(); got != 1 {
		t.Fatalf("Trips = %d, want exactly 1", got)
	}
}

func ExampleBreaker() {
	clk := newFakeClock()
	b := NewWithClock(2, time.Minute, clk.Now)
	b.ReportFailure()
	b.ReportFailure()
	fmt.Println(b.State(), b.Allow())
	clk.Advance(2 * time.Minute)
	fmt.Println(b.Allow(), b.State())
	// Output:
	// open false
	// true half-open
}
```

### The demo

A downstream that fails three times trips the breaker; calls fail fast; after
the (real) 100 ms cooldown one probe goes through, succeeds, and the breaker
closes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/circuitbreaker"
)

func main() {
	b := circuitbreaker.New(3, 100*time.Millisecond)

	for i := 1; i <= 3; i++ {
		b.ReportFailure()
		fmt.Printf("failure %d reported, state=%s\n", i, b.State())
	}

	fmt.Printf("fail fast: allow=%v\n", b.Allow())

	time.Sleep(150 * time.Millisecond)
	if b.Allow() {
		fmt.Printf("probe admitted, state=%s\n", b.State())
		b.ReportSuccess()
	}
	fmt.Printf("recovered, state=%s allow=%v\n", b.State(), b.Allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
failure 1 reported, state=closed
failure 2 reported, state=closed
failure 3 reported, state=open
fail fast: allow=false
probe admitted, state=half-open
recovered, state=closed allow=true
```

## Review

Audit this type with one rule: state is only ever written by `CompareAndSwap`
from an observed value, never by `Store`. That is what makes every "exactly
once" test pass — trip once under a failure storm, one probe token under a
stampede, and a stale success report unable to close an Open breaker. The
subtle line is the `trippedAt.Store` *before* the trip CAS: order them the other
way and there is a window where a goroutine sees Open with an old trip time and
probes immediately, shrinking your cooldown to nanoseconds under load. Keep the
semantics honest too: the failure count is consecutive (successes clear it), a
failed probe restarts the cooldown from the failure instant, and `Trips` is a
monitoring counter — alert on it, never branch on it. Production breakers
(e.g. sony/gobreaker) add success thresholds and sliding windows on top; the
transition core underneath is exactly this CAS machine.

## Resources

- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the pattern, its states, and why fail-fast protects the caller too.
- [sync/atomic: Int32.CompareAndSwap](https://pkg.go.dev/sync/atomic#Int32.CompareAndSwap) — the typed CAS these transitions are built on.
- [sony/gobreaker](https://pkg.go.dev/github.com/sony/gobreaker/v2) — a production breaker; compare its mutex-based generation counting with this lock-free core.
- [microservices.io: Circuit Breaker](https://microservices.io/patterns/reliability/circuit-breaker.html) — the pattern in a service-to-service context.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-cas-token-bucket-limiter.md](05-cas-token-bucket-limiter.md) | Next: [07-cow-subscriber-registry.md](07-cow-subscriber-registry.md)
