# Exercise 28: Circuit Breaker: Per-Service Instance Shares Mutable State in Closures

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A health-check system builds one circuit breaker per service in a loop, each
breaker a closure over some mutable failure-counting state. The trap: if
every service's closure captures the SAME state object instead of getting
its own, concurrent health checks for DIFFERENT services all increment ONE
counter — so whichever service happens to be checked after the COMBINED
failure count crosses the threshold reports tripped, even if that service
individually never had more than one or two failures of its own.

## What you'll build

```text
breaker/                     independent module: example.com/breaker
  go.mod                     go 1.24
  breaker.go                 counterState, Breaker, BuggyMakeBreakers, MakeBreakers, RunHealthChecks
  cmd/
    demo/
      main.go                runnable demo: 2 services, print which ones tripped
  breaker_test.go            table test: cross-service contamination vs isolation; edge cases
```

- Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
- Implement: `counterState.recordFailure` doing increment-then-compare-and-flip in one locked critical section; `BuggyMakeBreakers` sharing one `*counterState` across every service; `MakeBreakers` giving each service its own.
- Test: concurrently record different failure counts per service and assert `MakeBreakers` keeps each service's trip decision independent while `BuggyMakeBreakers` lets one service's failures trip another's breaker; `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/breaker/cmd/demo
cd ~/go-exercises/breaker
go mod init example.com/breaker
go mod edit -go=1.24
```

### Why the check-then-act must be one lock acquisition, and why sharing state is the real bug

`counterState.recordFailure` increments the failure count AND compares it to
the threshold under the SAME `mu.Lock()`/`mu.Unlock()` pair, so two
concurrent health checks can never interleave between "increment" and
"check": the trip decision is atomic with respect to the count that produced
it. That is necessary but not sufficient. `BuggyMakeBreakers` declares one
`*counterState` before the loop and every service's `Breaker` closes over
that SAME pointer instead of getting its own — so the atomicity is real, but
it is atomicity over the WRONG, shared, piece of state. A service that only
ever failed once can still read `Tripped() == true` once some OTHER
service's failures push the combined total past the threshold, because both
services' closures are reading and writing the identical counter.
`MakeBreakers` fixes this by allocating a fresh `*counterState` per service
inside the loop, so no service's health checks can ever influence another's
trip decision.

Create `breaker.go`:

```go
package breaker

import "sync"

// counterState is the mutable state behind one circuit breaker: a failure
// count and whether it has tripped. Both fields are read and written only
// under mu, and recordFailure does its check-then-act (increment, then
// compare-and-flip) in one critical section so concurrent callers can never
// interleave between the check and the act.
type counterState struct {
	mu        sync.Mutex
	failures  int
	tripped   bool
	threshold int
}

func newCounterState(threshold int) *counterState {
	return &counterState{threshold: threshold}
}

// recordFailure increments the failure count and flips tripped once the
// count reaches the threshold, all under one lock acquisition.
func (s *counterState) recordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	if s.failures >= s.threshold {
		s.tripped = true
	}
}

func (s *counterState) isTripped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tripped
}

// Breaker is the per-service handle a health-check goroutine calls into.
type Breaker struct {
	RecordFailure func()
	Tripped       func() bool
}

// BuggyMakeBreakers builds one Breaker per service, but every closure shares
// the SAME *counterState declared once before the loop instead of getting
// its own. Concurrent failures reported for DIFFERENT services all increment
// one counter, so whichever service happens to be checked after the
// COMBINED failure count across every service crosses the threshold reports
// tripped -- even if that service, on its own, never had more than one or
// two failures.
func BuggyMakeBreakers(services []string, threshold int) map[string]Breaker {
	out := make(map[string]Breaker, len(services))
	shared := newCounterState(threshold) // BUG: one state for every service
	for _, svc := range services {
		_ = svc
		out[svc] = Breaker{
			RecordFailure: shared.recordFailure,
			Tripped:       shared.isTripped,
		}
	}
	return out
}

// MakeBreakers builds one Breaker per service, each closing over its OWN
// counterState, so one service's failures can never trip another's breaker.
func MakeBreakers(services []string, threshold int) map[string]Breaker {
	out := make(map[string]Breaker, len(services))
	for _, svc := range services {
		state := newCounterState(threshold)
		out[svc] = Breaker{
			RecordFailure: state.recordFailure,
			Tripped:       state.isTripped,
		}
	}
	return out
}

// RunHealthChecks fires failuresPerService[svc] concurrent RecordFailure
// calls for each service in order, waiting for one service's group of
// goroutines to finish before starting the next so each service's total is
// deterministic, then reports which services ended up tripped.
func RunHealthChecks(breakers map[string]Breaker, order []string, failuresPerService map[string]int) map[string]bool {
	result := make(map[string]bool, len(order))
	for _, svc := range order {
		b := breakers[svc]
		var wg sync.WaitGroup
		for i := 0; i < failuresPerService[svc]; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.RecordFailure()
			}()
		}
		wg.Wait()
		result[svc] = b.Tripped()
	}
	return result
}
```

### The runnable demo

The demo runs health checks for two services, "payments" first, and prints
which ones ended up tripped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/breaker"
)

func main() {
	services := []string{"checkout", "payments"}
	// payments is checked first, so its 2 failures land in the shared
	// counter before checkout's 1 failure crosses the threshold.
	order := []string{"payments", "checkout"}
	failures := map[string]int{"checkout": 1, "payments": 2}

	buggy := breaker.BuggyMakeBreakers(services, 3)
	buggyResult := breaker.RunHealthChecks(buggy, order, failures)
	fmt.Println("buggy  tripped:", buggyResult)

	fixed := breaker.MakeBreakers(services, 3)
	fixedResult := breaker.RunHealthChecks(fixed, order, failures)
	fmt.Println("fixed  tripped:", fixedResult)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  tripped: map[checkout:true payments:false]
fixed  tripped: map[checkout:false payments:false]
```

### Tests

`TestRunHealthChecks` is a table test: with payments checked first (2
failures) and checkout second (1 failure) against a threshold of 3, `Make
Breakers` correctly reports neither service tripped, while `BuggyMake
Breakers` reports checkout — which never had more than 1 failure of its own
— as tripped, because it was the one checked after the combined total
crossed 3. `TestMakeBreakersSingleServiceEdgeCase` and `TestMakeBreakers
BelowThresholdNeverTrips` cover the boundaries where there is no other
service to leak into and where nobody comes close to the threshold at all.

Create `breaker_test.go`:

```go
package breaker

import "testing"

func TestRunHealthChecks(t *testing.T) {
	services := []string{"checkout", "payments"}
	// payments is checked first so its 2 failures land in the shared counter
	// before checkout's 1 failure pushes the combined total to the
	// threshold -- checkout, which never had more than 1 failure of its
	// own, is the one that ends up reading tripped=true in the buggy case.
	order := []string{"payments", "checkout"}
	failures := map[string]int{"checkout": 1, "payments": 2}

	tests := []struct {
		name string
		make func([]string, int) map[string]Breaker
		want map[string]bool
	}{
		{
			name: "fixed: each service's failures never affect another's breaker",
			make: MakeBreakers,
			want: map[string]bool{"checkout": false, "payments": false},
		},
		{
			name: "buggy: whichever service is checked after the combined total crosses threshold trips",
			make: BuggyMakeBreakers,
			want: map[string]bool{"checkout": true, "payments": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			breakers := tt.make(services, 3)
			got := RunHealthChecks(breakers, order, failures)
			for _, svc := range services {
				if got[svc] != tt.want[svc] {
					t.Fatalf("tripped[%q] = %v, want %v (got=%v)", svc, got[svc], tt.want[svc], got)
				}
			}
		})
	}
}

func TestMakeBreakersSingleServiceEdgeCase(t *testing.T) {
	services := []string{"solo"}
	failures := map[string]int{"solo": 3}

	fixed := MakeBreakers(services, 3)
	if got := RunHealthChecks(fixed, services, failures); !got["solo"] {
		t.Fatalf("tripped[solo] = %v, want true", got["solo"])
	}

	buggy := BuggyMakeBreakers(services, 3)
	if got := RunHealthChecks(buggy, services, failures); !got["solo"] {
		t.Fatalf("tripped[solo] = %v, want true (single service: bug can't manifest)", got["solo"])
	}
}

func TestMakeBreakersBelowThresholdNeverTrips(t *testing.T) {
	services := []string{"a", "b"}
	failures := map[string]int{"a": 1, "b": 1}

	fixed := MakeBreakers(services, 5)
	got := RunHealthChecks(fixed, services, failures)
	for _, svc := range services {
		if got[svc] {
			t.Fatalf("tripped[%q] = true, want false (well below threshold)", svc)
		}
	}
}
```

## Review

A circuit breaker is correct when one service's failures can never trip
another service's breaker, no matter how many health checks run
concurrently. The lock inside `counterState.recordFailure` makes the
increment-then-compare atomic, which rules out one class of bug (a race
between checking and flipping), but it says nothing about WHICH counter that
atomicity applies to. `BuggyMakeBreakers` is airtight concurrency around the
wrong shared object. The fix is not a bigger lock or `sync/atomic` instead of
`sync.Mutex` — it is making sure the loop that builds N breakers actually
allocates N independent `counterState` values instead of one. Run
`go test -race`; both variants are race-free, because the bug was never
about racing memory, only about which memory was shared in the first place.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — protecting a full check-then-act critical section.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the pattern this exercise's `Breaker` implements a minimal version of.
- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-resource-pool-checkout-defer-release-starvation.md](27-resource-pool-checkout-defer-release-starvation.md) | Next: [29-retry-exponential-backoff-timer-callback-index.md](29-retry-exponential-backoff-timer-callback-index.md)
