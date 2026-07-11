# 14. Circuit Breaker Pattern: Stop Calling A Failing Service

A circuit breaker sits between a caller and a remote service. When the remote
fails often enough, the breaker "trips" and subsequent calls fail fast without
touching the remote. After a cooldown, the breaker enters a "half-open" state
where the next call is a probe; success closes the breaker, failure re-trips
it. The pattern is from Michael Nygard's "Release It!" and is implemented in
`sony/gobreaker`, `hystrix-go`, and many service meshes.

This lesson builds the pattern from `sync/atomic` primitives so the test runs
hermetically.

```text
circuitbreaker/
  go.mod
  internal/circuitbreaker/circuitbreaker.go
  internal/circuitbreaker/circuitbreaker_test.go
  cmd/circuitbreakerdemo/main.go
```

The package exposes `Breaker` with three states (`closed`, `open`,
`half-open`), sentinel errors for each, and an `Execute` method that runs a
user-supplied function. Tests pin the closed-to-open transition, the
half-open probe, and the race-free state machine.

## Concepts

### Three States

`closed` is normal: every call passes through. After `MaxFailures` consecutive
failures, the breaker transitions to `open`. In `open`, every call fails fast
with `ErrOpen` until `Cooldown` elapses, after which the next call is a probe
and the breaker enters `half-open`. In `half-open`, the next call's outcome
determines whether the breaker returns to `closed` (success) or `open`
(failure).

### State Transitions Are Atomic

The state field is read by every `Execute` call and written on every
transition. A torn read would let two callers observe different states for
the same logical instant. The lesson uses `atomic.Uint32` with packed state
plus counters, or — equivalently — a `sync.Mutex`. The lesson picks
`sync.Mutex` for clarity; both are correct.

### Failure Counting Resets On Success

When the breaker is `closed` and a call succeeds, the failure counter resets
to zero. The breaker only trips after a string of consecutive failures, not
on cumulative lifetime failures. This is the property that makes it useful:
transient blips don't trip the breaker.

### Half-Open Allows Exactly One Probe

If two callers race into `half-open`, both fire probes; the first to return
toggles the state, but the second's probe runs against a now-`open` breaker.
The lesson's `Execute` guards the half-open transition with the mutex so
only one probe is in flight at a time.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/circuitbreaker/internal/circuitbreaker ~/go-exercises/circuitbreaker/cmd/circuitbreakerdemo
cd ~/go-exercises/circuitbreaker
go mod init example.com/circuitbreaker
```

### Exercise 1: Sentinel Errors And State

Create `internal/circuitbreaker/circuitbreaker.go`:

```go
package circuitbreaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrOpen         = errors.New("circuit breaker open")
	ErrTooManyCalls = errors.New("circuit breaker half-open: too many calls")
)

type State int

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

type Config struct {
	MaxFailures   int
	Cooldown      time.Duration
	OnStateChange func(name string, from, to State)
}

func (c Config) withDefaults() Config {
	if c.MaxFailures <= 0 {
		c.MaxFailures = 5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 100 * time.Millisecond
	}
	return c
}

type Breaker struct {
	name string
	cfg  Config

	mu               sync.Mutex
	state            State
	failures         int
	openedAt         time.Time
	halfOpenInFlight bool
}

func New(name string, cfg Config) *Breaker {
	return &Breaker{name: name, cfg: cfg.withDefaults(), state: StateClosed}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Execute runs fn under the breaker. If the breaker is open and the cooldown
// has not elapsed, it returns ErrOpen. If the breaker is open and the
// cooldown has elapsed, the next call is allowed as a half-open probe. If the
// breaker is half-open and a probe is already in flight, ErrTooManyCalls is
// returned.
func (b *Breaker) Execute(fn func() error) error {
	if err := b.beforeCall(); err != nil {
		return err
	}
	err := fn()
	b.afterCall(err)
	return err
}

func (b *Breaker) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(b.openedAt) < b.cfg.Cooldown {
			return fmt.Errorf("%s: %w", b.name, ErrOpen)
		}
		b.transition(StateHalfOpen)
		b.halfOpenInFlight = true
		return nil
	case StateHalfOpen:
		if b.halfOpenInFlight {
			return fmt.Errorf("%s: %w", b.name, ErrTooManyCalls)
		}
		b.halfOpenInFlight = true
		return nil
	}
	return nil
}

func (b *Breaker) afterCall(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		if err == nil {
			b.failures = 0
		} else {
			b.failures++
			if b.failures >= b.cfg.MaxFailures {
				b.openedAt = time.Now()
				b.transition(StateOpen)
			}
		}
	case StateHalfOpen:
		b.halfOpenInFlight = false
		if err == nil {
			b.failures = 0
			b.transition(StateClosed)
		} else {
			b.openedAt = time.Now()
			b.transition(StateOpen)
		}
	}
}

func (b *Breaker) transition(to State) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	if b.cfg.OnStateChange != nil {
		b.cfg.OnStateChange(b.name, from, to)
	}
}
```

The state machine is mutex-guarded. `beforeCall` either allows the call,
returns `ErrOpen`, or returns `ErrTooManyCalls`. `afterCall` updates the
state based on the outcome. `transition` calls `OnStateChange` if set so the
caller can log state changes.

### Exercise 2: Test The Contract

Create `internal/circuitbreaker/circuitbreaker_test.go`:

```go
package circuitbreaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClosedBreakerPassesCallsThrough(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 3, Cooldown: time.Second})

	for i := 0; i < 10; i++ {
		if err := b.Execute(func() error { return nil }); err != nil {
			t.Fatalf("call %d: err = %v, want nil", i, err)
		}
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s, want closed", got)
	}
}

func TestBreakerTripsAfterMaxFailures(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 3, Cooldown: time.Second})

	for i := 0; i < 3; i++ {
		_ = b.Execute(func() error { return errors.New("boom") })
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s, want open", got)
	}

	err := b.Execute(func() error { return nil })
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Execute after open: err = %v, want ErrOpen", err)
	}
}

func TestBreakerSuccessResetsFailures(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 3, Cooldown: time.Second})

	_ = b.Execute(func() error { return errors.New("boom") })
	_ = b.Execute(func() error { return errors.New("boom") })
	if err := b.Execute(func() error { return nil }); err != nil {
		t.Fatalf("call 3: err = %v, want nil", err)
	}

	_ = b.Execute(func() error { return errors.New("boom") })
	_ = b.Execute(func() error { return errors.New("boom") })
	if got := b.State(); got != StateClosed {
		t.Fatalf("state = %s after reset, want closed", got)
	}
}

func TestBreakerHalfOpenProbeSuccessClosesBreaker(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 2, Cooldown: 30 * time.Millisecond})

	_ = b.Execute(func() error { return errors.New("boom") })
	_ = b.Execute(func() error { return errors.New("boom") })
	if got := b.State(); got != StateOpen {
		t.Fatalf("state = %s, want open", got)
	}

	time.Sleep(40 * time.Millisecond)

	if err := b.Execute(func() error { return nil }); err != nil {
		t.Fatalf("probe: err = %v, want nil", err)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state after probe success = %s, want closed", got)
	}
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 2, Cooldown: 30 * time.Millisecond})

	_ = b.Execute(func() error { return errors.New("boom") })
	_ = b.Execute(func() error { return errors.New("boom") })

	time.Sleep(40 * time.Millisecond)

	if err := b.Execute(func() error { return errors.New("still broken") }); err == nil {
		t.Fatal("probe failure: err = nil, want error")
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("state after probe failure = %s, want open", got)
	}
}

func TestBreakerHalfOpenAllowsOneProbeAtATime(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 1, Cooldown: 10 * time.Millisecond})

	_ = b.Execute(func() error { return errors.New("boom") })
	time.Sleep(20 * time.Millisecond)

	release := make(chan struct{})
	var inFlight atomic.Int32
	var maxObserved atomic.Int32

	var wg sync.WaitGroup
	wg.Add(2)

	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			err := b.Execute(func() error {
				cur := inFlight.Add(1)
				for {
					prev := maxObserved.Load()
					if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
						break
					}
				}
				<-release
				return nil
			})
			if err != nil && !errors.Is(err, ErrTooManyCalls) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := maxObserved.Load(); got > 1 {
		t.Fatalf("max concurrent probes = %d, want 1", got)
	}
}

func TestBreakerOnStateChangeCallback(t *testing.T) {
	t.Parallel()

	var transitions []string
	b := New("test", Config{
		MaxFailures: 1,
		Cooldown:    time.Second,
		OnStateChange: func(name string, from, to State) {
			transitions = append(transitions, name+":"+from.String()+"->"+to.String())
		},
	})

	_ = b.Execute(func() error { return errors.New("boom") })

	want := []string{"test:closed->open"}
	if len(transitions) != 1 || transitions[0] != want[0] {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
}

func TestBreakerIsRaceFree(t *testing.T) {
	t.Parallel()

	b := New("test", Config{MaxFailures: 5, Cooldown: time.Millisecond})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = b.Execute(func() error {
				if i%5 == 0 {
					return errors.New("boom")
				}
				return nil
			})
		}(i)
	}
	wg.Wait()
}
```

`TestBreakerHalfOpenAllowsOneProbeAtATime` is the test that pins the
single-probe contract. Two goroutines race into half-open; the breaker
allows the first, returns `ErrTooManyCalls` to the second. The race
detector finds any torn read or write on the in-flight flag.

Your turn: add `TestBreakerDefaultsAreSensible` that constructs a breaker
with `Config{}` and asserts `MaxFailures == 5` and `Cooldown ==
100*time.Millisecond` (use the state to verify: 5 failures trip it, then
sleep 100ms and a successful probe closes it).

### Exercise 3: Runnable Demo

Create `cmd/circuitbreakerdemo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/circuitbreaker/internal/circuitbreaker"
)

func main() {
	b := circuitbreaker.New("downstream", circuitbreaker.Config{
		MaxFailures: 3,
		Cooldown:    200 * time.Millisecond,
		OnStateChange: func(name string, from, to circuitbreaker.State) {
			fmt.Printf("[%s] %s -> %s\n", name, from, to)
		},
	})

	for i := 0; i < 8; i++ {
		err := b.Execute(func() error {
			if i < 4 {
				return errors.New("downstream is down")
			}
			return nil
		})
		fmt.Printf("call %d: %v (state=%s)\n", i, err, b.State())
		if b.State() == circuitbreaker.StateOpen {
			time.Sleep(220 * time.Millisecond)
		}
	}
}
```

## Common Mistakes

### Counting Cumulative Failures

Wrong: increment a counter on every failure forever.

What happens: a long-lived breaker accumulates failures and trips even when
the recent success rate is high. Transient blips become permanent outages.

Fix: reset the counter to zero on every success. The breaker trips on
consecutive failures only.

### Allowing Concurrent Half-Open Probes

Wrong: read state in `beforeCall`, write state in `afterCall`, no mutex.

What happens: two probes race; both observe `half-open`, both run the call;
both outcomes update state.

Fix: hold the mutex across both `beforeCall` and `afterCall` (or use a
`halfOpenInFlight` flag guarded by the mutex) so only one probe is in
flight.

### Skipping The Half-Open State

Wrong: jump straight from `open` to `closed` after the cooldown.

What happens: the breaker allows all calls again before knowing whether the
downstream recovered. A flood of failing calls can re-trip the breaker, but
the breaker never proved recovery.

Fix: half-open allows exactly one probe. Success closes; failure re-opens.

### Mutating State Outside The Mutex

Wrong: read `b.state` without locking.

What happens: the race detector reports a data race; the test fails under
`-race`.

Fix: every state read and write goes through `b.mu`. `State()` is a method
that locks; the demo reads `b.State()` after `Execute` returns.

## Verification

From `~/go-exercises/circuitbreaker`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is essential: state transitions are
concurrent, and any unsynchronised access is a real bug, not a flaky test.

## Summary

- A circuit breaker protects a remote from a flood of failing calls.
- Three states: closed (normal), open (fail fast), half-open (probe).
- `MaxFailures` consecutive failures trip the breaker; success resets the
  counter.
- Half-open allows exactly one probe; success closes, failure re-opens.
- All state transitions go through a mutex; the race detector pins this.

## What's Next

Next: [Bounded Parallelism](../15-bounded-parallelism/15-bounded-parallelism.md) (already gold, skipped).

## Resources

- [Michael Nygard, Release It!](https://pragprog.com/titles/mnee2/release-it-second-edition/)
- [sony/gobreaker](https://github.com/sony/gobreaker)
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html)