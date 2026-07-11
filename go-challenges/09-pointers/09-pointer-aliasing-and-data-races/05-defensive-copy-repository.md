# Exercise 5: Stop an In-Memory Repository from Leaking Aliased Rows

An in-memory repository backed by a map is the first thing you build for a test
double, a cache, or a small service — and the most common place to leak aliased
mutable state. If `Get` returns the stored `*User`, every caller holds a live
pointer into the map: one caller's `u.Roles = append(...)` mutates the store and
races every other reader. This module builds the buggy version, then the fix that
returns a defensive copy with cloned reference-typed fields, and proves both the
regression and the concurrency safety under `-race`.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
userrepo/                  independent module: example.com/userrepo
  go.mod                   module example.com/userrepo
  userrepo.go              UserRepo (map[string]*User + RWMutex); Get returns a deep value copy; Put; GetAliased (buggy, for contrast)
  cmd/
    demo/
      main.go              runnable demo: mutate a returned User, show the store is unaffected
  userrepo_test.go         regression (mutation isolation) + concurrent Get/Put under -race + deep-copy of slice field
```

- Files: `userrepo.go`, `cmd/demo/main.go`, `userrepo_test.go`.
- Implement: a `UserRepo` on `map[string]*User` guarded by `sync.RWMutex`; a safe `Get` returning a value copy with `slices.Clone`d slice fields; a `Put`; and a deliberately buggy `GetAliased` for contrast.
- Test: (a) mutate a returned `User` and its slice field, assert a later `Get` is unaffected; (b) concurrent `Get`/`Put` under `-race`; (c) prove a shallow copy would still alias the slice.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userrepo/cmd/demo
cd ~/go-exercises/userrepo
go mod init example.com/userrepo
```

### Aliasing through a map value, and why a shallow copy is not enough

The store is `map[string]*User`. `GetAliased` returns the stored `*User` directly.
That pointer aliases the exact `User` the map owns, so a caller doing `u.Name =
"x"` or `u.Roles = append(u.Roles, "admin")` mutates the repository's own state —
and if another goroutine is reading that `User` at the same time, it is a data
race. This is the map-value aliasing root cause: the map hands out a pointer into
its own storage.

The naive fix is "return a copy": `cp := *stored; return &cp`. That copies the
struct, but a struct copy is *shallow*. The `Roles []string` field in the copy
shares its backing array with the original: `cp.Roles` and `stored.Roles` are two
slice headers pointing at one array. A caller who does `cp.Roles[0] = "hacked"`
still corrupts the store, and a caller who `append`s within capacity still writes
into the shared array. So the real fix must *deep* copy: copy the struct *and*
`slices.Clone` every reference-typed field. `maps.Clone` does the same for a map
field. Only then is the returned `User` fully independent of the store.

The `RWMutex` guards the map so concurrent `Get`s (readers) can overlap while
`Put` (writer) takes the exclusive lock. But the mutex alone does not solve
aliasing: even with the lock, if `Get` returns a pointer into the map, the caller
mutates *after* releasing the lock, unsynchronized against other readers. The lock
protects the map lookup; the deep copy protects the returned data. You need both.

Create `userrepo.go`:

```go
package userrepo

import (
	"slices"
	"sync"
)

// User is a stored row. Roles is a reference-typed field, so a shallow struct
// copy still aliases its backing array; a safe Get must clone it.
type User struct {
	ID    string
	Name  string
	Roles []string
}

// clone returns a fully independent copy of u: the struct is copied by value and
// every reference-typed field is cloned so the result shares no backing storage
// with the original.
func (u *User) clone() *User {
	cp := *u
	cp.Roles = slices.Clone(u.Roles)
	return &cp
}

// UserRepo is a concurrency-safe in-memory repository.
type UserRepo struct {
	mu    sync.RWMutex
	users map[string]*User
}

func NewUserRepo() *UserRepo {
	return &UserRepo{users: make(map[string]*User)}
}

// Put stores a deep copy of u, so a later mutation of the caller's u cannot
// reach into the store either.
func (r *UserRepo) Put(u *User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.ID] = u.clone()
}

// Get returns a deep copy of the stored user, so the caller cannot alias or race
// the store. Reports (nil, false) when absent.
func (r *UserRepo) Get(id string) (*User, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return nil, false
	}
	return u.clone(), true
}

// GetAliased is the BUGGY version kept for contrast: it returns the stored
// pointer directly, so callers alias and can mutate/race the store. Never ship
// this shape.
func (r *UserRepo) GetAliased(id string) (*User, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	return u, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/userrepo"
)

func main() {
	repo := userrepo.NewUserRepo()
	repo.Put(&userrepo.User{ID: "u1", Name: "alice", Roles: []string{"reader"}})

	// A caller mutates its returned copy, including the slice field.
	got, _ := repo.Get("u1")
	got.Name = "mallory"
	got.Roles[0] = "admin"

	// The store is unaffected because Get returned a deep copy.
	fresh, _ := repo.Get("u1")
	fmt.Printf("store name: %s, store roles: %v\n", fresh.Name, fresh.Roles)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
store name: alice, store roles: [reader]
```

### Tests

`TestGetReturnsDefensiveCopy` is the regression test: mutate the returned `User`'s
scalar and slice fields, then `Get` again and assert the store is untouched — this
fails against `GetAliased` and passes against `Get`. `TestShallowCopyStillAliases`
demonstrates *why* the deep copy is necessary by showing a shallow `cp := *stored`
shares the `Roles` backing array (using `GetAliased` to reach the stored pointer).
`TestConcurrentGetPut` runs readers `Get`-ing while a writer `Put`s, under `-race`.

Create `userrepo_test.go`:

```go
package userrepo

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestGetReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()

	repo := NewUserRepo()
	repo.Put(&User{ID: "u1", Name: "alice", Roles: []string{"reader"}})

	got, ok := repo.Get("u1")
	if !ok {
		t.Fatal("Get(u1) missing")
	}
	got.Name = "mallory"
	got.Roles[0] = "admin"

	fresh, _ := repo.Get("u1")
	if fresh.Name != "alice" {
		t.Errorf("store name mutated: %q, want alice", fresh.Name)
	}
	if !slices.Equal(fresh.Roles, []string{"reader"}) {
		t.Errorf("store roles mutated: %v, want [reader]", fresh.Roles)
	}
}

func TestPutStoresIndependentCopy(t *testing.T) {
	t.Parallel()

	repo := NewUserRepo()
	u := &User{ID: "u1", Name: "alice", Roles: []string{"reader"}}
	repo.Put(u)

	// Mutating the caller's u after Put must not reach the store.
	u.Roles[0] = "admin"

	fresh, _ := repo.Get("u1")
	if !slices.Equal(fresh.Roles, []string{"reader"}) {
		t.Errorf("Put aliased caller's slice: %v, want [reader]", fresh.Roles)
	}
}

func TestShallowCopyStillAliases(t *testing.T) {
	t.Parallel()

	repo := NewUserRepo()
	repo.Put(&User{ID: "u1", Name: "alice", Roles: []string{"reader"}})

	stored, _ := repo.GetAliased("u1")
	shallow := *stored // struct copy: Roles header copied, backing array shared

	shallow.Roles[0] = "admin"
	if stored.Roles[0] != "admin" {
		t.Fatal("expected a shallow copy to share the slice backing array")
	}
}

func TestConcurrentGetPut(t *testing.T) {
	t.Parallel()

	repo := NewUserRepo()
	repo.Put(&User{ID: "u1", Name: "alice", Roles: []string{"reader"}})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo.Put(&User{ID: "u1", Name: fmt.Sprintf("n%d", i), Roles: []string{"reader"}})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u, ok := repo.Get("u1"); ok {
				u.Roles = append(u.Roles, "local") // mutating the copy is safe
			}
		}()
	}
	wg.Wait()
}

func Example() {
	repo := NewUserRepo()
	repo.Put(&User{ID: "u1", Name: "alice", Roles: []string{"reader"}})
	got, _ := repo.Get("u1")
	got.Roles[0] = "admin"
	fresh, _ := repo.Get("u1")
	fmt.Println(fresh.Roles[0])
	// Output: reader
}
```

## Review

The repository is correct when a caller cannot reach the store through a returned
value: mutating the returned `User` or its `Roles` slice leaves a subsequent `Get`
unchanged, and concurrent `Get`/`Put` is `-race` clean. The two mistakes this
module pins are the whole lesson of the exercise. First, returning the stored
pointer (`GetAliased`) leaks aliased mutable state — every caller can corrupt and
race the store. Second, "return a copy" with a shallow struct copy is a false fix:
`TestShallowCopyStillAliases` shows the `Roles` backing array is still shared, so
you must `slices.Clone` (and `maps.Clone` a map field) every reference-typed field.
The `RWMutex` guards the map lookup; the deep copy guards the returned data; you
need both.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — a fresh backing array copy of a slice field.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the same for a map field of a returned struct.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — concurrent readers, exclusive writer, for the map lookup.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-rwmutex-compound-state-store.md](06-rwmutex-compound-state-store.md)
