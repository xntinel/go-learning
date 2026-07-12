# Exercise 6: One Shared Fixture Across Parallel Subtests

An integration fixture — a database, a testcontainer, a seeded store — is
expensive to build, so you build it once and let many parallel cases read it. The
correctness question is teardown: the fixture must be closed exactly once, and
only after every parallel child has finished using it. This exercise builds a
read-only repository over an in-memory store, reads it from parallel subtests, and
tears it down in a parent `t.Cleanup` that provably runs after all children.

This module is fully self-contained: its own `go mod init`, store, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
userrepo/                   independent module: example.com/userrepo
  go.mod                    go 1.26
  store.go                  type Store (read-only, RWMutex-guarded); Get, Live, Close
  cmd/
    demo/
      main.go               runnable demo: seed a store, read a user
  store_test.go             one fixture, parallel readers, parent Cleanup after children
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a concurrency-safe read-only `Store` with `Get`, `Live`, and `Close`.
- Test: build the fixture once in the parent, read it from parallel subtests,
  close it in a parent `t.Cleanup` that asserts all children completed first.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/04-subtests-and-t-run/06-shared-fixture-parallel/cmd/demo
cd go-solutions/12-testing-ecosystem/04-subtests-and-t-run/06-shared-fixture-parallel
```

### Why parent Cleanup is the right teardown hook

A parent's `t.Cleanup` runs when the test *and all its subtests* have finished —
including parallel children, which resume and complete only after the parent
function returns. So registering `store.Close` in the parent's `t.Cleanup` closes
the fixture exactly once, after the last parallel reader is done. Contrast the two
tempting-but-wrong alternatives: a `defer store.Close()` in the parent function
runs when that function returns, which is *before* the parallel children resume;
and a `Close` inside any child would close the fixture while its siblings are
still reading. `t.Cleanup` on the parent (or the wrapper-group idiom from the
previous exercise) is the only correct placement.

The shared state must be safe for concurrent reads. `Store` guards its map with a
`sync.RWMutex`: readers take `RLock`, the single teardown takes `Lock`. Because
the parallel phase does only reads and the write (`Close`) happens after every
reader, `-race` stays clean. To *prove* the ordering rather than assume it, the
test counts completed children with an atomic and asserts in the cleanup that the
count reached the full set before `Close` ran.

Create `store.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned by Get when no user has the requested id.
var ErrNotFound = errors.New("user not found")

// User is a repository row.
type User struct {
	ID   string
	Name string
}

// Store is a read-only, concurrency-safe user repository backed by an in-memory
// map — a stand-in for a database or testcontainer that is expensive to build
// and is shared read-only across many parallel test cases.
type Store struct {
	mu     sync.RWMutex
	users  map[string]User
	closed bool
}

// NewStore builds a store from a seed map, copying it so the caller cannot mutate
// the store's backing map afterward.
func NewStore(seed map[string]User) *Store {
	m := make(map[string]User, len(seed))
	for k, v := range seed {
		m[k] = v
	}
	return &Store{users: m}
}

// Get returns the user with the given id, or ErrNotFound. It fails if the store
// has been closed.
func (s *Store) Get(ctx context.Context, id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return User{}, errors.New("store is closed")
	}
	u, ok := s.users[id]
	if !ok {
		return User{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return u, nil
}

// Live reports whether the store is still open.
func (s *Store) Live() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed
}

// Close marks the store closed. It is idempotent.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/userrepo"
)

func main() {
	store := userrepo.NewStore(map[string]userrepo.User{
		"u1": {ID: "u1", Name: "Ada"},
		"u2": {ID: "u2", Name: "Bo"},
	})
	defer store.Close()

	ctx := context.Background()
	if u, err := store.Get(ctx, "u1"); err == nil {
		fmt.Printf("found: %s\n", u.Name)
	}
	if _, err := store.Get(ctx, "u9"); errors.Is(err, userrepo.ErrNotFound) {
		fmt.Println("u9: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: Ada
u9: not found
```

### Tests

`TestRepo` builds the fixture once, registers a parent `t.Cleanup` that asserts
every child completed before closing the store, then reads the store from one
parallel subtest per user. Each child asserts the store is `Live` during its run
and reads its user. The atomic `completed` counter, checked in the cleanup, proves
teardown happened strictly after all children. `TestGetNotFound` pins the sentinel
error. `-race` proves the concurrent reads are safe.

Create `store_test.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestRepo(t *testing.T) {
	seed := map[string]User{
		"u1": {ID: "u1", Name: "Ada"},
		"u2": {ID: "u2", Name: "Bo"},
		"u3": {ID: "u3", Name: "Cy"},
	}
	store := NewStore(seed)

	var completed atomic.Int64
	t.Cleanup(func() {
		// Parent cleanup: runs after every subtest, including the parallel ones.
		if got := completed.Load(); got != int64(len(seed)) {
			t.Errorf("cleanup ran with %d children complete, want %d", got, len(seed))
		}
		store.Close()
	})

	for id, want := range seed {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			if !store.Live() {
				t.Fatal("store was closed during a subtest")
			}
			got, err := store.Get(t.Context(), id)
			if err != nil {
				t.Fatalf("Get(%q) = %v", id, err)
			}
			if got != want {
				t.Fatalf("Get(%q) = %+v, want %+v", id, got, want)
			}
			completed.Add(1)
		})
	}
}

func TestGetNotFound(t *testing.T) {
	t.Parallel()
	store := NewStore(nil)
	_, err := store.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want errors.Is ErrNotFound", err)
	}
}

func ExampleStore_Get() {
	store := NewStore(map[string]User{"u1": {ID: "u1", Name: "Ada"}})
	u, _ := store.Get(context.Background(), "u1")
	fmt.Println(u.Name)
	// Output: Ada
}
```

## Review

The shared-fixture pattern is safe only when two things hold: the fixture is
read-only during the parallel phase, and its single teardown runs after every
reader. The `sync.RWMutex` gives the first — parallel `Get`/`Live` calls take a
read lock and never mutate — and the parent `t.Cleanup` gives the second, because
a parent's cleanup runs after all subtests including parallel ones. The atomic
`completed` counter turns "runs after" from an assumption into an assertion: if
`Close` ever ran with fewer than all children done, the cleanup's `t.Errorf`
fires. The banned alternative is `defer store.Close()` in the parent body, which
runs before the parallel children resume and would close the store out from under
them. Note there is no `id, want := id, want` shadowing before the `t.Run`: on Go
1.22+ loop variables are per-iteration, so capturing `id` and `want` in the
parallel closure is already safe — the old aliasing dance is obsolete at `go 1.26`.

## Resources

- [testing.T.Cleanup — pkg.go.dev](https://pkg.go.dev/testing#T.Cleanup)
- [testing.T.Parallel — pkg.go.dev](https://pkg.go.dev/testing#T.Parallel)
- [sync.RWMutex — pkg.go.dev](https://pkg.go.dev/sync#RWMutex)

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-subtest-failure-isolation.md](07-subtest-failure-isolation.md)
