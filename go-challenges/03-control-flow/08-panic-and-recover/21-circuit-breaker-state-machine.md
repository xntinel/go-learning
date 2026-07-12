# Exercise 21: Circuit Breaker State Transitions with Panic-Safe Rollback

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A circuit breaker's whole job is to protect the rest of the system from a
failing dependency, which means the breaker itself must never be the thing
that crashes when it probes that dependency's health. Moving from `Open` to
`HalfOpen` to run a health check is a moment of genuine risk: the check
might call into a client library with its own bugs, and if it panics
mid-transition, the breaker cannot be left showing `HalfOpen` to every other
caller — that would mean traffic gets let through against a dependency the
breaker never actually confirmed was healthy. This module builds `Breaker`,
which performs that transition under a mutex, recovers a panicking health
check with full context logged, and atomically rolls back to the last
stable state instead of getting stuck. It is fully self-contained: its own
module, demo, and tests.

## What you'll build

```text
circuitbreaker/              independent module: example.com/circuitbreaker
  go.mod                     go 1.24
  circuitbreaker.go           State, HealthCheck, Breaker, New, AttemptTransition
  cmd/
    demo/
      main.go                runnable demo: a panicking probe, then a healthy one
  circuitbreaker_test.go       table of transition outcomes + concurrent probing
```

Files: `circuitbreaker.go`, `cmd/demo/main.go`, `circuitbreaker_test.go`.
Implement: `Breaker.AttemptTransition(check HealthCheck) (finalState State, panicked bool)` that is a no-op unless the breaker is `Open`, otherwise moves to `HalfOpen`, runs `check` under a mutex-protected `defer`/`recover`, and rolls back to `Open` on either a failing check or a panicking one, closing to `Closed` only on a clean, successful check.
Test: a table covering already-`Closed` (no-op), `Open` with a healthy check (closes), `Open` with a failing check (rolls back, not panicked), `Open` with a check panicking an error value (rolls back, panicked), and `Open` with a check panicking a string value (rolls back, panicked); a concurrency test firing 50 simultaneous `AttemptTransition` calls (half panicking) against one breaker and asserting the final state is never observably stuck at `HalfOpen`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/21-circuit-breaker-state-machine/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/21-circuit-breaker-state-machine
go mod edit -go=1.24
```

### Why the rollback target is captured before the tentative move, and why the mutex spans the whole attempt

`AttemptTransition` reads `previous := b.state` and holds it in a local
variable *before* setting `b.state = HalfOpen`. That local is what the
deferred recover rolls back to — not some hardcoded `Open` constant,
because the guard above it already guarantees `previous` was `Open` at
entry, but writing the rollback in terms of the captured value rather than
a bare `Open` literal is what makes the pattern generalize correctly if this
breaker ever grows a third recoverable transition. The named return values
`finalState` and `panicked` exist specifically so the deferred recover can
set them: Go's rule that a deferred function can mutate a function's named
results is what lets a panic mid-transition still produce a normal-looking
return to the caller instead of propagating the crash — the caller gets
`(Open, true)` back, not a panic of its own.

The entire method body, transition attempt included, runs under `b.mu`,
held for the whole call via a single `defer b.mu.Unlock()` at the top. This
is what the concurrency test exercises: 50 goroutines calling
`AttemptTransition` at once on a breaker that starts `Open` would otherwise
race on reading and writing `b.state` — one goroutine's tentative
`HalfOpen` clobbering another's decision about what "previous" even was.
Serializing the entire attempt behind the mutex means only one goroutine is
ever inside the `Open -> HalfOpen -> {Closed | Open}` transition at a time;
every other caller either sees the already-settled `Closed`/`Open` result
from a `previous != Open` no-op, or waits its turn. The breaker is therefore
never observably `HalfOpen` to any caller outside the method that is
actively (and briefly) in that state — the very thing the panic-rollback
guarantees for the crash path, the mutex guarantees for the concurrent-access
path.

Create `circuitbreaker.go`:

```go
package circuitbreaker

import (
	"runtime/debug"
	"sync"
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

// HealthCheck probes whether the guarded dependency has recovered. It may
// panic — a bad response parser, a nil client left over from a previous
// failure — and that is exactly the failure mode this package contains.
type HealthCheck func() error

// Breaker manages Open -> HalfOpen -> Closed transitions behind a mutex, so
// concurrent callers attempting recovery at the same time see one
// consistent state machine rather than racing each other's transitions.
type Breaker struct {
	mu     sync.Mutex
	state  State
	logger func(format string, args ...any)
}

// New creates a Breaker starting Open, with logger receiving every
// transition attempt's outcome (nil logger is replaced with a no-op).
func New(logger func(format string, args ...any)) *Breaker {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Breaker{state: Open, logger: logger}
}

// State reports the breaker's current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// AttemptTransition is a no-op unless the breaker is currently Open. When
// Open, it tentatively moves to HalfOpen and runs check. If check panics
// mid-transition, the recover boundary logs the panic with full context
// (the transition attempted and a stack trace) and atomically rolls the
// state back to the previous stable value (Open) — the breaker never gets
// stuck showing HalfOpen to another caller. A check that returns an
// ordinary error also rolls back to Open (the fallback: keep failing
// closed against the dependency). Only a check that returns cleanly moves
// the breaker to Closed. panicked reports whether the panic path fired.
func (b *Breaker) AttemptTransition(check HealthCheck) (finalState State, panicked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	previous := b.state
	if previous != Open {
		return previous, false
	}

	b.state = HalfOpen
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			b.logger("health check panicked during %s->half-open transition: %v\n%s", previous, r, stack)
			b.state = previous
			finalState = previous
			panicked = true
		}
	}()

	if err := check(); err != nil {
		b.logger("health check failed during %s->half-open transition, rolling back: %v", previous, err)
		b.state = previous
		return previous, false
	}

	b.state = Closed
	return Closed, false
}
```

### The runnable demo

The breaker starts `Open`. A panicking probe rolls it back to `Open`; a
subsequent healthy probe then closes it. The demo's logger prints only the
first line of each log message so the stack trace does not flood the
terminal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/circuitbreaker"
)

func main() {
	breaker := circuitbreaker.New(func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Printf("log: %s\n", strings.SplitN(line, "\n", 2)[0])
	})

	fmt.Printf("start: %s\n", breaker.State())

	final, panicked := breaker.AttemptTransition(func() error {
		panic(errors.New("nil upstream client"))
	})
	fmt.Printf("after panicking check: state=%s panicked=%v\n", final, panicked)

	final, panicked = breaker.AttemptTransition(func() error {
		return nil
	})
	fmt.Printf("after healthy check: state=%s panicked=%v\n", final, panicked)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: open
log: health check panicked during open->half-open transition: nil upstream client
after panicking check: state=open panicked=true
after healthy check: state=closed panicked=false
```

### Tests

`TestAttemptTransitionTable` drives all five state/outcome combinations
through one table, asserting `finalState`, `panicked`, and that a panicking
check always produces at least one log entry.
`TestAttemptTransitionNeverObservablyStuckHalfOpen` fires 50 concurrent
`AttemptTransition` calls against one breaker — half of which panic — and
asserts the breaker settles on `Open` or `Closed`, never `HalfOpen`, with no
data race under `-race`.

Create `circuitbreaker_test.go`:

```go
package circuitbreaker

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestAttemptTransitionTable(t *testing.T) {
	cases := []struct {
		name         string
		startState   State
		check        HealthCheck
		wantState    State
		wantPanicked bool
	}{
		{
			name:       "already closed is a no-op",
			startState: Closed,
			check:      func() error { return nil },
			wantState:  Closed,
		},
		{
			name:       "open with healthy check closes",
			startState: Open,
			check:      func() error { return nil },
			wantState:  Closed,
		},
		{
			name:       "open with failing check rolls back to open",
			startState: Open,
			check:      func() error { return errors.New("still down") },
			wantState:  Open,
		},
		{
			name:         "open with panicking check (error value) rolls back",
			startState:   Open,
			check:        func() error { panic(errors.New("nil client dereferenced")) },
			wantState:    Open,
			wantPanicked: true,
		},
		{
			name:         "open with panicking check (string value) rolls back",
			startState:   Open,
			check:        func() error { panic("totally unexpected") },
			wantState:    Open,
			wantPanicked: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			b := New(func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			})
			b.state = tc.startState

			finalState, panicked := b.AttemptTransition(tc.check)

			if finalState != tc.wantState {
				t.Fatalf("finalState = %v, want %v", finalState, tc.wantState)
			}
			if panicked != tc.wantPanicked {
				t.Fatalf("panicked = %v, want %v", panicked, tc.wantPanicked)
			}
			if b.State() != tc.wantState {
				t.Fatalf("b.State() = %v, want %v", b.State(), tc.wantState)
			}
			if tc.wantPanicked && len(logs) == 0 {
				t.Fatal("a panicking check must produce at least one log entry")
			}
		})
	}
}

func TestAttemptTransitionNeverObservablyStuckHalfOpen(t *testing.T) {
	b := New(func(string, ...any) {})
	b.state = Open

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			check := func() error {
				if i%2 == 0 {
					panic(errors.New("flaky probe"))
				}
				return nil
			}
			b.AttemptTransition(check)
		}(i)
	}
	wg.Wait()

	final := b.State()
	if final != Open && final != Closed {
		t.Fatalf("final state = %v, want Open or Closed, never HalfOpen", final)
	}
}
```

## Review

`AttemptTransition` is correct when a panicking health check always leaves
the breaker in the state it started in — never stuck showing `HalfOpen` —
and when concurrent callers never observe a torn or racing transition. The
mutex spanning the entire method is what makes the concurrency guarantee
hold: without it, two goroutines could both read `previous = Open`, both
tentatively set `HalfOpen`, and one's rollback could stomp on the other's
successful close. The panic-specific guarantee comes from capturing
`previous` in a local before the tentative write and restoring exactly that
value from the deferred recover using named return values — the same
"capture, mutate tentatively, restore via defer" shape as any other
panic-safe rollback, just applied to a state machine instead of a resource
handle.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — serializing the whole transition attempt against concurrent callers.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — named return values mutated from a deferred recover, the mechanism the rollback relies on.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the Open/HalfOpen/Closed state machine this module implements defensively.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-event-sourcing-replay.md](20-event-sourcing-replay.md) | Next: [22-cache-invalidation-multi-backend.md](22-cache-invalidation-multi-backend.md)
