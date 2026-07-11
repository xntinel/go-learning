# Exercise 4: Compile-Time Interface Guards to Catch Contract Drift

When an interface or a method signature changes, you want the failure at build
time on the implementation, not at 3am on a route that suddenly serves a nil
handler. A one-line guard `var _ Store = (*PostgresStore)(nil)` forces the
compiler to prove satisfaction where the type is defined. This module puts guards
on a real and a fake repository, then walks through what happens when the
interface grows a `Ping` method — the guard catches the drift immediately.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
repoguard/                    independent module: example.com/repoguard
  go.mod                      go 1.26
  store.go                    Store interface (Get/Put/Ping); guards for both impls
  postgres.go                 *PostgresStore (in-memory stand-in) satisfying Store
  memory.go                   *InMemoryStore fake satisfying Store
  cmd/
    demo/
      main.go                 runnable demo: drive both impls through Store
  store_test.go               both impls behind Store; Ping; ErrNotFound
```

- Files: `store.go`, `postgres.go`, `memory.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` interface (`Get`, `Put`, `Ping`), a `*PostgresStore` (in-memory stand-in), and a `*InMemoryStore` fake, each with a compile-time guard.
- Test: construct both behind the `Store` interface and exercise `Get`/`Put`/`Ping`; assert `ErrNotFound`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repoguard/cmd/demo
cd ~/go-exercises/repoguard
go mod init example.com/repoguard
```

### What a guard buys you, and the drift it catches

`var _ Store = (*PostgresStore)(nil)` reads as: assign a nil `*PostgresStore` to a
throwaway `Store` variable. The assignment compiles only if `*PostgresStore`'s
method set is a superset of `Store`'s. The `_` blank identifier discards the value
so nothing is stored, and `(*PostgresStore)(nil)` is a typed nil that allocates
nothing — the guard is pure compile-time machinery with zero runtime cost. Put one
next to every implementation of an interface.

The payoff is drift detection. Suppose the service adds a health check and `Store`
grows a third method:

```go
type Store interface {
	Get(ctx context.Context, key string) (string, error)
	Put(ctx context.Context, key, value string) error
	Ping(ctx context.Context) error // newly added
}
```

Without guards, the day you add `Ping` to `Store`, `*PostgresStore` might have it
but the fake `*InMemoryStore` might not — and nothing fails until some test or
`main` tries to assign the fake to a `Store` at a distant call site, or worse, a
factory returns it and a route handler is nil. With a guard on each
implementation, the build breaks *at the fake's file*:

```text
memory.go:9:5: cannot use (*InMemoryStore)(nil) (value of type *InMemoryStore)
	as Store value in variable declaration: *InMemoryStore does not implement
	Store (missing method Ping)
```

That is exactly where you want the error: pinned to the type that fell behind the
contract, with a message naming the missing method. The buildable version below
has `Ping` on both implementations so it compiles; the point of the exercise is
that the guards are what would have caught the fake before it shipped.

Create `store.go`:

```go
package repoguard

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("key not found")

// Store is the consumer-defined repository interface. It gained Ping when the
// service added a health check; the guards below prove every implementation
// kept up with that change.
type Store interface {
	Get(ctx context.Context, key string) (string, error)
	Put(ctx context.Context, key, value string) error
	Ping(ctx context.Context) error
}

// Compile-time guards: the build fails here if either implementation drifts
// from the Store contract (e.g. a signature change or a missing new method).
var (
	_ Store = (*PostgresStore)(nil)
	_ Store = (*InMemoryStore)(nil)
)
```

Create `postgres.go` — the "real" implementation (in-memory stand-in so it builds
offline; in production these methods issue SQL):

```go
package repoguard

import (
	"context"
	"sync"
)

// PostgresStore stands in for a database-backed Store. Its map is a substitute
// for a real connection pool so the module builds without a database.
type PostgresStore struct {
	mu   sync.RWMutex
	rows map[string]string
}

// NewPostgresStore returns a ready *PostgresStore.
func NewPostgresStore() *PostgresStore {
	return &PostgresStore{rows: make(map[string]string)}
}

func (p *PostgresStore) Get(_ context.Context, key string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.rows[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (p *PostgresStore) Put(_ context.Context, key, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows[key] = value
	return nil
}

// Ping would verify the connection; the stand-in always reports healthy.
func (p *PostgresStore) Ping(_ context.Context) error {
	return nil
}
```

Create `memory.go` — the fake used in tests:

```go
package repoguard

import (
	"context"
	"sync"
)

// InMemoryStore is the fake used by tests. Because it is guarded by
// var _ Store = (*InMemoryStore)(nil), it can never silently fall behind the
// Store contract: forgetting to add Ping here would fail the build.
type InMemoryStore struct {
	mu   sync.RWMutex
	rows map[string]string
}

// NewInMemoryStore returns a ready *InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{rows: make(map[string]string)}
}

func (m *InMemoryStore) Get(_ context.Context, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.rows[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *InMemoryStore) Put(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[key] = value
	return nil
}

func (m *InMemoryStore) Ping(_ context.Context) error {
	return nil
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

	"example.com/repoguard"
)

func main() {
	ctx := context.Background()

	// Both implementations are used through the same Store interface.
	stores := map[string]repoguard.Store{
		"postgres": repoguard.NewPostgresStore(),
		"memory":   repoguard.NewInMemoryStore(),
	}

	for _, name := range []string{"memory", "postgres"} {
		s := stores[name]
		_ = s.Put(ctx, "region", "eu-west-1")
		v, _ := s.Get(ctx, "region")
		_, missErr := s.Get(ctx, "absent")
		fmt.Printf("%s: region=%s ping=%v missing=%v\n",
			name, v, s.Ping(ctx), errors.Is(missErr, repoguard.ErrNotFound))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
memory: region=eu-west-1 ping=<nil> missing=true
postgres: region=eu-west-1 ping=<nil> missing=true
```

### Tests

The test drives both implementations through the `Store` interface with one shared
helper, which itself is proof that both satisfy the contract, and asserts the
round-trip, `Ping`, and `ErrNotFound`.

Create `store_test.go`:

```go
package repoguard

import (
	"context"
	"errors"
	"testing"
)

func TestStoreImplementations(t *testing.T) {
	t.Parallel()

	impls := map[string]func() Store{
		"postgres": func() Store { return NewPostgresStore() },
		"memory":   func() Store { return NewInMemoryStore() },
	}

	for name, mk := range impls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := mk()

			if err := s.Put(ctx, "k", "v"); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(ctx, "k")
			if err != nil {
				t.Fatal(err)
			}
			if got != "v" {
				t.Fatalf("Get = %q, want v", got)
			}
			if err := s.Ping(ctx); err != nil {
				t.Fatalf("Ping = %v, want nil", err)
			}
			if _, err := s.Get(ctx, "absent"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(absent) err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestGuardsHold(t *testing.T) {
	t.Parallel()

	// The package-level guards already prove satisfaction at build time; these
	// mirror them at the value level for documentation.
	var _ Store = (*PostgresStore)(nil)
	var _ Store = (*InMemoryStore)(nil)
}
```

## Review

The design is correct when every implementation of `Store` carries a
`var _ Store = (*T)(nil)` guard, so a contract change fails the build at the type
that fell behind rather than surfacing later. The mental model: the guard turns a
runtime or integration-time surprise into a compile error with a precise message
(`missing method Ping`). The common mistake is to skip the guard because "the code
obviously implements it" — until someone renames a method or changes a signature
and the type silently stops satisfying the interface, discovered only when a
factory returns it and a handler is nil. Guards cost one line and pay for
themselves the first time a contract moves. Run `go build ./...` (the primary gate
here) and `go test -race` to confirm both implementations behave identically
through the interface.

## Resources

- [Effective Go: Interface checks](https://go.dev/doc/effective_go#blank_implements) — the `var _ Iface = (*T)(nil)` idiom.
- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types) — satisfaction and method sets.
- [`context.Context`](https://pkg.go.dev/context) — the first argument of repository methods.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-typed-nil-error-trap.md](05-typed-nil-error-trap.md)
