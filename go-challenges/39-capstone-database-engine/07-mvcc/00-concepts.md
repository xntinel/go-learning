# 7. Multi-Version Concurrency Control (MVCC) — Concepts

Traditional locking forces readers to block writers and writers to block readers. MVCC breaks that coupling by keeping multiple versions of each row: a writer creates a new version rather than overwriting the existing one, so readers can walk a version chain and find the newest revision that was committed before their transaction began. The hard part is the visibility predicate — an off-by-one in the snapshot counter, or a missing read-your-own-writes case, makes the engine silently return stale data with no error at all. This file is the conceptual foundation for the lesson: read it once and you will have everything you need to reason through each exercise, which build the engine piece by piece as independent, self-contained Go modules — a version store with snapshot isolation, garbage collection below a low watermark, the write-skew anomaly and its FOR UPDATE guard, and statement- versus transaction-level snapshots.

## Concepts

### The Version-Chain Storage Model

Each logical row is a linked list of `Version` values from newest (the head) to oldest. Every version carries two transaction identifiers that, together, encode its entire lifetime:

- `Xmin` — the transaction that created this version, via an insert or an update.
- `Xmax` — the transaction that deleted or superseded this version; zero means the version is still live.

The three write operations manipulate that pair in distinct ways. An insert prepends a new version with `Xmin = tx.ID` and `Xmax = 0`. An update sets `Xmax = tx.ID` on the currently visible version and prepends a fresh version (so an update is logically a delete-plus-insert that preserves the old image). A delete sets `Xmax = tx.ID` on the currently visible version without adding a new one.

```text
head -> Version{Xmin:5, Xmax:0, Data:"v2"} -> Version{Xmin:3, Xmax:5, Data:"v1"} -> nil
```

The chain above reads: version "v1" was created by transaction 3 and superseded by transaction 5; version "v2" was created by transaction 5 and is still live. A reader walks from the head and stops at the first version visible under its snapshot, so the current value is an O(1) read while an old snapshot pays for walking backward.

### Version-Storage Schemes

The layout of the chain is an engine-design choice that trades read cost against write and GC cost (CMU 15-445 lecture 19 surveys the space). Three schemes dominate real systems.

Append-only, newest-to-oldest is what this lesson builds: every version lives in the same logical table, an update appends a full new tuple and links it ahead of the prior one, and the head is always the newest version. Reads of the current version are O(1); reads under an old snapshot walk backward; GC is a cheap tail truncation; the price is table bloat. PostgreSQL is append-only but threads versions through a per-tuple forward pointer (`t_ctid`) rather than a strict newest-to-oldest list.

Delta storage keeps the main tuple as the current image and writes only the changed columns of an update as a delta into a separate undo segment. Current reads never pay for old versions; a snapshot read reconstructs an older image by applying deltas in reverse. Oracle and MySQL/InnoDB use this (the undo log), which keeps the table compact at the cost of a reconstruction penalty on old-snapshot reads.

Time-travel storage keeps the current image in the main table and copies the prior full image into a separate history table keyed by version, so historical reads hit the history table. The shared trade-off: append-only keeps the writer cheap and GC simple at the cost of bloat; delta keeps the table compact but makes old-snapshot reads pay.

### Snapshots and the Commit Sequence

When a transaction begins it captures a `startedAt` value equal to the current commit-sequence counter. That counter is the spine of the whole scheme: every successful commit atomically increments it, takes the resulting value, and records that value in a commit log keyed by transaction id. The counter is a monotone logical clock, deliberately not a wall clock — clock skew across goroutines and non-monotonic virtual-machine clocks would let a transaction see a version committed after it began, which silently breaks isolation.

The crucial ordering detail is the interleaving at `Begin`. A transaction reads `startedAt` after any already-committing writer has incremented the counter, so the value it captures is "the sequence number of the last commit that finished before I started." Commit order, not transaction-id order, is what defines a snapshot: a transaction with a high id can have committed early (low sequence) and must therefore be inside the snapshot of a later-beginning transaction with a lower id. Comparing commit sequence numbers rather than ids is exactly what makes that case correct.

### The Visibility Predicate and Why `<=`

A version `v` is visible to transaction `T` exactly when it was committed in T's past and not yet deleted in T's past, where "T's past" is the set of transactions whose commit sequence is at or below `T.startedAt`. The predicate has two halves, and every other mechanism — reads, scans, conflict detection, GC — depends on it being correct.

The creator half: `v` is visible only if its `Xmin` either equals `T.ID` (read-your-own-writes, so a transaction sees its own not-yet-committed inserts and updates) or committed at or below `T.startedAt`. The check consults the commit log, which is written only by Commit, so an in-flight or aborted `Xmin` is absent from the log and fails the test — that is precisely what prevents a dirty read of an uncommitted or doomed version.

The deleter half: `v` stays visible while its `Xmax` is zero (live) or while the deleting transaction lies in T's future (`commitLog[Xmax] > T.startedAt`). The special case `Xmax == T.ID` hides a row that T deleted itself.

Why "committed before" rather than merely "created before"? Because an uncommitted version may still abort, and exposing it would be a dirty read. Why `<=` rather than strict `<`? Because `T.startedAt` is captured *after* a committing writer has already incremented the counter and recorded its sequence. A transaction whose recorded sequence equals `T.startedAt` had therefore fully finished committing at the instant T took its snapshot, so it belongs in T's past. A strict `<` would hide a fully-committed predecessor and reintroduce a phantom: a row inserted and committed at the exact tick T began would wrongly become invisible. The off-by-one is the single most consequential character in the whole engine.

### xmin/xmax vs begin/end Timestamps

This engine tags each version with the creating and deleting transaction ids (`Xmin`, `Xmax`) and resolves commit order through a separate commit log; PostgreSQL uses the same `xmin`/`xmax` system columns. The alternative, used by timestamp-ordered MVCC and described in CMU 15-445 lecture 19, stamps each version with a begin-ts/end-ts pair drawn from a logical clock at commit, so visibility becomes a direct timestamp-range test with no commit-log lookup. The id-plus-commit-log scheme defers assigning commit order until commit, which keeps Begin cheap (an id is trivial to hand out); the begin/end-ts scheme makes reads a pure comparison but must stamp every version a transaction produced at commit time. The two designs trade a commit-time write against a read-time lookup.

### First-Writer-Wins vs First-Committer-Wins

Snapshot isolation lets concurrent readers proceed without blocking, but it cannot let two concurrent transactions modify the same row: the second writer would fork two diverging histories with no natural merge. Both standard policies forbid that lost-update (dirty-write) case; they differ only in when they detect the clash.

First-writer-wins (this lesson's policy, and PostgreSQL's row-lock behavior) detects the conflict at write time. The procedure is: find the visible version T would overwrite; if its `Xmax` is already set by a transaction that is still active, T loses and receives `ErrWriteConflict` and must abort and may retry. Detection is local and immediate.

First-committer-wins (the abstract SI definition of Berenson et al.) lets writers proceed optimistically and aborts, at commit, any transaction whose write-set overlaps a concurrent transaction that already committed. Detection requires a validation pass over write-sets at commit and the bookkeeping to track them. First-writer-wins is a conservative implementation of first-committer-wins: it can abort a writer that would have been fine had the first writer later aborted, but it never admits a lost update, and it is far simpler when the conflict rate is low.

### Abort and Rollback Atomicity

Each write records a write-set entry before returning, carrying enough information to undo it in reverse order: an insert is undone by removing the inserted version from the head of the chain; an update by removing the new version and clearing `Xmax` on the old one; a delete by clearing `Xmax` on the version that was marked. Rollback holds each chain's write lock for the duration of every undo step, because releasing the lock between reading the head pointer and rewriting it would let a concurrent reader observe a half-undone chain.

The lock order matters and is the part most easily gotten wrong. Abort removes the transaction from the active set *before* it calls rollback. Rollback then acquires per-chain locks; meanwhile concurrent writers hold per-chain locks before they acquire the active-set read lock (for the `isActive` check inside the visibility predicate). Releasing the active-set lock first, so that rollback never holds the active-set lock while it reaches for a chain lock, is what prevents a lock-order-reversal deadlock between aborts and ordinary writes.

### Snapshot Isolation Is Not Serializable: Write Skew

Snapshot isolation forbids dirty reads, non-repeatable reads, and lost updates, but Berenson et al. proved it is still weaker than serializable. The signature anomaly is write skew: two transactions read an overlapping set, then each writes a *different* row. Because the writes touch disjoint rows there is no write-write conflict, so first-writer-wins sees nothing, both commit, and together they break a cross-row invariant that each transaction individually believed it preserved. The classic example is two doctors going off call simultaneously when the rule requires at least one to remain: each reads "the other is on call," each takes itself off, and the invariant "at least one on call" is violated though neither transaction alone is wrong. A second, subtler anomaly survives too: the read-only anomaly of Fekete et al., in which a read-only transaction observes a state no serial order could have produced.

The manual guard is to materialize the conflict: re-read the rows in the read-set FOR UPDATE, which turns an otherwise invisible read-write conflict into a real write that first-writer-wins can catch. Acquiring a write intent on every row read makes a second transaction collide on one of them and abort, restoring serializable behavior for that pattern at the cost of the extra writes. Serializable Snapshot Isolation (SSI, Cahill et al., adopted by PostgreSQL's SERIALIZABLE level) automates this: it keeps the non-blocking SI machinery but tracks read-write dependency edges between concurrent transactions and aborts one when it detects the dangerous structure of two consecutive rw-edges that can close a dependency cycle. The FOR UPDATE guard is the hand-rolled equivalent of what SSI does for you.

### Garbage Collection and the Low Watermark

Old versions accumulate as updates and deletes happen, and a chain that is never trimmed grows without bound. A version is dead once no current or future transaction can ever see it, and "future" is the subtle half of the definition. Every transaction that begins from now on captures `startedAt >= lowWatermark`, where the low watermark is the minimum `startedAt` over all active transactions. A superseded version whose `Xmax` committed at or below that watermark is therefore invisible to every present and future transaction — each will see `commitLog[Xmax] <= startedAt` and skip it — so it can be unlinked from the chain.

When no transactions are active the watermark is the maximum `uint64`, which makes every superseded version collectable in a single pass. The flip side is the operational hazard: a single long-running transaction holds the watermark down at its old snapshot and pins every version superseded since it began, blocking reclamation. PostgreSQL calls this reclamation VACUUM and maintains an equivalent horizon (the cluster `xmin` / `OldestXmin`); an idle-in-transaction session that never commits is exactly why production tables bloat — the watermark cannot advance past it.

### Statement-Level vs Transaction-Level Snapshots

The default isolation in this engine is Repeatable Read (snapshot isolation): one snapshot is captured at Begin and reused for the transaction's entire life, so every read in the transaction sees the same consistent state. Read Committed instead captures a fresh snapshot at the start of every statement, so each statement sees the most recently committed data and a transaction can observe values that changed between its own statements. The difference is one of snapshot acquisition, not of the visibility rule: the same predicate runs in both cases; only the `startedAt` it consults changes. Snapshots only ever move forward, so refreshing a snapshot never resurrects a version an earlier statement could already not see — a refresh can reveal newer commits but never un-hides an older deletion. A practical consequence ties back to GC: refreshing a Read Committed session's snapshot lifts the low watermark it was pinning, which can make a previously pinned version collectable.

## Common Mistakes

### Using Wall-Clock Time as the Snapshot

Wrong: setting `startedAt = time.Now().UnixNano()` and comparing it against a commit timestamp stored in the commit log.

What happens: clock skew between goroutines, and the non-monotonic clocks common on virtual machines, can make a transaction see a version that committed after it began, which silently breaks snapshot isolation with no error.

Fix: use a monotonically incrementing counter (an `atomic.Uint64`) shared across all transactions. Every commit takes the next value; `startedAt` captures the value before the commit increments it; the commit records the value after. No wall clock is involved anywhere.

### Off-By-One in committedBefore

Wrong: writing `return seq < seqBound` (strict less-than) in the commit-log comparison.

What happens: a transaction that committed at exactly `seqBound` appears to fall outside the snapshot, causing a phantom — a row inserted and committed at the very tick T started becomes invisible to T even though it was fully committed when T began.

Fix: use `<=`. Because `startedAt` is captured after the commit sequence is loaded, any transaction whose sequence is at or below `startedAt` finished committing no later than T's start and belongs in T's past.

### Releasing the Chain Lock Between the Conflict Check and the Version Creation

Wrong: unlocking the chain between checking `visible.Xmax` and the pair of mutations that set it and prepend the new version.

What happens: a second writer can observe `Xmax == 0` in the gap and also proceed, so both writers believe they won the first-writer-wins race and the chain ends up with two divergent updates.

Fix: hold the per-chain write lock for the whole operation — the visibility walk, the `Xmax` mutation, and the new-version prepend — so the read-modify-write of the conflict check is atomic with respect to other writers on the same key.

### Aborting a Transaction That Was Already Committed

Wrong: calling Abort after Commit has already returned successfully for the same transaction.

What happens: Abort checks the active set, finds the id absent (Commit removed it), and returns `ErrTxNotActive`; nothing is rolled back, and a caller that ignored the error may believe it undid work it actually committed.

Fix: make Commit and Abort mutually exclusive per transaction in the calling code, and treat `ErrTxNotActive` as a sentinel checked with `errors.Is`.

### Assuming Concurrent Inserts to the Same New Key Conflict

Wrong: assuming two concurrent transactions that both call Insert for a key with no committed version yet will have a conflict detected.

What happens: each Insert checks for a version visible under its own snapshot; if neither has committed, neither sees the other's pending version, so both succeed and the chain ends up with two live versions — a lost update on concurrent insert.

Fix: this implementation documents the case as a known limitation; production engines prevent it with a predicate or next-key lock at SERIALIZABLE isolation. The FOR UPDATE pattern from the write-skew exercise is the same idea applied to reads — materialize an intent so the second writer collides.

---

Next: [01-version-store-and-snapshot-isolation.md](01-version-store-and-snapshot-isolation.md)
