# Exercise 2: A Polling Relay with At-Least-Once Publishing

The outbox is only useful once something drains it to the broker. This exercise
builds a polling relay: it selects unpublished rows in id order, publishes each
through a `Publisher` interface, and stamps `published_at` — strictly in
publish-then-mark order, strictly outside any writer transaction. That ordering
is what makes the pattern at-least-once, and the tests prove both the resume
behavior and the duplicate a crash produces.

This module is fully self-contained: its own `go mod init`, its own schema and
seed helper, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pollingrelay/                 independent module: example.com/pollingrelay
  go.mod                      go 1.26; requires modernc.org/sqlite
  relay.go                    Message, Publisher, Relay.RunOnce (publish-then-mark), OpenDB, SeedEvent
  cmd/
    demo/
      main.go                 runnable demo: seed 3 events, one relay pass, then an empty pass
  relay_test.go               success, stop-and-resume, empty pass, at-least-once duplicate, Example
```

- Files: `relay.go`, `cmd/demo/main.go`, `relay_test.go`.
- Implement: `Relay.RunOnce` selecting `WHERE published_at IS NULL ORDER BY id ASC LIMIT batch`, publishing each row then marking it; on a publish error it stops the batch, leaving that row and all later rows unpublished.
- Test: a successful pass marks every row in id order; a mid-batch failure marks only the rows before it and a second pass resumes in order; an empty pass publishes nothing; a crash between publish and mark yields a duplicate on the next pass.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pollingrelay/cmd/demo
cd ~/go-exercises/pollingrelay
go mod init example.com/pollingrelay
go get modernc.org/sqlite
```

### Publish-then-mark, and why the order is the whole point

The relay does two things per row: call the broker, then record that the row was
published. The order is not interchangeable. If it marked first and published
second, a crash in between would leave a row marked-published that the broker
never received — a silently lost message, the exact bug the outbox exists to
prevent. By publishing first and marking second, the only failure a crash can
cause is a row that gets published again on the next pass: a *duplicate*, which is
safe because consumers dedupe it (the inbox lesson). This is why the outbox is
at-least-once and never exactly-once, and the duplicate test below makes the
guarantee concrete rather than theoretical.

Two more structural rules are baked in. The relay reads the whole batch into a
slice and closes the `sql.Rows` *before* it starts publishing, so no result set
(and no connection) is held open across the network calls to the broker. And on a
publish error the relay stops immediately and returns, leaving the failing row
and everything after it unpublished; because the next pass again selects
`ORDER BY id`, it resumes exactly where it stopped, preserving per-aggregate
order. Skipping the failing row to publish later ones would reorder delivery.

### The Publisher seam

`Publisher` is a one-method interface, `Publish(ctx, Message) error`. The relay
depends only on it, so the real broker (Kafka, NATS, SQS) is one implementation
and the tests inject a recording fake. `Message` is the wire shape the relay
hands the broker: the outbox id (useful as a broker-side dedup key), the event
type, the aggregate id, and the raw payload bytes.

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

// Schema is the outbox table this relay drains.
const Schema = `
CREATE TABLE IF NOT EXISTS outbox (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	event_type     TEXT NOT NULL,
	aggregate_type TEXT NOT NULL,
	aggregate_id   TEXT NOT NULL,
	payload        BLOB NOT NULL,
	occurred_at    TEXT NOT NULL,
	published_at   INTEGER
);`

// Message is what the relay hands the broker for one outbox row.
type Message struct {
	ID          int64
	EventType   string
	AggregateID string
	Payload     []byte
}

// Publisher sends a Message to a broker. A real implementation wraps Kafka,
// NATS, or SQS; tests inject a fake.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Relay drains the outbox by polling for unpublished rows and publishing them.
type Relay struct {
	db    *sql.DB
	pub   Publisher
	batch int
}

// NewRelay returns a relay that publishes up to batch rows per pass.
func NewRelay(db *sql.DB, pub Publisher, batch int) *Relay {
	if batch <= 0 {
		batch = 100
	}
	return &Relay{db: db, pub: pub, batch: batch}
}

// RunOnce publishes one batch of unpublished rows in id order, marking each
// after a successful publish. It returns the number published. On a publish
// error it stops immediately, leaving the failing row and all later rows
// unpublished so the next pass resumes in order.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	msgs, err := r.pending(ctx)
	if err != nil {
		return 0, err
	}
	published := 0
	for _, m := range msgs {
		if err := r.pub.Publish(ctx, m); err != nil {
			return published, fmt.Errorf("publish id=%d: %w", m.ID, err)
		}
		if err := r.markPublished(ctx, m.ID); err != nil {
			return published, fmt.Errorf("mark id=%d: %w", m.ID, err)
		}
		published++
	}
	return published, nil
}

// pending reads the next batch of unpublished rows into memory and closes the
// result set before returning, so no rows are held open across publishing.
func (r *Relay) pending(ctx context.Context) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, event_type, aggregate_id, payload FROM outbox
		 WHERE published_at IS NULL ORDER BY id ASC LIMIT ?`, r.batch)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.EventType, &m.AggregateID, &m.Payload); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}

func (r *Relay) markPublished(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE outbox SET published_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return nil
}

// OpenDB opens a shared-cache in-memory SQLite database pinned to one
// connection (a single relay is the only writer) and applies the schema.
func OpenDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, Schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// SeedEvent inserts one unpublished outbox row and returns its id. In a real
// system the writer inserts these inside its domain transaction (Exercise 1).
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

The demo seeds three events for two aggregates, runs the relay with a publisher
that prints each delivery, then runs a second pass to show there is nothing left.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"

	"example.com/pollingrelay"
)

type printPublisher struct{}

func (printPublisher) Publish(_ context.Context, m relay.Message) error {
	fmt.Printf("delivered: %s %s\n", m.EventType, m.AggregateID)
	return nil
}

func main() {
	ctx := context.Background()
	db, err := relay.OpenDB(ctx, "file:demo?mode=memory&cache=shared")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	relay.SeedEvent(ctx, db, "order.created", "ord-1", []byte(`{"total":500}`))
	relay.SeedEvent(ctx, db, "order.created", "ord-2", []byte(`{"total":900}`))
	relay.SeedEvent(ctx, db, "order.shipped", "ord-1", []byte(`{"carrier":"dhl"}`))

	r := relay.NewRelay(db, printPublisher{}, 10)

	fmt.Println("publishing pending events...")
	n, err := r.RunOnce(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("first pass published %d events\n", n)

	n, err = r.RunOnce(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("second pass published %d events\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
publishing pending events...
delivered: order.created ord-1
delivered: order.created ord-2
delivered: order.shipped ord-1
first pass published 3 events
second pass published 0 events
```

Notice per-aggregate order: `ord-1`'s `created` is delivered before its
`shipped`, while `ord-2` interleaves — exactly what `ORDER BY id` buys.

### Tests

The recording fake captures delivered messages in order and can be told to fail
on a specific id, optionally recording the message first to model a crash *after*
a successful publish. `TestRunOnceSuccess` checks a clean pass. `TestStopsAndResumes`
fails mid-batch, asserts only earlier rows are marked, then resumes cleanly with
no reordering and no duplicate. `TestEmptyPass` checks the no-op. `TestAtLeastOnceDuplicate`
uses the record-then-fail fake to prove the same message is delivered twice
across a crash — the at-least-once contract. Publish errors are asserted with
`errors.Is` against a sentinel.

Create `relay_test.go`:

```go
// relay_test.go
package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

var errBroker = errors.New("broker rejected")

type recordingPublisher struct {
	mu           sync.Mutex
	delivered    []Message
	failOnID     int64
	recordOnFail bool
}

func (p *recordingPublisher) Publish(_ context.Context, m Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOnID != 0 && m.ID == p.failOnID {
		if p.recordOnFail {
			p.delivered = append(p.delivered, m)
		}
		return errBroker
	}
	p.delivered = append(p.delivered, m)
	return nil
}

func (p *recordingPublisher) ids() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, len(p.delivered))
	for i, m := range p.delivered {
		out[i] = m.ID
	}
	return out
}

func newDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	ctx := t.Context()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

func seedN(t *testing.T, ctx context.Context, db *sql.DB, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := SeedEvent(ctx, db, "order.created", fmt.Sprintf("ord-%d", i), []byte(`{}`)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func unpublishedIDs(t *testing.T, ctx context.Context, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT id FROM outbox WHERE published_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatalf("query unpublished: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return ids
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

func TestRunOnceSuccess(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t)
	seedN(t, ctx, db, 3)

	pub := &recordingPublisher{}
	r := NewRelay(db, pub, 10)

	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("published = %d; want 3", n)
	}
	if got := pub.ids(); !equalIDs(got, []int64{1, 2, 3}) {
		t.Fatalf("delivered ids = %v; want [1 2 3]", got)
	}
	if left := unpublishedIDs(t, ctx, db); len(left) != 0 {
		t.Fatalf("unpublished after pass = %v; want none", left)
	}
}

func TestStopsAndResumes(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t)
	seedN(t, ctx, db, 5)

	pub := &recordingPublisher{failOnID: 3}
	r := NewRelay(db, pub, 10)

	n, err := r.RunOnce(ctx)
	if !errors.Is(err, errBroker) {
		t.Fatalf("RunOnce error = %v; want wrap of errBroker", err)
	}
	if n != 2 {
		t.Fatalf("published before failure = %d; want 2", n)
	}
	if left := unpublishedIDs(t, ctx, db); !equalIDs(left, []int64{3, 4, 5}) {
		t.Fatalf("unpublished after failure = %v; want [3 4 5]", left)
	}

	pub.failOnID = 0 // broker healthy again
	n, err = r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("resume RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("published on resume = %d; want 3", n)
	}
	if got := pub.ids(); !equalIDs(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("delivered ids = %v; want [1 2 3 4 5] with no reorder", got)
	}
	if left := unpublishedIDs(t, ctx, db); len(left) != 0 {
		t.Fatalf("unpublished after resume = %v; want none", left)
	}
}

func TestEmptyPass(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t)

	pub := &recordingPublisher{}
	r := NewRelay(db, pub, 10)

	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce on empty outbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("published = %d; want 0", n)
	}
	if got := pub.ids(); len(got) != 0 {
		t.Fatalf("delivered = %v; want none", got)
	}
}

func TestAtLeastOnceDuplicate(t *testing.T) {
	t.Parallel()
	db, ctx := newDB(t)
	seedN(t, ctx, db, 1)

	// First pass: the publish reaches the broker, then the relay "crashes"
	// before marking the row (modeled as record-then-return-error).
	crashing := &recordingPublisher{failOnID: 1, recordOnFail: true}
	r := NewRelay(db, crashing, 10)
	if _, err := r.RunOnce(ctx); !errors.Is(err, errBroker) {
		t.Fatalf("first pass error = %v; want wrap of errBroker", err)
	}
	if got := crashing.ids(); !equalIDs(got, []int64{1}) {
		t.Fatalf("first delivery = %v; want [1]", got)
	}
	if left := unpublishedIDs(t, ctx, db); !equalIDs(left, []int64{1}) {
		t.Fatalf("row should still be unpublished after crash; got %v", left)
	}

	// Second pass with a healthy publisher republishes the same row: the
	// message is delivered a second time. That is at-least-once.
	healthy := &recordingPublisher{}
	r2 := NewRelay(db, healthy, 10)
	if _, err := r2.RunOnce(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := healthy.ids(); !equalIDs(got, []int64{1}) {
		t.Fatalf("second delivery = %v; want [1] (duplicate of the first)", got)
	}
}

func ExampleRelay_RunOnce() {
	ctx := context.Background()
	db, err := OpenDB(ctx, "file:example-runonce?mode=memory&cache=shared")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	SeedEvent(ctx, db, "order.created", "ord-1", []byte(`{"total":500}`))
	SeedEvent(ctx, db, "order.paid", "ord-1", []byte(`{"total":500}`))

	pub := &recordingPublisher{}
	n, err := NewRelay(db, pub, 10).RunOnce(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("published:", n)
	fmt.Println("ids:", pub.ids())
	// Output:
	// published: 2
	// ids: [1 2]
}
```

## Review

The relay is correct when it delivers unpublished rows in id order, marks each
only after the broker accepts it, and resumes at the first still-unpublished row
after any failure. `TestRunOnceSuccess` and `TestEmptyPass` cover the clean
cases; `TestStopsAndResumes` proves a mid-batch failure marks only earlier rows
and resumes without reordering; `TestAtLeastOnceDuplicate` proves the guarantee
the pattern actually offers — a crash between publish and mark redelivers, so
consumers must be idempotent.

The traps are all about ordering and boundaries. Publish then mark, never the
reverse, or a crash loses the message instead of duplicating it. Read the batch
into memory and close the rows before publishing, so a network call does not pin
a connection or an open result set. Stop on the first failure rather than
skipping ahead, or you reorder delivery. Always `ORDER BY id`. And keep the
publish out of any writer transaction — here the relay is a separate component
with its own passes, exactly so that constraint is structural. Run `go test -race`
to confirm the fake's recording is race-free under the driver.

## Resources

- [Pattern: Polling Publisher (microservices.io)](https://microservices.io/patterns/data/polling-publisher.html) — the relay that polls the outbox and publishes.
- [Revisiting the Outbox Pattern (Gunnar Morling)](https://www.morling.dev/blog/revisiting-the-outbox-pattern/) — why the relay is at-least-once and how publish-then-mark interacts with ordering.
- [`database/sql` — `Rows`](https://pkg.go.dev/database/sql#Rows) — `Next`/`Scan`/`Err`/`Close` and why `rows.Err()` must be checked.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-atomic-outbox-write.md](01-atomic-outbox-write.md) | Next: [03-claim-based-relay-with-dead-lettering.md](03-claim-based-relay-with-dead-lettering.md)
