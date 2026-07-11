# 6. Parallel Execution — Concepts

Running one goroutine per operator caps a stream pipeline at a single CPU core. The parallel execution layer breaks that cap by running N concurrent instances of an operator, distributing incoming records across them with a partitioning strategy, and merging their outputs back into one downstream channel. The genuinely hard problems here are about correctness, not raw concurrency: a key-based partitioner must map a key to the same partition every time (hash stability); the fan-out must not let one slow partition stall the others (head-of-line blocking); fan-in must close cleanly on cancellation; and when the engine rescales — adds or drops a partition — it should move as little keyed state as possible. This file is the conceptual foundation for the whole chapter. Read it once and every exercise that follows builds one self-contained piece: the partitioning harness itself, an affinity (rendezvous-hash) partitioner that minimises rescale cost, a keyed operator that preserves per-key order, and a fan-out worker pool whose bounded queues apply backpressure.

## Concepts

### Data Parallelism vs. Task Parallelism

A stream pipeline is already task-parallel: each operator runs concurrently in its own goroutine, so a source, a map, a filter, and a sink all make progress simultaneously. The parallel execution layer adds a second, orthogonal dimension. It duplicates a single operator N times so that different records are processed simultaneously by different instances of the *same* operator. A production engine uses both levels at once: a chain of operators (task parallel) where each link is itself fanned out (data parallel).

Data parallelism is most effective for operators whose work per record is independent of other records. Stateless operators (map, filter) are trivially parallelizable: any distribution strategy works because no instance needs to see another instance's records. Stateful keyed operators (windowed aggregation, keyed join) require that all records sharing a key reach the same instance; otherwise each instance holds a partial view of the key's state and results are wrong. A stateful operator that aggregates across *all* keys (a global count, a global distinct) cannot be naively parallelized at the data level at all — its state is shared by definition.

### Partitioning Strategies and Their Invariants

Three strategies cover the common cases.

Key-based partitioning routes every record with the same key to the same partition using `hash(key) % N`. The invariant is stability: for a fixed N, the same key always maps to the same partition. CRC-32/IEEE is a conventional choice because it is fast (a single pass over the key bytes with a hardware polynomial) and distributes adequately. Apache Flink uses MurmurHash; Kafka hashes the key bytes. Both meet the stability requirement. The critical limitation: when N changes (scale up or down) the mapping `hash % N` changes for almost every key, so nearly all keyed state must migrate before processing resumes. That cost is the motivation for affinity hashing, below.

Round-robin partitioning distributes records evenly, independent of content, using an atomic counter. Because the counter advances on every call, the distribution is exactly equal — not approximately equal — when the total record count is a multiple of N. Round-robin is appropriate for stateless operators and any case where load balance matters more than data locality. It carries no key affinity, so it must never feed a keyed stateful operator.

Broadcast partitioning sends every record to every partition, multiplying work by N. It is necessary when each instance needs the full data: a hash-join probe side that must match against every build-side row, or model inference where a shared lookup table is replicated across instances. The fan-out loop must treat the partitioner's return value as a sentinel ("send to all") rather than as a partition index.

### Affinity Hashing and Rescale Cost

`hash(key) % N` is stable for a fixed N but catastrophic to rescale: growing from 4 to 5 partitions remaps roughly 80% of all keys, because the modulus changes for almost everyone. In a stateful engine every remapped key is state — a window, an aggregate — that must be shipped to a new instance before work resumes. The rescale therefore stalls in proportion to total state.

Rendezvous hashing (also called highest-random-weight, or HRW) fixes this. For each key it computes a score `weight(key, partition)` for every partition and assigns the key to the highest-scoring partition. When a partition is added, a key moves only if the new partition happens to outscore the key's current winner, which happens with probability `1/(N+1)`; growing 4 to 5 moves about 20% of keys, the theoretical minimum. Consistent hashing (a hash ring) achieves the same asymptotic property with O(log N) lookup; rendezvous is O(N) per key but trivially simple and needs no ring structure. The one subtlety that makes or breaks rendezvous is the quality of the score mixing: the per-partition scores of a single key must be effectively independent and uniform, or the "winner" is biased and more keys move than the 1/(N+1) ideal. A strong finalizer (a SplitMix64-style avalanche) over `keyHash XOR (partition * constant)` gives that independence; a weak mix (for example feeding the partition index as trailing bytes into a streaming hash) does not, and the rescale cost creeps up measurably.

### Fan-Out and Head-of-Line Blocking

Naive fan-out — a single goroutine that receives a record and sends it to the target partition's input channel — has a head-of-line blocking problem. If partition 3's buffer is full, the fan-out goroutine blocks on that send, and while blocked it cannot route records destined for partitions 0, 1, or 2 even though their buffers have room. One slow partition stalls the whole pipeline.

The correct remedy for a non-broadcast send is to block only the targeted partition with a `select` over `partIn[target] <- r` and `ctx.Done()`. That block is intentional: it is the mechanism by which backpressure propagates upstream — when the consumer is slow, the bounded channel fills, the send blocks, and the producer is forced to wait rather than pile up unbounded memory. For broadcast, iterate the partitions and block each independently; a slow partition delays later partitions within that one broadcast iteration, but per-partition buffers absorb micro-bursts. True per-partition isolation requires a dedicated goroutine per partition draining a shared queue, which is what heavier engines use; the single-goroutine fan-out here is correct and sufficient to demonstrate the invariants.

### Fan-In and Merge Semantics

Fan-in collects output from N partition channels and forwards onto one merged channel: one goroutine per partition, all writing to the common channel via a `select` with `ctx.Done()`. Each goroutine becomes runnable as soon as its partition produces output, and the scheduler interleaves them without starvation.

The merge produces no total order across partitions. Records within a single key keep their relative order because all of that key's records flow through exactly one partition instance (key partitioning) — the partition processes its input channel sequentially and emits in that order, and one fan-in goroutine forwards that partition's output in order, so the key's subsequence in the merged stream is still in input order. Across keys the output is interleaved in scheduler order, which is not deterministic. An application that needs a total order must either sort after the merge or use a single-partition pipeline.

Goroutine lifecycle is the last detail. Each fan-in goroutine exits when its partition's output channel closes. A `sync.WaitGroup` tracks when all N have exited; a separate closer goroutine calls `wg.Wait()` and then closes the merged channel. This is the canonical fan-in pattern from the Go blog's pipelines article, and it is what guarantees the merged channel closes exactly once, after the last record.

### Backpressure and Bounded Queues

Backpressure is the property that a fast producer is slowed to the rate of the slowest consumer instead of being allowed to enqueue unbounded work. In Go it falls out of bounded buffered channels for free: a send on a full channel blocks. A fan-out worker pool with one bounded queue per worker therefore self-regulates — `Submit` blocks once the chosen worker's queue is full, which throttles the caller exactly when the workers fall behind. Sizing those queues is the tuning knob: too small and throughput suffers from constant blocking; too large and a burst can buffer megabytes of latency-sensitive work that is stale by the time it runs. The number of concurrently running handlers is bounded by the worker count regardless of queue size, because each worker runs one handler at a time, so the pool's peak parallelism is exactly its worker count — a useful invariant to assert in tests.

## Common Mistakes

### Using Modulo on a Signed Hash Result

Wrong: `int(crc32.ChecksumIEEE(key)) % numPartitions`. If the cast to `int` makes the value negative on a 32-bit build, the modulo result is negative — a valid Go result but an invalid partition index, which silently routes records to the wrong channel or panics on slice access. Fix: keep the hash as `uint32` and take the modulo before casting: `int(h % uint32(numPartitions))`. `uint32 % uint32` is always non-negative.

### Rescaling a `hash % N` Partitioner and Expecting Cheap Migration

Wrong: assuming a modulo partitioner only moves the keys near the boundary when N grows. It moves almost all of them, because the modulus changes for nearly every key. Fix: use rendezvous or consistent hashing when rescale cost matters; those move only the `1/(N+1)` fraction of keys that the new partition actually wins.

### A Weak Score Mix in Rendezvous Hashing

Wrong: scoring `(key, partition)` by appending the partition index as trailing bytes to a streaming hash of the key. The per-partition scores of one key end up correlated, the highest-weight choice is biased, and the rescale moves visibly more than the `1/(N+1)` ideal. Fix: combine the key hash and partition index and pass the result through a strong avalanche finalizer so the scores are effectively independent and uniform.

### Letting One Slow Partition Block the Others

Wrong: a fan-out that selects over all partition channels at once, or that blocks on a full partition while holding records for idle ones. Fix: for a non-broadcast send, block only on the target partition's channel (with `ctx.Done()`); that block is the intended backpressure to upstream, not a bug.

### Neglecting the Error Channel in Fan-In

Wrong: a fan-in goroutine that drains only the record channel and ignores the error channel. The `WaitGroup` decrements when the record channel closes, but an error sent afterward blocks the operator goroutine forever — a leaked goroutine under the race detector, a slow leak in production. Fix: drain both channels in the same fan-in goroutine, records first then errors.

### Assuming Fan-In Preserves Cross-Key Order

Wrong: reading the merged output and relying on records from different keys appearing in insertion order. Fan-in interleaves in scheduler order, which is not insertion order and not deterministic across runs. Fix: if total order is required, sort after collection or use a single partition; the parallel layer trades cross-key order for throughput, and only per-key order survives.

### Feeding a Keyed Operator from a Round-Robin Partitioner

Wrong: parallelizing a windowed aggregation behind a round-robin partitioner because it "balances load better". Each instance then sees a random slice of every key and every aggregate is partial and wrong. Fix: keyed stateful operators must sit behind a key (or affinity) partitioner so a key's whole history lands on one instance.

---

Next: [01-parallel-operator.md](01-parallel-operator.md)
