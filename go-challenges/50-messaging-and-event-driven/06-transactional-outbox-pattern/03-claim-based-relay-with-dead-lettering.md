# Exercise 3: Competing Relays — Lease Claiming and Poison-Message Dead-Lettering

One relay is a bottleneck and a single point of failure. To run several safely
you must stop two workers from grabbing the same row, survive a worker that
crashes mid-batch, and survive a message that can never be published. This
exercise builds a claim-based relay that does all three: an atomic
`UPDATE ... RETURNING` lease (the portable analogue of Postgres
`SELECT ... FOR UPDATE SKIP LOCKED`), a visibility timeout that makes a crashed
worker's rows reclaimable, and an attempts-plus-dead-letter escape hatch that
unblocks the queue when a poison message will not go.

This module is fully self-contained: its own `go mod init`, schema, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
claimrelay/                   independent module: example.com/claimrelay
  go.mod                      go 1.26; requires modernc.org/sqlite
  relay.go                    Message, Publisher, Relay (claim/publish/mark), lease + dead-letter, DSN/OpenDB
  cmd/
    demo/
      main.go                 runnable demo: one poison + two healthy events; dead-letter unblocks the queue
  relay_test.go               concurrent single-claim, poison->dead, lease reclaim, ordering, Example
```

- Files: `relay.go`, `cmd/demo/main.go`, `relay_test.go`.
- Implement: `Relay.RunOnce` that atomically claims a batch with `UPDATE outbox SET locked_until=?, attempts=attempts+1 WHERE id IN (SELECT ... WHERE published_at IS NULL AND status<>'dead' AND lease-expired ORDER BY id LIMIT ?) RETURNING ...`, publishes each, marks published, and moves a row that has exhausted its attempts (at or past `MaxAttempts`) to `status='dead'`.
- Test: two concurrent workers publish each row exactly once; a poison row's `attempts` climbs each pass and it becomes `dead` after `MaxAttempts` while healthy rows keep flowing; an unpublished claim becomes reclaimable once its lease expires; claims honor `ORDER BY id`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/06-transactional-outbox-pattern/03-claim-based-relay-with-dead-lettering/cmd/demo
cd go-solutions/50-messaging-and-event-driven/06-transactional-outbox-pattern/03-claim-based-relay-with-dead-lettering
go get modernc.org/sqlite
```

### The atomic claim: select and lock in one statement

The dangerous version of a multi-worker relay does a `SELECT` and then a separate
`UPDATE`: between the two, another worker runs the same `SELECT` and both publish
the same rows. The fix is to make claiming a single atomic statement. Inside one
transaction, an `UPDATE ... WHERE id IN (SELECT ... ORDER BY id LIMIT n)
RETURNING ...` both stamps `locked_until`/`attempts` on the chosen rows and hands
them back. Because the `UPDATE` is atomic, two workers running it concurrently
cannot pick the same row: the second worker's inner `SELECT` no longer matches
rows the first already leased into the future. This is the portable stand-in for
Postgres `SELECT ... FOR UPDATE SKIP LOCKED`; on Postgres you would use exactly
that, on SQLite you use the lease. `RETURNING` requires SQLite 3.35+.

The lease is a visibility timeout. A claimed row has `locked_until` set a lease
window into the future, so no other worker will re-claim it while a worker is
busy publishing it. If that worker crashes before it publishes or marks the row,
the lease simply expires and the row becomes claimable again — self-healing with
no operator intervention. The predicate `locked_until IS NULL OR locked_until <=
now` is what expresses "unclaimed, or claimed but the lease lapsed."

### Publishing and dead-lettering happen after the claim commits

The claim transaction commits before any broker call, so the lease is durable and
visible to other workers, and no transaction is held open across the network.
Then, per row: publish, and on success mark it published (and clear the lease).
On a publish failure the row's `attempts` has already been incremented by the
claim; once that count reaches `MaxAttempts` the row is moved to `status='dead'` so the
claim predicate stops selecting it — the queue drains past the poison message.
Below the threshold the row is simply left claimed; once its lease expires it is
retried on a later pass. This is the deliberate trade of strict ordering for
liveness: a permanently failing row is dead-lettered rather than allowed to
head-of-line-block everything behind it.

### The concurrent SQLite substrate

Competing writers need a substrate that serializes writers by waiting, not by
failing. A shared-cache in-memory database raises `SQLITE_LOCKED` (which
`busy_timeout` does not retry), so this exercise uses a file-backed database with
`journal_mode(WAL)`, `busy_timeout(5000)`, and `_txlock=immediate` — writers take
the write lock at `BEGIN` and any collision waits up to the busy timeout instead
of erroring. `SetMaxOpenConns` sizes the pool so several workers can hold
connections at once. On Postgres these knobs are a real connection pool and lock
timeout; the code shape is identical.

Create `relay.go`:

```go
// relay.go
package relay

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Schema is the outbox table with the lease/attempts/status columns a
// claim-based relay needs.
const Schema = `
CREATE TABLE IF NOT EXISTS outbox (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	event_type     TEXT NOT NULL,
	aggregate_type TEXT NOT NULL,
	aggregate_id   TEXT NOT NULL,
	payload        BLOB NOT NULL,
	occurred_at    TEXT NOT NULL,
	published_at   INTEGER,
	locked_until   INTEGER,
	attempts       INTEGER NOT NULL DEFAULT 0,
	status         TEXT NOT NULL DEFAULT 'pending'
);`

// Message is what the relay hands the broker. Attempts is the post-claim
// attempt count, used to decide dead-lettering.
type Message struct {
	ID          int64
	EventType   string
	AggregateID string
	Payload     []byte
	Attempts    int
}

// Publisher sends a Message to a broker.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// PassResult reports what one RunOnce did.
type PassResult struct {
	Claimed   int
	Published int
	Dead      int
	Deferred  int // failed but under MaxAttempts; left for a later pass
}

// Config tunes a relay.
type Config struct {
	Batch       int
	Lease       time.Duration
	MaxAttempts int
}

// Relay is a claim-based outbox relay safe to run in several instances.
type Relay struct {
	db          *sql.DB
	pub         Publisher
	batch       int
	lease       time.Duration
	maxAttempts int
	now         func() time.Time
}

// NewRelay builds a relay. Zero config fields fall back to sane defaults.
func NewRelay(db *sql.DB, pub Publisher, cfg Config) *Relay {
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	return &Relay{
		db:          db,
		pub:         pub,
		batch:       cfg.Batch,
		lease:       cfg.Lease,
		maxAttempts: cfg.MaxAttempts,
		now:         time.Now,
	}
}

// RunOnce claims one batch, publishes each row, marks successes published, and
// dead-letters rows that have exhausted their attempts. It is safe to call from
// several goroutines or processes concurrently.
func (r *Relay) RunOnce(ctx context.Context) (PassResult, error) {
	now := r.now()
	msgs, err := r.claim(ctx, now)
	if err != nil {
		return PassResult{}, err
	}
	res := PassResult{Claimed: len(msgs)}
	for _, m := range msgs {
		if perr := r.pub.Publish(ctx, m); perr != nil {
			if m.Attempts >= r.maxAttempts {
				if err := r.markDead(ctx, m.ID); err != nil {
					return res, fmt.Errorf("mark dead id=%d: %w", m.ID, err)
				}
				res.Dead++
				continue
			}
			res.Deferred++ // leave claimed; the lease will expire and it retries
			continue
		}
		if err := r.markPublished(ctx, m.ID, now); err != nil {
			return res, fmt.Errorf("mark published id=%d: %w", m.ID, err)
		}
		res.Published++
	}
	return res, nil
}

// claim atomically leases up to batch eligible rows and returns them. Eligible
// means unpublished, not dead, and either never claimed or past its lease.
func (r *Relay) claim(ctx context.Context, now time.Time) ([]Message, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback()

	nowMs := now.UnixMilli()
	untilMs := now.Add(r.lease).UnixMilli()
	rows, err := tx.QueryContext(ctx, `
		UPDATE outbox SET locked_until = ?, attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM outbox
			WHERE published_at IS NULL AND status <> 'dead'
			  AND (locked_until IS NULL OR locked_until <= ?)
			ORDER BY id ASC LIMIT ?)
		RETURNING id, event_type, aggregate_id, payload, attempts`,
		untilMs, nowMs, r.batch)
	if err != nil {
		return nil, fmt.Errorf("claim update: %w", err)
	}

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.EventType, &m.AggregateID, &m.Payload, &m.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate claim: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return out, nil
}

func (r *Relay) markPublished(ctx context.Context, id int64, now time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE outbox SET published_at = ?, locked_until = NULL WHERE id = ?`,
		now.UnixMilli(), id)
	return err
}

func (r *Relay) markDead(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE outbox SET status = 'dead', locked_until = NULL WHERE id = ?`, id)
	return err
}

// DSN builds a file-backed SQLite DSN configured for concurrent writers.
func DSN(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
}

// OpenDB opens the database, sizes the pool, and applies the schema.
func OpenDB(ctx context.Context, dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if maxConns > 0 {
		db.SetMaxOpenConns(maxConns)
	}
	if _, err := db.ExecContext(ctx, Schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// SeedEvent inserts one unpublished outbox row and returns its id.
func SeedEvent(ctx context.Context, db *sql.DB, eventType, aggregateID string, payload []byte) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO outbox(event_type, aggregate_type, aggregate_id, payload, occurred_at)
		 VALUES(?, 'order', ?, ?, ?)`,
		eventType, aggregateID, payload, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("seed event: %w", err)
	}
	return res.LastInsertId()
}
```

### The runnable demo

The demo seeds one poison event (aggregate `ord-BAD`, which the publisher always
rejects) between two healthy events, with `Lease: 0` so a deferred row is
immediately reclaimable, and `MaxAttempts: 3`. It runs passes until nothing is
claimed, then shows the poison row dead-lettered and the healthy events through.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"example.com/claimrelay"
)

type pickyPublisher struct{}

func (pickyPublisher) Publish(_ context.Context, m relay.Message) error {
	if m.AggregateID == "ord-BAD" {
		return fmt.Errorf("cannot serialize %s", m.AggregateID)
	}
	return nil
}

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "outbox")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	db, err := relay.OpenDB(ctx, relay.DSN(filepath.Join(dir, "outbox.db")), 1)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	relay.SeedEvent(ctx, db, "order.created", "ord-1", []byte(`{}`))
	relay.SeedEvent(ctx, db, "order.created", "ord-BAD", []byte(`{}`))
	relay.SeedEvent(ctx, db, "order.created", "ord-2", []byte(`{}`))

	r := relay.NewRelay(db, pickyPublisher{}, relay.Config{Batch: 10, Lease: 0, MaxAttempts: 3})

	for pass := 1; ; pass++ {
		res, err := r.RunOnce(ctx)
		if err != nil {
			panic(err)
		}
		fmt.Printf("pass %d: claimed=%d published=%d dead=%d deferred=%d\n",
			pass, res.Claimed, res.Published, res.Dead, res.Deferred)
		if res.Claimed == 0 {
			break
		}
	}

	var agg string
	var attempts int
	if err := db.QueryRowContext(ctx,
		`SELECT aggregate_id, attempts FROM outbox WHERE status = 'dead'`).Scan(&agg, &attempts); err == nil {
		fmt.Printf("dead-lettered: %s (attempts=%d)\n", agg, attempts)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pass 1: claimed=3 published=2 dead=0 deferred=1
pass 2: claimed=1 published=0 dead=0 deferred=1
pass 3: claimed=1 published=0 dead=1 deferred=0
pass 4: claimed=0 published=0 dead=0 deferred=0
dead-lettered: ord-BAD (attempts=3)
```

### Tests

`TestConcurrentSingleClaim` runs two workers against a file-backed WAL database
and asserts every row is delivered exactly once — the no-double-publish property.
`TestPoisonDeadLetters` drives a poison row across passes with an advancing fake
clock, asserting its `attempts` climbs and it reaches `status='dead'` after
`MaxAttempts` while a healthy row publishes. `TestLeaseReclaim` claims without
publishing, shows the rows are not re-claimable within the lease, then reclaimable
once a fake clock passes `locked_until`, and checks `ORDER BY id`.

Create `relay_test.go`:

```go
// relay_test.go
package relay

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingPublisher struct {
	mu      sync.Mutex
	perID   map[int64]int
	failAgg string // publish fails for this aggregate id
}

func newRecorder() *recordingPublisher {
	return &recordingPublisher{perID: make(map[int64]int)}
}

func (p *recordingPublisher) Publish(_ context.Context, m Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failAgg != "" && m.AggregateID == p.failAgg {
		return fmt.Errorf("rejecting %s", m.AggregateID)
	}
	p.perID[m.ID]++
	return nil
}

func (p *recordingPublisher) counts() map[int64]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[int64]int, len(p.perID))
	for k, v := range p.perID {
		out[k] = v
	}
	return out
}

func newDB(t *testing.T, maxConns int) (*sql.DB, context.Context) {
	t.Helper()
	ctx := t.Context()
	dsn := DSN(filepath.Join(t.TempDir(), "outbox.db"))
	db, err := OpenDB(ctx, dsn, maxConns)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

func TestConcurrentSingleClaim(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t, 4)

	const n = 50
	for i := 1; i <= n; i++ {
		if _, err := SeedEvent(ctx, db, "order.created", fmt.Sprintf("ord-%d", i), []byte(`{}`)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	pub := newRecorder()
	cfg := Config{Batch: 5, Lease: time.Minute, MaxAttempts: 5}

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewRelay(db, pub, cfg)
			for {
				res, err := r.RunOnce(ctx)
				if err != nil {
					errs <- err
					return
				}
				if res.Claimed == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("worker: %v", err)
	}

	counts := pub.counts()
	if len(counts) != n {
		t.Fatalf("delivered %d distinct rows; want %d", len(counts), n)
	}
	for id, c := range counts {
		if c != 1 {
			t.Fatalf("row %d delivered %d times; want exactly once", id, c)
		}
	}
}

func TestPoisonDeadLetters(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t, 1)

	if _, err := SeedEvent(ctx, db, "order.created", "ord-good", []byte(`{}`)); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	if _, err := SeedEvent(ctx, db, "order.created", "ord-poison", []byte(`{}`)); err != nil {
		t.Fatalf("seed poison: %v", err)
	}

	pub := newRecorder()
	pub.failAgg = "ord-poison"
	r := NewRelay(db, pub, Config{Batch: 10, Lease: time.Minute, MaxAttempts: 3})

	// Drive passes with a fake clock that advances past the lease each time so
	// the poison row is re-claimable, until it dead-letters.
	base := time.Unix(0, 0).UTC()
	step := 0
	r.now = func() time.Time { return base.Add(time.Duration(step) * time.Hour) }

	deadPass := -1
	for pass := 0; pass < 6; pass++ {
		step = pass
		res, err := r.RunOnce(ctx)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if res.Dead == 1 {
			deadPass = pass
		}
	}
	if deadPass < 0 {
		t.Fatal("poison row was never dead-lettered")
	}

	// The healthy row published exactly once.
	if got := pub.counts(); len(got) != 1 {
		t.Fatalf("healthy deliveries = %v; want exactly one row once", got)
	}

	var status string
	var attempts int
	if err := db.QueryRowContext(ctx,
		`SELECT status, attempts FROM outbox WHERE aggregate_id = 'ord-poison'`).
		Scan(&status, &attempts); err != nil {
		t.Fatalf("query poison: %v", err)
	}
	if status != "dead" {
		t.Fatalf("poison status = %q; want dead", status)
	}
	if attempts != 3 {
		t.Fatalf("poison attempts = %d; want 3 (MaxAttempts)", attempts)
	}

	// Once dead, the poison row is no longer claimed.
	step = 10
	res, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("final pass: %v", err)
	}
	if res.Claimed != 0 {
		t.Fatalf("claimed %d after dead-letter; want 0", res.Claimed)
	}
}

func TestLeaseReclaim(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t, 1)

	for i := 1; i <= 3; i++ {
		if _, err := SeedEvent(ctx, db, "order.created", fmt.Sprintf("ord-%d", i), []byte(`{}`)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	r := NewRelay(db, newRecorder(), Config{Batch: 10, Lease: 30 * time.Second, MaxAttempts: 5})
	t0 := time.Unix(1000, 0).UTC()

	// Claim without publishing; rows are now leased until t0+30s.
	first, err := r.claim(ctx, t0)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if got := ids(first); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("first claim ids = %v; want [1 2 3] (ORDER BY id)", got)
	}

	// Within the lease window nothing is re-claimable.
	within, err := r.claim(ctx, t0.Add(10*time.Second))
	if err != nil {
		t.Fatalf("within-lease claim: %v", err)
	}
	if len(within) != 0 {
		t.Fatalf("re-claimed %d rows inside the lease; want 0", len(within))
	}

	// After the lease expires the rows are reclaimable, in id order, with a
	// second attempt recorded.
	after, err := r.claim(ctx, t0.Add(31*time.Second))
	if err != nil {
		t.Fatalf("after-lease claim: %v", err)
	}
	if got := ids(after); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("reclaimed ids = %v; want [1 2 3]", got)
	}
	for _, m := range after {
		if m.Attempts != 2 {
			t.Fatalf("row %d attempts = %d; want 2 after reclaim", m.ID, m.Attempts)
		}
	}
}

func ids(ms []Message) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ExampleRelay_RunOnce() {
	ctx := context.Background()
	db, err := OpenDB(ctx, DSN(filepath.Join(os.TempDir(), "claimrelay-example.db")), 1)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	defer os.Remove(filepath.Join(os.TempDir(), "claimrelay-example.db"))

	SeedEvent(ctx, db, "order.created", "ord-1", []byte(`{}`))
	SeedEvent(ctx, db, "order.created", "ord-2", []byte(`{}`))

	r := NewRelay(db, newRecorder(), Config{Batch: 10, Lease: time.Minute, MaxAttempts: 3})
	res, err := r.RunOnce(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("claimed=%d published=%d\n", res.Claimed, res.Published)
	// Output:
	// claimed=2 published=2
}
```

## Review

The relay is correct when concurrent workers never double-publish a row, a
crashed worker's rows return to the pool after the lease, and a permanently
failing row is dead-lettered instead of blocking the queue.
`TestConcurrentSingleClaim` proves the first with two workers and an
exactly-once per-id assertion; `TestLeaseReclaim` proves the second by claiming
without publishing and advancing a fake clock past `locked_until`;
`TestPoisonDeadLetters` proves the third, watching `attempts` climb to
`MaxAttempts` and the row flip to `dead` while a healthy row still flows.

The traps are specific. Do the claim as one atomic `UPDATE ... RETURNING`, never
`SELECT` then `UPDATE`, or two workers grab the same row. Always `ORDER BY id`
inside the claim so per-aggregate order holds. Give every claim a lease, or a
crashed worker wedges its rows forever. Add the attempts/`MaxAttempts`/dead-letter
escape hatch, or one poison message head-of-line-blocks the ordered queue. Use a
file-backed WAL database with `busy_timeout` and `_txlock=immediate` for
concurrent writers — a shared-cache in-memory database raises `SQLITE_LOCKED`,
which the busy timeout will not retry. Run `go test -race`; the concurrent test is
the reason the race detector matters here.

## Resources

- [Reliable Microservices Data Exchange With the Outbox Pattern (Debezium)](https://debezium.io/blog/2019/02/19/reliable-microservices-data-exchange-with-the-outbox-pattern/) — the outbox, relays, and where dead-lettering fits.
- [`SELECT ... FOR UPDATE SKIP LOCKED` (PostgreSQL docs)](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE) — the production claim primitive this lease emulates.
- [SQLite `RETURNING`](https://www.sqlite.org/lang_returning.html) — the 3.35+ clause that makes an atomic claim possible.
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — the pure-Go driver and its `_pragma`/`_txlock` DSN options.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-polling-relay-publisher.md](02-polling-relay-publisher.md) | Next: [../07-idempotent-consumers-inbox/00-concepts.md](../07-idempotent-consumers-inbox/00-concepts.md)
