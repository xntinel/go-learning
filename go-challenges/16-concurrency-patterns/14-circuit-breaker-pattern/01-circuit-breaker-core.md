# Exercise 1: Circuit Breaker Core (Consecutive Failures)

A circuit breaker is a three-state machine that sits between a caller and a failing dependency and turns a slow, resource-consuming failure into a fast, local one. This exercise builds the baseline: a breaker that trips after a run of consecutive failures, fails fast while open, admits exactly one half-open probe after a cooldown, and guards every state transition under a mutex so the `-race` detector stays quiet. It is the foundation the later exercises generalize.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
breaker.go            State, Config, Breaker, New, Execute, ErrOpen, ErrTooManyCalls
cmd/
  demo/
    main.go           drive one breaker through closed -> open -> half-open -> closed
breaker_test.go       pass-through, trip on MaxFailures, success-resets, half-open probe,
                      single-probe race, OnStateChange, sensible defaults, race-freedom
```

- Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
- Implement: `Breaker` with `Execute(fn func() error) error` and `State() State`, the `New` constructor, the `Config` with `MaxFailures`/`Cooldown`/`OnStateChange`, and the sentinels `ErrOpen` and `ErrTooManyCalls`.
- Test: `breaker_test.go` pins the closed-to-open transition, the success reset, the half-open probe in both directions, the single-probe contract under concurrency, the callback, the defaults, and race-freedom.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p circuit-breaker-core/cmd/demo && cd circuit-breaker-core
go mod init example.com/circuit-breaker-core
```

### Why consecutive failures, and why the mutex spans the whole call

The trip policy here is the simplest one that is safe: count failures that happen back to back, and reset that count to zero the instant any call succeeds. The reset is the whole point. A dependency that hiccups once every few seconds but mostly works should never trip the breaker, and resetting on success guarantees it cannot — only an uninterrupted run of `MaxFailures` failures, with no success between them, trips the circuit. A counter that merely accumulated lifetime failures would eventually trip a perfectly healthy dependency, which is the canonical beginner mistake.

The harder design decision is concurrency. The breaker's state, its failure counter, its `openedAt` timestamp, and its half-open in-flight flag are all read on the way into a call and written on the way out, from every goroutine at once. The implementation splits each `Execute` into two locked phases. `beforeCall` is the admission check: it reads the state under the lock and decides whether to let the call through, reject it with `ErrOpen`, or reject it with `ErrTooManyCalls`. The function then runs *outside* the lock — a breaker must never hold its mutex across a slow network call. `afterCall` re-acquires the lock and records the outcome: reset or increment the failure count, trip the breaker, or resolve a half-open probe. Because the state is only ever touched inside one of these two locked phases, there is no path that reads or writes a breaker field without the mutex, which is exactly what keeps the `-race` build clean.

The half-open transition is the subtle one. When the breaker is open and the cooldown has elapsed, `beforeCall` flips the state to half-open and sets `halfOpenInFlight` *before releasing the lock*, so the very next goroutine to arrive sees the flag already set and is turned away with `ErrTooManyCalls`. Exactly one probe is ever in flight. When that probe returns, `afterCall` clears the flag and either closes the breaker (probe succeeded) or reopens it and restarts the cooldown (probe failed). Note that `afterCall` only ever sees the closed or half-open state: when `beforeCall` rejects a call it returns an error and `Execute` never runs the function or reaches `afterCall`, so there is no open case to handle on the way out.

Create `breaker.go`:

```go
package circuitbreaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors returned by Execute when a call is not forwarded to fn.
var (
	// ErrOpen is returned while the breaker is open and the cooldown has not
	// elapsed: the call fails fast without touching the dependency.
	ErrOpen = errors.New("circuit breaker open")
	// ErrTooManyCalls is returned in the half-open state when a probe is already
	// in flight: only one probe is admitted at a time.
	ErrTooManyCalls = errors.New("circuit breaker half-open: too many calls")
)

// State is the current state of the breaker's machine.
type State int

const (
	StateClosed State = iota // normal: calls pass through
	StateOpen                // tripped: calls fail fast
	StateHalfOpen            // probing: exactly one call admitted
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

// Config tunes a Breaker. Zero values are replaced with sensible defaults.
type Config struct {
	// MaxFailures is the number of consecutive failures that trips the breaker.
	MaxFailures int
	// Cooldown is how long the breaker stays open before admitting a probe.
	Cooldown time.Duration
	// OnStateChange, if set, is called on every state transition.
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

// Breaker is a consecutive-failure circuit breaker. The zero value is not
// usable; construct one with New.
type Breaker struct {
	name string
	cfg  Config

	mu               sync.Mutex
	state            State
	failures         int
	openedAt         time.Time
	halfOpenInFlight bool
}

// New returns a closed breaker with the given name and configuration.
func New(name string, cfg Config) *Breaker {
	return &Breaker{name: name, cfg: cfg.withDefaults(), state: StateClosed}
}

// State returns the breaker's current state. It locks, so it is safe to call
// concurrently with Execute.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Execute runs fn under the breaker. If the breaker is open and the cooldown
// has not elapsed it returns ErrOpen without calling fn. If the breaker is
// open and the cooldown has elapsed, this call is admitted as the half-open
// probe. If the breaker is half-open and a probe is already in flight it
// returns ErrTooManyCalls. Otherwise fn runs and its outcome updates the state.
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

// transition sets the state and fires OnStateChange. The caller must hold b.mu.
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

The state machine is entirely mutex-guarded. `beforeCall` returns one of three things: nil (admit), `ErrOpen` (fail fast), or `ErrTooManyCalls` (probe in flight). `afterCall` records the outcome. `transition` centralizes the state write and the optional callback, and it is a no-op when the state does not actually change so a self-transition never fires a spurious notification.

### The runnable demo

A test proves a property in the abstract; a demo makes the machine concrete. This one wires an `OnStateChange` callback that prints every transition, then drives the breaker through a downstream that fails its first four calls and recovers after. Watch the trace: three failures trip it closed-to-open, the probe after the cooldown still fails (half-open back to open), and the next probe succeeds (half-open to closed). The breaker uses a real cooldown, so the demo sleeps past it between probes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/circuit-breaker-core"
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

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 0: downstream is down (state=closed)
call 1: downstream is down (state=closed)
[downstream] closed -> open
call 2: downstream is down (state=open)
[downstream] open -> half-open
[downstream] half-open -> open
call 3: downstream is down (state=open)
[downstream] open -> half-open
[downstream] half-open -> closed
call 4: <nil> (state=closed)
call 5: <nil> (state=closed)
call 6: <nil> (state=closed)
call 7: <nil> (state=closed)
```

### Tests

The tests pin each edge of the machine. Pass-through confirms a closed breaker forwards every call. The trip test drives `MaxFailures` failures and asserts the next call is rejected with `ErrOpen`. The reset test interleaves a success and confirms the count restarts. The two half-open tests cover both probe outcomes. The single-probe test is the important one: two goroutines race into half-open, and the breaker must admit exactly one, rejecting the other with `ErrTooManyCalls`, with the `-race` detector watching the in-flight flag. The defaults test verifies the zero-config breaker trips at five failures and half-opens after 100ms. The final test floods the breaker from a hundred goroutines purely to give `-race` something to find.

Create `breaker_test.go`:

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

	want := "test:closed->open"
	if len(transitions) != 1 || transitions[0] != want {
		t.Fatalf("transitions = %v, want [%s]", transitions, want)
	}
}

func TestBreakerDefaultsAreSensible(t *testing.T) {
	t.Parallel()

	b := New("test", Config{}) // MaxFailures defaults to 5, Cooldown to 100ms.

	for i := 0; i < 5; i++ {
		_ = b.Execute(func() error { return errors.New("boom") })
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("after 5 failures: state = %s, want open (MaxFailures default 5)", got)
	}

	time.Sleep(110 * time.Millisecond) // past the default 100ms cooldown.

	if err := b.Execute(func() error { return nil }); err != nil {
		t.Fatalf("probe after default cooldown: err = %v, want nil", err)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state after successful probe = %s, want closed", got)
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

## Review

The breaker is correct when three properties hold together. First, it trips only on consecutive failures: a success anywhere in the run resets the counter, so transient blips never accumulate into a trip — the success-reset test pins this, and the cumulative-counter design is the mistake it guards against. Second, the half-open state admits exactly one probe: the in-flight flag is set under the lock before `beforeCall` returns, so a second racing caller is rejected with `ErrTooManyCalls`, and the single-probe test confirms `maxObserved` never exceeds one even under `-race`. Third, no breaker field is ever touched outside `b.mu` — including `State()`, which locks rather than peeking — which is why the hundred-goroutine flood finds nothing.

Common mistakes for this feature. Holding the mutex across `fn()` would serialize every call through the breaker and let one slow dependency call block all others; the two-phase `beforeCall`/`afterCall` split runs the function unlocked precisely to avoid that. Forgetting to set `halfOpenInFlight` inside `beforeCall` (setting it in `afterCall` instead) opens a window where two callers both pass the admission check and both probe. And reading `b.state` for a metric or log line without locking is a real data race the detector will report, not a harmless peek.

## Resources

- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the canonical write-up of the three-state machine and the half-open probe.
- [sony/gobreaker](https://github.com/sony/gobreaker) — a widely used Go implementation; compare its `Settings`, `ReadyToTrip`, and state machine against this one.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the primitive that guards every state transition here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-rate-window-breaker.md](02-rate-window-breaker.md)
