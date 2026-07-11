# Exercise 3: Defensive Copy on Read — Stop Callers Mutating Stored State

The repository in Exercise 1 returns its live `*Entity`, which means a caller can
reach back through the pointer and mutate state still sitting in the store. This
module makes that hazard concrete, then fixes it: a `Snapshot` method returns a
deep-enough *value* copy, built with `maps.Clone`, so a caller can mutate its copy
all day without touching the source.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
snapshot/                   independent module: example.com/snapshot
  go.mod                    go 1.25
  store.go                  Entity; Store with Get (aliasing) and Snapshot (defensive copy via maps.Clone)
  cmd/
    demo/
      main.go               show the aliasing leak, then the safe snapshot
  store_test.go             one test documents the leak, one proves the copy is isolated
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store.Get(id) (*Entity, error)` that returns the live pointer, and `Store.Snapshot(id) (Entity, error)` that returns a value with `Data` cloned by `maps.Clone`.
- Test: an aliasing test that mutates the pointer from `Get` and shows the store changed (documents the hazard); a safety test that mutates the copy from `Snapshot` and shows the store is unchanged, and that the copy's `Data` is a distinct map header.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/snapshot/cmd/demo
cd ~/go-exercises/snapshot
go mod init example.com/snapshot
go mod edit -go=1.25
```

### The aliasing leak, seen directly

Return a `*Entity` and you have returned a *handle to shared state*. The caller
holds the same pointer the store holds; there is one `Entity`, one `Data` map, and
two references to it. When the caller writes `e.Data["seen"] = "true"`, that write
lands in the map the store still owns, so the next `Get` of the same key observes
it. Nobody wrote a bug on purpose — the caller just annotated "its" entity — but
under concurrent load the annotation corrupts another request's view. This is the
silent killer from the concepts file, and the fix is to stop sharing the handle on
read paths that must not be mutated.

### `Snapshot` returns a value with a cloned map

`Snapshot` returns `Entity` by value, not `*Entity`. Copying the struct copies the
`ID` and `Name` strings cleanly (strings are immutable), but the `Data` field is a
map, and a map is a reference type: a plain struct copy would leave both the
snapshot and the store pointing at the *same* map. So `Snapshot` must clone the
map explicitly with `maps.Clone`, producing a new map header the caller owns
outright. Now the caller can mutate `copy.Data` freely; the store's map is a
different map and is untouched.

One honesty note the tests pin: `maps.Clone` is *one level deep*. Here `Data` is a
`map[string]string`, and strings are immutable, so a single `maps.Clone` is a true
deep copy. If `Data` were a `map[string][]string`, the cloned map's slices would
still alias the originals and you would have to clone each slice too. "Deep enough"
is a decision you make from the value's type, not a guarantee `maps.Clone` gives.

Create `store.go`:

```go
package snapshot

import (
	"errors"
	"maps"
	"sync"
)

var ErrNotFound = errors.New("entity not found")

type Entity struct {
	ID   string
	Name string
	Data map[string]string
}

type Store struct {
	mu sync.RWMutex
	m  map[string]*Entity
}

func NewStore() *Store {
	return &Store{m: make(map[string]*Entity)}
}

func (s *Store) Add(e *Entity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[e.ID] = e
}

// Get returns the live *Entity. The caller shares the store's state: mutating
// the returned entity's Data mutates the stored copy. Use only where the caller
// is trusted to own the entity.
func (s *Store) Get(id string) (*Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

// Snapshot returns a defensive value copy whose Data map is cloned, so a caller
// may mutate the copy without affecting stored state. maps.Clone is one level
// deep, which is a true deep copy here because Data is map[string]string.
func (s *Store) Snapshot(id string) (Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[id]
	if !ok {
		return Entity{}, ErrNotFound
	}
	return Entity{ID: e.ID, Name: e.Name, Data: maps.Clone(e.Data)}, nil
}
```

### The runnable demo

The demo shows both halves: first the aliasing `Get` leaks a mutation into the
store; then a fresh store with `Snapshot` keeps the store clean despite the same
caller-side mutation.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/snapshot"
)

func main() {
	// Aliasing hazard through Get.
	s1 := snapshot.NewStore()
	s1.Add(&snapshot.Entity{ID: "u1", Name: "alice", Data: map[string]string{"role": "user"}})
	e, _ := s1.Get("u1")
	e.Data["role"] = "admin" // reaches into stored state
	again, _ := s1.Get("u1")
	fmt.Printf("via Get: stored role=%s (leaked)\n", again.Data["role"])

	// Safe read through Snapshot.
	s2 := snapshot.NewStore()
	s2.Add(&snapshot.Entity{ID: "u1", Name: "alice", Data: map[string]string{"role": "user"}})
	cp, _ := s2.Snapshot("u1")
	cp.Data["role"] = "admin" // mutates only the copy
	src, _ := s2.Snapshot("u1")
	fmt.Printf("via Snapshot: stored role=%s (safe)\n", src.Data["role"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
via Get: stored role=admin (leaked)
via Snapshot: stored role=user (safe)
```

### Tests

`TestGetAliasesStoredState` documents the hazard: it mutates the entity returned
by `Get` and asserts a later `Get` sees the mutation — proving `Get` shares the
handle. `TestSnapshotIsolatesCopy` proves the fix: it mutates the value returned
by `Snapshot` and asserts a later read is unchanged, and it asserts the copy's map
is a *distinct* map so writing the copy cannot touch the original. Note the test
uses a pointer-based helper to compare map identity: two maps are the "same" map
only if a write to one is visible through the other, which is exactly what the
isolation assertion checks behaviorally.

Create `store_test.go`:

```go
package snapshot

import (
	"fmt"
	"testing"
)

func seed() *Store {
	s := NewStore()
	s.Add(&Entity{ID: "u1", Name: "alice", Data: map[string]string{"role": "user"}})
	return s
}

func TestGetAliasesStoredState(t *testing.T) {
	t.Parallel()
	s := seed()
	e, err := s.Get("u1")
	if err != nil {
		t.Fatal(err)
	}
	e.Data["role"] = "admin"

	again, _ := s.Get("u1")
	if again.Data["role"] != "admin" {
		t.Fatalf("Get did not alias: stored role=%q, want admin (leak expected)", again.Data["role"])
	}
}

func TestSnapshotIsolatesCopy(t *testing.T) {
	t.Parallel()
	s := seed()
	cp, err := s.Snapshot("u1")
	if err != nil {
		t.Fatal(err)
	}
	cp.Data["role"] = "admin"

	src, _ := s.Snapshot("u1")
	if src.Data["role"] != "user" {
		t.Fatalf("Snapshot leaked: stored role=%q, want user", src.Data["role"])
	}

	// The copy's map must be a distinct map: adding a key to it must not appear
	// in a fresh snapshot of the source.
	cp.Data["extra"] = "x"
	src2, _ := s.Snapshot("u1")
	if _, present := src2.Data["extra"]; present {
		t.Fatal("copy's Data aliases the source map; want a distinct map")
	}
}

func TestSnapshotMiss(t *testing.T) {
	t.Parallel()
	s := seed()
	if _, err := s.Snapshot("ghost"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func ExampleStore_Snapshot() {
	s := seed()
	cp, _ := s.Snapshot("u1")
	cp.Data["role"] = "admin"

	src, _ := s.Snapshot("u1")
	fmt.Println(src.Data["role"]) // unchanged
	// Output: user
}
```

## Review

The distinction is correct when `Get` shares the handle (a mutation through it is
visible in the store) and `Snapshot` isolates it (a mutation through the copy is
not), with the copy holding a distinct `Data` map. The aliasing test is not a bug
report — it is a *documented hazard*, deliberately asserting the leak so the
contrast with `Snapshot` is explicit. The mistake to avoid is reaching for a plain
struct copy and thinking you are safe: copying `Entity` by value still shares the
`Data` map, because the copy duplicates the map *header*, not the backing data. The
second mistake is over-trusting `maps.Clone`: it is one level deep, which is a true
deep copy only when the values are immutable (like strings here). For nested
reference types you must clone each level you need isolated.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — a shallow (one-level) copy of a map.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the slice analogue, also one level deep.
- [Go blog: Go maps in action](https://go.dev/blog/maps) — maps are reference types; a copy shares the backing data.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-nil-safe-entity-and-constructor.md](02-nil-safe-entity-and-constructor.md) | Next: [04-patch-tri-state-pointer-fields.md](04-patch-tri-state-pointer-fields.md)
