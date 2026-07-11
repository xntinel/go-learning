# Exercise 4: A Concurrent In-Memory Store Embedding sync.RWMutex

Embedding a lock is the textbook example of embedding that *widens your API* — and
that is exactly why it is worth building carefully. You will embed `sync.RWMutex`
in a generic store, use the promoted `Lock`/`RLock`, and then confront the
trade-off head-on: those methods are now public, and callers can reach into your
locking.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
store/                     independent module: example.com/store
  go.mod                   go 1.26
  store.go                 Store[K,V] embeds sync.RWMutex; Get/Set/Delete/Len; SortedKeys generic func
  cmd/
    demo/
      main.go              runnable demo: set keys, print sorted keys and length
  store_test.go            -race concurrency; a subtest pinning the exported-lock trade-off
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store[K comparable, V any]` embedding `sync.RWMutex` over a map, with `Get`/`Set`/`Delete`/`Len` using the promoted `Lock`/`RLock`, plus a free function `SortedKeys[K cmp.Ordered, V any]`.
- Test: concurrent `Set`/`Get`/`Delete` under `-race` with a `WaitGroup`, a final-state consistency check, and a subtest documenting that `s.Lock()` is callable from outside.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/store/cmd/demo
cd ~/go-exercises/store
go mod init example.com/store
```

### Promoted locking, and the API cost of it

Embedding `sync.RWMutex` promotes `Lock`, `Unlock`, `RLock`, `RUnlock`, and
`TryLock` onto `Store`, so the methods write `s.Lock()`/`s.RLock()` with no field
name. That reads cleanly, and for a type whose entire job is to be a lock-guarded
container it is defensible. But be clear about what promotion did: it made those
five methods part of `Store`'s public API. A caller can now write
`s.Lock()` in their own code, acquire your write lock, and — if they forget to
unlock, or hold it across a call back into the store — deadlock the store or
starve every reader. The lock is no longer an implementation detail you control; it
is a contract you exposed.

The conservative default for a concurrent container is therefore a *named*,
unexported field: `mu sync.RWMutex`. Then `Lock`/`Unlock` are private to the
package and callers cannot touch your invariants. Embed the lock only when
exporting it is a deliberate design decision — for example, a type explicitly
documented as "hold `Lock` while you mutate the exposed slice." This exercise
embeds it precisely so the trade-off is visible and testable; the next exercise
shows the copy hazard the embedding also introduces.

`SortedKeys` is a free generic function constrained on `cmp.Ordered`, not a method,
because sorting needs an ordering the store's `K comparable` constraint does not
provide. It takes a read lock, snapshots the keys, and sorts them with
`slices.Sort` — a small illustration that not everything belongs as a method on the
embedded-lock type.

Create `store.go`:

```go
package store

import (
	"cmp"
	"slices"
	"sync"
)

// Store is a concurrency-safe generic map. It embeds sync.RWMutex, so Lock/RLock
// are promoted onto Store — convenient internally, but note this also exports
// those methods to callers. A named unexported field would keep them private.
type Store[K comparable, V any] struct {
	sync.RWMutex
	m map[K]V
}

// New returns an empty Store.
func New[K comparable, V any]() *Store[K, V] {
	return &Store[K, V]{m: make(map[K]V)}
}

// Set stores value under key using the promoted write lock.
func (s *Store[K, V]) Set(key K, value V) {
	s.Lock()
	defer s.Unlock()
	s.m[key] = value
}

// Get reads value under the promoted read lock.
func (s *Store[K, V]) Get(key K) (V, bool) {
	s.RLock()
	defer s.RUnlock()
	v, ok := s.m[key]
	return v, ok
}

// Delete removes key under the write lock.
func (s *Store[K, V]) Delete(key K) {
	s.Lock()
	defer s.Unlock()
	delete(s.m, key)
}

// Len reports the number of entries under the read lock.
func (s *Store[K, V]) Len() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.m)
}

// SortedKeys returns the keys of s in ascending order. It is a free function, not
// a method, because ordering needs cmp.Ordered which the store's comparable K
// constraint does not provide.
func SortedKeys[K cmp.Ordered, V any](s *Store[K, V]) []K {
	s.RLock()
	defer s.RUnlock()
	keys := make([]K, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/store"
)

func main() {
	s := store.New[string, int]()
	s.Set("alpha", 1)
	s.Set("gamma", 3)
	s.Set("beta", 2)
	s.Delete("gamma")

	fmt.Println("keys:", store.SortedKeys(s))
	fmt.Println("len:", s.Len())

	if v, ok := s.Get("beta"); ok {
		fmt.Println("beta:", v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
keys: [alpha beta]
len: 2
beta: 2
```

### Tests

`TestConcurrent` hammers the store from many goroutines doing `Set`/`Get`/`Delete`
under `-race`; because each key is written exactly once by exactly one goroutine,
the final `Len` is deterministic and the run must be race-clean. `TestExportedLockTradeoff`
is the documenting test: it takes the promoted write lock from *outside* the store
(`s.Lock()`), which compiles only because embedding exported it, and confirms the
store still works once released — pinning the trade-off in a test so it cannot be
forgotten.

Create `store_test.go`:

```go
package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrent(t *testing.T) {
	t.Parallel()
	s := New[int, int]()
	const n = 200
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Set(i, i*i)
			if v, ok := s.Get(i); ok && v != i*i {
				t.Errorf("Get(%d) = %d, want %d", i, v, i*i)
			}
		}()
	}
	wg.Wait()

	if got := s.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
}

func TestConcurrentDelete(t *testing.T) {
	t.Parallel()
	s := New[int, int]()
	const n = 100
	for i := range n {
		s.Set(i, i)
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Delete(i)
		}()
	}
	wg.Wait()

	if got := s.Len(); got != 0 {
		t.Fatalf("Len after deleting all = %d, want 0", got)
	}
}

func TestExportedLockTradeoff(t *testing.T) {
	t.Parallel()
	s := New[string, int]()
	s.Set("k", 1)

	// Embedding sync.RWMutex exported Lock/Unlock: callers can take them from
	// outside the store. This is the widened-API cost of embedding the lock.
	s.Lock()
	// A named unexported mu field would make this line fail to compile.
	s.Unlock()

	if v, ok := s.Get("k"); !ok || v != 1 {
		t.Fatalf("Get(k) = %d,%v after external Lock/Unlock; want 1,true", v, ok)
	}
}

func Example() {
	s := New[string, int]()
	s.Set("b", 2)
	s.Set("a", 1)
	fmt.Println(SortedKeys(s))
	// Output: [a b]
}
```

## Review

The store is correct when every map access is guarded: writes under `Lock`, reads
under `RLock`, and the `-race` runs stay clean because there is no unguarded path.
`TestConcurrentDelete` and `TestConcurrent` are the proof. The design lesson is the
one `TestExportedLockTradeoff` pins: embedding the lock made `s.Lock()` callable by
anyone, which is a real widening of the contract — convenient here, dangerous in a
type whose locking you meant to encapsulate. If in doubt, prefer a named
unexported `mu` field and keep `Lock`/`Unlock` out of the public surface. The next
exercise shows the second hazard embedding a lock brings: the struct becomes
non-copyable.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — `Lock`/`Unlock`/`RLock`/`RUnlock` and the read-vs-write locking model.
- [`cmp.Ordered`](https://pkg.go.dev/cmp#Ordered) — the ordering constraint used by `SortedKeys`.
- [`slices.Sort`](https://pkg.go.dev/slices#Sort) — sorting the snapshot of keys.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-embedded-mutex-copylocks-pitfall.md](05-embedded-mutex-copylocks-pitfall.md)
