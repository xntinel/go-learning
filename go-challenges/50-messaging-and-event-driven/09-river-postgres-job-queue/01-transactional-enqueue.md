# Exercise 1: Atomic Job Enqueue in a Domain Transaction

This exercise builds the one thing River exists for: a domain service that writes
a business row and enqueues a background job in the *same* Postgres transaction,
so the job is created if and only if the business fact commits. There is no outbox
table, no relay, no dual write — just one `InsertTx` call sharing your `pgx.Tx`.

This module is fully self-contained and imports River, `pgx`, and testcontainers,
so gate it with `GOFLAGS=-mod=mod`. Its tests need a real Postgres and skip
cleanly when neither `DATABASE_URL` nor Docker is available.

## What you'll build

```text
txenqueue/                 independent module: example.com/txenqueue
  go.mod                   go 1.24; requires riverqueue/river, jackc/pgx/v5, testcontainers
  jobs.go                  WelcomeEmailArgs (Kind + per-type InsertOpts); AccountService.OpenAccount
  migrate.go               Migrate: run River's migrations up
  cmd/
    demo/
      main.go              connect, migrate, open an account in a tx, commit
  txenqueue_test.go        commit / rollback / unique-dedupe cases against real Postgres
```

- Files: `jobs.go`, `migrate.go`, `cmd/demo/main.go`, `txenqueue_test.go`.
- Implement: `WelcomeEmailArgs` with a stable `Kind()` and an `InsertOpts()` carrying `Queue`, `MaxAttempts`, and `UniqueOpts`; `AccountService.OpenAccount(ctx, tx, email)` that inserts the account row and calls `Client.InsertTx` on the *same* `tx`; a `Migrate` helper over `rivermigrate`.
- Test: case A commit — the account row exists AND exactly one `welcome_email` job is inserted (`rivertest.RequireInserted`); case B rollback — neither the account row nor any `river_job` row exists; case C — inserting identical args twice within the unique period yields a single job.
- Verify: `GOFLAGS=-mod=mod go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/txenqueue/cmd/demo
cd ~/go-exercises/txenqueue
go mod init example.com/txenqueue
go mod edit -go=1.24
go get github.com/riverqueue/river
go get github.com/riverqueue/river/riverdriver/riverpgxv5
go get github.com/riverqueue/river/rivermigrate
go get github.com/riverqueue/river/rivertest
go get github.com/jackc/pgx/v5
go get github.com/testcontainers/testcontainers-go
go get github.com/testcontainers/testcontainers-go/modules/postgres
```

### The insert-only client and why InsertTx is the whole point

The API process that opens accounts does not *work* jobs — it only enqueues them.
So it builds an insert-only River client: `river.NewClient(riverpgxv5.New(pool),
&river.Config{})` with no `Queues` and no `Workers`. That client can call
`InsertTx` and `Insert` and nothing else; a separate worker process (Exercise 3)
does the working. Keeping enqueue and execution in different processes is normal
River deployment: your latency-sensitive request path never competes with job
execution for CPU.

The service method is the crux. `OpenAccount` receives a `pgx.Tx` that the caller
began, inserts the `accounts` row on that transaction, and then calls
`s.riverClient.InsertTx(ctx, tx, ...)` on *the same transaction*. Both writes are
now uncommitted until the caller commits. If anything after `OpenAccount`
fails — a validation check, a second write, a panic — the caller rolls back and
*both* the account and the job vanish together. If the caller commits, both are
durable together. There is no ordering, no window, no lost job, no orphan job.
Contrast the wrong version: calling `s.riverClient.Insert(ctx, ...)` (pool-based)
inside the same method would commit the job on River's own connection,
independently of your `tx`, and you are back to the dual-write problem — a job for
an account that a rollback erased.

Note `OpenAccount` passes `nil` for the insert options: `WelcomeEmailArgs`
implements `JobArgsWithInsertOpts`, so River reads the per-type `InsertOpts()` —
queue `email`, five attempts, and producer-side uniqueness by args over a day.
The uniqueness means that if two concurrent requests both try to open an account
for the same email and enqueue the same welcome job, only one job row is created;
the second `InsertTx` returns the existing job with `UniqueSkippedAsDuplicate`
set, no error.

Create `jobs.go`:

```go
package txenqueue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// WelcomeEmailArgs is the payload for the welcome-email job. Kind is the stable
// contract between this insert and the worker that will run it; it must never
// change once jobs exist under it.
type WelcomeEmailArgs struct {
	AccountID int64  `json:"account_id"`
	Email     string `json:"email"`
}

// Kind identifies the job type. River stores it on the row and routes the row to
// the worker registered for this exact string.
func (WelcomeEmailArgs) Kind() string { return "welcome_email" }

// InsertOpts sets per-type defaults: a dedicated queue, a bounded attempt count,
// and producer-side uniqueness so the same email is not enqueued twice within a
// day. Explicit opts passed to InsertTx would override these.
func (WelcomeEmailArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       "email",
		MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: 24 * time.Hour,
		},
	}
}

// AccountService performs domain writes and enqueues the jobs they imply. It
// holds an insert-only River client (no Queues, no Workers).
type AccountService struct {
	riverClient *river.Client[pgx.Tx]
}

func NewAccountService(client *river.Client[pgx.Tx]) *AccountService {
	return &AccountService{riverClient: client}
}

// OpenAccount inserts the account row and enqueues its welcome email in the SAME
// transaction. The job exists if and only if tx commits. The caller owns the
// transaction and its Commit/Rollback, which is what couples the two writes.
func (s *AccountService) OpenAccount(ctx context.Context, tx pgx.Tx, email string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO accounts (email) VALUES ($1) RETURNING id`, email,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert account: %w", err)
	}

	// InsertTx, not Insert: the job is written on tx, atomically with the row.
	_, err = s.riverClient.InsertTx(ctx, tx, WelcomeEmailArgs{
		AccountID: id,
		Email:     email,
	}, nil)
	if err != nil {
		return 0, fmt.Errorf("enqueue welcome email: %w", err)
	}
	return id, nil
}
```

### Owning the migrations

River's tables must exist before any client can insert. The `Migrate` helper runs
River's own migration set up with `rivermigrate`. In a real service this runs in
the same ordered pipeline as your application-schema migrations; here it is a
function the demo and tests both call after connecting.

Create `migrate.go`:

```go
package txenqueue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Migrate brings River's schema (river_job and friends) up to date. It is
// idempotent: re-running it after the schema is current is a no-op.
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

### The runnable demo

The demo connects to `DATABASE_URL`, creates the `accounts` table and River's
schema, then opens an account inside a transaction and commits. It prints the new
account id and confirms one job is queued. Point it at any Postgres, for example
a throwaway container:

```bash
docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=secret --name pg postgres:16-alpine
export DATABASE_URL='postgres://postgres:secret@localhost:5432/postgres?sslmode=disable'
go run ./cmd/demo
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"example.com/txenqueue"
)

func main() {
	ctx := context.Background()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("set DATABASE_URL to a Postgres connection string")
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS accounts (id BIGSERIAL PRIMARY KEY, email TEXT NOT NULL)`,
	); err != nil {
		log.Fatalf("create accounts: %v", err)
	}
	if err := txenqueue.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// Insert-only client: no Queues, no Workers.
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	svc := txenqueue.NewAccountService(client)

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	id, err := svc.OpenAccount(ctx, tx, "alice@example.com")
	if err != nil {
		_ = tx.Rollback(ctx)
		log.Fatalf("open account: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}

	var queued int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'welcome_email'`,
	).Scan(&queued); err != nil {
		log.Fatalf("count jobs: %v", err)
	}

	fmt.Printf("opened account id=%d\n", id)
	fmt.Printf("welcome_email jobs queued: %d\n", queued)
}
```

Expected output (against a fresh database):

```
opened account id=1
welcome_email jobs queued: 1
```

### Tests

The tests prove atomicity is coupled to the transaction, not to the pool. Each
test provisions its own Postgres: it uses `DATABASE_URL` if set, otherwise starts
a throwaway container with testcontainers, otherwise skips. `newFixture` creates
the `accounts` table, runs River's migrations, truncates both `accounts` and
`river_job` so each test starts from an empty database, and returns a pool plus
an insert-only service. That truncation is what makes the absolute-count
assertions safe when `DATABASE_URL` points at a database shared across tests;
with testcontainers each test already owns its container.

`TestOpenAccountCommit` opens an account, commits, then asserts both facts: the
account row is present, and `rivertest.RequireInserted` confirms exactly one
`welcome_email` job with the expected queue. `TestOpenAccountRollback` does the
same up to the commit, then *rolls back*, and asserts zero account rows and zero
`river_job` rows — the job died with the domain write. `TestUniqueDedupe` inserts
the identical args twice in two committed transactions and asserts a single job
row survived, proving the producer-side `UniqueOpts`.

Create `txenqueue_test.go`:

```go
package txenqueue

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// setupPool returns a pool to a real Postgres, preferring DATABASE_URL and
// falling back to a testcontainers container. It skips the test when neither is
// available so the suite stays green on a machine without Docker.
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

func newFixture(t *testing.T) (*pgxpool.Pool, *AccountService) {
	t.Helper()
	ctx := t.Context()
	pool := setupPool(t)

	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS accounts (id BIGSERIAL PRIMARY KEY, email TEXT NOT NULL)`,
	); err != nil {
		t.Fatalf("create accounts: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Make every test hermetic even when DATABASE_URL points at a reused
	// database: reset both tables so the absolute-count assertions below can
	// never see rows a sibling test committed. Under testcontainers each test
	// already gets its own container, so this is a cheap no-op there.
	if _, err := pool.Exec(ctx,
		`TRUNCATE accounts, river_job RESTART IDENTITY CASCADE`,
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return pool, NewAccountService(client)
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestOpenAccountCommit(t *testing.T) {
	pool, svc := newFixture(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := svc.OpenAccount(ctx, tx, "commit@example.com")
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := countRows(t, pool, `SELECT count(*) FROM accounts`); got != 1 {
		t.Fatalf("accounts count = %d; want 1", got)
	}

	// The job exists after commit, with the expected args and queue.
	job := rivertest.RequireInserted[*riverpgxv5.Driver](ctx, t,
		riverpgxv5.New(pool),
		WelcomeEmailArgs{AccountID: id, Email: "commit@example.com"},
		&rivertest.RequireInsertedOpts{Queue: "email"},
	)
	if job.Args.AccountID != id {
		t.Fatalf("job AccountID = %d; want %d", job.Args.AccountID, id)
	}
}

func TestOpenAccountRollback(t *testing.T) {
	pool, svc := newFixture(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := svc.OpenAccount(ctx, tx, "rollback@example.com"); err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := countRows(t, pool, `SELECT count(*) FROM accounts`); got != 0 {
		t.Fatalf("accounts count = %d after rollback; want 0", got)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM river_job`); got != 0 {
		t.Fatalf("river_job count = %d after rollback; want 0", got)
	}
}

func TestUniqueDedupe(t *testing.T) {
	pool, svc := newFixture(t)
	ctx := t.Context()

	args := WelcomeEmailArgs{AccountID: 999, Email: "dup@example.com"}
	for range 2 {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := svc.riverClient.InsertTx(ctx, tx, args, nil); err != nil {
			t.Fatalf("InsertTx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	got := countRows(t, pool,
		`SELECT count(*) FROM river_job WHERE kind = 'welcome_email'`)
	if got != 1 {
		t.Fatalf("welcome_email jobs = %d; want 1 (unique dedupe)", got)
	}
}

func ExampleWelcomeEmailArgs_Kind() {
	a := WelcomeEmailArgs{}
	fmt.Println(a.Kind(), a.InsertOpts().Queue, a.InsertOpts().MaxAttempts)
	// Output: welcome_email email 5
}
```

## Review

The service is correct when atomicity is a property of the transaction, not of
timing. The commit test and the rollback test are two halves of one proof: after
commit there is exactly one account and exactly one job; after rollback there is
neither. If you see a job survive a rollback, the tell-tale cause is calling
`Insert` (pool) instead of `InsertTx` (the passed `tx`) — the job committed on
River's own connection, decoupled from your transaction. That is the single
mistake this exercise exists to inoculate against.

Two subtler points. First, `Kind()` is a contract: `"welcome_email"` here must be
byte-for-byte identical to the string the worker registers in Exercise 3, or the
row is inserted and never worked. Second, producer-side `UniqueOpts` dedupes the
*enqueue*, not the *execution* — the dedupe test proves two inserts collapse to
one row, but it says nothing about a single job running twice after a worker
crash. That gap is closed by idempotent `Work`, which is Exercise 2's subject.
Run the suite with `-race` against a real Postgres (or let it skip); a green run
means the atomic-enqueue guarantee holds.

## Resources

- [River docs: Inserting and working jobs](https://riverqueue.com/docs/inserting-and-working-jobs) — `InsertTx`, `JobArgs`, and per-type `InsertOpts`.
- [River docs: Database migrations](https://riverqueue.com/docs/migrations) — the CLI and `rivermigrate` for owning River's schema.
- [River docs: Testing](https://riverqueue.com/docs/testing) — `rivertest.RequireInserted` and friends.
- [`rivertest` reference](https://pkg.go.dev/github.com/riverqueue/river/rivertest) — exact signatures for the assertion helpers.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-worker-retry-policy.md](02-worker-retry-policy.md)
