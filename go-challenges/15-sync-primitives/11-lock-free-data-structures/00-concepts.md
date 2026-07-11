# Lock-Free Data Structures — Concepts

Lock-free data structures replace the lock-then-act pattern with compare-and-swap
(CAS) loops on atomic values. The payoff is a progress guarantee a mutex cannot
give — a preempted goroutine cannot stall everyone else — and, on the right
workloads, dramatically better scalability. The price is that every line of a
lock-free algorithm carries a correctness argument a mutex would have made
trivial. This lesson builds the structures a senior backend engineer actually
meets in production: a free-list, sharded metrics counters, a rate-limiter fast
path, a circuit-breaker state machine, a copy-on-write registry, ring and intake
queues, and an ABA-safe gauge. Read this file once; each exercise then stands on
its own.

## Concepts

### The CAS loop is the primitive

Every lock-free structure in this lesson is built from one move: load the current
state, compute the successor state off to the side, publish it with
`CompareAndSwap`, and retry from a fresh load if the CAS fails because someone
else published first.

```go
for {
	old := head.Load()
	next := computeSuccessor(old)
	if head.CompareAndSwap(old, next) {
		return
	}
}
```

Two properties make this correct. First, the successor state is built *privately*
— no other goroutine can observe it until the CAS publishes it. Second,
everything reachable from the published pointer is immutable after publication.
If a reader can load the pointer, nothing behind that pointer may ever change
again; updates happen by building a new state and swapping the pointer, never by
mutating in place. Violate that and readers see torn, racing writes — the race
detector will name the bug, but only if you run it.

### Progress guarantees are a hierarchy

Blocking (mutex): a goroutine holding the lock that gets preempted, page-faults,
or is descheduled stalls every waiter. Lock-free: the *system* always makes
progress — if your CAS failed, it is because someone else's succeeded — but an
individual goroutine may retry unboundedly under contention. Wait-free: every
goroutine finishes in a bounded number of steps; far harder to construct, and
rarely worth it. The structures in this lesson are lock-free, not wait-free
(with one interesting exception: the read side of the copy-on-write registry and
the SPSC ring buffer operations are wait-free, because they never loop).

Between blocking and lock-free sits obstruction-freedom (a goroutine running
alone makes progress), mostly of theoretical interest; know the term, do not
design for it.

### The ABA problem

CAS compares *identity*, not *history*. If the value goes A to B and back to A
while your goroutine sits between its `Load` and its `CompareAndSwap`, the stale
CAS succeeds even though the world changed twice underneath it. For pointer-based
structures in Go, the garbage collector is the mitigation: a node cannot be
freed and reallocated at the same address while any goroutine still holds a
reference to it, so `atomic.Pointer[T]` CAS loops are ABA-safe in the way C
implementations (which recycle nodes through free-lists) are not. This is a real,
load-bearing advantage of writing lock-free code in a garbage-collected language.

Value-type CAS gets no such help. A counter that goes 0, 10, then is reset to 0
looks unchanged to a stale CAS. The standard fix is a version counter packed into
the same atomic word — for example a 32-bit version in the high bits of a
`uint64` and the value in the low bits — so that every semantic change, including
a reset back to the same value, changes the word. Module 09 implements exactly
this, next to an unversioned control that demonstrably loses an update.

### Go's memory model does you a favor

The Go memory model specifies that `sync/atomic` operations behave like the
sequentially consistent atomics of C++ and Java: if the effect of an atomic
operation A is observed by atomic operation B, then A happens before B. There are
no relaxed/acquire/release ordering knobs to choose (and therefore to get subtly
wrong). When the SPSC ring buffer's producer writes the element and then stores
the new tail index, a consumer that loads that tail value is guaranteed to see
the element write. Every correctness argument in this lesson leans on that
happens-before edge; none of them needs anything weaker or faster.

### The honest cost model: CAS is not free

A failed CAS costs a cache-line round trip and a retry. Under contention, the
cache line holding the atomic bounces between cores (MESI ownership transfer) on
every attempt, the failure rate climbs, and a spinning CAS loop burns CPU exactly
when the system is busiest. A `sync.Mutex` parks waiters instead of burning
cores — and it also has a spinning fast path for the uncontended case, so it is
not slow when free. The crossover point between "CAS wins" and "mutex wins" is
empirical, not theoretical: benchmark both implementations with `b.RunParallel`
at several parallelism levels (`b.SetParallelism`) before choosing. Module 03
does this for the two stacks; do not skip the part where you look at the numbers.

### False sharing: contention without sharing

Two logically independent atomics that live in the same 64-byte cache line
contend as if they were one variable, because coherence traffic moves whole
lines. A struct with eight hot `atomic.Int64` counters packed together scales no
better than one counter. The fix is structural: shard the hot counter into
roughly one slot per CPU and pad each slot to a cache line (64 bytes on
amd64/arm64), so concurrent writers touch different lines. Reads become
O(shards) sums and are exact only when writers are quiet — which is precisely
right for metrics. Module 04 builds this for an HTTP request counter.

### Read-mostly data wants copy-on-write

When data is read on every request but mutated rarely (a subscriber list, a
routing table, a config snapshot), the winning shape is RCU-style publication:
readers do one atomic pointer load and get an immutable snapshot — wait-free,
no loop, no lock; writers clone the current state, apply the change, and CAS the
new pointer in, retrying on collision. Reads cost O(1); writes cost O(n) plus
allocation. This is the same pattern as `atomic.Value` config hot-reload from
lesson 08, generalized: module 07 applies it to a subscriber registry with a CAS
loop so concurrent writers cannot lose each other's updates.

### Specializing the concurrency shape removes synchronization

The strongest lock-free trick is not a cleverer CAS — it is proving you do not
need one. A single-producer/single-consumer ring buffer needs zero CAS
operations: the tail index has exactly one writer (the producer) and the head
index has exactly one writer (the consumer), so plain atomic loads and stores
carry all the synchronization (module 08). A multi-producer/single-consumer
intake queue needs CAS only on the producer side; the consumer takes the entire
chain with one `Swap(nil)` and reverses it in private memory (module 10). Name
the real concurrency shape of your problem before writing a general MPMC
structure you do not need.

### Auxiliary state is best-effort

A size counter riding alongside a lock-free stack cannot be updated in the same
atomic step as the CAS that changes the stack, so it is updated *after* the
winning CAS and can transiently disagree with the structure — a `Size()` of 3
while a concurrent pop is mid-flight, or briefly negative-looking interleavings
under churn. That is fine for dashboards and monitoring, and wrong for control
decisions: never write `if s.Size() == 0 { shutdown() }`. If a count must be
linearizable with the structure, it must be inside the CAS word — which usually
means you wanted a mutex.

### atomic.Pointer[T] is the typed API

Before Go 1.19, lock-free code used `unsafe.Pointer` and manual casts. Since
1.19, `atomic.Pointer[T]` is type-checked: `Load` returns `*T`,
`CompareAndSwap(old, new *T) bool`, `Swap(new *T) *T`, and the zero value is a
ready-to-use nil pointer, so the zero `Stack[T]` is a valid empty stack. The
integer types (`atomic.Int32`, `Int64`, `Uint64`) carry `Load`, `Store`, `Add`,
`Swap`, `CompareAndSwap` — and, since Go 1.23, bitwise `And`/`Or`. None of these
types may be copied after first use; `go vet`'s copylocks check flags some
copies, but pass pointers as a rule.

### Production placement

Lock-free earns its complexity only on measured hot paths touched by every
request: metrics counters in middleware, a rate limiter's `Allow()`, a circuit
breaker's state check, snapshot reads of a registry, the intake side of an async
logger. Everything else ships with a mutex first. When you do replace a mutex
version, the lock-free replacement must keep the same tested behavioral contract
— module 02 shows how to run one contract suite against both implementations so
a swap cannot silently change semantics.

## Common Mistakes

### Value-type CAS with no version counter

Wrong: a CAS loop on a bare counter or packed struct where other goroutines can
cycle the value A to B and back to A (for example, a metrics reset racing with
updaters).

What happens: the stale CAS succeeds and silently destroys the intervening
update — a `Reset` that never happened, under load, unreproducible.

Fix: pack a version counter into the CAS word so every semantic change changes
the bits, or move the state behind `atomic.Pointer[T]` and let the GC provide
identity.

### Mutating state after publishing it

Wrong: pushing a node onto a lock-free stack and then writing its fields, or
appending in place to a slice already visible through a COW pointer.

What happens: readers that loaded the pointer race with the writes; `append` may
scribble into the shared backing array a reader is iterating.

Fix: everything reachable from a published pointer is frozen. Build the new
node/slice completely, then publish. Clone (`slices.Clone`) before modifying.

### Assuming lock-free is faster

Wrong: replacing a mutex with a CAS loop because "lock-free scales".

What happens: under high contention the CAS failure rate and cache-line
ping-pong push throughput *below* the mutex version, while burning more CPU.

Fix: write the mutex version first, benchmark both with `b.RunParallel` at
realistic parallelism, and keep the mutex unless the numbers justify the
complexity.

### Shipping without -race

Wrong: validating concurrent code with plain `go test`.

What happens: ordering bugs, torn reads, and unsynchronized access hide in
normal runs and surface in production.

Fix: `go test -count=1 -race ./...` is the non-negotiable gate for every module
in this lesson.

### Control flow from a best-effort counter

Wrong: `if stack.Size() == 0 { drainDone() }` or reading a sharded counter
mid-storm as an exact value.

What happens: the counter transiently disagrees with the structure while CAS
loops are in flight; the decision races.

Fix: best-effort counters feed dashboards. Control decisions need state that is
linearizable with the operation — usually a mutex, a channel, or a WaitGroup.

### Retrying a CAS with a stale snapshot

Wrong: after `CompareAndSwap` fails, retrying with the `old` value loaded before.

What happens: the CAS can never succeed (livelock) or, worse, succeeds later
against a coincidentally matching value and corrupts state.

Fix: every retry iteration starts with a fresh `Load` and recomputes the
successor state.

### Hot atomics packed into one cache line

Wrong: `struct { reads, writes, errors atomic.Int64 }` on the hot path.

What happens: false sharing — three independent counters contend like one.

Fix: pad hot counters to 64 bytes or shard them per-CPU-ish when write
throughput matters (module 04). Cold counters can stay packed.

### Copying a structure that embeds atomics or a mutex

Wrong: passing a `Stack[T]` or `MutexStack[T]` by value after first use.

What happens: the copies desynchronize; each copy has its own head/lock.

Fix: document "must not be copied after first use", pass pointers, and let
`go vet`'s copylocks check catch the mutex cases.

### Conflating empty and full in a ring buffer

Wrong: comparing masked indexes (`head&mask == tail&mask`) to detect state.

What happens: empty and full are indistinguishable; the buffer silently drops or
double-reads at the wrap boundary.

Fix: keep head/tail as monotonically increasing `uint64`s and compare their
difference against capacity; mask only when indexing (module 08).

Next: [01-lock-free-treiber-stack.md](01-lock-free-treiber-stack.md)
