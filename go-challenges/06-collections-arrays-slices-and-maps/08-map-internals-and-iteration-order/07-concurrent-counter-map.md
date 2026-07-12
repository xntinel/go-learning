# Exercise 7: Fix `fatal error: concurrent map writes` in a Shared Request Counter

A hit counter shared across handler goroutines is the textbook place a plain map kills
a process. This module starts from the broken version the runtime crashes on, then wraps
the map in an `RWMutex` so `Inc`, `Get`, and `Snapshot` are safe under concurrency — and
proves it with `-race`.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
counter/                   independent module: example.com/counter
  go.mod                   go 1.26
  counter.go               type Counter (RWMutex + map); Inc, Get, Snapshot, Total
  cmd/
    demo/
      main.go              N goroutines increment, prints the total
  counter_test.go          -race concurrency test, snapshot independence, parallel read
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: `Counter` over `sync.RWMutex` + `map[string]int` with `Inc(key)`,
  `Get(key) int`, `Snapshot() map[string]int` (a clone), and `Total() int`.
- Test: N goroutines each doing many `Inc`, total equals `N*iters` under `-race`;
  `Snapshot` returns an independent copy; a parallel reader while writers run.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/07-concurrent-counter-map/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/07-concurrent-counter-map
```

### The broken version, and why it is worse than a race warning

The naive counter is a bare map with an `Inc` method:

```go
// BROKEN: do not use. A map is not safe for concurrent writes.
type Counter struct {
	hits map[string]int
}

func (c *Counter) Inc(key string) {
	c.hits[key]++ // read-modify-write on a shared map, no lock
}
```

Run this from two goroutines and you do not get a subtly wrong count — you get the
runtime aborting the entire process:

```text
fatal error: concurrent map writes
```

This is the crucial distinction. A slice data race under `-race` is a *warning* you can
choose to ignore in production (the binary keeps running, possibly with corrupt data). A
concurrent map write is detected by the runtime *without* `-race` and it is *fatal*: the
process calls `os.Exit`-equivalent abort logic. You cannot `recover` it. One buggy
handler goroutine incrementing a shared counter takes down every in-flight request in
the process. So this is not a lint-nicety; it is a latent crash, and the only acceptable
fix is real synchronization proven by `-race`.

### The fixed version

Wrap the map in a `sync.RWMutex`. Writes (`Inc`) take the exclusive `Lock`; reads
(`Get`, `Total`, `Snapshot`) take the shared `RLock`, so many readers proceed in
parallel while any writer is exclusive. The subtle rule is `Snapshot`: it must return
`maps.Clone(c.hits)` built *under the read lock*, not the live map. Returning the
internal map would hand callers a view they can mutate (corrupting shared state) and,
worse, iterate while a writer holds the write lock elsewhere — which is
`concurrent map iteration and map write`, fatal again. Cloning under the lock produces a
consistent, independent snapshot the caller owns.

Create `counter.go`:

```go
package counter

import (
	"maps"
	"sync"
)

// Counter is a concurrency-safe hit counter. Every access goes through the mutex:
// writes take the exclusive lock, reads the shared read lock.
type Counter struct {
	mu   sync.RWMutex
	hits map[string]int
}

// New returns an empty Counter ready for concurrent use.
func New() *Counter {
	return &Counter{hits: make(map[string]int)}
}

// Inc atomically increments the counter for key.
func (c *Counter) Inc(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits[key]++
}

// Get returns the current count for key (0 if absent).
func (c *Counter) Get(key string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits[key]
}

// Total returns the sum of all counters.
func (c *Counter) Total() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sum := 0
	for _, n := range c.hits {
		sum += n
	}
	return sum
}

// Snapshot returns an independent copy of the counters, cloned under the read
// lock so the caller can read and mutate it without touching shared state.
func (c *Counter) Snapshot() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return maps.Clone(c.hits)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/counter"
)

func main() {
	c := counter.New()
	const goroutines, perGoroutine = 8, 1000

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				c.Inc("requests")
			}
		}()
	}
	wg.Wait()

	fmt.Println("requests:", c.Get("requests"))
	fmt.Println("total:", c.Total())
}
```

Run it (with the race detector, to prove it is clean):

```bash
go run -race ./cmd/demo
```

Expected output:

```
requests: 8000
total: 8000
```

### Tests

`TestConcurrentIncrementIsRaceFree` is the core: `N` goroutines each `Inc` a shared key
`iters` times, and after the `WaitGroup` the total must equal exactly `N*iters` — a
lost update (the symptom of an unsynchronized read-modify-write) would make it low. It
must pass under `-race`. `TestSnapshotIsIndependent` mutates the returned snapshot and
checks the counter is unchanged. `TestParallelReadWhileWriting` runs readers in a
`t.Parallel` subtest alongside writers to exercise `RLock`/`Lock` interleaving.

Create `counter_test.go`:

```go
package counter

import (
	"sync"
	"testing"
)

func TestConcurrentIncrementIsRaceFree(t *testing.T) {
	t.Parallel()

	const goroutines, iters = 50, 200
	c := New()

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iters {
				c.Inc("k")
			}
		}()
	}
	wg.Wait()

	if got := c.Get("k"); got != goroutines*iters {
		t.Fatalf("Get(k) = %d, want %d (lost updates indicate a race)", got, goroutines*iters)
	}
	if got := c.Total(); got != goroutines*iters {
		t.Fatalf("Total = %d, want %d", got, goroutines*iters)
	}
}

func TestSnapshotIsIndependent(t *testing.T) {
	t.Parallel()

	c := New()
	c.Inc("a")
	c.Inc("a")
	c.Inc("b")

	snap := c.Snapshot()
	snap["a"] = 999   // mutate the copy
	snap["c"] = 1     // add to the copy
	delete(snap, "b") // remove from the copy

	if c.Get("a") != 2 {
		t.Fatalf("Get(a) = %d, want 2; snapshot mutation leaked", c.Get("a"))
	}
	if c.Get("b") != 1 {
		t.Fatalf("Get(b) = %d, want 1; snapshot deletion leaked", c.Get("b"))
	}
	if c.Get("c") != 0 {
		t.Fatalf("Get(c) = %d, want 0; snapshot addition leaked", c.Get("c"))
	}
}

func TestParallelReadWhileWriting(t *testing.T) {
	t.Parallel()

	c := New()
	done := make(chan struct{})

	go func() {
		for range 1000 {
			c.Inc("x")
		}
		close(done)
	}()

	// Read concurrently with the writer; -race asserts the lock guards both.
	for {
		select {
		case <-done:
			if c.Get("x") != 1000 {
				t.Fatalf("final Get(x) = %d, want 1000", c.Get("x"))
			}
			return
		default:
			_ = c.Get("x")
			_ = c.Snapshot()
		}
	}
}
```

## Review

The counter is correct when every access is serialized by the mutex and `Snapshot`
hands back an owned copy. The lesson the broken version teaches is not "add a lock to
pass `-race`" — it is that a shared-writer map is a *process crash*, detected by the
runtime independent of `-race`, so the lock is load-bearing for availability, not
tidiness. `Inc` under `Lock`, reads under `RLock`, and `Snapshot` cloning *inside* the
read lock are the three rules; drop the clone and a caller ranging the returned map
during a concurrent `Inc` re-introduces the fatal `concurrent map iteration and map
write`. The concurrency test's exact `N*iters` total is the signal that no updates were
lost. Always run `go test -race` on this one — it is the whole point.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the reader/writer lock.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the independent snapshot.
- [Go blog: Introducing the race detector](https://go.dev/blog/race-detector) — how `-race` finds these before production does.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-sync-map-vs-mutex-cache.md](08-sync-map-vs-mutex-cache.md)
