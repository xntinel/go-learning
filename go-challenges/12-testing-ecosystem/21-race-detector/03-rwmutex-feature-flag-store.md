# Exercise 3: A Read-Heavy Feature-Flag Store Guarded by sync.RWMutex

A feature-flag or dynamic-config store is read on nearly every request and
reloaded only occasionally by a control goroutine. That read-heavy, write-rare
shape is the textbook case for `sync.RWMutex`. This exercise builds one, and it
drills the second half of the lesson: returning a defensive copy under the lock
(`maps.Clone`) so a caller cannot race on the internal map after you unlock.

This module is self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
flagstore/                  independent module: example.com/flagstore
  go.mod                    go 1.26
  store.go                  type Store (RWMutex): New, IsEnabled, Snapshot, Reload
  cmd/
    demo/
      main.go               reload flags, read a snapshot, print it
  store_test.go             concurrent readers during reload, under -race -count=10
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: a `Store` with `IsEnabled` and `Snapshot` under `RLock`, `Reload`
under `Lock`, and a defensive `maps.Clone` on `Snapshot`.
Test: `TestFlagStoreConcurrentReadDuringReload` runs readers in a loop while a
writer reloads, asserting every snapshot is internally consistent, under `-race`.
Verify: `go test -count=10 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/21-race-detector/03-rwmutex-feature-flag-store/cmd/demo
cd go-solutions/12-testing-ecosystem/21-race-detector/03-rwmutex-feature-flag-store
```

### Why RWMutex, and why the defensive copy is non-negotiable

`IsEnabled` is called by every request goroutine; `Reload` is called rarely by a
config-watcher. If both took an exclusive `Mutex`, all the readers would
serialize behind each other for no reason -- they do not conflict with one
another, only with the writer. `sync.RWMutex` encodes exactly that: any number of
`RLock` holders concurrently, but `Lock` is exclusive against everything. That is
the read-heavy branch of the fix-by-access-pattern decision tree.

The trap is `Snapshot`. The naive version does `RLock`, `return s.flags`,
`RUnlock` -- handing the caller a reference to the internal map. The moment the
caller reads that map after `RUnlock`, it is reading lock-protected state without
the lock, and the next `Reload` writes the same map concurrently: a data race,
and potentially a `concurrent map writes` crash. The lock protected the map only
while held; returning the map defeats it entirely. The fix is `maps.Clone` while
still holding the lock: build a fresh, caller-owned copy the writer will never
touch, then return that. The clone is O(n) but a flag set is small, and it is the
only way the caller can safely read the result after unlocking. (For a large map
where cloning is too costly, the alternative is copy-on-write with
`atomic.Pointer`, which the next exercise covers.)

Create `store.go`:

```go
package flagstore

import (
	"maps"
	"sync"
)

// Store is a read-heavy feature-flag store: many request goroutines read flags
// via IsEnabled/Snapshot (shared RLock), a control goroutine swaps the whole set
// via Reload (exclusive Lock).
type Store struct {
	mu    sync.RWMutex
	flags map[string]bool
}

// New returns a Store seeded with the given flags. The input is cloned so the
// caller cannot mutate the store's map afterward.
func New(initial map[string]bool) *Store {
	return &Store{flags: maps.Clone(initial)}
}

// IsEnabled reports whether the named flag is on. Absent flags are off.
func (s *Store) IsEnabled(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.flags[name]
}

// Snapshot returns a defensive copy of all flags. The caller owns the returned
// map and may read it after the lock is released without racing Reload.
func (s *Store) Snapshot() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.flags)
}

// Reload replaces the entire flag set atomically with respect to readers.
func (s *Store) Reload(next map[string]bool) {
	clone := maps.Clone(next)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags = clone
}
```

Note `Reload` clones the incoming map *before* taking the lock, so the O(n) copy
happens outside the critical section and the exclusive `Lock` is held for only
the pointer swap -- minimizing the window that blocks readers.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/flagstore"
)

func main() {
	s := flagstore.New(map[string]bool{"new_checkout": false})

	fmt.Printf("new_checkout before: %v\n", s.IsEnabled("new_checkout"))

	s.Reload(map[string]bool{"new_checkout": true, "dark_mode": true})

	snap := s.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Printf("%s=%v\n", k, snap[k])
	}
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
new_checkout before: false
dark_mode=true
new_checkout=true
```

### Tests

`TestFlagStoreConcurrentReadDuringReload` recreates the production contention:
several reader goroutines call `IsEnabled` and `Snapshot` in a tight loop while a
writer goroutine calls `Reload` repeatedly, all bounded by a short deadline. The
assertion is that every `Snapshot` is internally consistent -- because `Reload`
swaps the whole map atomically, a snapshot always reflects exactly one generation
(here: both flags present together), never a half-updated mix. Running under
`-race -count=10` re-runs the test ten times so the scheduler explores different
interleavings; a race that appears one run in ten is caught.

`t.Context()` (Go 1.24+) gives a context that is cancelled when the test ends,
which we combine with a deadline to bound the hammering.

Create `store_test.go`:

```go
package flagstore

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestFlagStoreConcurrentReadDuringReload(t *testing.T) {
	t.Parallel()

	s := New(map[string]bool{"a": true, "b": true})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup

	// One writer swapping the whole flag set.
	wg.Add(1)
	go func() {
		defer wg.Done()
		gen := 0
		for ctx.Err() == nil {
			gen++
			on := gen%2 == 0
			s.Reload(map[string]bool{"a": on, "b": on})
		}
	}()

	// Many readers taking consistent snapshots.
	const readers = 8
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				snap := s.Snapshot()
				// Every generation sets a and b together, so within one
				// snapshot they must agree. A torn read would break this.
				if len(snap) == 2 && snap["a"] != snap["b"] {
					t.Errorf("inconsistent snapshot: a=%v b=%v", snap["a"], snap["b"])
					return
				}
				_ = s.IsEnabled("a")
			}
		}()
	}

	wg.Wait()
}

func TestSnapshotIsDefensiveCopy(t *testing.T) {
	t.Parallel()

	s := New(map[string]bool{"x": true})
	snap := s.Snapshot()
	snap["x"] = false // mutating the copy must not affect the store

	if !s.IsEnabled("x") {
		t.Fatal("mutating a Snapshot changed the store; copy is not defensive")
	}
}

func TestIsEnabledAbsent(t *testing.T) {
	t.Parallel()

	s := New(nil)
	if s.IsEnabled("nope") {
		t.Fatal("absent flag reported enabled")
	}
}
```

## Review

The store is correct when readers scale without blocking each other and no reader
ever observes a partially-reloaded flag set. The proof is
`TestFlagStoreConcurrentReadDuringReload` passing under `-race -count=10`: readers
and a reloader run concurrently and the detector finds no unordered access, while
the consistency assertion confirms each snapshot is one whole generation.
`TestSnapshotIsDefensiveCopy` pins the reason the clone matters -- mutating a
returned snapshot must not reach back into the store.

The mistake to avoid is the one the defensive copy prevents: returning
`s.flags` directly from `Snapshot` and reading it after `RUnlock`. That is a data
race on lock-protected state and, on a map, a potential `concurrent map writes`
crash. Clone under the lock, or hold the lock for the entire read; never leak a
reference to guarded state. Run `go test -count=10 -race ./...`.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) -- the read/write locking contract this store is built on.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) -- the shallow defensive copy returned by `Snapshot`.
- [The Go Memory Model](https://go.dev/ref/mem) -- why leaking lock-protected state to a caller is a race.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-diagnose-the-race-report.md](02-diagnose-the-race-report.md) | Next: [04-sync-once-lazy-pool-init.md](04-sync-once-lazy-pool-init.md)
