# Exercise 15: Admission Gate as a Paired Acquire/Release Closure

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch consumer wants to cap how many items it has dispatched but not yet
finished processing, without spinning up real goroutines to prove it. Every
other factory in this lesson returns exactly one closure over its captured
state; `NewGate` returns two — `acquire` and `release` — that share a single
captured in-flight counter, which is the shape a semaphore-like admission
control actually needs.

## What you'll build

```text
admission/                 independent module: example.com/admission-gate-pair
  go.mod                   go 1.24
  gate.go                  NewGate returns (acquire func() bool, release func())
  gate_test.go             table test: limit enforced, release frees a slot
```

- Files: `gate.go`, `gate_test.go`.
- Implement: `NewGate(limit int) (acquire func() bool, release func())`, both closing over one captured `inFlight int`.
- Test: a table acquires up to the limit and confirms the next call is refused, then checks `release` frees exactly one slot and cannot drive the counter negative, and that two gates never share state.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/15-admission-gate-closure-pair
cd go-solutions/04-functions/04-first-class-functions-and-closures/15-admission-gate-closure-pair
go mod edit -go=1.24
```

### Two closures, one captured counter

`NewGate` declares a single `inFlight` variable and returns two function
literals that both reference it: `acquire` reads and increments it, `release`
decrements it. Neither closure could do its job alone — `acquire` without
`release` can only ever fill up, and `release` without `acquire` has nothing to
free. What makes this a single unit instead of two independent closures is that
they share the exact same captured variable, allocated once per `NewGate` call;
two separate calls to `NewGate` allocate two separate `inFlight` counters, so
gate `A` filling up has no effect on gate `B`.

`release` clamps at zero instead of letting `inFlight` go negative, because a
caller that calls `release` one extra time (a double-release after a retry, a
release for a call that was itself refused) is a realistic bug and should not
silently open extra capacity by pushing the counter below zero.

This version is single-goroutine on purpose, matching the rest of this
lesson's stateful modules: nothing guards `inFlight` with a lock, so calling
`acquire`/`release` from multiple goroutines races on the captured counter.
Production either wraps both closures in one `sync.Mutex` or builds the gate
on a buffered channel used as a semaphore (`make(chan struct{}, limit)`).

Create `gate.go`:

```go
package admission

// NewGate returns a paired acquire/release closure sharing one captured
// in-flight counter. acquire reports true and increments the counter if
// fewer than limit admissions are currently in flight; otherwise it refuses
// and leaves the counter untouched. release decrements the counter, clamped
// at zero so a stray extra release cannot drive it negative.
//
// This version is single-goroutine: nothing guards inFlight, so calling
// acquire or release from multiple goroutines races on the captured counter.
// Production wraps both closures in a sync.Mutex, or builds the gate on a
// buffered channel used as a semaphore.
func NewGate(limit int) (acquire func() bool, release func()) {
	inFlight := 0

	acquire = func() bool {
		if inFlight >= limit {
			return false
		}
		inFlight++
		return true
	}

	release = func() {
		if inFlight > 0 {
			inFlight--
		}
	}

	return acquire, release
}
```

### Tests

Create `gate_test.go`:

```go
package admission

import "testing"

func TestGateEnforcesLimitAndReleaseFreesASlot(t *testing.T) {
	acquire, release := NewGate(2)

	tests := []struct {
		name string
		want bool
	}{
		{"acquire 1 of 2", true},
		{"acquire 2 of 2", true},
		{"acquire 3rd refused", false},
	}

	for _, tc := range tests {
		if got := acquire(); got != tc.want {
			t.Fatalf("%s: acquire() = %v, want %v", tc.name, got, tc.want)
		}
	}

	release()
	if !acquire() {
		t.Fatal("acquire after release: got false, want true (release must free a slot)")
	}
	if acquire() {
		t.Fatal("acquire beyond refilled limit: got true, want false")
	}
}

func TestGateReleaseClampsAtZero(t *testing.T) {
	acquire, release := NewGate(1)

	release() // release with nothing in flight must not go negative
	if !acquire() {
		t.Fatal("acquire after no-op release: got false, want true")
	}
	if acquire() {
		t.Fatal("second acquire: got true, want false (limit is 1)")
	}
}

func TestTwoGatesDoNotShareInFlightCounter(t *testing.T) {
	acquireA, _ := NewGate(1)
	acquireB, _ := NewGate(1)

	if !acquireA() {
		t.Fatal("gate A first acquire: got false, want true")
	}
	if !acquireB() {
		t.Fatal("gate B first acquire: got false, want true — gates must not share captured state")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The first test walks the gate to its limit, confirms the next `acquire` is
refused, then confirms `release` frees exactly one slot rather than resetting
the counter entirely. The clamp test is the defensive case: an extra `release`
must not manufacture capacity that was never there. The isolation test is the
same structural guarantee every stateful factory in this lesson relies on —
two calls to `NewGate` never share the variable each closure pair closes over.

## Resources

- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how `acquire` and `release` share one captured `inFlight`.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — how production guards the shared counter under concurrent calls.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-key-scoped-debounce-detector.md](14-key-scoped-debounce-detector.md) | Next: [16-per-tenant-request-sampler-closure.md](16-per-tenant-request-sampler-closure.md)
