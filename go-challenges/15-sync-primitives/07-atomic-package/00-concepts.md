# The atomic Package: Lock-Free Primitives for Backend Hot Paths

A mutex protects a critical section by serializing goroutines through it. That is
the right tool when an invariant spans several values. But on the hottest paths in
a Go backend the thing you are protecting is often a *single* value: a per-request
counter incremented on every request, an in-flight gauge checked and bumped on
admission, a drain flag read on every readiness probe, a pointer to a read-mostly
snapshot served to thousands of concurrent readers. Taking and releasing a lock to
move one integer is disproportionate, and under real contention the lock itself
becomes the bottleneck the profiler points at. `sync/atomic` gives you lock-free
operations that complete in a single hardware instruction and never block another
goroutine.

The senior skill is not "use atomics because they are fast". It is knowing exactly
the shape of problem they solve — single value, read-heavy, uncontended or
loop-retried writes — and recognizing the moment the problem stops being that
shape, because that is the moment atomics turn from an optimization into a
correctness bug. This file is the model you need to reason through every exercise
that follows. Read it once and the ten modules are variations on five ideas: the
memory-model guarantee, the CAS retry loop, the read-mostly pointer swap, the
discipline to fall back to a mutex the instant more than one value must move
together, and the hardware truth that lock-free does not mean contention-free.

## The memory model: atomics are sequentially consistent

The Go memory model says all atomic operations in a program behave as though
executed in some single total order that every goroutine observes consistently.
This is the same guarantee C++ calls sequentially-consistent atomics and Java
attaches to `volatile`. It is *stronger* than what a mutex gives you: a mutex
guarantees ordering only for goroutines that acquire the same lock, whereas atomic
operations on a variable are globally ordered for anyone who touches it. That total
order is precisely what lets you build a correct lock-free algorithm — every
`CompareAndSwap` sees a well-defined "current" value, never a torn or half-written
one.

There is a second, subtler guarantee that makes lock-free *publishing* work. An
atomic operation does not only synchronize its own variable; it creates a
happens-before edge. When a writer stores a value into an atomic and a reader loads
it, every ordinary (non-atomic) write the writer performed *before* that store is
guaranteed visible to the reader *after* that load. This is the mechanism behind
`atomic.Pointer[T]`: the writer fully builds an immutable struct, then stores the
pointer; a reader that loads the pointer is guaranteed to see all the struct's
fields, fully written, with no lock. If you do not understand this edge, lock-free
publishing looks like undefined behavior; once you do, it is obviously correct.

The flip side is the trap: an atomic synchronizes the atomic variable and whatever
was written before the store *by the same writer*. It does not magically publish
unrelated writes made by some other goroutine, and it does not make two separate
atomic variables move together. The happens-before edge is real but narrow.

## The typed wrappers, and why the free functions are legacy

Since Go 1.19 the API you should use is the typed wrappers: `atomic.Int32`,
`atomic.Int64`, `atomic.Uint32`, `atomic.Uint64`, `atomic.Bool`, and the generic
`atomic.Pointer[T]`. The legacy free functions (`atomic.AddInt64(&x, 1)`) still
exist but the wrappers exist to kill two classic bugs. First, alignment: a bare
`int64` field accessed with the free functions must be 64-bit aligned, which on
32-bit platforms is not guaranteed unless the field sits first in its struct — a
famous source of crashes; the wrappers guarantee their own alignment. Second,
pointer confusion: the free functions take an address, so passing the wrong address
compiles fine and silently operates on the wrong variable. The wrappers make the
operation a method on the value, so the compiler binds it for you.

The wrappers carry a `noCopy` marker. They must never be copied — a copy has its
own independent state and is no longer synchronized with the original. `go vet`'s
copylocks check flags a copy, which is why you always embed a wrapper in a struct
and pass that struct by pointer, never return it by value, never write `m := n`
where `n` is an `atomic.Int64`.

## CompareAndSwap is the lock-free update primitive

`Add` and `Swap` are single-instruction fast paths: `Add(delta)` atomically adds
and returns the *new* value; `Swap(new)` atomically replaces and returns the *old*
value. When your operation is a plain unconditional add or replace, reach for them
directly — a CAS loop would be slower and more code.

But most interesting updates are conditional: increment only while below a ceiling,
raise a high-water mark only if the new value is larger, move a state machine only
along legal edges. `sync/atomic` deliberately provides no atomic `Max`, `Min`,
`Clamp`, or bounded-increment. You build every one of them from `CompareAndSwap`,
which is the universal primitive: atomically, if `*addr == old` then set
`*addr = new` and return `true`, otherwise change nothing and return `false`. The
idiom is always the same retry loop:

```go
for {
	cur := v.Load()
	next := compute(cur)     // max, clamp, admit, next-state, ...
	if v.CompareAndSwap(cur, next) {
		return
	}
	// another goroutine moved v; re-read and retry
}
```

The loop is wait-free-ish in practice: a CAS fails only when a concurrent writer
won the race, which means real progress was made by someone. The one rule you can
never break is *check the return value and loop*. A `CompareAndSwap` whose result
you ignore is a silent lost update, and it is the single most common atomics bug.

The three return conventions are easy to confuse and each confusion is a real bug:
`Add` returns the NEW value, `Swap` returns the OLD value, `CompareAndSwap` returns
a bool "did it swap". Reading `Add`'s return as if it were the old value gives you
off-by-one IDs; reading it as new is what makes a sequence generator correct.

## Add/Or/And: the read-modify-write fast paths

Go 1.23 added atomic `And` and `Or` methods to the integer wrappers, each returning
the OLD value. They are the correct way to set or clear individual bits in a shared
bitmask. The wrong way — `v.Store(v.Load() | bit)` — is a load-then-store race:
between the load and the store another goroutine can update the mask and its change
is lost. `v.Or(bit)` performs the read-modify-write as one atomic step. `And` with
an inverted mask clears bits: `v.And(^bit)`. These methods do not exist before Go
1.23, so any module that uses them must build on a 1.23+ toolchain or it will not
compile.

That OLD return value is more than a curiosity — it is a transition detector.
After `old := v.Or(bit)`, the condition `(old & bit) == 0` is true for exactly one
of any number of concurrent callers: the one whose `Or` actually flipped the bit.
This is the bitmask analogue of `atomic.Bool.CompareAndSwap(false, true)` one-shot
semantics, and it is what lets you attach an exactly-once side effect to a state
transition — log "feature X degraded" once, fire one alert, bump one metric — no
matter how many goroutines race to degrade the same feature during an incident.
The same reading works for `And`: after `old := v.And(^bit)`, `(old & bit) != 0`
means this caller is the one that restored the feature.

## atomic.Pointer[T]: the read-mostly / copy-on-write pattern

The read-mostly pattern is everywhere in a backend: configuration that reloads
occasionally but is read on every request, a stats or health snapshot rebuilt by a
background goroutine and served to every handler, a routing table swapped on
deploy. `atomic.Pointer[T]` is built for this. The writer constructs a fresh,
*immutable* value and `Store`s a pointer to it; every reader `Load`s the pointer
wait-free and works with a fully consistent snapshot. Readers never block the
writer and the writer never blocks readers — there is no lock at all. `Swap`
returns the previous pointer (useful when you want the old value back), and
`CompareAndSwap` lets you install a new snapshot only if the current one has not
changed, which is how you enforce "never overwrite a newer state with an older
one".

The two things to internalize. First, this is copy-on-write: you never mutate a
published snapshot in place — a reader might be looking at it. You build a new one
and swap the pointer. Second, the trade-off: each publish allocates a fresh value,
and a reader may briefly observe the *previous* snapshot until the `Store` lands.
That stale window is fully consistent (never torn) but it is stale. For metrics,
config, and health that is exactly right — approximate-but-consistent beats
blocking. For anything requiring the absolute latest value with a hard ordering
guarantee (a bank balance, an idempotency ledger) it is wrong; use a mutex or a
transaction.

The GC quietly saves you from the ABA problem here. A value CAS on an integer can
succeed after the world went A to B and back to A, because the two A's are
indistinguishable bit patterns. With `atomic.Pointer[T]`, the Go runtime will not
reuse the memory of a value while any pointer to it is still live, so the "same"
pointer really is the same object — ABA cannot bite the pointer swap. For integer
CAS loops you design so that the intermediate states are benign (a high-water mark
only moves up; an admission counter's absolute value is what matters, not its
history).

## Lock-free is not contention-free: cache lines, sharding, false sharing

There is a level of contention that no atomic can remove, because it lives below
the instruction set: cache coherence. Every core caches memory in 64-byte lines,
and the coherence protocol (MESI and its descendants) allows a line to be writable
by only one core at a time. When sixteen cores hammer `Add` on the same
`atomic.Int64`, each increment must first pull the line into that core's cache in
exclusive mode, invalidating everyone else's copy — the line ping-pongs between
cores, and every hop is a cross-core round trip costing tens of nanoseconds. The
counter is lock-free, no goroutine ever parks, and yet throughput flatlines or
*drops* as you add cores, because the hardware is serializing the writes for you.
The profiler shows a single innocent `Add` burning CPU, and the fix is not a
faster atomic — there isn't one — it is to stop sharing the line.

The standard fix is sharding: replace the one counter with a small array of
counters (a power of two near `runtime.NumCPU()`), have each writer pick a shard
cheaply, and read by summing. Writers now spread across many cache lines and
stop invalidating each other, and write throughput scales again. But sharding
comes with a subtlety that erases the whole benefit if you miss it: false
sharing. Two adjacent `atomic.Int64` shards are 8 bytes apart, so eight of them
pack into one 64-byte line — physically distinct variables, same line, same
ping-pong. The shards must be padded so each occupies a full cache line (an
8-byte atomic plus 56 bytes of filler), and because a silent size drift
reintroduces the problem invisibly, the shard size deserves a compile-time or
test-time assertion via `unsafe.Sizeof`.

What sharding costs is read-side semantics. A sharded counter's `Value()` is a
loop of per-shard atomic `Load`s summed in ordinary code — each load is atomic,
but the sum is not a global snapshot. While writers are running, an increment can
land on a shard you already summed and be missed, or land on one you have not
reached and be counted "early". The sum is exact once writers quiesce and
approximate while they run — the same consistency class as a Prometheus counter
scrape, which is why this trade is standard for metrics and telemetry and wrong
for anything transactional: fine for requests-per-second, never for money. If
you need an exact value under concurrent motion, you are back to a single atomic
(and its contention) or a mutex.

## The hard boundary: atomics protect exactly one value

This is the line that separates a correct lock-free design from a subtle data
corruption. An atomic operation is atomic with respect to *its own variable and
nothing else*. Two atomic operations are two separate events; another goroutine can
run in the gap between them and observe a broken invariant. `debit(a)` then
`credit(b)`, each atomic, still lets a reader see the money debited but not yet
credited — the "sum is conserved" invariant is violated in the window between them.
A map insert plus a counter increment, a "swap these two fields together", a
"compute the new total from three gauges" — none of these is a single value, and no
amount of cleverness with individual atomics makes them one transaction. The moment
more than one value must move together, or an invariant spans several fields, you
need a mutex (or a single `atomic.Pointer` to an immutable struct that bundles all
the fields, published as one swap). Reaching for a hand-rolled CAS-loop map or a
multi-field atomic dance produces more code, more bugs, and a speedup that
disappears under the first `go test -race`. Reserve atomics for single counters,
flags, pointers, and bitmasks; use a mutex for everything compound.

## Common Mistakes

### Ignoring the CompareAndSwap return value

Writing `old := n.Load(); n.CompareAndSwap(old, old+1)` without a loop silently
loses updates: under contention the CAS fails, and because you never checked, the
increment simply vanishes. Every CAS must sit in a loop that retries until it
returns `true` (or until a legitimate exit condition like "already at the ceiling"
is met).

### Treating two atomic ops as one transaction

`debit(a); credit(b)` with two atomics is not a transfer — another goroutine
observes the sum-broken intermediate state. Any invariant that spans more than one
value needs a mutex, or a single `atomic.Pointer` swap of a struct that holds both
values.

### Reaching for atomics where a mutex is clearer

Hand-rolling a CAS-loop map, or juggling five atomics to keep a compound invariant,
is more code and more bugs for a marginal or negative speedup. If the state is
compound, a mutex is both correct and clearer. Atomics earn their keep on single
counters, flags, pointers, and bitmasks.

### Copying an atomic wrapper

Returning a struct that embeds an `atomic.Int64` by value, or writing `m := n`,
gives the copy its own independent state; `m.Add(1)` no longer affects `n`. Always
pass the containing struct by pointer and heed `go vet`'s copylocks warning.

### Load-then-Store to modify a shared value

`f := flags.Load(); flags.Store(f | bit)` races: a concurrent updater's change
between the load and the store is lost. Use `flags.Or(bit)` / `flags.And(^bit)`
(Go 1.23+) or a CAS loop.

### A naive Add for a bounded counter

An in-flight limiter that does `Add(1)`, checks the ceiling, and `Add(-1)` on
overflow does not over-admit — `Add` returns unique post-increment values in the
atomics total order, so only callers whose return is at or below the ceiling
proceed. Its real defects are subtler. First, the counter transiently overshoots
the ceiling: every rejected caller pushes it past `max` before backing off, so any
concurrent reader of the raw value — a metrics gauge, a readiness probe, an
alerting rule — observes impossible in-flight counts (several times `max` under a
rejection storm). Second, racing rejectors inflate the counter for each other:
a caller can read a value above the ceiling that is composed mostly of other
rejectors' not-yet-reverted increments, and shed load while real capacity is
free — spurious 503s below the limit. A related pattern that DOES over-admit is
check-then-act: `if Load() < max { Add(1) }` lets two goroutines at `max-1` both
pass the check before either increments. Enforce the ceiling atomically inside a
CAS loop so the counter itself never exceeds the limit and a shed means the
service is genuinely full.

### Confusing Add's return with Swap's return

`Add` returns the NEW value, `Swap` returns the OLD value, `CompareAndSwap` returns
whether it swapped. A sequence generator that treats `Add(1)`'s result as the old
value issues off-by-one or duplicated IDs.

### Assuming atomic.Pointer readers never see stale data

Readers can observe the previous snapshot until the `Store` lands. That is correct
and fine for metrics, config, and health; it is wrong for anything that must read
the absolute latest value with a hard ordering guarantee.

### Using And/Or without gating on Go 1.23

These methods did not exist before Go 1.23 and will fail to compile on older
toolchains. Ensure the module builds on 1.23+.

### Assuming lock-free means contention-free

A single hot atomic still bounces its 64-byte cache line between cores on every
write — the coherence protocol serializes the increments even though no goroutine
ever blocks. Under multi-core write pressure the "fast" counter becomes the
bottleneck the profiler points at. The fix is sharding across padded per-core
counters, not searching for a faster atomic.

### Forgetting the padding when sharding

Adjacent unpadded `atomic.Int64` shards sit 8 bytes apart, so eight of them share
one cache line and false sharing restores the exact ping-pong you sharded to
escape. Pad every shard to 64 bytes and assert the shard size with
`unsafe.Sizeof` at compile or test time so a refactor cannot silently shrink it.

### Treating a sharded counter's summed Value as a consistent snapshot

`Value()` sums per-shard atomic loads in ordinary code; while writers run, the
sum can miss or double-window in-flight increments. It is exact only at
quiescence — acceptable for metrics (a Prometheus scrape has the same
semantics), wrong for balances, quotas billed for money, or anything audited.

### Expecting an atomic flag to publish unrelated writes it did not order

An atomic creates a happens-before edge only for writes the *same writer* made
before its store, seen by a reader that loads that atomic. A flag set by goroutine
A does not, by itself, make goroutine C's earlier writes visible to goroutine B.
Match every publish/consume pair to the atomic that orders it.

Next: [01-request-metrics-counters.md](01-request-metrics-counters.md)
