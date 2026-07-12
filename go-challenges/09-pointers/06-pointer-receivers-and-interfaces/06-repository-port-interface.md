# Exercise 6: Hexagonal Port: An In-Memory Store Satisfied Only by *T

Dependency inversion in Go is a port interface plus an adapter, and the adapter is
almost always `*T` because it owns mutable state behind a mutex. This module builds
a `UserStore` port and an in-memory `*memStore` adapter guarded by a
`sync.RWMutex`, with a sentinel not-found error and a `-race` concurrency test.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
userstore/                  independent module: example.com/userstore
  go.mod                    go 1.25
  store.go                  UserStore port; *memStore (map + RWMutex); ErrNotFound
  cmd/
    demo/
      main.go               wire the port to *memStore, save and look up
  store_test.go             satisfaction contract, save/find, sentinel miss, -race
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `UserStore` interface (`Save`, `FindByID`, `Snapshot`) and a `*memStore` adapter holding a `map` behind a `sync.RWMutex`, returning a wrapped `ErrNotFound` on a miss.
- Test: `var _ UserStore = (*memStore)(nil)`; save-then-find returns the value; a missing id returns an error matched with `errors.Is(err, ErrNotFound)`; parallel `Save`/`FindByID` under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why the adapter must be *memStore

The `memStore` holds two pieces of mutable state: a `map[string]User` and the
`sync.RWMutex` that guards it. Both force a pointer adapter. The mutex must not be
copied — a copy is a *different* lock, so two goroutines could each hold "the"
lock and corrupt the map; `go vet` copylocks flags copies of `sync.RWMutex`. The
map header and its contents are mutated by `Save`. Either reason alone requires a
pointer receiver on every method; together they mean the type is only ever used as
`*memStore`. Following the convention, `FindByID` and `Snapshot` — which only read
— also take pointer receivers, so the method set is uniform.

That uniformity is what makes `*memStore` satisfy the `UserStore` port. Callers
depend on the interface, never on `memStore`; the wiring code constructs a
`*memStore` and passes it wherever a `UserStore` is expected. The compile-time
assertion `var _ UserStore = (*memStore)(nil)` proves the adapter satisfies the
port at the definition site — if a method signature drifts, this line fails to
compile immediately.

`FindByID` returns a sentinel-wrapped error on a miss so callers can branch with
`errors.Is(err, ErrNotFound)` without string matching. `Snapshot` returns a
defensive copy via `maps.Clone` so a caller iterating the result cannot race with
a concurrent `Save`.

Create `store.go`:

```go
// store.go
package userstore

import (
	"errors"
	"fmt"
	"maps"
	"sync"
)

// ErrNotFound is the sentinel returned (wrapped) when an id is absent.
var ErrNotFound = errors.New("user not found")

// User is the stored aggregate.
type User struct {
	ID   string
	Name string
}

// UserStore is the persistence port. Callers depend on this interface; the
// concrete adapter is wired in.
type UserStore interface {
	Save(u User) error
	FindByID(id string) (User, error)
	Snapshot() map[string]User
}

// memStore is an in-memory adapter. It holds a map behind an RWMutex, so it must
// never be copied and is always used as *memStore.
type memStore struct {
	mu    sync.RWMutex
	users map[string]User
}

// Compile-time contract: *memStore satisfies UserStore. A memStore value does
// not (its methods have pointer receivers), and copying it would copy the lock.
var _ UserStore = (*memStore)(nil)

// NewMemStore returns an empty in-memory UserStore.
func NewMemStore() *memStore {
	return &memStore{users: make(map[string]User)}
}

// Save inserts or replaces a user. It takes the write lock.
func (s *memStore) Save(u User) error {
	if u.ID == "" {
		return errors.New("userstore: empty id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

// FindByID returns the user or a wrapped ErrNotFound. It takes the read lock.
func (s *memStore) FindByID(id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, fmt.Errorf("find %q: %w", id, ErrNotFound)
	}
	return u, nil
}

// Snapshot returns a defensive copy of the store under the read lock, so callers
// can iterate it without racing a concurrent Save.
func (s *memStore) Snapshot() map[string]User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.users)
}
```

Note `NewMemStore` returns the unexported `*memStore` directly. That is idiomatic
for an adapter: callers hold it as the `UserStore` interface, and returning the
concrete pointer lets the wiring code also reach any adapter-specific method if it
needs to. (`go vet` does not object; the exported interface is the contract.)

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"

	"example.com/userstore"
)

func main() {
	var store userstore.UserStore = userstore.NewMemStore() // depend on the port

	_ = store.Save(userstore.User{ID: "u1", Name: "alice"})
	_ = store.Save(userstore.User{ID: "u2", Name: "bob"})

	u, _ := store.FindByID("u1")
	fmt.Printf("found: %s -> %s\n", u.ID, u.Name)

	_, err := store.FindByID("missing")
	fmt.Printf("miss is ErrNotFound: %v\n", errors.Is(err, userstore.ErrNotFound))

	fmt.Printf("snapshot size: %d\n", len(store.Snapshot()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: u1 -> alice
miss is ErrNotFound: true
snapshot size: 2
```

### Tests

`TestSaveThenFind` covers the happy path. `TestMissIsSentinel` asserts the miss
matches `ErrNotFound` via `errors.Is`. `TestConcurrent` fans out parallel `Save`
and `FindByID` calls; under `-race` it proves the `RWMutex` actually guards the
map.

Create `store_test.go`:

```go
// store_test.go
package userstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestStoreContract(t *testing.T) {
	t.Parallel()
	var _ UserStore = (*memStore)(nil)
}

func TestSaveThenFind(t *testing.T) {
	t.Parallel()

	s := NewMemStore()
	if err := s.Save(User{ID: "u1", Name: "alice"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	u, err := s.FindByID("u1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if u.Name != "alice" {
		t.Fatalf("Name = %q, want alice", u.Name)
	}
}

func TestMissIsSentinel(t *testing.T) {
	t.Parallel()

	s := NewMemStore()
	_, err := s.FindByID("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wrapped ErrNotFound", err)
	}
}

func TestEmptyIDRejected(t *testing.T) {
	t.Parallel()

	s := NewMemStore()
	if err := s.Save(User{ID: "", Name: "x"}); err == nil {
		t.Fatal("Save with empty id = nil, want error")
	}
}

func TestConcurrent(t *testing.T) {
	t.Parallel()

	s := NewMemStore()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("u%d", i)
			_ = s.Save(User{ID: id, Name: id})
			_, _ = s.FindByID(id)
			_ = s.Snapshot()
		}()
	}
	wg.Wait()
	if got := len(s.Snapshot()); got != 200 {
		t.Fatalf("snapshot size = %d, want 200", got)
	}
}
```

## Review

The adapter is correct when every access goes through the `RWMutex` and a miss is
a wrapped `ErrNotFound` that `errors.Is` matches. The `var _ UserStore =
(*memStore)(nil)` contract is the enforcement point for dependency inversion: it
guarantees the concrete adapter still fits the port after any refactor. The reason
the whole type lives behind a pointer is the mutex — copy it and you get two locks
and a corrupt map, which is exactly the class of bug `go vet` copylocks exists to
prevent. `Snapshot` returning `maps.Clone` of the map (under the read lock) is the
defensive-copy pattern that lets callers iterate results without holding the lock
or racing a writer.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — read/write locking for a read-mostly store.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — a shallow copy for defensive snapshots.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-typed-nil-error-interface.md](05-typed-nil-error-interface.md) | Next: [07-method-value-callback-worker.md](07-method-value-callback-worker.md)
