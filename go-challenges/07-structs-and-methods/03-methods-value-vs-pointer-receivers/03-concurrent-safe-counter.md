# Exercise 3: A Race-Free Counter Under Concurrent Increment

The counter from Exercise 1 is not safe to call from many goroutines. This module
builds two production implementations that are — one over `atomic.Int64`, one
over `sync.Mutex` + `int64` — and uses them to show the third reason a type is
*forced* onto pointer receivers: it contains a value that must never be copied.
Copying a used mutex or atomic is a real bug, and `go vet`'s copylocks analyzer
exists to catch it.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
racecounter/               independent module: example.com/racecounter
  go.mod
  counter.go               interface Counter; AtomicCounter, MutexCounter (pointer receivers)
  cmd/
    demo/
      main.go              N goroutines increment each counter concurrently
  counter_test.go          table test over both impls, race-free; vet-clean copylocks
```

Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
Implement: a `Counter` interface (`Inc()`, `Get() int64`) satisfied by `*AtomicCounter` (wrapping `atomic.Int64`) and `*MutexCounter` (wrapping `sync.Mutex` + `int64`), each with a `New...` constructor returning a pointer.
Test: spawn N goroutines that each call `Inc`, wait, and assert `Get() == N`, for both implementations, under `-race`; `go vet` passes (proving no lock/atomic value-copy).
Verify: `go test -count=1 -race ./...` and `go vet ./...`

### Why these types cannot use value receivers

`sync.Mutex` and `atomic.Int64` both carry internal state that is only meaningful
in one place in memory. A `sync.Mutex` records who holds the lock; an
`atomic.Int64` is manipulated with atomic CPU instructions on a specific address.
Copy either one and the copy is a *different* lock protecting nothing, or a
*different* word that other goroutines are not touching. A value receiver takes
exactly that copy on every call, so a value-receiver method on such a type is a
correctness bug even when it looks harmless.

The language cannot forbid the copy outright (these are ordinary structs), but
`go vet` ships a `copylocks` analyzer that flags it: pass a `sync.Mutex` by value,
give it a value receiver, or return a struct containing one by value, and vet
reports `passes lock by value`. So the rules compound here: the mutating methods
*need* a pointer receiver, and the non-copyable field *forbids* a value receiver
even for the reads — every method is a pointer receiver, and the constructors
return `*T`. `atomic.Int64` (Go 1.19+) is the modern replacement for the older
`atomic.AddInt64(&x, 1)` free functions: it makes the "do not copy" contract part
of the type and its methods (`Add`, `Load`, `Store`) are the atomic operations.

Both implementations satisfy one small interface so a single test can drive them
identically. The interface holds `*AtomicCounter` and `*MutexCounter`, never the
value types, because their methods have pointer receivers.

Create `counter.go`:

```go
package racecounter

import (
	"sync"
	"sync/atomic"
)

// Counter is a concurrency-safe request counter. Implementations must be safe
// for concurrent Inc from many goroutines.
type Counter interface {
	Inc()
	Get() int64
}

// AtomicCounter is lock-free via atomic.Int64. It must not be copied once used,
// so every method takes a pointer receiver and NewAtomicCounter returns *T.
type AtomicCounter struct {
	n atomic.Int64
}

func NewAtomicCounter() *AtomicCounter {
	return &AtomicCounter{}
}

func (c *AtomicCounter) Inc() {
	c.n.Add(1)
}

func (c *AtomicCounter) Get() int64 {
	return c.n.Load()
}

// MutexCounter guards a plain int64 with a mutex. The mutex must not be copied,
// so it, too, is pointer-receiver-only with a *T constructor.
type MutexCounter struct {
	mu sync.Mutex
	n  int64
}

func NewMutexCounter() *MutexCounter {
	return &MutexCounter{}
}

func (c *MutexCounter) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *MutexCounter) Get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Compile-time proof that the POINTER types satisfy Counter (the value types do
// not: their methods have pointer receivers).
var (
	_ Counter = (*AtomicCounter)(nil)
	_ Counter = (*MutexCounter)(nil)
)
```

### The runnable demo

The demo launches a fixed number of goroutines against each counter and prints
the final totals, which are deterministic: N increments always sum to N when the
counter is race-free.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/racecounter"
)

const workers = 1000

func hammer(c racecounter.Counter) int64 {
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	return c.Get()
}

func main() {
	fmt.Printf("atomic total: %d\n", hammer(racecounter.NewAtomicCounter()))
	fmt.Printf("mutex total:  %d\n", hammer(racecounter.NewMutexCounter()))
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```
atomic total: 1000
mutex total:  1000
```

### Tests

One table test drives both implementations through the `Counter` interface,
spawning N goroutines that each `Inc` once and asserting the total is exactly N.
Running it under `-race` is the point: a value-copied counter, or an unguarded
increment, either fails the count or trips the race detector. A separate
`go vet ./...` step (part of the gate) proves no method accidentally copies the
lock or atomic.

Create `counter_test.go`:

```go
package racecounter

import (
	"sync"
	"testing"
)

func TestConcurrentIncIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 500

	impls := []struct {
		name string
		make func() Counter
	}{
		{"atomic", func() Counter { return NewAtomicCounter() }},
		{"mutex", func() Counter { return NewMutexCounter() }},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()
			c := impl.make()

			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.Inc()
				}()
			}
			wg.Wait()

			if got := c.Get(); got != n {
				t.Fatalf("%s: Get() = %d after %d concurrent Inc, want %d",
					impl.name, got, n, n)
			}
		})
	}
}

func TestSingleThreadedInc(t *testing.T) {
	t.Parallel()

	c := NewAtomicCounter()
	c.Inc()
	c.Inc()
	if got := c.Get(); got != 2 {
		t.Fatalf("Get() = %d, want 2", got)
	}
}
```

## Review

Both counters are correct when N concurrent increments produce exactly N and the
`-race` build stays silent. The deeper lesson is that the receiver decision was
not yours to make freely here: the moment the struct held a `sync.Mutex` or an
`atomic.Int64`, value receivers became a bug and `go vet`'s copylocks check became
your early-warning system — keep `go vet ./...` in the pipeline and green. The
compile-time `var _ Counter = (*AtomicCounter)(nil)` lines document that only the
pointer types satisfy the interface; try changing one to
`var _ Counter = AtomicCounter{}` and the build fails with "method Inc has
pointer receiver", which is the same method-set rule Exercise 8 explores in
depth.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` and its `Add`/`Load`/`Store` methods (Go 1.19+).
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — and the note that a Mutex must not be copied after first use.
- [Go Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` catches and how to run it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-counter-swap-two-pointers.md](02-counter-swap-two-pointers.md) | Next: [04-immutable-money-value-object.md](04-immutable-money-value-object.md)
