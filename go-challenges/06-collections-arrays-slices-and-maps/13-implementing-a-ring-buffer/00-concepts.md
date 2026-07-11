# Ring Buffers in Production — Concepts

A ring buffer is the workhorse behind every bounded, memory-safe backend
component that must never grow without limit. When you expose an in-memory
`/debug/logs` endpoint that keeps the last N lines, track a rolling p99 over the
most recent K requests, buffer bytes between a `net.Conn` read loop and a parser,
cap a worker queue so a slow consumer applies backpressure to fast producers, or
keep an always-on flight recorder of breadcrumbs to dump on panic — the shape
underneath is the same: a fixed-capacity array with two moving indices and a
size counter. The senior skill here is not "implement a circular array." It is
knowing the operational contract that ring imposes: bounded memory under
unbounded input, the correct eviction policy for the workload, releasing evicted
references so you do not leak the heap, and the concurrency layer that makes it
safe without stalling your producers. This file is the conceptual foundation for
the ten independent modules that follow; read it once and each module becomes a
concrete artifact you assemble from these ideas.

## Concepts

### Backing array, head, tail, and the size counter

A ring buffer is a fixed slice of length `cap` plus three integers. `head` is the
index where the next `Push` writes; `tail` is the index where the next `Pop`
reads; both advance modulo `cap` so they wrap from the last slot back to slot
zero. The subtle part is telling *empty* from *full*: when `head == tail`, the
buffer could be either — completely drained or completely packed. A separate
`size` counter disambiguates: empty when `size == 0`, full when `size == cap`.
This is why `size` is not redundant with `head`/`tail`; it is the field that
resolves the ambiguity of their equality.

There is a classic alternative that avoids the counter: leave one slot
permanently empty and detect "full" as `(head+1) % cap == tail`. That design
trades one slot of capacity for one field of state, and it is common in
lock-free SPSC rings where every field you can drop from the shared cache line
matters. For a general-purpose generic buffer, the explicit `size` counter is
clearer and wastes no slot, so these modules use it — but know both designs and
why each exists.

### Overwrite-oldest versus reject-newest: one array, two contracts

The same backing array supports two opposite operational policies, and choosing
the wrong one silently corrupts either your history or your workload.

Overwrite-oldest (drop-front): when full, `Push` advances `tail` over the oldest
entry and writes the new one at `head`. You keep the freshest data and discard
stale history. This is correct for telemetry, recent-log endpoints, latency
windows, and flight recorders — the newest observations are the ones you want,
and losing the oldest is exactly the point of a bounded window.

Reject-newest (drop-back) or block: when full, refuse the write — return an
error, a short count, or block the producer until space frees. You never
silently lose committed work. This is correct for a bounded work queue (a dropped
job is a lost job) and for anything exposed as `io.Writer`, whose contract
forbids silently discarding bytes. A blocking variant turns "reject" into
backpressure: the fast producer waits for the slow consumer.

A telemetry ring that rejects-newest goes blind under load exactly when you most
want data; a work queue that overwrites-oldest drops jobs no one will ever run
again. The array is identical; the policy is a design decision tied to what the
data means.

### Zeroing evicted slots is a correctness requirement, not hygiene

When `Pop` reads `data[tail]` and advances, it must also write the zero value
back into that slot: `data[tail] = zero`. For a `Ring[int]` this looks pointless.
For a `Ring[*Request]`, a `Ring[[]byte]`, or a `Ring[struct{ buf []byte }]` it is
the difference between a bounded and a leaking process. The backing array holds
its full `cap` of slots for the ring's entire lifetime. If you `Pop` a `*Request`
but leave the pointer sitting in `data[tail]`, that `*Request` — and everything
it transitively references — stays reachable from the GC's roots and is never
freed, even though logically it left the buffer. The leak is bounded by capacity
and therefore slow and easy to miss in a load test, then obvious in a week-long
production run. Writing `var zero T` back drops the reference. Do it in `Pop` and
on the overwrite path of `Push`. This is invisible for value types and lethal for
reference-holding ones, so make it unconditional.

### Snapshot copy semantics

A `/debug` endpoint, a metrics reader, or a percentile computation needs to read
the buffer's contents. The wrong way is to hand back a sub-slice of the internal
array — `return data[tail:]` — because that slice aliases the live storage: it
pins the backing array so the GC cannot reclaim it, it races the writer (a
concurrent `Push` mutates the same cells the reader is walking), and the caller
can accidentally corrupt the buffer by writing through it. `Snapshot` must
allocate a fresh slice and copy the logical range (`tail` for `size` steps,
wrapping) into it. The caller then owns an independent copy it can sort, marshal,
mutate, and outlive the buffer without touching internal state. Copying is O(size)
and allocates, which is precisely why a hot path uses an iterator (see below)
instead, but any handoff across an API boundary must be a copy.

### Modulo versus power-of-two masking

`(i + 1) % cap` is correct for any capacity and is what a general buffer uses. If
you constrain capacity to a power of two, `(i + 1) & (cap - 1)` computes the same
wrap with a bitmask instead of a division — a measurable win on a hot per-byte or
per-sample path, because integer division is one of the slower ALU operations.
The trade-off is flexibility: masking forces every buffer to a power-of-two size,
rounding a requested 100 up to 128. Know the option; reach for it when a profile
shows the modulo on the hot path, not by default.

### The concurrency ladder

There is a ladder of concurrency strategies, each with a real use:

1. Single-goroutine ring: no synchronization at all, the fastest possible. Correct
   when one goroutine owns the buffer (a per-connection read buffer, a
   single-threaded event loop).
2. Mutex-guarded ring: a `sync.Mutex` (or `RWMutex` for read-mostly snapshots)
   around every operation. Simple, obviously correct, and contended under many
   producers. The default for shared telemetry.
3. `sync.Cond` blocking bounded queue: `Put` blocks while full, `Take` blocks
   while empty, giving true backpressure for a producer/consumer worker pool.
4. Lock-free SPSC ring: single-producer/single-consumer using atomic head/tail,
   highest throughput, hardest to get right, and sensitive to false sharing (put
   the producer and consumer indices on separate cache lines or they ping-pong).

A crucial perspective: a buffered Go channel *is* a mutex-guarded ring buffer in
disguise, with blocking send/receive built in. It is almost always the right
default. Reach down the ladder to a hand-written ring only when profiling shows
the channel is the bottleneck, or when you need an operation a channel does not
offer — overwrite-oldest, a snapshot of all contents, or an index-addressable
window for percentiles.

### sync.Cond discipline

`sync.Cond` coordinates goroutines around a shared predicate. Its one hard rule:
`Wait` atomically unlocks the mutex, sleeps, and re-locks on wake, so the
predicate *must* be re-checked in a `for` loop, never a single `if`. Spurious
wakeups and lost wakeups are real; a goroutine can return from `Wait` while the
condition is still false. `for !condition { cond.Wait() }` is the only safe shape.
`Signal` wakes one waiter, `Broadcast` wakes all. On a queue with two predicates
sharing one mutex (`notEmpty` and `notFull`), signal the *right* condition after
each operation — a `Take` frees a slot, so it signals `notFull`; a `Put` adds an
item, so it signals `notEmpty`. Signal the wrong one and a producer or consumer
strands forever.

### The io.Writer / io.Reader contract on a bounded buffer

Exposing a ring as `io.ReadWriter` is exactly where the subtle `io` contracts
bite. `io.Writer.Write(p)` must return `n < len(p)` together with a non-nil error
when it cannot accept all of `p` — the canonical error is `io.ErrShortWrite`. It
must never silently drop bytes, because a caller that sees `n == len(p), nil`
believes every byte was committed. So a byte ring behind `io.Writer` uses
reject-newest / backpressure semantics, not overwrite-oldest: overwriting unread
bytes would destroy committed data and violate the contract. `io.Reader.Read`
returns what it currently has; returning `0, nil` is discouraged but legal, and
`io.EOF` is reserved for a genuine end of stream, which a still-open buffer does
not have.

### Cache locality and bounded cost

A contiguous array ring gives O(1) push and pop with a predictable,
allocation-free steady state and cache-friendly sequential access — the CPU
prefetcher loves a linear walk over an array. Contrast a linked-list queue (a
pointer chase per node, a heap allocation per enqueue, fragmented memory) or a
growable-slice queue (amortized reallocations and copies, and unbounded growth
under a producer faster than the consumer). The fixed capacity is not a
limitation to work around; it *is* the feature. Bounded memory under unbounded
input is the property that keeps a long-running backend from OOM-killing itself.

### Sliding-window statistics

A ring of the last K samples gives a fixed-cost rolling window: p50/p95/p99 over
recent traffic in O(K) space and O(K log K) per query, with no unbounded history.
The catch is that the window is *count-based*, not *time-based*: it holds the last
K requests, whatever wall-clock span that covers. Under bursty traffic that span
shrinks; under idle traffic it stretches across minutes. Reporting "p99 over the
last 60 seconds" from a ring of the last 1000 samples is simply wrong when the
rate varies. When you need a true time window, bucket by time; when you need a
smooth long-horizon estimate, use an exponentially-decayed reservoir. A plain ring
is the right approximation when "the most recent K observations" is genuinely what
you want to summarize.

### iter.Seq and range-over-func

Go 1.23 added range-over-func: a function `func(yield func(T) bool)` — the type
`iter.Seq[T]` — can be ranged with `for v := range r.All()`. This lets a caller
walk the buffer's logical order without the allocation a `Snapshot` requires,
which matters on a hot read path. The `yield` function returns `false` when the
caller `break`s the loop; the iterator must stop pushing values the moment `yield`
returns false, or it will run code after the caller has left. The safety
condition mirrors the classic map/slice hazard: the yielded view is only valid if
the buffer is not mutated during iteration. Iterator invalidation is the caller's
contract — snapshot first if you need to mutate while reading.

## Common Mistakes

### Using head == tail alone to detect empty versus full

Wrong: `if head == tail { return ErrEmpty }`. The equality is true in *both* the
empty and the full state, so this reports a full buffer as empty. Fix: track a
`size` counter (empty when `size == 0`, full when `size == cap`) or reserve a
sentinel slot. This is the canonical ring-buffer bug.

### Returning a sub-slice from Snapshot

Wrong: `return r.data[r.tail:]`. That slice aliases the internal array — it pins
the backing store, races a concurrent writer, and lets the caller corrupt the
buffer. Fix: allocate a fresh slice and copy the logical range into it.

### Forgetting to zero the evicted slot

Wrong: `Pop` reads `data[tail]` and advances without clearing the slot. For a ring
of pointers, slices, or pointer-bearing structs this retains the referent and
leaks heap memory bounded by capacity. Fix: write `var zero T` back into the slot
after reading it out, in both `Pop` and the overwrite branch of `Push`.

### Off-by-one in full detection

Wrong: incrementing `size` past `cap`, or advancing `tail` in the wrong branch of
`Push`. Either corrupts FIFO order after the first wrap. The invariant is: on
`Push`, if `size < cap` increment `size`; otherwise advance `tail` (evicting the
oldest). Exactly one branch runs.

### sync.Cond.Wait inside an if instead of a for

Wrong: `if !ready { cond.Wait() }`. A spurious or lost wakeup lets the goroutine
proceed while the predicate is still false. Fix: `for !ready { cond.Wait() }` —
always re-check after waking.

### Signal versus Broadcast confusion on a two-condition queue

Wrong: a `Take` signaling `notEmpty` (the condition it just consumed) instead of
`notFull` (the slot it just freed). Signaling the wrong `Cond`, or using `Signal`
where several waiters must all re-check, strands producers or consumers. Fix: after
`Put` signal `notEmpty`; after `Take` signal `notFull`.

### Silent overwrite in an io.Writer-backed ring

Wrong: a `Write` that overwrites unread bytes when full and returns
`len(p), nil`. That loses committed data and violates the `io.Writer` contract.
Fix: accept only what fits and return `n < len(p)` with `io.ErrShortWrite`.

### Sorting while holding the lock, or reading with none

Wrong: computing a percentile by sorting the window while holding the mutex (an
O(k log k) stall on every producer) — or, worse, reading the window with no lock
at all and racing the writer. Fix: copy the window under the lock, release it, then
sort and compute on the private copy.

### Assuming a capacity-0 buffer holds elements

Wrong: `New[T](0)` and expecting storage. `New` clamps non-positive capacity to 1,
so a cap-0 request becomes a one-slot buffer that overwrites on every second push.
Fix: validate capacity at the boundary and document the clamp.

### Mutating the ring while ranging its iterator

Wrong: pushing or popping inside `for v := range r.All()`. The iterator yields a
torn or stale view (iterator invalidation). Fix: snapshot first, or forbid
concurrent mutation for the duration of the range.

### Assuming a count-based window is time-based

Wrong: reporting "p99 over the last 60s" from a ring of the last K samples. The
window is K *requests*, not a *duration*, and the two diverge whenever the traffic
rate changes. Fix: use time buckets when you need a real time window; use the ring
only when "the last K observations" is what you mean.

Next: [01-fixed-capacity-ring-core.md](01-fixed-capacity-ring-core.md)
