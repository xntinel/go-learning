# Exercise 2: Worker Execution and Retry Taxonomy

Enqueue is exactly-once; execution is at-least-once. This exercise builds the
execution side: a River `Worker` whose `Work` method encodes a production retry
taxonomy in its return value, and whose body is idempotent so a redelivery after
a crash does not double-charge a card. The taxonomy — success, transient,
permanent, not-ready — is the entire retry API, and getting it right is the
difference between a self-healing queue and one that retries forever or discards
recoverable work.

This module is fully self-contained and imports River, `pgx`, and testcontainers,
so gate it with `GOFLAGS=-mod=mod`. The pure `Work` logic is unit-tested with no
database; the state-level behavior is checked with `rivertest.NewWorker` against a
real Postgres that skips when Docker is absent.

## What you'll build

```text
retrytaxonomy/             independent module: example.com/retrytaxonomy
  go.mod                   go 1.24; requires riverqueue/river, jackc/pgx/v5, testcontainers
  worker.go                ChargeCardArgs; ChargeCardWorker (Work/Timeout/NextRetry); Charger seam
  cmd/
    demo/
      main.go              run Work directly for each taxonomy branch; show idempotency
  retrytaxonomy_test.go    pure error-semantics tests + rivertest state-level tests
```

- Files: `worker.go`, `cmd/demo/main.go`, `retrytaxonomy_test.go`.
- Implement: `ChargeCardWorker.Work` returning `nil` on success, a plain error on transient failure, `river.JobCancel` on a permanent failure, and `river.JobSnooze` when rate-limited; a `Timeout` bound and a linear `NextRetry`; a `Charger` interface so `Work` is idempotent and unit-testable.
- Test: pure unit tests calling `Work` directly and asserting the returned error's semantics (`nil` / `errors.Is` a transient sentinel / `errors.As` a `*rivertype.JobCancelError` / `*rivertype.JobSnoozeError`); state-level tests via `rivertest.NewWorker` asserting the resulting job state per branch and that snooze preserved more retry budget than a plain error.
- Verify: `GOFLAGS=-mod=mod go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
go get github.com/riverqueue/river
go get github.com/riverqueue/river/riverdriver/riverpgxv5
go get github.com/riverqueue/river/rivertest
go get github.com/riverqueue/river/rivertype
go get github.com/jackc/pgx/v5
go get github.com/testcontainers/testcontainers-go
go get github.com/testcontainers/testcontainers-go/modules/postgres
```

### The return value is the retry API

River has no "retry this job" call. Whatever `Work` returns classifies the
outcome, and River acts on the classification:

- `nil` completes the job.
- a plain `error` marks it `retryable` and reschedules with backoff, consuming
  one attempt; at `MaxAttempts` it is discarded (dead-lettered).
- `river.JobCancel(err)` moves it straight to `cancelled` with `err` persisted,
  regardless of attempts remaining — for failures no retry can fix.
- `river.JobSnooze(d)` reschedules after `d` *without consuming an attempt* — for
  "not ready yet", like a rate limit.

The worker below charges a card. A declined card is permanent (retrying a valid
decline is pointless and delays the failure signal), so it returns `JobCancel`. A
gateway that is down is transient, so it returns a plain error and lets backoff
handle it. A rate-limit response is not a failure at all, so it returns
`JobSnooze` to back off without burning the retry budget. Encoding these three
downstream conditions into three different return values is the whole job of a
production `Work` method.

### Why Work must be idempotent, and how

Because execution is at-least-once, `Work` can run more than once for a single
job: a worker can charge the card, then crash before River records completion, and
another worker re-runs the job. If `Work` naively calls "charge $50" each time,
the customer is charged twice. The fix is not in River — it is an idempotency key
carried in the job args and passed to the gateway on every attempt. A correct
gateway records the key and treats a repeat as a no-op, so the second execution
charges nothing. The `Charger` interface is that seam: injecting it lets the tests
supply a gateway that records keys (proving idempotency) or one that returns a
fixed error (exercising each taxonomy branch), all without a network.

### Timeout and NextRetry

`Timeout(job)` bounds one execution; the `ctx` passed to `Work` is cancelled when
it fires, so a `Work` that threads `ctx` into the gateway call aborts promptly
instead of hanging a worker slot and blocking graceful shutdown. `NextRetry(job)`
overrides River's default `attempt^4` backoff; here it is a gentle linear schedule
(fifteen seconds times the attempt number) that is kinder to a recovering gateway
for the first few retries. Both are overrides of the defaults that
`river.WorkerDefaults` supplies, which is why the worker embeds it.

Create `worker.go`:

```go
package retrytaxonomy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"
)

// Sentinel errors from the payment gateway, each mapping to a taxonomy branch.
var (
	ErrCardDeclined = errors.New("card declined")        // permanent -> JobCancel
	ErrGatewayDown  = errors.New("payment gateway down") // transient -> plain error
	ErrRateLimited  = errors.New("rate limited")         // not ready -> JobSnooze
)

// Charger is the downstream payment gateway. The idempotency key makes a repeat
// charge for the same key a no-op, which is what lets Work be safe under
// at-least-once execution. Injecting it keeps Work testable with no network.
type Charger interface {
	Charge(ctx context.Context, idempotencyKey string, amountCents int64) error
}

// ChargeCardArgs is the job payload. IdempotencyKey is stable across attempts of
// the same job, so redelivery after a crash charges nothing extra.
type ChargeCardArgs struct {
	OrderID        int64  `json:"order_id"`
	AmountCents    int64  `json:"amount_cents"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (ChargeCardArgs) Kind() string { return "charge_card" }

func (ChargeCardArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "payments", MaxAttempts: 5}
}

// ChargeCardWorker runs charge_card jobs. It embeds WorkerDefaults for the
// methods it does not override (Middleware).
type ChargeCardWorker struct {
	river.WorkerDefaults[ChargeCardArgs]
	Gateway Charger
}

// Work encodes the retry taxonomy. It is idempotent: the same IdempotencyKey is
// handed to the gateway on every attempt.
func (w *ChargeCardWorker) Work(ctx context.Context, job *river.Job[ChargeCardArgs]) error {
	err := w.Gateway.Charge(ctx, job.Args.IdempotencyKey, job.Args.AmountCents)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCardDeclined):
		// Permanent: cancel now, do not waste attempts on a hard decline.
		return river.JobCancel(fmt.Errorf("order %d: %w", job.Args.OrderID, err))
	case errors.Is(err, ErrRateLimited):
		// Not ready: snooze without consuming an attempt.
		return river.JobSnooze(30 * time.Second)
	default:
		// Transient: a plain error retries with backoff and consumes an attempt.
		return fmt.Errorf("order %d attempt %d: %w", job.Args.OrderID, job.Attempt, err)
	}
}

// Timeout bounds a single execution. The ctx handed to Work is cancelled when it
// fires; a zero return would mean "no timeout".
func (w *ChargeCardWorker) Timeout(job *river.Job[ChargeCardArgs]) time.Duration {
	return 10 * time.Second
}

// NextRetry replaces the default attempt^4 backoff with a linear schedule.
func (w *ChargeCardWorker) NextRetry(job *river.Job[ChargeCardArgs]) time.Time {
	return time.Now().Add(time.Duration(job.Attempt) * 15 * time.Second)
}
```

### The runnable demo

The demo needs no database: it constructs the worker and calls `Work` directly
with crafted gateways, classifying each return value. It first proves idempotency
by working the same job twice against a recording gateway and showing only one
charge landed, then runs each failure branch.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"example.com/retrytaxonomy"
)

// recordingGateway is an idempotent gateway: a repeat of a seen key is a no-op.
type recordingGateway struct {
	mu      sync.Mutex
	charged map[string]int64
}

func (g *recordingGateway) Charge(_ context.Context, key string, cents int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, seen := g.charged[key]; seen {
		return nil
	}
	g.charged[key] = cents
	return nil
}

// stubGateway always returns the same error.
type stubGateway struct{ err error }

func (g stubGateway) Charge(_ context.Context, _ string, _ int64) error { return g.err }

func classify(err error) string {
	if err == nil {
		return "success"
	}
	var cancel *rivertype.JobCancelError
	if errors.As(err, &cancel) {
		return "cancel (permanent)"
	}
	var snooze *rivertype.JobSnoozeError
	if errors.As(err, &snooze) {
		return fmt.Sprintf("snooze %s", snooze.Duration)
	}
	return "retry (transient)"
}

func newJob() *river.Job[retrytaxonomy.ChargeCardArgs] {
	return &river.Job[retrytaxonomy.ChargeCardArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 5},
		Args: retrytaxonomy.ChargeCardArgs{
			OrderID:        1001,
			AmountCents:    5000,
			IdempotencyKey: "order-1001-charge",
		},
	}
}

func main() {
	ctx := context.Background()

	rec := &recordingGateway{charged: map[string]int64{}}
	ok := &retrytaxonomy.ChargeCardWorker{Gateway: rec}
	job := newJob()

	fmt.Printf("first run:  %s\n", classify(ok.Work(ctx, job)))
	fmt.Printf("redelivery: %s\n", classify(ok.Work(ctx, job)))
	fmt.Printf("distinct charges: %d\n", len(rec.charged))

	branches := []struct {
		name    string
		gateway retrytaxonomy.Charger
	}{
		{"declined", stubGateway{err: retrytaxonomy.ErrCardDeclined}},
		{"rate-limited", stubGateway{err: retrytaxonomy.ErrRateLimited}},
		{"gateway-down", stubGateway{err: retrytaxonomy.ErrGatewayDown}},
	}
	for _, b := range branches {
		w := &retrytaxonomy.ChargeCardWorker{Gateway: b.gateway}
		fmt.Printf("%-12s %s\n", b.name+":", classify(w.Work(ctx, newJob())))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first run:  success
redelivery: success
distinct charges: 1
declined:    cancel (permanent)
rate-limited: snooze 30s
gateway-down: retry (transient)
```

The two `success` lines with a single distinct charge are the idempotency proof:
the second execution of the same job hit the recorded key and charged nothing.

### Tests

The tests come in two layers. The pure layer calls `Work` directly and asserts
the *shape* of the return value — `nil`, a transient error you can match with
`errors.Is`, a `*rivertype.JobCancelError` you can match with `errors.As` (which
also unwraps to the original sentinel), and a `*rivertype.JobSnoozeError` whose
`Duration` you can read. That layer needs no database and runs instantly. The
state layer uses `rivertest.NewWorker`, which inserts and works a real job against
Postgres, so you can assert the *resulting job state* per branch and confirm the
budget claim: a snoozed job keeps more retry budget (`MaxAttempts - Attempt`) than
one that returned a plain error. `rivertest`'s `Work` returns a non-nil error only
for the plain-transient branch (snooze and cancel are recorded in `EventKind`, not
returned), so the state helper checks the `WorkResult`, not the error.

Create `retrytaxonomy_test.go`:

```go
package retrytaxonomy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

type recordingGateway struct {
	mu      sync.Mutex
	charged map[string]int64
}

func (g *recordingGateway) Charge(_ context.Context, key string, cents int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, seen := g.charged[key]; seen {
		return nil
	}
	g.charged[key] = cents
	return nil
}

type stubGateway struct{ err error }

func (g stubGateway) Charge(_ context.Context, _ string, _ int64) error { return g.err }

func newJob() *river.Job[ChargeCardArgs] {
	return &river.Job[ChargeCardArgs]{
		JobRow: &rivertype.JobRow{Attempt: 2, MaxAttempts: 5},
		Args: ChargeCardArgs{
			OrderID:        7,
			AmountCents:    5000,
			IdempotencyKey: "order-7-charge",
		},
	}
}

func TestWorkTaxonomy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		w := &ChargeCardWorker{Gateway: &recordingGateway{charged: map[string]int64{}}}
		if err := w.Work(ctx, newJob()); err != nil {
			t.Fatalf("Work success = %v; want nil", err)
		}
	})

	t.Run("transient is a plain retryable error", func(t *testing.T) {
		t.Parallel()
		w := &ChargeCardWorker{Gateway: stubGateway{err: ErrGatewayDown}}
		err := w.Work(ctx, newJob())
		if !errors.Is(err, ErrGatewayDown) {
			t.Fatalf("Work = %v; want wrapped ErrGatewayDown", err)
		}
		var cancel *rivertype.JobCancelError
		var snooze *rivertype.JobSnoozeError
		if errors.As(err, &cancel) || errors.As(err, &snooze) {
			t.Fatal("transient error must not be a cancel or snooze")
		}
	})

	t.Run("permanent is a JobCancel", func(t *testing.T) {
		t.Parallel()
		w := &ChargeCardWorker{Gateway: stubGateway{err: ErrCardDeclined}}
		err := w.Work(ctx, newJob())
		var cancel *rivertype.JobCancelError
		if !errors.As(err, &cancel) {
			t.Fatalf("Work = %v; want *rivertype.JobCancelError", err)
		}
		if !errors.Is(err, ErrCardDeclined) {
			t.Fatalf("cancel error does not unwrap to ErrCardDeclined: %v", err)
		}
	})

	t.Run("rate limited is a JobSnooze", func(t *testing.T) {
		t.Parallel()
		w := &ChargeCardWorker{Gateway: stubGateway{err: ErrRateLimited}}
		err := w.Work(ctx, newJob())
		var snooze *rivertype.JobSnoozeError
		if !errors.As(err, &snooze) {
			t.Fatalf("Work = %v; want *rivertype.JobSnoozeError", err)
		}
		if snooze.Duration != 30*time.Second {
			t.Fatalf("snooze duration = %s; want 30s", snooze.Duration)
		}
	})
}

func TestIdempotentWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rec := &recordingGateway{charged: map[string]int64{}}
	w := &ChargeCardWorker{Gateway: rec}
	job := newJob()

	for range 3 {
		if err := w.Work(ctx, job); err != nil {
			t.Fatalf("Work = %v; want nil", err)
		}
	}
	if len(rec.charged) != 1 {
		t.Fatalf("distinct charges = %d; want 1 (idempotent)", len(rec.charged))
	}
}

func TestTimeoutAndNextRetry(t *testing.T) {
	t.Parallel()
	w := &ChargeCardWorker{Gateway: stubGateway{err: nil}}
	if got := w.Timeout(newJob()); got != 10*time.Second {
		t.Fatalf("Timeout = %s; want 10s", got)
	}
	job := &river.Job[ChargeCardArgs]{JobRow: &rivertype.JobRow{Attempt: 3}}
	got := time.Until(w.NextRetry(job)).Round(time.Second)
	if got != 45*time.Second {
		t.Fatalf("NextRetry for attempt 3 = %s from now; want ~45s", got)
	}
}

// --- state-level tests against a real Postgres ---

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

func migrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	if _, err := migrator.Migrate(t.Context(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func workOnce(t *testing.T, pool *pgxpool.Pool, gw Charger) *rivertest.WorkResult {
	t.Helper()
	ctx := t.Context()
	worker := &ChargeCardWorker{Gateway: gw}
	tw := rivertest.NewWorker(t, riverpgxv5.New(pool), &river.Config{}, worker)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, _ := tw.Work(ctx, t, tx, ChargeCardArgs{
		OrderID:        42,
		AmountCents:    5000,
		IdempotencyKey: "order-42-charge",
	}, nil)
	if res == nil {
		t.Fatal("rivertest Work returned nil result (framework error)")
	}
	return res
}

func TestWorkStates(t *testing.T) {
	pool := setupPool(t)
	migrate(t, pool)

	cases := []struct {
		name      string
		gateway   Charger
		wantKind  river.EventKind
		wantState rivertype.JobState
	}{
		{"success", &recordingGateway{charged: map[string]int64{}}, river.EventKindJobCompleted, rivertype.JobStateCompleted},
		{"transient", stubGateway{err: ErrGatewayDown}, river.EventKindJobFailed, rivertype.JobStateRetryable},
		{"permanent", stubGateway{err: ErrCardDeclined}, river.EventKindJobCancelled, rivertype.JobStateCancelled},
		{"rate_limited", stubGateway{err: ErrRateLimited}, river.EventKindJobSnoozed, rivertype.JobStateScheduled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := workOnce(t, pool, tc.gateway)
			if res.EventKind != tc.wantKind {
				t.Fatalf("EventKind = %s; want %s", res.EventKind, tc.wantKind)
			}
			if res.Job.State != tc.wantState {
				t.Fatalf("State = %s; want %s", res.Job.State, tc.wantState)
			}
		})
	}
}

func TestSnoozePreservesBudget(t *testing.T) {
	pool := setupPool(t)
	migrate(t, pool)

	budget := func(j *rivertype.JobRow) int { return j.MaxAttempts - j.Attempt }

	snoozed := workOnce(t, pool, stubGateway{err: ErrRateLimited})
	failed := workOnce(t, pool, stubGateway{err: ErrGatewayDown})

	if budget(snoozed.Job) <= budget(failed.Job) {
		t.Fatalf("snooze budget %d not greater than transient budget %d",
			budget(snoozed.Job), budget(failed.Job))
	}
}

func ExampleChargeCardArgs_Kind() {
	a := ChargeCardArgs{}
	fmt.Println(a.Kind(), a.InsertOpts().Queue, a.InsertOpts().MaxAttempts)
	// Output: charge_card payments 5
}
```

## Review

The taxonomy is correct when each downstream condition maps to the return value
that produces the right lifecycle move. The pure tests pin the shape: success is
`nil`; a transient fault is a plain error that `errors.Is` matches and is neither
a cancel nor a snooze; a permanent fault is a `*rivertype.JobCancelError` that
still unwraps to your sentinel; a rate limit is a `*rivertype.JobSnoozeError`
carrying the backoff `Duration`. The state tests confirm those returns land the
job in `completed`, `retryable`, `cancelled`, and `scheduled` respectively, and
that the snoozed job kept more `MaxAttempts - Attempt` budget than the one that
failed transiently — the concrete proof that snooze does not burn a retry.

The mistakes this exercise inoculates against are the two inversions. Returning a
plain error for a hard decline retries a doomed job 25 times over three weeks;
returning `JobSnooze` for a genuine transient fault means the job never advances
toward the dead-letter it deserves. And never assume exactly-once execution: the
idempotency test works the same job three times and still records one charge,
which is only true because the key is threaded to the gateway. Drop the key and
the same test would record three charges — the double-charge bug in miniature.

## Resources

- [River docs: Job retries and backoff](https://riverqueue.com/docs/job-retries) — the default `attempt^4` policy and `NextRetry` overrides.
- [River docs: Cancelling jobs](https://riverqueue.com/docs/cancelling-jobs) — `JobCancel` semantics.
- [River docs: Snoozing jobs](https://riverqueue.com/docs/snoozing-jobs) — `JobSnooze` and why it does not consume an attempt.
- [`rivertype` reference](https://pkg.go.dev/github.com/riverqueue/river/rivertype) — `JobCancelError`, `JobSnoozeError`, `JobState`, and `JobRow`.

---

Back to [01-transactional-enqueue.md](01-transactional-enqueue.md) | Next: [03-client-lifecycle-and-error-handling.md](03-client-lifecycle-and-error-handling.md)
