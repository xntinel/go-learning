# Exercise 3: Deadline-Bounded Repository Query

The rule in a well-run backend is absolute: every database call takes a context and
a query budget. A query that hangs must not hang the request. This exercise builds a
repository method that runs a lookup under a derived per-query timeout, passes the
context into the context-aware query API so the driver can cancel a slow query,
always closes the rows, and translates a deadline into a domain-level error while
"no such row" stays a distinct not-found.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs (including a small `Queryer` abstraction and a fake), and ships its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
user-repo/                           independent module: example.com/userrepo
  go.mod                             go 1.26
  repo.go                            Queryer, Rows, Repo, GetUser; ErrQueryTimeout, ErrUserNotFound
  cmd/
    demo/
      main.go                        runnable demo: fast row, slow timeout, not-found
  repo_test.go                       fake Queryer (fast/slow/missing), rows-closed check, -race
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Queryer` interface (the context-aware query surface), a `Repo` with a `QueryTimeout`, and `GetUser(ctx, id) (User, error)` that derives a `WithTimeout`, calls `QueryContext`, closes `Rows`, maps `DeadlineExceeded` to `ErrQueryTimeout` and a no-row result to `ErrUserNotFound`.
- Test: a slow fake that blocks on `ctx.Done()` yields `ErrQueryTimeout` (with `DeadlineExceeded` underneath); a fast fake returns the row; a missing id yields `ErrUserNotFound`; rows are always closed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/user-repo/cmd/demo
cd ~/go-exercises/user-repo
go mod init example.com/userrepo
```

### Why an interface instead of *sql.DB

The real method would call `db.QueryContext` on a `*sql.DB`, but a lesson that
spins up a database is neither self-contained nor deterministic. The production-grade
move is to depend on the *narrow surface* the repository actually uses — a `Queryer`
with a single `QueryContext` method — rather than the concrete `*sql.DB`. `*sql.DB`
satisfies this interface as-is in production (`QueryContext(ctx, query, args...)
(*sql.Rows, error)`), and in tests a hand-written fake satisfies it too. This is the
standard hexagonal seam: the domain depends on a port, the adapter is `database/sql`
in production and a fake in the test. It also makes the cancellation behavior
directly testable, because the fake can *choose* to block on `ctx.Done()` exactly
like a real driver waiting on a slow query.

The critical detail is that `ctx` flows into `QueryContext`. `database/sql`'s
context-aware methods hand the context to the driver, which uses it to cancel the
query on the server (Postgres issues a cancel request; MySQL kills the query) when
the deadline fires. Using the non-context `Query` would leave the query running on
the database even after the Go side gave up — the deadline would free the goroutine
but not the expensive query, which is the exact resource you were trying to protect.

`Rows` must be closed on every path. We `defer rows.Close()` immediately after a
successful `QueryContext`, so the not-found path, the scan-error path, and the happy
path all release the underlying connection. `sql.ErrNoRows` is the sentinel a real
`QueryRow`-style scan returns for an empty result; here the `Rows`-based shape
reports emptiness through `rows.Next()` returning false, which we translate to
`ErrUserNotFound`. A timeout is caught by `errors.Is(err, context.DeadlineExceeded)`
and mapped to `ErrQueryTimeout`, wrapping the original so both remain discoverable.

Create `repo.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrQueryTimeout is the domain error for a query that exceeded its budget.
// It wraps context.DeadlineExceeded.
var ErrQueryTimeout = errors.New("query timed out")

// ErrUserNotFound is returned when no row matches the id.
var ErrUserNotFound = errors.New("user not found")

// User is the row we read.
type User struct {
	ID   int
	Name string
}

// Rows is the minimal row-cursor surface the repo consumes. *sql.Rows satisfies it.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Queryer is the context-aware query surface. *sql.DB satisfies it.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (Rows, error)
}

// Repo reads users under a per-query time budget.
type Repo struct {
	DB           Queryer
	QueryTimeout time.Duration
}

// GetUser looks up a user by id under a derived per-query deadline. It maps a
// deadline hit to ErrQueryTimeout and an empty result to ErrUserNotFound.
func (r *Repo) GetUser(ctx context.Context, id int) (User, error) {
	ctx, cancel := context.WithTimeout(ctx, r.QueryTimeout)
	defer cancel()

	rows, err := r.DB.QueryContext(ctx, "SELECT id, name FROM users WHERE id = ?", id)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return User{}, fmt.Errorf("%w: %w", ErrQueryTimeout, err)
		}
		return User{}, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return User{}, fmt.Errorf("%w: %w", ErrQueryTimeout, err)
			}
			return User{}, fmt.Errorf("iterate rows: %w", err)
		}
		return User{}, fmt.Errorf("id %d: %w", id, ErrUserNotFound)
	}

	var u User
	if err := rows.Scan(&u.ID, &u.Name); err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}
```

### The runnable demo

The demo wires a tiny in-memory fake implementing `Queryer` in three modes — a fast
row, a slow query that blocks past the budget, and a missing id — and prints how
`GetUser` maps each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/userrepo"
)

// fakeDB blocks for delay before returning the row, or reports missing.
type fakeDB struct {
	delay   time.Duration
	missing bool
}

func (f fakeDB) QueryContext(ctx context.Context, query string, args ...any) (userrepo.Rows, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &fakeRows{missing: f.missing}, nil
}

type fakeRows struct {
	missing bool
	done    bool
}

func (r *fakeRows) Next() bool {
	if r.missing || r.done {
		return false
	}
	r.done = true
	return true
}
func (r *fakeRows) Scan(dest ...any) error {
	*dest[0].(*int) = 7
	*dest[1].(*string) = "alice"
	return nil
}
func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

func main() {
	fast := &userrepo.Repo{DB: fakeDB{}, QueryTimeout: time.Second}
	u, err := fast.GetUser(context.Background(), 7)
	fmt.Printf("fast: user=%+v err=%v\n", u, err)

	slow := &userrepo.Repo{DB: fakeDB{delay: 500 * time.Millisecond}, QueryTimeout: 30 * time.Millisecond}
	_, err = slow.GetUser(context.Background(), 7)
	fmt.Printf("slow: timeout=%v\n", errors.Is(err, userrepo.ErrQueryTimeout))

	missing := &userrepo.Repo{DB: fakeDB{missing: true}, QueryTimeout: time.Second}
	_, err = missing.GetUser(context.Background(), 99)
	fmt.Printf("missing: notfound=%v\n", errors.Is(err, userrepo.ErrUserNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: user={ID:7 Name:alice} err=<nil>
slow: timeout=true
missing: notfound=true
```

### Tests

The fake `Queryer` records whether its rows were closed and can block on
`ctx.Done()` to emulate a slow query. `TestSlowQueryTimesOut` asserts the error
`errors.Is` both `ErrQueryTimeout` and `context.DeadlineExceeded` and that the call
returns near the budget. `TestFastQueryReturnsUser` asserts the happy path.
`TestMissingUserNotFound` asserts the not-found mapping. `TestRowsAlwaysClosed`
runs all three modes and asserts `Close` fired every time.

Create `repo_test.go`:

```go
package userrepo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRows struct {
	missing bool
	done    bool
	closed  *atomic.Bool
}

func (r *fakeRows) Next() bool {
	if r.missing || r.done {
		return false
	}
	r.done = true
	return true
}
func (r *fakeRows) Scan(dest ...any) error {
	*dest[0].(*int) = 7
	*dest[1].(*string) = "alice"
	return nil
}
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Close() error {
	if r.closed != nil {
		r.closed.Store(true)
	}
	return nil
}

type fakeDB struct {
	delay   time.Duration
	missing bool
	closed  *atomic.Bool
}

func (f fakeDB) QueryContext(ctx context.Context, query string, args ...any) (Rows, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &fakeRows{missing: f.missing, closed: f.closed}, nil
}

func TestSlowQueryTimesOut(t *testing.T) {
	t.Parallel()
	r := &Repo{DB: fakeDB{delay: 500 * time.Millisecond}, QueryTimeout: 30 * time.Millisecond}

	start := time.Now()
	_, err := r.GetUser(context.Background(), 7)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("err = %v, want ErrQueryTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded in chain", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("query took %v, want near the 30ms budget", elapsed)
	}
}

func TestFastQueryReturnsUser(t *testing.T) {
	t.Parallel()
	r := &Repo{DB: fakeDB{}, QueryTimeout: time.Second}
	u, err := r.GetUser(context.Background(), 7)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if u.ID != 7 || u.Name != "alice" {
		t.Fatalf("user = %+v, want {7 alice}", u)
	}
}

func TestMissingUserNotFound(t *testing.T) {
	t.Parallel()
	r := &Repo{DB: fakeDB{missing: true}, QueryTimeout: time.Second}
	_, err := r.GetUser(context.Background(), 99)
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
	if errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("err = %v, must NOT be a timeout", err)
	}
}

func TestRowsAlwaysClosed(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		var closed atomic.Bool
		r := &Repo{DB: fakeDB{missing: missing, closed: &closed}, QueryTimeout: time.Second}
		_, _ = r.GetUser(context.Background(), 7)
		if !closed.Load() {
			t.Fatalf("missing=%v: rows were not closed", missing)
		}
	}
}
```

## Review

The repository is correct when a slow query cannot outlast its budget and the three
outcomes stay distinct. The context must flow into `QueryContext` so the driver can
cancel the server-side query; the fake proves this by blocking on `ctx.Done()` and
returning `ctx.Err()`, which `GetUser` translates to `ErrQueryTimeout` wrapping
`context.DeadlineExceeded`. A missing row maps to `ErrUserNotFound` and is never
confused with a timeout, so a caller retries a timeout but returns a clean 404 for a
genuine absence. `Rows` are closed on every path, which `TestRowsAlwaysClosed`
enforces across both the found and not-found branches.

The mistakes to avoid: calling `Query` instead of `QueryContext` (the query keeps
running on the database after Go gives up); forgetting `defer rows.Close()` (leaked
connections that eventually exhaust the pool); and collapsing not-found into the
timeout branch or vice versa. Wrapping with `%w` at every boundary is what keeps
`errors.Is` working two layers deep. Run `go test -race`; the atomic close-flag and
the fake's goroutine-free blocking are checked under the detector.

## Resources

- [database/sql DB.QueryContext](https://pkg.go.dev/database/sql#DB.QueryContext) — the context-aware query the driver uses to cancel a slow query.
- [database/sql Rows](https://pkg.go.dev/database/sql#Rows) — the cursor whose Close releases the connection, and Err surfaces a mid-iteration failure.
- [sql.ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables) — the sentinel for an empty single-row result.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the per-query budget derived from the caller's context.

---

Back to [02-outbound-http-timeout.md](02-outbound-http-timeout.md) | Next: [04-total-budget-retry.md](04-total-budget-retry.md)
