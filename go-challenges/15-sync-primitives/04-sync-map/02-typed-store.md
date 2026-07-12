# Exercise 2: Type-safe wrapper over sync.Map for a stable-keys registry

`sync.Map` stores `any`, which leaks type assertions all over the call site and
invites the comma-ok panic. The standard fix in real code is to wrap it once in a
small typed struct that exposes the operations you actually use with concrete
types, so callers never touch `any`. This module builds that wrapper for the
write-once-read-many registry pattern — a service-discovery table, a per-tenant
config map, any set of keys written at startup and read forever after.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
typedstore/                   independent module: example.com/typedstore
  go.mod                      go 1.26
  store.go                    type Pair, type Store; Put, Get, Delete, Len
  cmd/
    demo/
      main.go                 runnable demo: put a few records, read, delete, count
  store_test.go               round-trip, concurrent-same-key winner, Range-visits-all, Example
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store` with `Put(k, v string)`, `Get(k string) (Pair, bool)`, `Delete(k string)`, `Len() int`.
- Test: Put/Get/Delete round-trip with comma-ok; 100 goroutines write one key and the survivor is well-shaped; a test pinning `Len`/`Range` visits five keys exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/02-typed-store && cd go-solutions/15-sync-primitives/04-sync-map/02-typed-store
```

### Why wrap the map at all

The value of the wrapper is that the untyped surface exists in exactly one place.
`Put` stores a concrete `Pair`, `Get` performs the single `v.(Pair)` assertion
and returns a typed value plus an `ok` bool, and callers of `Store` never write a
type assertion or risk the missing-key panic. This is the difference between
`sync.Map` being a footgun scattered through a codebase and being a contained
implementation detail behind a clean API.

`Len` is where the "no cheap length" reality shows. `sync.Map` has no length
method, so `Len` counts by ranging every entry — O(n) and, under concurrency, a
best-effort number. For the stable-keys registry pattern that is fine: the key set
is written once at startup and rarely changes, so `Len` is called on a settled map
where an O(n) walk is cheap and accurate. If you needed a hot-path length under
churn, this is the signal to keep a separate `atomic.Int64` or switch to
`map`+`RWMutex`.

The concurrent-same-key behavior is worth being precise about. If many goroutines
`Put` the same key with different values, `sync.Map` serializes the stores and one
of them wins; which one is unspecified, but the surviving entry is always a
*whole* value written by exactly one goroutine — never a torn mix of two. That is
the guarantee the test pins: after 100 racing writers, the survivor has the right
key and a non-empty value from some single writer, and `-race` confirms no data
race on the map itself.

Create `store.go`:

```go
package typedstore

import "sync"

// Pair is the typed value the Store holds. Keeping the key inside the value
// lets Range hand back fully-formed records.
type Pair struct {
	Key, Value string
}

// Store is a strongly-typed wrapper over sync.Map for the write-once-read-many
// registry pattern: keys are written at startup and read many times. The any
// surface of sync.Map is confined to this one file.
type Store struct {
	m sync.Map // map[string]Pair
}

// NewStore returns an empty store ready for concurrent use.
func NewStore() *Store {
	return &Store{}
}

// Put stores value under key, overwriting any previous entry.
func (s *Store) Put(k, v string) {
	s.m.Store(k, Pair{Key: k, Value: v})
}

// Get returns the Pair for k and whether it was present.
func (s *Store) Get(k string) (Pair, bool) {
	v, ok := s.m.Load(k)
	if !ok {
		return Pair{}, false
	}
	return v.(Pair), true
}

// Delete removes k. Deleting an absent key is a no-op.
func (s *Store) Delete(k string) {
	s.m.Delete(k)
}

// Len counts stored entries by ranging. It is O(n) and best-effort under
// concurrency; suitable for a settled registry, not a hot path under churn.
func (s *Store) Len() int {
	n := 0
	s.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/typedstore"
)

func main() {
	s := typedstore.NewStore()
	s.Put("alpha", "10.0.0.1")
	s.Put("beta", "10.0.0.2")
	s.Put("gamma", "10.0.0.3")

	if p, ok := s.Get("beta"); ok {
		fmt.Printf("beta -> %s\n", p.Value)
	}
	fmt.Println("len:", s.Len())

	s.Delete("beta")
	if _, ok := s.Get("beta"); !ok {
		fmt.Println("beta deleted")
	}
	fmt.Println("len:", s.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
beta -> 10.0.0.2
len: 3
beta deleted
len: 2
```

### Tests

`TestGetPutDelete` walks the round-trip and checks the comma-ok on a deleted key.
`TestConcurrentSameKey` runs 100 goroutines writing the same key and asserts the
survivor is a well-shaped value (right key, non-empty value) rather than any
particular winner — the correct contract for a race. `TestRangeIteratesAllKeys`
populates five keys and asserts `Len` sees exactly five and `Range` visits each
key exactly once, pinning the enumeration contract.

Create `store_test.go`:

```go
package typedstore

import (
	"fmt"
	"sync"
	"testing"
)

func TestGetPutDelete(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Put("a", "1")
	s.Put("b", "2")

	if got, ok := s.Get("a"); !ok || got.Value != "1" || got.Key != "a" {
		t.Fatalf("Get(a) = %+v ok=%v, want {a,1} true", got, ok)
	}

	s.Delete("a")
	if _, ok := s.Get("a"); ok {
		t.Fatalf("Get(a) after Delete: ok = true, want false")
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestConcurrentSameKey(t *testing.T) {
	t.Parallel()

	s := NewStore()
	const goroutines = 100

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Put("k", fmt.Sprintf("v%d", i))
		}()
	}
	wg.Wait()

	p, ok := s.Get("k")
	if !ok {
		t.Fatal("Get(k): ok = false, want true")
	}
	// The winner is unspecified; the survivor must be a whole value.
	if p.Key != "k" || p.Value == "" {
		t.Fatalf("Get(k) = %+v, want key=k and a non-empty value", p)
	}
}

func TestRangeIteratesAllKeys(t *testing.T) {
	t.Parallel()

	s := NewStore()
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		s.Put(k, "v")
	}
	if got := s.Len(); got != len(keys) {
		t.Fatalf("Len() = %d, want %d", got, len(keys))
	}

	seen := map[string]int{}
	s.m.Range(func(key, _ any) bool {
		seen[key.(string)]++
		return true
	})
	for _, k := range keys {
		if seen[k] != 1 {
			t.Errorf("Range visited %q %d times, want 1", k, seen[k])
		}
	}
	if len(seen) != len(keys) {
		t.Fatalf("Range visited %d distinct keys, want %d", len(seen), len(keys))
	}
}

func ExampleStore() {
	s := NewStore()
	s.Put("region", "us-east-1")
	p, ok := s.Get("region")
	fmt.Println(p.Value, ok)
	// Output: us-east-1 true
}
```

## Review

The wrapper is correct when the `any` surface of `sync.Map` appears in exactly one
place — the `v.(Pair)` inside `Get` — and every caller works in concrete types.
The round-trip test pins the comma-ok contract; the concurrent-same-key test pins
that racing writers yield a whole survivor, not a torn value, which is the honest
guarantee `sync.Map` gives (it does not pick a *particular* winner, so asserting
one would be wrong). The enumeration test pins that `Len`/`Range` see every key
exactly once on a settled map. The trap to avoid is treating `Len` as cheap: it is
O(n) here by design and correct only for the stable-keys pattern this wrapper
serves. Run `go test -race` to confirm the concurrent writes are clean.

## Resources

- [sync.Map](https://pkg.go.dev/sync#Map) — `Store`, `Load`, `Delete`, `Range` and why there is no `Len`.
- [Go blog: Maps in Go](https://go.dev/blog/maps) — how Go maps work and when to add synchronization.
- [Effective Go: concurrency](https://go.dev/doc/effective_go#concurrency) — sharing by communicating, and when shared state needs a guard.

---

Back to [01-visit-counter.md](01-visit-counter.md) | Next: [03-generic-typed-map.md](03-generic-typed-map.md)
