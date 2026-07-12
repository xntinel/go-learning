# Exercise 6: Bounded Readiness Retry Against A Just-Started Database

In CI and with testcontainers, the test binary routinely starts before Postgres
accepts connections. This module builds `waitForDB`, a bounded `PingContext` retry
with capped backoff and a `context` deadline — it returns the moment the database is
ready and returns `context.DeadlineExceeded` (never hangs) when it is not. The retry
logic is written against a `Pinger` interface so it is fully tested in the default
build with fakes, no database required.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
dbready/                   independent module: example.com/dbready
  go.mod
  wait.go                  Pinger interface; waitForDB with capped backoff + deadline
  cmd/
    demo/
      main.go              runs waitForDB against a fake that comes up on attempt 3
  wait_test.go             fakes: fail-N-then-succeed, never-succeed (DeadlineExceeded)
  ready_integration_test.go   //go:build integration: waitForDB against the real DSN
```

- Files: `wait.go`, `cmd/demo/main.go`, `wait_test.go`, `ready_integration_test.go`.
- Implement: `waitForDB(ctx, p Pinger, base, max)` retrying `PingContext` with exponential-capped backoff, bounded by the context deadline.
- Test: a fake that fails N times then succeeds (assert bounded attempts, no infinite loop); a fake that never succeeds (assert `context.DeadlineExceeded`); the real DSN in the integration build.
- Verify: the default build tests the backoff/deadline logic with fakes; the integration test drives `waitForDB` against a real `*sql.DB`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/06-db-readiness-retry-connect/cmd/demo
cd go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/06-db-readiness-retry-connect
```

### Why not a sleep, and why not an unbounded loop

The two wrong fixes are worth naming because both are common. A fixed
`time.Sleep(5 * time.Second)` before the first query is *simultaneously* flaky and
slow: too short when the runner is loaded and Postgres takes longer, too long on
every run where it is already up. An unbounded ping-until-success loop is worse —
when the database never comes up (a bad DSN, a crashed container), the CI job hangs
until the outer timeout kills it with no useful error.

`waitForDB` is the correct shape. It calls `PingContext` in a loop; on success it
returns immediately (so a ready database costs one ping), and on failure it waits a
backoff that doubles up to a cap, selecting on `ctx.Done()` so the *context
deadline* bounds the whole operation. When the deadline elapses it returns a wrapped
`ctx.Err()`, which callers match with `errors.Is(err, context.DeadlineExceeded)`. It
never hangs and it never sleeps longer than it must.

The deadline is supplied by the caller — in a test, `t.Context()`, so a stuck
readiness check fails the test rather than wedging the tier. The `Pinger` interface
(`PingContext(ctx) error`) is satisfied by `*sql.DB` and by test fakes, so the exact
retry/deadline behavior is pinned in the fast tier without a database.

Create `wait.go`:

```go
package dbready

import (
	"context"
	"fmt"
	"time"
)

// Pinger is the readiness surface. *sql.DB satisfies it via PingContext, and so do
// test fakes, which is what lets the retry logic be tested without a database.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// waitForDB retries p.PingContext until it succeeds or ctx is done. Backoff starts
// at base and doubles up to max between attempts. It returns the attempt number on
// success, or a wrapped ctx.Err() (never an infinite loop) when the deadline
// elapses first.
func waitForDB(ctx context.Context, p Pinger, base, max time.Duration) (int, error) {
	backoff := base
	for attempt := 1; ; attempt++ {
		if err := p.PingContext(ctx); err == nil {
			return attempt, nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempt, fmt.Errorf("waitForDB: gave up after %d attempt(s): %w", attempt, ctx.Err())
		case <-timer.C:
		}
		if backoff *= 2; backoff > max {
			backoff = max
		}
	}
}

// WaitForDB is the exported entry point production callers use with a *sql.DB.
func WaitForDB(ctx context.Context, p Pinger, base, max time.Duration) (int, error) {
	return waitForDB(ctx, p, base, max)
}
```

Now the tag-gated integration check drives the real database:

Create `ready_integration_test.go`:

```go
//go:build integration

package dbready

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestWaitForRealDB(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration tier: set DATABASE_URL to run")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t.Context() bounds the whole readiness check: a database that never comes up
	// fails this test instead of hanging the integration stage.
	attempts, err := WaitForDB(t.Context(), db, 100*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("WaitForDB: %v", err)
	}
	t.Logf("database ready after %d attempt(s)", attempts)
}
```

### The runnable demo

The demo uses a fake that fails twice then succeeds, so you can watch the retry
converge without a database.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/dbready"
)

// slowStarter fails until the nth ping, modeling a database still booting.
type slowStarter struct {
	readyAt int
	calls   int
}

func (s *slowStarter) PingContext(ctx context.Context) error {
	s.calls++
	if s.calls < s.readyAt {
		return errors.New("connection refused")
	}
	return nil
}

func main() {
	p := &slowStarter{readyAt: 3}
	attempts, err := dbready.WaitForDB(context.Background(), p, time.Millisecond, 8*time.Millisecond)
	if err != nil {
		fmt.Println("not ready:", err)
		return
	}
	fmt.Printf("ready after %d attempt(s)\n", attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
ready after 3 attempt(s)
```

### Tests

The tests pin both outcomes with fakes: a database that comes up after N attempts
(bounded, converges) and one that never comes up (returns `DeadlineExceeded`, does
not loop forever). The never-succeed test *must* supply a deadline; that is the
whole point.

Create `wait_test.go`:

```go
package dbready

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// flakyPinger fails the first (readyAt-1) pings, then succeeds.
type flakyPinger struct {
	readyAt int
	calls   int
}

func (p *flakyPinger) PingContext(ctx context.Context) error {
	p.calls++
	if p.calls < p.readyAt {
		return errors.New("connection refused")
	}
	return nil
}

// deadPinger never succeeds.
type deadPinger struct{ calls int }

func (p *deadPinger) PingContext(ctx context.Context) error {
	p.calls++
	return errors.New("connection refused")
}

func TestWaitForDBConverges(t *testing.T) {
	t.Parallel()
	p := &flakyPinger{readyAt: 4}
	attempts, err := waitForDB(context.Background(), p, time.Millisecond, 4*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForDB: %v", err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	if p.calls != 4 {
		t.Fatalf("PingContext called %d times, want 4", p.calls)
	}
}

func TestWaitForDBDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	p := &deadPinger{}
	_, err := waitForDB(ctx, p, time.Millisecond, 4*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if p.calls == 0 {
		t.Fatal("expected at least one ping attempt before the deadline")
	}
}

func ExampleWaitForDB() {
	p := &flakyPinger{readyAt: 2}
	attempts, err := WaitForDB(context.Background(), p, time.Millisecond, 4*time.Millisecond)
	fmt.Println(attempts, err)
	// Output: 2 <nil>
}
```

## Review

`waitForDB` is correct when a ready database costs exactly one ping, a slow starter
converges in a bounded number of attempts, and a database that never comes up
returns a wrapped `context.DeadlineExceeded` instead of looping forever. The whole
design exists to avoid the two production failures: a fixed sleep (flaky and slow)
and an unbounded loop (hangs the CI job). Note that the never-succeed test must pass
a context with a deadline — without one there is nothing to bound the retry, which
is exactly the bug being guarded against. Threading `t.Context()` into the
integration check means a wedged database fails the test rather than the stage.
Confirm the default tests pass with fakes and no database, and that the deadline test
returns promptly rather than hanging.

## Resources

- [database/sql: DB.PingContext](https://pkg.go.dev/database/sql#DB.PingContext) — the readiness check the retry drives.
- [context: WithTimeout and DeadlineExceeded](https://pkg.go.dev/context#WithTimeout) — bounding the retry and the sentinel it returns.
- [time: NewTimer](https://pkg.go.dev/time#NewTimer) — the backoff timer selected against `ctx.Done()`.
- [testing: T.Context](https://pkg.go.dev/testing#T.Context) — the per-test deadline source for the integration check.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-transaction-per-test-isolation.md](05-transaction-per-test-isolation.md) | Next: [07-schema-migrate-and-seed.md](07-schema-migrate-and-seed.md)
