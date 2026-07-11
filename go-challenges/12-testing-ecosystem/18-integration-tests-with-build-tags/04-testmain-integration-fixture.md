# Exercise 4: One-Time Fixture Lifecycle In A Tag-Gated TestMain

A real integration package connects to Postgres once, migrates once, runs the whole
suite, and tears down once — not per test. This module builds a repository behind an
interface with both an in-memory and a `database/sql`-backed implementation, and a
`main_integration_test.go` whose `TestMain` owns that once-per-package lifecycle and
routes the exit through a `run(m)` helper so deferred teardown actually fires.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
pgrepo/                    independent module: example.com/pgrepo
  go.mod
  store.go                 Store interface, Account, wrapped ErrNotFound
  memory.go                MemStore (the hermetic implementation, tested)
  sql.go                   SQLStore over *sql.DB (compiles in the default build)
  cmd/
    demo/
      main.go              exercises MemStore through the Store interface
  store_test.go            default-build tests of MemStore + Example
  main_integration_test.go //go:build integration: TestMain, run(m), once-per-package setup
```

- Files: `store.go`, `memory.go`, `sql.go`, `cmd/demo/main.go`, `store_test.go`, `main_integration_test.go`.
- Implement: a `Store` interface with `MemStore` and `SQLStore`, and a tag-gated `TestMain` that connects+migrates once via a `run(m)` helper.
- Test: MemStore in the default build; `TestSQLStoreRoundTrip` and a once-only setup counter in the integration build.
- Verify: the default build compiles `sql.go` (no driver import) and runs the MemStore tests; the tagged `TestMain` and driver import exist only under `-tags=integration`.

Set up the module:

```bash
mkdir -p ~/go-exercises/pgrepo/cmd/demo
cd ~/go-exercises/pgrepo
go mod init example.com/pgrepo
```

### Why the interface, and why sql.go compiles by default

The repository is defined by an interface so the fast tier can test the not-found
contract and the round-trip against `MemStore`, while the integration tier tests
the same contract against `SQLStore` and a real database. A key structural point:
`sql.go` imports only `database/sql`, which is standard library — it compiles in the
default build with no Postgres driver present. A driver (`pgx`) is a runtime
dependency of `sql.Open`, so it is imported only where the connection is opened: in
the tag-gated `main_integration_test.go`. That is the whole trick — the repository
code is always compiled and vetted, but the driver only enters the build under
`-tags=integration`.

### Why TestMain must route through run(m)

`TestMain` runs once per package and is where the fixture lifecycle belongs:
connect, migrate, run, tear down. The trap is `os.Exit`. `m.Run()` returns the exit
code, and the obvious `os.Exit(m.Run())` seems fine — until you add
`defer db.Close()`. `os.Exit` terminates the process immediately and does *not* run
deferred functions, so the connection (or a testcontainers container) leaks. The fix
is to move the body into a helper that *returns* the code:

```go
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	db := connectAndMigrate()
	defer db.Close() // run returns before os.Exit, so this fires
	return m.Run()
}
```

`run` returns normally, its defers execute, and only then does `TestMain` call
`os.Exit`. A `setupCount` counter proves the setup runs exactly once no matter how
many tests are in the package.

Create `store.go`:

```go
package pgrepo

import (
	"context"
	"errors"
)

// ErrNotFound is returned (wrapped) when an account id is absent.
var ErrNotFound = errors.New("pgrepo: account not found")

// Account is the row the repository stores.
type Account struct {
	ID   string
	Name string
}

// Store is the repository contract. MemStore backs the fast tier; SQLStore backs
// the integration tier against a real database.
type Store interface {
	Put(ctx context.Context, acct Account) error
	Get(ctx context.Context, id string) (Account, error)
}
```

Create `memory.go`:

```go
package pgrepo

import (
	"context"
	"fmt"
	"sync"
)

// MemStore is the hermetic Store implementation used by the fast tier.
type MemStore struct {
	mu   sync.RWMutex
	data map[string]Account
}

func NewMemStore() *MemStore {
	return &MemStore{data: make(map[string]Account)}
}

func (s *MemStore) Put(ctx context.Context, acct Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[acct.ID] = acct
	return nil
}

func (s *MemStore) Get(ctx context.Context, id string) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[id]
	if !ok {
		return Account{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	return a, nil
}

var _ Store = (*MemStore)(nil)
```

Create `sql.go` — note it imports only `database/sql`, no driver:

```go
package pgrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SQLStore is the Store implementation over a real *sql.DB. It compiles in the
// default build because database/sql is standard library; the driver is imported
// only where sql.Open is called, in the tag-gated integration file.
type SQLStore struct {
	db *sql.DB
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) Put(ctx context.Context, acct Account) error {
	const q = `INSERT INTO accounts (id, name) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`
	if _, err := s.db.ExecContext(ctx, q, acct.ID, acct.Name); err != nil {
		return fmt.Errorf("put %q: %w", acct.ID, err)
	}
	return nil
}

func (s *SQLStore) Get(ctx context.Context, id string) (Account, error) {
	const q = `SELECT name FROM accounts WHERE id = $1`
	a := Account{ID: id}
	err := s.db.QueryRowContext(ctx, q, id).Scan(&a.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get %q: %w", id, err)
	}
	return a, nil
}

var _ Store = (*SQLStore)(nil)
```

Now the tag-gated fixture. In the default build this file — and the `pgx` import —
does not exist:

Create `main_integration_test.go`:

```go
//go:build integration

package pgrepo

import (
	"context"
	"database/sql"
	"log"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	testDB     *sql.DB
	setupCount int
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

// run owns the once-per-package fixture. It returns the exit code so its deferred
// teardown fires before TestMain calls os.Exit.
func run(m *testing.M) int {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Println("DATABASE_URL not set; integration package has nothing to run")
		return 0
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("open: %v", err)
		return 1
	}
	defer db.Close() // fires because run returns before os.Exit

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		log.Printf("ping: %v", err)
		return 1
	}
	const ddl = `CREATE TABLE IF NOT EXISTS accounts (id text PRIMARY KEY, name text NOT NULL)`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		log.Printf("migrate: %v", err)
		return 1
	}

	setupCount++
	testDB = db
	return m.Run()
}

func TestSQLStoreRoundTrip(t *testing.T) {
	if setupCount != 1 {
		t.Fatalf("setup ran %d times, want exactly 1", setupCount)
	}
	store := NewSQLStore(testDB)
	acct := Account{ID: "acct:1", Name: "alice"}
	if err := store.Put(t.Context(), acct); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(t.Context(), "acct:1")
	if err != nil || got.Name != "alice" {
		t.Fatalf("Get(acct:1) = %+v, %v; want alice", got, err)
	}
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

	"example.com/pgrepo"
)

func main() {
	ctx := context.Background()
	var store pgrepo.Store = pgrepo.NewMemStore()

	_ = store.Put(ctx, pgrepo.Account{ID: "acct:1", Name: "alice"})
	got, _ := store.Get(ctx, "acct:1")
	fmt.Printf("get acct:1 -> %s\n", got.Name)

	if _, err := store.Get(ctx, "acct:404"); errors.Is(err, pgrepo.ErrNotFound) {
		fmt.Println("get acct:404 -> not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
get acct:1 -> alice
get acct:404 -> not found
```

### Tests

The default-build tests exercise `MemStore` through the `Store` interface. The
once-per-package proof and the `SQLStore` round-trip live in the integration file
above; they run under `-tags=integration` with a DSN.

Create `store_test.go`:

```go
package pgrepo

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMemStoreRoundTrip(t *testing.T) {
	t.Parallel()
	var store Store = NewMemStore()
	if err := store.Put(t.Context(), Account{ID: "acct:1", Name: "alice"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(t.Context(), "acct:1")
	if err != nil || got.Name != "alice" {
		t.Fatalf("Get(acct:1) = %+v, %v; want alice", got, err)
	}
}

func TestMemStoreNotFound(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	_, err := store.Get(t.Context(), "absent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(absent) = %v, want wrapped ErrNotFound", err)
	}
}

func ExampleMemStore() {
	store := NewMemStore()
	_ = store.Put(context.Background(), Account{ID: "a", Name: "amy"})
	got, _ := store.Get(context.Background(), "a")
	fmt.Println(got.Name)
	// Output: amy
}
```

## Review

`TestMain` is the single owner of the integration fixture: connect, migrate, and
seed once, run the suite, tear down once. The defining mistake is
`os.Exit(m.Run())` with a deferred `db.Close()`, because `os.Exit` never runs
defers and the connection leaks; the `run(m) int` helper exists precisely so the
defers fire before the exit. The `setupCount` counter proves setup happened exactly
once regardless of how many tests the package holds. Note that the entire heavyweight
lifecycle — and the `pgx` driver import — lives in the `//go:build integration` file,
so the default build has no `TestMain`, no driver, and compiles `sql.go` purely
against `database/sql`. Confirm `go test ./...` runs only the `MemStore` tests, and
that the `SQLStore` round-trip appears only under `-tags=integration` with a DSN.

## Resources

- [testing: TestMain](https://pkg.go.dev/testing#hdr-Main) — the `TestMain(m *testing.M)` lifecycle and `m.Run()`.
- [database/sql: Open and PingContext](https://pkg.go.dev/database/sql#Open) — opening a pool and verifying connectivity.
- [os.Exit](https://pkg.go.dev/os#Exit) — why deferred functions do not run, motivating the `run(m)` helper.
- [Accessing a relational database](https://go.dev/doc/tutorial/database-access) — the official tutorial for `database/sql` with a driver.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-run-and-verify-both-tiers.md](03-run-and-verify-both-tiers.md) | Next: [05-transaction-per-test-isolation.md](05-transaction-per-test-isolation.md)
