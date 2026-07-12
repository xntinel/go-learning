# Exercise 1: Circuit Breaker

A circuit breaker is the data plane's first line of defence against a failing upstream: once a service is clearly down, the breaker stops sending it traffic instead of piling more load onto a service that cannot answer. This exercise builds the three-state machine — closed, open, half-open — with a consecutive-failure threshold, a cooldown, and a single-probe recovery path, all safe for concurrent use.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
circuit.go             State, CircuitBreaker, CircuitBreakerConfig, Allow, Record
cmd/
  demo/
    main.go            trip the circuit, reject while open, probe, recover
circuit_test.go        threshold trip, open rejection, success reset, half-open probe
```

- Files: `circuit.go`, `cmd/demo/main.go`, `circuit_test.go`.
- Implement: `CircuitBreaker` with `Allow() error`, `Record(success bool)`, and `CurrentState() State`, plus the `State` type and its `String` method.
- Test: `circuit_test.go` trips the circuit at the threshold, rejects while open, resets the failure count on success, and drives the full half-open probe in both the success and failure directions.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/06-traffic-management/01-circuit-breaker/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/06-traffic-management/01-circuit-breaker
go mod edit -go=1.26
```

### Why three states, and what each transition guarantees

A two-state breaker — closed or open — can stop traffic to a dead upstream, but it cannot decide on its own when to resume. Something external has to flip it back to closed, and whatever that something is has to guess whether the upstream has recovered. The half-open state removes the guess: after the cooldown the breaker itself sends exactly one probe and lets the result decide. A successful probe closes the circuit; a failed probe re-opens it and restarts the cooldown. The breaker is therefore self-healing, and the only tuning knobs are the failure threshold and the cooldown.

The failure counter in the closed state counts *consecutive* failures, and a single success resets it. This is the difference between "the upstream is having a bad moment" and "the upstream is down". An upstream that fails one request in fifty is healthy and the counter never approaches the threshold because each failure is followed by successes that zero it. An upstream that fails five times in a row has almost certainly fallen over, and that is the run the threshold is designed to catch.

The half-open state admits exactly one probe. This is enforced by a `halfOpenSent` flag rather than by counting, because the hazard is concurrency: if the breaker let every caller through the moment the cooldown elapsed, a busy proxy would slam the just-recovering upstream with its full backlog and knock it straight back down. `Allow` sets `halfOpenSent` to true for the one caller it admits and returns `ErrCircuitOpen` to everyone else until `Record` reports that probe's outcome. The state transitions run through a single `transitionLocked` helper so that the `openedAt` timestamp and the optional `StateChange` callback are updated in exactly one place and can never drift out of sync with the state field.

Create `circuit.go`:

```go
// Package circuit implements a three-state circuit breaker (closed, open,
// half-open) for guarding calls to a failing upstream. It is safe for
// concurrent use.
package circuit

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCircuitOpen is returned by Allow when the circuit refuses a request.
// Callers check identity with errors.Is.
var ErrCircuitOpen = errors.New("circuit breaker open")

// State is the circuit breaker state.
type State int

const (
	StateClosed   State = iota // normal; requests pass through
	StateOpen                  // failing; requests rejected immediately
	StateHalfOpen              // recovery probe; one request is allowed
)

// String returns the lowercase name of the state.
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

// CircuitBreakerConfig holds the tuning knobs for a CircuitBreaker.
type CircuitBreakerConfig struct {
	// Threshold is the number of consecutive failures before the circuit opens.
	// Default: 5.
	Threshold int
	// Cooldown is how long the circuit stays open before entering half-open.
	// Default: 30s.
	Cooldown time.Duration
}

// CircuitBreaker implements the closed -> open -> half-open state machine.
// It is safe for concurrent use.
type CircuitBreaker struct {
	mu           sync.Mutex
	state        State
	failures     int
	cfg          CircuitBreakerConfig
	openedAt     time.Time
	halfOpenSent bool
	// StateChange is called on every state transition. Nil is safe.
	StateChange func(from, to State)
}

// NewCircuitBreaker creates a CircuitBreaker. Zero fields are replaced with
// defaults (Threshold=5, Cooldown=30s).
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	return &CircuitBreaker{cfg: cfg}
}

// Allow returns nil if the request may be forwarded, or a wrapped ErrCircuitOpen.
//
// In StateOpen, requests are rejected until the cooldown elapses, at which
// point the circuit transitions to StateHalfOpen and the first caller gets nil.
// In StateHalfOpen, exactly one concurrent probe is allowed; subsequent callers
// get ErrCircuitOpen until Record is called.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return nil

	case StateOpen:
		if time.Since(cb.openedAt) >= cb.cfg.Cooldown {
			cb.transitionLocked(StateHalfOpen)
			cb.halfOpenSent = true
			return nil
		}
		return fmt.Errorf("%w", ErrCircuitOpen)

	case StateHalfOpen:
		if !cb.halfOpenSent {
			cb.halfOpenSent = true
			return nil
		}
		return fmt.Errorf("%w", ErrCircuitOpen)

	default:
		return fmt.Errorf("%w", ErrCircuitOpen)
	}
}

// Record records the outcome of a forwarded request.
//
//   - StateClosed + failure: increments failure count; trips to StateOpen at threshold.
//   - StateClosed + success: resets failure count.
//   - StateHalfOpen + success: resets to StateClosed.
//   - StateHalfOpen + failure: re-opens; probe window restarts after next cooldown.
//   - StateOpen: no-op (no request reached the upstream).
func (cb *CircuitBreaker) Record(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		if success {
			cb.failures = 0
			return
		}
		cb.failures++
		if cb.failures >= cb.cfg.Threshold {
			cb.transitionLocked(StateOpen)
		}

	case StateHalfOpen:
		cb.halfOpenSent = false
		if success {
			cb.failures = 0
			cb.transitionLocked(StateClosed)
		} else {
			cb.transitionLocked(StateOpen)
		}
	}
}

// CurrentState returns the current state without side effects.
func (cb *CircuitBreaker) CurrentState() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

func (cb *CircuitBreaker) transitionLocked(to State) {
	from := cb.state
	cb.state = to
	if to == StateOpen {
		cb.openedAt = time.Now()
	}
	if cb.StateChange != nil {
		cb.StateChange(from, to)
	}
}
```

`Allow` and `Record` are the two halves of one protocol: `Allow` decides whether a request may go out, `Record` feeds the result back so the state machine can advance. They must always be paired — every `Allow` that returns nil is followed by exactly one `Record` once the outcome is known. The `Open` branch of `Allow` is the only place that reads the clock: when the cooldown has elapsed it promotes the circuit to half-open and admits the caller in the same critical section, so two goroutines racing on the cooldown boundary cannot both become the probe.

### The runnable demo

The demo walks one circuit through its whole life: three failures trip it, a request while open is rejected without reaching any upstream, the cooldown elapses, a probe is admitted, and a successful probe closes the circuit again. The `StateChange` callback prints every transition so the sequence is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/circuit-breaker"
)

func main() {
	cb := circuit.NewCircuitBreaker(circuit.CircuitBreakerConfig{
		Threshold: 3,
		Cooldown:  5 * time.Millisecond,
	})
	cb.StateChange = func(from, to circuit.State) {
		fmt.Printf("transition: %s -> %s\n", from, to)
	}

	fmt.Println("start:", cb.CurrentState())

	cb.Record(false)
	cb.Record(false)
	fmt.Println("after 2 failures:", cb.CurrentState())

	cb.Record(false) // third failure trips the circuit
	fmt.Println("after 3 failures:", cb.CurrentState())

	if err := cb.Allow(); err != nil {
		fmt.Println("allow while open:", err)
	}

	time.Sleep(10 * time.Millisecond) // wait out the cooldown
	if err := cb.Allow(); err == nil {
		fmt.Println("probe admitted, state:", cb.CurrentState())
	}

	cb.Record(true) // successful probe closes the circuit
	fmt.Println("after successful probe:", cb.CurrentState())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: closed
after 2 failures: closed
transition: closed -> open
after 3 failures: open
allow while open: circuit breaker open
transition: open -> half-open
probe admitted, state: half-open
transition: half-open -> closed
after successful probe: closed
```

### Tests

The tests pin every transition. `TestCircuitBreakerTripsAtThreshold` checks the circuit stays closed below the threshold and opens exactly at it. `TestCircuitBreakerSuccessResetsFailures` proves a single success zeroes the consecutive-failure count. `TestCircuitBreakerHalfOpenProbeSucceeds` drives the full recovery path and asserts that a second, concurrent `Allow` during the probe is rejected; `TestCircuitBreakerHalfOpenProbeFailsReopens` checks the opposite outcome. These use a short cooldown so the test runs in milliseconds without flakiness.

Create `circuit_test.go`:

```go
package circuit

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCircuitBreakerStartsClosed(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 3, Cooldown: time.Hour})
	if got := cb.CurrentState(); got != StateClosed {
		t.Fatalf("initial state = %s, want closed", got)
	}
}

func TestCircuitBreakerTripsAtThreshold(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 3, Cooldown: time.Hour})
	cb.Record(false)
	cb.Record(false)
	if cb.CurrentState() != StateClosed {
		t.Fatal("should still be closed after 2 failures (threshold=3)")
	}
	cb.Record(false)
	if got := cb.CurrentState(); got != StateOpen {
		t.Fatalf("state = %s, want open after 3 failures", got)
	}
}

func TestCircuitBreakerRejectsWhenOpen(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1, Cooldown: time.Hour})
	cb.Record(false)
	if err := cb.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() = %v, want ErrCircuitOpen", err)
	}
}

func TestCircuitBreakerSuccessResetsFailures(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 3, Cooldown: time.Hour})
	cb.Record(false)
	cb.Record(false)
	cb.Record(true) // resets the count
	cb.Record(false)
	cb.Record(false)
	if cb.CurrentState() != StateClosed {
		t.Fatal("success should reset failure count; circuit should still be closed")
	}
}

func TestCircuitBreakerHalfOpenProbeSucceeds(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1, Cooldown: 5 * time.Millisecond})
	cb.Record(false) // trip to open
	time.Sleep(10 * time.Millisecond)

	// After cooldown: first Allow transitions to half-open and returns nil.
	if err := cb.Allow(); err != nil {
		t.Fatalf("first Allow after cooldown = %v, want nil (probe)", err)
	}
	if cb.CurrentState() != StateHalfOpen {
		t.Fatalf("state = %s, want half-open", cb.CurrentState())
	}

	// Concurrent Allow must be rejected while the probe is in flight.
	if err := cb.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("concurrent Allow during half-open = %v, want ErrCircuitOpen", err)
	}

	// Successful probe closes the circuit.
	cb.Record(true)
	if got := cb.CurrentState(); got != StateClosed {
		t.Fatalf("state after successful probe = %s, want closed", got)
	}
}

func TestCircuitBreakerHalfOpenProbeFailsReopens(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1, Cooldown: 5 * time.Millisecond})
	cb.Record(false)
	time.Sleep(10 * time.Millisecond)
	if err := cb.Allow(); err != nil { // probe admitted
		t.Fatalf("Allow after cooldown = %v, want nil", err)
	}
	cb.Record(false) // probe fails
	if got := cb.CurrentState(); got != StateOpen {
		t.Fatalf("state after failed probe = %s, want open", got)
	}
}

func TestCircuitBreakerStateChangeCallback(t *testing.T) {
	t.Parallel()

	var transitions []string
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1, Cooldown: time.Hour})
	cb.StateChange = func(from, to State) {
		transitions = append(transitions, fmt.Sprintf("%s->%s", from, to))
	}
	cb.Record(false)

	if len(transitions) != 1 || transitions[0] != "closed->open" {
		t.Fatalf("transitions = %v, want [closed->open]", transitions)
	}
}

func ExampleCircuitBreaker_Record() {
	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 2, Cooldown: time.Hour})
	fmt.Println(cb.CurrentState())
	cb.Record(false)
	fmt.Println(cb.CurrentState()) // still closed; 1 failure, threshold=2
	cb.Record(false)
	fmt.Println(cb.CurrentState()) // tripped; 2 consecutive failures
	// Output:
	// closed
	// closed
	// open
}
```

## Review

The breaker is correct when each transition is driven only by the events that should cause it. The most common error is tripping on a cumulative failure rate rather than a consecutive run: if `Record(true)` does not reset `failures` to zero, the circuit eventually opens on an upstream that is mostly healthy, because failures accumulate forever. The second error is admitting more than one probe in half-open; the `halfOpenSent` flag exists precisely to prevent a recovering upstream from being hit by the whole backlog at once, so a second concurrent `Allow` must return `ErrCircuitOpen`. The third is reading or mutating state outside the mutex — every method takes `cb.mu`, and the transition bookkeeping lives in the single `transitionLocked` helper so `openedAt` and the callback can never lag the state field. Running the suite under `go test -race` confirms there is no unsynchronised access.

## Resources

- [Circuit Breaker pattern (Martin Fowler)](https://martinfowler.com/bliki/CircuitBreaker.html) — the canonical description of the three-state model this exercise implements.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the lock that makes the state machine safe for concurrent callers.
- [errors.Is](https://pkg.go.dev/errors#Is) — the identity check callers use to recognise `ErrCircuitOpen`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retry-backoff.md](02-retry-backoff.md)
