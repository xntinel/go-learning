# Exercise 4: Repository Pattern — Hide The Persistence Driver In internal/store

The point of the repository pattern is that callers depend on a domain interface,
never on `database/sql` or a specific driver. `internal` is how you enforce that:
the port (interface) lives in the public package, and the adapter (the concrete,
driver-backed store) lives under `internal/store` where no downstream module can
import it. Consumers get the interface; the persistence details never appear on the
public API.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
userstore/                        module example.com/userstore
  go.mod
  userstore.go                    port: UserRepo interface; User; NewInMemory; re-exported sentinels
  userstore_test.go               tests the public constructor + interface only
  internal/store/store.go         adapter: Memory store; Create, Get; ErrNotFound, ErrExists
  internal/store/store_test.go    white-box test of the concrete store
  cmd/demo/main.go                runnable demo using only UserRepo
```

- Files: `internal/store/store.go`, `internal/store/store_test.go`, `userstore.go`, `userstore_test.go`, `cmd/demo/main.go`.
- Implement: a `UserRepo` interface and `User` type in the public package; a concrete `Memory` store under `internal/store`; a `NewInMemory` constructor returning the interface.
- Test: white-box test of the store's insert/get/not-found/duplicate behavior via `errors.Is`; a public test that `NewInMemory` returns a working `UserRepo`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/03-internal-packages/04-internal-postgres-repository/internal/store go-solutions/11-packages-and-modules/03-internal-packages/04-internal-postgres-repository/cmd/demo
cd go-solutions/11-packages-and-modules/03-internal-packages/04-internal-postgres-repository
```

### Why the store goes under internal and the interface does not

Split the module along the hexagonal seam. The public package `userstore` owns the
port: the `UserRepo` interface and the `User` domain type. The concrete adapter —
the thing that actually talks to a database — lives under `internal/store`, so a
downstream service can import `userstore` and program against `UserRepo`, but can
never import `internal/store` and reach for the driver. That is what lets you swap a
Postgres implementation for an in-memory one, or change the SQL, without touching a
single consumer: they only ever held the interface.

The critical discipline is that no persistence type may leak onto the public API.
`NewInMemory` returns `UserRepo`, not `*store.Memory`; in a real build,
`NewPostgres(db *sql.DB)` would also return `UserRepo`, and the `*sql.DB` would be a
parameter, never a return value or a field on an exported struct. If a `*sql.DB` or
a driver-specific row type appeared on an exported signature, consumers would be
coupled to the driver and the whole reason to hide the store would be defeated.

To keep this exercise hermetic, the concrete store is an in-memory map rather than a
real Postgres connection — but it implements the same `UserRepo` interface a
SQL-backed store would. A real `internal/store` adapter has the same shape; only the
method bodies differ:

```go
// Illustrative only (needs a driver and a live DB; not built here).
type Postgres struct{ db *sql.DB } // *sql.DB stays unexported, inside internal

func NewPostgres(db *sql.DB) *Postgres { return &Postgres{db: db} }

func (p *Postgres) Get(ctx context.Context, id string) (User, error) {
	var u User
	err := p.db.QueryRowContext(ctx, "SELECT id, email FROM users WHERE id=$1", id).
		Scan(&u.ID, &u.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return u, err
}
```

Create `internal/store/store.go`. The concrete store, its domain `User`, and its
sentinels live here; the sentinels are wrapped with `%w` so callers match them with
`errors.Is`:

```go
// Package store is the persistence adapter. It is internal, so only
// example.com/userstore may import it; the driver never escapes this package.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Sentinels for repository outcomes; callers match them with errors.Is.
var (
	ErrNotFound = errors.New("store: user not found")
	ErrExists   = errors.New("store: user already exists")
)

// User is the domain record persisted by the store.
type User struct {
	ID    string
	Email string
}

// Memory is a concurrency-safe in-memory UserRepo implementation. A real
// adapter would hold a *sql.DB here; the shape of the methods is identical.
type Memory struct {
	mu    sync.RWMutex
	users map[string]User
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{users: make(map[string]User)}
}

// Create inserts u, returning ErrExists (wrapped) if the ID is already present.
func (m *Memory) Create(ctx context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; ok {
		return fmt.Errorf("create %q: %w", u.ID, ErrExists)
	}
	m.users[u.ID] = u
	return nil
}

// Get returns the user by ID, or ErrNotFound (wrapped) if absent.
func (m *Memory) Get(ctx context.Context, id string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return u, nil
}
```

Create `userstore.go`. The public package defines the port and a constructor that
returns the interface; `User` is a type alias of the store's record and the
sentinels are re-exported, so nothing forces a consumer to import `internal/store`:

```go
// Package userstore is the public API: a UserRepo port whose adapters are hidden
// under internal/store. Consumers depend on the interface, never the driver.
package userstore

import (
	"context"

	"example.com/userstore/internal/store"
)

// User is the domain record, aliased from the store so the two never diverge.
type User = store.User

// Re-exported sentinels so callers match errors without importing internal/store.
var (
	ErrNotFound = store.ErrNotFound
	ErrExists   = store.ErrExists
)

// UserRepo is the port. Callers program against this, not a concrete store.
type UserRepo interface {
	Create(ctx context.Context, u User) error
	Get(ctx context.Context, id string) (User, error)
}

// NewInMemory returns an in-memory UserRepo. The concrete type stays hidden;
// callers only ever see the interface.
func NewInMemory() UserRepo {
	return store.NewMemory()
}
```

### The runnable demo

The demo uses only `UserRepo` and the re-exported sentinels — the consumer's path.
It never names `store`, because it could not import it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/userstore"
)

func main() {
	ctx := context.Background()
	repo := userstore.NewInMemory()

	_ = repo.Create(ctx, userstore.User{ID: "u1", Email: "alice@example.com"})

	u, _ := repo.Get(ctx, "u1")
	fmt.Println("got:", u.ID, u.Email)

	_, err := repo.Get(ctx, "missing")
	fmt.Println("missing is ErrNotFound:", errors.Is(err, userstore.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got: u1 alice@example.com
missing is ErrNotFound: true
```

### Tests

`internal/store/store_test.go` is a white-box test of the concrete adapter — insert,
read, not-found, and duplicate — matching sentinels with `errors.Is`. `userstore_test.go`
tests only the public constructor and interface, proving the consumer path works and
that the re-exported sentinels match.

Create `internal/store/store_test.go`:

```go
package store

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryCreateGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemory()

	if err := m.Create(ctx, User{ID: "u1", Email: "a@example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := m.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != "a@example.com" {
		t.Fatalf("Get email = %q, want a@example.com", got.Email)
	}
}

func TestMemoryErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemory()

	if _, err := m.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}

	_ = m.Create(ctx, User{ID: "dup"})
	if err := m.Create(ctx, User{ID: "dup"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
}
```

Create `userstore_test.go`:

```go
package userstore

import (
	"context"
	"errors"
	"testing"
)

func TestNewInMemoryReturnsWorkingRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var repo UserRepo = NewInMemory()

	if err := repo.Create(ctx, User{ID: "u1", Email: "z@example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u, err := repo.Get(ctx, "u1"); err != nil || u.Email != "z@example.com" {
		t.Fatalf("Get = %+v, %v; want email z@example.com", u, err)
	}
	if _, err := repo.Get(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound via re-export", err)
	}
}
```

## Review

The design is correct when the public API mentions only `UserRepo`, `User`, and the
sentinels — no `*sql.DB`, no `*store.Memory`, no driver row type anywhere on an
exported signature. `NewInMemory` returns the interface, so a consumer holds a port
and the adapter stays swappable. The white-box store test proves the adapter's
behavior directly; the public test proves the seam.

The mistake that quietly undoes all of this is leaking a persistence type through
the boundary: returning `*sql.DB`, exposing a `Rows`, or making `NewInMemory` return
the concrete `*store.Memory`. Any of those recouples consumers to the very details
you hid the store to protect. Keep the driver and its types entirely inside
`internal/store`, hand out the interface, and you can rewrite the adapter — or add a
Postgres one — without a breaking change.

## Resources

- [Go Modules Reference: Internal packages](https://go.dev/ref/mod#internal-packages) — hiding the adapter from downstream modules.
- [`database/sql`](https://pkg.go.dev/database/sql) — `*sql.DB`, `QueryRowContext`, `sql.ErrNoRows` (the types you keep off the public API).
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces) — defining the port as an interface.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-module-root-internal-api-surface.md](03-module-root-internal-api-surface.md) | Next: [05-internal-config-loader.md](05-internal-config-loader.md)
