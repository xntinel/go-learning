# 4. Load Balancing — Concepts

A service mesh data plane must spread incoming traffic across a pool of upstream endpoints, and the choice of how it spreads that traffic is one of the highest-leverage decisions in the whole proxy. The hard parts are three: picking an algorithm that matches the traffic pattern, keeping selection correct while thousands of goroutines call it concurrently, and supporting backends that join and leave the pool without disrupting in-flight requests. This file is the conceptual foundation. Read it once and you will have the reasoning you need for each exercise, which builds one balancing algorithm at a time as an independent, self-contained Go module behind a single `Balancer` interface: smooth weighted round-robin, least-connections, and consistent hashing with bounded loads.

## Concepts

### Why Algorithm Choice Matters

There is no universally optimal load-balancing algorithm; each one is the right answer to a different question about the traffic.

Round-robin is fair when every backend completes a request in roughly the same time. It is stateless, trivial to reason about, and adds essentially no per-request cost. The moment backends have heterogeneous latency — one is serving a cold database query while another returns cache hits — round-robin's fairness becomes a liability: it keeps shoving new work at the slow backend at exactly the rate it shoves work at the fast one, and the slow backend's queue grows without bound.

Least-connections answers "which backend is least busy right now?" instead of "whose turn is it?". By routing to the backend with the fewest in-flight requests, it naturally steers traffic away from a slow or stalled backend, because a backend that is slow accumulates active connections and stops being the minimum. It is the standard choice when request cost varies widely and you have no better signal than concurrency.

Consistent hashing answers a different question entirely: "which backend owns this key?". When the same cache key, session ID, or shard key must always reach the same backend, hashing the key to a fixed point on a ring gives cache affinity that can raise an effective hit rate by orders of magnitude over round-robin, which would scatter the same key across the whole pool.

A proxy that exposes a pluggable interface and lets an operator choose per-cluster is far more valuable than one that hardcodes a single approach. That is why every algorithm in this lesson implements the same `Balancer` interface: `Pick` to select a backend, `Release` to signal completion, and `AddBackend`/`RemoveBackend` for dynamic membership. The proxy core calls the interface; the algorithm is a swappable detail.

### The Backend Object and the Pick/Release Lifecycle

Every algorithm shares one value type: a `Backend` describing a single upstream endpoint. It carries its address, its configured weight, a health flag, a live count of active connections, and a cumulative request total. The last three are read and written from many goroutines at once, so they are `atomic.Bool` and `atomic.Int64` rather than plain fields guarded by an external mutex. Two reasons drive this. First, a single-value atomic is both faster and harder to misuse than the `var mu sync.Mutex; var v int64` pattern it replaces. Second, `atomic.Bool` and `atomic.Int64` embed a `noCopy` guard, so `go vet` reports any accidental copy of a `Backend` as "assignment copies lock value". The whole system therefore passes `*Backend` everywhere; a `Backend` is allocated once by `NewBackend` and never copied.

The contract between the proxy and the balancer is a strict pair: every successful `Pick` must be matched by exactly one `Release`. `Pick` increments the chosen backend's `activeConns` (and its `totalReqs`); `Release` decrements `activeConns`. This count is not bookkeeping for its own sake — it is the live signal least-connections and bounded-load consistent hashing route on. A missing `Release` makes a backend look permanently busy and slowly poisons every load-aware decision, which is why the discipline is identical to deferring an unlock or a file close.

A backend starts unhealthy. Health is a separate flag from membership: a backend can be in the pool but temporarily unhealthy, and `Pick` must skip it without removing it. This separation is what lets a health checker flip a backend out of rotation and back without racing the membership-changing `AddBackend`/`RemoveBackend` calls.

### Smooth Weighted Round-Robin

Weights let an operator send more traffic to a bigger backend. The naive implementation — repeat each backend in sequence as many times as its weight — is correct in aggregate but produces long bursts: weights A=3, B=1 yield the order A, A, A, B, hammering A three times in a row before B sees a single request. Bursts translate into bursty load on the upstream and worse tail latency.

Nginx's smooth weighted round-robin (commit `27e94984`) fixes the bursting while preserving the exact proportions. Each backend keeps a `currentWeight` initialized to zero. Every `Pick` runs three phases:

1. For every healthy backend, add its configured weight to its `currentWeight`.
2. Select the backend with the highest `currentWeight` (ties broken by pool order).
3. Subtract the total weight of all healthy backends from the winner's `currentWeight`.

For A=3, B=1 (total 4) the cycle of four picks becomes A, A, B, A: the light backend B is interleaved at position 3 rather than bunched at the end. The invariant that makes this correct is that after any complete cycle every `currentWeight` returns to its starting value, so each backend has received exactly its proportional share — the smoothness is free, paid for by nothing but the order of selection.

The three phases form one indivisible logical step: increment all, find the max, decrement the winner. They are not expressible as a single atomic operation, and `currentWeight` is mutated on every `Pick`, so the entire `Pick` must run under an exclusive `sync.Mutex`. A read lock would let two goroutines read and write the same `currentWeight` field concurrently — a data race the race detector catches immediately. The critical section is O(n) over the backend count, typically well under a hundred, so the exclusive lock is not a throughput bottleneck in practice.

### Least-Connections

Least-connections keeps no per-algorithm state beyond the pool itself: each backend already tracks its active connections in an `atomic.Int64`. `Pick` scans every healthy backend and returns the one with the smallest count, breaking ties by pool order (so an idle pool degrades gracefully to round-robin-like behavior). A linear scan beats a heap here: with the typical backend count below a hundred, a heap's larger constant factors lose, and a heap would also demand maintenance work on every `Release` that the linear scan avoids entirely.

The concurrency model differs from round-robin. `Pick` reads the pool slice and then increments one backend's counter atomically; it never mutates shared per-algorithm state under the lock. That lets the pool slice be guarded by a `sync.RWMutex`: many `Pick` callers hold the read lock simultaneously, while `AddBackend`/`RemoveBackend` take the write lock. There is a small, deliberate race: between a `Pick` reading the minimum and its atomic increment, another goroutine may pick the same backend, so two requests can briefly land on a backend that was the minimum by one. The error is bounded to a single extra connection and self-corrects on the next `Pick`, which is a fully acceptable trade for not serializing every selection.

### Consistent Hashing With Virtual Nodes

A consistent hash ring maps both keys and backends onto the same circular hash space; a key is served by the first backend encountered walking clockwise from the key's position. Its defining property is that adding or removing one backend remaps only the keys in that backend's arc — roughly 1/N of all keys for N backends — instead of reshuffling everything the way `hash(key) % N` would.

The naive ring gives each backend exactly one position, and with only a handful of backends those positions are badly uneven: one backend can randomly own 60% of the key space. Virtual nodes fix the skew. Each backend is hashed to many positions using distinct keys (`"addr#0"`, `"addr#1"`, ...). With 150 virtual nodes per backend, the relative standard deviation of load falls to roughly 1/sqrt(150), about 8% — even enough for most caches. The ring is a slice of `(hash, backend)` entries kept sorted by hash; lookup hashes the key, then uses `sort.Search` (binary search) to find the first position at or above the key's hash in O(log n), wrapping to position 0 when the key hashes past the last entry.

### Bounded-Load Consistent Hashing

Plain consistent hashing distributes *keys* evenly, but not necessarily *load*: if a small set of keys is extremely popular, the backend that owns them becomes a hotspot no matter how even the ring is. Bounded-load consistent hashing (Google Research, 2017) adds one constraint. Compute the average active connections across healthy backends; if the backend a key would normally land on already exceeds `1.25 * average` connections, walk clockwise on the ring to the next backend within the bound. The 1.25 factor caps any backend's load at 25% above average while keeping the disruption to key mapping minimal — most keys still hit their natural backend, and only the overflow from hotspots spills to neighbors.

One subtlety: the bound is `1.25 * average + 1`, not `1.25 * average`. The `+1` keeps the bound at least 1 when the cluster is idle (average zero), so an all-idle pool does not reject every backend and fail every pick. The ring and the `byAddr` map are guarded by a `sync.RWMutex`, because reads (`Pick`) vastly outnumber membership changes, and read-write locking gives real concurrency over a plain mutex here for the same reason it does in least-connections.

### Concurrency Model Summary

The three algorithms make three different locking choices, and the reason in each case is the same question: does `Pick` write shared per-algorithm state? Smooth WRR mutates `currentWeight` on every call, so it needs an exclusive `sync.Mutex`. Least-connections and consistent hashing only read their pool/ring under the lock and write solely through per-backend atomics, so both use a `sync.RWMutex` and let many pickers run in parallel. In all three, the per-backend `activeConns`/`totalReqs` updates are atomic and happen without serializing on the pool lock, which is what keeps the hot path scalable.

## Common Mistakes

### Holding a Read Lock in Smooth WRR Pick

Wrong: giving `RoundRobinBalancer.Pick` an `RWMutex` and taking only a read lock so multiple goroutines can pick concurrently.

What happens: `currentWeight` is written by every `Pick`. Under a read lock, several goroutines read and write the same non-atomic field at once, a data race that `go test -race` flags immediately and that can corrupt the weight bookkeeping so the distribution drifts off its configured ratio.

Fix: `RoundRobinBalancer.Pick` takes an exclusive `sync.Mutex`. The whole increment-all / find-max / decrement-winner sequence runs in one critical section. Least-connections and consistent hashing may use `RWMutex` precisely because their `Pick` writes only through atomics, never to shared per-algorithm state.

### Forgetting to Call Release

Wrong: calling `Pick` once per request but never calling `Release`, or releasing only on the success path and leaking on every error return.

What happens: `activeConns` grows without bound. Least-connections stops routing to the earliest-picked backends because they look permanently maximal; bounded-load consistent hashing's average drifts upward and its spill logic misfires; every load metric becomes meaningless.

Fix: pair every successful `Pick` with a deferred `Release`, exactly as you would defer an unlock or a `Close`:

```go
b, err := lb.Pick(ctx, key)
if err != nil {
	return err
}
defer lb.Release(b)
```

### Copying a Backend Instead of Passing a Pointer

Wrong: `shadow := *backend` to take a local copy, then handing `&shadow` to `Release`.

What happens: `atomic.Bool` and `atomic.Int64` embed a `noCopy` guard. `go vet` reports "assignment copies lock value", and at runtime the copy has its own independent counters, so `Release` decrements a struct nobody else can see while the real backend's count never drops.

Fix: always pass `*Backend`. Allocate once with `NewBackend`, store the pointer if you must hold a reference, and never dereference-and-copy. The `var _ Balancer = (*T)(nil)` assertion in each module reinforces that implementations work with pointers.

### Sorting the Ring on Every Virtual-Node Insert

Wrong: calling `sort.Slice(ch.ring, ...)` inside the inner loop that appends each virtual node.

What happens: for `virtualNodes = 150` and n backends the ring grows to 150*n entries while being re-sorted 150*n times, turning an O(n log n) build into O(n^2 log n). With 100 backends that is 15000 entries sorted 15000 times.

Fix: append all virtual nodes for a backend first, then sort the ring once. `insertLocked` in the consistent-hash module does exactly that — the inner loop only appends, and `sort.Slice` runs a single time at the end.

---

Next: [01-weighted-round-robin.md](01-weighted-round-robin.md)
