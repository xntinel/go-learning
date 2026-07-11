# Exercise 29: Circuit Breaker — Deferred Timeout Resets Half-Open State

**Nivel: Intermedio** — validacion rapida (un test corto).

A circuit breaker protects a caller from hammering a downstream that has
already shown it is failing: after enough consecutive failures it opens,
short-circuiting every call with an immediate error instead of waiting
for a timeout each time. After a reset period it allows exactly one
probe call through in a half-open state — success closes the breaker
again, failure reopens it. The subtle failure mode is a probe that never
reports back: a goroutine that hung, panicked somewhere upstream of the
breaker, or simply never returned leaves the breaker permanently
half-open, offering its one probe slot to a caller that will never
release it. This module builds a breaker whose every call — even one
rejected immediately because a probe is already in flight — runs a
deferred check for exactly that condition, self-healing back to open
instead of staying stuck. The module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
breaker/                     independent module: example.com/circuit-breaker-half-open-probe
  go.mod                      go 1.24
  breaker.go                   State, Breaker (Call, State)
  cmd/
    demo/
      main.go                 runnable demo: trip open, rejected while open, probe succeeds, closes
  breaker_test.go              table over the closed/open/half-open transition sequence; stale-probe case
```

- Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
- Implement: `Breaker` with `Now`, `FailureThreshold`, `ResetTimeout`, `ProbeTimeout` fields, `Call(fn func() error) error`, and `State() State`.
- Test: a sequential table driving the breaker through closed, open, half-open, and back to closed, plus a case proving a stale half-open probe expires even on a rejected call.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/circuit-breaker-half-open-probe/cmd/demo
cd ~/go-exercises/circuit-breaker-half-open-probe
go mod init example.com/circuit-breaker-half-open-probe
go mod edit -go=1.24
```

### Why the stale-probe check has to be a defer, not a step inside one branch

`Call` has three branches that can return early — `ErrOpen` while still
inside the reset timeout, `ErrProbeInFlight` when a probe is already
running, and `fn`'s own error or success once it actually runs — plus one
implicit path where `fn` never runs at all. The stale-probe check,
`expireStaleProbe`, needs to run on *every one* of those paths, because a
hung probe can be discovered by any caller, not just the one that
happens to be the next successful call. Writing that check as a
non-deferred call only at the bottom of the success path would miss the
`ErrOpen` and `ErrProbeInFlight` early returns entirely — exactly the
paths a caller takes while a probe is stuck. `defer
b.expireStaleProbe()`, registered as the very first line of `Call`,
sidesteps that: it is the function's *last* action on every exit,
regardless of which branch got there, so a caller that gets rejected
with `ErrProbeInFlight` still triggers the check that might reset the
breaker back to `Open` for the *next* caller to get a fresh probe.

Create `breaker.go`:

```go
package breaker

import (
	"errors"
	"time"
)

// State represents the circuit breaker's current mode.
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

// ErrOpen is returned when a call is rejected because the breaker is open.
var ErrOpen = errors.New("breaker: circuit is open")

// ErrProbeInFlight is returned when a half-open probe is already running
// and another call arrives before it reports back.
var ErrProbeInFlight = errors.New("breaker: a half-open probe is already in flight")

// Breaker is a circuit breaker with a half-open probe phase, driven by an
// injectable clock so its timeout logic is deterministic and testable
// without real sleeps.
type Breaker struct {
	Now              func() time.Time
	FailureThreshold int
	ResetTimeout     time.Duration
	ProbeTimeout     time.Duration

	state        State
	failures     int
	openedAt     time.Time
	probeStarted time.Time
	probeActive  bool
}

// Call runs fn if the breaker currently allows it, and updates the
// breaker's state based on both the outcome of fn and the passage of time.
//
// Entering the half-open probe registers a deferred check that always
// runs, on every exit path out of Call -- including the early return when
// a probe is already in flight and fn never runs -- so "is the in-flight
// probe now stale" gets answered on every call, not just the one that
// happens to succeed or fail.
func (b *Breaker) Call(fn func() error) error {
	defer b.expireStaleProbe()

	switch b.state {
	case Open:
		if b.Now().Sub(b.openedAt) < b.ResetTimeout {
			return ErrOpen
		}
		b.state = HalfOpen
		b.probeActive = true
		b.probeStarted = b.Now()
	case HalfOpen:
		if b.probeActive {
			return ErrProbeInFlight
		}
		b.probeActive = true
		b.probeStarted = b.Now()
	case Closed:
		// fall through to running fn below
	}

	err := fn()
	b.record(err)
	return err
}

func (b *Breaker) record(err error) {
	if err != nil {
		b.failures++
		if b.state == Closed && b.failures < b.FailureThreshold {
			return // isolated failure while closed: still tolerated
		}
		b.state = Open
		b.openedAt = b.Now()
		b.probeActive = false
		return
	}
	b.failures = 0
	b.state = Closed
	b.probeActive = false
}

// expireStaleProbe resets a half-open probe that has been in flight longer
// than ProbeTimeout back to Open -- a hung probe (a caller whose fn call
// never reports back) must not leave the breaker offering its one probe
// slot to nobody, forever.
func (b *Breaker) expireStaleProbe() {
	if b.state == HalfOpen && b.probeActive && b.Now().Sub(b.probeStarted) >= b.ProbeTimeout {
		b.state = Open
		b.openedAt = b.Now()
		b.probeActive = false
	}
}

// State returns the breaker's current state.
func (b *Breaker) State() State { return b.state }
```

### The runnable demo

Two consecutive failures trip the breaker open. A third call is rejected
immediately — its `fn` would panic if it ran, proving it never does.
After the reset timeout elapses, the next call becomes the half-open
probe; it succeeds and closes the breaker, and a normal call afterward
runs `fn` as usual.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/circuit-breaker-half-open-probe"
)

func main() {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	clock := func() time.Time { return now }

	b := &breaker.Breaker{
		Now:              clock,
		FailureThreshold: 2,
		ResetTimeout:     10 * time.Second,
		ProbeTimeout:     5 * time.Second,
	}

	failing := errors.New("upstream unavailable")

	fmt.Println("call 1:", b.Call(func() error { return failing }), "state:", b.State())
	fmt.Println("call 2:", b.Call(func() error { return failing }), "state:", b.State())

	fmt.Println("call 3:", b.Call(func() error { panic("must not run while open") }), "state:", b.State())

	now = now.Add(11 * time.Second)
	fmt.Println("call 4 (probe, succeeds):", b.Call(func() error { return nil }), "state:", b.State())

	fmt.Println("call 5:", b.Call(func() error { return nil }), "state:", b.State())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
call 1: upstream unavailable state: closed
call 2: upstream unavailable state: open
call 3: breaker: circuit is open state: open
call 4 (probe, succeeds): <nil> state: closed
call 5: <nil> state: closed
```

### Tests

`TestCallTransitionsThroughOpenAndHalfOpen` drives the same closed →
open → half-open → closed sequence as the demo, as a sequential table
sharing one breaker and one mutable clock, checking both the returned
error and the resulting state after each call.
`TestStaleHalfOpenProbeExpiresEvenOnRejectedCall` manually places the
breaker into a half-open state with a probe that never reports back,
then confirms a *rejected* call — one where `fn` never runs — still
triggers the deferred expiry that resets the breaker to `Open`.

Create `breaker_test.go`:

```go
package breaker

import (
	"errors"
	"testing"
	"time"
)

func TestCallTransitionsThroughOpenAndHalfOpen(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	clock := func() time.Time { return now }

	b := &Breaker{Now: clock, FailureThreshold: 2, ResetTimeout: 10 * time.Second, ProbeTimeout: 5 * time.Second}
	failing := errors.New("upstream down")

	tests := []struct {
		name      string
		advance   time.Duration
		call      func() error
		wantErr   error
		wantState State
	}{
		{"closed: first failure tolerated", 0, func() error { return failing }, failing, Closed},
		{"closed: second failure trips open", 0, func() error { return failing }, failing, Open},
		{"open: rejected without running fn", 0, func() error { panic("must not run") }, ErrOpen, Open},
		{"half-open: probe after reset timeout succeeds", 11 * time.Second, func() error { return nil }, nil, Closed},
		{"closed: normal call runs fn", 0, func() error { return nil }, nil, Closed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now = now.Add(tc.advance)
			err := b.Call(tc.call)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if b.State() != tc.wantState {
				t.Errorf("state = %v, want %v", b.State(), tc.wantState)
			}
		})
	}
}

// TestStaleHalfOpenProbeExpiresEvenOnRejectedCall proves the deferred
// expireStaleProbe check runs on every exit path out of Call -- including
// the early return for a call that gets rejected because a probe is
// already in flight -- not only on the path where fn actually runs.
func TestStaleHalfOpenProbeExpiresEvenOnRejectedCall(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	b := &Breaker{Now: func() time.Time { return now }, FailureThreshold: 1, ResetTimeout: time.Second, ProbeTimeout: 2 * time.Second}

	// Simulate a probe that started and never reported back (its own
	// goroutine is still blocked inside fn somewhere).
	b.state = HalfOpen
	b.probeActive = true
	b.probeStarted = base

	now = base.Add(3 * time.Second) // past ProbeTimeout

	err := b.Call(func() error {
		t.Fatal("fn must not run: a probe was already in flight")
		return nil
	})
	if !errors.Is(err, ErrProbeInFlight) {
		t.Fatalf("err = %v, want ErrProbeInFlight", err)
	}
	if b.State() != Open {
		t.Fatalf("state = %v, want Open (deferred expiry should have reset the stale probe)", b.State())
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Call` is correct when a stuck half-open probe is never a permanent
condition: some later caller — even one that gets rejected outright
because it arrived while the stuck probe was still "in flight" — always
has the chance to notice the timeout and reopen the breaker for a fresh
probe. Deferring `expireStaleProbe` as the very first statement in
`Call` is what guarantees that check runs on every one of the function's
several early-return paths, not only the path where `fn` happens to run
to completion. The mistake this design avoids is placing the staleness
check only after a successful `fn` call, which would mean a genuinely
hung probe — the exact case the check exists for — is precisely the one
scenario where it would never run.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a deferred call registered first in a function runs, unconditionally, on every return path out of it.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the closed/open/half-open state machine this exercise implements.
- [Microsoft Azure Architecture Center: Circuit Breaker pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) — production guidance on half-open probe timeouts and reset behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-opentelemetry-style-active-span.md](28-opentelemetry-style-active-span.md) | Next: [30-feature-flag-rollout-rule-evaluation.md](30-feature-flag-rollout-rule-evaluation.md)
