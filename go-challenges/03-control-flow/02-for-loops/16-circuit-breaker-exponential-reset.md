# Exercise 16: Circuit Breaker with Exponential Reset Backoff

**Nivel: Intermedio** — validacion rapida (un test corto).

A dependency that is down does not get better because your service keeps
hammering it every second. A circuit breaker stops calling a failing
dependency once it crosses a failure threshold, and — the part a naive
implementation gets wrong — widens the wait before each retry the longer the
dependency stays broken, instead of probing it at the same fixed interval
forever. This module builds that breaker: a bounded loop computes the
exponential delay, and the delay is capped so a permanently dead dependency
is still probed, just rarely.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
breaker/                      module example.com/breaker
  go.mod                      go 1.24
  breaker.go                  State; Breaker; New/Allow/RecordSuccess/RecordFailure/State
  breaker_test.go              full lifecycle, delay doubling, cap at max
  cmd/demo/
    main.go                    walks the breaker through trip, backoff, re-trip, recovery
```

- Files: `breaker.go`, `breaker_test.go`, `cmd/demo/main.go`.
- Implement: `Breaker` with `Closed`/`Open`/`HalfOpen` states; `Allow() bool`, `RecordSuccess()`, `RecordFailure()`, `State() State`; a private `resetDelay()` that computes the exponential backoff with a counted `for i := 1; i < openCount; i++` loop bounded by the trip count, capped at `max`.
- Test: closed allows and counts failures toward the threshold; trip opens and denies; the reset delay elapses and admits exactly one half-open trial; a failed trial re-trips with a doubled delay; a successful trial closes and resets the trip count; the delay never exceeds `max`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the backoff loop is bounded by the trip count, not by time

`resetDelay` does not ask "how long has it been broken" — it asks "how many
times in a row has a trial failed," which is exactly `openCount`. That count
is a small, monotonically-reset integer (it goes back to zero the moment a
trial succeeds), so a `for i := 1; i < openCount; i++` loop that doubles the
delay once per trip is both bounded and cheap: it can never run more times
than the breaker has actually tripped, and it recomputes from `base` on every
call rather than storing a mutable "current delay" field that could drift out
of sync with `openCount`. The cap check happens *inside* the loop
(`if d*2 > max`) rather than after it, so doubling never overflows past `max`
even for a very large trip count — the loop returns the capped value the
instant it would exceed the ceiling, instead of computing an enormous
duration and clamping it afterward.

The state machine itself is the other half of the design: `Allow` is the only
place that reads the clock, and it is also the only place that transitions
`Open` to `HalfOpen` — a half-open breaker admits exactly one trial, and
whether that trial becomes a `RecordSuccess` (full close, trip count reset) or
a `RecordFailure` (immediate re-trip with a now-doubled delay) is entirely up
to the caller's next call. This mirrors the token bucket's discipline from
Exercise 1: the clock is injected once in `New` and there is no setter, so a
test advances a fake clock by exact amounts instead of sleeping.

Create `breaker.go`:

```go
package breaker

import (
	"sync"
	"time"
)

// State is one of the three circuit breaker states.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker is a circuit breaker that opens after threshold consecutive
// failures and reopens with an exponentially growing delay each time a
// half-open trial fails again, so a persistently broken dependency is probed
// less and less often instead of hammering it at a fixed interval.
type Breaker struct {
	mu        sync.Mutex
	state     State
	failures  int
	threshold int
	openedAt  time.Time
	openCount int
	base      time.Duration
	max       time.Duration
	clock     func() time.Time
}

// New builds a Breaker that trips after threshold consecutive failures and
// resets with a delay that starts at base and doubles on every repeated trip,
// capped at max. clock is taken once and never mutated.
func New(threshold int, base, max time.Duration, clock func() time.Time) *Breaker {
	return &Breaker{
		threshold: threshold,
		base:      base,
		max:       max,
		clock:     clock,
	}
}

// Allow reports whether a call should proceed. A closed breaker always
// allows; an open breaker allows exactly once its reset delay has elapsed,
// moving to half-open to admit a single trial call; a half-open breaker
// allows the trial itself.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case HalfOpen:
		return true
	default: // Open
		if b.clock().Sub(b.openedAt) >= b.resetDelay() {
			b.state = HalfOpen
			return true
		}
		return false
	}
}

// RecordSuccess reports a successful call. From half-open it fully closes the
// breaker and resets the trip count, so the next failure streak starts back
// at the base delay.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Closed
	b.failures = 0
	b.openCount = 0
}

// RecordFailure reports a failed call. A failing half-open trial re-trips
// immediately; a failing closed call counts toward the threshold and trips
// once it is reached.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == HalfOpen {
		b.trip()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.trip()
	}
}

func (b *Breaker) trip() {
	b.state = Open
	b.openCount++
	b.openedAt = b.clock()
	b.failures = 0
}

// State reports the breaker's current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// resetDelay computes the exponential reset delay for the current trip count.
// The loop is bounded by openCount -- exactly the number of consecutive times
// the breaker has tripped -- doubling once per trip and capping at max so a
// permanently broken dependency is probed less and less often instead of at
// an ever-growing, unbounded interval.
func (b *Breaker) resetDelay() time.Duration {
	d := b.base
	for i := 1; i < b.openCount; i++ {
		if d*2 > b.max {
			return b.max
		}
		d *= 2
	}
	return d
}
```

### The runnable demo

The demo drives a hand-advanced clock through a full trip/backoff/re-trip/
recovery cycle: two failures trip the breaker (base delay 1s), the delay
elapses and admits a half-open trial, that trial fails and re-trips with the
delay doubled to 2s, and the second, longer wait finally admits a trial that
succeeds and closes the breaker.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/breaker"
)

// demoClock is advanced by hand so this demo prints the same output on every
// run instead of depending on wall-clock timing.
type demoClock struct{ now time.Time }

func (c *demoClock) Now() time.Time { return c.now }

func main() {
	clock := &demoClock{now: time.Unix(0, 0).UTC()}
	b := breaker.New(2, time.Second, 8*time.Second, clock.Now)

	report := func(label string) {
		allow := b.Allow()
		fmt.Printf("%-28s state=%-9s allow=%v\n", label, b.State(), allow)
	}

	report("fresh breaker")

	b.RecordFailure()
	report("after 1 failure")
	b.RecordFailure()
	report("after 2 failures (trips)")

	clock.now = clock.now.Add(1100 * time.Millisecond)
	report("1.1s later (base delay passed)")

	b.RecordFailure() // the half-open trial fails: re-trips with doubled delay
	report("half-open trial failed")

	clock.now = clock.now.Add(1500 * time.Millisecond)
	report("1.5s later (< doubled 2s delay)")

	clock.now = clock.now.Add(600 * time.Millisecond)
	report("2.1s later (>= doubled 2s delay)")

	b.RecordSuccess()
	report("half-open trial succeeded")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
fresh breaker                state=closed    allow=true
after 1 failure              state=closed    allow=true
after 2 failures (trips)     state=open      allow=false
1.1s later (base delay passed) state=half-open allow=true
half-open trial failed       state=open      allow=false
1.5s later (< doubled 2s delay) state=open      allow=false
2.1s later (>= doubled 2s delay) state=half-open allow=true
half-open trial succeeded    state=closed    allow=true
```

### Tests

`TestBreakerLifecycle` scripts the full sequence with a fake clock, exactly as
the demo does, and asserts each transition; `TestBreakerCapsDelayAtMax` trips
the breaker three times in a row via failed half-open trials so the
uncapped delay would reach 4s and confirms it is held at the 3s max instead.

Create `breaker_test.go`:

```go
package breaker

import (
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestBreakerLifecycle(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(0, 0).UTC()}
	b := New(2, time.Second, 8*time.Second, clock.Now)

	if !b.Allow() {
		t.Fatal("a fresh breaker must be closed and allow calls")
	}
	b.RecordFailure()
	if b.State() != Closed {
		t.Fatalf("state after 1 failure = %v, want closed", b.State())
	}

	b.RecordFailure()
	if b.State() != Open {
		t.Fatalf("state after 2 failures = %v, want open", b.State())
	}
	if b.Allow() {
		t.Fatal("an open breaker must deny before its reset delay elapses")
	}

	// Advance less than the base delay: still open.
	clock.now = clock.now.Add(500 * time.Millisecond)
	if b.Allow() {
		t.Fatal("an open breaker must deny before its reset delay elapses")
	}

	// Advance past the base delay: one trial is admitted, moving to half-open.
	clock.now = clock.now.Add(600 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow after the reset delay should admit exactly one trial")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state after trial admitted = %v, want half-open", b.State())
	}

	// The trial fails: re-trip immediately, and the delay must have doubled
	// (2s now, since this is the second trip).
	b.RecordFailure()
	if b.State() != Open {
		t.Fatalf("state after failed trial = %v, want open", b.State())
	}
	clock.now = clock.now.Add(1500 * time.Millisecond) // 1.5s < 2s doubled delay
	if b.Allow() {
		t.Fatal("second trip should wait the doubled delay (2s), not the base (1s)")
	}
	clock.now = clock.now.Add(600 * time.Millisecond) // total 2.1s >= 2s
	if !b.Allow() {
		t.Fatal("Allow after the doubled delay should admit a trial")
	}

	// This time the trial succeeds: breaker closes and the trip count resets.
	b.RecordSuccess()
	if b.State() != Closed {
		t.Fatalf("state after successful trial = %v, want closed", b.State())
	}
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Open {
		t.Fatal("breaker should trip again on a fresh failure streak")
	}
	clock.now = clock.now.Add(1100 * time.Millisecond) // just over the base delay again
	if !b.Allow() {
		t.Fatal("trip count should have reset after RecordSuccess, so the base delay applies again")
	}
}

func TestBreakerCapsDelayAtMax(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Unix(0, 0).UTC()}
	b := New(1, time.Second, 3*time.Second, clock.Now)

	// Trip repeatedly via failed half-open trials so the delay would grow
	// past max (1s, 2s, 4s...) and confirm it is capped at 3s.
	b.RecordFailure() // trip 1: delay 1s
	clock.now = clock.now.Add(1100 * time.Millisecond)
	b.Allow() // -> half-open
	b.RecordFailure() // trip 2: delay 2s
	clock.now = clock.now.Add(2100 * time.Millisecond)
	b.Allow() // -> half-open
	b.RecordFailure() // trip 3: uncapped would be 4s, capped to 3s
	clock.now = clock.now.Add(2500 * time.Millisecond)
	if b.Allow() {
		t.Fatal("delay should be capped at 3s, so 2.5s must still deny")
	}
	clock.now = clock.now.Add(600 * time.Millisecond) // total 3.1s
	if !b.Allow() {
		t.Fatal("delay capped at 3s should admit a trial once 3s elapsed")
	}
}
```

## Review

The breaker is correct when `resetDelay` is the only place the exponential
schedule is computed and `Allow` is the only place that reads the clock and
transitions `Open` to `HalfOpen`. The common mistake this design avoids is
storing a mutable `currentDelay` field that a caller doubles by hand on each
re-trip — that field can drift out of sync with `openCount` (for example, if
`RecordSuccess` forgets to reset it) and silently keeps a dependency in a slow
backoff long after it recovered. Recomputing from `base` and `openCount` on
every call makes that class of bug structurally impossible. Run
`go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counted form used by `resetDelay`.
- [Circuit Breaker pattern (Microsoft Azure Architecture Center)](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) — the closed/open/half-open state machine this module implements.
- [time package](https://pkg.go.dev/time) — `Duration` arithmetic used by the backoff schedule.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-sorted-id-reconciliation-merge.md](15-sorted-id-reconciliation-merge.md) | Next: [17-health-check-aggregator.md](17-health-check-aggregator.md)
