# Exercise 16: Circuit Breaker With Validated State Transitions

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A circuit breaker has three states — `Closed`, `Open`, `HalfOpen` — and two
tuning knobs that decide how it moves between them: a failure threshold that
trips it, and a timeout that lets a trial request back through. This module
builds the breaker with options, then guards the one state transition that
must never happen from the wrong place: forcing a half-open probe is only
ever valid starting from `Open`.

## What you'll build

```text
circuitbreaker/                  independent module: example.com/circuitbreaker
  go.mod                         go 1.24
  circuitbreaker.go              State, Breaker, Option, New, WithFailureThreshold,
                                  WithSuccessThreshold, WithOpenTimeout, WithClock,
                                  Allow, RecordSuccess, RecordFailure, ForceHalfOpen
  cmd/
    demo/
      main.go                    manual clock drives trip, timeout probe, and forced reset
  circuitbreaker_test.go          table test over options, state transitions, -race concurrency
```

- Files: `circuitbreaker.go`, `cmd/demo/main.go`, `circuitbreaker_test.go`.
- Implement: `New(opts ...Option) (*Breaker, error)` whose `Allow`/`RecordSuccess`/`RecordFailure` drive `Closed -> Open -> HalfOpen -> Closed`, and whose `ForceHalfOpen` rejects any state but `Open`.
- Test: option validation, the full state-transition sequence with an injected clock, `ForceHalfOpen` from every state, and a `-race` concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two knobs, one injected clock

`WithFailureThreshold` and `WithSuccessThreshold` count consecutive
outcomes; `WithOpenTimeout` bounds how long the breaker stays `Open` before
`Allow` lets a single trial request through as `HalfOpen`. That timeout is
measured against an injected clock rather than `time.Now`, for the same
reason it is everywhere else in this chapter: a test that must wait a real
30 seconds to prove a timeout fires is a test nobody runs. The demo and the
tests both advance a frozen clock by reassigning a captured variable instead
of sleeping.

### Why `ForceHalfOpen` checks the state, not the caller's intent

An operator escape hatch that resets a breaker to `HalfOpen` is a real
production need — someone who knows the downstream is healthy again
shouldn't have to wait out the full timeout. But that reset is only
meaningful coming from `Open`: forcing it from `Closed` would let a probe
bypass the failure threshold entirely (the breaker was never tripped), and
forcing it from `HalfOpen` is already where it is, so the operation would
only zero the success counter unexpectedly and lie about what changed.
`ForceHalfOpen` is the one method in this module whose entire job is
enforcing that a transition is valid only from a specific state — every
other state change happens automatically as a side effect of `Allow`,
`RecordSuccess`, or `RecordFailure`.

Create `circuitbreaker.go`:

```go
package circuitbreaker

import (
	"fmt"
	"sync"
	"time"
)

// State is one of the three circuit-breaker states.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "Closed"
	case Open:
		return "Open"
	case HalfOpen:
		return "HalfOpen"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Breaker is a concurrency-safe circuit breaker with a manual half-open
// escape hatch that is only ever valid from the Open state.
type Breaker struct {
	mu               sync.Mutex
	failureThreshold int
	successThreshold int
	openTimeout      time.Duration
	now              func() time.Time

	state     State
	failures  int
	successes int
	openedAt  time.Time
}

// Option configures a Breaker and may reject invalid input.
type Option func(*Breaker) error

// New seeds coherent defaults and applies opts in order.
func New(opts ...Option) (*Breaker, error) {
	b := &Breaker{
		failureThreshold: 5,
		successThreshold: 2,
		openTimeout:      30 * time.Second,
		now:              time.Now,
		state:            Closed,
	}
	for _, opt := range opts {
		if err := opt(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// WithFailureThreshold sets how many consecutive failures trip Closed to Open.
func WithFailureThreshold(n int) Option {
	return func(b *Breaker) error {
		if n < 1 {
			return fmt.Errorf("failure threshold must be >= 1, got %d", n)
		}
		b.failureThreshold = n
		return nil
	}
}

// WithSuccessThreshold sets how many consecutive successes in HalfOpen close
// the breaker again.
func WithSuccessThreshold(n int) Option {
	return func(b *Breaker) error {
		if n < 1 {
			return fmt.Errorf("success threshold must be >= 1, got %d", n)
		}
		b.successThreshold = n
		return nil
	}
}

// WithOpenTimeout sets how long the breaker stays Open before Allow will let
// a single trial request through as HalfOpen.
func WithOpenTimeout(d time.Duration) Option {
	return func(b *Breaker) error {
		if d <= 0 {
			return fmt.Errorf("open timeout must be positive, got %s", d)
		}
		b.openTimeout = d
		return nil
	}
}

// WithClock injects the clock used to time the Open->HalfOpen transition.
func WithClock(now func() time.Time) Option {
	return func(b *Breaker) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		b.now = now
		return nil
	}
}

// State reports the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a request may proceed. In Open it also performs the
// timeout-driven transition to HalfOpen once openTimeout has elapsed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) >= b.openTimeout {
			b.state = HalfOpen
			b.successes = 0
			return true
		}
		return false
	default: // HalfOpen
		return true
	}
}

// RecordSuccess reports a successful call. In HalfOpen enough consecutive
// successes close the breaker; in Closed it resets the failure count.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		b.successes++
		if b.successes >= b.successThreshold {
			b.state = Closed
			b.failures = 0
			b.successes = 0
		}
	case Closed:
		b.failures = 0
	}
}

// RecordFailure reports a failed call. In Closed enough consecutive failures
// trip to Open; any failure in HalfOpen reopens immediately.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		b.failures++
		if b.failures >= b.failureThreshold {
			b.state = Open
			b.openedAt = b.now()
			b.failures = 0
		}
	case HalfOpen:
		b.state = Open
		b.openedAt = b.now()
		b.failures = 0
		b.successes = 0
	}
}

// ForceHalfOpen manually resets the breaker to HalfOpen, for operator
// intervention. This transition is only valid from Open: forcing a probe
// while Closed would bypass the failure threshold entirely, and forcing it
// while already HalfOpen is a no-op that would only reset the success
// counter unexpectedly.
func (b *Breaker) ForceHalfOpen() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != Open {
		return fmt.Errorf("cannot force half-open from state %s, only from Open", b.state)
	}
	b.state = HalfOpen
	b.successes = 0
	return nil
}
```

### The runnable demo

The demo trips the breaker with three failures, shows `Allow` refusing
requests immediately after, advances a manual clock past the timeout to let
one trial request through, and shows `ForceHalfOpen` rejecting a call once
the breaker is already `HalfOpen`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/circuitbreaker"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(3),
		circuitbreaker.WithSuccessThreshold(2),
		circuitbreaker.WithOpenTimeout(10*time.Second),
		circuitbreaker.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	fmt.Printf("state after 3 failures: %s\n", b.State())
	fmt.Printf("allow immediately after trip: %t\n", b.Allow())

	current = current.Add(11 * time.Second)
	fmt.Printf("allow after timeout: %t\n", b.Allow())
	fmt.Printf("state after timeout probe: %s\n", b.State())

	if err := b.ForceHalfOpen(); err != nil {
		fmt.Printf("force half-open from HalfOpen: %v\n", err)
	}

	b.RecordSuccess()
	b.RecordSuccess()
	fmt.Printf("state after 2 successes: %s\n", b.State())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
state after 3 failures: Open
allow immediately after trip: false
allow after timeout: true
state after timeout probe: HalfOpen
force half-open from HalfOpen: cannot force half-open from state HalfOpen, only from Open
state after 2 successes: Closed
```

### Tests

`TestNewValidation` tables the option-level checks. `TestStateTransitions`
drives the full `Closed -> Open -> HalfOpen -> Open` sequence with a fake
clock, proving the timeout gate and that any `HalfOpen` failure reopens
immediately. `TestForceHalfOpenOnlyFromOpen` proves the transition succeeds
only from `Open` and fails from both `Closed` and `HalfOpen`.
`TestConcurrentAccess` runs `-race` over concurrent successes, failures, and
reads.

Create `circuitbreaker_test.go`:

```go
package circuitbreaker

import (
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "invalid failure threshold", opts: []Option{WithFailureThreshold(0)}, wantErr: true},
		{name: "invalid success threshold", opts: []Option{WithSuccessThreshold(0)}, wantErr: true},
		{name: "invalid open timeout", opts: []Option{WithOpenTimeout(0)}, wantErr: true},
		{name: "nil clock rejected", opts: []Option{WithClock(nil)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestStateTransitions(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	b, err := New(
		WithFailureThreshold(2),
		WithSuccessThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if b.State() != Closed {
		t.Fatalf("initial state = %s, want Closed", b.State())
	}

	b.RecordFailure()
	if b.State() != Closed {
		t.Fatalf("state after 1 failure = %s, want Closed", b.State())
	}
	b.RecordFailure()
	if b.State() != Open {
		t.Fatalf("state after 2 failures = %s, want Open", b.State())
	}

	if b.Allow() {
		t.Fatal("Allow() = true before openTimeout elapsed, want false")
	}

	current = base.Add(6 * time.Second)
	if !b.Allow() {
		t.Fatal("Allow() = false after openTimeout elapsed, want true")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state after timeout probe = %s, want HalfOpen", b.State())
	}

	b.RecordFailure() // any failure in HalfOpen reopens
	if b.State() != Open {
		t.Fatalf("state after HalfOpen failure = %s, want Open", b.State())
	}
}

func TestForceHalfOpenOnlyFromOpen(t *testing.T) {
	t.Parallel()

	b, err := New(WithFailureThreshold(1))
	if err != nil {
		t.Fatal(err)
	}

	if err := b.ForceHalfOpen(); err == nil {
		t.Fatal("expected error forcing half-open from Closed, got nil")
	}

	b.RecordFailure() // Closed -> Open
	if b.State() != Open {
		t.Fatalf("state = %s, want Open", b.State())
	}

	if err := b.ForceHalfOpen(); err != nil {
		t.Fatalf("unexpected error forcing half-open from Open: %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("state = %s, want HalfOpen", b.State())
	}

	if err := b.ForceHalfOpen(); err == nil {
		t.Fatal("expected error forcing half-open from HalfOpen, got nil")
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	b, err := New(WithFailureThreshold(50), WithSuccessThreshold(50))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				b.RecordSuccess()
			} else {
				b.RecordFailure()
			}
			b.Allow()
			_ = b.State()
		}(i)
	}
	wg.Wait()
}
```

## Review

The breaker is correct when every automatic transition follows from a
recorded outcome or an elapsed timeout, and when the one manual transition —
`ForceHalfOpen` — refuses to run from anywhere but `Open`. The instructive
case is `TestForceHalfOpenOnlyFromOpen`, which checks both the state where
the call must fail (`Closed`, where it would bypass the failure threshold)
and the state where it is already true (`HalfOpen`, where it would just be a
confusing no-op). Every state read and write goes through the same mutex,
including inside `Allow`'s timeout check, which is why `-race` passes even
though `RecordSuccess`, `RecordFailure`, and `Allow` all mutate the same
state concurrently.

## Resources

- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [AWS Builders' Library: retries and circuit breakers](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-distributed-trace-span-options.md](15-distributed-trace-span-options.md) | Next: [17-sliding-window-rate-limiter.md](17-sliding-window-rate-limiter.md)
