# Exercise 8: Circuit breaker half-open transition driven by an injected clock

A circuit breaker protects a backend from hammering a failing dependency: after
enough failures it *opens* and rejects calls immediately, then after a cooldown it
goes *half-open* and lets a single probe through to test recovery, closing on
success or re-opening on failure. The open-to-half-open transition is measured in
elapsed time, and the half-open state must admit exactly one probe. Inject a
`Clock` and the cooldown boundary and the single-probe gate both become exact,
deterministic assertions.

## What you'll build

```text
breaker/                       independent module: example.com/breaker
  go.mod
  breaker.go                   Clock; FakeClock; State; Breaker (Allow, Record)
  cmd/
    demo/
      main.go                  trip the breaker, advance past cooldown, probe, close
  breaker_test.go              trip; reject before cooldown; one probe at boundary; close/reopen
```

Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
Implement: a three-state `Breaker` (closed, open, half-open) that trips to open after a failure threshold, allows one half-open probe once a cooldown elapses (measured against an injected `Clock`), closes on probe success, and re-opens (resetting the cooldown) on probe failure.
Test: inject `FakeClock` — trip to open; assert reject at `cooldown-1ns`; assert exactly one probe admitted at the cooldown instant; a success closes, a failure re-opens and resets the clock; `-race`, `t.Parallel`.
Verify: `go test -count=1 -race ./...`

### The state machine and its two time-sensitive contracts

The breaker has three states. *Closed* is normal: calls are allowed, and failures
accumulate. When the failure count reaches the threshold, it trips to *open* and
records the instant with `openedAt`. *Open* rejects every call until the cooldown
elapses. Once `clock.Now()` reaches `openedAt.Add(cooldown)`, the next `Allow()`
transitions to *half-open* and returns `true` for that one call — the probe. In
*half-open*, further `Allow()` calls return `false`: only one probe is in flight.
`Record(true)` on the probe closes the breaker and resets the failure count;
`Record(false)` re-opens it and resets `openedAt`, starting a fresh cooldown.

Two contracts hinge on controlled time. First, the cooldown boundary is `<` vs
`<=`: at `cooldown-1ns` the clock is still before `openedAt+cooldown`, so `Allow`
rejects; at exactly `cooldown` the clock is no longer before it (`!Before`), so
`Allow` admits the probe. The `!now.Before(deadline)` form makes the boundary
*inclusive* — the probe is admitted at the exact cooldown instant, not one tick
later. Second, the single-probe gate: the transition to half-open and the "return
true" happen together inside one locked `Allow`, so a concurrent second `Allow`
observes half-open and is rejected. Without controlled time you could not assert
either boundary precisely; with a `FakeClock` both are one-line assertions.

Create `breaker.go`:

```go
package breaker

import (
	"sync"
	"time"
)

// Clock is the minimal time surface the breaker reads.
type Clock interface {
	Now() time.Time
}

// FakeClock is a test clock advanced by hand, safe for concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// State is a breaker state.
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

// Breaker is a three-state circuit breaker. The open->half-open cooldown is
// measured against the injected clock.
type Breaker struct {
	mu        sync.Mutex
	clock     Clock
	threshold int
	cooldown  time.Duration

	state    State
	failures int
	openedAt time.Time
}

func New(clock Clock, threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{clock: clock, threshold: threshold, cooldown: cooldown, state: Closed}
}

// Allow reports whether a call may proceed. In open state it rejects until the
// cooldown elapses, then admits exactly one half-open probe.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if !b.clock.Now().Before(b.openedAt.Add(b.cooldown)) {
			b.state = HalfOpen
			return true // the single probe
		}
		return false
	case HalfOpen:
		return false // a probe is already in flight
	default: // Closed
		return true
	}
}

// State returns the current state (for tests and observability).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Record reports the outcome of an allowed call. Success closes the breaker; a
// failure trips it (from closed, at the threshold) or re-opens it (from
// half-open), resetting the cooldown clock.
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.state = Closed
		b.failures = 0
		return
	}
	switch b.state {
	case HalfOpen:
		b.state = Open
		b.openedAt = b.clock.Now()
	default:
		b.failures++
		if b.failures >= b.threshold {
			b.state = Open
			b.openedAt = b.clock.Now()
		}
	}
}
```

### The runnable demo

The demo trips a breaker with a 3-failure threshold and a 1-second cooldown on a
`FakeClock`, shows it rejecting before cooldown, advances past the cooldown, and
closes it with a successful probe.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/breaker"
)

func main() {
	fc := breaker.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b := breaker.New(fc, 3, time.Second)

	for range 3 {
		b.Record(false) // three failures trip it
	}
	fmt.Printf("state: %s, allow: %v\n", b.State(), b.Allow())

	fc.Advance(time.Second) // cooldown elapsed
	fmt.Printf("probe allowed: %v, state: %s\n", b.Allow(), b.State())

	b.Record(true) // probe succeeds -> closed
	fmt.Printf("after success: %s, allow: %v\n", b.State(), b.Allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
state: open, allow: false
probe allowed: true, state: half-open
after success: closed, allow: true
```

### Tests

`TestTripsAtThreshold` proves the breaker opens exactly at the failure threshold,
not before. `TestCooldownBoundary` trips it, asserts reject at `cooldown-1ns`, and
asserts the probe is admitted at exactly `cooldown` and that a second `Allow` is
rejected (single probe). `TestProbeSuccessCloses` and `TestProbeFailureReopens`
prove the half-open outcomes, the latter checking the cooldown clock is reset.

Create `breaker_test.go`:

```go
package breaker

import (
	"fmt"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestTripsAtThreshold(t *testing.T) {
	t.Parallel()
	b := New(NewFakeClock(epoch), 3, time.Second)
	b.Record(false)
	b.Record(false)
	if b.State() != Closed {
		t.Fatalf("state after 2 failures = %s, want closed", b.State())
	}
	b.Record(false) // third failure trips
	if b.State() != Open {
		t.Fatalf("state after 3 failures = %s, want open", b.State())
	}
}

func TestCooldownBoundary(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	b := New(fc, 1, time.Second)
	b.Record(false) // trip

	fc.Advance(time.Second - time.Nanosecond)
	if b.Allow() {
		t.Fatal("Allow admitted a probe 1ns before cooldown")
	}
	fc.Advance(time.Nanosecond) // exactly cooldown
	if !b.Allow() {
		t.Fatal("Allow rejected the probe at the exact cooldown instant")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state after probe admitted = %s, want half-open", b.State())
	}
	if b.Allow() {
		t.Fatal("Allow admitted a second probe in half-open")
	}
}

func TestProbeSuccessCloses(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	b := New(fc, 1, time.Second)
	b.Record(false)
	fc.Advance(time.Second)
	if !b.Allow() {
		t.Fatal("probe not admitted after cooldown")
	}
	b.Record(true)
	if b.State() != Closed {
		t.Fatalf("state after successful probe = %s, want closed", b.State())
	}
	if !b.Allow() {
		t.Fatal("closed breaker rejected a call")
	}
}

func TestProbeFailureReopens(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	b := New(fc, 1, time.Second)
	b.Record(false)
	fc.Advance(time.Second)
	if !b.Allow() {
		t.Fatal("probe not admitted after cooldown")
	}
	b.Record(false) // probe fails -> reopen, cooldown resets from now
	if b.State() != Open {
		t.Fatalf("state after failed probe = %s, want open", b.State())
	}
	// Cooldown restarts: still rejecting just under a full cooldown later.
	fc.Advance(time.Second - time.Nanosecond)
	if b.Allow() {
		t.Fatal("Allow admitted before the reset cooldown elapsed")
	}
	fc.Advance(time.Nanosecond)
	if !b.Allow() {
		t.Fatal("Allow rejected after the reset cooldown elapsed")
	}
}

func ExampleBreaker() {
	fc := NewFakeClock(epoch)
	b := New(fc, 2, time.Second)
	b.Record(false)
	b.Record(false)
	fmt.Println(b.State(), b.Allow())
	fc.Advance(time.Second)
	fmt.Println(b.Allow(), b.State())
	// Output:
	// open false
	// true half-open
}
```

## Review

The breaker is correct when it trips exactly at the threshold, rejects throughout
the cooldown, admits exactly one probe at the inclusive cooldown instant, closes
on a good probe, and re-opens with a fresh cooldown on a bad one. The two
time-sensitive contracts — the `!now.Before(openedAt+cooldown)` boundary and the
single-probe gate inside one locked `Allow` — are only assertable precisely with a
controlled clock; that is why the clock is injected rather than read from
`time.Now`. The classic bugs this guards against are a breaker that admits at
`cooldown+1` instead of `cooldown` (an off-by-one on the deadline) and one that
lets two probes through half-open (the gate not being atomic with the transition).
Run `go test -race`; the mutex must serialize `Allow` and `Record`.

## Resources

- [`time.Time.Add`](https://pkg.go.dev/time#Time.Add) and [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) — the cooldown deadline comparison.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the three-state model this implements.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — serializing the state transitions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-debounce-timer-reset.md](07-debounce-timer-reset.md) | Next: [09-token-expiry-clock-skew.md](09-token-expiry-clock-skew.md)
