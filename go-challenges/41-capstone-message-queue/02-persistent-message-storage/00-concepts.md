# 2. Persistent Message Storage — Concepts

An in-memory queue forgets everything the moment the process exits. A durable message queue cannot: a broker that loses acknowledged messages on restart is worse than no broker at all, because producers believe their data is safe. The fix is the same structure that Kafka, RabbitMQ's stream queues, and every event-sourcing system reach for — an append-only, segmented log on disk. Messages are written sequentially to bounded segment files, located on read by a sparse offset index and a binary search, served from a memory map once a segment is sealed, and repaired on restart by truncating any half-written record at the tail. This file is the conceptual foundation: read it once and you have everything needed to reason through the exercises, which build the storage engine piece by piece as independent, self-contained Go modules.

## Concepts

### Why Append-Only, and Why a Log

A message queue has an unusual access pattern. Writes are almost always appends — a new message goes to the end. Reads are almost always sequential — a consumer reads forward from where it left off. Deletes are coarse — whole ranges of old messages expire together by age or by a consumer's committed offset. No workload here benefits from in-place updates or random deletion, which is exactly the workload a B-tree or a mutable file is built for and pays for.

An append-only log matches the access pattern exactly. Appending is the cheapest write a disk can do: the head (or the SSD's flash translation layer) never seeks, bytes stream out in order, and the OS page cache turns a burst of small appends into a few large sequential writes. Sequential writes are the one thing spinning disks and SSDs are both fast at, often by an order of magnitude over random writes. And because nothing is ever overwritten, a reader can scan a region while a writer appends to the end with no lock between them, and a crash can only ever damage the very last record — never silently corrupt one in the middle.

The price is that the log only grows. That is what segmentation and retention exist to bound.

### The Segmented Log Model

A single ever-growing file has two problems: recovery would have to scan it from byte zero on every restart, and old data could never be reclaimed without rewriting the whole file. A segmented log solves both by splitting the log into a directory of bounded, append-only files named by the offset of their first message:

```text
data/
  00000000000000000000.log   base offset 0
  00000000000000001024.log   base offset 1024
  00000000000000002048.log   base offset 2048
```

Messages are always appended to the active (last) segment. When the active segment exceeds a size limit, it is sealed and a new one begins at the next offset. The zero-padded fixed-width names sort lexicographically into offset order, so the directory listing alone reconstructs the segment sequence.

Two invariants flow from this. First, the global offset of a message is the base offset of its segment plus its position within that segment, so offsets are monotonically increasing across the whole log and are never reused — which is what lets a consumer store a single integer ("I have read through offset N") and resume exactly. Second, only the active segment ever changes. Every sealed segment is immutable, and immutability is the property that makes memory mapping and lock-free concurrent reads safe: a reader scanning a sealed segment can never observe a half-written record, because no one is writing to it.

### Binary Framing and the CRC32 Checksum

A log file is a flat byte stream with no built-in record boundaries. The on-disk format must supply them. Each message is encoded into a self-describing record: a fixed-width header giving the offset, timestamp, and the lengths of the variable fields, then the key, value, and headers, then a trailing CRC32 checksum over everything before it.

```text
offset       uint64  8 bytes
timestamp    int64   8 bytes
key_len      uint32  4 bytes
key          []byte  key_len bytes
value_len    uint32  4 bytes
value        []byte  value_len bytes
header_count uint16  2 bytes
  (per header: key_len uint16, key bytes, val_len uint16, val bytes)
crc32        uint32  4 bytes   IEEE checksum of all bytes above
```

The fixed-width prefix is what makes the record *framed*: a reader can read the header, learn the variable lengths, and read exactly the body — never one byte into the next record. This two-pass read is the same technique TCP, gRPC, and Kafka's own wire protocol use.

On top of the record, the segment writer prepends a 4-byte big-endian length. That length lets recovery walk the file record by record with two `io.ReadFull` calls each — one for the length, one for the body — without decoding every field. The CRC32, computed with `hash/crc32.ChecksumIEEE` over all bytes preceding it, is the integrity guarantee: a single flipped bit anywhere in the record changes the checksum, so a corrupt record is rejected rather than silently believed. The checksum is validated *before* any field is interpreted, so corruption can never produce a partially populated message.

### The Sparse Offset Index and O(log n) Seek

A consumer asks for "the message at offset 5,000,000." Without an index, the only way to find it is to scan the segment from the start, decoding every record — O(n) in the worst case. Storing one index entry per message would make the lookup O(log n), but the index would then be as large as the log itself, defeating the purpose.

The resolution is a *sparse* index: store one entry every N messages (a few thousand, typically). Each entry maps a relative offset to a byte position in the log file:

```text
relative_offset  (absolute offset - segment base offset)
file_position    byte offset of that record within the .log file
```

A lookup for absolute offset O is then two steps. First, binary-search the in-memory entries for the largest one whose relative offset is at most O − base — `sort.Search` finds the first entry strictly greater, so the answer is one before it. Second, seek to that entry's file position and scan forward, decoding records, until the target offset appears. The binary search is O(log n) over the sparse entries; the forward scan reads at most N records (the index interval). For N a few thousand and kilobyte records, the worst-case scan is a few megabytes — trivially fast and served from the page cache. The sparse index is the classic space-for-time knob: a larger interval shrinks the index and lengthens the scan; a smaller interval does the reverse.

The index can live purely in memory, rebuilt by scanning the log once on open, or be persisted to its own `.index` file as Kafka does. Rebuilding on open is simpler and immune to log/index disagreement after a crash; persisting avoids the rescan on a large segment. Both are legitimate; the exercises rebuild in memory because the correctness story is cleaner.

### Memory-Mapped Reads for Sealed Segments

Reading a record with `read(2)` copies bytes from the kernel page cache into a user buffer on every call. Memory mapping skips that copy: `syscall.Mmap` maps the file's bytes directly into the process address space, and a read becomes an ordinary slice access against the page cache, with the kernel faulting in pages on demand.

```go
data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
```

The hard constraint is that a mapping covers exactly the file size at the moment of the `mmap` call. The active segment is always growing, so its newest bytes lie past the mapped region — mapping it would give a reader a stale, truncated view. This is precisely why segments are sealed: once a segment is rolled, its size is fixed forever, so it can be mapped once and that mapping cached for the life of the process. The active segment uses ordinary file reads; only sealed segments are mmap candidates. `syscall.Munmap` releases the mapping; it must be called before the file is closed or the segment deleted.

`syscall.Mmap` with `PROT_READ` and `MAP_SHARED` is a POSIX interface, so this targets Linux and macOS. On Windows the equivalent is `golang.org/x/sys/windows.MapViewOfFile`, or one falls back to regular file reads — every other part of the engine is unchanged.

### Fsync Policies: Durability Versus Throughput

A successful `f.Write` does not mean the bytes are on the platter. It means they are in the kernel page cache, which the OS will flush on its own schedule. A power loss before that flush discards everything still cached, even though every `Write` returned without error. The only call that forces durability is `f.Sync` (fsync), which blocks until the page cache — and, on a correct implementation, the drive's own cache — is flushed.

That makes fsync frequency a tunable trade-off rather than a fixed choice:

```text
SyncEveryMessage   fsync after every append; safest, slowest (~100 us per message on SSD)
SyncEveryN         fsync once per N appends; the cost is amortized across the batch
SyncOSDefault      never fsync explicitly; the OS flushes whenever it chooses
```

`SyncEveryMessage` loses nothing on a crash but caps throughput at one fsync per message. `SyncEveryN` and `SyncOSDefault` are orders of magnitude faster and risk losing only the messages written since the last flush. Most production systems sit at the relaxed end: Kafka's `log.flush.interval.messages` defaults to effectively never, deferring to OS flushing and to replication across brokers for durability. The right point depends on whether a lost tail of recent messages is recoverable from elsewhere (a replica, a re-sending producer) or is gone for good.

A macOS note worth carrying: a plain `fsync(2)` on macOS only pushes bytes out of the kernel, not out of the drive's own write cache, so the truly durable call is the `F_FULLFSYNC` fcntl. Since Go 1.12, `os.File.Sync()` issues `F_FULLFSYNC` for you on darwin, so a single `Sync()` is the strongest durability the platform offers — and also genuinely slow on Apple SSDs, which is exactly why batching fsyncs matters.

### Crash Recovery: Truncating the Torn Tail

A crash in the middle of an append leaves a partial record at the end of the active segment: a length prefix with no body, a body cut short, or a complete-looking frame whose CRC fails because only some of its bytes were flushed. Recovery must treat this torn tail as expected, not as data loss, and repair it in place so the next open can append cleanly.

The algorithm scans the active segment from byte zero:

1. Read the 4-byte length, then the record body, validating the CRC32.
2. Track the file position at the end of the last record that decoded cleanly.
3. Stop at the first failure — a short read on the length or body, a length that runs past the file, or a CRC mismatch — and `f.Truncate` the file to the last clean position.
4. Rebuild the in-memory offset index and the next-offset counter from the records that survived.

The positional rule is what makes this correct: a bad record at the very tail of the log is a crash artifact and is truncated; a bad record anywhere earlier means a previously-acknowledged write is now unreadable, which is real corruption and must be reported, not silently dropped. The invariant after recovery is that every readable record has a valid checksum and a strictly increasing offset, and the next append continues exactly where the durable log ended. This is the same recovery strategy used by SQLite's WAL, RocksDB, and Kafka.

## Common Mistakes

### Memory-Mapping the Active Segment

Wrong: calling `mmap` on the segment that is still being appended to. A mapping is fixed to the file size at the moment of the call, so reads of records written after the mapping land past its end and return zeroes or fault. The bug is invisible until the segment grows past its size at seal time.

Fix: only `mmap` a segment after it is sealed and its size is final. The active segment always reads through the file API; sealed segments read through their map.

### Using sort.Search's Result Directly as the Index Entry

Wrong: taking `i := sort.Search(n, func(i int) bool { return entries[i].relativeOffset > rel })` and reading `entries[i]`. `sort.Search` returns the first entry strictly *greater* than the target, which sits one past the entry you want, so the scan starts after the target offset and never finds it.

Fix: the answer is `entries[i-1]`, the largest entry at or below the target. Because the segment always records an index entry for its first message (relative offset 0), `i` is never zero for an in-range offset, so `i-1` is always valid.

### Checksumming the Bytes That Include the Checksum

Wrong: computing the CRC32 over a buffer that already contains the CRC field. The field is part of the very value it is supposed to cover, so the checksum can never validate on read without first zeroing the slot on both sides — and the day one side forgets, every record reports a false mismatch.

Fix: compute `crc32.ChecksumIEEE` over only the bytes preceding the checksum, append it last, and on decode recompute over that same prefix range. Neither side ever zeroes anything.

### Trusting a Length Prefix Without Bounding It

Wrong: reading a 4-byte length from a possibly-torn file and immediately doing `make([]byte, recLen)`. A crash can leave a garbage length claiming gigabytes, and the allocation either exhausts memory or the subsequent read scans far past the real data.

Fix: before allocating, reject a length that is smaller than the minimum record or that would run past the current file size. At the tail of the log, such a length is a torn write — stop and truncate rather than trust it.

### Treating a Corrupt Tail Record as Fatal

Wrong: returning an error from recovery when the last record of the active segment fails its CRC. Every crash mid-append produces exactly this, so a fatal reaction means no log that ever crashed can be reopened — the opposite of what durability is for.

Fix: a CRC failure or short read at the tail of the active segment is an expected crash artifact; truncate to the last clean record and continue. Only corruption earlier in the log, in already-sealed data, is a real, reportable error.

---

Next: [01-message-encoding.md](01-message-encoding.md)
