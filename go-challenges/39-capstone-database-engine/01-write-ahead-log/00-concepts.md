# 1. Write-Ahead Log — Concepts

A write-ahead log is the durability backbone of every serious storage engine. The governing rule is "force log at commit": before any change reaches a data page on disk, its intent must land in a sequential append-only log that survives crashes. The hard parts are designing a binary frame format that survives partial writes, deciding when to call fsync (and when to batch calls with group commit for throughput), and implementing a recovery algorithm that can handle a crash at any byte offset without treating a torn tail write as data loss. This file is the conceptual foundation: read it once and you will have everything you need to reason through each of the exercises, which build the log piece by piece as independent, self-contained Go modules.

## Concepts

### The WAL Protocol: Write First, Apply Second

The protocol can be stated in one sentence: no change is ever applied to a data page unless the corresponding log record is first safely on disk. This gives the recovery manager enough information to redo every committed operation and ignore every uncommitted one, regardless of where the process crashed.

The three invariants that make this work:

1. A record is written to the log before the corresponding data page is modified in memory.
2. A transaction's commit record is fsynced before the caller receives an acknowledgment.
3. During recovery, every record between the last checkpoint and the end of the log is replayed in LSN order.

The third invariant is why LSNs must be strictly monotonic: the recovery algorithm uses them to determine ordering and to skip records already reflected in data pages (LSN <= page LSN means "already applied").

### The Recovery Taxonomy: Steal/No-Steal and Force/No-Force

The reason a WAL exists at all is best understood through the two-axis buffer-pool taxonomy that CMU 15-445 and the ARIES paper use to classify every recovery scheme:

- Steal vs no-steal: may a dirty page belonging to an uncommitted transaction be written (stolen) back to disk before the transaction commits? No-steal forbids it; steal allows it.
- Force vs no-force: must every page a transaction dirtied be forced to disk at commit time? Force requires it; no-force does not.

The four corners trade recovery simplicity against runtime cost:

- No-steal + force is the easiest to recover (no undo, no redo) but the slowest at runtime: commit must flush every dirtied data page, and the buffer pool can never evict an uncommitted page, so a transaction larger than RAM cannot run.
- Steal + no-force is the fastest at runtime and is what essentially every production engine, including PostgreSQL and SQLite, chooses. Its price is that recovery must do both: redo committed work whose data pages never reached disk (because of no-force) and undo uncommitted work whose dirty pages did reach disk (because of steal).

WAL is precisely the mechanism that makes steal + no-force safe. The write-ahead invariant has two halves that map one-to-one onto the two axes:

1. The undo half (makes steal safe): a page's log record must be durable before that dirty page is allowed to overwrite its on-disk copy. If the engine then crashes, recovery has the undo information to roll the stolen page back.
2. The redo half (makes no-force safe): a transaction's commit record must be durable before commit is acknowledged. If the engine crashes with the data pages still in cache, recovery has the redo information to reapply them.

The code in this lesson implements the durability substrate for both halves: `Append` followed by fsync is the primitive that "make this log record durable" is built on. The data-page write path (the part that consults page LSNs and enforces "log record durable before page write") lives in the buffer pool manager, which is the next layer up the stack.

### Record Format: Framing and CRC32 Integrity

A WAL record needs three properties: it must be self-delimiting (the reader must know where one record ends and the next begins), it must be detectable as corrupt if bits were flipped during a partial write, and it must carry enough metadata to support redo and undo during recovery.

The binary layout used in this lesson:

```
Offset  Size  Field
     0     4  payloadLen (uint32, little-endian): byte count of the Payload field
     4     4  crc32 (uint32, little-endian): IEEE CRC32 of bytes [8:end]
     8     8  lsn (uint64, little-endian): log sequence number
    16     8  txid (uint64, little-endian): transaction identifier
    24     1  type (byte): RecordType enum (INSERT=1, UPDATE=2, DELETE=3, ...)
    25  plen  payload: variable-length operation data
```

Total header: 25 bytes. The CRC covers bytes [8:end] (lsn, txid, type, payload). The payloadLen and CRC fields at [0:8] are the framing envelope and are intentionally excluded from the checksum: the decoder reads them unconditionally, and corrupting them makes the record unreadable (not silently wrong).

Length-prefixed framing means the decoder needs exactly one `io.ReadFull` call for the 4-byte length, then one more for the variable body. This lets the recovery scanner advance through records without seeking.

### LSN Design: Monotonicity, Page LSN, and the Flushed Frontier

An LSN is more than a counter; it is the total order on which the whole recovery argument rests. Three design properties matter.

Strict monotonicity. Every record gets an LSN strictly greater than the previous one, assigned under the lock so concurrent appenders never collide. In this lesson `nextLSN` is a simple incrementing counter; `Open` resumes it by scanning the last segment so a restart never reuses a number. Production engines such as PostgreSQL instead make the LSN the byte offset of the record in the logical log (an `XLogRecPtr`), which gives monotonicity for free and lets any LSN double as a physical position. A counter is simpler to teach; an offset-LSN is what you graduate to when the WAL and the on-disk layout must agree on "where."

Page LSN and the idempotence of redo. Each data page stores the LSN of the last log record that modified it (its pageLSN). During redo, recovery compares each log record's LSN against the pageLSN of the page it touches: if `record.LSN <= page.pageLSN`, the change is already reflected on disk and is skipped. This single comparison is what makes redo idempotent, which is the property that lets recovery run, crash halfway, and run again to the same result. Without it, replaying a log twice would double-apply increments. This lesson's records carry the LSN needed for that comparison; the comparison itself belongs to the page layer.

The flushed frontier. At any instant there is a highest LSN that is durably on disk (call it the flushedLSN) and a possibly higher highest LSN that has only been appended to the page cache. The commit rule restated in LSN terms is: a commit at LSN L may be acknowledged only once flushedLSN >= L. Group commit, below, is entirely about advancing flushedLSN for many waiters with one fsync. The default (per-append) path in this lesson keeps flushedLSN equal to the last appended LSN at all times, which is correct but maximally expensive.

### Segment Files, Rotation, and fsync

Writing everything to one giant file creates two problems: crash recovery must scan from the beginning, and old records that are no longer needed cannot be reclaimed without rewriting the file. Segment files solve both: each segment is a bounded, named, append-only file; recovery scans only segments newer than the last checkpoint; segments fully below the checkpoint LSN can be deleted.

Naming convention: `wal-000000.log`, `wal-000001.log`, ... Zero-padded sequence numbers sort lexicographically and let the recovery scanner enumerate them with `os.ReadDir`.

The rotation invariant: fsyncing the segment before closing it guarantees that the segment boundary itself is durable. A crash between the sync and the close of the old segment leaves the segment cleanly readable.

`os.File.Sync()` is Go's portable durability call. The OS-level fact is real: on macOS a plain `fsync(2)` only pushes bytes out of the kernel page cache and does not force the drive to flush its own write cache, so a power loss can still lose acknowledged writes. The fix at the syscall level is the `F_FULLFSYNC` fcntl. The detail that is easy to get wrong is who issues it. Since Go 1.12 (golang/go#26650), `os.File.Sync()` on darwin already issues `F_FULLFSYNC` for you; it falls back to plain `fsync(2)` only when the filesystem returns `ENOTSUP`, notably SMB network mounts (golang/go#64215). On current Go (1.24+, verified against the 1.26 toolchain `src/internal/poll/fd_fsync_darwin.go`), a single `Sync()` is therefore the strongest durability the platform offers; you do not add an `F_FULLFSYNC` path by hand. The practical consequence is the opposite of free: `F_FULLFSYNC` is genuinely slow on Apple SSDs, which is exactly why group commit (below) matters even more on macOS than on Linux.

### fsync vs fdatasync: What You Actually Have to Flush

`fsync(2)` flushes a file's data and all of its inode metadata (size, timestamps, block pointers). `fdatasync(2)` flushes the data and only the metadata a later read needs to find that data; per the Linux man page it skips an `st_mtime`/`st_atime`-only update but still flushes a change to `st_size`. For a WAL this distinction is worth real latency. An append that grows the file changes `st_size`, so the first sync after a size-extending write must flush metadata under either call. The classic optimization is to preallocate segment files to their full `MaxSegmentSize` up front (for example with `fallocate`), so that ordinary appends only overwrite already-allocated, already-sized bytes; subsequent syncs then carry no metadata and `fdatasync` becomes strictly cheaper than `fsync`. PostgreSQL exposes exactly this choice as `wal_sync_method` (`fdatasync`, `fsync`, `open_datasync`, `open_sync`, `fsync_writethrough`) and preallocates WAL segments for the same reason. Go's `os.File.Sync()` always maps to the full `fsync`/`F_FULLFSYNC` path and gives you no `fdatasync` knob, so a Go engine that wants the cheaper call reaches for `golang.org/x/sys/unix.Fdatasync` directly. This lesson stays on `Sync()` for portability; the trade-off is that every size-extending append pays for a metadata flush.

### Group Commit: Amortizing the fsync Cost

A naive implementation calls `fsync` after every `Append`. On a modern SSD, a single `fsync` costs 0.1–10 ms. At 100 records/s that is negligible; at 10,000 records/s it becomes the bottleneck.

Group commit batches multiple appends into a single fsync cycle:

1. Appenders write their encoded record to a pending queue and block on a per-write `chan error`.
2. A dedicated flusher goroutine wakes on a configurable interval (e.g., 10 ms), drains the pending queue, writes all records in a single pass, calls `fsync` once, then sends the result to each waiting channel.
3. All appenders in the batch unblock simultaneously.

The tradeoff: group commit reduces write latency variance and greatly improves throughput (one `fsync` per interval instead of per record), at the cost of adding up to one interval of additional commit latency. PostgreSQL calls this "commit_delay"; InnoDB calls it "innodb_flush_log_at_trx_commit=2".

The deeper trade-off is latency versus throughput, and it is not symmetric. Let one fsync cost `S` and the arrival rate be `R` commits per second. Without batching, each commit pays `S` of latency and the system tops out at `1/S` commits per second. With a batching window `W`, the flusher emits at most one fsync per `W`, so throughput rises toward `min(R, batch_size/S)` while the latency of an individual commit becomes `wait_for_window + S`, up to `W + S` in the worst case. Increasing `W` strictly trades single-commit latency for throughput; past the point where a full batch already saturates the device, extra `W` buys nothing but latency, which is why PostgreSQL warns that `commit_delay` set too high reduces throughput. The amortization is real only under contention: at one commit in flight, group commit is pure added latency, which is why a good implementation lets a lone committer flush immediately rather than wait out the window.

There are two common designs for the window. The timer design used in this lesson (`GroupCommitInterval`) wakes a flusher on a fixed tick; it is simple and bounds latency at one tick but wastes a partial tick when the queue is already full. The leader/follower design instead elects the first waiter of a batch as the leader, lets it perform the fsync, and has every commit that arrives during that fsync coalesce into the next batch automatically, with no timer at all. The leader/follower form self-tunes: when the device is slow the batches grow, when it is fast they shrink, and the window is exactly "however long the previous fsync took." The leader/follower exercise builds that coalescer in isolation and measures the amortization directly; the baseline WAL ships with the simpler timer design.

### Crash Recovery: Tail Truncation and the CRC32 Invariant

The recovery algorithm has one rule about the tail of the last segment: a partial record there is expected, not fatal. The process might have written 14 of the 25 header bytes before the kernel buffered but did not fsync, then the machine lost power. Those 14 bytes are garbage. Recovery must truncate them and return only the clean records.

Three cases the scanner handles:

- `io.EOF` at the start of a record: clean end of file. No truncation needed.
- `io.ErrUnexpectedEOF` reading the length or body: truncated record. Truncate to `startOffset`.
- CRC mismatch: corrupt record (could be a partial write of a full record). Truncate to `startOffset`.

The key distinction: these three cases apply only to the tail of the final segment. Corruption in the middle of any segment, or in any non-final segment, is a real error: it means records that were acknowledged as durable are now unreadable, and recovery cannot guarantee consistency.

### Checkpoints: Sharp vs Fuzzy, and the Redo Point

A WAL that is never trimmed grows without bound and makes every restart replay the entire history. A checkpoint bounds both. Its job is to establish a redo point: a single LSN such that recovery is guaranteed correct if it replays only from that LSN forward, because everything before it is already reflected in the data files. Once a redo point exists, every segment whose records all lie below it can be deleted. The baseline's `Checkpoint` writes an empty marker record and `Truncate` performs the segment GC; the checkpoint-payloads exercise extends the marker to carry the redo LSN and the set of still-active transactions so recovery knows where to start and whom to undo.

The cost question is what to do with concurrent writers while the checkpoint runs, and it splits into two classic forms.

- A sharp (blocking) checkpoint quiesces the engine: it stops accepting new writes, waits for in-flight transactions, flushes every dirty page, then writes the checkpoint record. Recovery is trivial because the redo point is exactly the checkpoint and no transaction straddles it, but throughput drops to zero for the duration, which is unacceptable on a busy system.
- A fuzzy checkpoint runs concurrently with normal traffic. It records the set of dirty pages and active transactions at the moment it begins and lets writes continue. Because pages dirtied just before the checkpoint may not have been flushed yet, the redo point is not the checkpoint LSN itself but the oldest unflushed (recovery) LSN among those dirty pages. ARIES uses this form: the begin-checkpoint and end-checkpoint records bracket the captured state, and recovery starts its redo scan from the minimum recoveryLSN in the dirty-page table.

The subtlety that makes fuzzy checkpoints correct is exactly the page-LSN comparison from the LSN section: even though redo starts at an LSN earlier than the checkpoint, replaying records whose effects already reached disk is harmless because each is skipped when `record.LSN <= page.pageLSN`. That idempotence is what lets the checkpoint avoid flushing everything synchronously. SQLite's WAL takes a different but related stance: its checkpoint is the act of copying committed WAL frames back into the main database file (it syncs the WAL before the copy and syncs the database before resetting the WAL), and it triggers automatically once the WAL passes a threshold of 1000 pages or when the last connection closes. PostgreSQL's checkpoints additionally interact with `full_page_writes`, which logs an entire page image the first time it is touched after a checkpoint so that a torn page write can be repaired from the WAL rather than from a partially overwritten data file. The shared lesson: the checkpoint policy and the durability format are co-designed, and the redo point is the contract between them.

## Common Mistakes

### Checksumming the Entire Buffer Including the CRC Field

Wrong: computing `crc32.ChecksumIEEE(buf[0:])` after placing a zero in the CRC slot, then embedding the result. The decoder must then zero the same slot before re-computing the checksum.

What happens: the implementation works but the CRC field must be zeroed on both sides (encoder and decoder). This is easy to get wrong; the second `copy` or `binary.Read` that forgets to zero the slot produces a false CRC mismatch.

Fix: checksum `buf[8:]` only. The length and CRC fields at [0:8] are excluded from the checksum by design, so neither side needs to zero anything. The encoder writes the CRC last; the decoder reads the CRC first, then computes over the same range.

### Treating CRC Failure as a Fatal Error During Recovery

Wrong: returning an error from `Recover` when `Decode` returns a CRC mismatch on the last record of the last segment.

What happens: every crash that happened mid-write (the process wrote a partial record and died) leaves a tail with a corrupt CRC. Treating it as fatal means no WAL directory that survived a crash is ever recoverable, which defeats the purpose of the WAL.

Fix: in `readSegment`, distinguish the position of the corruption. A CRC failure at the tail of the last segment is a crash artifact; truncate and continue. A CRC failure anywhere else is unrecoverable data loss and should be returned as an error.

### Deleting the Active Segment in Truncate

Wrong: iterating all segment files and deleting any whose max LSN is below the truncation point, without excluding the currently active segment.

What happens: the active segment is the file the WAL is currently appending to. Deleting it causes the next write to succeed (the OS still has the file open) but the segment is gone from the directory. After the next rotation, the just-deleted records are lost.

Fix: read `w.segSeq` under the mutex before iterating. In the loop, `break` (or `continue`) when `seq >= activeSeq`.

### Using `os.O_TRUNC` Instead of `os.O_APPEND` for Segment Files

Wrong: opening a segment with `os.O_CREATE|os.O_RDWR` without `os.O_APPEND`.

What happens: when the WAL is reopened after a crash, `os.O_RDWR` positions the cursor at offset 0. The next write overwrites the beginning of the segment. All previously written records are silently destroyed.

Fix: always open segment files with `os.O_APPEND`. The kernel atomically moves the write cursor to the end of the file before every write, even when the file is reopened after a crash.

---

Next: [01-record-encoding.md](01-record-encoding.md)
