# Exercise 3: A Metrics Registry Usable at Its Zero Value

The opposite design from Exercise 1: a type with **no invariant to establish**,
whose zero value should just work. `var reg Registry; reg.Inc("requests")` must be
correct with no constructor, exactly like `var b bytes.Buffer` or `var mu sync.Mutex`.
This module builds an in-memory counter registry that lazily initializes its map
on first write under a lock.

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
zeroreg/                    independent module: example.com/zeroreg
  go.mod                    go 1.24
  registry.go               type Registry; Inc; Add; Get; Snapshot (lazy map init)
  cmd/
    demo/
      main.go               uses a zero-value Registry, prints counts
  registry_test.go          zero-value usable; accumulation; concurrent Inc under -race
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a `Registry` whose zero value is usable — `Inc(name)`, `Add(name, delta)`, `Get(name) int64`, `Snapshot() map[string]int64` — lazily creating the internal map on first write, guarded by a `sync.Mutex`.
- Test: a zero-value `Registry` can `Inc`/`Get` without panic and returns 0 for unknown keys; repeated `Inc` accumulates; concurrent `Inc` from many goroutines under `-race` yields the exact expected total.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/03-make-the-zero-value-useful/cmd/demo
cd go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/03-make-the-zero-value-useful
go mod edit -go=1.24
```

### Why no constructor

A counter registry has nothing to validate at construction: an empty registry is
a perfectly valid registry with no counters yet. That is precisely the condition
under which a **useful zero value** beats a mandatory `New`. The standard library
makes this choice constantly — `bytes.Buffer`, `sync.Mutex`, `strings.Builder`,
`sync.WaitGroup` are all ready at their zero value — and a registry is the same
shape.

The one subtlety is the map. A map field's zero value is `nil`, and writing to a
`nil` map panics. If we required the caller to run `New` just to `make` the map,
we would have reintroduced the constructor we are trying to avoid. Instead the
registry initializes the map **lazily**, on the first write, inside the same
critical section that does the write. Reads on a `nil` map are safe (they return
the zero value), so `Get` on a never-written registry returns 0 without needing
the map at all.

The mutex must be on a **pointer receiver** because the methods mutate the
registry (and because a `sync.Mutex` must never be copied — see Exercise 4). The
zero value of a `sync.Mutex` is an unlocked mutex, so `var reg Registry` yields a
usable, unlocked registry with a `nil` map, ready for the first `Inc`.

Create `registry.go`:

```go
package registry

import "sync"

// Registry is a concurrency-safe counter collector whose zero value is ready to
// use: var reg Registry works with no constructor. The map is created lazily on
// the first write.
type Registry struct {
	mu     sync.Mutex
	counts map[string]int64
}

// Add increments name by delta, creating the map on first write.
func (r *Registry) Add(name string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = make(map[string]int64)
	}
	r.counts[name] += delta
}

// Inc adds one to name.
func (r *Registry) Inc(name string) {
	r.Add(name, 1)
}

// Get returns the current value of name, or 0 if it was never written. A read of
// a nil map is safe, so this works on a zero-value Registry.
func (r *Registry) Get(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

// Snapshot returns a copy of all counters, safe for the caller to keep.
func (r *Registry) Snapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}
```

`Inc` delegates to `Add`, which takes the lock; `Inc` itself does not lock, so
there is no double-lock (Go's `sync.Mutex` is not reentrant, and locking twice on
one goroutine would deadlock). Keep the lock in exactly one place per call path.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/zeroreg"
)

func main() {
	var reg registry.Registry // zero value, no constructor

	fmt.Println("cold read:", reg.Get("requests")) // 0, map still nil

	reg.Inc("requests")
	reg.Inc("requests")
	reg.Add("bytes", 512)

	fmt.Println("requests:", reg.Get("requests"))
	fmt.Println("bytes:", reg.Get("bytes"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cold read: 0
requests: 2
bytes: 512
```

### Tests

The pivotal test is `TestZeroValueUsable`: it declares `var reg Registry` and
immediately reads and writes it, proving no constructor is needed and that a read
of the `nil` map returns 0 rather than panicking. The concurrency test launches
many goroutines each doing a fixed number of `Inc`s and asserts the exact total
under `-race`, which is what proves the lock actually serializes the
read-modify-write and the lazy init.

Create `registry_test.go`:

```go
package registry

import (
	"fmt"
	"sync"
	"testing"
)

func TestZeroValueUsable(t *testing.T) {
	t.Parallel()
	var reg Registry // no New

	if got := reg.Get("missing"); got != 0 {
		t.Fatalf("cold Get = %d, want 0", got)
	}
	reg.Inc("hits")
	if got := reg.Get("hits"); got != 1 {
		t.Fatalf("Get after Inc = %d, want 1", got)
	}
}

func TestAccumulates(t *testing.T) {
	t.Parallel()
	var reg Registry
	for range 5 {
		reg.Inc("x")
	}
	reg.Add("x", 10)
	if got := reg.Get("x"); got != 15 {
		t.Fatalf("Get(x) = %d, want 15", got)
	}
}

func TestConcurrentIncExactTotal(t *testing.T) {
	t.Parallel()
	var reg Registry
	const goroutines = 50
	const perGoroutine = 1000

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				reg.Inc("total")
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perGoroutine)
	if got := reg.Get("total"); got != want {
		t.Fatalf("Get(total) = %d, want %d", got, want)
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	t.Parallel()
	var reg Registry
	reg.Add("a", 3)
	snap := reg.Snapshot()
	snap["a"] = 999 // mutating the snapshot must not touch the registry
	if got := reg.Get("a"); got != 3 {
		t.Fatalf("registry mutated through snapshot: Get(a) = %d, want 3", got)
	}
}

func ExampleRegistry() {
	var reg Registry
	reg.Inc("ok")
	reg.Inc("ok")
	fmt.Println(reg.Get("ok"))
	// Output: 2
}
```

## Review

The registry is correct when its zero value behaves identically to a
freshly-constructed one: a cold `Get` returns 0, the first `Inc` creates the map,
and concurrent increments never lose an update under `-race`. Contrast this
deliberately with Exercise 1's `User`: that type has an invariant (non-empty ID),
so it *forces* a constructor and its zero value is explicitly "empty/invalid"; this
type has no invariant, so forcing a `New` would be pure ceremony. Deciding which
category a type is in — useful zero value versus mandatory constructor — is the
design judgment this module trains. The trap on the other side is a type that
*looks* zero-value-safe but silently isn't (a `nil` map you forgot to lazily
init, a channel that must be `make`d): if the zero value can panic or misbehave,
it needs a constructor, and you should make that constructor the only way in. Run
`go test -race`.

## Resources

- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — the canonical useful-zero-value type ("The zero value for Buffer is an empty buffer ready to use").
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — zero value is an unlocked mutex.
- [Effective Go: the zero value](https://go.dev/doc/effective_go#allocation_new) — designing types whose zero value is useful.
- [`expvar`](https://pkg.go.dev/expvar) — the standard library's real published-variable registry this exercise strips down.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-value-vs-pointer-and-copylocks.md](04-value-vs-pointer-and-copylocks.md)
