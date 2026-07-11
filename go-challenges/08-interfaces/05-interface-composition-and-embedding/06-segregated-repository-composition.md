# Exercise 6: Composing Read and Write Repository Contracts

Interface composition at the *contract* level, for a data layer. You define a
narrow `ReaderRepo` (Get/List) and a narrow `WriterRepo` (Save/Delete), compose
them into a `Store`, and implement one in-memory struct that satisfies all three.
Query handlers depend on `ReaderRepo`; the write path depends on `WriterRepo`; the
wiring uses `Store`. This is "accept the narrowest interface" made concrete.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
repo/                       independent module: example.com/repo
  go.mod                    go 1.26
  repo.go                   ReaderRepo, WriterRepo, Store; MemStore; Names, Seed; ErrNotFound
  cmd/
    demo/
      main.go               seed via WriterRepo, read via ReaderRepo, missing key is ErrNotFound
  repo_test.go              static satisfaction, narrow consumers, ErrNotFound, -race reads, copy safety
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `ReaderRepo` and `WriterRepo` interfaces composed into `Store`; a `MemStore` guarded by `sync.RWMutex` that satisfies all three; `Get` returning a wrapped `ErrNotFound`; `List` returning a stable sorted copy; consumer functions `Names(ReaderRepo)` and `Seed(WriterRepo, ...)`.
- Test: static assertions for all three interfaces; `Names` works via `ReaderRepo` and `Seed` via `WriterRepo`; `Get` on a missing key is `ErrNotFound` (`errors.Is`); concurrent reads under `-race`; `List` returns a copy callers cannot mutate to corrupt internal state.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repo/cmd/demo
cd ~/go-exercises/repo
go mod init example.com/repo
```

### Why segregate, then compose

A single fat `Store` interface with Get/List/Save/Delete forces every consumer to
depend on all four methods even if it only reads. That widens coupling and, more
practically, makes fakes tedious: a read-only handler's test double must stub Save
and Delete it never calls. The senior move is to define the two narrow contracts
that real consumers actually need — `ReaderRepo` for the query path, `WriterRepo`
for the command path — and compose them into `Store` only where the wiring needs
both (the constructor that builds the app). A function then declares the narrowest
interface it uses: `func Names(r ReaderRepo)` cannot accidentally write, and it
accepts any read source, including an in-test fake with two methods.

The composed `Store` interface embeds the two smaller ones — the same
interface-embeds-interfaces mechanic as `ReadWriteCloser`, applied to a domain
contract. One `MemStore` satisfies all three structurally. `Get` wraps
`ErrNotFound` with `%w` so callers branch with `errors.Is` while still getting a
message that names the key. `List` returns a freshly sorted *copy* so a caller
cannot mutate the slice (or reorder it) and corrupt the store's state through the
returned value — a classic aliasing bug when a method hands out its internal slice.

Create `repo.go`:

```go
package repo

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get and Delete for an unknown id.
var ErrNotFound = errors.New("repo: not found")

// User is the stored entity.
type User struct {
	ID   string
	Name string
}

// ReaderRepo is the narrow read-only contract a query handler depends on.
type ReaderRepo interface {
	Get(id string) (User, error)
	List() []User
}

// WriterRepo is the narrow write contract a command handler depends on.
type WriterRepo interface {
	Save(u User) error
	Delete(id string) error
}

// Store composes both contracts for the wiring layer.
type Store interface {
	ReaderRepo
	WriterRepo
}

// MemStore is an in-memory Store guarded by an RWMutex.
type MemStore struct {
	mu    sync.RWMutex
	users map[string]User
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{users: make(map[string]User)}
}

// Get returns the user or a wrapped ErrNotFound.
func (s *MemStore) Get(id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return u, nil
}

// List returns all users sorted by ID, as a fresh slice callers may not use to
// mutate internal state.
func (s *MemStore) List() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b User) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// Save inserts or replaces a user.
func (s *MemStore) Save(u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

// Delete removes a user or returns a wrapped ErrNotFound.
func (s *MemStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return fmt.Errorf("delete %q: %w", id, ErrNotFound)
	}
	delete(s.users, id)
	return nil
}

// Snapshot returns a clone of the backing map so callers cannot mutate the
// store's internal state.
func (s *MemStore) Snapshot() map[string]User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.users)
}

// Names is a read-path consumer that depends only on ReaderRepo.
func Names(r ReaderRepo) []string {
	users := r.List()
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Name
	}
	return names
}

// Seed is a write-path consumer that depends only on WriterRepo.
func Seed(w WriterRepo, users ...User) error {
	for _, u := range users {
		if err := w.Save(u); err != nil {
			return err
		}
	}
	return nil
}

// Static assertions: *MemStore satisfies all three composed contracts.
var _ ReaderRepo = (*MemStore)(nil)
var _ WriterRepo = (*MemStore)(nil)
var _ Store = (*MemStore)(nil)
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
	store := repo.NewMemStore()

	// The write path depends only on WriterRepo.
	repo.Seed(store,
		repo.User{ID: "u2", Name: "bob"},
		repo.User{ID: "u1", Name: "alice"},
	)

	// The read path depends only on ReaderRepo.
	fmt.Println("names:", repo.Names(store))

	u, _ := store.Get("u1")
	fmt.Printf("get u1: %s\n", u.Name)

	_, err := store.Get("missing")
	fmt.Println("missing is ErrNotFound:", errors.Is(err, repo.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
names: [alice bob]
get u1: alice
missing is ErrNotFound: true
```

### Tests

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNarrowConsumers(t *testing.T) {
	t.Parallel()
	s := NewMemStore()
	if err := Seed(s, User{ID: "u1", Name: "alice"}, User{ID: "u2", Name: "bob"}); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got := Names(s)
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("Names = %v, want [alice bob]", got)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	t.Parallel()
	s := NewMemStore()
	_, err := s.Get("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
}

func TestDeleteMissingIsNotFound(t *testing.T) {
	t.Parallel()
	s := NewMemStore()
	if err := s.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete err = %v, want ErrNotFound", err)
	}
}

func TestListReturnsIndependentCopy(t *testing.T) {
	t.Parallel()
	s := NewMemStore()
	s.Save(User{ID: "u1", Name: "alice"})

	list := s.List()
	list[0].Name = "corrupted"

	again := s.List()
	if again[0].Name != "alice" {
		t.Fatalf("internal state corrupted through returned slice: %q", again[0].Name)
	}
}

func TestConcurrentReads(t *testing.T) {
	t.Parallel()
	s := NewMemStore()
	s.Save(User{ID: "u1", Name: "alice"})
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Get("u1")
			s.List()
			s.Snapshot()
		}()
	}
	wg.Wait()
}

func ExampleNames() {
	s := NewMemStore()
	Seed(s, User{ID: "u1", Name: "alice"}, User{ID: "u2", Name: "bob"})
	fmt.Println(Names(s))
	// Output: [alice bob]
}
```

## Review

The design is correct when a read-only consumer typed to `ReaderRepo` and a
write-only consumer typed to `WriterRepo` both compile against `*MemStore`, proving
the narrow contracts are real and the composition holds — the three static `var _`
lines make a signature drift a compile error. `TestListReturnsIndependentCopy` is
the guard against the aliasing bug where a method leaks its internal slice and a
caller's mutation silently corrupts the store. `errors.Is` against `ErrNotFound`
proves the `%w` wrapping is intact. Run `-race` to confirm `RWMutex` actually
permits concurrent readers without a data race. The takeaway: segregate contracts
by what consumers need, compose them only at the wiring seam, and never hand out a
mutable view of internal state.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types) — embedding interfaces to compose contracts.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) and [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the stable ordering for `List`.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the defensive copy behind `Snapshot`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-gzip-decoding-readcloser.md](05-gzip-decoding-readcloser.md) | Next: [07-idle-timeout-conn.md](07-idle-timeout-conn.md)
