# Transactional Job Queues with River — Concepts

Every backend eventually needs to do work after a request returns: send the
welcome email after the account is created, charge the card after the order is
placed, rebuild a projection after a write. The moment you push that work out of
the request path you have an async job, and you have to answer one uncomfortable
question: *is the job guaranteed to exist if and only if the business fact that
justifies it committed?* The two dominant answers in production both have sharp
edges, and River exists precisely to remove them.

If your jobs live in an external broker — SQS, Kafka, Redis — that broker cannot
participate in your Postgres transaction. So you are forced into one of two bad
orderings. Publish-then-commit: you enqueue the job, then commit the domain
write, and if the process dies in between you have a job whose business fact
never happened (a welcome email for an account that does not exist), or worse,
the commit fails and the job runs anyway. Commit-then-publish: you commit the
domain write, then publish, and if you die in between the fact is committed but
the job is lost. The standard fix for that second ordering is the transactional
outbox (chapter 06): write the job as a row in the *same* transaction as the
domain change, then a relay process reads that table and republishes to the
broker. The outbox is correct, but it is machinery you now own — a table, a
relay, a poller, at-least-once redelivery, and consumer-side deduplication
(chapter 07).

The other common answer is to skip the broker entirely and hand-roll a queue in
the database you already have: a `jobs` table and a worker loop that runs
`SELECT ... FOR UPDATE SKIP LOCKED` to claim rows. This is the right instinct —
the job and the domain write can share a transaction because they share a
database — but a home-grown version re-implements retries, exponential backoff,
uniqueness, priorities, scheduled jobs, leader election for maintenance, and
table pruning, and it re-implements them badly the first several times.

River is the pragmatic middle ground between "just use the DB you already have"
and "stand up Kafka". It is a Postgres-backed job queue for Go (built on `pgx`)
that gives you the outbox's core guarantee for free: because a job is just a row
in `river_job`, you can insert it in the same `pgx.Tx` as your domain mutation.
That single fact — one insert, one transaction — is the entire reason to reach
for River. No outbox table, no relay, no dual write, no change-data-capture.

## Enqueue atomicity versus execution semantics

This is the one distinction you must hold in your head for the whole lesson, and
it is where most River bugs come from.

*Enqueue is exactly-once.* `Client.InsertTx(ctx, tx, args, opts)` writes the job
row inside the transaction you pass it. The job is created if and only if that
transaction commits. Roll back and the job is gone with the domain change; commit
and both are durable together. There is no window in which one exists without the
other. This is the guarantee an outbox gives you, collapsed into a single insert.

*Execution is at-least-once.* Once the job is committed and available, a worker
claims it, runs your `Work` method, and marks it complete. But a worker is a
process, and processes crash. If a worker dies after your `Work` has produced its
side effect (the card was charged, the email was sent) but before River could
mark the job completed, River will — correctly — re-run the job on another worker,
because from the queue's point of view the job never finished. River cannot know
your side effect already happened.

The consequence is not optional: **your `Work` method must be idempotent.**
River guarantees the job is *enqueued* exactly once; it guarantees the job is
*executed* at least once. The gap between "at least once" and "exactly once" on
the execution side is yours to close, with the same tools chapter 07 used for any
at-least-once consumer — an idempotency key threaded to the downstream (so the
payment gateway dedupes the charge), or an inbox/dedupe row keyed by the job's
identity. River collapses the outbox *producer* side into one insert; it does not
remove the need for consumer idempotency.

## Kind is the contract between insert and worker

A job type is defined by an args struct that implements `river.JobArgs`, whose
only method is `Kind() string`. That string is the wire contract. When you
insert, River serializes the args to JSON and stores the row with that kind. When
a worker starts, River routes each claimed row to the worker registered for its
kind. If the kind used at insert does not match a kind registered via
`river.AddWorker`, the row is inserted but never worked — it sits in the queue
unclaimed. `Kind()` must therefore be stable across deploys and identical on both
sides. Treat it like a database column name, not a display string: renaming it
strands every job already in the table under the old name.

Per-type defaults ride along by also implementing `JobArgsWithInsertOpts` — an
`InsertOpts() river.InsertOpts` method that sets the job's default `Queue`,
`MaxAttempts`, `Priority`, and uniqueness. Options passed explicitly to `Insert`
/`InsertTx` override the per-type defaults, which override the client defaults.

## Producer-side uniqueness

`river.InsertOpts.UniqueOpts` gives you idempotent *enqueue* on top of atomic
enqueue. `UniqueOpts{ByArgs: true, ByPeriod: 24 * time.Hour}` tells River that
two jobs of the same kind with identical args inserted within a 24-hour window
are the same job — the second insert is skipped and returns the existing job with
`InsertResult.UniqueSkippedAsDuplicate == true`, no error. This is deduplication
at the *producer*, and it is different from consumer idempotency: it stops you
from creating a duplicate row (two "rebuild projection X" jobs from two concurrent
requests), but it does nothing about a single job being *executed* twice after a
crash. You often want both. `UniqueOpts` can also key on `ByQueue`, `ByState`
(the set of job states over which uniqueness holds), and a period; note
`ByPeriod` is a `time.Duration`, not a boolean.

## The retry taxonomy

What your `Work` method *returns* is the entire retry API. There is no separate
"retry this" call; the return value classifies the outcome, and getting the
taxonomy right is the difference between a self-healing queue and one that either
retries forever or discards recoverable work.

- **`return nil`** — success. The job moves to `completed` and is never run again.
- **`return err` (a plain error)** — a transient failure: the downstream is down,
  a connection dropped, a 503 came back. River moves the job to `retryable` and
  reschedules it with backoff. This consumes one attempt. When `Attempt` reaches
  `MaxAttempts`, the job is `discarded` (dead-lettered, in chapter 10's terms).
  The default backoff is `attempt^4` seconds plus up to ten percent jitter, which
  climbs from about a second to roughly three weeks by the default 25th attempt;
  you can override it per worker with `NextRetry`.
- **`return river.JobCancel(err)`** — a permanent failure: the input is
  malformed, the downstream returned a hard 404 or "card declined", nothing will
  fix it by trying again. This moves the job straight to `cancelled` with the
  wrapped error persisted, *regardless of how many attempts remain*. Returning a
  plain error here is a real bug: you burn 25 attempts over three weeks on work
  that was never going to succeed, and you delay the failure signal.
- **`return river.JobSnooze(d)`** — not a failure at all: the job is not ready
  yet (you are rate-limited, an upstream dependency has not produced its input,
  the account is temporarily locked). River reschedules the job after `d`
  *without consuming an attempt* — snooze does not count against `MaxAttempts`, so
  a job can snooze indefinitely. Using a plain error for rate-limit backoff is a
  common mistake: it burns the retry budget and can dead-letter a perfectly good
  job. Using `JobSnooze` for a genuine transient error is the opposite mistake:
  the job never advances toward the dead-letter it deserves.

Both special returns are detectable as concrete types — `*rivertype.JobCancelError`
(which also `Unwrap`s to the error you wrapped, so `errors.Is` against a sentinel
still works) and `*rivertype.JobSnoozeError` (whose exported `Duration` field
carries the snooze). That is exactly how River's executor classifies them via
`errors.As`, and it is how a unit test asserts your taxonomy without a database.

## Timeouts and bounding a single execution

A worker can override `Timeout(job) time.Duration` to bound one execution; the
context passed to `Work` is cancelled when it fires. A zero return means "no
timeout", which is dangerous — a `Work` that ignores `ctx` and blocks forever
holds a worker slot and, more importantly, blocks graceful shutdown. `Work` must
watch `ctx.Done()` (pass the ctx to every downstream call) so a timeout or a
shutdown actually stops it.

## Queues are bulkheads

A `river.Client` processes the queues named in its `Config.Queues` map, each with
its own `MaxWorkers`. This is not cosmetic. If you run every job type in
`river.QueueDefault` behind a single `MaxWorkers`, a burst of slow bulk jobs (a
nightly report export, a backfill) fills every slot and *starves* your
latency-sensitive jobs (the welcome email, the payment) behind them. Put slow
bulk work and fast interactive work in *separate* queues with separate worker
budgets, and one cannot exhaust the other. This is the bulkhead pattern applied
to job execution, and it is the single most valuable operational knob River gives
you.

An insert-only process (your API) constructs a client with no `Queues` and no
`Workers` and only ever calls `InsertTx`/`Insert` — it never processes anything,
by design. A worker process constructs a client *with* `Queues` and a `Workers`
bundle and calls `Start`. Starting a client with zero queues, or building a
worker `Config` with no `Workers`, and then expecting jobs to run, is a
configuration mistake, not a bug in River.

## Lifecycle: start, drain, and stop

`Client.Start(ctx)` launches the producers, workers, and maintenance goroutines
and returns immediately; the client runs until stopped. On deploy you must stop
it *gracefully*, or the orchestrator's `SIGKILL` abandons in-flight jobs (they
sit in `running` until River's rescue process notices and reschedules them,
adding latency and a spurious error). `Client.Stop(ctx)` initiates a soft stop:
it stops fetching new jobs and waits for in-flight jobs to finish (bounded by
`Config.SoftStopTimeout`), then returns. `Client.StopAndCancel(ctx)` is the hard
version: it cancels the context handed to running `Work` methods so they abort.
The production shape is `signal.NotifyContext` for `SIGINT`/`SIGTERM`, `Start`
under that context, and `Stop` with a bounded shutdown context in a `defer`. This
is also why `Work` must honor `ctx`: `Stop` can only drain jobs that respond to
cancellation.

## Error and panic telemetry

`Config.ErrorHandler` is where operational visibility lives. `HandleError(ctx,
jobRow, err)` is called every time a `Work` returns an error (each attempt, not
just the last), giving you the `*rivertype.JobRow` (kind, attempt, the recorded
`Errors` slice) to feed a metric or an error tracker. `HandlePanic(ctx, jobRow,
panicVal, trace)` is called when `Work` panics — River recovers the panic so one
bad job does not crash the worker process. Either handler can return
`&river.ErrorHandlerResult{SetCancelled: true}` to promote that failure to
permanent (moving the job to `cancelled` instead of `retryable`), which is how you
say "any job that panics like this is not worth retrying". Returning `nil` leaves
River's normal retry behavior intact.

For deterministic tests and live dashboards, `Client.Subscribe(kinds...)` returns
a channel of `*river.Event` for kinds like `EventKindJobCompleted` and
`EventKindJobFailed`. Awaiting an event is how you synchronize a test with a
worker without sleeping.

## Migrations are yours to run

River's tables (`river_job`, `river_leader`, `river_migration`, ...) do not
appear by magic. You run River's migrations, either with the `river migrate-up`
CLI or programmatically with `rivermigrate.New(driver, nil)` and
`migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)`. If you forget, or run
them out of band from your application migrations so they drift, `NewClient` /
`InsertTx` fails at runtime complaining the `river_job` table is missing. Own the
River migrations the same way you own your own schema migrations — in the same
ordered pipeline.

## Connection pool sizing

River shares your `*pgxpool.Pool`. Its workers, producers, and maintenance
routines all draw connections from it, on top of your application queries. Size
the pool for both, defer `pool.Close()`, and do not construct a fresh pool per
request. A pool starved by application traffic will stall River's polling; a pool
leaked (never closed) outlives the process's need for it and exhausts Postgres's
connection limit.

## Common Mistakes

### Using Insert instead of InsertTx inside a domain transaction

Wrong: perform the domain write in a `pgx.Tx`, then call `Client.Insert` (which
uses the pool, its own connection). The job is now committed independently of your
transaction — the exact dual-write / lost-job problem River exists to remove is
reintroduced.

Fix: call `Client.InsertTx(ctx, tx, args, opts)` with the *same* `tx` as the
domain write, so the job commits atomically with the business fact.

### Writing non-idempotent Work and assuming exactly-once execution

Wrong: charge a card or send an email in `Work` with no idempotency key,
believing River runs the job exactly once. Execution is at-least-once; a crash
after the side effect but before completion re-runs it, double-charging.

Fix: make `Work` idempotent — thread an idempotency key to the downstream, or
dedupe against an inbox row, so redelivery is a no-op.

### Returning a plain error for a permanent failure

Wrong: return `err` for a declined card or a hard 404. River retries it 25 times
over three weeks before discarding, and your failure signal is three weeks late.

Fix: return `river.JobCancel(err)` for known-permanent failures so the job is
cancelled immediately; reserve plain errors for genuinely transient faults.

### Confusing JobSnooze with a retry (and vice versa)

Wrong: return a plain error for rate-limit backoff. That consumes an attempt and
can dead-letter a healthy job. Or: return `JobSnooze` for a transient downstream
error, so the job snoozes forever and never dead-letters.

Fix: `JobSnooze(d)` for "not ready yet" (it does not consume an attempt); a plain
error for "tried and failed, retry with backoff".

### Forgetting to run River's migrations

Wrong: call `NewClient`/`InsertTx` against a database where River's migrations
never ran, or where they ran out of band and drifted. It fails at runtime with a
missing `river_job` table.

Fix: run `rivermigrate` up (or the `river migrate-up` CLI) in the same ordered
pipeline as your own schema migrations.

### Not stopping the client on shutdown

Wrong: let the process exit (or take a `SIGKILL`) without `Client.Stop`. In-flight
jobs are abandoned mid-run and only recovered later by River's rescuer, adding
latency and noise. Worse, a `Work` that ignores `ctx` blocks `Stop` forever.

Fix: `signal.NotifyContext` for `SIGINT`/`SIGTERM`, `Stop(ctx)` with a bounded
shutdown deadline, and a `Work` that honors context cancellation and has a
`Timeout`.

### One queue, one MaxWorkers, for everything

Wrong: run every job type in `QueueDefault` behind a single `MaxWorkers`. A burst
of slow bulk jobs fills every slot and starves latency-sensitive jobs.

Fix: isolate slow bulk work and fast interactive work into separate queues with
separate `MaxWorkers` budgets — a bulkhead so one class cannot starve the other.

### Kind mismatch between insert and worker

Wrong: insert with args whose `Kind()` is `"welcome_email"` but register a worker
for `"welcome-email"`. Jobs are inserted but never claimed; they pile up
unworked.

Fix: `Kind()` is the contract — keep it identical on both sides and stable across
deploys. Reuse the same args type for insert and worker.

### An insert-only client that you expect to work jobs

Wrong: build a `Config` with no `Workers` bundle, or start a client with no
`Queues`, and wait for jobs to run.

Fix: a processing client needs both a `Workers` bundle (via `NewWorkers` +
`AddWorker`) and at least one entry in `Queues`; an insert-only client has
neither and only enqueues.

### Leaking or under-sizing the pool

Wrong: construct a `*pgxpool.Pool` and never `Close` it, or reuse one pool sized
only for application queries while River also draws from it.

Fix: `defer pool.Close()`, and size the pool for application traffic plus River's
workers, producers, and maintenance connections.

Next: [01-transactional-enqueue.md](01-transactional-enqueue.md)
