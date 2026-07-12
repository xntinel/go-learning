# Exercise 3: Lock-Free State Machine via CAS Retry Loop

A small state machine — a traffic light, a connection lifecycle, a circuit-breaker
phase — advances along legal edges. When several goroutines might advance it
concurrently, you want each transition to be all-or-nothing without a lock. This
exercise builds that with the canonical `CompareAndSwap` retry loop over an
`atomic.Int32`, plus a defensive guard against a corrupted state.

This module is fully self-contained.

## What you'll build

```text
casstate/                  independent module: example.com/casstate
  go.mod
  light.go                 type Light; type TrafficLight; Current, Advance (CAS loop)
  cmd/
    demo/
      main.go              cycles the light five times
  light_test.go            deterministic-cycle test, concurrent-validity test, Example
```

- Files: `light.go`, `cmd/demo/main.go`, `light_test.go`.
- Implement: a `TrafficLight` over `atomic.Int32`; `Advance` reads the current state, computes the next, and `CompareAndSwap`es in a retry loop, guarding against invalid states.
- Test: a single-goroutine cycle asserting the exact sequence; 100 concurrent `Advance` calls asserting the final state is always valid.
- Verify: `go test -count=1 -race ./...`

### Why a CAS loop and not Add

A state transition is not an addition. "Next" is a function of the current state
along legal edges (red then green then yellow then red), not `current + 1`, and the
transition must be conditional: apply it only if no one else changed the state in
the meantime. That is exactly the shape `CompareAndSwap` exists for, and there is no
dedicated atomic "transition" op, so you build it from the retry loop.

`Advance` reads the current state with `Load`, computes the next state from it, and
calls `CompareAndSwap(cur, next)`. If it returns `true`, the transition landed and
we are done. If it returns `false`, another goroutine advanced the light between our
`Load` and our CAS; we simply loop, re-read the now-current state, and recompute.
Every iteration makes progress somewhere — a CAS only fails because someone else
succeeded — so the loop terminates. This is the difference between a lock (which
blocks the loser) and a CAS loop (which retries the loser): under contention the
CAS loop never parks a goroutine.

The `default` branch is a deliberate guard. If the stored `int32` is ever a value
outside the three legal states (a bug, a bad `Store` elsewhere), `Advance` returns
`false` instead of looping forever on a state it cannot compute a successor for.
Defensive, cheap, and it turns "silent infinite loop" into "observable false".

Create `light.go`:

```go
package casstate

import "sync/atomic"

// Light is a traffic-light phase stored as an int32 so it fits an atomic.Int32.
type Light int32

const (
	Red Light = iota
	Green
	Yellow
)

func (l Light) String() string {
	switch l {
	case Red:
		return "red"
	case Green:
		return "green"
	case Yellow:
		return "yellow"
	default:
		return "unknown"
	}
}

// TrafficLight is a lock-free state machine. Advance moves it along legal edges
// using a CompareAndSwap retry loop.
type TrafficLight struct {
	state atomic.Int32
}

func NewTrafficLight() *TrafficLight {
	t := &TrafficLight{}
	t.state.Store(int32(Red))
	return t
}

// Current returns the current phase with a wait-free load.
func (t *TrafficLight) Current() Light {
	return Light(t.state.Load())
}

// Advance moves the light to its next phase. It returns true if it transitioned,
// and false only if the stored state was corrupt (outside the legal set).
func (t *TrafficLight) Advance() bool {
	for {
		cur := t.state.Load()
		var next int32
		switch Light(cur) {
		case Red:
			next = int32(Green)
		case Green:
			next = int32(Yellow)
		case Yellow:
			next = int32(Red)
		default:
			return false // corrupt state; do not spin forever
		}
		if t.state.CompareAndSwap(cur, next) {
			return true
		}
		// another goroutine advanced between Load and CAS; retry
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/casstate"
)

func main() {
	tl := casstate.NewTrafficLight()
	fmt.Println("start:", tl.Current())
	for range 5 {
		tl.Advance()
		fmt.Println("advance:", tl.Current())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: red
advance: green
advance: yellow
advance: red
advance: green
advance: yellow
```

### Tests

`TestCycle` runs a single goroutine and asserts the exact deterministic sequence.
`TestConcurrentValid` fires 100 concurrent `Advance` calls; because the transitions
race, the final state is nondeterministic, but it must always be one of the three
legal phases — never corrupt — which proves the CAS loop never produces a torn or
illegal state.

Create `light_test.go`:

```go
package casstate

import (
	"fmt"
	"sync"
	"testing"
)

func TestCycle(t *testing.T) {
	t.Parallel()

	tl := NewTrafficLight()
	if got := tl.Current(); got != Red {
		t.Fatalf("initial = %v, want red", got)
	}
	want := []Light{Green, Yellow, Red, Green, Yellow}
	for i, w := range want {
		if !tl.Advance() {
			t.Fatalf("Advance[%d] returned false", i)
		}
		if got := tl.Current(); got != w {
			t.Fatalf("after Advance[%d] = %v, want %v", i, got, w)
		}
	}
}

func TestConcurrentValid(t *testing.T) {
	t.Parallel()

	tl := NewTrafficLight()
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			tl.Advance()
		}()
	}
	wg.Wait()

	switch cur := tl.Current(); cur {
	case Red, Green, Yellow:
		// valid
	default:
		t.Fatalf("invalid final state: %v", cur)
	}
}

func ExampleTrafficLight() {
	tl := NewTrafficLight()
	tl.Advance()
	tl.Advance()
	fmt.Println(tl.Current())
	// Output: yellow
}
```

## Review

The machine is correct when a transition is atomic end-to-end: `Advance` either
moves the light exactly one legal edge or, on a corrupt state, returns `false` —
never leaves a half-updated or illegal value. `TestConcurrentValid` under `-race` is
the proof that concurrent advances never tear the state. The mistake to avoid is
computing `next` outside the loop and CAS-ing once: under contention that CAS fails
and, without the loop, the transition is silently dropped. Re-read inside the loop
every iteration so `next` is always computed from the genuinely current state.

## Resources

- [`atomic.Int32.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Int32.CompareAndSwap) — the transition primitive.
- [The Go Memory Model](https://go.dev/ref/mem) — the total order that makes the CAS loop correct.
- [ABA problem](https://en.wikipedia.org/wiki/ABA_problem) — why value CAS loops must keep intermediate states benign.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-drain-flag-readiness-gate.md](02-drain-flag-readiness-gate.md) | Next: [04-monotonic-sequence-generator.md](04-monotonic-sequence-generator.md)
