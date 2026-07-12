# Exercise 5: Lock-Rank Assertions — a lockdep-Lite Debug Mutex

A lock-ordering discipline that lives in a comment decays the first time
someone adds a code path without reading it. The Linux kernel (lockdep) and
CockroachDB solve this by making the ordering *executable*: every lock carries
a rank, and acquiring out of rank panics immediately with both lock names in
the message. This exercise builds that wrapper for a realistic
registry-tenant-session hierarchy.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
lockrank/                  independent module: example.com/lockrank
  go.mod
  lockrank.go              type Rank, RankedMutex, Trace; New, NewTrace,
                           NewUncheckedTrace; Lock/Unlock with rank assertions
  cmd/
    demo/
      main.go              runnable demo: correct-order walk of the
                           registry -> tenant -> session hierarchy, then a
                           recovered violation showing the panic message
  lockrank_test.go         panic-message assertions via recover, correct-order
                           concurrency under -race, unchecked-mode behavior
```

- Files: `lockrank.go`, `cmd/demo/main.go`, `lockrank_test.go`.
- Implement: `RankedMutex` with a name and a fixed integer rank; `Trace`, the explicit per-call-chain record of held locks; `Lock(trace)` that panics when the trace already holds a mutex of equal or higher rank; `Unlock(trace)` that maintains the trace and panics on unlock-of-unheld.
- Test: a recover-based helper asserting the panic names both locks; correct-order acquisition passing; the correctly-ordered path run concurrently under `-race`; unchecked traces skipping the assertion while preserving exclusion.
- Verify: `go test -count=1 -race ./...`

### Turning a latent ordering bug into an immediate panic

An ordering violation only deadlocks when the timing cooperates: the wrong-
order path must interleave with a right-order path at exactly the wrong
moment. That is why these bugs ship — the code review missed it, the tests
never hit the interleaving, and the first manifestation is a production wedge
at 3 a.m. Rank assertions change the detection condition from "the timing
cooperated" to "the code path executed at all": if any goroutine ever acquires
rank 10 while holding rank 20, it panics with a stack trace on the spot, in
the first CI run that exercises the path, whether or not any other goroutine
was contending.

The hierarchy here is the shape found in real multi-tenant servers: a global
registry lock (rank 10) may be taken before a per-tenant lock (rank 20), which
may be taken before a per-session lock (rank 30) — never the reverse. Ranks
are strictly increasing along any acquisition chain; *equal* ranks are also
rejected, because two locks at the same rank (two sessions, say) have no
defined order between them — code that needs two sessions at once must either
order them by a finer key or restructure (exactly the situation exercises 1
and 6 solve with ID and shard-index ordering).

The Go-specific design problem is knowing what the current goroutine already
holds. The kernel's lockdep tracks held locks in per-thread state; Go
deliberately has no goroutine-local storage. So the held-set is an explicit
`Trace` value passed down the call chain — one per request or per worker
iteration, never shared between goroutines. This costs a parameter, and pays
twice: the checker gets its data structure, and every function signature that
takes a `*Trace` self-documents as "this path acquires locks", which reviewers
learn to read the way they read a `context.Context` parameter.

Two contract details matter. The assertion runs *before* `mu.Lock()`, so a
violation panics without acquiring — state stays consistent for the recover
path, and the panic cannot itself deadlock the process during unwinding.
And `Unlock` verifies the mutex is actually on the trace: releasing a lock
you do not hold is the same class of corruption as `sync.Mutex.Unlock` on an
unlocked mutex, which also panics.

The `checked` flag on the trace is the debug/production toggle: an unchecked
trace skips the rank scan but still tracks held locks so `Unlock` bookkeeping
works. If even that bookkeeping is too much for a hot path, the production-
grade version of this pattern swaps the whole wrapper for a bare `sync.Mutex`
behind a build tag (`//go:build locktrace`), the way CockroachDB compiles its
assertions out of release builds; the API shape stays the same and the type
disappears at compile time.

Create `lockrank.go`:

```go
// Package lockrank enforces a lock-acquisition hierarchy at runtime,
// lockdep-style: every mutex has a rank, and a goroutine may only acquire
// mutexes of strictly increasing rank along one call chain. Violations
// panic immediately with both lock names, turning a latent deadlock into
// a stack-traced CI failure.
//
// Go has no goroutine-local storage, so the set of held locks is carried
// in an explicit *Trace passed through the call chain. A Trace must not
// be shared between goroutines.
package lockrank

import (
	"fmt"
	"slices"
	"sync"
)

// Rank is a lock's fixed position in the global acquisition hierarchy.
// Lower ranks are acquired first: registry (10) before tenant (20)
// before session (30).
type Rank int

// RankedMutex is a mutex with a name and a rank. Create with New.
type RankedMutex struct {
	name string
	rank Rank
	mu   sync.Mutex
}

// New returns a RankedMutex with the given diagnostic name and rank.
func New(name string, rank Rank) *RankedMutex {
	return &RankedMutex{name: name, rank: rank}
}

// Trace records the ranked mutexes held along one call chain, in
// acquisition order. It is not safe for concurrent use: give each
// goroutine (each request, each worker iteration) its own Trace.
type Trace struct {
	checked bool
	held    []*RankedMutex
}

// NewTrace returns a Trace with rank checking enabled — the CI/debug
// configuration.
func NewTrace() *Trace {
	return &Trace{checked: true}
}

// NewUncheckedTrace returns a Trace that skips the rank assertion but
// still tracks held locks for Unlock bookkeeping — the production
// configuration when the O(held) scan matters.
func NewUncheckedTrace() *Trace {
	return &Trace{}
}

// Lock acquires the mutex. If the trace is checked and already holds a
// mutex of equal or higher rank, Lock panics BEFORE acquiring, naming
// both locks. Equal ranks are rejected too: two locks at the same rank
// have no defined order between them.
func (m *RankedMutex) Lock(tr *Trace) {
	if tr.checked {
		for _, h := range tr.held {
			if h.rank >= m.rank {
				panic(fmt.Sprintf(
					"lockrank: acquiring %q (rank %d) while holding %q (rank %d); acquire in strictly increasing rank order",
					m.name, m.rank, h.name, h.rank))
			}
		}
	}
	m.mu.Lock()
	tr.held = append(tr.held, m)
}

// Unlock releases the mutex and removes it from the trace. It panics if
// the mutex is not held on this trace.
func (m *RankedMutex) Unlock(tr *Trace) {
	for i := len(tr.held) - 1; i >= 0; i-- {
		if tr.held[i] == m {
			tr.held = slices.Delete(tr.held, i, i+1)
			m.mu.Unlock()
			return
		}
	}
	panic(fmt.Sprintf("lockrank: unlock of %q, which this trace does not hold", m.name))
}

// Held reports how many ranked mutexes the trace currently holds.
func (tr *Trace) Held() int {
	return len(tr.held)
}
```

### The runnable demo

The demo walks the hierarchy correctly once, then deliberately violates it
inside a `recover` so you can read the exact message CI would print. Note the
violation is caught with *zero* contention — no second goroutine, no unlucky
interleaving required.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lockrank"
)

func main() {
	registry := lockrank.New("registry", 10)
	tenant := lockrank.New("tenant:acme", 20)
	session := lockrank.New("session:s-9f2", 30)

	tr := lockrank.NewTrace()
	registry.Lock(tr)
	tenant.Lock(tr)
	session.Lock(tr)
	fmt.Printf("correct order held %d locks: registry -> tenant -> session\n", tr.Held())
	session.Unlock(tr)
	tenant.Unlock(tr)
	registry.Unlock(tr)

	// The violation: a code path that grabs the tenant lock and then
	// reaches back up for the registry. With plain mutexes this is a
	// latent deadlock waiting for the right interleaving; here it panics
	// deterministically on first execution.
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("violation caught:", r)
			}
		}()
		tr2 := lockrank.NewTrace()
		tenant.Lock(tr2)
		defer tenant.Unlock(tr2)
		registry.Lock(tr2) // panics
	}()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct order held 3 locks: registry -> tenant -> session
violation caught: lockrank: acquiring "registry" (rank 10) while holding "tenant:acme" (rank 20); acquire in strictly increasing rank order
```

### Tests

`mustPanic` centralizes the recover dance and returns the message so tests can
assert it names *both* locks — a panic that says only "lock ordering violated"
sends the on-call spelunking; one that says which lock, while holding which,
is actionable from the CI log alone. The concurrency test runs the correctly-
ordered path from many goroutines, each with its own `Trace`, under `-race`:
it proves the wrapper still provides real mutual exclusion (a non-atomic
counter guarded by the session lock) and that correct code never trips the
checker. The unchecked test pins the production configuration: wrong-order
acquisition does not panic (it is a *latent* hazard, deliberately tolerated
when assertions are off) and unlock bookkeeping still works.

Create `lockrank_test.go`:

```go
package lockrank

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// mustPanic runs fn and returns the recovered panic message, failing the
// test if fn does not panic.
func mustPanic(t *testing.T, fn func()) string {
	t.Helper()
	var msg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				msg = fmt.Sprint(r)
			}
		}()
		fn()
	}()
	if msg == "" {
		t.Fatal("expected panic, got none")
	}
	return msg
}

func TestWrongOrderPanicsNamingBothLocks(t *testing.T) {
	t.Parallel()

	registry := New("registry", 10)
	tenant := New("tenant:acme", 20)

	msg := mustPanic(t, func() {
		tr := NewTrace()
		tenant.Lock(tr)
		defer tenant.Unlock(tr)
		registry.Lock(tr)
	})

	for _, want := range []string{"registry", "tenant:acme", "rank 10", "rank 20"} {
		if !strings.Contains(msg, want) {
			t.Errorf("panic message %q does not mention %q", msg, want)
		}
	}
}

func TestEqualRankPanics(t *testing.T) {
	t.Parallel()

	s1 := New("session:a", 30)
	s2 := New("session:b", 30)

	msg := mustPanic(t, func() {
		tr := NewTrace()
		s1.Lock(tr)
		defer s1.Unlock(tr)
		s2.Lock(tr)
	})
	if !strings.Contains(msg, "session:a") || !strings.Contains(msg, "session:b") {
		t.Errorf("panic message %q should name both same-rank locks", msg)
	}
}

func TestCorrectOrderPasses(t *testing.T) {
	t.Parallel()

	registry := New("registry", 10)
	tenant := New("tenant:acme", 20)
	session := New("session:s-1", 30)

	tr := NewTrace()
	registry.Lock(tr)
	tenant.Lock(tr)
	session.Lock(tr)
	if got := tr.Held(); got != 3 {
		t.Fatalf("Held() = %d, want 3", got)
	}
	session.Unlock(tr)
	tenant.Unlock(tr)
	registry.Unlock(tr)
	if got := tr.Held(); got != 0 {
		t.Fatalf("Held() after full unlock = %d, want 0", got)
	}

	// Releasing the higher rank re-opens it: down-then-up again is legal.
	tenant.Lock(tr)
	tenant.Unlock(tr)
	registry.Lock(tr)
	registry.Unlock(tr)
}

func TestUnlockNotHeldPanics(t *testing.T) {
	t.Parallel()

	m := New("registry", 10)
	msg := mustPanic(t, func() {
		m.Unlock(NewTrace())
	})
	if !strings.Contains(msg, "registry") {
		t.Errorf("panic message %q should name the lock", msg)
	}
}

func TestUncheckedTraceSkipsAssertion(t *testing.T) {
	t.Parallel()

	registry := New("registry", 10)
	tenant := New("tenant:acme", 20)

	// Wrong order, unchecked: no panic (the hazard is latent, tolerated
	// in production mode), and bookkeeping still balances.
	tr := NewUncheckedTrace()
	tenant.Lock(tr)
	registry.Lock(tr)
	if got := tr.Held(); got != 2 {
		t.Fatalf("Held() = %d, want 2", got)
	}
	registry.Unlock(tr)
	tenant.Unlock(tr)
	if got := tr.Held(); got != 0 {
		t.Fatalf("Held() = %d, want 0", got)
	}
}

func TestConcurrentCorrectOrderUnderRace(t *testing.T) {
	t.Parallel()

	registry := New("registry", 10)
	tenant := New("tenant:acme", 20)
	session := New("session:s-1", 30)

	var inside int // guarded by session; -race verifies the exclusion
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				tr := NewTrace() // one trace per goroutine, never shared
				registry.Lock(tr)
				tenant.Lock(tr)
				session.Lock(tr)
				inside++
				if inside != 1 {
					t.Errorf("mutual exclusion violated: inside = %d", inside)
				}
				inside--
				session.Unlock(tr)
				tenant.Unlock(tr)
				registry.Unlock(tr)
			}
		}()
	}
	wg.Wait()
}

func ExampleRankedMutex_Lock() {
	registry := New("registry", 10)
	tenant := New("tenant:acme", 20)

	tr := NewTrace()
	registry.Lock(tr)
	tenant.Lock(tr)
	fmt.Println("held:", tr.Held())
	tenant.Unlock(tr)
	registry.Unlock(tr)
	// Output: held: 2
}
```

## Review

The checker's power comes from what it does *not* require: no contention, no
second goroutine, no lucky interleaving. Rank assertions convert "this
ordering could deadlock under load" into "this code path panics every time it
runs", which moves detection from production to the first CI run. The
essential invariants to re-check in your implementation: the assertion runs
before acquisition (so a violation never leaves a lock held), equal ranks are
rejected (same-rank locks have no order — order them by a finer key or do not
hold two), and each `Trace` belongs to exactly one goroutine (sharing one
between goroutines is itself a data race, and `-race` will say so).

The limits are worth knowing. Rank checking finds ordering violations along a
single call chain; it cannot see cycles through non-mutex resources (a pool
slot, a channel — exercise 8's territory), and the explicit `Trace` parameter
must actually be threaded through, which is a per-team convention like
`context.Context`. When the parameter cost is unacceptable, keep the checked
type in CI builds behind a build tag and alias it to `sync.Mutex` in release
builds — same discipline, zero release overhead. Confirm with
`go test -count=1 -race ./...`.

## Resources

- [Runtime locking correctness validator (Linux lockdep)](https://docs.kernel.org/locking/lockdep-design.html) — the rank/class design this exercise miniaturizes.
- [sync package — Mutex](https://pkg.go.dev/sync#Mutex) — the underlying lock; Unlock-of-unlocked panics, the contract Unlock mirrors.
- [slices package — Delete](https://pkg.go.dev/slices#Delete) — the held-set removal used in Unlock.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-sharded-store-two-shard-move.md](06-sharded-store-two-shard-move.md)
