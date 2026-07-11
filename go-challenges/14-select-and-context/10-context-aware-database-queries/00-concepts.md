# Context-Aware Database Queries — Concepts

A request context is not a convenience argument you thread through for tidiness.
It is the single lifetime budget for everything the request touches, and the
database is where that budget most often goes to die. A senior backend engineer
treats the context as something that must reach the driver *unbroken*, because a
query that ignores cancellation does not just waste a goroutine — it keeps a
pooled connection, a lock, and a server-side transaction alive after the client
has already walked away. That is the exact shape of a real incident: a slow
downstream dependency turns a handful of abandoned queries into pool exhaustion,
and pool exhaustion turns one slow dependency into a full outage. This chapter is
about making the context arrive at the driver every time, classifying *why* a
query died so you can respond correctly, and making sure the side effects you
still owe survive a cancellation. Read this file once; each of the ten exercises
that follow is independent and self-contained, and everything you need to reason
through them is here.

## The model

### The `*Context` variants are the only safe methods

`database/sql` gives you two families of methods. `db.Query`, `db.Exec`,
`db.Begin`, and `db.Prepare` call `context.Background()` internally. They cannot
see your request's deadline, so cancellation never reaches the driver: the HTTP
handler times out and returns to the client while the database keeps grinding on
a query nobody is waiting for. The `*Context` family — `QueryContext`,
`ExecContext`, `QueryRowContext`, `BeginTx`, `PingContext`, `PrepareContext`, and
`Stmt.QueryContext` — carries the deadline into the driver. When the context
fires, a well-behaved driver issues an actual cancel to the server (for Postgres
this is a `CancelRequest` on a side connection; for MySQL a `KILL QUERY`), so the
server stops working and the connection returns to the pool. Every query you
write in production should use the context variant. There is no "small query"
exception: the small query is the one holding a connection when the pool is
already at its limit.

### The real cost is the connection pool, not the goroutine

It is tempting to think an ignored cancellation just leaks one goroutine for a
few seconds. The expensive resource is the pooled `*sql.Conn`. An in-flight query
holds that connection plus server-side state: row-level locks, a
multi-version-concurrency row version pinned open, temp files for a sort, a
transaction snapshot. `SetMaxOpenConns(n)` caps how many connections exist at
once. If cancelled queries keep their connections checked out past the client's
give-up point, and requests keep arriving, every connection ends up parked on
work no one wants. New requests block waiting to acquire a connection, their own
deadlines expire, and the failure cascades outward from the database to every
handler that touches it. This is why "just use the context variant" is an
availability requirement, not a style preference.

### Sentinel translation at the boundary

`sql.ErrNoRows` is the driver-independent sentinel that `Row.Scan` returns when a
`QueryRowContext` matched nothing. It is a `database/sql` concept, not a driver
message, which is exactly why you assert it with `errors.Is(err, sql.ErrNoRows)`
and never by string-matching `"sql: no rows in result set"`. A repository should
translate that sentinel *once*, at its boundary, into a domain sentinel:

```
if errors.Is(err, sql.ErrNoRows) {
    return User{}, fmt.Errorf("user %d: %w", id, ErrNotFound)
}
```

Callers then assert `errors.Is(err, ErrNotFound)` against your domain error and
never learn that a SQL database exists underneath. The other sentinel worth
knowing is `sql.ErrTxDone`, returned by `Commit` or `Rollback` on a transaction
that is already finished — which is the entire reason `defer tx.Rollback()` is a
safe no-op after a successful commit.

### Transaction lifetime is tied to the context

`BeginTx(ctx, opts)` binds the transaction to `ctx`. If `ctx` is cancelled while
the transaction is open, the driver rolls it back and every subsequent operation
on the `*sql.Tx` returns an error. The correct shape is fixed and worth
memorizing:

```
tx, err := db.BeginTx(ctx, opts)
if err != nil { return err }
defer tx.Rollback()          // safe no-op after Commit
// ... ExecContext/QueryRowContext, all on the SAME ctx ...
return tx.Commit()
```

The deferred `Rollback` is the guard rail: it covers every early return, every
error path, and every panic between `BeginTx` and `Commit`. After a successful
`Commit`, that deferred `Rollback` runs, sees the transaction is done, and
returns `sql.ErrTxDone`, which you ignore. The one non-negotiable rule inside the
block is that every query uses the *same* `ctx` the transaction was begun with.
Passing a different or `Background` context to a query inside the transaction
breaks cancellation for the transaction body while leaving the rest of it
cancellable — an inconsistency that is very hard to debug later.

### `TxOptions` — isolation and read-only

`BeginTx` takes `*sql.TxOptions{Isolation, ReadOnly}`. `Isolation` requests a
level such as `sql.LevelSerializable` or `sql.LevelReadCommitted`; the driver
enforces whether it is supported. `ReadOnly: true` marks a transaction as
read-only — both a correctness guard (writes will be rejected) and a routing hint
some deployments use to send the transaction to a read replica. A `nil` options
argument means the driver's default isolation.

### Deadline propagation and per-call budgets

A request-scoped deadline is a *budget*, and a budget is meant to be split, not
handed whole to the first dependency. If your handler has 200ms and forwards that
entire 200ms context to a single query, one slow query can consume the whole
request and leave nothing for the response encode, the cache write, or the second
query. The tool is `context.WithTimeout(parent, perCallMax)`: it derives a child
whose effective deadline is the *earlier* of `perCallMax` from now and the
parent's own deadline. Reading `parent.Deadline()` lets you compute the remaining
time with `time.Until(dl)` and skip work you already cannot finish — returning a
fast, honest "out of budget" instead of starting a query that is guaranteed to be
cancelled. Every derived context's `CancelFunc` must be called (a `defer cancel()`
right after the `WithTimeout`) or you leak the timer until the parent is done.

### Cancellation *cause* is how you tell why a query died

`ctx.Err()` is deliberately coarse: it returns only `context.Canceled` or
`context.DeadlineExceeded`. That is not enough to act on. Was it your own
server-side query budget that expired (retry might help)? Did the client
disconnect (retrying is pointless — nobody is listening; respond 499)? Are you
draining for shutdown (respond 503 and stop accepting work)? Go 1.21 added
*causes* to answer this. `context.WithCancelCause(parent)` returns a
`CancelCauseFunc` you call with a specific error; `context.WithTimeoutCause` and
`context.WithDeadlineCause` attach a cause to a timeout. `context.Cause(ctx)`
returns that specific error, falling back to `ctx.Err()` when there is no cause
(and to `context.Canceled` when `WithCancelCause` was called with a `nil` cause).
You define sentinel causes — `errClientGone`, `errServerBudget`, `errShutdown` —
and classify on `context.Cause(ctx)` to pick the response.

### Retry must be context-aware and error-aware

A retry loop is where a good intention becomes a self-inflicted outage. Two rules
keep it honest. First, retry only *transient* errors — a dropped connection, a
serialization failure, a deadlock victim. Never retry `sql.ErrNoRows` (the row
will not appear because you asked again) and never retry a validation or domain
error (the input is still invalid). Second, stop the instant the context is done.
Check `ctx.Err()` before every attempt, and — this is the subtle part — do the
backoff wait inside a `select` on `ctx.Done()` versus a timer, never
`time.Sleep`. A `time.Sleep(backoff)` blindly sleeps through the whole delay even
though the request was cancelled a microsecond after it started; a `select` wakes
the moment the context fires. When you do give up, join the last error with
`ctx.Err()` so the caller sees both the driver failure and the cancellation.

### Detached cleanup for side effects you still owe

When the request context is cancelled, some work still has to happen: an audit
row, an outbox insert, releasing a lock you took. Running that cleanup on the
just-cancelled context fails instantly with the same error, and the side effect
is silently lost — a real bug that shows up as missing audit trails whenever a
client disconnects mid-write. Two Go 1.21 tools solve it. `context.WithoutCancel(parent)`
returns a child that keeps the parent's *values* (trace id, request id) but is
immune to the parent's cancellation; give it a fresh short `WithTimeout` so the
detached write is still bounded. `context.AfterFunc(ctx, f)` registers `f` to run
in its own goroutine when `ctx` becomes done, and returns a `stop` function that
deregisters it — call `stop()` on the normal-completion path so the compensating
action fires only on actual cancellation.

### Pool and probe hygiene

Four knobs bound the pool: `SetMaxOpenConns` (hard ceiling on open connections),
`SetMaxIdleConns` (how many to keep warm), `SetConnMaxLifetime` (recycle a
connection after this age, which matters behind a load balancer that rotates
backends), and `SetConnMaxIdleTime` (close connections idle this long). Leave the
pool unbounded and a slow database grows connections without limit until
something upstream falls over. The matching rule for health checks: a readiness
probe must call `PingContext` with *its own* short timeout, never the unbounded
`Ping`. A wedged database should fail the probe fast so the load balancer pulls
the instance out of rotation; a `Ping` with no deadline hangs the health endpoint
and hides the fault instead of surfacing it.

### Iterating rows correctly

After a `QueryContext` loop you must do two things the compiler will not remind
you about. First, always `defer rows.Close()` — a `*sql.Rows` left open holds its
connection, and leaked open result sets are a slow-motion pool leak. Second,
check `rows.Err()` *after* the `for rows.Next()` loop: a cancellation or scan
error that happens mid-iteration does not surface from the final `Next()`
returning false (which is indistinguishable from "no more rows") — it surfaces
from `rows.Err()`. In a manual key-by-key loop, pre-check `ctx.Err()` at the top
of each iteration so a cancelled batch short-circuits with one progress-bearing
error ("aborted after 3/20") instead of producing N identical driver-internal
cancellation errors.

## Common Mistakes

### Using the non-context methods

Wrong: `db.Query(sql, args...)`, `db.Exec(...)`, `db.Begin()`, `db.Prepare(...)`.
The query runs under `context.Background()`; the request deadline never reaches
the driver, so the HTTP layer times out while the database keeps working and the
connection stays checked out. Fix: always use the `*Context` variant —
`QueryContext`, `ExecContext`, `BeginTx`, `PrepareContext`.

### Forgetting `defer tx.Rollback()` after `BeginTx`

Wrong: begin a transaction, then rely on reaching `Commit`. Any early return or
panic leaks the transaction and its connection until the driver reaps it. Fix:
`defer tx.Rollback()` on the line after `BeginTx`. It is a no-op after `Commit`
(returns `sql.ErrTxDone`), so it is always safe to leave in place.

### Passing a different context to queries inside a transaction

Wrong: `tx, _ := db.BeginTx(ctx, nil)` then `tx.ExecContext(context.Background(), ...)`.
The transaction is bound to `ctx` but its body ignores cancellation. Fix: use the
same `ctx` for every operation from `BeginTx` through `Commit`.

### String-comparing errors

Wrong: `if err.Error() == "sql: no rows in result set"`. Brittle across drivers
and versions, and it defeats error wrapping. Fix:
`errors.Is(err, sql.ErrNoRows)` at the boundary, then map it to a domain sentinel
wrapped with `%w`.

### Treating `ctx.Err()` as the reason

Wrong: reading `ctx.Err()` and assuming `DeadlineExceeded` means "the server was
slow". It does not distinguish a server-side timeout from a client disconnect
from a shutdown drain. Fix: attach causes with `WithCancelCause` /
`WithTimeoutCause` and classify on `context.Cause(ctx)`.

### Retrying after cancellation

Wrong: a retry loop that only checks whether the error is transient, so it keeps
hammering the database after the user or an upstream deadline already said stop.
Also retrying `sql.ErrNoRows` or a validation error. Fix: check `ctx.Err()`
before every attempt and abort; classify errors so only transient ones retry.

### Sleeping through backoff

Wrong: `time.Sleep(backoff)` between attempts. A request cancelled at the start of
the sleep still wastes the entire delay. Fix:
`select { case <-ctx.Done(): return ...; case <-timer.C: }` so cancellation wakes
the wait immediately.

### Ignoring `rows.Err()` and leaking `rows`

Wrong: loop with `for rows.Next()`, use the values, return — never checking
`rows.Err()` and never closing. A mid-iteration cancellation is lost and the
connection is pinned. Fix: `defer rows.Close()` and check `rows.Err()` after the
loop.

### Running cleanup on the cancelled context

Wrong: the request context is cancelled, and you write the audit row on that same
context — it fails instantly and the audit is lost. Fix:
`context.WithoutCancel(reqCtx)` plus a fresh `WithTimeout` for the detached write.

### Unbounded pool or unbounded `Ping`

Wrong: never calling `SetMaxOpenConns`, and using `db.Ping()` in a health check.
A wedged database grows connections without limit and hangs the probe, so the
load balancer cannot detect the fault. Fix: bound the pool and use
`PingContext` with its own short timeout.

Next: [01-context-aware-kv-store.md](01-context-aware-kv-store.md)
