# Exercise 1: A Pointer-Safe Repository — (nil, ErrNotFound), Never (nil, nil)

An in-memory repository is the first thing you build behind almost any service,
and its `Get` signature is the first place pointer-safety is won or lost. This
module builds a concurrency-safe `MemoryRepository` whose `Get` returns a non-nil
`*Entity` on a hit and `(nil, ErrNotFound)` on a miss — never `(nil, nil)` — and
whose `Add` rejects a nil entity, so no caller is ever forced to guess whether a
result is a miss or a bug.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
repo/                       independent module: example.com/repo
  go.mod                    go 1.25
  repo.go                   Entity; Repository interface; MemoryRepository (RWMutex); ErrNotFound, ErrNilEntity
  cmd/
    demo/
      main.go               add, get-hit, get-miss, delete, get-after-delete
  repo_test.go              table tests pinning the miss/hit/nil-receiver contracts; -race
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `MemoryRepository` with `Get`/`Add`/`Delete` guarded by a `sync.RWMutex`; `Get` returns `(nil, ErrNotFound)` on a miss and on a nil receiver; `Add` rejects a nil entity and a nil receiver.
- Test: hit returns the same pointer that was added; miss returns `errors.Is(err, ErrNotFound)` with a nil pointer; a nil `*MemoryRepository` receiver returns `ErrNotFound` without panicking; `Add(nil)` returns a non-nil error; `Delete` of an unknown key returns `ErrNotFound`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repo/cmd/demo
cd ~/go-exercises/repo
go mod init example.com/repo
go mod edit -go=1.25
```

### Why the miss contract is `(nil, ErrNotFound)`

The single most consequential line in a repository is what `Get` returns when the
key is absent. Return `(nil, nil)` and every caller must write two checks — one
for the error, one for the nil pointer — where the second is redundant scaffolding
that exists only because the first was made ambiguous. The day a caller writes
`e, _ := r.Get(id); use(e.Name)` and forgets the pointer check, a miss becomes a
nil dereference and the request panics. Return `(nil, ErrNotFound)` instead: the
caller writes one check, `errors.Is(err, ErrNotFound)`, and only reads the pointer
on the branch where `err == nil`. The value being nil on a miss is fine precisely
*because* the error already told the whole story.

The concurrency shape is a `sync.RWMutex`: `Get` takes `RLock` (many concurrent
readers are safe), `Add` and `Delete` take the write `Lock`. Reads dominate in a
repository, so an `RWMutex` lets readers proceed in parallel while still
serializing every write. Running the tests under `-race` is what proves the lock
discipline is actually correct rather than merely present.

### Nil receivers as a documented no-panic contract

`Get`, `Add`, and `Delete` each guard `if r == nil` at the top. A method on a nil
`*MemoryRepository` is legal — the call is just `Get(r, id)` — and it only panics
if the body touches `r.mu` or `r.m`. Guarding first turns "someone passed a nil
repository" from a panic into an ordinary `ErrNotFound`, which a caller already
knows how to handle. This is a deliberate, tested contract, not an accident.

One design note on `Add`: a nil *entity* is a different failure than a nil
*receiver*, so it gets its own sentinel, `ErrNilEntity`. That lets a caller (or a
test) distinguish "you tried to store nothing" from "the store said not found."

Create `repo.go`:

```go
package repo

import (
	"errors"
	"sync"
)

// ErrNotFound is returned when a lookup misses or a repository is nil.
var ErrNotFound = errors.New("entity not found")

// ErrNilEntity is returned when Add is called with a nil entity.
var ErrNilEntity = errors.New("nil entity")

// Entity is a stored record. Data is always initialized by NewEntity so it is
// never nil for a properly constructed entity.
type Entity struct {
	ID   string
	Name string
	Data map[string]string
}

// NewEntity validates its inputs and returns a non-nil *Entity on success or
// (nil, ErrNotFound-family error) on failure.
func NewEntity(id, name string) (*Entity, error) {
	if id == "" || name == "" {
		return nil, errors.New("id and name must be non-empty")
	}
	return &Entity{ID: id, Name: name, Data: make(map[string]string)}, nil
}

// Repository is the pointer-safe contract: Get never returns (nil, nil).
type Repository interface {
	Get(id string) (*Entity, error)
	Add(e *Entity) error
	Delete(id string) error
}

// MemoryRepository is a concurrency-safe in-memory Repository.
type MemoryRepository struct {
	mu sync.RWMutex
	m  map[string]*Entity
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{m: make(map[string]*Entity)}
}

// Get returns a non-nil *Entity on a hit and (nil, ErrNotFound) on a miss or a
// nil receiver. It never returns (nil, nil).
func (r *MemoryRepository) Get(id string) (*Entity, error) {
	if r == nil {
		return nil, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

// Add stores e under its ID. A nil entity is rejected with ErrNilEntity; a nil
// receiver is rejected with ErrNotFound.
func (r *MemoryRepository) Add(e *Entity) error {
	if r == nil {
		return ErrNotFound
	}
	if e == nil {
		return ErrNilEntity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[e.ID] = e
	return nil
}

// Delete removes id, returning ErrNotFound if it was not present (or the
// receiver is nil).
func (r *MemoryRepository) Delete(id string) error {
	if r == nil {
		return ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; !ok {
		return ErrNotFound
	}
	delete(r.m, id)
	return nil
}
```

### The runnable demo

The demo walks the full lifecycle so you can watch the miss contract in action:
add an entity, read it back, read a key that was never stored (observe the
`ErrNotFound`), delete it, then read again (observe the same sentinel).

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repo"
)

func main() {
	r := repo.NewMemoryRepository()

	alice, _ := repo.NewEntity("u1", "alice")
	_ = r.Add(alice)

	if e, err := r.Get("u1"); err == nil {
		fmt.Printf("hit: %s -> %s\n", e.ID, e.Name)
	}

	if _, err := r.Get("u2"); errors.Is(err, repo.ErrNotFound) {
		fmt.Println("miss: u2 -> not found")
	}

	if err := r.Add(nil); errors.Is(err, repo.ErrNilEntity) {
		fmt.Println("add nil: rejected")
	}

	_ = r.Delete("u1")
	if _, err := r.Get("u1"); errors.Is(err, repo.ErrNotFound) {
		fmt.Println("after delete: u1 -> not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit: u1 -> alice
miss: u2 -> not found
add nil: rejected
after delete: u1 -> not found
```

### Tests

The tests pin every branch of the contract. The hit test asserts `Get` returns the
*same pointer* that was added (identity, `got != want`), because this repository's
`Get` deliberately shares its stored handle — Exercise 3 shows when that is a
hazard and how to break it. The miss test asserts both halves at once: the error
`errors.Is` `ErrNotFound` *and* the pointer is nil. The nil-receiver test proves
`Get` on a `var r *MemoryRepository` returns `ErrNotFound` rather than panicking.
`Add(nil)` must return a non-nil error. The concurrency test hammers the map from
many goroutines so `-race` can prove the `RWMutex` discipline.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNewEntityRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	if _, err := NewEntity("", "n"); err == nil {
		t.Fatal("NewEntity(\"\", \"n\") = nil error, want non-nil")
	}
	if _, err := NewEntity("id", ""); err == nil {
		t.Fatal("NewEntity(\"id\", \"\") = nil error, want non-nil")
	}
}

func TestGetHitReturnsSamePointer(t *testing.T) {
	t.Parallel()
	r := NewMemoryRepository()
	e, _ := NewEntity("u1", "alice")
	if err := r.Add(e); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got != e {
		t.Fatal("Get should return the same pointer that was Added")
	}
}

func TestGetMissReturnsNotFoundAndNil(t *testing.T) {
	t.Parallel()
	r := NewMemoryRepository()
	got, err := r.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil on miss", got)
	}
}

func TestNilReceiverGetReturnsNotFound(t *testing.T) {
	t.Parallel()
	var r *MemoryRepository
	if _, err := r.Get("id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil-receiver Get err = %v, want ErrNotFound", err)
	}
}

func TestAddRejectsNilEntity(t *testing.T) {
	t.Parallel()
	r := NewMemoryRepository()
	if err := r.Add(nil); !errors.Is(err, ErrNilEntity) {
		t.Fatalf("Add(nil) err = %v, want ErrNilEntity", err)
	}
}

func TestDeleteContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		seed      bool
		key       string
		wantError bool
	}{
		{name: "delete existing", seed: true, key: "u1", wantError: false},
		{name: "delete unknown", seed: false, key: "ghost", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewMemoryRepository()
			if tc.seed {
				e, _ := NewEntity(tc.key, "x")
				_ = r.Add(e)
			}
			err := r.Delete(tc.key)
			if tc.wantError {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Delete err = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete err = %v, want nil", err)
			}
			if _, err := r.Get(tc.key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := NewMemoryRepository()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("u%d", i)
			e, _ := NewEntity(id, "x")
			_ = r.Add(e)
			_, _ = r.Get(id)
			_ = r.Delete(id)
		}()
	}
	wg.Wait()
}

func ExampleMemoryRepository_Get() {
	r := NewMemoryRepository()
	e, _ := NewEntity("u1", "alice")
	_ = r.Add(e)

	got, err := r.Get("u1")
	fmt.Println(got.Name, err == nil)

	_, err = r.Get("u2")
	fmt.Println(errors.Is(err, ErrNotFound))
	// Output:
	// alice true
	// true
}
```

## Review

The repository is correct when `Get` returns exactly one of two shapes — a non-nil
pointer with a nil error, or a nil pointer with `ErrNotFound` — and never the
ambiguous `(nil, nil)`. The identity assertion in `TestGetHitReturnsSamePointer`
documents that this `Get` shares its stored handle; that is intentional here and
becomes the subject of the defensive-copy exercise. The nil-receiver tests prove
the `if r == nil` guards turn a would-be panic into an ordinary error. The most
common way to get this wrong is to collapse the miss into `(nil, nil)` "to keep the
signature simple," which does the opposite: it moves complexity into every caller.
Run `go test -race` to confirm the `RWMutex` actually serializes writes against
concurrent reads.

## Resources

- [Go Code Review Comments: pointers vs. values](https://go.dev/wiki/CodeReviewComments#pass-values) — when a signature should be `*T` versus `T`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel like `ErrNotFound`.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read/write locking for a read-heavy store.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-nil-safe-entity-and-constructor.md](02-nil-safe-entity-and-constructor.md)
