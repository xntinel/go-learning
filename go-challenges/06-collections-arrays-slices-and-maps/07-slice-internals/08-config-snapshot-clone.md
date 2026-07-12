# Exercise 8: Copy-on-Read Config Snapshots in a Feature-Flag Store

A hot-reloadable feature-flag store is read by many goroutines and reloaded by
one. If `Get` returns a view into the store's own slice, a caller can mutate the
shared source — or read it mid-reload and observe torn state. This exercise
builds the read-path safety idiom: `Get` returns `slices.Clone` of the active
flag list, and reload swaps the backing slice atomically under a lock.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
flagstore/                  independent module: example.com/flagstore
  go.mod
  flagstore.go              type Store; Get (slices.Clone), Reload (swap under lock)
  cmd/
    demo/
      main.go               runnable demo: snapshot, caller mutation, reload
  flagstore_test.go         caller mutation isolated; clone of nil; -race readers vs reloader
```

Files: `flagstore.go`, `cmd/demo/main.go`, `flagstore_test.go`.
Implement: a `Store` with `Get() []string` returning `slices.Clone` of the active flags, and `Reload(flags []string)` swapping the backing slice under an `RWMutex`.
Test: a returned snapshot mutated by the caller does not affect a later `Get`; `Get` on an empty store returns a zero-length slice; concurrent readers against a reloader are race-free with no torn state.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/08-config-snapshot-clone/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/08-config-snapshot-clone
```

### Why the read path must clone

The store holds `flags []string`, the currently active feature flags. The unsafe
accessor would `return s.flags` — but that hands every caller a slice header
pointing at the store's own backing array. Two failures follow. First, a caller
can write through it (`flags[0] = "..."`), silently editing the store's state.
Second, when `Reload` runs concurrently, a reader holding that header can observe
elements changing underneath it — a torn read. Returning `slices.Clone(s.flags)`
copies the elements into a fresh array the caller owns; mutating it cannot reach
the store, and the snapshot is a stable point-in-time view.

`slices.Clone` is exactly right here: for `nil` or an empty slice it returns a
zero-length slice (a clone of `nil` is `nil`, whose `len` is 0), so `Get` on an
uninitialized store is safe with no special-casing. The clone happens under a
read lock so it observes a single consistent version of `flags`.

`Reload` takes the write lock and **swaps the whole slice**: `s.flags = newFlags`.
It does not mutate the existing backing array in place — any reader that already
cloned sees the old version intact, and the next reader clones the new one. This
swap-don't-mutate discipline, plus the clone on read, is what makes the store
safe under `-race` with concurrent readers and a writer. (An `atomic.Pointer` to
an immutable slice header is an even leaner lock-free variant; the `RWMutex`
version here is the clearest starting point.)

Create `flagstore.go`:

```go
package flagstore

import (
	"slices"
	"sync"
)

// Store holds the active feature flags and supports concurrent reads with
// hot reloads. Readers get an independent copy; reload swaps the whole slice.
type Store struct {
	mu    sync.RWMutex
	flags []string
}

// New returns a Store initialised with the given flags (cloned so the caller's
// slice cannot mutate the store).
func New(flags []string) *Store {
	return &Store{flags: slices.Clone(flags)}
}

// Get returns an independent snapshot of the active flags. The caller may
// mutate the result freely; it does not share storage with the Store.
func (s *Store) Get() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.flags)
}

// Has reports whether flag is currently active.
func (s *Store) Has(flag string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Contains(s.flags, flag)
}

// Reload atomically replaces the active flag set. It swaps the backing slice
// rather than mutating it, so readers holding a prior snapshot are unaffected.
func (s *Store) Reload(flags []string) {
	next := slices.Clone(flags)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags = next
}
```

### The runnable demo

The demo takes a snapshot, mutates it, and shows the store is unchanged, then
reloads and shows the new snapshot.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flagstore"
)

func main() {
	s := flagstore.New([]string{"checkout_v2", "dark_mode"})

	snap := s.Get()
	snap[0] = "HACKED" // caller mutates its own copy
	fmt.Printf("after caller mutation, store still has: %v\n", s.Get())

	s.Reload([]string{"checkout_v2", "dark_mode", "new_search"})
	fmt.Printf("after reload: %v\n", s.Get())
	fmt.Printf("has new_search: %v\n", s.Has("new_search"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after caller mutation, store still has: [checkout_v2 dark_mode]
after reload: [checkout_v2 dark_mode new_search]
has new_search: true
```

### Tests

`TestSnapshotIsIndependent` mutates a returned snapshot and asserts a later `Get`
is unaffected. `TestGetEmptyStore` asserts a clone of nil is a zero-length slice.
`TestConcurrentReadersAndReloader` runs many readers against a reloading writer
under `-race`, asserting every snapshot is one of the valid whole versions (no
torn state).

Create `flagstore_test.go`:

```go
package flagstore

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestSnapshotIsIndependent(t *testing.T) {
	t.Parallel()
	s := New([]string{"a", "b"})
	snap := s.Get()
	snap[0] = "mutated"
	if got := s.Get(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("store changed after caller mutated snapshot: %v", got)
	}
}

func TestGetEmptyStore(t *testing.T) {
	t.Parallel()
	var s Store // zero value: nil flags
	if got := s.Get(); len(got) != 0 {
		t.Fatalf("Get() on empty store = %v, want length 0", got)
	}
	s2 := New(nil)
	if got := s2.Get(); len(got) != 0 {
		t.Fatalf("New(nil).Get() = %v, want length 0", got)
	}
}

func TestConcurrentReadersAndReloader(t *testing.T) {
	t.Parallel()
	old := []string{"a", "b"}
	updated := []string{"a", "b", "c"}
	s := New(old)

	var wg sync.WaitGroup
	// Reloader flips between two whole versions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 200 {
			if i%2 == 0 {
				s.Reload(updated)
			} else {
				s.Reload(old)
			}
		}
	}()
	// Readers must always see one whole version, never a torn mix.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				snap := s.Get()
				if !slices.Equal(snap, old) && !slices.Equal(snap, updated) {
					t.Errorf("torn snapshot observed: %v", snap)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func ExampleStore() {
	s := New([]string{"a"})
	snap := s.Get()
	snap[0] = "x" // does not touch the store
	fmt.Println(s.Get(), s.Has("a"))
	// Output: [a] true
}
```

## Review

The store is safe when the read path clones and the write path swaps. `Get`
returning `slices.Clone(s.flags)` gives each caller an independent snapshot, so
`TestSnapshotIsIndependent` sees the store unchanged after a caller mutates its
copy, and a clone of `nil` yields a zero-length slice with no special-casing.
`Reload` replaces the whole slice under the write lock rather than editing it in
place, so a reader either sees the old version or the new one, never a torn mix —
`TestConcurrentReadersAndReloader` asserts exactly that under `-race`. The mistake
this prevents is `return s.flags`, which leaks the internal array to every caller.
Run `go test -race` to confirm no data race and no torn state.

## Resources

- [pkg.go.dev: slices.Clone](https://pkg.go.dev/slices#Clone) — the defensive copy for a read-path boundary.
- [pkg.go.dev: sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — many readers, one writer.
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro) — why returning a raw slice leaks storage.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-two-dim-shared-row.md](09-two-dim-shared-row.md)
