# Pointer Aliasing and Data Races in Production Go Services — Concepts

Aliasing and data races are the dominant class of "works on my machine, corrupts
data in prod" bugs in Go backends. They are quiet: the code compiles, the happy
path passes, the demo runs, and then under real concurrent load a returned `*User`
gets mutated by a caller while another goroutine reads it, a 64-bit counter loses
updates, or a map corrupts and the process panics deep inside the runtime with a
stack that points nowhere near the bug. A senior engineer's job is to (1)
recognize when a returned pointer or a shared slice backing array silently aliases
mutable state across goroutines, (2) choose the correct synchronization primitive
from the *shape* of the state rather than from habit, and (3) treat the race
detector as a mandatory CI gate, not a local afterthought. The through-line of
this whole lesson is one sentence: a race is undefined behavior, not a flaky test,
and the fix is dictated by the memory-access pattern.

This file is the conceptual foundation. Read it once and you have everything you
need to reason through each of the ten independent exercises that follow.

## Concepts

### Aliasing, defined precisely

Aliasing is when two pointers — or two slice headers, or a slice header and
another slice header — reference the same underlying memory, so that a mutation
through one is visible through the other. `a, b := &x, &x` aliases: `*a = 5`
changes `*b`. A slice `s` and any sub-slice `s[2:4]` alias the same backing array:
writing `s[2] = 9` changes the sub-slice's element `0`. Aliasing is legal and
useful — it is how Go passes large structures cheaply and how `append` grows in
place. It becomes a *defect* only in two situations: when it is combined with
concurrency (two goroutines aliasing one location, one of them writing), or when
an API returns a pointer into internal state and a caller mutates through it. Both
situations are the subject of this lesson.

### A data race is a formal condition, not "nondeterministic output"

A data race is precisely: two goroutines access the same memory location
concurrently, at least one of the accesses is a write, and there is no
happens-before edge ordering them. All three clauses matter. Two concurrent reads
are fine. A read and a write ordered by a channel send/receive are fine. Only the
unsynchronized concurrent write-with-anything is a race. The Go memory model
(`go.dev/ref/mem`) states that a program with a data race has *undefined
behavior* — this is the crucial part that separates Go from a mental model where
"racy" merely means "the output is sometimes wrong".

### Undefined behavior means the outcomes are unbounded

Because a racy program is undefined, the compiler and hardware are free to
reorder, tear, or cache the access. A 64-bit read on a 32-bit platform can be
*torn* — you observe the high half of one write and the low half of another, a
value that was never stored. A read-modify-write like `c.value++` can *lose
updates*: two goroutines read 41, both compute 42, both store 42, and one
increment vanishes. A concurrent write to a Go map is specifically detected by the
runtime and *crashes the process* with "concurrent map writes". A racy loop
condition can be hoisted into a register and spin forever. "It works in practice
on my laptop" is not a guarantee of anything; the next compiler version, the next
CPU, or the next Tuesday can change the outcome.

### Happens-before is established only by synchronization

The only way to order two memory accesses so they do not race is a synchronization
edge. The primitives that create happens-before are: a channel send that is
received (the send happens-before the receive completes), `sync.Mutex` /
`sync.RWMutex` Lock/Unlock, the `sync/atomic` operations, `sync.WaitGroup`
Add/Done/Wait, and `sync.Once.Do`. If there is *no* such edge between a write and
a concurrent access to the same location, you have a race — full stop. This is why
"I only read the field, I did not lock" is wrong when another goroutine writes it:
the read still needs the happens-before edge.

### Choose the primitive by the shape of the state

This is the single most important design skill in the lesson. Do not reach for a
mutex reflexively; read the shape of the state and match it:

- A single machine word (an `int64` counter, a boolean flag, one pointer):
  `sync/atomic`. `atomic.Int64.Add` for a counter, `atomic.Bool` for a flag.
- A read-mostly whole-value snapshot (config, feature flags, a routing table)
  that is replaced wholesale and never mutated in place: `atomic.Pointer[T]`
  copy-on-write. Readers `Load` a snapshot lock-free; the writer builds a brand
  new value and `Store`/`Swap`s the pointer.
- A compound invariant that spans multiple fields (a map plus a size counter that
  must agree, a set of entries plus an aggregate): `sync.Mutex` or `sync.RWMutex`.
  A single atomic cannot protect an invariant across two fields, because the two
  fields update non-atomically relative to each other and the invariant can tear.
- One-time initialization of an expensive shared resource: `sync.Once`.
- A check-then-act transition (TOCTOU: "if not seen, mark seen and process"):
  `CompareAndSwap`. The check and the act must be one atomic step, or two
  goroutines both pass the check.

### Copy-on-write with atomic.Pointer, and the invariant that makes it safe

`atomic.Pointer[Config]` gives lock-free reads for read-mostly state. A reader
calls `Load()` and gets a `*Config` snapshot with zero locking; a background
watcher builds an entirely new `*Config` and `Store`s it. This is faster than a
`RWMutex` on the read path because there is no lock at all — just one atomic
pointer load. The invariant that makes it correct is strict: **a published value
is never mutated after the Store.** Readers alias the pointed-to value, so mutating
it in place after publishing is a race against every reader that already loaded it.
The discipline is "build a fresh value, publish, never touch it again"; if you need
to change the config, build another fresh value and swap again.

### Defensive copying at API boundaries

Returning a `*T` or a slice that points into internal storage leaks aliased
mutable state to the caller. A repository whose `Get` returns the stored `*User`
hands every caller a live pointer into the map; one caller doing `u.Name = "x"`
mutates the store — and races every other reader. The fix is to return a *value
copy*, and to `slices.Clone` / `maps.Clone` any reference-typed fields, because a
struct value copy is shallow: the copy's slice and map fields still share backing
storage with the original. A shallow copy that still aliases a `[]string` field is
the subtle version of this bug that survives a naive "I return a copy now" fix.

### Slice backing-array aliasing is data-dependent, hence a latent race

Sub-slices share a backing array, and `append` may or may not reallocate depending
on capacity. So whether two sub-slices alias the same storage is *data-dependent*:
it works while there is spare capacity and silently changes behavior when a grow
reallocates, or vice versa. Handing sub-slices of a shared buffer to goroutines is
therefore a latent race whose presence depends on runtime capacity. The two tools
that force independent storage are `slices.Clone` (a fresh backing array with the
same elements) and the full three-index slice expression `s[low:high:max]`, which
caps the capacity so the next `append` is *forced* to reallocate instead of
overwriting shared tail storage.

### An atomic or a mutex inside a struct must not be copied

`sync/atomic` types and `sync.Mutex`/`sync.RWMutex` must not be copied after first
use. Copying a struct that embeds one duplicates the lock or counter state, so the
copy and the original synchronize against different words — the guarantee is gone.
`go vet`'s copylocks pass catches many of these (passing such a struct by value,
returning it, storing it in an interface), but not all. The rule is: once a struct
holds a live atomic or mutex, give it a `*T` receiver everywhere and never pass it
by value.

### The race detector is dynamic and belongs in CI

`go test -race` (and `-race` builds) install a happens-before-based dynamic
detector: it watches the reads and writes that *actually execute during the run*
and reports a race when two of them touch the same location without an ordering
edge. Two consequences follow. First, it only finds races on code paths the test
actually exercises, so it must run against realistic concurrent tests, not just the
happy path. Second, because it is dynamic and code-path-dependent, it belongs in
CI running the full suite (`go test -count=1 -race ./...`), not as an occasional
local convenience. A race report is not advisory; it names two goroutines, the
racing address, and the read/write stacks, and every one it prints is undefined
behavior to be fixed before shipping.

### RWMutex vs Mutex is a real trade-off

`sync.RWMutex` lets multiple readers hold the lock concurrently, which helps a
genuinely read-heavy workload with non-trivial critical sections. But it has higher
per-operation overhead than a plain `Mutex` and can starve writers under sustained
read load. For short critical sections a plain `Mutex` is often faster, because the
bookkeeping an `RWMutex` does to track reader counts outweighs the benefit of
letting two one-nanosecond reads overlap. Reach for `RWMutex` only when the read
critical section is substantial and reads dominate; otherwise `Mutex`.

### Deterministic concurrency testing with testing/synctest

Timing-sensitive concurrency tests are flaky when they use real `time.Sleep`
fences to wait for a background goroutine. Go 1.25 stabilized `testing/synctest`:
`synctest.Test(t, func(t *testing.T){...})` runs goroutines in an isolated "bubble"
with a fake clock that only advances when every bubble goroutine is durably
blocked, and `synctest.Wait()` blocks until every *other* bubble goroutine is
durably blocked. Together they replace arbitrary sleeps with a deterministic fence:
publish a config, `synctest.Wait()`, then read what the reader observed — no race
between "I advanced time" and "the goroutine reacted". It is still `-race`
compatible, so you get determinism and detection at once. It requires Go 1.25+.

## Common Mistakes

### Treating a race report as a flaky test to retry

Wrong: seeing a `-race` report in CI, re-running the job, and merging when it goes
green. The race is undefined behavior; the next run can lose an update, tear a
64-bit value, or corrupt a map. Fix: treat every race report as a defect and fix
the synchronization; a green re-run means the detector did not happen to observe
the racing interleaving that time, not that the race is gone.

### Wrong primitive for the shape of the state

Wrong (one direction): a `sync.Mutex` around a single `int64` counter where
`sync/atomic.Int64` is simpler and faster. Wrong (the other direction): a lone
atomic guarding an invariant that spans two fields (a map and its size), which
tears because the fields update non-atomically relative to each other. Fix: single
word to `sync/atomic`; compound invariant to a mutex.

### Returning a pointer into internal state

Wrong: a repository or cache `Get` that returns the stored `*T` (a map value) or a
slice of the store's backing array, letting callers alias and mutate the owner's
memory and race other readers. Fix: return a value copy and `slices.Clone` /
`maps.Clone` the reference-typed fields.

### Assuming a struct value copy is a deep copy

Wrong: "I return `*u` by value now, so callers cannot alias." The struct copy is
shallow — its slice and map fields still share backing storage, so the aliasing
race survives. Fix: `slices.Clone` / `maps.Clone` the reference-typed fields of the
copy.

### Mutating a snapshot after publishing it

Wrong: publishing a `*Config` via `atomic.Pointer` and then mutating a field of it
in place ("just updating the timeout"). Readers already alias the pointed-to value,
so the in-place write is a race. Fix: build a fresh value and `Store`/`Swap`;
never touch a published value again.

### Relying on append's reallocation behavior

Wrong: handing sub-slices of a shared buffer to goroutines and assuming `append`
either always or never reallocates. Whether two sub-slices alias is
capacity-dependent, so this is a latent, data-dependent race. Fix: `s[i:j:j]` or
`slices.Clone` to force independent backing arrays.

### Copying a struct that embeds a Mutex or atomic

Wrong: passing a struct that holds a `sync.Mutex` or an atomic by value, or storing
it in an interface, which duplicates the lock/counter state. Fix: `*T` receivers
everywhere; run `go vet` (copylocks catches many cases).

### Forgetting to decrement a gauge on every return path

Wrong: incrementing an in-flight gauge, then returning early on error or a recovered
panic without decrementing, so the gauge leaks upward forever. Fix:
`defer gauge.Add(-1)` immediately after the increment, so every path — including a
recovered panic — decrements exactly once.

### Running CI without -race, or without realistic concurrency

Wrong: CI runs `go test` with no `-race`, so races only surface in production; or CI
runs `-race` but the tests never exercise concurrency, so there is nothing for the
detector to observe. Fix: `go test -count=1 -race ./...` in CI *and* concurrent
tests that drive the real access patterns.

### A check-then-act idempotency guard under concurrency

Wrong: `if !seen[key] { seen[key] = true; process() }` from many goroutines — two
goroutines both read `false`, both set `true`, and both process the event. Fix:
make the transition atomic with `CompareAndSwap` (exactly one goroutine wins the
new→processing swap) or `sync.Once` for a one-time initialization.

Next: [01-racy-counter-vs-atomic.md](01-racy-counter-vs-atomic.md)
