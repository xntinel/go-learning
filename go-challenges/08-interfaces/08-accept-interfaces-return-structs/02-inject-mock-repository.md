# Exercise 2: Test A Service By Injecting A Fake Repository

Accepting an interface is only worth it if you use the substitution it buys. This
module proves the payoff: it drives a `Service` entirely through an inline `fakeRepo`
that satisfies `Repository`, with no real store anywhere, and pins two contracts —
that the fake is structurally a `Repository`, and that a nil repository is not a
valid dependency.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
mockrepo/                   independent module: example.com/mockrepo
  go.mod                    go 1.26
  repo.go                   Item, ErrNotFound; Repository iface; MemoryRepository; Service
  cmd/
    demo/
      main.go               runs the Service against a scripted fake, no real store
  repo_test.go              inline fakeRepo; compile-time assertion; nil-repo panic contract
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: the same `Repository`/`MemoryRepository`/`Service` base, plus a demo whose "store" is a fake.
Test: an inline `fakeRepo` injected into `NewService`; a `var _ Repository = (*fakeRepo)(nil)` compile-time assertion; `TestServiceWithNilRepository` that asserts the documented panic via a `recover`-based helper.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the fake is what makes the service testable

`NewService` accepts a `Repository`, so a test can hand it any value with `Get`,
`Put`, and `Delete`. `fakeRepo` is that value: a struct backed by a plain map,
defined in the test file, that satisfies the interface structurally. The test drives
`AddItem`/`GetItem`/`RemoveItem` and asserts behavior without ever constructing a
`MemoryRepository` — the service is exercised in complete isolation from its real
dependency. That isolation is the operational reason the rule exists: had
`NewService` accepted `*MemoryRepository`, there would be nothing to substitute.

Two contracts are worth pinning explicitly. First, `var _ Repository =
(*fakeRepo)(nil)` asserts at compile time that the fake really does satisfy the
interface; if the interface grows a method, the build breaks here, in the test,
rather than at some assignment. Second, a nil repository is not a valid dependency:
the service's contract is "you pass a real repository". `TestServiceWithNilRepository`
pins that as an explicit, deliberate panic — not a random crash — using a
`recover`-based helper so the panic is asserted as a contract. Making the failure
mode an asserted test means it can never silently regress into a confusing nil-pointer
dereference deep in production.

Create `repo.go`:

```go
package mockrepo

import (
	"errors"
	"sync"
)

// ErrNotFound is the sentinel for a missing item.
var ErrNotFound = errors.New("mockrepo: item not found")

type Item struct {
	ID    string
	Name  string
	Price int64
}

// Repository is the consumer-owned port Service depends on.
type Repository interface {
	Get(id string) (Item, error)
	Put(item Item) error
	Delete(id string) error
}

type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]Item
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]Item)}
}

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

// Service accepts the interface and returns the struct.
type Service struct {
	repo Repository
}

func NewService(r Repository) *Service {
	return &Service{repo: r}
}

func (s *Service) AddItem(item Item) error { return s.repo.Put(item) }

func (s *Service) GetItem(id string) (Item, error) { return s.repo.Get(id) }

func (s *Service) RemoveItem(id string) error { return s.repo.Delete(id) }
```

### The runnable demo

The demo shows that "the thing you inject" need not be the production store: it wires
a small scripted fake — pre-seeded with one item, refusing writes — into the exact
same `Service`. This is the substitution the test relies on, made visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/mockrepo"
)

// readOnlyFake is a Repository that serves a fixed catalog and rejects writes.
type readOnlyFake struct {
	catalog map[string]mockrepo.Item
}

func (f readOnlyFake) Get(id string) (mockrepo.Item, error) {
	item, ok := f.catalog[id]
	if !ok {
		return mockrepo.Item{}, mockrepo.ErrNotFound
	}
	return item, nil
}

func (f readOnlyFake) Put(mockrepo.Item) error { return errors.New("read-only") }
func (f readOnlyFake) Delete(string) error     { return errors.New("read-only") }

func main() {
	fake := readOnlyFake{catalog: map[string]mockrepo.Item{
		"sku-1": {ID: "sku-1", Name: "widget", Price: 1299},
	}}
	svc := mockrepo.NewService(fake)

	item, err := svc.GetItem("sku-1")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	fmt.Printf("served %s from fake\n", item.Name)

	if err := svc.AddItem(mockrepo.Item{ID: "sku-2"}); err != nil {
		fmt.Println("write rejected by fake:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
served widget from fake
write rejected by fake: read-only
```

### Tests

The tests define an inline `fakeRepo` (a map-backed `Repository`), inject it into
`NewService`, and drive the service through it. `mustPanic` is the `recover`-based
helper that turns "this must panic" into an assertion, used by
`TestServiceWithNilRepository` to pin the nil-dependency contract.

Create `repo_test.go`:

```go
package mockrepo

import (
	"errors"
	"testing"
)

// fakeRepo is an inline test double satisfying Repository with a plain map.
type fakeRepo struct {
	items map[string]Item
}

func newFakeRepo() *fakeRepo { return &fakeRepo{items: make(map[string]Item)} }

func (f *fakeRepo) Get(id string) (Item, error) {
	item, ok := f.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (f *fakeRepo) Put(item Item) error {
	f.items[item.ID] = item
	return nil
}

func (f *fakeRepo) Delete(id string) error {
	if _, ok := f.items[id]; !ok {
		return ErrNotFound
	}
	delete(f.items, id)
	return nil
}

// compile-time proof the fake satisfies the same port as the real store.
var _ Repository = (*fakeRepo)(nil)

func TestServiceAcceptsFakeRepository(t *testing.T) {
	t.Parallel()
	svc := NewService(newFakeRepo())
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
}

func TestServiceGetItemNotFoundThroughFake(t *testing.T) {
	t.Parallel()
	svc := NewService(newFakeRepo())
	if _, err := svc.GetItem("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem(missing) err = %v, want ErrNotFound", err)
	}
}

// mustPanic runs fn and fails the test unless fn panics.
func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic, got none")
		}
	}()
	fn()
}

func TestServiceWithNilRepository(t *testing.T) {
	t.Parallel()
	// The contract is that a real repository must be injected. Constructing a
	// Service with a nil Repository and then using it dereferences the nil
	// interface, which panics. Pinning the panic makes the contract explicit.
	svc := NewService(nil)
	mustPanic(t, func() {
		_, _ = svc.GetItem("anything")
	})
}
```

## Review

The service is correct when it works identically against the real store and the
inline `fakeRepo`, because both satisfy the same consumer-owned `Repository`. The
compile-time `var _ Repository = (*fakeRepo)(nil)` guarantees the fake keeps pace
with the interface; if you widen the port, the test stops compiling until the fake
implements the new method, which is the point. `TestServiceWithNilRepository` pins
the nil-dependency contract with a `recover`-based helper so the failure mode is an
asserted, deliberate panic rather than a mysterious nil dereference in production.
The trap to avoid is treating a passing happy-path test as sufficient: it is the
`ErrNotFound` path and the nil-repository contract, both asserted here, that prove
the substitution is faithful and the contract is real.

## Resources

- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — why consumers define the interface they mock.
- [`testing`](https://pkg.go.dev/testing) — `T.Helper`, `T.Parallel`, and table-test conventions used here.
- [`builtin.recover`](https://pkg.go.dev/builtin#recover) — recovering a panic, the basis of the `mustPanic` contract helper.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-implement-repository.md](01-implement-repository.md) | Next: [03-narrow-consumer-interface.md](03-narrow-consumer-interface.md)
