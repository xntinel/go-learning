# Exercise 10: A Thread-Safe In-Memory Repository that Returns Copies

A repository shared across goroutines must do two things at once: guard its store
so reads and writes never race, and hand out copies so a caller cannot reach
through a returned reference and mutate shared state. This module builds exactly
that over a `User` entity — `RWMutex` for many-readers-or-one-writer, snapshot
copies from every accessor, and the internal map never leaked — and proves it under
`go test -race`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
repo/                       independent module: example.com/repo
  go.mod                    go 1.26
  repo.go                   type User (value entity); type Repository (RWMutex); Save/Get/List/Len
  cmd/
    demo/
      main.go               runnable demo: save several, get one, list all
  repo_test.go              tests: not-found, snapshot copy, concurrent readers+writers under -race
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Repository` over `User` guarded by `sync.RWMutex`, with `Get`/`List` taking `RLock`, `Save` taking `Lock`, returning copies and never leaking the internal map; `Get` of a missing id returns `ErrNotFound`.
- Test: concurrent `Save`/`Get`/`List` from many goroutines run clean under `-race`; a missing key returns `ErrNotFound`; a value from `Get` does not alias the stored value; `List` returns a snapshot copy.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repo/cmd/demo
cd ~/go-exercises/repo
go mod init example.com/repo
```

### Guard the store, and hand out copies

These are two separate rules, and a correct repository needs both. *Guard the
store*: `Save`, `Get`, and `List` all touch the internal map, and a map is not safe
for concurrent read-and-write — the runtime will even detect concurrent map access
and crash. An `RWMutex` is the right lock here because reads dominate: `Get` and
`List` take `RLock`, so many readers proceed at once, while `Save` takes the
exclusive `Lock`, so a writer excludes everyone. A plain `Mutex` would serialize
readers needlessly; no lock at all would ship a data race that only shows up under
load. `go test -race` is the arbiter that keeps this honest.

*Hand out copies*: locking alone is not enough. If `Get` returned a pointer into
the map, or `List` returned the internal slice, the caller would hold a reference
to shared state and could mutate it *without holding the lock*, reintroducing the
exact race the mutex was meant to prevent — and corrupting other readers'
invariants. Here `User` is a value type, so returning it from `Get` and storing it
in the map both copy it; a fetched `User` is a snapshot that never changes when the
store moves on. `List` builds a fresh slice and copies each value in, so a caller
can append to or overwrite the returned slice with no effect on the repository. The
internal map is never returned in any form. That is encapsulation surviving
concurrency: the boundary is the lock plus the copy, and nothing crosses it by
reference.

`Repository` uses pointer receivers because it holds a `sync.RWMutex`, which must
not be copied after first use (`go vet` flags a copy). Callers pass `*Repository`
everywhere, so there is a single shared lock guarding a single shared map.

Create `repo.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	ErrNotFound   = errors.New("repo: user not found")
	ErrEmptyID    = errors.New("repo: id is required")
	ErrEmptyName  = errors.New("repo: name is required")
	ErrEmptyEmail = errors.New("repo: email is required")
)

// User is a value-type entity: copying it is a full copy, so the repository can
// hand out snapshots that never alias its stored state.
type User struct {
	id    string
	name  string
	email string
}

func NewUser(id, name, email string) (User, error) {
	if id == "" {
		return User{}, ErrEmptyID
	}
	if name == "" {
		return User{}, ErrEmptyName
	}
	if email == "" {
		return User{}, ErrEmptyEmail
	}
	return User{id: id, name: name, email: email}, nil
}

func (u User) ID() string    { return u.id }
func (u User) Name() string  { return u.name }
func (u User) Email() string { return u.email }

// Repository is a concurrency-safe in-memory store. Reads take RLock, writes take
// Lock; every accessor returns copies and the internal map is never leaked.
type Repository struct {
	mu    sync.RWMutex
	users map[string]User
}

func NewRepository() *Repository {
	return &Repository{users: make(map[string]User)}
}

// Save inserts or replaces a user under an exclusive lock.
func (r *Repository) Save(u User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.id] = u
}

// Get returns a copy of the stored user, or ErrNotFound.
func (r *Repository) Get(id string) (User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return User{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return u, nil
}

// List returns a snapshot slice of all users, sorted by id, that shares no state
// with the repository.
func (r *Repository) List() []User {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]User, 0, len(r.users))
	for _, u := range r.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// Len reports the number of stored users.
func (r *Repository) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repo"
)

func main() {
	r := repo.NewRepository()
	for _, u := range []struct{ id, name, email string }{
		{"u1", "Alice", "alice@example.com"},
		{"u2", "Bob", "bob@example.com"},
	} {
		user, _ := repo.NewUser(u.id, u.name, u.email)
		r.Save(user)
	}

	got, _ := r.Get("u1")
	fmt.Printf("got: %s <%s>\n", got.Name(), got.Email())

	for _, u := range r.List() {
		fmt.Printf("list: %s %s\n", u.ID(), u.Name())
	}

	if _, err := r.Get("missing"); errors.Is(err, repo.ErrNotFound) {
		fmt.Println("missing: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got: Alice <alice@example.com>
list: u1 Alice
list: u2 Bob
missing: not found
```

### Tests

The tests prove both halves. `TestNotFound` checks the missing-key sentinel.
`TestGetIsSnapshot` saves a user, fetches it, overwrites it in the store with a
different name, and asserts the earlier fetch still reads the original — proof the
returned value is a copy. `TestListIsSnapshot` mutates the returned slice and
asserts the repository length is unchanged. `TestConcurrentAccess` is the core one:
it launches many writer and reader goroutines hammering `Save`, `Get`, and `List`
at once and must run clean under `-race`; if the lock were missing or a reference
leaked, the race detector would fire.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func mustUser(t *testing.T, id, name, email string) User {
	t.Helper()
	u, err := NewUser(id, name, email)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	r := NewRepository()
	if _, err := r.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetIsSnapshot(t *testing.T) {
	t.Parallel()
	r := NewRepository()
	r.Save(mustUser(t, "u1", "Alice", "a@b"))

	snapshot, _ := r.Get("u1")
	r.Save(mustUser(t, "u1", "Renamed", "a@b")) // overwrite in store

	if snapshot.Name() != "Alice" {
		t.Fatalf("Get value aliased the store: name = %q, want Alice", snapshot.Name())
	}
}

func TestListIsSnapshot(t *testing.T) {
	t.Parallel()
	r := NewRepository()
	r.Save(mustUser(t, "u1", "Alice", "a@b"))

	list := r.List()
	list = append(list, mustUser(t, "u2", "Bob", "b@c")) // mutate returned slice
	_ = list

	if r.Len() != 1 {
		t.Fatalf("repository changed through List(): Len = %d, want 1", r.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := NewRepository()
	const n = 50
	var wg sync.WaitGroup

	for i := range n {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			id := fmt.Sprintf("u%d", k)
			r.Save(mustUser(t, id, "name", "e@x"))
		}(i)
	}
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Get("u1")
			_ = r.List()
			_ = r.Len()
		}()
	}
	wg.Wait()

	if r.Len() != n {
		t.Fatalf("after %d saves Len = %d", n, r.Len())
	}
}
```

## Review

The repository is correct when it is race-free under `-race` and no accessor leaks
a reference into its store. The two snapshot tests are what catch the classic
leak: `TestGetIsSnapshot` proves a fetched value survives an overwrite unchanged,
and `TestListIsSnapshot` proves mutating the returned slice cannot alter the
store. The mistakes to avoid: a plain `Mutex` where an `RWMutex` fits (needless
reader contention), no lock at all (a race that surfaces only under load), and
returning the internal map or a pointer into it (which reintroduces the race past
the lock). Run `go test -race`; a clean run with concurrent readers and writers is
the proof.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — many readers or one writer; must not be copied after use.
- [Go Blog: Race Detector](https://go.dev/blog/race-detector) — what `go test -race` catches.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) — sharing by communicating vs. guarding with a mutex.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-optimistic-concurrency-version.md](09-optimistic-concurrency-version.md) | Next: [../../08-interfaces/01-implicit-interface-satisfaction/00-concepts.md](../../08-interfaces/01-implicit-interface-satisfaction/00-concepts.md)
