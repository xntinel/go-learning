# The Transactional Outbox Pattern — Concepts

You have a service that owns a database and needs to tell the rest of the system
when something changed: an order was placed, a payment settled, an account was
suspended. The obvious code writes the row and then publishes to a broker. That
obvious code is wrong, and it is wrong in a way that only shows up under crashes
and load, which is exactly when it matters. The transactional outbox is the
standard, boring, correct answer. It is a staple of both real systems and system
design interviews, and a senior backend engineer is expected to be able to
derive it, implement it, and name precisely what it does and does not guarantee.

This file is the conceptual foundation. Read it once and you can reason through
all three exercises: the atomic write, a polling relay, and a claim-based relay
with dead-lettering.

## Concepts

### The dual-write problem

The core difficulty is that "update my database" and "publish to my broker" are
two independent operations against two independent systems with no shared
transaction between them. Whatever order you pick, a crash in the gap corrupts
the system:

```text
write DB, then publish:   crash after commit, before publish -> event LOST
                          (state changed, nobody was told)
publish, then write DB:   crash after publish, before commit -> event FABRICATED
                          (everyone was told about a state that never committed)
```

There is no cheap, generally available distributed transaction spanning a
heterogeneous database and message broker. Two-phase commit (2PC/XA) exists on
paper, but it is operationally expensive, it reduces availability (a coordinator
outage blocks in-doubt transactions), and most brokers either do not support it
or support it only with heavy caveats. So the practical constraint is: you get
exactly one system that can give you an ACID transaction, and it is your
database. Every correct solution funnels the atomicity requirement into that one
transaction.

### The outbox insight: collapse the dual write into one local transaction

The move is to stop treating "the message" as something that lives only in the
broker. Instead, inserting the message becomes part of the *same* local database
transaction as the state change. Alongside the domain table you keep an `outbox`
table, and the write does both inserts in one transaction:

```text
BEGIN
  INSERT INTO orders (...)          -- the state change
  INSERT INTO outbox (...)          -- the intent to notify, as a row
COMMIT                              -- both, or neither
```

Because commit is all-or-nothing, the guarantee becomes exact and easy to state:
**an event row exists if and only if the state change committed.** There is no
window anymore. The hard distributed-consistency problem has been moved entirely
inside a single-datastore transaction, where the database already solves it for
you. A separate process — the relay — later reads the outbox and publishes to the
broker. The relay's failures can no longer lose or fabricate events relative to
state; the worst it can do is publish the *same* committed event more than once,
which is a much easier problem (see delivery semantics below).

### The relay taxonomy: polling vs transaction-log tailing

Two families of relay move rows from the outbox to the broker.

A **polling publisher** is a process that periodically runs `SELECT ... WHERE
published_at IS NULL ORDER BY id LIMIT n`, publishes each row, and marks it
published. It works on any RDBMS, needs no special privileges, and is trivial to
operate and reason about. Its costs are added latency (bounded by the poll
interval) and steady query load, both of which are manageable with a sensible
interval and a good index on the unpublished predicate. Exercises 2 and 3 build
this style.

**Transaction-log tailing**, also called change data capture (CDC), reads the
database's replication log directly — the Postgres WAL via logical decoding, the
MySQL binlog — so a tool such as Debezium sees every committed outbox insert with
no application polling and near-zero added latency. The price is more
infrastructure and operational surface: you need log access, a connector, and a
place to run it. Know both, and know that polling is the right default until poll
latency or load actually hurts.

### Delivery semantics: at-least-once, never exactly-once

This is the single most important thing to say correctly in an interview. The
outbox gives you **at-least-once** delivery end to end. It does not, and cannot
by itself, give you exactly-once. The reason is structural: a relay that
publishes a row to the broker and then crashes *before* it records that the row
was published will, on restart, see the row still unpublished and publish it
again. The broker got the message twice.

The outbox solves atomicity on the *write* side (an event exists iff the state
change did). It does not solve duplicate suppression on the *read* side. That is
a separate job, handled by making consumers idempotent — typically with an inbox
table keyed by message id that records which ids have already been processed
(the next lesson). Selling the outbox as "exactly-once" is the classic
misconception; the honest phrase is "at-least-once, with idempotent consumers
for effective-once."

### Publish ordering and where the transaction boundary goes

Two ordering rules are non-negotiable.

First, the broker call must happen **outside and after** the writer's commit.
Publishing from inside the writing transaction is doubly wrong: it can emit an
event for a state that then rolls back (you told the world about an order that
never existed), and it pins a database transaction and its connection open across
a network round-trip to the broker, which under load means long-held locks and
pool exhaustion. Keep network I/O out of the transaction.

Second, the relay must **publish then mark**, never mark then publish. If you set
`published_at` (or delete the row) before the broker acknowledges, a crash in
between silently drops the message forever — you have recreated the lost-event
bug inside the relay. Publish-then-mark is exactly what makes the failure mode a
*duplicate* (safe, dedup on the consumer) rather than a *loss* (unrecoverable).

### Ordering guarantees

Most systems need **per-aggregate ordering**: all events for a single order must
be delivered in the order they occurred, even though events for *different*
orders may interleave freely. This is cheap: a monotonic outbox id
(`INTEGER PRIMARY KEY AUTOINCREMENT`) plus `ORDER BY id` gives it for free within
a single relay. Strict **global** ordering across all aggregates is far more
expensive (it serializes everything) and is almost never actually required;
demanding it is usually a design smell. Note that multiple relays working in
parallel can reorder delivery unless you partition work by aggregate or accept
that ordering holds only per aggregate, not globally.

### Competing relays and the claim problem

A single relay is a throughput bottleneck and a single point of failure, so you
will want several. The instant you do, you must prevent two relays from grabbing
the same outbox row and double-publishing it. In Postgres the idiomatic tool is
`SELECT ... FOR UPDATE SKIP LOCKED`, which lets each worker lock and take a
disjoint batch, skipping rows another worker already holds.

A portable equivalent that works anywhere is a **lease** (visibility timeout): a
`locked_until` column claimed with an atomic statement that both selects and
stamps the rows in one shot:

```text
UPDATE outbox SET locked_until = now+lease, attempts = attempts + 1
 WHERE id IN (
   SELECT id FROM outbox
    WHERE published_at IS NULL AND (locked_until IS NULL OR locked_until <= now)
    ORDER BY id LIMIT n)
RETURNING id, payload
```

Two workers running this concurrently cannot claim the same row, because the
`UPDATE` is atomic and the second worker's predicate excludes rows the first
already leased into the future. The lease also self-heals a crashed worker: rows
it claimed but never published become claimable again once `locked_until`
elapses, with no manual intervention. `UPDATE ... RETURNING` requires SQLite
3.35+ (the bundled engine is far newer) and maps directly onto the Postgres form.

### Poison messages and head-of-line blocking

Ordering has a dark side. If you enforce strict order and one row can never be
published — a malformed payload, a permanently rejecting broker topic, a bug — it
blocks every row behind it forever. This is head-of-line blocking, and it will
take down the whole pipeline over a single bad message.

The escape hatch is an **attempts** counter and a **max-attempts** threshold.
Each claim increments `attempts` (and a row is only re-claimed after a prior
publish failure); once that count reaches the threshold the row is moved to a
`dead` status so the relay stops selecting it, and the queue drains again. This is a deliberate, documented trade of strict ordering for liveness:
you have chosen to skip a poison message (into a dead-letter state for a human or
a separate process to inspect) rather than let it wedge everything. Exercise 3
implements exactly this.

### Retention and table bloat

An outbox that keeps every published row grows without bound, and the hot polling
query (`WHERE published_at IS NULL`) degrades as the table fills with published
rows the query must skip. You have two retention strategies: delete rows on
publish (smallest table, but you lose the audit trail) or keep them and run a
periodic reaper/archiver that deletes rows published longer than some retention
window. Either way, put a partial or covering index on the unpublished predicate
(`published_at IS NULL, id`) so the polling query stays cheap regardless of how
many published rows accumulate, and understand your engine's vacuum behavior
(autovacuum on Postgres, `VACUUM` on SQLite) because a high-churn table needs it.

### Event payload design

Store an **immutable envelope captured at write time**, not a reference to the
live row. The envelope holds `event_type`, `aggregate_type`, `aggregate_id`, a
serialized `payload`, optional headers/metadata, and `occurred_at`. The reason is
subtle but important: the relay publishes later, possibly after the aggregate has
changed again. If the relay serialized "whatever the row looks like now," it
would emit the wrong intent. By snapshotting the payload into the outbox row
inside the write transaction, the relay faithfully publishes the event as it was
at the moment it happened, decoupled from all later mutations. CloudEvents-style
envelopes make these cross-service contracts explicit and versionable.

### database/sql transaction hygiene

A few habits make transactional code with `database/sql` correct:

- `BeginTx(ctx, *sql.TxOptions)` takes a context and options (isolation level,
  read-only); use it rather than `Begin` so cancellation and isolation are
  explicit.
- Always `defer tx.Rollback()` immediately after a successful `BeginTx`. It is a
  safety net for every early return, and it is a harmless no-op after a
  successful `Commit` (which returns `sql.ErrTxDone` to the deferred call, which
  you ignore).
- Never ignore the error from `tx.Commit()`. Commit is where deferred constraint
  checks and the actual durability happen; it can fail, and treating it as
  infallible loses errors.
- Scope one transaction to one logical unit of work and keep network I/O out of
  it.

### SQLite as the teaching substrate

These exercises use the pure-Go driver `modernc.org/sqlite` (driver name
`"sqlite"`, no cgo) so the pattern is what gets tested, not a cloud vendor.
SQLite is a real ACID engine but is single-writer, and a few facts about it
under `database/sql` matter:

- A plain `:memory:` DSN gives *each pooled connection its own separate empty
  database*, so writes appear to vanish across connections. Fix it with a shared
  cache (`file::memory:?cache=shared`) or by pinning the pool to one connection
  with `db.SetMaxOpenConns(1)`. Exercises 1 and 2, which have a single writer,
  use `SetMaxOpenConns(1)`.
- For genuinely concurrent writers (Exercise 3's competing relays), use a
  file-backed database with `_pragma=journal_mode(WAL)` and
  `_pragma=busy_timeout(5000)` so writers serialize by waiting instead of failing
  with `SQLITE_BUSY`, and `_txlock=immediate` so a write transaction takes the
  write lock at `BEGIN`. A shared-cache in-memory database, by contrast, raises
  `SQLITE_LOCKED` (which `busy_timeout` does *not* retry), so it is the wrong
  substrate for concurrent writers.
- `UPDATE/INSERT/DELETE ... RETURNING` needs SQLite 3.35+; the bundled version
  is newer, so it works.

Every one of these shapes maps directly onto Postgres or MySQL: `SetMaxOpenConns`
and `busy_timeout` become a real connection pool and lock timeout, and the lease
`UPDATE ... RETURNING` becomes `SELECT ... FOR UPDATE SKIP LOCKED`.

## Common Mistakes

### Doing the dual write anyway

Wrong: committing the domain transaction and then, in the very next line,
calling the broker, believing that physical proximity in the code implies
atomicity. The crash window between commit and publish is still there; the event
is still lost if the process dies in it. Fix: write the event as a row inside the
same transaction and let a separate relay publish it.

### Publishing inside the database transaction

Wrong: calling the broker before `Commit`, so an event can fire for a state that
later rolls back, and a database transaction is held open across a network call
(long locks, pool exhaustion). Fix: publish strictly outside and after commit,
from the relay.

### Mark-then-publish

Wrong: setting `published_at` (or deleting the row) before the broker
acknowledges. A crash in between drops the message with no way to recover. Fix:
publish first, then mark; the correct failure mode is a duplicate, not a loss.

### Assuming exactly-once

Wrong: treating the outbox as exactly-once and therefore skipping idempotent
consumers. Relay-crash-after-publish duplicates are guaranteed, not
hypothetical. Fix: make consumers idempotent (the inbox pattern, next lesson).

### The vanishing-writes in-memory SQLite trap

Wrong: opening `:memory:` with the default pool (`MaxOpenConns > 1`). Each
connection gets its own empty database, so a row written on one connection is
invisible on the next and tests fail bafflingly. Fix: `cache=shared` or
`SetMaxOpenConns(1)`.

### No busy_timeout / txlock for concurrent relays

Wrong: running competing relays against SQLite with no `busy_timeout` and no
`_txlock=immediate`, so writers collide with `SQLITE_BUSY` instead of serializing.
Fix: set both in the DSN and use a file-backed WAL database, not shared-cache
memory.

### SELECT without ORDER BY id

Wrong: selecting the batch with no `ORDER BY`, yielding nondeterministic delivery
order that violates per-aggregate ordering. Fix: always `ORDER BY id`.

### No lease on claimed rows

Wrong: claiming a batch with no visibility timeout, so a relay that crashes
mid-batch leaves its rows stuck forever with no worker willing to retry them.
Fix: stamp `locked_until` on claim and make expired leases reclaimable.

### No poison-message handling

Wrong: no attempts counter, so one permanently failing row head-of-line-blocks
the entire ordered outbox. Fix: increment `attempts` per failed claim and move a
row past `max_attempts` to a `dead` status.

### Letting the outbox grow forever

Wrong: never deleting or archiving published rows, so the table and its indexes
bloat and the polling query slows. Fix: delete on publish or run a retention
reaper, backed by a partial index on the unpublished predicate.

### Ignoring rows.Err() / not closing rows

Wrong: iterating `sql.Rows` with `Next()` and never checking `rows.Err()` after
the loop or calling `rows.Close()`. A mid-iteration error is silently swallowed
and a leaked result set pins a connection. Fix: check `rows.Err()` and close
(defer `Close` or close before the transaction commits).

### Serializing the live entity at publish time

Wrong: having the relay read and serialize the current domain row when it
publishes. It emits whatever the row looks like now, not the intent at the time
of the event. Fix: snapshot the payload into the outbox row inside the write
transaction.

### Treating Commit as infallible

Wrong: ignoring the error from `tx.Commit()` or forgetting the deferred
`tx.Rollback()` safety net, leaking transactions on early returns. Fix: check the
commit error and always defer rollback right after `BeginTx`.

Next: [01-atomic-outbox-write.md](01-atomic-outbox-write.md)
