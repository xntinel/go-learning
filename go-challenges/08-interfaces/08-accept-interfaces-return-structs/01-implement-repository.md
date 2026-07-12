# Exercise 1: Implement A Repository: Accept The Interface, Return The Struct

This is the base artifact every later module composes with: a `Repository`
interface, a concurrency-safe `MemoryRepository` struct, and a `Service` whose
constructor accepts the interface and returns the concrete `*Service`. It is the
smallest complete instance of the rule that names the whole chapter.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
repository/                 independent module: example.com/repository
  go.mod                    go 1.26
  repo.go                   Item, ErrNotFound; Repository iface; MemoryRepository struct; Service
  cmd/
    demo/
      main.go               wires a MemoryRepository into a Service and exercises it
  repo_test.go              table tests on MemoryRepository; Service round-trip; -race
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: a `Repository` interface (`Get`/`Put`/`Delete`), a `MemoryRepository` guarded by a `sync.RWMutex`, and a `Service` whose `NewService(Repository) *Service` accepts the interface and returns the struct.
Test: table tests over `Put`/`Get`/`Delete`; `errors.Is(err, ErrNotFound)` on a missing key; `Service.AddItem`/`GetItem`/`RemoveItem` round-trip through the injected interface; `go test -race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/08-accept-interfaces-return-structs/01-implement-repository/cmd/demo
cd go-solutions/08-interfaces/08-accept-interfaces-return-structs/01-implement-repository
go mod edit -go=1.26
```

### Why the constructor accepts an interface and returns a struct

`NewService` takes a `Repository` — an interface — so a caller can hand it the real
in-memory store, a fake in a test, or a decorator that adds caching or retries. It
returns `*Service` — a concrete pointer — so callers keep every method the service
has now and every method it grows later, with no interface to widen. That single
signature, `func NewService(r Repository) *Service`, is the rule in one line: the
dependency comes in through the abstraction, the product goes out as the concrete
type.

The `Repository` interface lives in this package because *this* package is the
consumer: `Service` is the thing that calls `Get`/`Put`/`Delete`. `MemoryRepository`
is the producer; it exports only the concrete type. The compile-time assertion
`var _ Repository = (*MemoryRepository)(nil)` pins the satisfaction so that if the
interface and the implementation ever drift, the build fails immediately rather than
some call site failing later.

The store is guarded by a `sync.RWMutex`: reads (`Get`) take the read lock so
concurrent reads proceed in parallel, and writes (`Put`, `Delete`) take the write
lock exclusively. A missing key returns the package sentinel `ErrNotFound`, which
every later layer — cache, retry, HTTP handler — matches with `errors.Is` to branch
on the "absent" outcome without depending on a concrete error type.

Create `repo.go`:

```go
package repository

import (
	"errors"
	"sync"
)

// ErrNotFound is returned by Get and Delete when no item exists for the id. It
// is a package-level sentinel so callers and decorators can branch on it with
// errors.Is without depending on a concrete error type.
var ErrNotFound = errors.New("repository: item not found")

// Item is the stored value. A struct so callers keep full field access.
type Item struct {
	ID    string
	Name  string
	Price int64
}

// Repository is the consumer-owned port. Service depends on these three methods;
// any store that provides them can be substituted.
type Repository interface {
	Get(id string) (Item, error)
	Put(item Item) error
	Delete(id string) error
}

// MemoryRepository is a concurrency-safe in-memory Repository. It is the concrete
// producer type; constructors return it, callers narrow to Repository at the seam.
type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]Item
}

// NewMemoryRepository returns an empty store. It returns the concrete *type* so a
// caller keeps access to every MemoryRepository method, present and future.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]Item)}
}

// compile-time proof that the concrete type still satisfies the port.
var _ Repository = (*MemoryRepository)(nil)

func (m *MemoryRepository) Get(id string) (Item, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (m *MemoryRepository) Put(item Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.ID] = item
	return nil
}

func (m *MemoryRepository) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrNotFound
	}
	delete(m.items, id)
	return nil
}

// Service is the consumer. Its constructor accepts the interface (so the store is
// substitutable) and returns the struct (so callers keep every method).
type Service struct {
	repo Repository
}

// NewService accepts a Repository and returns *Service: accept the interface,
// return the struct, in one signature.
func NewService(r Repository) *Service {
	return &Service{repo: r}
}

// AddItem stores an item through the injected repository.
func (s *Service) AddItem(item Item) error {
	return s.repo.Put(item)
}

// GetItem reads an item through the injected repository.
func (s *Service) GetItem(id string) (Item, error) {
	return s.repo.Get(id)
}

// RemoveItem deletes an item through the injected repository.
func (s *Service) RemoveItem(id string) error {
	return s.repo.Delete(id)
}
```

### The runnable demo

The demo wires a concrete `MemoryRepository` into a `Service` through the
`Repository` interface and exercises the round-trip: add, read, remove, read-again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repository"
)

func main() {
	store := repository.NewMemoryRepository()
	svc := repository.NewService(store)

	if err := svc.AddItem(repository.Item{ID: "sku-1", Name: "widget", Price: 1299}); err != nil {
		fmt.Println("add:", err)
		return
	}

	item, err := svc.GetItem("sku-1")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	fmt.Printf("got %s priced %d\n", item.Name, item.Price)

	if err := svc.RemoveItem("sku-1"); err != nil {
		fmt.Println("remove:", err)
		return
	}

	if _, err := svc.GetItem("sku-1"); errors.Is(err, repository.ErrNotFound) {
		fmt.Println("after remove: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got widget priced 1299
after remove: not found
```

### Tests

The tests cover `MemoryRepository` directly (table-driven `Put`/`Get`/`Delete`,
including the `ErrNotFound` paths asserted with `errors.Is`), the `Service`
round-trip through the injected interface, and a `-race` concurrency test that
hammers the `RWMutex`.

Create `repo_test.go`:

```go
package repository

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestMemoryRepositoryPutGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item Item
	}{
		{"simple", Item{ID: "i1", Name: "apple", Price: 100}},
		{"zero price", Item{ID: "i2", Name: "sample", Price: 0}},
		{"long name", Item{ID: "i3", Name: "a-rather-long-product-name", Price: 999}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewMemoryRepository()
			if err := r.Put(tc.item); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := r.Get(tc.item.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got != tc.item {
				t.Fatalf("Get = %+v, want %+v", got, tc.item)
			}
		})
	}
}

func TestMemoryRepositoryGetMissing(t *testing.T) {
	t.Parallel()
	r := NewMemoryRepository()
	if _, err := r.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) err = %v, want ErrNotFound", err)
	}
}

func TestMemoryRepositoryDelete(t *testing.T) {
	t.Parallel()
	r := NewMemoryRepository()
	if err := r.Put(Item{ID: "i1", Name: "apple"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := r.Delete("i1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
	if err := r.Delete("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete err = %v, want ErrNotFound", err)
	}
}

func TestServiceRoundTripThroughInterface(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryRepository())
	if err := svc.AddItem(Item{ID: "i1", Name: "banana", Price: 50}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	got, err := svc.GetItem("i1")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Name != "banana" {
		t.Fatalf("GetItem = %+v, want name banana", got)
	}
	if err := svc.RemoveItem("i1"); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}
	if _, err := svc.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem after remove err = %v, want ErrNotFound", err)
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
			id := fmt.Sprintf("k%d", i)
			_ = r.Put(Item{ID: id, Price: int64(i)})
			_, _ = r.Get(id)
		}()
	}
	wg.Wait()
}

func Example() {
	svc := NewService(NewMemoryRepository())
	_ = svc.AddItem(Item{ID: "sku-1", Name: "widget", Price: 1299})
	item, _ := svc.GetItem("sku-1")
	fmt.Printf("%s %d\n", item.Name, item.Price)
	// Output: widget 1299
}
```

## Review

The artifact is correct when `NewService` accepts `Repository` and returns
`*Service`, when `MemoryRepository` guards its map with the `RWMutex` (proven by the
clean `-race` run), and when a missing key yields `ErrNotFound` that the tests match
with `errors.Is` rather than string comparison. The compile-time assertion
`var _ Repository = (*MemoryRepository)(nil)` means a drift between interface and
implementation is a build error, not a latent runtime bug. The mistakes to avoid are
the two inversions of the rule: returning `Repository` from the constructor (which
would force callers to type-assert for concrete methods) and accepting
`*MemoryRepository` in `NewService` (which would make the service untestable without
the real store). Everything the later modules do — fakes, caches, retries, metrics —
depends on this constructor accepting the interface.

## Resources

- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — accept interfaces where you use a value, return concrete types.
- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces) — how structural satisfaction shapes Go's interface design.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read/write locking semantics used by `MemoryRepository`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-inject-mock-repository.md](02-inject-mock-repository.md)
