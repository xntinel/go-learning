# Exercise 3: Generic TypedMap[K,V] eliminating any-boxing at the call site

The previous module wrapped `sync.Map` for one concrete type. In a real codebase
you want that ergonomic win for *any* key and value type without writing a new
wrapper each time. Go generics let you build `TypedMap[K comparable, V any]` once:
the public API is fully type-checked, the internal `any`-boxing is hidden, and the
comma-ok panic is impossible to write at the call site. This is the module that
makes `sync.Map` palatable in production code.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
typedmap/                     independent module: example.com/typedmap
  go.mod                      go 1.26
  typedmap.go                 type TypedMap[K,V]; Load, Store, LoadOrStore, LoadAndDelete, Delete, Range
  cmd/
    demo/
      main.go                 runnable demo over a struct value type
  typedmap_test.go            per-method table tests, concurrent LoadOrStore, Range early-stop, Example
```

- Files: `typedmap.go`, `cmd/demo/main.go`, `typedmap_test.go`.
- Implement: `TypedMap[K comparable, V any]` with `Load`, `Store`, `LoadOrStore`, `LoadAndDelete`, `Delete`, `Range`, all in real types.
- Test: each method over a struct value; concurrent `LoadOrStore` of one key yields a single stored value; `Range` early-stop (`return false`) halts iteration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir typedmap && cd typedmap
go mod init example.com/typedmap
```

### What generics do and do not remove

The type parameters buy you two things. First, every method signature speaks in
`K` and `V`: `Load(key K) (V, bool)`, `Store(key K, value V)`,
`LoadOrStore(key K, value V) (V, bool)`. The caller never sees `any` and never
writes a type assertion, so the comma-ok panic and the wrong-type assertion are
gone from the call site — the single assertion `v.(V)` lives inside `Load` and
`LoadOrStore`, and it is guaranteed to succeed because only these typed methods
ever wrote to the map. Second, `Range` takes `func(K, V) bool`, so the callback is
type-checked too.

What generics do **not** remove is the interface-boxing allocation cost. Internally
`TypedMap` still stores `V` as `any`, so a non-pointer `V` still gets boxed on
`Store` exactly as before. Generics erase the *assertion ergonomics* cost, not the
*boxing* cost. This is the honest trade-off to state in review: `TypedMap` makes
`sync.Map` safe and pleasant to use, but if profiling shows the boxing allocations
dominate a write-heavy path, the answer is still `map`+`RWMutex` with a concrete
value, not a fancier wrapper. Exercise 10 measures exactly this.

`Load` and `LoadAndDelete` must convert a miss into a zero `V`. On a miss the
underlying `sync.Map` returns `(nil, false)`; the wrapper returns `(zeroV, false)`
where `var zero V` gives the correct zero for whatever `V` is. Returning the zero
rather than asserting `nil.(V)` is what keeps a miss from panicking.

Create `typedmap.go`:

```go
package typedmap

import "sync"

// TypedMap is a generic, type-safe wrapper over sync.Map. The public API is
// fully type-checked in K and V; the any-boxing sync.Map requires is confined
// to this file, and no type assertion can fail because only these methods write
// the map. Do not copy a TypedMap after first use: it embeds a sync.Map.
type TypedMap[K comparable, V any] struct {
	m sync.Map
}

// Store sets value for key, overwriting any previous value.
func (t *TypedMap[K, V]) Store(key K, value V) {
	t.m.Store(key, value)
}

// Load returns the value for key and whether it was present. A miss returns
// the zero V and false.
func (t *TypedMap[K, V]) Load(key K) (V, bool) {
	v, ok := t.m.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	return v.(V), true
}

// LoadOrStore returns the existing value for key if present, otherwise stores
// and returns value. The loaded bool reports whether the value was already
// present. The check-and-store is atomic.
func (t *TypedMap[K, V]) LoadOrStore(key K, value V) (V, bool) {
	actual, loaded := t.m.LoadOrStore(key, value)
	return actual.(V), loaded
}

// LoadAndDelete atomically removes key and returns its previous value and
// whether it was present. A miss returns the zero V and false.
func (t *TypedMap[K, V]) LoadAndDelete(key K) (V, bool) {
	v, loaded := t.m.LoadAndDelete(key)
	if !loaded {
		var zero V
		return zero, false
	}
	return v.(V), true
}

// Delete removes key. Deleting an absent key is a no-op.
func (t *TypedMap[K, V]) Delete(key K) {
	t.m.Delete(key)
}

// Range calls f for each key/value. Iteration stops early if f returns false.
// Like sync.Map.Range it is best-effort, not a consistent snapshot.
func (t *TypedMap[K, V]) Range(f func(K, V) bool) {
	t.m.Range(func(k, v any) bool {
		return f(k.(K), v.(V))
	})
}
```

### The runnable demo

The demo uses a struct value type — a common real case (a config record, a session
row) where boxing is visible and the type safety matters most.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/typedmap"
)

type Node struct {
	Addr   string
	Weight int
}

func main() {
	var nodes typedmap.TypedMap[string, Node]
	nodes.Store("a", Node{Addr: "10.0.0.1", Weight: 5})
	nodes.Store("b", Node{Addr: "10.0.0.2", Weight: 3})

	if n, ok := nodes.Load("a"); ok {
		fmt.Printf("a -> %s w=%d\n", n.Addr, n.Weight)
	}

	// LoadOrStore does not overwrite an existing key.
	actual, loaded := nodes.LoadOrStore("a", Node{Addr: "x", Weight: 99})
	fmt.Printf("loadOrStore a: loaded=%v weight=%d\n", loaded, actual.Weight)

	if old, ok := nodes.LoadAndDelete("b"); ok {
		fmt.Printf("deleted b (was %s)\n", old.Addr)
	}

	var keys []string
	nodes.Range(func(k string, _ Node) bool {
		keys = append(keys, k)
		return true
	})
	sort.Strings(keys)
	fmt.Println("remaining:", keys)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a -> 10.0.0.1 w=5
loadOrStore a: loaded=true weight=5
deleted b (was 10.0.0.2)
remaining: [a]
```

### Tests

`TestMethods` walks each method over a struct value type and asserts the typed
results, including that a miss returns the zero value and false (no panic).
`TestConcurrentLoadOrStore` runs many goroutines racing to `LoadOrStore` the same
key with distinct values and asserts exactly one value survives and every caller
that saw `loaded=false` agrees on it — the atomic check-and-set contract.
`TestRangeEarlyStop` asserts returning `false` halts iteration.

Create `typedmap_test.go`:

```go
package typedmap

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type rec struct {
	ID   int
	Name string
}

func TestMethods(t *testing.T) {
	t.Parallel()

	var m TypedMap[string, rec]

	if _, ok := m.Load("missing"); ok {
		t.Fatal("Load(missing): ok = true, want false")
	}

	m.Store("a", rec{ID: 1, Name: "alice"})
	if got, ok := m.Load("a"); !ok || got.ID != 1 || got.Name != "alice" {
		t.Fatalf("Load(a) = %+v ok=%v, want {1 alice} true", got, ok)
	}

	got, loaded := m.LoadOrStore("a", rec{ID: 9, Name: "x"})
	if !loaded || got.ID != 1 {
		t.Fatalf("LoadOrStore(a) = %+v loaded=%v, want existing {1 alice} true", got, loaded)
	}
	got, loaded = m.LoadOrStore("b", rec{ID: 2, Name: "bob"})
	if loaded || got.ID != 2 {
		t.Fatalf("LoadOrStore(b) = %+v loaded=%v, want stored {2 bob} false", got, loaded)
	}

	old, ok := m.LoadAndDelete("a")
	if !ok || old.ID != 1 {
		t.Fatalf("LoadAndDelete(a) = %+v ok=%v, want {1 alice} true", old, ok)
	}
	if _, ok := m.Load("a"); ok {
		t.Fatal("Load(a) after LoadAndDelete: ok = true, want false")
	}
	if _, ok := m.LoadAndDelete("gone"); ok {
		t.Fatal("LoadAndDelete(gone): ok = true, want false")
	}

	m.Delete("b")
	if _, ok := m.Load("b"); ok {
		t.Fatal("Load(b) after Delete: ok = true, want false")
	}
}

func TestConcurrentLoadOrStore(t *testing.T) {
	t.Parallel()

	var m TypedMap[string, int]
	const goroutines = 200

	var wg sync.WaitGroup
	var storedCount atomic.Int64
	survivors := make([]int, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actual, loaded := m.LoadOrStore("k", i)
			if !loaded {
				storedCount.Add(1)
			}
			survivors[i] = actual
		}()
	}
	wg.Wait()

	if got := storedCount.Load(); got != 1 {
		t.Fatalf("stored %d times, want exactly 1", got)
	}
	final, ok := m.Load("k")
	if !ok {
		t.Fatal("Load(k): ok = false, want true")
	}
	for i, s := range survivors {
		if s != final {
			t.Fatalf("goroutine %d saw %d, but final is %d; LoadOrStore not atomic", i, s, final)
		}
	}
}

func TestRangeEarlyStop(t *testing.T) {
	t.Parallel()

	var m TypedMap[int, int]
	for i := range 10 {
		m.Store(i, i)
	}
	visited := 0
	m.Range(func(_, _ int) bool {
		visited++
		return visited < 3 // stop after visiting 3
	})
	if visited != 3 {
		t.Fatalf("Range visited %d entries, want it to stop at 3", visited)
	}
}

func ExampleTypedMap() {
	var m TypedMap[string, int]
	m.Store("port", 8080)
	v, ok := m.Load("port")
	fmt.Println(v, ok)
	_, ok = m.Load("missing")
	fmt.Println(ok)
	// Output:
	// 8080 true
	// false
}
```

## Review

The wrapper is correct when every public method is typed and the single internal
`v.(V)` assertion cannot fail — which holds because only these methods ever write
the map, so anything stored under a `K` is a `V`. `TestMethods` pins that a miss
returns the zero value rather than panicking; `TestConcurrentLoadOrStore` pins the
atomic check-and-set (exactly one store, all callers agree on the survivor);
`TestRangeEarlyStop` pins that `return false` halts iteration. The trap to avoid is
believing generics also removed the allocation cost: they did not — a non-pointer
`V` is still boxed to `any` on `Store`, and that boxing is the reason to prefer
`map`+`RWMutex` on write-heavy paths. Run `go test -race`; the concurrent
`LoadOrStore` test is what proves the map access is clean.

## Resources

- [sync.Map](https://pkg.go.dev/sync#Map) — the underlying `Load`/`Store`/`LoadOrStore`/`LoadAndDelete`/`Range`.
- [Go generics: type parameters](https://go.dev/doc/tutorial/generics) — declaring and using `[K comparable, V any]`.
- [The Go blog: An Introduction To Generics](https://go.dev/blog/intro-generics) — where generics fit and their runtime cost.

---

Back to [02-typed-store.md](02-typed-store.md) | Next: [04-connection-registry.md](04-connection-registry.md)
