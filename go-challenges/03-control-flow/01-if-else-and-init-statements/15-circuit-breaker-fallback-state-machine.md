# Exercise 15: Circuit Breaker: Three States and Fallback Decisions

**Nivel: Intermedio** — validacion rapida (un test corto).

When a downstream service starts failing intermittently, hammering it with
every incoming request only makes the outage worse and ties up your own
goroutines waiting on a dependency that is not going to answer. A circuit
breaker tracks three states — closed (normal), open (fail fast), half-open
(testing recovery) — and each transition is one guard clause deciding
whether to call the dependency or return a fallback error immediately. This
module is fully self-contained: its own `go mod init`, all code inline, its
own test file.

## What you'll build

```text
breaker/                    independent module: example.com/circuit-breaker-fallback-state-machine
  go.mod                    go 1.24
  breaker.go                State, Breaker, Decide, RecordFailure, RecordSuccess
  breaker_test.go           table: closed trips, open blocks, cooldown, half-open outcomes
```

- Files: `breaker.go`, `breaker_test.go`.
- Implement: a value-typed `Breaker` (no pointers, no mutex — state transitions are pure) with `Decide(b Breaker, now time.Time) (allow bool, next Breaker)`, `RecordFailure(b Breaker, now time.Time) Breaker`, and `RecordSuccess(b Breaker) Breaker`.
- Test: a table covering the closed path always allowing, failures accumulating below threshold, the threshold trip opening the circuit, an open circuit blocking before cooldown, an open circuit allowing exactly one half-open trial after cooldown, a half-open success closing the circuit, and a half-open failure reopening it.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/breaker
cd ~/go-exercises/breaker
go mod init example.com/circuit-breaker-fallback-state-machine
go mod edit -go=1.24
```

### Why the state lives in a value, not a pointer with a mutex

A circuit breaker's state transitions are deterministic given the current
state, the current time, and the outcome of the last call — there is no
reason to hide that behind a mutex if the caller already serializes calls
through a single decision point per request. Keeping `Breaker` a plain
struct passed and returned by value makes every transition a pure function:
`Decide` and `RecordFailure`/`RecordSuccess` take a `Breaker` and return the
next one, with nothing hidden in shared mutable state. That is what makes
the table test exhaustive — every transition is reachable by constructing
the input value directly, with no setup sequence required to reach a
particular state.

The three-state shape resists a naive two-state model (`open`/`closed`)
because a circuit that reopens the instant traffic resumes never recovers:
after `Cooldown` elapses, exactly one trial call must be let through in
`HalfOpen` to test whether the dependency has recovered, and that trial's
outcome — not a timer — decides the next state. `Decide` only ever returns
`allow == true` for `HalfOpen` on the tick right after cooldown elapses;
callers must not call `Decide` again for a second concurrent trial before
`RecordSuccess`/`RecordFailure` resolves the first one.

Create `breaker.go`:

```go
// Package breaker implements a three-state circuit breaker (closed, open,
// half-open) as a pure value type: every transition takes the current state
// and returns the next one, with no hidden mutable state.
package breaker

import "time"

// State is one of the three circuit breaker states.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

// Breaker is the circuit breaker's state. It is a plain value: callers own
// storing and passing it between calls, typically guarded by whatever they
// already use to serialize decisions for one logical dependency.
type Breaker struct {
	State        State
	FailureCount int
	Threshold    int           // failures in Closed before tripping to Open
	OpenedAt     time.Time     // when the circuit last opened
	Cooldown     time.Duration // how long Open blocks before trying HalfOpen
}

// Decide reports whether a call should be allowed through at now, and the
// Breaker state to persist afterward. Closed always allows. Open blocks until
// Cooldown has elapsed since OpenedAt, then allows exactly one trial call and
// moves to HalfOpen. HalfOpen allows the trial call already in flight.
func Decide(b Breaker, now time.Time) (allow bool, next Breaker) {
	if b.State == Open {
		if now.Sub(b.OpenedAt) < b.Cooldown {
			return false, b
		}
		b.State = HalfOpen
		return true, b
	}

	// Closed and HalfOpen both allow the call; RecordFailure/RecordSuccess
	// decide what happens next based on the outcome.
	return true, b
}

// RecordFailure applies a failed call's outcome. In HalfOpen, the trial
// failed, so the circuit reopens immediately regardless of Threshold. In
// Closed, the failure count increments and trips the circuit open once it
// reaches Threshold.
func RecordFailure(b Breaker, now time.Time) Breaker {
	if b.State == HalfOpen {
		b.State = Open
		b.OpenedAt = now
		b.FailureCount = 0
		return b
	}

	b.FailureCount++
	if b.FailureCount >= b.Threshold {
		b.State = Open
		b.OpenedAt = now
		b.FailureCount = 0
	}
	return b
}

// RecordSuccess applies a succeeded call's outcome. In HalfOpen, the trial
// succeeded, so the circuit closes. In any state, the failure count resets —
// a success means the failure streak that mattered is over.
func RecordSuccess(b Breaker) Breaker {
	if b.State == HalfOpen {
		b.State = Closed
	}
	b.FailureCount = 0
	return b
}
```

### Tests

The table drives each transition directly by constructing the starting
`Breaker` value, rather than replaying a sequence of calls, so every branch
in `Decide`, `RecordFailure`, and `RecordSuccess` is independently reachable.

Create `breaker_test.go`:

```go
package breaker

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	epoch := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		b    Breaker
		now  time.Time
		want bool
	}{
		{
			name: "closed always allows",
			b:    Breaker{State: Closed, Threshold: 3},
			now:  epoch,
			want: true,
		},
		{
			name: "open blocks before cooldown elapses",
			b:    Breaker{State: Open, OpenedAt: epoch, Cooldown: time.Minute},
			now:  epoch.Add(30 * time.Second),
			want: false,
		},
		{
			name: "open allows a trial exactly at cooldown",
			b:    Breaker{State: Open, OpenedAt: epoch, Cooldown: time.Minute},
			now:  epoch.Add(time.Minute),
			want: true,
		},
		{
			name: "half-open allows the in-flight trial",
			b:    Breaker{State: HalfOpen},
			now:  epoch,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Decide(tc.b, tc.now)
			if got != tc.want {
				t.Errorf("Decide(%+v, %v) = %v, want %v", tc.b, tc.now, got, tc.want)
			}
		})
	}
}

func TestRecordFailureTripsAtThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	b := Breaker{State: Closed, Threshold: 3}

	b = RecordFailure(b, now)
	if b.State != Closed {
		t.Fatalf("after 1 failure, state = %v, want Closed", b.State)
	}
	b = RecordFailure(b, now)
	if b.State != Closed {
		t.Fatalf("after 2 failures, state = %v, want Closed", b.State)
	}
	b = RecordFailure(b, now)
	if b.State != Open {
		t.Fatalf("after 3 failures (threshold), state = %v, want Open", b.State)
	}
	if b.FailureCount != 0 {
		t.Errorf("FailureCount after trip = %d, want reset to 0", b.FailureCount)
	}
}

func TestHalfOpenOutcomes(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("success in half-open closes the circuit", func(t *testing.T) {
		t.Parallel()
		b := RecordSuccess(Breaker{State: HalfOpen, FailureCount: 2})
		if b.State != Closed {
			t.Errorf("state = %v, want Closed", b.State)
		}
		if b.FailureCount != 0 {
			t.Errorf("FailureCount = %d, want 0", b.FailureCount)
		}
	})

	t.Run("failure in half-open reopens immediately", func(t *testing.T) {
		t.Parallel()
		b := RecordFailure(Breaker{State: HalfOpen}, now)
		if b.State != Open {
			t.Errorf("state = %v, want Open", b.State)
		}
		if !b.OpenedAt.Equal(now) {
			t.Errorf("OpenedAt = %v, want %v", b.OpenedAt, now)
		}
	})
}
```

Verify: `go test -count=1 ./...`

## Review

The half-open trial only exists because `Decide` treats `Open` and
`HalfOpen` as different questions: "has cooldown elapsed" versus "is the
trial call in flight." Collapsing them into one guard would either let
every request through the instant cooldown elapses (a thundering herd
against a barely-recovered dependency) or never let any request through at
all. Carry this forward: any resilience primitive with a "probe, then
decide" recovery step needs its probe state kept distinct from both the
healthy and unhealthy states, not folded into a boolean.

## Resources

- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the pattern this module implements.
- [Microsoft Azure Architecture Center: Circuit Breaker pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) — production guidance on thresholds and cooldowns.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — why passing `Breaker` by value gives each call its own copy.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-accept-header-negotiation.md](14-accept-header-negotiation.md) | Next: [16-connection-pool-eviction-lru-age.md](16-connection-pool-eviction-lru-age.md)
