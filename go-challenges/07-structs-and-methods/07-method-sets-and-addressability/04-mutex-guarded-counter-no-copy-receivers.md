# Exercise 4: Mutex-Guarded Counter — Pointer Receivers and the copylocks Rule

A type that carries a `sync.Mutex` is non-copyable, and the reason is the deepest
"why" behind the pointer-receiver rule: a value receiver copies the mutex, which
splits one lock into two that guard nothing in common, and mutual exclusion
silently disappears. This module builds a correct mutex-guarded counter, proves
it under `-race`, and shows what `go vet`'s copylocks analyzer flags on the wrong
version.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
safecounter/                   independent module: example.com/safecounter
  go.mod                       module path + go directive
  counter.go                   SafeCounter (sync.Mutex, pointer methods); lazy Registry via sync.Once
  cmd/
    demo/
      main.go                  increment a shared *SafeCounter from goroutines
  counter_test.go              -race total test; documented copylocks wrong-example
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: `SafeCounter` with a `sync.Mutex` and `Inc`/`Value` on pointer receivers, plus a lazily-initialized registry using `sync.Once`.
- Test: N goroutines incrementing a shared `*SafeCounter`, asserting the exact total under `-race`; a documented illustrative block showing the value-receiver method that `go vet` flags.
- Verify: `go vet ./...` (must be clean on the correct code), `go test -count=1 -race ./...`.

### Why a mutex forces pointer receivers

A `sync.Mutex` is a value that holds lock state. Copy the struct that embeds it
and you copy the mutex too, producing a second, independent lock. Now imagine
`SafeCounter` had a value-receiver method `func (c SafeCounter) Inc()`. Every call
copies the whole counter — mutex included — so the method locks and mutates a
throwaway copy while the caller's counter is never touched under its own lock.
Two goroutines calling `Inc` copy the counter independently, each locks its own
private mutex, and the shared state is updated with no mutual exclusion at all.
The counter loses updates and can corrupt its state, and the failure is invisible
until you run under load.

Because of this, a type containing a mutex is non-copyable: all its methods take
pointer receivers, and it is passed only as `*T`. Go's toolchain enforces this
mechanically. `go vet` runs the `copylocks` analyzer, which flags a value receiver
on a lock-bearing type and flags passing such a type by value, with a message like
"Inc passes lock by value: safecounter.SafeCounter contains sync.Mutex". The
analyzer is the reason this class of bug is catchable before production — but only
if the method is written the wrong way; the correct pointer-receiver version below
is what you ship, and `go vet` is clean on it.

The registry adds a second real pattern: a `sync.Once` guarantees the counter map
is initialized exactly once even under concurrent first access, without a separate
constructor call at every use site. `sync.Once` is itself non-copyable, which is
another reason the registry lives behind a pointer.

Create `counter.go`:

```go
package safecounter

import "sync"

// SafeCounter is a concurrency-safe counter. It carries a sync.Mutex, so it is
// non-copyable: all methods take pointer receivers and it must be used as
// *SafeCounter. A value receiver here would copy the mutex and lose mutual
// exclusion (go vet's copylocks analyzer flags that).
type SafeCounter struct {
	mu sync.Mutex
	n  int64
}

// Inc adds one under the lock.
func (c *SafeCounter) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

// Value reads the count under the lock.
func (c *SafeCounter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Registry lazily initializes a set of named counters exactly once via sync.Once.
type Registry struct {
	once     sync.Once
	mu       sync.Mutex
	counters map[string]*SafeCounter
}

func (r *Registry) init() {
	r.once.Do(func() { r.counters = make(map[string]*SafeCounter) })
}

// Counter returns the shared *SafeCounter for name, creating it on first use.
func (r *Registry) Counter(name string) *SafeCounter {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		c = &SafeCounter{}
		r.counters[name] = c
	}
	return c
}
```

The wrong version, shown for contrast only — this is NOT compiled into the module
(no `Create` marker), because `go vet` would flag it and it must not ship:

```go
// WRONG: value receiver copies the mutex on every call, so each call locks a
// private copy and mutual exclusion is lost. go vet reports:
//   Inc passes lock by value: SafeCounter contains sync.Mutex
func (c SafeCounter) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}
```

### The runnable demo

The demo increments a shared `*SafeCounter` from many goroutines and prints the
total, which must equal the number of increments exactly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/safecounter"
)

func main() {
	var reg safecounter.Registry
	c := reg.Counter("requests")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	fmt.Printf("requests = %d\n", c.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests = 100
```

### Tests

The race test spins up N goroutines each doing many increments on one shared
`*SafeCounter` and asserts the exact total under `-race`. If the receiver were a
value, the total would be wrong and the race detector would fire. The wrong
value-receiver method is documented in a comment, not compiled, so `go vet` stays
clean on the suite.

Create `counter_test.go`:

```go
package safecounter

import (
	"sync"
	"testing"
)

func TestSafeCounterConcurrent(t *testing.T) {
	t.Parallel()
	var c SafeCounter // addressable local; &c is passed to goroutines implicitly

	const goroutines, per = 64, 500
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				c.Inc() // c.Inc() is (&c).Inc(); c is addressable so this compiles
			}
		}()
	}
	wg.Wait()

	if got := c.Value(); got != goroutines*per {
		t.Fatalf("count = %d, want %d", got, goroutines*per)
	}
}

func TestRegistryLazyInit(t *testing.T) {
	t.Parallel()
	var reg Registry // zero value is usable thanks to sync.Once init
	a := reg.Counter("a")
	a.Inc()
	a.Inc()
	if got := reg.Counter("a").Value(); got != 2 {
		t.Fatalf("a = %d, want 2", got)
	}
	if reg.Counter("a") != a {
		t.Fatal("Counter returned a different pointer for the same name")
	}
}
```

Note `c.Inc()` in the test compiles because `c` is a local variable and therefore
addressable, so the compiler forms `(&c).Inc()` for you. The same call on a
non-addressable operand — a map element, a function return — would not compile.

## Review

The counter is correct when every increment goes through one shared mutex: the
`-race` test drives 64 goroutines and asserts the exact total, which only holds if
`Inc` mutates the caller's counter under its lock rather than a copy. That is the
entire case for pointer receivers on stateful concurrent types.

The mistake this module exists to prevent is a value receiver (or by-value
passing) on a type containing a `sync.Mutex`, `sync.Once`, or `sync.WaitGroup`.
The copy splits the lock and mutual exclusion vanishes; the only automated catch
is `go vet`'s copylocks analyzer, so run it. Keep the type non-copyable: pointer
receivers everywhere, pass as `*T`, and let the zero value be usable via
`sync.Once` rather than copying a configured instance around. Run `go vet` and
`go test -race`; both must be clean on the correct code.

## Resources

- [`go vet` copylock analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/copylock) — what it flags and why lock-bearing types are non-copyable.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — "A Mutex must not be copied after first use."
- [`sync.Once`](https://pkg.go.dev/sync#Once) — once-only lazy initialization used by the registry.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-typed-nil-error-interface-trap.md](05-typed-nil-error-interface-trap.md)
