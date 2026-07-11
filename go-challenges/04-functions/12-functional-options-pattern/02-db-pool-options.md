# Exercise 2: database/sql Connection Pool Configurator via Options

Tuning a `database/sql` pool is real production work: too few connections and
requests queue, too many and you exhaust the database's connection limit. This
module wraps `*sql.DB` in a `Pool` constructor that takes the database as a
mandatory positional argument and the tuning knobs as validating options — and it
shows the one thing per-option validation cannot do: check an invariant that
spans two options.

This module is fully self-contained: its own `go mod init`, a fake
`driver.Connector` so no real database is needed, its own demo and tests.

## What you'll build

```text
dbpool/                          independent module: example.com/dbpool
  go.mod                         go 1.26
  pool.go                        Pool, Option, NewPool(*sql.DB, ...Option),
                                 WithMaxOpenConns, WithMaxIdleConns,
                                 WithConnMaxLifetime, WithConnMaxIdleTime
  cmd/
    demo/
      main.go                    builds a *sql.DB over a fake connector, tunes it, prints Stats
  pool_test.go                   asserts Stats().MaxOpenConnections and cross-field rejection
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `NewPool(db *sql.DB, opts ...Option) (*Pool, error)` that takes a mandatory `*sql.DB`, seeds coherent defaults, applies options, validates the cross-field invariant `maxIdle <= maxOpen`, then calls the `SetMax*`/`SetConnMax*` setters.
- Test: build a `*sql.DB` with `sql.OpenDB` over a no-op `driver.Connector` (no dialing), apply options, and assert `db.Stats().MaxOpenConnections`; assert the constructor rejects `maxIdle > maxOpen` and negative durations.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dbpool/cmd/demo
cd ~/go-exercises/dbpool
go mod init example.com/dbpool
```

### A mandatory dependency plus optional tuning

Not every constructor argument should be an option. The `*sql.DB` here is
*required* — a pool without a database is meaningless — so it stays a positional
argument, and passing `nil` is rejected immediately. Everything that is genuinely
optional (the four tuning parameters) becomes an option with a sane default. This
is the idiomatic split: required collaborators are positional, optional tuning is
functional. Forcing a required dependency through an option would let a caller
build a `Pool` with no database and only discover it at first query.

### Why the invariant is checked after the loop

`maxIdleConns` must not exceed `maxOpenConns` when `maxOpen > 0`; an idle pool
larger than the open limit is nonsensical, and the real `database/sql` silently
clamps it, hiding the caller's mistake. We want to reject it. The catch is that no
single option can check this. `WithMaxIdleConns(20)` does not know what
`WithMaxOpenConns` will be set to — it may run before or after it. The
relationship is only visible once *both* options have run, which is exactly why
the check lives in the constructor body after the option loop, not inside an
option. This is the canonical example of a cross-field invariant.

The defaults are chosen to be coherent (`maxOpen = 10`, `maxIdle = 5`) so a caller
who overrides only one still gets a valid combination unless they deliberately
invert it. Durations default to zero, which `database/sql` reads as "no limit";
we validate only that they are non-negative.

Create `pool.go`:

```go
package dbpool

import (
	"database/sql"
	"fmt"
	"time"
)

// Pool wraps a *sql.DB with validated connection-pool tuning.
type Pool struct {
	db              *sql.DB
	maxOpen         int
	maxIdle         int
	connMaxLifetime time.Duration
	connMaxIdleTime time.Duration
}

// Option configures a Pool and may reject invalid input.
type Option func(*Pool) error

// NewPool tunes db according to opts. db is mandatory. Cross-field invariants
// are validated after all options have run, before the settings are applied.
func NewPool(db *sql.DB, opts ...Option) (*Pool, error) {
	if db == nil {
		return nil, fmt.Errorf("db is required")
	}

	p := &Pool{
		db:      db,
		maxOpen: 10,
		maxIdle: 5,
	}

	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}

	if p.maxOpen > 0 && p.maxIdle > p.maxOpen {
		return nil, fmt.Errorf("maxIdleConns (%d) must not exceed maxOpenConns (%d)", p.maxIdle, p.maxOpen)
	}

	p.db.SetMaxOpenConns(p.maxOpen)
	p.db.SetMaxIdleConns(p.maxIdle)
	p.db.SetConnMaxLifetime(p.connMaxLifetime)
	p.db.SetConnMaxIdleTime(p.connMaxIdleTime)
	return p, nil
}

// WithMaxOpenConns caps the total open connections (0 means unlimited).
func WithMaxOpenConns(n int) Option {
	return func(p *Pool) error {
		if n < 0 {
			return fmt.Errorf("maxOpenConns must be >= 0, got %d", n)
		}
		p.maxOpen = n
		return nil
	}
}

// WithMaxIdleConns caps the idle connections kept in the pool.
func WithMaxIdleConns(n int) Option {
	return func(p *Pool) error {
		if n < 0 {
			return fmt.Errorf("maxIdleConns must be >= 0, got %d", n)
		}
		p.maxIdle = n
		return nil
	}
}

// WithConnMaxLifetime bounds how long a connection may be reused.
func WithConnMaxLifetime(d time.Duration) Option {
	return func(p *Pool) error {
		if d < 0 {
			return fmt.Errorf("connMaxLifetime must be >= 0, got %s", d)
		}
		p.connMaxLifetime = d
		return nil
	}
}

// WithConnMaxIdleTime bounds how long a connection may sit idle.
func WithConnMaxIdleTime(d time.Duration) Option {
	return func(p *Pool) error {
		if d < 0 {
			return fmt.Errorf("connMaxIdleTime must be >= 0, got %s", d)
		}
		p.connMaxIdleTime = d
		return nil
	}
}

// DB returns the tuned database handle.
func (p *Pool) DB() *sql.DB { return p.db }

// MaxOpenConns returns the configured open-connection cap.
func (p *Pool) MaxOpenConns() int { return p.maxOpen }

// MaxIdleConns returns the configured idle-connection cap.
func (p *Pool) MaxIdleConns() int { return p.maxIdle }
```

### The fake connector

`sql.OpenDB` takes a `driver.Connector` and returns a `*sql.DB` without ever
dialing — connections are opened lazily on first use. Since we only tune the pool
and read `Stats()`, no connection is ever opened, so a connector whose `Connect`
would fail is perfectly fine: it is never called. This is what makes the whole
module deterministic and network-free.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"example.com/dbpool"
)

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("fake: no real connection")
}
func (fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake: no real connection")
}

func main() {
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	pool, err := dbpool.NewPool(db,
		dbpool.WithMaxOpenConns(25),
		dbpool.WithMaxIdleConns(10),
		dbpool.WithConnMaxLifetime(30*time.Minute),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("stats MaxOpenConnections: %d\n", pool.DB().Stats().MaxOpenConnections)
	fmt.Printf("configured MaxIdleConns: %d\n", pool.MaxIdleConns())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stats MaxOpenConnections: 25
configured MaxIdleConns: 10
```

### Tests

`TestAppliesPoolSettings` proves the setter actually reached the driver by reading
`db.Stats().MaxOpenConnections`, the one configured value `DBStats` exposes.
`TestRejectsIdleExceedingOpen` proves the cross-field invariant fires when
`maxIdle > maxOpen`, and `TestRejectsNegativeDuration` proves the per-option
duration check. All of it runs against a fake connector, so it is instant and
needs no database.

Create `pool_test.go`:

```go
package dbpool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("fake: no real connection")
}
func (fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake: no real connection")
}

func newFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeConnector{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAppliesPoolSettings(t *testing.T) {
	t.Parallel()

	db := newFakeDB(t)
	pool, err := NewPool(db,
		WithMaxOpenConns(25),
		WithMaxIdleConns(10),
		WithConnMaxLifetime(time.Hour),
		WithConnMaxIdleTime(10*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := db.Stats().MaxOpenConnections; got != 25 {
		t.Fatalf("Stats().MaxOpenConnections = %d, want 25", got)
	}
	if pool.MaxIdleConns() != 10 {
		t.Fatalf("MaxIdleConns() = %d, want 10", pool.MaxIdleConns())
	}
}

func TestRejectsIdleExceedingOpen(t *testing.T) {
	t.Parallel()

	db := newFakeDB(t)
	_, err := NewPool(db,
		WithMaxOpenConns(4),
		WithMaxIdleConns(8),
	)
	if err == nil {
		t.Fatal("expected error for maxIdle > maxOpen, got nil")
	}
}

func TestRejectsNegativeDuration(t *testing.T) {
	t.Parallel()

	db := newFakeDB(t)
	_, err := NewPool(db, WithConnMaxLifetime(-time.Second))
	if err == nil {
		t.Fatal("expected error for negative connMaxLifetime, got nil")
	}
}
```

## Review

The configurator is correct when a required `*sql.DB` is positional and the
tuning is optional, and when the cross-field invariant is enforced where it can
actually be seen — after every option has run. The instructive failure to avoid
is scattering the `maxIdle <= maxOpen` check inside an option, where it cannot
observe the other value; `TestRejectsIdleExceedingOpen` only passes because the
check lives in the constructor body. Reading `db.Stats().MaxOpenConnections`
rather than trusting the `Pool`'s own field proves the setter reached the driver,
not just that the struct recorded a number. Because `sql.OpenDB` never dials until
a query runs, the fake connector keeps the whole test suite deterministic and
offline.

## Resources

- [database/sql OpenDB](https://pkg.go.dev/database/sql#OpenDB)
- [database/sql DBStats](https://pkg.go.dev/database/sql#DBStats)
- [database/sql DB.SetMaxOpenConns](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns)
- [database/sql/driver Connector](https://pkg.go.dev/database/sql/driver#Connector)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-error-returning-options-http-client.md](01-error-returning-options-http-client.md) | Next: [03-slog-logger-options.md](03-slog-logger-options.md)
