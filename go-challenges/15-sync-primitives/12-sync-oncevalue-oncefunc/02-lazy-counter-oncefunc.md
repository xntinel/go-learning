# Exercise 2: Lazy Counter: OnceFunc Guarding a Private Load Method

A service often needs a startup statistic — a table row count, a baseline
queue depth — that is expensive to compute and not needed at boot; this
exercise builds the standard struct pattern for that: a `sync.OnceFunc`
wrapping a private load method, with atomics making the value observable from
outside the once path.

## What you'll build

```text
lazycounter/               independent module: example.com/lazycounter
  go.mod                   go mod init example.com/lazycounter
  counter/
    counter.go             type Counter; New, Value, Loaded (OnceFunc + atomics)
    counter_test.go        lazy-init contract, 100-goroutine exactly-once, Example
  cmd/
    demo/
      main.go              runnable demo: Loaded before/after, cached Value, one init
```

- Files: `counter/counter.go`, `counter/counter_test.go`, `cmd/demo/main.go`.
- Implement: `Counter` with `New(init func() int64) *Counter`, `Value() int64` triggering init on first call via a stored `sync.OnceFunc`, and `Loaded() bool` as an `atomic.Bool` observability flag.
- Test: not loaded before first `Value`, loaded after, init runs exactly once across 100 concurrent `Value` calls under `-race`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/lazycounter/counter ~/go-exercises/lazycounter/cmd/demo
cd ~/go-exercises/lazycounter
go mod init example.com/lazycounter
```

### The pattern: OnceFunc over a method, stored in a field

Exercise 1 wrapped free functions. Real services more often need the pattern
in struct form: a type constructed with its init function, whose public
accessor triggers initialization on first use. The shape is:

```
c := &Counter{init: init}
c.once = sync.OnceFunc(c.load)
```

Two details are load-bearing. First, the `OnceFunc` closure is created **once,
in the constructor**, and stored in a field. Creating it inside `Value()`
would mint a fresh once per call and `load` would run every time — the
per-call-construction mistake from the concepts file, in its most tempting
disguise. Second, `sync.OnceFunc(c.load)` captures the *bound method value*
`c.load`: the closure holds a reference to this specific `*Counter`, so the
once-state and the counter it fills travel together. Callers share the struct
by pointer; copying a `Counter` after construction would be a bug (the copy's
`once` field still points at the original's method), which is one more reason
the constructor returns `*Counter`.

### Why value and loaded must be atomics

This is the subtle part. `Value()` itself would be safe with a plain `int64`
field: the memory model guarantees that `load`'s writes are synchronized
before every return of `c.once()`, so any reader that went through the once
sees the stored value. But `Loaded()` deliberately does **not** go through the
once — it is an observability probe ("has the lazy init happened yet?") that
must not itself trigger the init. That read happens outside the once path, so
it can race with `load`'s write; without an atomic, `-race` flags it and the
result is undefined. `loaded` must therefore be `atomic.Bool`. We make `value`
an `atomic.Int64` as well: it costs nothing on modern hardware, keeps the two
fields under one discipline, and protects any future accessor someone adds
without re-deriving the happens-before argument. The ordering inside `load`
also matters: `value` is stored **before** `loaded`, so a goroutine that
observes `Loaded() == true` can only be seeing a fully written value —
atomics in Go are sequentially consistent, so the store order is the
observation order.

Create `counter/counter.go`:

```go
// Package counter provides a lazily computed int64 statistic: the expensive
// init function runs on first Value call, exactly once, and the result is
// cached for the life of the process.
package counter

import (
	"sync"
	"sync/atomic"
)

// Counter is a lazily initialized statistic, such as a row-count baseline a
// service reads on demand rather than computing at boot. The zero value is
// not usable; construct with New and share by pointer.
type Counter struct {
	init   func() int64
	once   func()
	value  atomic.Int64
	loaded atomic.Bool
}

// New returns a Counter whose init runs on the first Value call.
func New(init func() int64) *Counter {
	c := &Counter{init: init}
	c.once = sync.OnceFunc(c.load)
	return c
}

// load runs the init exactly once (guarded by the OnceFunc in c.once).
// value is stored before loaded so an observer of Loaded() == true can only
// see a fully written value.
func (c *Counter) load() {
	c.value.Store(c.init())
	c.loaded.Store(true)
}

// Value returns the lazily computed value, initializing it on first call.
// Concurrent first callers block until the single init completes.
func (c *Counter) Value() int64 {
	c.once()
	return c.value.Load()
}

// Loaded reports whether the value has been initialized. It never triggers
// the init itself, which is why it reads an atomic rather than calling once.
func (c *Counter) Loaded() bool {
	return c.loaded.Load()
}
```

### The demo

The demo plays the production scenario: the "expensive scan" (here just a
function returning 1000, standing in for a `SELECT count(*)` baseline) does
not run at construction. `Loaded()` reports false until the first `Value()`,
the second `Value()` is a cached read, and the init counter proves the scan
ran once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lazycounter/counter"
)

func main() {
	scans := 0
	baseline := counter.New(func() int64 {
		scans++ // stands in for an expensive full-table scan
		return 1000
	})

	fmt.Println("loaded before first read:", baseline.Loaded())
	fmt.Println("baseline:", baseline.Value())
	fmt.Println("baseline again:", baseline.Value())
	fmt.Println("loaded after first read:", baseline.Loaded())
	fmt.Println("scans:", scans)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loaded before first read: false
baseline: 1000
baseline again: 1000
loaded after first read: true
scans: 1
```

### Tests

`TestCounterLazyInit` pins the lifecycle: not loaded before the first `Value`,
loaded after, exactly one init across repeated reads. `TestCounterConcurrentValue`
is the hard one — 100 goroutines race on a cold counter and the init must run
exactly once, with `-race` verifying that `Loaded` reads and `load` writes do
not conflict. `TestLoadedDoesNotTrigger` guards the probe's defining property:
observing the counter must not initialize it.

Create `counter/counter_test.go`:

```go
package counter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCounterLazyInit(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	c := New(func() int64 {
		calls.Add(1)
		return 1000
	})
	if c.Loaded() {
		t.Fatal("counter should not be loaded initially")
	}

	if got := c.Value(); got != 1000 {
		t.Fatalf("Value = %d, want 1000", got)
	}
	if !c.Loaded() {
		t.Fatal("counter should be loaded after Value")
	}
	if got := c.Value(); got != 1000 {
		t.Fatalf("Value (cached) = %d, want 1000", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestCounterConcurrentValue(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	c := New(func() int64 {
		calls.Add(1)
		return 7
	})

	var wg sync.WaitGroup
	const goroutines = 100
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if got := c.Value(); got != 7 {
				t.Errorf("Value = %d, want 7", got)
			}
			if !c.Loaded() {
				t.Error("Loaded = false after Value returned")
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestLoadedDoesNotTrigger(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	c := New(func() int64 {
		calls.Add(1)
		return 1
	})

	for range 10 {
		if c.Loaded() {
			t.Fatal("Loaded reported true before any Value call")
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Loaded triggered init %d times, want 0", got)
	}
}

func ExampleCounter_Value() {
	c := New(func() int64 { return 1000 })
	fmt.Println(c.Loaded())
	fmt.Println(c.Value())
	fmt.Println(c.Loaded())
	// Output:
	// false
	// 1000
	// true
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The type is correct when three properties hold together: `Value` initializes
lazily and exactly once under arbitrary concurrency; `Loaded` observes
initialization without causing it; and no interleaving of the two produces a
race — which is precisely what `go test -race` on `TestCounterConcurrentValue`
proves. The classic mistakes here are building the `OnceFunc` inside `Value`
(a fresh once per call, so the scan runs on every read — watch for this in
code review, it type-checks fine), and making `loaded` a plain `bool` because
"the once synchronizes everything" — it synchronizes only readers that go
through the once, and `Loaded` intentionally does not. If you want to see the
race for yourself, change `loaded` to a plain `bool`, run the concurrent test
with `-race`, and read the report: the write in `load` races with the read in
`Loaded`.

## Resources

- [sync.OnceFunc](https://pkg.go.dev/sync#OnceFunc) — the wrapper this type stores in a field.
- [sync/atomic: Bool and Int64](https://pkg.go.dev/sync/atomic#Bool) — the atomic types guarding reads outside the once path.
- [The Go Memory Model](https://go.dev/ref/mem) — why Value would be safe through the once alone, and why Loaded is not.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-lazy-init-library.md](01-lazy-init-library.md) | Next: [03-once-panic-contract.md](03-once-panic-contract.md)
