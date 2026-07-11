# 10. Generic Repository Pattern

A generic repository removes repeated CRUD boilerplate while keeping each entity type explicit. This lesson builds a thread-safe in-memory repository for values that expose a stable string ID.

## Concepts

### The Constraint Captures The Minimum Entity Contract

The repository only needs an ID, so the constraint is `interface { ID() string }`. It does not require database methods, JSON methods, or a shared base struct.

### Generic Interfaces Describe Typed Behavior

`Repository[T Entity]` returns `T`, not `any`. A `Repository[User]` cannot accidentally return a `Product`, and callers do not need type assertions.

### Concurrency Is Still A Runtime Concern

Generics do not make maps safe for concurrent access. The in-memory implementation protects its map with `sync.RWMutex` and returns snapshots for listing.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/repository/cmd/demo
cd ~/go-exercises/repository
go mod init example.com/verify
```

### Exercise 1: Build The Repository

Create `repository.go`:

```go
package repository

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrEmptyID  = errors.New("id must not be empty")
	ErrConflict = errors.New("entity already exists")
	ErrNotFound = errors.New("entity not found")
)

type Entity interface {
	ID() string
}

type Repository[T Entity] interface {
	Create(T) error
	Get(string) (T, error)
	Delete(string) error
	List() []T
}

type Memory[T Entity] struct {
	mu    sync.RWMutex
	items map[string]T
}

func NewMemory[T Entity]() *Memory[T] {
	return &Memory[T]{items: make(map[string]T)}
}

func (m *Memory[T]) Create(entity T) error {
	id := entity.ID()
	if id == "" {
		return fmt.Errorf("create: %w", ErrEmptyID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; ok {
		return fmt.Errorf("create %q: %w", id, ErrConflict)
	}
	m.items[id] = entity
	return nil
}

func (m *Memory[T]) Get(id string) (T, error) {
	var zero T
	if id == "" {
		return zero, fmt.Errorf("get: %w", ErrEmptyID)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entity, ok := m.items[id]
	if !ok {
		return zero, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return entity, nil
}

func (m *Memory[T]) Delete(id string) error {
	if id == "" {
		return fmt.Errorf("delete: %w", ErrEmptyID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return fmt.Errorf("delete %q: %w", id, ErrNotFound)
	}
	delete(m.items, id)
	return nil
}

func (m *Memory[T]) List() []T {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]T, 0, len(m.items))
	for _, entity := range m.items {
		out = append(out, entity)
	}
	return out
}
```

### Exercise 2: Add An Entity, Tests, And An Example

Create `repository_test.go`:

```go
package repository

import (
	"errors"
	"fmt"
	"testing"
)

type User struct {
	UserID string
	Name   string
}

func (u User) ID() string { return u.UserID }

func TestMemoryCreateGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user User
		err  error
	}{
		{name: "valid", user: User{UserID: "u1", Name: "Alice"}},
		{name: "empty id", user: User{Name: "Alice"}, err: ErrEmptyID},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := NewMemory[User]()
			err := repo.Create(tt.user)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Create() err = %v, want %v", err, tt.err)
			}
			if tt.err == nil {
				got, err := repo.Get(tt.user.ID())
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.user {
					t.Fatalf("Get() = %#v, want %#v", got, tt.user)
				}
			}
		})
	}
}

func TestMemoryErrors(t *testing.T) {
	t.Parallel()

	repo := NewMemory[User]()
	if err := repo.Create(User{UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(User{UserID: "u1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := repo.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func ExampleMemory_Get() {
	repo := NewMemory[User]()
	_ = repo.Create(User{UserID: "u1", Name: "Alice"})
	user, _ := repo.Get("u1")
	fmt.Println(user.Name)
	// Output: Alice
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	repository "example.com/verify"
)

type Product struct {
	SKU  string
	Name string
}

func (p Product) ID() string { return p.SKU }

func main() {
	repo := repository.NewMemory[Product]()
	if err := repo.Create(Product{SKU: "p1", Name: "Keyboard"}); err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(repo.List()))
}
```

## Common Mistakes

### Storing `any`

Wrong: one repository stores `map[string]any` and callers assert the result.

Fix: store `map[string]T` so the compiler preserves the entity type.

### Returning The Internal Map

Wrong: expose `items` or return it directly.

Fix: return a slice snapshot from `List` while holding a read lock.

### Ignoring Empty IDs

Wrong: allowing all empty-ID entities to overwrite the same map entry.

Fix: reject empty IDs with `ErrEmptyID`.

## Verification

Run this from `~/go-exercises/repository`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that deletes an existing user and then verifies `Get` returns `ErrNotFound`.

## Summary

- Generic repositories preserve the concrete entity type.
- The entity constraint should state only the required contract.
- Maps still need synchronization for concurrent access.
- CRUD error contracts should use wrapped sentinel errors.

## What's Next

Next: [Generics vs Interfaces](../11-generics-vs-interfaces/11-generics-vs-interfaces.md).

## Resources

- [sync package](https://pkg.go.dev/sync)
- [Go Blog: When To Use Generics](https://go.dev/blog/when-generics)
- [Go Specification: Method declarations](https://go.dev/ref/spec#Method_declarations)
