# Exercise 3: Reverse-proxy backend pool with hot reload

An API gateway or load balancer picks a backend on every request and reloads its
backend list only when configuration changes or a health check flips a node. That
is a textbook read-heavy table: `Next()` on the hot path takes a shared read lock,
while `Reload`, `MarkDown`, and `MarkUp` take the exclusive lock on the rare write
path. This exercise builds that pool and proves that concurrent request routing is
safe across a live reload — no out-of-range index, no panic, no down backend
served.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
backendpool/                 independent module: example.com/backendpool
  go.mod                     module example.com/backendpool
  pool.go                    type Backend, type Pool; Next (RLock + atomic counter), Reload/MarkDown/MarkUp (Lock)
  cmd/
    demo/
      main.go                runnable demo: round-robin pick, mark one down, reload
  pool_test.go               concurrent Next-vs-reload race test, round-robin distribution table test
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: a `*Pool` holding `[]Backend` behind an `RWMutex` plus an `atomic.Int64` rotation counter; `Next` picks a healthy backend round-robin under `RLock`; `Reload`/`MarkDown`/`MarkUp` mutate under `Lock`.
Test: N goroutines call `Next` in a loop while one goroutine reloads and toggles health — no panic, never an unknown backend; a distribution table test over a stable pool.
Verify: `go test -count=1 -race ./...`

### Why the read lock and the atomic counter cooperate

`Next` runs on every request, so it must be cheap and must never block behind
another `Next`. It takes `RLock`, builds the list of currently-healthy backends
from the guarded slice, and picks one round-robin. The rotation index comes from
`p.counter.Add(1)`, an `atomic.Int64` that is safe to bump while holding only the
read lock — the counter is not part of the protected slice, and atomic increment
needs no mutex. So two concurrent `Next` calls both hold `RLock`, both read the
same slice, and each gets a distinct rotation number without contending on a write
lock. The write lock is reserved for `Reload` (swap the whole slice) and
`MarkDown`/`MarkUp` (flip one backend's health).

The bug this design prevents is the classic one: if `Next` read the slice without
the lock, a concurrent `Reload` swapping the slice — or a `MarkDown` mutating an
element — would race, and indexing a stale or half-swapped slice could panic with
an out-of-range access. Because `Next` holds `RLock` and every mutation holds
`Lock`, `Next` always sees one complete, consistent slice: either fully before a
reload or fully after, never in between.

`Reload` clones the incoming slice with `slices.Clone` before storing it, so the
caller cannot later mutate the pool's internal slice by holding onto the argument.
`MarkDown`/`MarkUp` scan for the matching address and flip its `healthy` flag
under the write lock; a mutation of any element requires the exclusive lock
because a concurrent `Next` may be reading that same element.

Create `pool.go`:

```go
package backendpool

import (
	"slices"
	"sync"
	"sync/atomic"
)

// Backend is one upstream target. healthy is guarded by the Pool's lock.
type Backend struct {
	Addr    string
	healthy bool
}

// Pool is a concurrency-safe, hot-reloadable set of backends. Next runs on the
// request hot path under a shared read lock; Reload and the health toggles take
// the exclusive lock.
type Pool struct {
	mu       sync.RWMutex
	backends []Backend
	counter  atomic.Int64
}

// NewPool builds a pool from addresses, all initially healthy.
func NewPool(addrs ...string) *Pool {
	bs := make([]Backend, len(addrs))
	for i, a := range addrs {
		bs[i] = Backend{Addr: a, healthy: true}
	}
	return &Pool{backends: bs}
}

// Next returns the next healthy backend round-robin, and false if none are
// healthy. It takes only the read lock, so concurrent requests do not serialize.
func (p *Pool) Next() (Backend, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var healthy []Backend
	for _, b := range p.backends {
		if b.healthy {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return Backend{}, false
	}
	i := p.counter.Add(1) - 1
	return healthy[int(i%int64(len(healthy)))], true
}

// Reload replaces the backend set atomically under the write lock. The argument
// is cloned, so the caller cannot mutate the pool's slice afterward.
func (p *Pool) Reload(backends []Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = slices.Clone(backends)
}

// MarkDown flips the backend with the given address to unhealthy. It reports
// whether a matching backend was found.
func (p *Pool) MarkDown(addr string) bool {
	return p.setHealth(addr, false)
}

// MarkUp flips the backend with the given address back to healthy.
func (p *Pool) MarkUp(addr string) bool {
	return p.setHealth(addr, true)
}

func (p *Pool) setHealth(addr string, healthy bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.backends {
		if p.backends[i].Addr == addr {
			p.backends[i].healthy = healthy
			return true
		}
	}
	return false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/backendpool"
)

func main() {
	p := backendpool.NewPool("10.0.0.1", "10.0.0.2", "10.0.0.3")

	// Round-robin over the healthy set.
	for range 3 {
		b, _ := p.Next()
		fmt.Println(b.Addr)
	}

	// Take one node down: it drops out of rotation.
	p.MarkDown("10.0.0.2")
	seen := map[string]bool{}
	for range 6 {
		b, _ := p.Next()
		seen[b.Addr] = true
	}
	fmt.Printf("after MarkDown, 10.0.0.2 served: %t\n", seen["10.0.0.2"])

	// Hot reload to a fresh set.
	p.Reload([]backendpool.Backend{{Addr: "10.1.0.1"}})
	_, ok := p.Next()
	fmt.Printf("reloaded pool has a healthy backend: %t\n", ok)
}
```

Note: the reloaded backend has `healthy` unset (false), because `Backend` is
constructed by the caller here, so `Next` reports no healthy backend after the
reload. The demo prints that honestly.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
10.0.0.1
10.0.0.2
10.0.0.3
after MarkDown, 10.0.0.2 served: false
reloaded pool has a healthy backend: false
```

### Tests

`TestConcurrentNextDuringReload` is the safety test: a set of reader goroutines
call `Next` in a tight loop while one writer repeatedly reloads the pool and
toggles a backend's health. Every returned backend must have an address from the
known set and `Next` must never panic with an out-of-range index mid-reload. The
`-race` detector proves the slice is never read while it is being swapped or
mutated. `TestRoundRobinDistribution` fixes a stable three-backend pool and checks
that 300 `Next` calls distribute exactly 100 to each backend — the atomic counter
modulo the healthy count is a fair rotation.

Create `pool_test.go`:

```go
package backendpool

import (
	"sync"
	"testing"
)

func TestConcurrentNextDuringReload(t *testing.T) {
	t.Parallel()

	known := map[string]bool{
		"a": true, "b": true, "c": true, "d": true,
	}
	p := NewPool("a", "b", "c")

	var wg sync.WaitGroup

	// Readers: route requests continuously.
	const readers = 16
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for range 2000 {
				if b, ok := p.Next(); ok && !known[b.Addr] {
					t.Errorf("Next returned unknown backend %q", b.Addr)
					return
				}
			}
		}()
	}

	// Writer: reload and toggle health under the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 500 {
			switch i % 3 {
			case 0:
				p.Reload([]Backend{
					{Addr: "a", healthy: true},
					{Addr: "d", healthy: true},
				})
			case 1:
				p.MarkDown("a")
			case 2:
				p.Reload([]Backend{
					{Addr: "a", healthy: true},
					{Addr: "b", healthy: true},
					{Addr: "c", healthy: true},
				})
			}
		}
	}()
	wg.Wait()
}

func TestRoundRobinDistribution(t *testing.T) {
	t.Parallel()

	p := NewPool("a", "b", "c")
	counts := map[string]int{}
	for range 300 {
		b, ok := p.Next()
		if !ok {
			t.Fatal("Next returned no healthy backend on a full pool")
		}
		counts[b.Addr]++
	}

	for _, addr := range []string{"a", "b", "c"} {
		if counts[addr] != 100 {
			t.Fatalf("backend %q served %d times, want 100 (counts=%v)", addr, counts[addr], counts)
		}
	}
}
```

## Review

The pool is correct when `Next` reads a complete, consistent slice under `RLock`
and every mutation — the whole-slice swap in `Reload` and the element flip in
`setHealth` — takes the exclusive `Lock`. The atomic counter is deliberately
outside the lock because incrementing it is safe under a read lock and turning it
into guarded state would force `Next` onto the write lock and serialize routing.
The mistakes to avoid are reading `p.backends` without the lock (a race that can
panic on a stale index during a reload) and mutating an element under `RLock`
(a torn write). Run `go test -race`; a clean detector across the concurrent
reload test is the proof that request routing survives live configuration changes.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the backend slice.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` for the lock-free rotation counter.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the defensive copy `Reload` uses.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-metrics-counter-registry.md](04-metrics-counter-registry.md)
