# Exercise 4: Lazy Database Handle: OnceValues and the Cached-Error Trap

Wrapping "open the database, ping it, share the handle" in `sync.OnceValues`
is the textbook use of the helper — and it hides a production failure mode
severe enough to deserve its own module: a transient network blip at first
call gets cached as the permanent answer until the process restarts.

## What you'll build

```text
dbhandle/                  independent module: example.com/dbhandle
  go.mod                   go mod init example.com/dbhandle
  dbhandle.go              PingCloser interface; Repo with DB() over OnceValues; SQLOpener adapter
  dbhandle_test.go         opens-exactly-once concurrent test; the cached-error trap test; Example
  cmd/
    demo/
      main.go              runnable demo: fail-once opener, error cached across three calls
```

- Files: `dbhandle.go`, `dbhandle_test.go`, `cmd/demo/main.go`.
- Implement: `Repo` whose `DB() (PingCloser, error)` is backed by `sync.OnceValues` wrapping open-plus-ping, with an injectable opener `func(ctx context.Context) (PingCloser, error)`; `SQLOpener(driver, dsn string)` adapting `database/sql`.
- Test: success path opens exactly once across 100 concurrent callers and all see the same handle; a fail-once-then-succeed opener proves the error stays cached forever, asserted via `errors.Is` against a sentinel.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/dbhandle/cmd/demo
cd ~/go-exercises/dbhandle
go mod init example.com/dbhandle
```

### The right shape for a shared handle

`*sql.DB` is already a concurrency-safe pool designed to be created once and
shared; what teams add on top is the lazy, verified open: `sql.Open` alone
validates nothing (it only prepares the pool), so a first-use `PingContext`
is the moment the DSN, network, and credentials are actually checked. Doing
open-plus-ping inside `sync.OnceValues` gives every caller the single-flight
property — one goroutine dials, the rest wait — and a shared `(handle, error)`
answer with the memory-model guarantee that the fully constructed pool is
safely visible to all of them.

The repository takes the opener as a function rather than calling `sql.Open`
directly. That is not test-only ceremony: the injectable opener is what lets
this module *demonstrate* its own failure mode deterministically, and in real
code it is where you hang PgBouncer endpoints, IAM token minting, or a tracing
wrapper. `SQLOpener` adapts the real `database/sql` API to the same signature,
so production wiring is one line. The `PingCloser` interface is the small
surface the repo needs — `*sql.DB` satisfies it — which keeps the fake in the
tests honest and tiny.

### The trap this module exists to make visible

Read the init closure carefully: if `open` or the ping fails, `OnceValues`
caches that error and returns it to every future caller, forever. Now play the
incident: the service boots during a 10-second database failover. The first
request arrives at second 3, the dial fails with `connection refused`,
`OnceValues` latches it. At second 10 the database is healthy — but this
process will answer `open database: ... connection refused` until someone
restarts it. Kubernetes will not restart it: the process is alive and its
liveness probe passes. You have built a service that must be manually bounced
after any blip that happens to coincide with its first query.

When is permanent caching *correct*? When the failure is deterministic — the
same call can never succeed in this process: a malformed DSN, an unregistered
driver name (both are exactly what `sql.Open` itself reports), a missing CA
bundle baked into the image. Those deserve latching: retrying them lies to the
operator about the health of a broken deploy. The senior judgment call is
classifying your failure mode before choosing the primitive. Transient
failures need retry-until-success, which `Once` cannot express — that is
exercise 05, and the trap test here is its motivation.

Create `dbhandle.go`:

```go
// Package dbhandle provides a lazily opened, process-wide database handle
// behind sync.OnceValues, and deliberately exposes the design consequence:
// the first error is cached for the life of the process.
package dbhandle

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// PingCloser is the subset of *sql.DB the repository needs. Fakes in tests
// implement it; *sql.DB satisfies it.
type PingCloser interface {
	PingContext(ctx context.Context) error
	Close() error
}

// Repo owns the shared database handle. DB opens and verifies it at most
// once; the (handle, error) pair of that single attempt is the answer for
// every caller, forever.
type Repo struct {
	db func() (PingCloser, error)
}

// New builds a Repo around open. The first DB call runs open and verifies
// the connection with PingContext under timeout; concurrent first callers
// block until that single attempt finishes (single-flight).
func New(open func(ctx context.Context) (PingCloser, error), timeout time.Duration) *Repo {
	return &Repo{
		db: sync.OnceValues(func() (PingCloser, error) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			db, err := open(ctx)
			if err != nil {
				return nil, fmt.Errorf("open database: %w", err)
			}
			if err := db.PingContext(ctx); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("ping database: %w", err)
			}
			return db, nil
		}),
	}
}

// DB returns the shared handle, opening it on first call. If the single
// open attempt failed, DB returns that cached error on every call: correct
// for deterministic failures (bad DSN, missing driver), wrong for transient
// ones -- see the retryable Lazy in the next exercise.
func (r *Repo) DB() (PingCloser, error) {
	return r.db()
}

// SQLOpener adapts database/sql to the opener signature. sql.Open validates
// only its arguments (an unregistered driver name fails here); the network
// is first touched by the PingContext in New.
func SQLOpener(driverName, dsn string) func(ctx context.Context) (PingCloser, error) {
	return func(ctx context.Context) (PingCloser, error) {
		db, err := sql.Open(driverName, dsn)
		if err != nil {
			return nil, err
		}
		return db, nil
	}
}
```

Production wiring is then one line per service — for example with a
registered Postgres driver:

```
repo := dbhandle.New(dbhandle.SQLOpener("pgx", os.Getenv("DATABASE_URL")), 5*time.Second)
```

### The demo

The demo injects a flaky opener: attempt 1 fails with `connection refused`,
and every attempt after that would succeed — but there is no attempt 2. Three
`DB()` calls print the same cached error and the attempt counter stays at 1.
This is the trap, made observable in four lines of output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/dbhandle"
)

type fakeDB struct{}

func (fakeDB) PingContext(context.Context) error { return nil }
func (fakeDB) Close() error                      { return nil }

func main() {
	attempts := 0
	open := func(ctx context.Context) (dbhandle.PingCloser, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
		}
		return fakeDB{}, nil // the outage is over from attempt 2 on
	}

	repo := dbhandle.New(open, time.Second)
	for i := 1; i <= 3; i++ {
		_, err := repo.DB()
		fmt.Printf("call %d: %v\n", i, err)
	}
	fmt.Println("open attempts:", attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: open database: dial tcp 10.0.0.5:5432: connect: connection refused
call 2: open database: dial tcp 10.0.0.5:5432: connect: connection refused
call 3: open database: dial tcp 10.0.0.5:5432: connect: connection refused
open attempts: 1
```

The database recovered before call 2. The process never noticed.

### Tests

`TestOpensExactlyOnce` is the success-path contract: 100 concurrent `DB()`
calls, one open, one ping, and every caller receives the identical handle.
`TestTransientErrorCachedForever` is the trap: the opener fails once and then
flips healthy, yet every later call still gets the first error — asserted
through the `%w` wrap with `errors.Is` — and the attempt counter proves no
retry ever happened. `TestPingFailureClosesHandle` checks the cleanup detail:
a handle that fails its ping is closed, not leaked.

Create `dbhandle_test.go`:

```go
package dbhandle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errConnRefused = errors.New("connection refused")

type fakeDB struct {
	pingErr error
	closed  atomic.Int64
}

func (f *fakeDB) PingContext(context.Context) error { return f.pingErr }
func (f *fakeDB) Close() error                      { f.closed.Add(1); return nil }

func TestOpensExactlyOnce(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	handle := &fakeDB{}
	repo := New(func(ctx context.Context) (PingCloser, error) {
		attempts.Add(1)
		return handle, nil
	}, time.Second)

	var wg sync.WaitGroup
	const goroutines = 100
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			db, err := repo.DB()
			if err != nil {
				t.Errorf("DB() error: %v", err)
				return
			}
			if db != PingCloser(handle) {
				t.Error("DB() returned a different handle")
			}
		}()
	}
	wg.Wait()
	if got := attempts.Load(); got != 1 {
		t.Fatalf("opener called %d times, want 1", got)
	}
	if got := handle.closed.Load(); got != 0 {
		t.Fatalf("healthy handle was closed %d times", got)
	}
}

func TestTransientErrorCachedForever(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	repo := New(func(ctx context.Context) (PingCloser, error) {
		if attempts.Add(1) == 1 {
			return nil, fmt.Errorf("dial tcp 10.0.0.5:5432: %w", errConnRefused)
		}
		return &fakeDB{}, nil // healthy from attempt 2 on -- but there is no attempt 2
	}, time.Second)

	if _, err := repo.DB(); !errors.Is(err, errConnRefused) {
		t.Fatalf("first DB() = %v, want errConnRefused", err)
	}
	for i := range 10 {
		_, err := repo.DB()
		if !errors.Is(err, errConnRefused) {
			t.Fatalf("call %d after recovery = %v, want the cached errConnRefused", i, err)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("opener retried: %d attempts, want 1 (OnceValues never re-runs)", got)
	}
}

func TestPingFailureClosesHandle(t *testing.T) {
	t.Parallel()

	sick := &fakeDB{pingErr: fmt.Errorf("ping: %w", errConnRefused)}
	repo := New(func(ctx context.Context) (PingCloser, error) {
		return sick, nil
	}, time.Second)

	if _, err := repo.DB(); !errors.Is(err, errConnRefused) {
		t.Fatalf("DB() = %v, want errConnRefused", err)
	}
	if got := sick.closed.Load(); got != 1 {
		t.Fatalf("failed handle closed %d times, want 1", got)
	}
}

func ExampleRepo_DB() {
	repo := New(func(ctx context.Context) (PingCloser, error) {
		return &fakeDB{}, nil
	}, time.Second)
	_, err := repo.DB()
	fmt.Println("connected:", err == nil)
	// Output: connected: true
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The module behaves correctly when both of its faces are proven: the success
path opens exactly once under 100-goroutine contention with every caller
sharing one handle, and the failure path shows the opener never being retried
even after the fake dependency recovers. The mistake to internalize is not in
the code — the code does exactly what `OnceValues` promises — it is in the
*choice*: reaching for `OnceValues` because the shape fits, without asking
whether the init's failures are deterministic. Bad DSN, unknown driver,
missing CA: latch them. Connection refused, DNS timeout, failover in
progress: never latch them. A secondary trap worth remembering from the ping
branch: when init acquires a resource and then fails a later step, it must
release the resource before returning, because there will never be another
init run to clean it up. If your service must survive a dependency that is
down at first use, continue to the next exercise.

## Resources

- [sync.OnceValues](https://pkg.go.dev/sync#OnceValues) — the (value, error) caching contract.
- [database/sql: Open and DB.PingContext](https://pkg.go.dev/database/sql#Open) — why Open validates nothing and the ping is the real check.
- [Opening a database handle](https://go.dev/doc/database/open-handle) — the official guidance on creating one shared *sql.DB.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-once-panic-contract.md](03-once-panic-contract.md) | Next: [05-retryable-lazy-init.md](05-retryable-lazy-init.md)
