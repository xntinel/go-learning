# Exercise 3: Worker Client Lifecycle, Queue Isolation, and Error Telemetry

This is the worker-process side, and it mirrors the real operational work of
running a job queue in production: a River client with isolated queues as
bulkheads, an error handler that turns failures and panics into telemetry (and can
promote a class of failure to permanent), a subscription for observability, and a
graceful shutdown that drains in-flight jobs on deploy instead of abandoning them
to a `SIGKILL`.

This module is fully self-contained and imports River, `pgx`, and testcontainers,
so gate it with `GOFLAGS=-mod=mod`. Its tests start a real client against Postgres
and use `Subscribe` to await outcomes deterministically; they skip cleanly when
Docker is absent.

## What you'll build

```text
lifecycle/                 independent module: example.com/lifecycle
  go.mod                   go 1.24; requires riverqueue/river, jackc/pgx/v5, testcontainers
  workers.go               NotifyArgs/ReportArgs/FlakyArgs/PanicArgs + workers; TelemetryHandler; NewWorkerClient
  cmd/
    demo/
      main.go              start, enqueue, await via Subscribe, graceful Stop with signal handling
  lifecycle_test.go        error handler, panic->cancelled, queue isolation, graceful drain
```

- Files: `workers.go`, `cmd/demo/main.go`, `lifecycle_test.go`.
- Implement: four job types across two isolated queues (`notifications` with many workers, `reports` with one), a `Workers` bundle built with `NewWorkers`/`AddWorker`, a `TelemetryHandler` implementing `river.ErrorHandler` with atomic counters that can `SetCancelled` panics, and `NewWorkerClient` wiring `Queues`, `Workers`, and `ErrorHandler`.
- Test: a failing job increments `HandleError` and its `JobRow` carries the error; a panicking job increments `HandlePanic` and, with `SetCancelled`, ends `cancelled`; a fast job in one queue completes first despite a saturated slow queue; `Stop` drains an in-flight job to completion.
- Verify: `GOFLAGS=-mod=mod go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
go get github.com/riverqueue/river
go get github.com/riverqueue/river/riverdriver/riverpgxv5
go get github.com/riverqueue/river/rivermigrate
go get github.com/riverqueue/river/rivertype
go get github.com/jackc/pgx/v5
go get github.com/testcontainers/testcontainers-go
go get github.com/testcontainers/testcontainers-go/modules/postgres
```

### Queues are bulkheads, not decoration

The client's `Config.Queues` maps a queue name to a `QueueConfig{MaxWorkers}`.
Each queue gets its own pool of worker goroutines. Here `notifications` gets five
and `reports` gets one. The point is isolation: a burst of slow report jobs can
saturate the single `reports` slot and back up *that* queue without touching the
five `notifications` slots, so a latency-sensitive notify still runs immediately.
If every job type shared `river.QueueDefault` behind one `MaxWorkers`, a report
backlog would starve notifications — the classic head-of-line blocking a bulkhead
prevents. A job lands in a queue via its args' `InsertOpts().Queue`, so
`ReportArgs` routes to `reports` and `NotifyArgs` to `notifications` with no
per-insert bookkeeping.

### Registering workers: the Kind contract, again

A processing client needs a `Workers` bundle. `river.NewWorkers()` creates it and
`river.AddWorker(workers, &W{})` registers each worker under the `Kind()` of its
args type. This is the other half of the contract from Exercise 1: the kind a
worker registers must equal the kind used at insert, or the row is never claimed.
A client built with no `Workers`, or started with no `Queues`, is insert-only and
processes nothing — a configuration mistake, not a River bug.

### The error handler is your telemetry seam

`Config.ErrorHandler` receives every failure. `HandleError(ctx, jobRow, err)`
fires on each errored attempt (not only the last), so it is where you increment a
failure metric or forward to an error tracker; the `*rivertype.JobRow` carries the
kind, the attempt, and the accumulated `Errors` slice. `HandlePanic(ctx, jobRow,
panicVal, trace)` fires when a `Work` panics — River recovers it so one bad job
does not crash the process — and it hands you the panic value and stack trace.
Either handler may return `&river.ErrorHandlerResult{SetCancelled: true}` to
promote that failure to permanent: instead of becoming `retryable`, the job goes
straight to `cancelled`. That is how you encode "any job that panics like this is
not worth retrying". The `TelemetryHandler` below counts both with atomic counters
(the handlers run on worker goroutines, so the counters must be concurrency-safe)
and cancels panics when configured to.

### Subscribe: synchronize without sleeping

`Client.Subscribe(kinds...)` returns a channel of `*river.Event` and a cancel
function. Awaiting `EventKindJobCompleted`/`JobFailed`/`JobCancelled` is how both
the demo and the tests know an outcome happened without polling the database or
sleeping an arbitrary interval. Always call the returned cancel function when
done, or the subscription leaks.

Create `workers.go`:

```go
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

// ErrFlaky is the sentinel a FlakyWorker returns to exercise HandleError.
var ErrFlaky = errors.New("flaky failure")

// NotifyArgs is a fast, latency-sensitive job on the notifications queue.
type NotifyArgs struct {
	UserID int64 `json:"user_id"`
}

func (NotifyArgs) Kind() string { return "notify" }
func (NotifyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "notifications"}
}

type NotifyWorker struct {
	river.WorkerDefaults[NotifyArgs]
}

func (w *NotifyWorker) Work(_ context.Context, _ *river.Job[NotifyArgs]) error { return nil }

// ReportArgs is a slow bulk job on the reports queue. WorkFor simulates the work
// so tests can control timing.
type ReportArgs struct {
	Name    string        `json:"name"`
	WorkFor time.Duration `json:"work_for"`
}

func (ReportArgs) Kind() string { return "report" }
func (ReportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "reports"}
}

type ReportWorker struct {
	river.WorkerDefaults[ReportArgs]
}

// Work honors ctx so that Stop can drain or cancel it.
func (w *ReportWorker) Work(ctx context.Context, job *river.Job[ReportArgs]) error {
	select {
	case <-time.After(job.Args.WorkFor):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FlakyArgs always fails transiently, to drive HandleError.
type FlakyArgs struct {
	Label string `json:"label"`
}

func (FlakyArgs) Kind() string { return "flaky" }
func (FlakyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "notifications", MaxAttempts: 3}
}

type FlakyWorker struct {
	river.WorkerDefaults[FlakyArgs]
}

func (w *FlakyWorker) Work(_ context.Context, job *river.Job[FlakyArgs]) error {
	return fmt.Errorf("flaky %s: %w", job.Args.Label, ErrFlaky)
}

// PanicArgs panics, to drive HandlePanic.
type PanicArgs struct {
	Reason string `json:"reason"`
}

func (PanicArgs) Kind() string { return "panic" }
func (PanicArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "notifications", MaxAttempts: 3}
}

type PanicWorker struct {
	river.WorkerDefaults[PanicArgs]
}

func (w *PanicWorker) Work(_ context.Context, job *river.Job[PanicArgs]) error {
	panic("boom: " + job.Args.Reason)
}

// TelemetryHandler records failure and panic telemetry. Its counters are atomic
// because the handlers run concurrently on worker goroutines.
type TelemetryHandler struct {
	errors       atomic.Int64
	panics       atomic.Int64
	cancelPanics bool

	mu          sync.Mutex
	lastErrKind string
}

func NewTelemetryHandler(cancelPanics bool) *TelemetryHandler {
	return &TelemetryHandler{cancelPanics: cancelPanics}
}

func (h *TelemetryHandler) Errors() int64 { return h.errors.Load() }
func (h *TelemetryHandler) Panics() int64 { return h.panics.Load() }

func (h *TelemetryHandler) LastErrorKind() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErrKind
}

func (h *TelemetryHandler) HandleError(_ context.Context, job *rivertype.JobRow, _ error) *river.ErrorHandlerResult {
	h.errors.Add(1)
	h.mu.Lock()
	h.lastErrKind = job.Kind
	h.mu.Unlock()
	return nil
}

func (h *TelemetryHandler) HandlePanic(_ context.Context, _ *rivertype.JobRow, _ any, _ string) *river.ErrorHandlerResult {
	h.panics.Add(1)
	// Promote panics to permanent cancellation when configured.
	return &river.ErrorHandlerResult{SetCancelled: h.cancelPanics}
}

// BuildWorkers registers every worker under its Kind.
func BuildWorkers() *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &NotifyWorker{})
	river.AddWorker(workers, &ReportWorker{})
	river.AddWorker(workers, &FlakyWorker{})
	river.AddWorker(workers, &PanicWorker{})
	return workers
}

// NewWorkerClient builds a processing client with two isolated queues, the
// worker bundle, and the telemetry error handler.
func NewWorkerClient(pool *pgxpool.Pool, handler river.ErrorHandler) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			"notifications": {MaxWorkers: 5},
			"reports":       {MaxWorkers: 1},
		},
		Workers:      BuildWorkers(),
		ErrorHandler: handler,
	})
}

// Migrate brings River's schema up to date.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("new migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
```

### The runnable demo: full lifecycle with signal handling

The demo is the production shape of a worker process. It derives a context from
`signal.NotifyContext` so `SIGINT`/`SIGTERM` cancels it, subscribes before
starting, `Start`s the client, enqueues one fast notify and one slow report, waits
for both completion events, then `Stop`s with a bounded shutdown deadline so
in-flight work drains. Point it at any Postgres.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"example.com/lifecycle"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("set DATABASE_URL to a Postgres connection string")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := lifecycle.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	client, err := lifecycle.NewWorkerClient(pool, lifecycle.NewTelemetryHandler(false))
	if err != nil {
		log.Fatalf("new client: %v", err)
	}

	sub, cancelSub := client.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()

	if err := client.Start(ctx); err != nil {
		log.Fatalf("start: %v", err)
	}

	if _, err := client.Insert(ctx, lifecycle.NotifyArgs{UserID: 1}, nil); err != nil {
		log.Fatalf("insert notify: %v", err)
	}
	if _, err := client.Insert(ctx, lifecycle.ReportArgs{Name: "daily", WorkFor: 200 * time.Millisecond}, nil); err != nil {
		log.Fatalf("insert report: %v", err)
	}

	for range 2 {
		ev := <-sub
		fmt.Printf("event: %s kind=%s\n", ev.Kind, ev.Job.Kind)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Stop(shutdownCtx); err != nil {
		log.Fatalf("stop: %v", err)
	}
	fmt.Println("stopped cleanly")
}
```

Run it:

```bash
docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=secret --name pg postgres:16-alpine
export DATABASE_URL='postgres://postgres:secret@localhost:5432/postgres?sslmode=disable'
go run ./cmd/demo
```

Expected output:

```
event: job_completed kind=notify
event: job_completed kind=report
stopped cleanly
```

The notify completes first: it runs in the five-worker `notifications` queue while
the slower report runs in the single-worker `reports` queue, and neither waits on
the other.

### Tests

The tests start a real client and drive each operational property. `awaitEvent`
reads one event with a timeout, so a hang fails fast instead of blocking the
suite. `TestErrorHandlerOnFailure` enqueues a flaky job, awaits its `JobFailed`
event, and asserts the handler counted the error and the `JobRow` recorded it.
`TestPanicSetCancelled` uses a handler configured to cancel panics and asserts the
panicking job ends `cancelled`, not `retryable`. `TestQueueIsolation` saturates
the single-worker `reports` queue with two slow reports, enqueues a fast notify,
and asserts the notify's completion arrives first — proof the queues are bulkheads.
`TestGracefulDrain` starts an in-flight report, calls `Stop`, and asserts the job
drained to `completed` rather than being abandoned.

Create `lifecycle_test.go`:

```go
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	if url := os.Getenv("DATABASE_URL"); url != "" {
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Fatalf("connect DATABASE_URL: %v", err)
		}
		t.Cleanup(pool.Close)
		return pool
	}

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("app"),
		postgres.WithUsername("app"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("no DATABASE_URL and Docker unavailable: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func startedClient(t *testing.T, pool *pgxpool.Pool, handler river.ErrorHandler) *river.Client[pgx.Tx] {
	t.Helper()
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	client, err := NewWorkerClient(pool, handler)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(ctx)
	})
	return client
}

func awaitEvent(t *testing.T, sub <-chan *river.Event) *river.Event {
	t.Helper()
	select {
	case ev := <-sub:
		return ev
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for a river event")
		return nil
	}
}

func TestErrorHandlerOnFailure(t *testing.T) {
	pool := setupPool(t)
	handler := NewTelemetryHandler(false)
	client := startedClient(t, pool, handler)

	sub, cancel := client.Subscribe(river.EventKindJobFailed)
	defer cancel()

	if _, err := client.Insert(context.Background(), FlakyArgs{Label: "invoice"}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ev := awaitEvent(t, sub)
	if ev.Job.Kind != "flaky" {
		t.Fatalf("failed job kind = %s; want flaky", ev.Job.Kind)
	}
	if handler.Errors() == 0 {
		t.Fatal("HandleError was not called")
	}
	if handler.LastErrorKind() != "flaky" {
		t.Fatalf("LastErrorKind = %q; want flaky", handler.LastErrorKind())
	}
	if len(ev.Job.Errors) == 0 {
		t.Fatal("JobRow.Errors is empty; the failure was not recorded on the row")
	}
}

func TestPanicSetCancelled(t *testing.T) {
	pool := setupPool(t)
	handler := NewTelemetryHandler(true) // promote panics to cancelled
	client := startedClient(t, pool, handler)

	sub, cancel := client.Subscribe(river.EventKindJobCancelled, river.EventKindJobFailed)
	defer cancel()

	if _, err := client.Insert(context.Background(), PanicArgs{Reason: "nil deref"}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ev := awaitEvent(t, sub)
	if ev.Kind != river.EventKindJobCancelled {
		t.Fatalf("event kind = %s; want %s (SetCancelled)", ev.Kind, river.EventKindJobCancelled)
	}
	if ev.Job.State != rivertype.JobStateCancelled {
		t.Fatalf("job state = %s; want cancelled", ev.Job.State)
	}
	if handler.Panics() == 0 {
		t.Fatal("HandlePanic was not called")
	}
}

func TestQueueIsolation(t *testing.T) {
	pool := setupPool(t)
	client := startedClient(t, pool, NewTelemetryHandler(false))

	sub, cancel := client.Subscribe(river.EventKindJobCompleted)
	defer cancel()

	ctx := context.Background()
	// Saturate the single-worker reports queue.
	for _, name := range []string{"r1", "r2"} {
		if _, err := client.Insert(ctx, ReportArgs{Name: name, WorkFor: 600 * time.Millisecond}, nil); err != nil {
			t.Fatalf("insert report: %v", err)
		}
	}
	// A fast notify in its own queue must not wait on the report backlog.
	if _, err := client.Insert(ctx, NotifyArgs{UserID: 1}, nil); err != nil {
		t.Fatalf("insert notify: %v", err)
	}

	first := awaitEvent(t, sub)
	if first.Job.Kind != "notify" {
		t.Fatalf("first completion = %s; want notify (queues are not isolated)", first.Job.Kind)
	}
}

func TestGracefulDrain(t *testing.T) {
	pool := setupPool(t)
	client := startedClient(t, pool, NewTelemetryHandler(false))
	ctx := context.Background()

	res, err := client.Insert(ctx, ReportArgs{Name: "drain", WorkFor: 400 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Let the worker claim and begin the job so Stop must drain it.
	time.Sleep(100 * time.Millisecond)

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	var state string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM river_job WHERE id = $1`, res.Job.ID,
	).Scan(&state); err != nil {
		t.Fatalf("query state: %v", err)
	}
	if state != string(rivertype.JobStateCompleted) {
		t.Fatalf("drained job state = %s; want completed", state)
	}
}

func ExampleNotifyArgs_Kind() {
	fmt.Println(NotifyArgs{}.Kind(), NotifyArgs{}.InsertOpts().Queue)
	fmt.Println(ReportArgs{}.Kind(), ReportArgs{}.InsertOpts().Queue)
	// Output:
	// notify notifications
	// report reports
}
```

## Review

The worker process is correct when each operational property holds independently.
The error-handler test proves `HandleError` fires and the failure is persisted on
the `JobRow` (its `Errors` slice), which is what lets an SRE see *why* a job is
retrying. The panic test proves `HandlePanic` fires and that returning
`ErrorHandlerResult{SetCancelled: true}` promotes the failure to a terminal
`cancelled` instead of a retry loop over a deterministic panic. The isolation test
is the bulkhead proof: with the `reports` queue saturated at its single worker, a
notify in the five-worker queue still completes first; collapse both into one
queue and that assertion fails. The drain test proves `Stop` waits for in-flight
work — the report finishes and lands `completed` rather than being abandoned to the
rescuer.

The mistakes to avoid are the wiring ones. A client with a `Workers` bundle but no
`Queues`, or `Queues` but no `Workers`, processes nothing; a `Kind()` that differs
between insert and `AddWorker` leaves rows unclaimed. And graceful shutdown only
works if `Work` honors `ctx` — the `ReportWorker` selects on `ctx.Done()`, which is
what lets `Stop` (soft) drain and `StopAndCancel` (hard) abort; a `Work` that
ignores `ctx` blocks `Stop` until the timeout. Run the suite with `-race` against a
real Postgres (or let it skip); the atomic counters in `TelemetryHandler` are there
precisely because `-race` would flag a plain `int`.

## Resources

- [River docs: Error and panic handling](https://riverqueue.com/docs/error-handling) — `ErrorHandler`, `HandlePanic`, and `SetCancelled`.
- [River docs: Graceful shutdown](https://riverqueue.com/docs/graceful-shutdown) — `Stop` versus `StopAndCancel` and draining in-flight jobs.
- [River docs: Subscriptions](https://riverqueue.com/docs/subscriptions) — `Subscribe` and the event kinds.
- [River package reference](https://pkg.go.dev/github.com/riverqueue/river) — `Config`, `QueueConfig`, `NewWorkers`, `AddWorker`, and the client lifecycle methods.

---

Back to [02-worker-retry-policy.md](02-worker-retry-policy.md) | Next: [../10-dead-letter-and-retry-topologies/00-concepts.md](../10-dead-letter-and-retry-topologies/00-concepts.md)
