# Exercise 1: Atomic Domain-and-Outbox Write in One Transaction

The whole outbox pattern rests on one move: write the domain row and the event
row in a single database transaction, so the event exists if and only if the
state change committed. This exercise builds that write and proves its
atomicity by forcing the outbox insert to fail and showing that the order insert
is rolled back with it.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
atomicoutbox/                 independent module: example.com/atomicoutbox
  go.mod                      go 1.26; requires modernc.org/sqlite
  outbox.go                   Event envelope, OrderRepository, CreateOrder (one tx), OpenDB, Schema
  cmd/
    demo/
      main.go                 runnable demo: one create, then a rejected create, counts unchanged
  outbox_test.go              atomic-persist test, rollback test (errors.Is ErrOutboxWrite), Example
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: an `OrderRepository` whose `CreateOrder` inserts the `orders` row and an immutable `outbox` event row inside one `sql.Tx`, returning the committed `Event`.
- Test: after a successful create there is exactly one order and one outbox row whose payload unmarshals to the expected event; when the outbox insert fails, `CreateOrder` returns an error wrapping `ErrOutboxWrite` and both tables are empty.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/atomicoutbox/cmd/demo
cd ~/go-exercises/atomicoutbox
go mod init example.com/atomicoutbox
go get modernc.org/sqlite
```

### Why the event is a row, captured at write time

The event is not a live view of the order; it is an immutable envelope
serialized at the instant of the write and stored as a row. `CreateOrder`
marshals an `order.created` payload from the values being written and inserts it
into `outbox` in the same transaction. A relay (Exercise 2) publishes that stored
payload later, faithfully reproducing the intent as it was, even if the order is
mutated afterward. Storing a reference to the row instead would let the relay
publish "whatever the order looks like now," which is the wrong event.

The envelope carries `event_type`, `aggregate_type`, `aggregate_id`, the
serialized `payload`, and `occurred_at`. Those fields are the contract other
services consume; the payload is opaque JSON so the schema can evolve.

### One transaction, all-or-nothing

`CreateOrder` opens a transaction with `BeginTx`, inserts the order, marshals and
inserts the outbox row, and commits. The first line after `BeginTx` is
`defer tx.Rollback()`: it is the safety net for every early return, and after a
successful `Commit` it is a harmless no-op (Commit has already finished the
transaction, so the deferred rollback returns `sql.ErrTxDone`, which we ignore).
The error from `Commit` is checked, never dropped — commit is where durability
actually happens and it can fail.

To make atomicity observable, the repository has an unexported `failOutbox`
switch that the test sets. When on, the outbox insert omits the `NOT NULL`
`event_type` column, so the database rejects it. That returned error is wrapped
with `%w` around the sentinel `ErrOutboxWrite`, `CreateOrder` returns, and the
deferred `tx.Rollback()` undoes the *order* insert too. The test then counts zero
rows in both tables — proof that a failure anywhere in the unit of work rolls the
whole thing back. This is the failure mode a naive "commit then publish" design
cannot achieve: there, the order would already be committed.

### The in-memory database and the one-connection rule

SQLite's pure-Go driver (`modernc.org/sqlite`, driver name `"sqlite"`) is a real
ACID engine. Under `database/sql`, a bare `:memory:` DSN would give each pooled
connection its own separate empty database, so rows would seem to vanish. `OpenDB`
avoids that by using a shared-cache in-memory DSN and pinning the pool to a single
connection with `SetMaxOpenConns(1)` — there is exactly one writer here, so one
connection is both correct and simplest.

Create `outbox.go`:

```go
// outbox.go
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Schema creates the domain table and the outbox table. The outbox id is a
// monotonic autoincrement so a relay can publish in insertion order.
const Schema = `
CREATE TABLE IF NOT EXISTS orders (
	id           TEXT PRIMARY KEY,
	customer     TEXT NOT NULL,
	amount_cents INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS outbox (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	event_type     TEXT NOT NULL,
	aggregate_type TEXT NOT NULL,
	aggregate_id   TEXT NOT NULL,
	payload        BLOB NOT NULL,
	occurred_at    TEXT NOT NULL,
	published_at   INTEGER
);`

// ErrOutboxWrite wraps any failure to persist the outbox event row.
var ErrOutboxWrite = errors.New("outbox write failed")

// Event is the immutable envelope stored in the outbox at write time.
type Event struct {
	EventType     string          `json:"event_type"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Payload       json.RawMessage `json:"payload"`
	OccurredAt    time.Time       `json:"occurred_at"`
}

// Order is the domain aggregate written by CreateOrder.
type Order struct {
	ID          string
	Customer    string
	AmountCents int64
}

type orderCreatedPayload struct {
	OrderID     string `json:"order_id"`
	Customer    string `json:"customer"`
	AmountCents int64  `json:"amount_cents"`
}

// OpenDB opens a shared-cache in-memory SQLite database pinned to one
// connection and applies the schema.
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

// OrderRepository persists orders and their outbox events atomically.
type OrderRepository struct {
	db  *sql.DB
	now func() time.Time

	// failOutbox forces the outbox insert to violate a NOT NULL constraint,
	// used by tests to demonstrate that the whole transaction rolls back.
	failOutbox bool
}

// NewOrderRepository returns a repository backed by db.
func NewOrderRepository(db *sql.DB) *OrderRepository {
	return &OrderRepository{db: db, now: time.Now}
}

// CreateOrder inserts the order row and its order.created outbox event in a
// single transaction. Either both commit or neither does. The committed event
// is returned.
func (r *OrderRepository) CreateOrder(ctx context.Context, o Order) (Event, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // safety net; no-op after a successful Commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO orders(id, customer, amount_cents) VALUES(?, ?, ?)`,
		o.ID, o.Customer, o.AmountCents); err != nil {
		return Event{}, fmt.Errorf("insert order: %w", err)
	}

	payload, err := json.Marshal(orderCreatedPayload{
		OrderID:     o.ID,
		Customer:    o.Customer,
		AmountCents: o.AmountCents,
	})
	if err != nil {
		return Event{}, fmt.Errorf("marshal payload: %w", err)
	}
	ev := Event{
		EventType:     "order.created",
		AggregateType: "order",
		AggregateID:   o.ID,
		Payload:       payload,
		OccurredAt:    r.now().UTC(),
	}

	if err := r.insertOutbox(ctx, tx, ev); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrOutboxWrite, err)
	}

	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit: %w", err)
	}
	return ev, nil
}

func (r *OrderRepository) insertOutbox(ctx context.Context, tx *sql.Tx, ev Event) error {
	if r.failOutbox {
		// Omit the NOT NULL event_type column to force a constraint failure,
		// standing in for any error during the outbox insert.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO outbox(aggregate_type, aggregate_id, payload, occurred_at)
			 VALUES(?, ?, ?, ?)`,
			ev.AggregateType, ev.AggregateID, []byte(ev.Payload),
			ev.OccurredAt.Format(time.RFC3339Nano))
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO outbox(event_type, aggregate_type, aggregate_id, payload, occurred_at)
		 VALUES(?, ?, ?, ?, ?)`,
		ev.EventType, ev.AggregateType, ev.AggregateID, []byte(ev.Payload),
		ev.OccurredAt.Format(time.RFC3339Nano))
	return err
}

// Count returns the number of rows in the named table. It is a small helper for
// the demo and tests to observe atomicity.
func Count(ctx context.Context, db *sql.DB, table string) (int, error) {
	var n int
	// table is a fixed internal identifier, not user input.
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n)
	return n, err
}
```

### The runnable demo

The demo creates one order, prints the committed event, then tries to create a
second order with the *same* id. The duplicate violates the `orders` primary key,
so `CreateOrder` fails and its deferred rollback leaves the counts unchanged —
atomicity via the public API.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"

	"example.com/atomicoutbox"
)

func main() {
	ctx := context.Background()
	db, err := outbox.OpenDB(ctx, "file:demo?mode=memory&cache=shared")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	repo := outbox.NewOrderRepository(db)

	ev, err := repo.CreateOrder(ctx, outbox.Order{
		ID: "ord-1001", Customer: "alice", AmountCents: 4200,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("created event: %s aggregate=%s\n", ev.EventType, ev.AggregateID)
	fmt.Printf("payload: %s\n", ev.Payload)

	orders, _ := outbox.Count(ctx, db, "orders")
	events, _ := outbox.Count(ctx, db, "outbox")
	fmt.Printf("counts after create: orders=%d outbox=%d\n", orders, events)

	if _, err := repo.CreateOrder(ctx, outbox.Order{
		ID: "ord-1001", Customer: "mallory", AmountCents: 9999,
	}); err != nil {
		fmt.Println("second create with same id: rejected")
	}

	orders, _ = outbox.Count(ctx, db, "orders")
	events, _ = outbox.Count(ctx, db, "outbox")
	fmt.Printf("counts after rejected create: orders=%d outbox=%d\n", orders, events)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
created event: order.created aggregate=ord-1001
payload: {"order_id":"ord-1001","customer":"alice","amount_cents":4200}
counts after create: orders=1 outbox=1
second create with same id: rejected
counts after rejected create: orders=1 outbox=1
```

### Tests

The happy-path test asserts exactly one row in each table and that the stored
payload unmarshals to the values written. The atomicity test flips `failOutbox`,
asserts the returned error `errors.Is` the `ErrOutboxWrite` sentinel, and then
counts zero rows in *both* tables — the domain insert was rolled back by the
outbox failure. The `Example` prints the committed event and is checked against
its `// Output` comment.

Create `outbox_test.go`:

```go
// outbox_test.go
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func newRepo(t *testing.T) (*OrderRepository, context.Context) {
	t.Helper()
	ctx := t.Context()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewOrderRepository(db), ctx
}

func TestCreateOrderPersistsBoth(t *testing.T) {
	t.Parallel()
	repo, ctx := newRepo(t)

	ev, err := repo.CreateOrder(ctx, Order{ID: "ord-1", Customer: "alice", AmountCents: 500})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if ev.EventType != "order.created" || ev.AggregateID != "ord-1" {
		t.Fatalf("event = %+v; want order.created for ord-1", ev)
	}

	if n, _ := Count(ctx, repo.db, "orders"); n != 1 {
		t.Fatalf("orders count = %d; want 1", n)
	}
	if n, _ := Count(ctx, repo.db, "outbox"); n != 1 {
		t.Fatalf("outbox count = %d; want 1", n)
	}

	var (
		aggID   string
		payload []byte
	)
	row := repo.db.QueryRowContext(ctx, `SELECT aggregate_id, payload FROM outbox WHERE id = 1`)
	if err := row.Scan(&aggID, &payload); err != nil {
		t.Fatalf("scan outbox row: %v", err)
	}
	if aggID != "ord-1" {
		t.Fatalf("outbox aggregate_id = %q; want ord-1", aggID)
	}
	var got orderCreatedPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	want := orderCreatedPayload{OrderID: "ord-1", Customer: "alice", AmountCents: 500}
	if got != want {
		t.Fatalf("payload = %+v; want %+v", got, want)
	}
}

func TestCreateOrderAtomicRollback(t *testing.T) {
	t.Parallel()
	repo, ctx := newRepo(t)
	repo.failOutbox = true

	_, err := repo.CreateOrder(ctx, Order{ID: "ord-2", Customer: "bob", AmountCents: 700})
	if !errors.Is(err, ErrOutboxWrite) {
		t.Fatalf("CreateOrder error = %v; want wrap of ErrOutboxWrite", err)
	}

	if n, _ := Count(ctx, repo.db, "orders"); n != 0 {
		t.Fatalf("orders count = %d after rollback; want 0", n)
	}
	if n, _ := Count(ctx, repo.db, "outbox"); n != 0 {
		t.Fatalf("outbox count = %d after rollback; want 0", n)
	}
}

func TestCreateOrderDuplicateRejected(t *testing.T) {
	t.Parallel()
	repo, ctx := newRepo(t)

	if _, err := repo.CreateOrder(ctx, Order{ID: "dup", Customer: "a", AmountCents: 1}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := repo.CreateOrder(ctx, Order{ID: "dup", Customer: "b", AmountCents: 2}); err == nil {
		t.Fatal("second create with duplicate id succeeded; want error")
	}
	if n, _ := Count(ctx, repo.db, "orders"); n != 1 {
		t.Fatalf("orders count = %d; want 1 (duplicate rolled back)", n)
	}
	if n, _ := Count(ctx, repo.db, "outbox"); n != 1 {
		t.Fatalf("outbox count = %d; want 1 (duplicate rolled back)", n)
	}
}

func ExampleOrderRepository_CreateOrder() {
	ctx := context.Background()
	db, err := OpenDB(ctx, "file:example-create?mode=memory&cache=shared")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	repo := NewOrderRepository(db)
	ev, err := repo.CreateOrder(ctx, Order{ID: "ord-9", Customer: "carol", AmountCents: 1500})
	if err != nil {
		panic(err)
	}
	fmt.Println(ev.EventType, ev.AggregateID)
	fmt.Println(string(ev.Payload))
	// Output:
	// order.created ord-9
	// {"order_id":"ord-9","customer":"carol","amount_cents":1500}
}
```

## Review

The repository is correct when the event exists exactly when the order does.
`TestCreateOrderPersistsBoth` confirms the forward direction: one create yields
one order and one faithfully-serialized outbox row.
`TestCreateOrderAtomicRollback` confirms the reverse: a failure during the outbox
insert rolls the order insert back too, so both tables are empty — the guarantee
a "commit then publish" design cannot provide. `TestCreateOrderDuplicateRejected`
shows the same rollback through the public API alone.

The mistakes to avoid are the transaction-hygiene ones. Defer `tx.Rollback()`
immediately after `BeginTx` so every early return is covered, and do not treat it
as a leak that it is a no-op after commit — that is by design. Never ignore the
error from `Commit`. Do not do the network publish here: this exercise
deliberately has no broker call, because publishing belongs to the relay outside
this transaction. And snapshot the payload from the values being written, not
from a later read of the row, so the event captures intent at write time. Run
`go test -race` to confirm the single-connection pool serializes correctly.

## Resources

- [Pattern: Transactional Outbox (microservices.io)](https://microservices.io/patterns/data/transactional-outbox.html) — the canonical description of writing the event in the same local transaction.
- [`database/sql` — `Tx`, `BeginTx`, `TxOptions`](https://pkg.go.dev/database/sql#Tx) — transaction lifecycle, deferred rollback, and commit semantics.
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — the pure-Go driver (`"sqlite"`) and its DSN options.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-polling-relay-publisher.md](02-polling-relay-publisher.md)
