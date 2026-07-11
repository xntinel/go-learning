# Exercise 9: Enforce Exactly-Once Processing with CompareAndSwap

Webhook deliveries and job queues are at-least-once: the same event arrives twice,
sometimes simultaneously on two workers. A naive `if !seen[key] { seen[key]=true;
process() }` guard is a check-then-act race — two goroutines both read `false` and
both process. This module builds an idempotency guard that uses `CompareAndSwap` so
exactly one goroutine wins the new→processing transition, plus a `sync.Once` lazy
initializer for an expensive shared resource, and proves exactly-once under `-race`.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
idemguard/                 independent module: example.com/idemguard
  go.mod                   module example.com/idemguard
  idemguard.go             Guard (sync.Map of *atomic.Int32 states) with CompareAndSwap; Resource lazy init via sync.Once
  cmd/
    demo/
      main.go              runnable demo: duplicate deliveries processed once; lazy resource built once
  idemguard_test.go        many goroutines, one key: side-effect runs exactly once, under -race; Once single-init
```

- Files: `idemguard.go`, `cmd/demo/main.go`, `idemguard_test.go`.
- Implement: a `Guard` keyed by event ID whose per-key state transitions `new -> processing` via `atomic.Int32.CompareAndSwap`; a `Resource` built lazily via `sync.Once`.
- Test: many goroutines call `Process` with the same key; assert the side-effect runs exactly once and only one CAS wins, under `-race`; a separate test hammers the `Once` initializer and asserts a single, non-nil init.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/idemguard/cmd/demo
cd ~/go-exercises/idemguard
go mod init example.com/idemguard
```

### Check-then-act is the failure mode; CAS is the fix

The bug is TOCTOU: time-of-check to time-of-use. `if !seen[key]` is the check;
`seen[key]=true; process()` is the act. Between the two, a second goroutine runs
the same check, still sees `false`, and both proceed to process. Adding a mutex
around the whole block *would* fix correctness but serializes every key through one
lock and still requires you to hold the lock across `process()` or manage a
separate "in progress" flag. `CompareAndSwap` collapses the check and the act into
one indivisible step: `state.CompareAndSwap(stateNew, stateProcessing)` atomically
tests that the state is still `new` and, if so, sets it to `processing`, returning
`true` to exactly one caller. Every other concurrent caller sees the state already
moved and gets `false`, so precisely one goroutine runs the side-effect. This is
the canonical exactly-once primitive.

Per-key state lives in a `sync.Map` from key to `*atomic.Int32`. `sync.Map` is
suited to this "write-once, read-many keys, disjoint per-key" access pattern and
handles concurrent key insertion via `LoadOrStore`, which itself is atomic — two
goroutines racing to register the same new key both end up with the *same*
`*atomic.Int32`, so the subsequent CAS is a fair race with a single winner. The
pointer matters: the state must be shared, so the map stores a `*atomic.Int32`, not
a value (copying an atomic would defeat it).

The `sync.Once` half solves a related one-time problem: initialize an expensive
shared resource (a DB pool, a compiled ruleset) lazily, exactly once, no matter how
many goroutines request it first. `once.Do(f)` runs `f` exactly once and blocks all
other callers until it completes, so every caller sees a fully constructed
resource. It is the check-then-act fix specialized to "initialize once".

Create `idemguard.go`:

```go
package idemguard

import (
	"sync"
	"sync/atomic"
)

const (
	stateNew        int32 = 0
	stateProcessing int32 = 1
)

// Guard enforces that each event key is processed at most once, even when
// duplicate deliveries race on multiple goroutines.
type Guard struct {
	states sync.Map // key -> *atomic.Int32
}

func NewGuard() *Guard {
	return &Guard{}
}

// Process runs fn for key exactly once across all concurrent callers. The winner
// of the new->processing CompareAndSwap runs fn and returns true; every loser
// returns false without running fn.
func (g *Guard) Process(key string, fn func()) bool {
	v, _ := g.states.LoadOrStore(key, new(atomic.Int32))
	state := v.(*atomic.Int32)

	if !state.CompareAndSwap(stateNew, stateProcessing) {
		return false // another goroutine already claimed this key
	}
	fn()
	return true
}

// Resource is an expensive shared value built lazily and exactly once.
type Resource struct {
	once  sync.Once
	value *Pool
}

// Pool stands in for an expensive-to-build shared resource.
type Pool struct {
	Conns int
}

// Get returns the resource, building it on first call. build runs at most once
// no matter how many goroutines call Get concurrently.
func (r *Resource) Get(build func() *Pool) *Pool {
	r.once.Do(func() {
		r.value = build()
	})
	return r.value
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/idemguard"
)

func main() {
	g := idemguard.NewGuard()
	var processed atomic.Int64

	// The same event delivered five times concurrently.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Process("evt-42", func() { processed.Add(1) })
		}()
	}
	wg.Wait()
	fmt.Printf("event processed %d time(s)\n", processed.Load())

	// A shared resource built lazily exactly once.
	var res idemguard.Resource
	var builds atomic.Int64
	build := func() *idemguard.Pool {
		builds.Add(1)
		return &idemguard.Pool{Conns: 10}
	}
	var wg2 sync.WaitGroup
	for range 5 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			_ = res.Get(build)
		}()
	}
	wg2.Wait()
	fmt.Printf("resource built %d time(s), conns=%d\n", builds.Load(), res.Get(build).Conns)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event processed 1 time(s)
resource built 1 time(s), conns=10
```

### Tests

`TestProcessExactlyOnce` is the core test: many goroutines call `Process` with the
same key, and it asserts the side-effect counter is exactly 1 and exactly one call
returned `true` (won the CAS), under `-race`. `TestProcessDistinctKeys` confirms
different keys each process independently. `TestResourceInitializedOnce` hammers the
`sync.Once` initializer from many goroutines and asserts a single, non-nil init.

Create `idemguard_test.go`:

```go
package idemguard

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestProcessExactlyOnce(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	var sideEffects atomic.Int64
	var winners atomic.Int64

	const n = 500
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.Process("evt-1", func() { sideEffects.Add(1) }) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := sideEffects.Load(); got != 1 {
		t.Errorf("side-effect ran %d times, want exactly 1", got)
	}
	if got := winners.Load(); got != 1 {
		t.Errorf("%d goroutines won the CAS, want exactly 1", got)
	}
}

func TestProcessDistinctKeys(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	var sideEffects atomic.Int64

	const keys = 100
	var wg sync.WaitGroup
	for i := range keys {
		key := string(rune('a')) + string(rune(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Process(key, func() { sideEffects.Add(1) })
		}()
	}
	wg.Wait()

	if got := sideEffects.Load(); got != keys {
		t.Fatalf("side-effects = %d for %d distinct keys, want %d", got, keys, keys)
	}
}

func TestResourceInitializedOnce(t *testing.T) {
	t.Parallel()

	var res Resource
	var builds atomic.Int64
	build := func() *Pool {
		builds.Add(1)
		return &Pool{Conns: 10}
	}

	const n = 500
	var wg sync.WaitGroup
	results := make([]*Pool, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = res.Get(build)
		}()
	}
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Errorf("build ran %d times, want exactly 1", got)
	}
	for i, p := range results {
		if p == nil {
			t.Fatalf("goroutine %d observed a nil resource", i)
		}
		if p != results[0] {
			t.Fatalf("goroutine %d observed a different resource instance", i)
		}
	}
}
```

## Review

The guard is correct when, under any number of concurrent duplicate deliveries of
one key, the side-effect runs exactly once and exactly one caller wins — with a
clean `-race` run. The mistake it pins is the check-then-act guard
(`if !seen[key] { seen[key]=true; process() }`): under concurrency two goroutines
both pass the check and both process. `CompareAndSwap` collapses the check and the
act into one atomic transition so precisely one wins. The `sync.Map.LoadOrStore`
detail matters — racing registrations of a new key resolve to the *same*
`*atomic.Int32`, which is why the subsequent CAS is a fair single-winner race; store
a pointer, never a copied atomic. `sync.Once` is the same fix specialized to
one-time initialization: `Do` runs the builder once and blocks the rest until the
resource is fully constructed.

## Resources

- [`sync/atomic` — `Int32.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Int32.CompareAndSwap) — the atomic check-then-act.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — run an initializer exactly once, blocking concurrent callers.
- [`sync.Map`](https://pkg.go.dev/sync#Map) — `LoadOrStore` for concurrent, write-once-per-key state.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-deterministic-race-tests-synctest.md](10-deterministic-race-tests-synctest.md)
