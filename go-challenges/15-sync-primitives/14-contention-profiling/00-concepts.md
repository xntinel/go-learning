# Contention Profiling: Finding and Killing Lock Bottlenecks in Production

A service that falls over at high concurrency while its CPUs sit half-idle is
almost always losing time to a lock. Throughput collapses not because the work is
expensive but because goroutines spend their lives queued behind a `Lock` call,
context-switching in and out of a runtime semaphore instead of doing anything
useful. A senior backend engineer does not guess at this. They turn on the mutex
and block profilers, capture data under real load, and read the exact call stack
where goroutines are waiting — then apply the one fix that stack actually calls
for and prove the win with a repeatable benchmark. This chapter is the full loop:
instrument, expose the profiles safely, watch a continuous contention signal,
triage wait-bound versus CPU-bound stalls, and pick the correct remedy.

The artifacts throughout are shaped like real infrastructure — a hot key-value
store behind an HTTP service, an authenticated admin debug mux, an observability
hook that turns cumulative lock-wait into an SLO gauge — not syntax demos. Each of
the ten modules that follow is independent and self-contained; this file is the
conceptual spine that ties them together.

## Concepts

### Two profilers, two different questions

Go ships two contention profilers and they are not the same thing. The **mutex
profile**, armed with `runtime.SetMutexProfileFraction`, attributes contended-lock
*wait time* to the goroutine that eventually acquires the lock. It answers "which
locks are goroutines queuing behind, and in what call stacks". The **block
profile**, armed with `runtime.SetBlockProfileRate`, records *any* goroutine
blocking event that lasts longer than its rate threshold — channel sends and
receives, `sync.Cond` waits, `sync.WaitGroup.Wait`, and mutex blocking too. They
overlap on mutexes, but the block profile is broader: a non-empty block profile
does not by itself mean you have mutex contention, it might be a slow channel or a
`WaitGroup` that everyone waits on. When the question is specifically "are my locks
the bottleneck", read the mutex profile; when the question is "why is this goroutine
parked at all", the block profile casts the wider net.

### Sampling semantics, and why you must restore them

`SetMutexProfileFraction(n)` records 1 in every `n` contention events and returns
the *previous* fraction. `n = 1` records all of them — ideal for a benchmark or a
tight load test, too expensive for a hot production path. `n = 100` or `n = 1000`
is the production compromise: enough samples to see the shape, little enough
overhead to leave on. `SetBlockProfileRate(rate)` records a blocking event if it
lasted longer than `rate` nanoseconds; `rate = 1` records essentially everything,
`rate = 0` disables it. Both settings are process-global and sticky. If a test or
benchmark cranks the fraction to 1 and never restores it, every subsequent test in
the binary pays the recording overhead on every contended lock. The discipline is
non-negotiable: capture the previous fraction from the return value of
`SetMutexProfileFraction`, and restore it (and set the block rate back to 0) in
`t.Cleanup`.

### The single most common misreading: wait time is not execution time

The mutex profile measures **wait** time — how long goroutines sat blocked before
they got the lock — not how long the locked code took to run. This distinction is
where most people misdiagnose a stall. Consider a locked function that is slow
because the work *inside* the critical section is genuinely expensive: serializing
a large struct, hashing, a tight numeric loop. Goroutines are slow, throughput is
bad, CPUs are pinned — but they are not primarily *waiting* on the lock, they are
*executing*. That cost shows up in the **CPU profile**, not the mutex profile. Read
only the mutex profile and you will "optimize" the lock (shard it, shrink it) and
watch nothing improve, because the lock was never the problem. The rule: when a
path is slow under load, capture both profiles from the same run and compare. High
mutex-wait with low CPU on the locked function means contention; high CPU inside
the locked function means the work itself is the cost.

### The cheap always-on signal: runtime/metrics

Sampled profiles are a targeted tool — you turn them on to diagnose. What you want
continuously, at near-zero cost, is an aggregate gauge that tells you *when* to
reach for the profiler. `runtime/metrics` exposes exactly that:
`/sync/mutex/wait/total:seconds` is the cumulative number of seconds goroutines
have spent blocked on `sync.Mutex`, `sync.RWMutex`, and runtime-internal locks.
Read it with `metrics.Read` into a `[]metrics.Sample`, take the delta between two
scrapes, and you have the *rate* at which the process is accumulating lock-wait —
a first-class SLO signal an SRE can alert on. When that rate rises, you turn on the
mutex profile to find the specific lock. Metrics are continuous and cheap;
profiling is targeted and expensive. Guard the metric name against a Go-version
rename by checking it exists in `metrics.All()` and that its `Value.Kind()` is
`KindFloat64` before trusting the reading — a silent zero from a renamed metric
would hide a real regression.

### Reading a profile

Once you have written a profile, `go tool pprof mutex.prof` opens it. `top` lists
the hottest wait stacks; `list <Func>` shows the annotated source with wait time
attributed per line, which is how you find the exact `Lock` call to fix; `web`
draws the call graph (needs Graphviz). In code, `pprof.Lookup("mutex").WriteTo(w,
debug)` writes the profile: `debug = 0` emits the binary pprof protobuf that the
tooling consumes, `debug > 0` emits a human-readable text dump. In tests you assert
the written file is non-empty; the tooling is for the human reading it afterward.

### The four canonical fixes, matched to what the profile shows

Sharding is the default hammer, but a senior engineer matches the fix to the
stack the profile points at:

1. **A single mutex over a hot map** ⟹ shard into N independent locks so
   unrelated keys stop serializing behind one another.
2. **A long critical section** ⟹ snapshot the state you need under the lock,
   release, do the slow work lock-free, then take a second short lock to write the
   result back.
3. **A read-mostly access pattern** ⟹ switch the read path to `sync.RWMutex` so
   readers run concurrently and only writers exclude.
4. **A hot counter** ⟹ drop the mutex entirely and use an `atomic` operation.

Reaching for sharding when the profile actually shows a read-mostly load, or a
long critical section, produces harder code that does not fix the bottleneck.

### Sharding trade-offs

Sharding reduces contention roughly N-fold *only if keys distribute evenly across
shards*, which is why hash quality matters — a poor hash piles keys into one shard
and you are back to a single lock. Sharding also costs memory (N maps, N locks),
breaks cross-shard atomicity (you cannot atomically update two keys in different
shards), and makes any globally-consistent operation — total size, iteration, a
snapshot — expensive or racy because it must touch every shard. Choose a shard
count near your real parallelism (GOMAXPROCS-scaled), not an arbitrarily large
number; over-sharding wastes memory and hurts cache locality for no contention
benefit past the point where shards outnumber the goroutines contending.

### Exposing profiles in production, safely

The convenient move — a blank import `_ "net/http/pprof"` — is a trap on a public
listener. Importing `net/http/pprof` at all runs its `init`, which registers
`/debug/pprof/*` on `http.DefaultServeMux`. If your public server is that mux, you
have just exposed CPU/heap/goroutine profiles, plus a denial-of-service vector (the
`profile` and `trace` endpoints block for a caller-controlled duration), to the
internet. The correct pattern: never pass `http.DefaultServeMux` to a public
`ListenAndServe`; build your public server on your own `ServeMux`, and register the
`net/http/pprof` handlers on a *separate* admin mux behind authentication and a
debug flag — and only raise the profiler fractions while you are actively
diagnosing.

### Measure, then optimize — then measure again

The discipline that separates a fix from a guess: capture the profile first,
optimize the exact line pprof points to, then re-benchmark to prove the win *and*
confirm the bottleneck did not simply relocate. Benchmarks should run under `-race`
for correctness and with `-benchmem`/`-mutexprofile` for the contention picture.
Wall-time comparisons are flaky on shared CI and must not be asserted in tests;
assert correctness (the refactor is behavior-preserving) and inspect the profile to
confirm the wait samples moved off the hot function. A benchmark that is green but
whose profile still shows the old stack has not fixed anything.

## Common Mistakes

### Leaving the profilers on at fraction 1 in production

Wrong: shipping a service with `SetMutexProfileFraction(1)` or
`SetBlockProfileRate(1)`. Every contended `Lock` and every block then pays
recording overhead on the hot path. Fix: gate the profilers behind a debug flag,
and when they must stay on, lower the fraction to 100 or 1000.

### Reading the mutex profile when the problem is CPU-bound

Wrong: seeing a slow locked function and jumping to the mutex profile. If the
function is slow because of the work inside the critical section, that cost is in
the *CPU* profile; the mutex profile (wait time only) will look innocent. Fix: pair
the two profiles whenever you triage a slow path.

### Optimizing before measuring

Wrong: refactoring to sharding "because it looks slow". You get harder code and
often just move the bottleneck. Fix: capture the profile first and optimize the
line it points at.

### Not restoring profiler rates in tests and benchmarks

Wrong: leaving `SetMutexProfileFraction(1)` set after a test. Every later test in
the binary is taxed. Fix: capture the previous value from
`SetMutexProfileFraction`'s return and restore it (and set the block rate back to
0) in `t.Cleanup`.

### Assuming the block profile equals the mutex profile

Wrong: treating a non-empty block profile as proof of mutex contention. The block
profile also captures channel and `Cond` blocking. Fix: to confirm *lock*
contention specifically, read the mutex profile.

### Letting the simulated critical-section work be optimized away

Wrong: a `busyWork` helper whose result is never observed. The compiler elides the
loop, the lock is held for essentially zero time, and the profile shows nothing —
the contention signal was an artifact of real work being done under the lock. Fix:
make the work observable (return it, or store it in a package-level sink) so the
compiler cannot eliminate it.

### Importing net/http/pprof onto a public mux

Wrong: a blank import registers `/debug/pprof/*` on `http.DefaultServeMux`,
exposing profiles and a DoS vector to any caller if that mux is public. Fix:
isolate the pprof handlers on an authenticated admin listener, and never serve
`http.DefaultServeMux` publicly.

### Over-sharding

Wrong: hundreds of shards "to be safe". You waste memory, hurt cache locality, and
make every consistent global read (size, iteration, snapshot) expensive or racy.
Fix: size the shard count to real parallelism, not to a maximum.

### Trusting a runtime/metrics name without guarding it

Wrong: reading `/sync/mutex/wait/total:seconds` blindly. If a Go release renames or
removes it, `metrics.Read` leaves the value at its zero and a real regression hides
behind a silent 0. Fix: validate the name against `metrics.All()` and check
`Value.Kind()` before trusting the number.

Next: [01-contended-single-mutex-store.md](01-contended-single-mutex-store.md)
