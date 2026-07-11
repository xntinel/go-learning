# 8. Transaction Manager — Concepts

A transaction manager is the component that makes a database trustworthy. It ties together the write-ahead log, the buffer pool, the lock table, and the recovery algorithm into a single subsystem that enforces the ACID properties. The hard part is not any one of those mechanisms in isolation — it is their interaction: a commit must flush the log before it releases locks; an abort must walk the log backwards before it announces completion; recovery must redo committed work and undo uncommitted work without re-undoing what compensation log records have already fixed. This file is the conceptual foundation. Read it once and you will have the vocabulary and the reasoning you need for every exercise, each of which builds one slice of the manager as an independent, self-contained Go module.

## Concepts

### The ACID Contract and the Manager's Sequencing Role

Atomicity, Consistency, Isolation, and Durability each require a specific mechanism, and the manager's real job is to sequence those mechanisms correctly rather than to invent them:

- Atomicity: the log records a before-image for every write. On abort the manager walks the log backwards, restoring each before-image and writing a compensation log record (CLR). Either all of a transaction's writes become visible or none of them do.
- Durability: a commit flushes the log to disk (fdatasync in a production engine) before returning to the caller. Anything that reached the flushed log survives a crash.
- Isolation: strict two-phase locking holds locks until commit or abort, which prevents dirty reads and unrepeatable reads.
- Consistency: the application encodes the invariants (for example, the sum of two account balances stays constant); the manager supplies the isolation and atomicity that let the application preserve them.

The ordering inside Commit is the part that is easy to get wrong and impossible to fix after the fact: write the dirty pages, write the COMMIT record, flush the log, then release the locks. Any other order breaks at least one guarantee. Releasing locks before the flush, in particular, lets another transaction read and commit on top of data whose commit record is not yet durable — so a crash in that window erases a commit that a second transaction already depended on.

### Write-Ahead Logging and the Undo/Redo Duality

The log is append-only. Each write record carries a before-image (what the row looked like before the write, used for undo) and an after-image (what it looks like after, used for redo). A per-record back-pointer to the previous record of the same transaction forms a singly linked undo chain.

Undo walks that chain backwards: starting from the transaction's last record, follow the back-pointer to the previous write, apply its before-image, write a CLR, then follow the pointer again. A CLR records the position it undid to and points past the already-undone record, so that a crash in the middle of undo is handled identically on the next recovery run. That is precisely what makes the recovery idempotent: the same log replayed twice produces the same store.

### Strict Two-Phase Locking

Plain two-phase locking (2PL) splits a transaction into a growing phase (acquire locks) and a shrinking phase (release locks), with the single rule that the shrinking phase begins only after the growing phase ends. Strict 2PL delays the shrinking phase entirely until commit or abort. This is what prevents cascading aborts: if a transaction could release a lock early, a second transaction could read the value, and then if the first aborts the second has read a value that never officially existed. Holding the locks to commit makes that impossible.

The two-mode compatibility core (held versus requested):

```
Held \ Requested | Shared | Exclusive
-----------------+--------+----------
Shared           | OK     | FAIL
Exclusive        | FAIL   | FAIL
```

A transaction may upgrade from shared to exclusive only when it is the sole holder. Two transactions both holding shared locks cannot both upgrade — each waits for the other to drop its shared lock, which is itself a deadlock, and the standard cure is the update lock of a later exercise.

### Deadlock Detection via the Wait-For Graph

When a transaction blocks on a lock it records edges in a wait-for graph: one directed edge from itself to every transaction currently holding the contested lock. A background goroutine runs depth-first search over a snapshot of that graph on a fixed interval. If it finds a cycle, the youngest transaction in the cycle (the highest transaction id, hence the one that has done the least work) is the victim. The victim is aborted and every lock condition variable is broadcast; the victim's own goroutine then observes its aborted status on the next return from a conditional wait and propagates a deadlock error to the caller, which must roll back any partial work.

### ARIES Crash Recovery: Analysis, Redo, Undo

ARIES recovery runs three phases, in order:

- Analysis scans the log (from the last checkpoint, or from the beginning when there is none) to classify each transaction as committed, aborted, or still active at crash time, tracking the last log record seen for each one.
- Redo replays every log record in forward order, re-applying writes (including CLRs). After redo the store reflects the exact state at the instant of the crash, in-flight uncommitted writes included.
- Undo, for each transaction that was active at crash time, walks its undo chain in reverse and restores before-images, writing CLRs as it goes so that a crash during recovery is handled identically on re-run.

Redo always precedes undo. Redo brings the store up to the crash state; undo then removes the uncommitted changes from that state. Running undo first would undo changes that redo would then re-apply — a logical contradiction.

### Savepoints and Partial Rollback

A savepoint records the log position and a snapshot of the held lock set at a point inside a transaction. Rolling back to a savepoint does three things: it walks the undo chain backwards from the current position to the savepoint position, applying before-images only for this transaction's own write records; it releases the locks acquired after the savepoint by comparing the current lock set against the snapshot; and it truncates the savepoint list to discard any nested savepoints created after this one. The transaction remains active and can continue. The lock-set snapshot must be a value copy, never a pointer alias to the live map, or the comparison sees a moving target and releases nothing.

### The Full Lock-Compatibility Matrix, Including Intention Locks

The shared/exclusive matrix above is the two-mode core. Production engines add three intention modes — IS, IX, and SIX — so that a transaction can lock a coarse object (a whole table) without forcing every fine-grained holder (individual rows) to be checked one by one. An intention lock on a table announces "I hold, or will hold, a finer lock somewhere inside." Before locking a row in shared mode a transaction first takes IS on the table; before locking a row in exclusive mode it first takes IX. SIX (shared-and-intention-exclusive) means "I read the whole table (S) and will write some of its rows (IX)" — the natural mode for a full scan that updates a few matching rows.

The full five-mode compatibility matrix (held versus requested) is symmetric:

```
Held \ Req | IS    IX    S     SIX   X
-----------+----------------------------
IS         | OK    OK    OK    OK    FAIL
IX         | OK    OK    FAIL  FAIL  FAIL
S          | OK    FAIL  OK    FAIL  FAIL
SIX        | OK    FAIL  FAIL  FAIL  FAIL
X          | FAIL  FAIL  FAIL  FAIL  FAIL
```

Reading it: IS conflicts only with X — a reader's intent does not block another reader's intent, a writer's intent, a shared scan, or a SIX scan, only an exclusive lock on the whole table. IX conflicts with everything except IS and IX, because two writers may proceed together only when their row-level locks, checked one level down, do not overlap. S blocks any intent to write (IX, SIX, X). This is the matrix the hierarchical-locking exercise encodes and tests directly.

### Basic, Strict, and Rigorous Two-Phase Locking

2PL has several variants, and they differ in exactly which guarantees they buy:

- Basic 2PL: one growing phase, then one shrinking phase. It guarantees conflict-serializability but not recoverability or cascadelessness — a transaction can release a lock, let another read the value, then abort.
- Conservative (static) 2PL: acquire every lock before the transaction begins. It is deadlock-free because there is no incremental waiting, but it requires knowing the full lock set up front, which is rarely practical.
- Strict 2PL: hold all exclusive locks until commit or abort. This gives cascadelessness (no transaction reads uncommitted data) and recoverability (a transaction commits only after every transaction it read from has committed). Shared locks may be dropped earlier.
- Rigorous 2PL: hold all locks, shared and exclusive, until commit or abort. This is the strongest common variant and the one the exercise manager implements — Commit and Abort release the entire lock set, never piecemeal. Rigorous 2PL makes the commit order equal to the serialization order, which simplifies reasoning and distributed coordination.

Guarantee summary:

```
Variant       | Serializable | Recoverable | Cascadeless
--------------+--------------+-------------+------------
Basic 2PL     | yes          | no          | no
Strict 2PL    | yes          | yes         | yes
Rigorous 2PL  | yes          | yes         | yes
```

All 2PL variants remain vulnerable to deadlock; serializability does not imply deadlock-freedom.

### Deadlock Handling: Detection, Prevention, Timeout

There are three strategies, in increasing order of how eagerly they act:

- Detection (used by the exercise manager): build a wait-for graph — an edge from Ti to Tj when Ti waits for a lock Tj holds — and periodically search it for a cycle with depth-first search. A cycle is a deadlock; break it by aborting a victim. Victim-selection policies include the youngest transaction (least work lost — the exercise's choice), the one holding the fewest locks, the one with the fewest log records, or the one cheapest to roll back. A good policy also avoids repeatedly victimizing the same transaction (starvation), often by counting how often a transaction has already been chosen.
- Prevention via timestamps (the deadlock-prevention exercise): give every transaction a start timestamp; smaller means older. On a conflict the scheme decides deterministically by age, so that waits can only ever point one way around the timestamp order, which makes a cycle impossible.
  - wait-die (non-preemptive): an older requester waits for a younger holder; a younger requester dies (aborts and restarts). Waits go old-to-young only.
  - wound-wait (preemptive): an older requester wounds (aborts) a younger holder and takes the lock; a younger requester waits. Waits go young-to-old only.
  Both restart the aborted transaction with its original timestamp, so a transaction grows older relative to new arrivals every time it restarts and eventually becomes the oldest, at which point it can no longer be aborted — that is what rules out starvation.
- Timeout: abort any transaction that waits longer than a threshold. Trivial to implement and sometimes the only option in a distributed setting, but it produces false positives (a long but progressing transaction is killed) and is sensitive to threshold tuning.

The direction rule is the whole point of prevention, and it is easy to invert by accident, which silently reintroduces the deadlock it was meant to remove. The prevention exercise tests the direction of each scheme explicitly.

### Isolation Levels and the Anomalies They Permit

ANSI SQL defines four isolation levels by which read anomalies they forbid:

```
Level            | Dirty read | Non-repeatable read | Phantom
-----------------+------------+---------------------+---------
Read Uncommitted | allowed    | allowed             | allowed
Read Committed   | forbidden  | allowed             | allowed
Repeatable Read  | forbidden  | forbidden           | allowed
Serializable     | forbidden  | forbidden           | forbidden
```

The anomalies, in lock terms:

- Dirty read: reading another transaction's uncommitted write. Prevented by holding exclusive locks until commit (strict 2PL).
- Non-repeatable read: reading a row twice and getting two values because another transaction updated and committed it in between. Prevented by holding shared locks on read rows until commit.
- Phantom: a predicate query ("all rows where balance > 100") returns a different set on re-execution because another transaction inserted or deleted a matching row. Row locks cannot prevent this — the new row had no lock to take. Prevention needs predicate locks or, in practice, next-key / index-range locks.
- Lost update: two transactions read the same value, each computes a new value from it, and the second write silently overwrites the first. It is absent from the original ANSI table; the Berenson et al. critique adds it, along with write skew, to argue that the ANSI anomaly list is incomplete and its English definitions ambiguous.

Berenson et al. ("A Critique of ANSI SQL Isolation Levels", 1995) further show that snapshot isolation — widely deployed and stronger than Read Committed — does not fit the ANSI lattice at all: it prevents all three ANSI anomalies yet still permits write skew, so it is neither Repeatable Read nor Serializable in the ANSI sense. The exercise manager, holding both shared and exclusive locks to commit (rigorous 2PL), provides serializability for the rows it locks; preventing phantoms as well would require predicate locking, which the row-granular lock table here does not implement.

## Common Mistakes

### Releasing Locks Before Flushing the Log

Wrong: in Commit, calling the lock-release step before the log flush. Another transaction then acquires the lock, reads the data, and commits its own change on top — and if the engine crashes before the first transaction's commit record is durable, that first commit is lost while the second is not. The second transaction is now committed on top of a ghost.

Fix: flush the log first, then release locks. The ordering in Commit is fixed: write the COMMIT record, flush, release locks, remove the transaction from the active set.

### Using `if` Instead of `for` Around a Conditional Wait

Wrong: guarding a `cond.Wait()` with a single `if` that checks the lock condition once. A `sync.Cond.Wait` can return spuriously, without any matching `Signal` or `Broadcast`, and a one-shot `if` lets the goroutine proceed as if it owns the lock when the condition may still be false.

Fix: always wrap the wait in a `for` loop that rechecks the predicate on every wakeup. As the `sync` documentation states, "the caller should not assume that the condition is true when Wait returns." This applies to every lock table in this lesson.

### Sharing the Lock-Set Map in a Savepoint

Wrong: storing the live lock-set map in the savepoint by reference. Locks acquired after the savepoint are then added to the same map, so when the rollback compares the current lock set against the savepoint's snapshot it sees one identical map: no difference, no locks released.

Fix: copy the map by value at savepoint-creation time, so the snapshot is frozen and the post-savepoint locks are detectable as additions.

### Re-Undoing CLRs During Recovery

Wrong: during the undo phase, treating a compensation log record like an ordinary write and applying its image, which either deletes the row (a CLR has no before-image of its own) or restores a stale value, corrupting the recovered state.

Fix: when the undo walk encounters a CLR, follow its "undo-next" pointer to skip past the record it already compensated. That skip is exactly what makes recovery idempotent: a crash mid-undo, followed by a fresh recovery, lands on the same result.

---

Next: [01-transaction-manager-core.md](01-transaction-manager-core.md)
