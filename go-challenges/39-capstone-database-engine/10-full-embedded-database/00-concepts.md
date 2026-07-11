# 10. Full Embedded Database Engine — Concepts

The integration problem is harder than any individual subsystem. The nine preceding lessons each produced a component that passes its own tests in isolation; the work that remains is to make them coexist, and the difficulties that only appear at the seam are the subject of this lesson: initialization order (the log must be open before the page cache; the catalog before any query), the log-before-state mutation discipline that crash recovery depends on, the durability ordering that turns a commit into a promise the database can keep, and the storage and catalog formats that none of the earlier components addressed. This file is the conceptual foundation. Read it once and the exercises that follow — each an independent, self-contained Go module that builds one of these pieces offline — will read as variations on a single set of rules rather than a pile of unrelated code.

## Concepts

### The Layered Dependency Stack

Hellerstein, Stonebraker, and Hamilton's "Architecture of a Database System" organizes a DBMS as a stack of layers, and the discipline that makes the whole system tractable is that each layer depends only on the one directly below it:

```
wire protocol         speaks the client wire format (e.g. the Postgres protocol)
  | calls
SQL front-end         parser + binder + planner/optimizer: text -> plan tree
  | calls
executor              runs the plan: scans, joins, aggregates, projections
  | calls
txn / MVCC            visibility rules, snapshots, lock or version management
  | calls
access methods        sequential scan, B+Tree index, the heap-file abstraction
  | calls
buffer pool           page cache: fetch/pin/dirty/evict over fixed-size pages
  | calls
storage               disk manager + WAL: durable bytes, the bottom of the stack
```

Each arrow is a one-way dependency. The executor asks the access methods for tuples by identifier; it never reaches past them to read a raw page, and it never reaches up to learn which wire client triggered it. The buffer pool hands out fixed-size page buffers but knows nothing about tuples, transactions, or SQL — it manages bytes in frames. This is why an integration layer can accept its dependencies as narrow interfaces: a layer is defined by the contract it offers upward, not by the identity of its caller. The payoff is independent evolution and testing — you can swap a hash index for a B+Tree, or a disk-backed buffer pool for an in-memory mock, without touching the executor.

The ordering is also a strict initialization and teardown order. Storage and the log must be open before the buffer pool, which must be open before the access methods and the catalog, before any query can run. Teardown reverses it: the higher layers shut down before the lower ones they depend on.

### The Integration Invariant: Log Before State

The single rule that must hold across every subsystem boundary is "log before mutate": no change to in-memory state — the catalog, a buffer-pool page, the transaction table — may become visible until the corresponding log record is durably written. Violating it once can produce a database that is inconsistent after a crash in a way recovery cannot detect, because recovery trusts the log to be ground truth: it reconstructs the world by replaying the log, so any state that exists without a matching log record is invisible to recovery and silently lost.

This invariant is enforced most sharply at the DDL level. A `CreateTable` logs the DDL record first, updates the catalog second, and allocates the first heap page third. If any step fails after the catalog was updated, the catalog change is rolled back before returning — and the rollback's own error must be checked, not discarded, because a failed rollback leaves the engine inconsistent and the caller deserves to know. The log record that was already written is harmless: during recovery, a DDL record whose transaction never produced a commit record is skipped by the undo pass.

### Durability Ordering: fsync Before Commit-Ack

"Log before mutate" governs the order of in-memory effects; a second, sharper rule governs durability: the commit record must reach stable storage before the engine acknowledges the commit to the caller. Concretely, commit must fsync the log up to and including the commit record before it returns success. If the engine returns "committed" first and crashes before the fsync completes, recovery finds no commit record, treats the transaction as a loser, and undoes it — the caller was told a lie the database cannot honor. This is the durability (D) of ACID, and it is the reason the log exists at all: data pages may stay dirty in the buffer pool long after commit, because the log, not the heap, is the authority on what is durable.

The ordering constraint cascades. A dirty data page may be written to disk only after the log records describing its changes are durable (write-ahead logging proper), and on shutdown the log is flushed before the buffer pool for the same reason. The cost is one fsync per commit on the critical path; the standard mitigation is group commit — batch many transactions' commit records into one fsync — which trades a little latency for much higher throughput. SQLite exposes the same trade-off directly through `PRAGMA synchronous`: `FULL` fsyncs before acknowledging, `OFF` does not and risks losing committed transactions on power loss.

### Subsystem Interfaces Enable Independent Testing

The subsystem implementations from the earlier lessons are large and carry filesystem and network dependencies. An integration layer should accept them through narrow Go interfaces — a write-ahead log, a buffer pool, a transaction manager — rather than concrete types. Tests then supply lightweight mocks that satisfy the same interfaces, and the integration logic is exercised without touching a disk or a network port.

This pattern — narrow interface, concrete implementation, mock for tests — is the right model for any subsystem with external dependencies. The interface must be as small as the integration logic actually needs, not a transcription of every method on the concrete type. A buffer-pool interface that exposes only fetch, dirty, flush, allocate, and close is enough to drive DDL; the executor's richer needs are a separate, larger contract.

### Slotted-Page Heap Layout

A heap file stores rows (tuples) as a collection of fixed-size pages — 4096 bytes is the conventional choice, matching a common disk block. Within each page the slotted-page layout separates a slot directory, which grows downward from the page header, from the tuple data, which grows upward from the bottom of the page:

```
[0:2]        slot count
[2:4]        free-space pointer — first byte of the tuple area
[4:12]       page LSN (updated by the WAL on every modification)
[12:12+N*4]  slot directory: N entries, each 4 bytes (offset + length)
[12+N*4:ptr] free space
[ptr:4096]   tuple data (packed from the bottom upward)
```

The two-pointer design is what makes this layout work. A slot directory entry is a small, fixed-size handle (offset plus length) and the variable-length tuple lives at the offset it names, so the row's external address is a stable pair of (page, slot index) even as tuples of different sizes come and go. A slot whose length field is zero is a tombstone: the tuple was deleted, but the slot index is preserved so that external references remain valid. Compaction rewrites the tuple area to reclaim tombstoned space while keeping every slot index stable, which is the property that lets indexes and the executor hold onto a tuple identifier across a vacuum.

The free-space pointer and the end of the slot directory form a one-line invariant: `12 + SlotCount()*4 <= freeSpacePtr`. The directory growing up from the header and the tuples growing down from the tail must never cross. Insert checks this before writing; a page that would violate it is full, and an overfull page is a bug in the layer above, not in the page itself.

### System Catalog Bootstrap

A relational system catalog is, conceptually, a set of regular tables — `sys_tables`, `sys_columns`, `sys_indexes` — that describe every other table. This is elegant and it creates a chicken-and-egg problem: you cannot query the catalog to discover how to create the catalog. The standard solution is to hard-code the initial creation (no SQL, no planner) and then treat every subsequent catalog mutation as ordinary DDL through the normal path. A practical implementation keeps the live catalog as an in-memory map guarded by a read/write mutex — reads take a shared lock, the rare DDL write takes an exclusive one — with persistence handled by checkpointing to catalog pages and reconstruction from the log on restart.

Because the system tables are ordinary tables, the same access methods, executor, and durability machinery that serve user data also serve the catalog: a `CREATE TABLE` is, at bottom, an insert into `sys_tables` and several inserts into `sys_columns`. The planner reads the catalog to resolve names to table identifiers, column ordinals, and types during binding, and to learn which indexes exist when choosing a plan. This self-describing design — the schema stored in tables the system already knows how to read and write — is why Postgres exposes `pg_class` and `pg_attribute` and SQLite exposes `sqlite_schema` (formerly `sqlite_master`): the catalog is queried through the same SQL surface as everything else, once the bootstrap is broken by hard-coded initialization.

### ARIES Crash Recovery at the Integration Level

ARIES recovery runs three passes over the log. Analysis scans forward from the last checkpoint to rebuild a dirty-page table (pages modified since the checkpoint) and an active-transaction table (transactions that started but never committed). Redo then replays every log record whose LSN exceeds the affected page's LSN, bringing data pages up to their state at the instant of the crash — including changes made by transactions that had not yet committed. Undo finally walks backward in LSN order and rolls back every change made by a transaction still listed as active, removing those uncommitted effects.

At the integration level, recovery is orchestration: the recovery routine drives the log's replay and threads the result through the buffer pool and catalog in the correct order on open. The detailed redo and undo logic belongs to the log and buffer-pool layers; what the integration adds is calling them in sequence so that the engine becomes available only once the three passes have restored a consistent state. The key conceptual guarantee the integration relies on is that the commit record is the sole source of truth for whether a transaction's work survives: an insert is durable if and only if a commit for its transaction appears in the log, and replaying into a freshly initialized page reconstructs exactly the committed set, no more.

### Shutdown Order: Flush Log, Then Flush Pages

Shutdown order is the mirror image of the write-ahead rule and is just as unforgiving. The log must be flushed before the buffer-pool pages are written, because a data page written to disk carrying an LSN higher than the log's durable tail would leave the log unable to redo that page's changes on the next recovery — the page is ahead of the record that describes it, and recovery, seeing the page's LSN already past the record, would skip the redo and trust a page it never finished writing. The correct sequence is flush the log, flush the buffer pool, close the log, close the buffer pool. A robust close collects failures from all four steps with `errors.Join` rather than stopping at the first error, so a problem in one subsystem does not hide a problem in another.

### Embedded vs Client-Server Architecture

The same seven layers can be packaged two ways, and the choice shapes everything above the storage engine. An embedded database links into the application process: the engine is a library, calls cross a function boundary, and there is no wire layer at all — the SQL front-end's plan runs in the caller's address space. SQLite is the canonical example, and it is deliberately a single-writer design, allowing at most one write transaction at a time on a database file, which lets it dispense with a separate server, a connection manager, and inter-process concurrency control. The cost of an operation is a function call and a page fault; the durability boundary is the file and its fsync. This is why SQLite is the most deployed database in the world — it ships inside phones, browsers, and applications where running a server process would be absurd.

A client-server database runs the engine as its own process (or cluster) that many clients connect to over a socket. Postgres is the canonical example: every layer in the stack still exists, but a wire protocol and a per-connection process or thread sit on top, and the transaction manager must arbitrate genuine concurrency between independent sessions. This buys multi-writer concurrency, network access, centralized resource control, and independent scaling of the database tier — at the cost of a process to operate, a protocol to speak, and a network round-trip per statement. The wire-protocol and MVCC layers are precisely what an embedded engine can omit or simplify and a client-server engine cannot. An integration layer written against narrow interfaces supports either packaging: it is an in-process object today, but because its dependencies are interfaces, putting a wire listener in front of it changes nothing below the front end.

## Common Mistakes

### Mutating Catalog State Before the Log Append Succeeds

Wrong: update the in-memory `tables[name] = meta` map and then call the log's append. If the append fails, the catalog is corrupted in memory and the database must be restarted.

What happens: the next `CreateTable` with the same name returns "table exists" for a table that was never durably created, and on restart — with no log record for it — the catalog disagrees with the heap files on disk.

Fix: append to the log first, update the catalog only on success. Because the append precedes the catalog write, a log failure leaves no catalog state to roll back. A rollback is still needed on the failure paths that occur after the catalog has already been updated — for example after page allocation fails — but never on the log-append failure path itself. Do not discard the rollback error with `_`: if the rollback itself fails the engine is left inconsistent, so wrap both the original error and the rollback error and return them.

### Wrong Shutdown Order: Pages Before Log

Wrong: flush the buffer pool before flushing the log.

What happens: a page reaches disk with an LSN higher than the log's durable tail. On the next crash, redo cannot replay the record for that page because the log does not contain it; recovery believes the page is up to date when it is actually a half-written stale image.

Fix: flush the log first, then the buffer pool, then close the log, then close the pool. The order is not a style preference; reversing it can make recovery impossible.

### Not Compacting After Deletes

Wrong: insert thousands of rows, delete half, and assume the freed space is reclaimed automatically.

What happens: free space does not account for tombstoned bytes, because a tombstone keeps its slot while the tuple area is never moved. The page reports itself full when it could hold many more rows.

Fix: compact periodically. In a real engine the buffer-pool eviction path or a vacuum background process triggers compaction; the contract to pin in a test is that free space strictly increases after a compaction that follows a delete.

### Testing Subsystems With Real Implementations That Touch Disk

Wrong: write integration tests that open a real log file and a disk-backed buffer pool, making the suite slow, flaky, and dependent on filesystem state.

What happens: CI fails on machines with slow storage, tests interfere when they share a path, and a corrupt file from one run poisons the next.

Fix: drive the integration logic through narrow interfaces with lightweight in-memory mocks. Reserve real-subsystem tests for a separate binary gated behind a build tag, where their cost and flakiness are isolated.

---

Next: [01-slotted-page-heap.md](01-slotted-page-heap.md)
