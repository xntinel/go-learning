# Exercise 2: Wiring a Repository into a Store Interface (Pointer-Only Method Set)

The moment you assemble dependencies — hand a concrete repository to a service
that wants a `Store` interface — the method-set rule stops being theory and
becomes a compile error. This module reproduces the exact
"method Save has pointer receiver" failure a senior engineer hits at DI wiring,
then fixes it the disciplined way with a compile-time assertion beside the type.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
userstore/                     independent module: example.com/userstore
  go.mod                       module path + go directive
  store.go                     Store interface; MemStore with pointer methods; compile-time guard
  cmd/
    demo/
      main.go                  wire *MemStore behind Store, round-trip a user
  store_test.go                round-trip test through the interface; documented compile-failure
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` interface (`Save`, `Get`) whose methods are implemented on `*MemStore`, plus a compile-time guard `var _ Store = (*MemStore)(nil)`.
- Test: construct the store, hold it behind `Store`, round-trip a record; a documented commented-out line quoting the exact compiler error for the value form.
- Verify: `go build ./...`, `go vet ./...`, `go test -count=1 -race ./...`.

### Why *MemStore satisfies Store but MemStore does not

`MemStore` guards an in-memory map with a mutex and mutates it in `Save`, so both
methods take a pointer receiver: `Save` must mutate the shared map, and once one
method is a pointer method, consistency says they all are. That puts `Save` and
`Get` in the method set of `*MemStore` only. The `Store` interface requires both
methods, so its method-set superset test passes for `*MemStore` and fails for
`MemStore` — a plain `MemStore` value is missing `Save` and `Get` from its method
set entirely.

If you only find this out when you write `var s Store = MemStore{}` deep in your
`main` wiring, the compiler tells you exactly what happened:

```text
cannot use MemStore{} (value of type MemStore) as Store value in variable
declaration: MemStore does not implement Store (method Get has pointer receiver)
```

The professional fix is not to memorize that message but to make the mismatch
impossible to introduce silently. A single line beside the type,
`var _ Store = (*MemStore)(nil)`, asserts at compile time that `*MemStore`
satisfies `Store`. It costs nothing at runtime (the blank identifier discards a
typed nil), and if someone later changes a receiver or a signature so the
interface no longer fits, the build breaks right there at the type definition —
not three packages away at the injection site.

The methods take a `context.Context` first parameter, as real repository methods
do, so cancellation and deadlines propagate; the in-memory implementation honors
`ctx.Err()` before touching its map.

Create `store.go`:

```go
package userstore

import (
	"context"
	"errors"
	"sync"
)

// ErrNotFound is returned by Get when no user has the requested id.
var ErrNotFound = errors.New("userstore: user not found")

// User is the stored record.
type User struct {
	ID   string
	Name string
}

// Store is the interface a service depends on. Both methods are required, so a
// concrete type satisfies Store only if both are in its method set.
type Store interface {
	Save(ctx context.Context, u User) error
	Get(ctx context.Context, id string) (User, error)
}

// MemStore is an in-memory Store. It carries a mutex and mutates a shared map,
// so its methods take pointer receivers and live in the method set of *MemStore
// only. A plain MemStore value does NOT satisfy Store.
type MemStore struct {
	mu    sync.RWMutex
	users map[string]User
}

// NewMemStore returns a ready *MemStore.
func NewMemStore() *MemStore {
	return &MemStore{users: make(map[string]User)}
}

// Save stores u, honoring context cancellation.
func (s *MemStore) Save(ctx context.Context, u User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

// Get returns the user or wraps ErrNotFound.
func (s *MemStore) Get(ctx context.Context, id string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

// Compile-time guard: *MemStore must satisfy Store. If a receiver or signature
// drifts, this line fails the build at the type definition, not at DI wiring.
var _ Store = (*MemStore)(nil)

// The value form does NOT compile. Uncommenting it fails with:
//   cannot use MemStore{} (value of type MemStore) as Store value in variable
//   declaration: MemStore does not implement Store (method Get has pointer receiver)
// var _ Store = MemStore{}
```

### The runnable demo

The demo wires a `*MemStore` behind the `Store` interface — the exact shape of
dependency injection — and round-trips a user through it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/userstore"
)

// service depends on the interface, not the concrete type.
func service(s userstore.Store) error {
	ctx := context.Background()
	if err := s.Save(ctx, userstore.User{ID: "u1", Name: "alice"}); err != nil {
		return err
	}
	u, err := s.Get(ctx, "u1")
	if err != nil {
		return err
	}
	fmt.Printf("loaded: %s -> %s\n", u.ID, u.Name)

	_, err = s.Get(ctx, "missing")
	if errors.Is(err, userstore.ErrNotFound) {
		fmt.Println("missing: not found")
	}
	return nil
}

func main() {
	// *MemStore satisfies Store; a MemStore value would not compile here.
	if err := service(userstore.NewMemStore()); err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loaded: u1 -> alice
missing: not found
```

### Tests

The test constructs the store, stores it behind the `Store` interface (the DI
move), round-trips a record, and asserts the not-found path with `errors.Is`.
The value-form failure is documented as a commented line quoting the compiler
message, so the suite stays green while the trap stays visible.

Create `store_test.go`:

```go
package userstore

import (
	"context"
	"errors"
	"testing"
)

// take accepts the interface, mirroring a service constructor at DI time.
func take(s Store) Store { return s }

func TestRoundTripThroughInterface(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// *MemStore satisfies Store; assign it through the interface parameter.
	s := take(NewMemStore())

	if err := s.Save(ctx, User{ID: "u1", Name: "alice"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	u, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.Name != "alice" {
		t.Fatalf("Get name = %q, want alice", u.Name)
	}

	// The value form would fail to compile:
	//   _ = take(MemStore{})
	// cannot use MemStore{} (value of type MemStore) as Store value in argument
	// to take: MemStore does not implement Store (method Get has pointer receiver)
}

func TestGetMissingIsNotFound(t *testing.T) {
	t.Parallel()
	s := NewMemStore()
	_, err := s.Get(t.Context(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestCanceledContextStopsSave(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := NewMemStore()
	if err := s.Save(ctx, User{ID: "u1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save on canceled ctx = %v, want context.Canceled", err)
	}
}
```

## Review

The store is correct when `*MemStore` — and only `*MemStore` — satisfies `Store`.
The compile-time guard `var _ Store = (*MemStore)(nil)` is the load-bearing line:
it converts a wiring-time surprise into a build-time error at the type, so a
receiver change can never quietly break the interface fit. `take(NewMemStore())`
in the test exercises the real DI path, passing the concrete pointer where an
interface is expected.

The mistake this module exists to prevent is assuming a value type satisfies an
interface whose methods are on the pointer. It does not, and the compiler says
so with "method has pointer receiver" — but only at the site where you widen the
value to the interface, which may be far from the type. Keep the guard beside the
type and pass `*MemStore`. Run `go build` and `go vet`; both must be clean.

## Resources

- [Go Language Specification: Method sets](https://go.dev/ref/spec#Method_sets) — the superset test for interface satisfaction.
- [Go Language Specification: Interface types](https://go.dev/ref/spec#Interface_types) — how implementation is determined structurally.
- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces_and_types) — idiomatic interface satisfaction by pointer types.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-metrics-registry-map-value-addressability.md](03-metrics-registry-map-value-addressability.md)
