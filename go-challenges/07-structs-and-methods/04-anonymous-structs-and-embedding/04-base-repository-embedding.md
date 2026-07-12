# Exercise 4: A Base Repository Embedded into Concrete Repositories

Every data-access layer accumulates the same handful of helpers on each
repository: does a row with this id exist, how many rows are there, run this in a
transaction. Copying those onto `UserRepo`, `OrderRepo`, `InvoiceRepo`, ... is the
classic duplication that a *base repository* removes. This exercise builds a
`BaseRepo` holding a shared store handle and common helpers, then embeds it by
value into concrete repositories so those helpers are promoted while each repo
adds its own domain methods.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
repo/                       independent module: example.com/repo
  go.mod                    module example.com/repo
  repo.go                   Store interface; BaseRepo with Exists/Count; UserRepo, OrderRepo
  cmd/
    demo/
      main.go               wire a fake store; call promoted and domain methods
  repo_test.go              promotion reachable, domain methods coexist, BaseRepo explicit
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: a `Store` interface (`Has`, `Count`), a `BaseRepo` embedding a `Store`
and a table name with promoted `Exists(ctx, id)` / `Count(ctx)` helpers, and
`UserRepo` / `OrderRepo` that embed `BaseRepo` by value and add domain methods.
Test: the promoted helper is callable directly on `UserRepo`/`OrderRepo` and
operates on the embedded handle; domain and promoted methods coexist;
`repo.BaseRepo` is still reachable explicitly.
Verify: `go test -count=1 -race ./...`

### Why embed the base by value and what promotion buys

`BaseRepo` holds the two things every repository shares: a `Store` handle (a small
interface standing in for `*sql.DB` or a query runner) and the table name it
operates on. Its methods — `Exists`, `Count` — are written once against that
handle. When `UserRepo` embeds `BaseRepo`, those methods are *promoted*:
`userRepo.Exists(ctx, id)` is shorthand for `userRepo.BaseRepo.Exists(ctx, id)`,
and the promoted method sees the *embedded* `BaseRepo`'s receiver — its store and
its table name — not the outer type. That is the whole mechanism: each concrete
repo constructs its `BaseRepo` with its own table (`"users"`, `"orders"`), and the
shared helper automatically targets the right table because it reads the embedded
receiver's field.

Embedding by *value* is right here because a `BaseRepo` is small (an interface
value plus a string) and each repo owns its own copy — there is nothing shared to
alias. The concrete repos then add domain methods (`UserRepo.EmailTaken`,
`OrderRepo.HasPending`) that themselves *call the promoted helpers*, composing the
shared behavior with domain logic. And because embedding never hides the inner
value, `userRepo.BaseRepo` remains reachable explicitly — useful when a caller
wants the base behavior without the domain wrapper.

Create `repo.go`:

```go
package repo

import "context"

// Store is the minimal data handle a repository needs. In production this is
// backed by *sql.DB or a query runner; here it is an interface so tests can
// supply a fake.
type Store interface {
	Has(ctx context.Context, table, id string) (bool, error)
	Count(ctx context.Context, table string) (int, error)
}

// BaseRepo holds the shared handle and table name, and defines the helpers every
// concrete repository promotes.
type BaseRepo struct {
	table string
	store Store
}

// NewBase constructs a BaseRepo bound to a table and store.
func NewBase(table string, store Store) BaseRepo {
	return BaseRepo{table: table, store: store}
}

// Exists reports whether a row with id exists in this repo's table.
func (b BaseRepo) Exists(ctx context.Context, id string) (bool, error) {
	return b.store.Has(ctx, b.table, id)
}

// Count reports the number of rows in this repo's table.
func (b BaseRepo) Count(ctx context.Context) (int, error) {
	return b.store.Count(ctx, b.table)
}

// UserRepo embeds BaseRepo by value, promoting Exists/Count, and adds domain
// methods over the users table.
type UserRepo struct {
	BaseRepo
}

// NewUserRepo binds a UserRepo to the "users" table.
func NewUserRepo(store Store) *UserRepo {
	return &UserRepo{BaseRepo: NewBase("users", store)}
}

// EmailTaken is a domain method that composes the promoted Exists helper.
func (r *UserRepo) EmailTaken(ctx context.Context, id string) (bool, error) {
	return r.Exists(ctx, id)
}

// OrderRepo embeds BaseRepo by value over the orders table.
type OrderRepo struct {
	BaseRepo
}

// NewOrderRepo binds an OrderRepo to the "orders" table.
func NewOrderRepo(store Store) *OrderRepo {
	return &OrderRepo{BaseRepo: NewBase("orders", store)}
}

// Backlog is a domain method that composes the promoted Count helper.
func (r *OrderRepo) Backlog(ctx context.Context) (int, error) {
	return r.Count(ctx)
}
```

### The runnable demo

The demo wires a small in-memory store, then calls both a promoted helper
(`Exists`) and a domain method (`Backlog`) to show they coexist on the same repo.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/repo"
)

// memStore is a tiny in-memory Store: table -> set of ids.
type memStore struct {
	rows map[string]map[string]bool
}

func (m memStore) Has(_ context.Context, table, id string) (bool, error) {
	return m.rows[table][id], nil
}

func (m memStore) Count(_ context.Context, table string) (int, error) {
	return len(m.rows[table]), nil
}

func main() {
	store := memStore{rows: map[string]map[string]bool{
		"users":  {"u1": true},
		"orders": {"o1": true, "o2": true},
	}}
	ctx := context.Background()

	users := repo.NewUserRepo(store)
	orders := repo.NewOrderRepo(store)

	ok, _ := users.Exists(ctx, "u1") // promoted helper
	fmt.Printf("user u1 exists: %v\n", ok)

	n, _ := orders.Backlog(ctx) // domain method over promoted Count
	fmt.Printf("order backlog: %d\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user u1 exists: true
order backlog: 2
```

### Tests

The fake store lets the tests assert that a promoted helper targets the embedded
receiver's table, that domain and promoted methods coexist, and that the embedded
`BaseRepo` is reachable explicitly.

Create `repo_test.go`:

```go
package repo

import (
	"context"
	"testing"
)

// fakeStore records which (table, id) pairs exist and per-table counts.
type fakeStore struct {
	has    map[string]map[string]bool
	counts map[string]int
}

func (f fakeStore) Has(_ context.Context, table, id string) (bool, error) {
	return f.has[table][id], nil
}

func (f fakeStore) Count(_ context.Context, table string) (int, error) {
	return f.counts[table], nil
}

func newFake() fakeStore {
	return fakeStore{
		has: map[string]map[string]bool{
			"users":  {"u1": true},
			"orders": {"o9": true},
		},
		counts: map[string]int{"users": 1, "orders": 5},
	}
}

func TestPromotedExistsTargetsEmbeddedTable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	users := NewUserRepo(newFake())

	got, err := users.Exists(ctx, "u1") // promoted from BaseRepo
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("Exists(u1) on users repo = false, want true")
	}
	// u1 is a users id, not an orders id: the orders repo must not find it.
	orders := NewOrderRepo(newFake())
	if got, _ := orders.Exists(ctx, "u1"); got {
		t.Fatal("orders repo found a users id: promotion used the wrong table")
	}
}

func TestDomainAndPromotedMethodsCoexist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	orders := NewOrderRepo(newFake())

	n, err := orders.Backlog(ctx) // domain method
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("Backlog = %d, want 5", n)
	}
	if c, _ := orders.Count(ctx); c != n { // promoted method
		t.Fatalf("promoted Count = %d, want %d", c, n)
	}
}

func TestBaseRepoReachableExplicitly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	users := NewUserRepo(newFake())

	// The embedded value is never hidden: reach it by its type name.
	got, err := users.BaseRepo.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("users.BaseRepo.Count = %d, want 1", got)
	}
}
```

## Review

The base-repository pattern is correct when a promoted helper reads the *embedded*
receiver — so each concrete repo's `Exists`/`Count` automatically targets its own
table — and when domain methods sit alongside the promoted ones on the same value.
The test that the orders repo cannot find a users id is the proof promotion bound
to the right receiver. The mistakes to avoid: reaching for embedding to bundle
unrelated collaborators (embed the one shared base, name distinct dependencies);
assuming a promoted method somehow knows about the outer type (it only ever sees
its own embedded receiver); and forgetting that `repo.BaseRepo` stays reachable,
which is the escape hatch when you want the raw base behavior.

## Resources

- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — promotion of fields and methods for composition.
- [Go Specification: Selectors](https://go.dev/ref/spec#Selectors) — how `x.f` resolves through embedded fields.
- [database/sql](https://pkg.go.dev/database/sql) — the real handle a `BaseRepo` wraps in production.

---

Prev: [03-safe-cache-embedded-mutex.md](03-safe-cache-embedded-mutex.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-store-decorator-interface-embed.md](05-store-decorator-interface-embed.md)
