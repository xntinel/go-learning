# 3. Buffer Pool Manager — Concepts

A database engine performs random I/O at the granularity of fixed-size pages, and reading from disk on every access is prohibitive. The buffer pool manager solves this by maintaining a fixed-size pool of in-memory frames and deciding which pages to keep resident and which to evict when the pool is full. The hard part is not the data structure — it is the invariants. A pinned frame must never be evicted, a dirty frame must be written back before its slot is reused, and the write-ahead log must be durable past a page's LSN before that page is allowed to reach disk (the ARIES no-force/steal protocol). On top of that, many goroutines pin and unpin pages concurrently, so every mutation of the pool's metadata must be atomic with respect to the others. This file is the conceptual foundation: read it once and you will have everything you need to reason through each of the exercises, which build the pool and its policies as independent, self-contained Go modules.

## Concepts

### Pages, Frames, and the Page Table

The unit of I/O in a database is a page — a fixed-size block, 4 KiB here. The buffer pool allocates a fixed number of frames, in-memory slots that each hold exactly one page. A page table (`map[PageID]FrameID`) maps an on-disk page identifier to the frame index that currently holds it.

A cache hit means the page table already has an entry: the frame is used immediately and its reference bit is set. A cache miss means the page is absent: the pool finds a victim frame via the eviction policy, writes it back if it is dirty, reads the requested page from disk into the freed frame, and updates the page table.

```text
Disk:        [page 1][page 2][page 3][page 4]...
                         |
              FetchPage(3) — cache miss
                         v
Frames: [0: page 1][1: page 2, pinned][2: page 4]
pageTable: {1:0, 2:1, 4:2}
Evict frame 0 (page 1, unpinned, refBit=0), load page 3 into frame 0.
```

### Clock-Sweep Eviction

Exact LRU requires a doubly-linked list updated on every access, which is expensive under high concurrency. Clock sweep approximates LRU with a single reference bit per frame. Each frame carries a reference bit that a cache hit sets. A clock hand sweeps frames in circular order looking for a victim. Pinned frames (`pinCount > 0`) are skipped unconditionally. If a frame's reference bit is set, the hand clears it and advances, granting that frame a second chance. If the reference bit is already clear, the frame is evicted.

Two full sweeps guarantee termination when the pool is not exhausted: the first sweep clears reference bits, the second finds a victim. The algorithm is O(pool size) in the worst case and O(1) amortized for workloads with temporal locality. PostgreSQL uses clock sweep for exactly these reasons.

### Pin Count and the PageGuard Pattern

A pin count tracks how many callers are actively using a frame. `FetchPage` and `NewPage` increment it; `UnpinPage` decrements it. A frame with `pinCount > 0` is exempt from eviction. The pin count is a reference count, not a boolean, and the core invariant is that a positive pin count makes a frame unevictable.

Why pinned pages must be unevictable: a caller that holds a guard also holds a pointer into the frame's buffer. Evicting that frame means reusing the slot for a different page, overwriting the bytes under the caller's pointer. The caller would then read another page's contents, or write its update into the wrong page. Pinning is the buffer-pool analog of holding a reference so the storage cannot be repurposed — the same role `defer f.Close()` plays for a file descriptor, or RAII lifetime in C++.

Forgetting to unpin is the single most common bug: pin counts never reach zero and the pool exhausts under load. The PageGuard pattern wraps the frame in a struct whose `Unpin` method the caller defers immediately, mirroring `defer f.Close()` for files: fetch, check the error, then `defer guard.Unpin(false)` on the next line, and work with the guarded data in between. The contract is symmetric and must balance exactly: each successful fetch pairs with exactly one unpin. A missing unpin leaks a pin and eventually exhausts the pool; a double unpin drives the count negative and corrupts eviction eligibility. The guard exists precisely to make the unpin lexically scoped and hard to forget.

### WAL-Before-Page (No-Force, Steal)

ARIES uses two policies that together minimize I/O without sacrificing crash safety. No-force means a dirty page need not reach disk at transaction commit; recovery uses redo. Steal means a dirty page can be evicted before its transaction commits; recovery uses undo.

Both require the WAL protocol: before writing a dirty page to disk, the WAL record that last modified that page — identified by its pageLSN — must already be on durable storage, that is `pageLSN <= flushedLSN`. If `pageLSN > flushedLSN`, the buffer pool must force the log forward first. A crash after the page write but before the WAL write would leave a disk page with no redo record for that modification, making recovery impossible. Steal needs the log so recovery can undo the uncommitted change; no-force needs the log so recovery can redo a committed change whose page never reached disk. Drop the protocol and a crash between the page write and the log write leaves a disk page with no recovery record — unrecoverable.

### Dirty-Page Write-Back and Checkpoints

A frame's dirty bit is set on the first write after load and cleared by write-back. Write-back happens lazily on eviction or eagerly on an explicit flush or a checkpoint. Lazy write-back under a steal policy is what lets a buffer pool absorb many updates to a hot page with a single disk write. Every write-back path, lazy or eager, runs through the same WAL-before-page check, so the protocol is enforced in one place rather than scattered across call sites.

A checkpoint bounds recovery time by flushing dirty pages and recording how far back redo must start. A simple sharp checkpoint write-backs every dirty frame, each through the WAL-before-page path, and returns the highest pageLSN written as the checkpoint LSN — the point up to which all in-memory updates are now durable. The property worth proving is twofold: no frame remains dirty afterward, and at the instant of every page write the WAL had already been flushed to at least that page's LSN.

### Concurrency Design: Two Latch Tiers

The pool uses two layers of locking for two distinct kinds of state, and conflating them is a classic source of either races or contention.

The coarse pool latch (a `sync.Mutex`) protects the metadata: the page table, the free list, the clock hand, and the per-frame pin counts and bits. Mutations here are short — a map insert, a counter bump — so the critical section stays tiny.

The fine per-frame latch (a `sync.RWMutex`) protects the contents of one frame's page buffer. Multiple readers share it; a writer needs it exclusively. This latch is held across the actual data access, which can be long, and must not be a single global lock or every read serializes.

Two rules keep this safe. First, never hold the pool latch across disk I/O on the read path: on a cache miss, pin the frame inside the lock, release the pool latch, then call `ReadPage`, then re-acquire the latch to update the page table. Pinning before the release prevents a concurrent sweep from claiming the same frame during the read. A slow disk read with the pool latch held would serialize every concurrent fetch, unpin, and flush across all goroutines. Second, observe a consistent lock order — pool latch then frame latch, never the reverse — to avoid deadlock; the guard enforces this by exposing the frame latch only after the pool latch has been released. The write-back path intentionally holds the pool latch during its single synchronous page write, because those paths are less frequent than cache-miss reads and the lock scope stays bounded. Production engines push the metadata tier further: PostgreSQL partitions its buffer mapping table behind many partition locks and protects each buffer header with an atomic state word plus a separate content lock, so independent pages contend on neither a single map latch nor a single header.

### Eviction-Policy Trade-offs: LRU, CLOCK, LRU-K, 2Q

The replacement policy decides which unpinned frame to reuse on a miss. The choice is a trade-off between accuracy, per-access cost, and resistance to pathological workloads, and it is worth factoring behind one interface so the policy is swappable while the pool's correctness invariants stay fixed.

LRU (least recently used) is the accuracy baseline: evict the page untouched for the longest. Exact LRU needs a doubly-linked list moved on every hit, and that list is a single hot data structure whose latch becomes the bottleneck under concurrency. LRU's fatal weakness is sequential flooding: a one-time scan of a large table touches each cold page exactly once, making those pages the most-recently-used and pushing the genuinely hot working set out, so the next evictions discard hot pages the query is about to reuse.

CLOCK approximates LRU with one reference bit per frame and a sweeping hand. A hit only sets a bit — no list surgery — so it scales far better under concurrency, which is exactly why PostgreSQL uses it. It is still flooding-vulnerable: a scan sets the reference bit on every cold page it touches, buying each a second chance and delaying eviction of the cold set.

LRU-K (O'Neil, O'Neil, Weikum, 1993) remembers the last K reference timestamps per page and evicts by backward K-distance — the time since the K-th most recent access. A page referenced fewer than K times has an infinite K-distance and is evicted first. With K = 2, a scan page (one reference) is always preferred for eviction over a working-set page (two or more references), so LRU-2 resists sequential flooding by construction. The cost is per-page history and a tunable correlated-reference period so bursts of accesses to the same page within a short window count as one logical reference. This is the policy the CMU 15-445 buffer-pool project asks students to build.

2Q (Johnson, Shasha, 1994) gets most of LRU-K's flooding resistance at O(1) cost using two queues: a FIFO probationary queue for first-touch pages and an LRU hot queue. A page graduates to the hot queue only on a second reference, so scan pages live and die in the probationary queue without polluting the hot set. ARC (adaptive replacement cache) is a later self-tuning relative in the same family.

In replacer terms, the eviction candidate set is exactly the frames whose pin count is zero and that the pool has marked evictable: the pool marks a frame evictable only when its last pin is released, so a policy never even considers a pinned frame. The same three operations — record an access on a pin or hit, mark evictable on the last unpin, ask for a victim on a miss — describe CLOCK, LRU, and LRU-K alike, which is why they slot behind one interface.

### Read-Ahead Prefetch

A sequential scan that fetches page-by-page pays a cache miss on every page. Read-ahead warms the pool before the scan touches the pages, by fetching then immediately unpinning each page so a later fetch finds it cached. Prefetch deliberately holds no pins: read-ahead warms the cache, it does not reserve frames. A page warmed by prefetch that is later evicted before the scan reaches it simply pays its miss then — prefetch is a hint, never a correctness dependency. The measurable effect is on the disk-read count: with read-ahead, the scan phase incurs zero misses because the reads were already paid up front.

### Why a Buffer Pool and Not the OS Page Cache

A tempting shortcut is to `mmap` the data files and let the operating-system page cache do the caching. Real engines deliberately do not, and the reasons are exactly the invariants above. Eviction control: the OS uses a generic policy that knows nothing about query semantics, while the DBMS knows a sequential scan should not evict the working set, knows which pages to pin, and can prefetch deliberately. WAL ordering: the kernel may flush a dirty `mmap` page to disk at any moment, with no way for the DBMS to enforce `pageLSN <= flushedLSN` first — that single fact makes the ARIES protocol unimplementable over `mmap`, the central argument of "Are You Sure You Want to Use MMAP in Your DBMS?" (Crotty, Leis, Pavlo, 2022). Commit-time control: no-force and steal require the DBMS to choose precisely when each page reaches disk, and the OS cache removes that choice. Predictability: an explicitly sized pool avoids double buffering and gives stable, tunable performance instead of behavior at the mercy of kernel heuristics. This is why every serious storage engine ships its own buffer manager rather than leaning on the page cache.

## Common Mistakes

### Calling FetchPage Without Unpinning

Fetching a page and forgetting the matching unpin leaves the pin count above zero forever. The frame can never be evicted, and under sustained load the pool exhausts and every subsequent fetch returns the pool-exhausted error. The fix is to defer the unpin immediately after the nil-check, the same way you defer `f.Close()` after `os.Open`: `guard, err := bp.FetchPage(id)`, check `err`, then `defer guard.Unpin(false)` on the very next line. The guard type exists precisely so the unpin is lexically scoped and hard to drop.

### Reading or Writing Page Data Without the Frame Lock

Touching `guard.Data()` without holding the per-frame latch races any concurrent reader or writer of the same frame. The race detector reports it immediately, and in production a concurrent reader sees torn, partially written data. The fix is to take the frame latch around every access: `guard.Lock()` before a write and `guard.Unlock()` after, or the read variants for a read. The pool latch protects metadata, not page contents, so it is the wrong lock for this.

### Omitting SetLSN After a WAL-Logged Modification

If a caller modifies a page under a WAL record but never records that record's LSN on the frame, write-back sees a zero pageLSN, skips the WAL check, and writes the page to disk before its WAL record is durable. A crash between the page write and the WAL write then leaves no redo record for that modification. The fix is to call the guard's `SetLSN` with the LSN the WAL manager returned, before unpinning dirty. The pageLSN is the only link between a page and the log record that protects it.

### Holding the Pool Mutex During Disk I/O on the Read Path

If a cache-miss read happens with the pool mutex held, one goroutine's slow disk read blocks every other goroutine from fetching, unpinning, or flushing. Under concurrency this collapses throughput to single-threaded. The fix is to pin the victim frame inside the lock, release the pool mutex, perform `ReadPage` outside it, then re-acquire to update the page table. Pinning before the release stops a concurrent sweep from stealing the same frame mid-read. Write-back is the deliberate exception: it holds the pool mutex across its single synchronous page write because those paths are rarer and their lock scope is bounded.

### Treating the Pin Count as a Boolean

A pin count must reference-count concurrent holders, not flip on and off. If two callers fetch the same resident page, the count must reach two, so the first unpin still leaves the frame pinned and unevictable for the second holder. Modeling pin state as a boolean lets the first unpin make the frame evictable while a second caller still holds a pointer into it, which is the eviction-of-a-live-page bug the whole contract exists to prevent.

---

Next: [01-buffer-pool-core.md](01-buffer-pool-core.md)
