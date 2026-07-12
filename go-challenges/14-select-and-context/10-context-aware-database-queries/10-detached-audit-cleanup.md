# Exercise 10: Persisting an Audit Write Even When the Request Is Cancelled Mid-Flight

The primary work is bound to the request context, but some side effects are not
optional: an audit row, an outbox insert, releasing a lock. Run them on the
just-cancelled request context and they fail instantly and vanish — a real bug
that shows up as missing audit trails on every client disconnect. This exercise
builds the fix with `context.WithoutCancel` for the detached write and
`context.AfterFunc` for the compensating cleanup.

## What you'll build

```text
auditcleanup/                independent module: example.com/auditcleanup
  go.mod                     go 1.25; requires modernc.org/sqlite
  auditcleanup.go            WithTrace/TraceID; Store.Audit (detached); HandleWithAudit (AfterFunc)
  cmd/
    demo/
      main.go                audit survives a pre-cancelled request; compensator fires on cancel
  auditcleanup_test.go       survives-cancel, carries-values, AfterFunc-once, stop-deregisters, -race
```

Files: `auditcleanup.go`, `cmd/demo/main.go`, `auditcleanup_test.go`.
Implement: `Audit(reqCtx, action)` that writes on `context.WithoutCancel(reqCtx)` plus a fresh `WithTimeout`, carrying the request's trace id through as a value; and `HandleWithAudit` that registers a `context.AfterFunc` compensator (with its `stop`) and always audits.
Test: cancelling `reqCtx` first still writes the audit row (queried under `Background`); `WithoutCancel` still carries the trace id; the `AfterFunc` callback fires exactly once on cancellation; `stop()` deregisters it on the normal path so it never fires.
Verify: `go test -count=1 -race ./...`

### Why the audit needs a detached context, and what `AfterFunc` adds

The trap is subtle because the buggy version looks correct: you write the audit
row with `s.db.ExecContext(reqCtx, ...)`. But if `reqCtx` was cancelled — the
client disconnected mid-transaction — that `ExecContext` returns `context.Canceled`
before it touches the database, and the audit is silently lost. The fix is to
*detach* the write from the request's cancellation while keeping its values:

```
ctx := context.WithoutCancel(reqCtx)          // immune to reqCtx being cancelled
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)  // but still bounded
defer cancel()
s.db.ExecContext(ctx, `INSERT INTO audits(trace, action) VALUES(?, ?)`, TraceID(ctx), action)
```

`context.WithoutCancel(reqCtx)` returns a context that is never cancelled by
`reqCtx`, but still carries its values — so `TraceID(ctx)` still returns the trace
id set upstream, and the audit row is correctly attributed to the request even
though the request itself was aborted. You immediately give it a fresh short
`WithTimeout` so a detached write cannot hang forever; "detached" must not mean
"unbounded".

`context.AfterFunc(ctx, f)` is the second tool, for a *compensating* action —
releasing a lock, marking an outbox entry — that should run precisely *because*
the request was cancelled. It registers `f` to run in its own goroutine when `ctx`
becomes done, and returns a `stop`:

```
stop := context.AfterFunc(reqCtx, onCancel)
defer stop()
```

If the request completes normally, `reqCtx` is never cancelled, so `f` was never
scheduled; the deferred `stop()` deregisters it and returns `true`, meaning it
prevented the callback from running. If the request is cancelled, `f` runs once in
its goroutine (a later `stop()` returns `false` and does not un-run it). Because
`f` runs asynchronously, tests synchronize on it with a `sync.WaitGroup` rather
than a sleep.

`HandleWithAudit` composes both: it registers the compensator, runs the work under
`reqCtx`, and then audits on the detached context no matter what the work returned
— so the audit lands whether the request succeeded, failed, or was cancelled.

Set up the module:

```bash
go mod edit -go=1.25
go get modernc.org/sqlite
```

Create `auditcleanup.go`:

```go
package auditcleanup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ctxKey string

const traceKey ctxKey = "trace"

// WithTrace attaches a request trace id as a context value.
func WithTrace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey, id)
}

// TraceID reads the trace id, or "" if unset.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey).(string)
	return id
}

// Store writes audit rows.
type Store struct{ db *sql.DB }

// New wraps db in a Store.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Migrate creates the audits table.
func (s *Store) Migrate(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS audits (
	trace  TEXT NOT NULL,
	action TEXT NOT NULL
)`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("auditcleanup.Migrate: %w", err)
	}
	return nil
}

// Audit writes an audit row on a context detached from reqCtx's cancellation, so
// the row is not lost when the request is aborted mid-flight. It keeps reqCtx's
// values (the trace id) and bounds the detached write with a fresh timeout.
func (s *Store) Audit(reqCtx context.Context, action string) error {
	ctx := context.WithoutCancel(reqCtx)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audits(trace, action) VALUES(?, ?)`, TraceID(ctx), action); err != nil {
		return fmt.Errorf("auditcleanup.Audit: %w", err)
	}
	return nil
}

// AuditCount returns how many audit rows exist for a trace id.
func (s *Store) AuditCount(ctx context.Context, trace string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audits WHERE trace = ?`, trace).Scan(&n); err != nil {
		return 0, fmt.Errorf("auditcleanup.AuditCount: %w", err)
	}
	return n, nil
}

// HandleWithAudit runs work under reqCtx, registers onCancel to fire iff reqCtx
// is cancelled (via AfterFunc, deregistered on the normal path), and always
// writes an audit row on a detached context so it is never lost.
func (s *Store) HandleWithAudit(reqCtx context.Context, action string, onCancel func(), work func(context.Context) error) error {
	stop := context.AfterFunc(reqCtx, onCancel)
	defer stop()

	err := work(reqCtx)
	if auditErr := s.Audit(reqCtx, action); auditErr != nil {
		return errors.Join(err, auditErr)
	}
	return err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/auditcleanup"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	s := auditcleanup.New(db)
	if err := s.Migrate(context.Background()); err != nil {
		panic(err)
	}

	// The request is already cancelled, yet the detached audit still lands.
	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = auditcleanup.WithTrace(reqCtx, "trace-1")
	cancel()
	auditErr := s.Audit(reqCtx, "order.created")
	n, _ := s.AuditCount(context.Background(), "trace-1")
	fmt.Printf("detached audit: err=%v count=%d\n", auditErr, n)

	// Work cancels mid-flight; the compensator fires and the audit still lands.
	req2, cancel2 := context.WithCancel(context.Background())
	req2 = auditcleanup.WithTrace(req2, "trace-2")
	var wg sync.WaitGroup
	wg.Add(1)
	var compensated atomic.Bool
	_ = s.HandleWithAudit(req2, "order.paid",
		func() { compensated.Store(true); wg.Done() },
		func(ctx context.Context) error { cancel2(); return ctx.Err() })
	wg.Wait()
	n, _ = s.AuditCount(context.Background(), "trace-2")
	fmt.Printf("compensated: fired=%v audit-count=%d\n", compensated.Load(), n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
detached audit: err=<nil> count=1
compensated: fired=true audit-count=1
```

### Tests

Create `auditcleanup_test.go`:

```go
package auditcleanup

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Skipf("sqlite driver unavailable: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestAuditSurvivesCancellation(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = WithTrace(reqCtx, "trace-abc")
	cancel() // the request is already aborted

	if err := s.Audit(reqCtx, "order.created"); err != nil {
		t.Fatalf("Audit on cancelled reqCtx: err = %v, want nil (detached write)", err)
	}
	n, err := s.AuditCount(context.Background(), "trace-abc")
	if err != nil {
		t.Fatalf("AuditCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows for trace-abc = %d, want 1 (row lost to cancellation)", n)
	}
}

func TestWithoutCancelCarriesValues(t *testing.T) {
	t.Parallel()
	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = WithTrace(reqCtx, "trace-xyz")
	cancel()

	detached := context.WithoutCancel(reqCtx)
	if err := detached.Err(); err != nil {
		t.Fatalf("detached context is cancelled: %v (WithoutCancel must ignore parent cancel)", err)
	}
	if got := TraceID(detached); got != "trace-xyz" {
		t.Fatalf("TraceID(detached) = %q, want trace-xyz (values must survive)", got)
	}
}

func TestAfterFuncFiresOnceOnCancel(t *testing.T) {
	t.Parallel()
	reqCtx, cancel := context.WithCancel(context.Background())

	var count atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	stop := context.AfterFunc(reqCtx, func() {
		count.Add(1)
		wg.Done()
	})
	defer stop()

	cancel()
	wg.Wait()

	if got := count.Load(); got != 1 {
		t.Fatalf("compensator fired %d times, want exactly 1", got)
	}
}

func TestStopDeregistersOnNormalCompletion(t *testing.T) {
	t.Parallel()
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var count atomic.Int32
	stop := context.AfterFunc(reqCtx, func() { count.Add(1) })

	// Normal completion: deregister before any cancellation. stop() returns true
	// iff it prevented the callback from ever running.
	if !stop() {
		t.Fatal("stop() = false, want true (callback should have been deregistered)")
	}
	cancel() // even now, the callback must not run
	if got := count.Load(); got != 0 {
		t.Fatalf("compensator fired %d times after deregistration, want 0", got)
	}
}

func TestHandleWithAuditPersistsOnCancelledWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = WithTrace(reqCtx, "trace-h")

	var wg sync.WaitGroup
	wg.Add(1)
	var compensated atomic.Bool

	err := s.HandleWithAudit(reqCtx, "order.paid",
		func() { compensated.Store(true); wg.Done() },
		func(ctx context.Context) error {
			cancel() // client disconnects mid-work
			return ctx.Err()
		})
	wg.Wait()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleWithAudit: err = %v, want Canceled from the work", err)
	}
	if !compensated.Load() {
		t.Fatal("compensator did not fire on cancellation")
	}
	n, _ := s.AuditCount(context.Background(), "trace-h")
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1 (audit lost despite detached write)", n)
	}
}

func TestHandleWithAuditNormalPathDoesNotCompensate(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqCtx = WithTrace(reqCtx, "trace-ok")

	var count atomic.Int32
	err := s.HandleWithAudit(reqCtx, "order.ok",
		func() { count.Add(1) },
		func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("HandleWithAudit: err = %v, want nil", err)
	}
	// reqCtx never cancelled -> compensator was deregistered and never ran.
	if got := count.Load(); got != 0 {
		t.Fatalf("compensator fired %d times on the normal path, want 0", got)
	}
	n, _ := s.AuditCount(context.Background(), "trace-ok")
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}
```

## Review

The path is correct when a side effect owed by the request survives the request's
own cancellation. `TestAuditSurvivesCancellation` cancels `reqCtx` up front, yet
the audit row still lands — because `Audit` writes on `context.WithoutCancel(reqCtx)`
plus a fresh timeout, not on the dead request context.
`TestWithoutCancelCarriesValues` pins the two halves of that guarantee: the
detached context is not cancelled, and it still carries the trace id.
`TestAfterFuncFiresOnceOnCancel` and `TestStopDeregistersOnNormalCompletion` prove
the compensator semantics: it fires exactly once on cancellation, and `stop()`
prevents it on the normal path (returning `true`). The `sync.WaitGroup` is
essential — the callback runs on its own goroutine, so the test synchronizes on it
rather than sleeping. The failure mode all of this prevents is the quietest and
worst kind: an audit or outbox write that disappears whenever a client hangs up.
Run `-race`.

## Resources

- [context: WithoutCancel](https://pkg.go.dev/context#WithoutCancel) — a child that keeps values but ignores the parent's cancellation.
- [context: AfterFunc](https://pkg.go.dev/context#AfterFunc) — run a compensating action when a context is done, with a `stop` to deregister.
- [context: WithValue](https://pkg.go.dev/context#WithValue) — carrying a request-scoped trace id.
- [Go docs: Canceling in-progress operations](https://go.dev/doc/database/cancel-operations) — how context cancellation reaches database operations.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../11-graceful-shutdown-with-context/00-concepts.md](../11-graceful-shutdown-with-context/00-concepts.md)
