# Exercise 23: Circuit Breaker Transition — Restore State on Panic

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A circuit breaker's `HalfOpen` state exists to let exactly one trial request
through after a trip, so the breaker can decide whether to heal (`Closed`) or
stay tripped (`Open`) based on that trial's outcome. But "outcome" only means
something if the trial actually finishes. A panic mid-probe is the one case
where the breaker cannot conclude anything — and if the deferred cleanup
doesn't account for it, the breaker gets stuck in `HalfOpen` forever,
rejecting every future call with "probe already in progress."

## What you'll build

```text
breaker/                     independent module: example.com/breaker
  go.mod
  breaker/breaker.go           State, Breaker (mutex-guarded); Call; probe (defer restore-on-panic)
  breaker/breaker_test.go      table: closed success/failure; open probe success/failure; half-open rejection; panic restore
  cmd/demo/main.go             runnable demo: trip the breaker, then heal it with a successful probe
```

- Files: `breaker/breaker.go`, `breaker/breaker_test.go`, `cmd/demo/main.go`.
- Implement: a `State` enum (`Closed`, `Open`, `HalfOpen`); a mutex-guarded `Breaker` with `State() State`; `Call(work func() error) error` that runs `work` directly when `Closed` (tripping to `Open` on failure), runs a trial `probe` when `Open`, and returns `ErrProbeInProgress` when `HalfOpen`; and `probe(work func() error) (err error)`, which locks once to snapshot `Open` and set `HalfOpen`, defers a closure that restores the snapshot and re-`panic`s if `recover()` sees a non-nil value, and otherwise transitions to `Closed` (success) or `Open` (ordinary failure).
- Test: `Closed` circuit on success (stays `Closed`) and on failure (trips to `Open`); `Open` circuit whose probe succeeds (heals to `Closed`) and whose probe fails (returns to `Open`), both asserting the state actually observed *during* the probe is `HalfOpen`; a `HalfOpen` circuit rejecting a concurrent `Call` with `ErrProbeInProgress` without running `work`; a probe that panics, asserting the breaker is restored to `Open` (not stuck in `HalfOpen`) and the panic still propagates.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/23-circuit-breaker-state-unwinding/breaker go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/23-circuit-breaker-state-unwinding/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/23-circuit-breaker-state-unwinding
go mod edit -go=1.24
```

### Three outcomes, but only one needs a restore

`probe` has exactly three ways to end: `work` returns `nil` (heal to
`Closed`), `work` returns a non-nil error (trip back to `Open`), or `work`
panics. The first two are both normal completions from the breaker's
point of view — a trial ran to completion and gave a real answer — so both
set the *next* state explicitly, on the line right after `work` returns,
with no need to consult `prior` at all. Only the third case is different in
kind: a panic means `work` never produced an answer, so there is nothing to
conclude, and the only correct move is to put the breaker back exactly where
it was before the trial started — `prior`, captured once, at the top of
`probe`, by `snapshotAndTransition`. This is the same three-way split
`17-flag-flip-panic-restore.md` makes between success, ordinary error, and
panic — applied here to a three-state machine instead of a boolean.

### HalfOpen is also a lock, not just a state

Rejecting `Call` with `ErrProbeInProgress` while the breaker is `HalfOpen`
is what keeps at most one trial request in flight at a time — without it, a
burst of concurrent calls arriving right after a trip would all see `Open`,
all start their own probes, and the breaker's transitions would race each
other unpredictably. `HalfOpen` is a real state precisely because it is
also, functionally, a mutex on "who gets to run the trial": `Call`'s
`switch` checks `State()` before deciding what to do, and only the call that
actually executes `snapshotAndTransition(HalfOpen)` inside `probe` ever gets
to run `work`.

Create `breaker/breaker.go`:

```go
package breaker

import (
	"errors"
	"sync"
)

// State is one of a circuit breaker's three states.
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

// ErrProbeInProgress is returned when Call is invoked while a trial request
// (probe) is already in flight, so only one probe is ever outstanding.
var ErrProbeInProgress = errors.New("breaker: probe already in progress")

// Breaker is a minimal, concurrency-safe circuit breaker.
type Breaker struct {
	mu    sync.Mutex
	state State
}

// New returns a Breaker starting in the Closed state.
func New() *Breaker {
	return &Breaker{state: Closed}
}

// State reports the breaker's current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) setState(s State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = s
}

// snapshotAndTransition locks once, records the current state, sets the new
// one, and returns the prior value -- a single locked check-then-act so no
// other goroutine can observe or change the state between the read and the
// write.
func (b *Breaker) snapshotAndTransition(to State) (prior State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prior = b.state
	b.state = to
	return prior
}

// Call runs work through the breaker. In the Closed state it runs work
// directly and trips the breaker to Open on failure. In the Open state it
// runs work as a single trial probe (see probe). In the HalfOpen state --
// meaning a probe is already in flight -- it rejects the call immediately
// with ErrProbeInProgress rather than letting two trial requests race.
func (b *Breaker) Call(work func() error) error {
	switch b.State() {
	case HalfOpen:
		return ErrProbeInProgress
	case Open:
		return b.probe(work)
	default: // Closed
		if err := work(); err != nil {
			b.setState(Open)
			return err
		}
		return nil
	}
}

// probe transitions Open -> HalfOpen for the duration of one trial request.
// If work succeeds, the circuit heals to Closed; if it returns an ordinary
// error, the breaker goes back to Open -- both are normal, expected outcomes
// of a trial. Only an unhandled panic mid-probe is different in kind: the
// trial never finished, so its result cannot be trusted at all, and the
// deferred closure restores the breaker to its pre-probe state (prior,
// which is always Open here) before re-raising the panic, rather than
// leaving the breaker stuck in HalfOpen where every subsequent Call would
// be wrongly rejected with ErrProbeInProgress forever.
func (b *Breaker) probe(work func() error) (err error) {
	prior := b.snapshotAndTransition(HalfOpen)

	defer func() {
		if r := recover(); r != nil {
			b.setState(prior)
			panic(r)
		}
	}()

	if werr := work(); werr != nil {
		b.setState(Open)
		return werr
	}
	b.setState(Closed)
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/breaker/breaker"
)

func main() {
	b := breaker.New()
	fmt.Println("initial state:", b.State())

	// A failure while closed trips the breaker open.
	_ = b.Call(func() error { return errors.New("downstream 500") })
	fmt.Println("after failure:", b.State())

	// While open, the next Call is a trial probe. A successful probe heals it.
	_ = b.Call(func() error { return nil })
	fmt.Println("after successful probe:", b.State())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial state: closed
after failure: open
after successful probe: closed
```

### Tests

Create `breaker/breaker_test.go`:

```go
package breaker

import (
	"errors"
	"testing"
)

func TestCallOnClosedCircuit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workErr   error
		wantState State
	}{
		{name: "success stays closed", workErr: nil, wantState: Closed},
		{name: "failure trips to open", workErr: errors.New("downstream 500"), wantState: Open},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New()
			err := b.Call(func() error { return tt.workErr })

			if !errors.Is(err, tt.workErr) {
				t.Fatalf("err = %v, want %v", err, tt.workErr)
			}
			if got := b.State(); got != tt.wantState {
				t.Fatalf("State() = %v, want %v", got, tt.wantState)
			}
		})
	}
}

func TestCallOnOpenCircuitProbesAndTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workErr   error
		wantState State
	}{
		{name: "probe succeeds heals to closed", workErr: nil, wantState: Closed},
		{name: "probe fails returns to open", workErr: errors.New("still failing"), wantState: Open},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New()
			b.setState(Open)

			var stateDuringProbe State
			err := b.Call(func() error {
				stateDuringProbe = b.State()
				return tt.workErr
			})

			if !errors.Is(err, tt.workErr) {
				t.Fatalf("err = %v, want %v", err, tt.workErr)
			}
			if stateDuringProbe != HalfOpen {
				t.Fatalf("state during probe = %v, want HalfOpen", stateDuringProbe)
			}
			if got := b.State(); got != tt.wantState {
				t.Fatalf("State() after Call = %v, want %v", got, tt.wantState)
			}
		})
	}
}

func TestCallOnHalfOpenRejectsConcurrentProbe(t *testing.T) {
	t.Parallel()

	b := New()
	b.setState(HalfOpen)

	ran := false
	err := b.Call(func() error {
		ran = true
		return nil
	})

	if !errors.Is(err, ErrProbeInProgress) {
		t.Fatalf("err = %v, want ErrProbeInProgress", err)
	}
	if ran {
		t.Fatal("work must not run while a probe is already in flight")
	}
	if got := b.State(); got != HalfOpen {
		t.Fatalf("State() = %v, want HalfOpen (unchanged)", got)
	}
}

func TestProbePanicRestoresPriorStateAndRePanics(t *testing.T) {
	t.Parallel()

	b := New()
	b.setState(Open)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = b.Call(func() error {
			panic("downstream client crashed")
		})
	}()

	if got := b.State(); got != Open {
		t.Fatalf("State() after panic = %v, want Open (restored, not stuck in HalfOpen)", got)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The panic test is the one that would catch the classic circuit-breaker bug:
a `probe` that transitions to `HalfOpen` and then calls `work` without a
deferred restore leaves the breaker in `HalfOpen` forever the moment `work`
panics, because nothing ever runs to move it back out. Every subsequent
`Call` then hits the `HalfOpen` branch and returns `ErrProbeInProgress` —
the breaker is permanently wedged, rejecting all traffic, and the only fix
is a process restart. The two `Open`-circuit table cases matter for a
different reason: they assert the state observed *during* the probe (not
just before or after), which is the only way to prove `HalfOpen` is a real,
observable intermediate state and not something the implementation skips
past by mistake.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [22-multipart-upload-abort-defer.md](22-multipart-upload-abort-defer.md) | Next: [24-read-lock-demote-upgrade-defer.md](24-read-lock-demote-upgrade-defer.md)
