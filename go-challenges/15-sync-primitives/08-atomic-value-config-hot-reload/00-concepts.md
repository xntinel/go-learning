# atomic.Value and atomic.Pointer: Config Hot Reload in Production Services — Concepts

Every high-QPS Go service eventually needs to change behavior without a
redeploy: flip a feature flag, raise a rate limit, rotate a database endpoint,
lower a log level. The read side of that problem is brutal — millions of
requests per second all consulting the same configuration — while the write
side is trivial: a reload every few seconds or minutes at most. `sync/atomic`'s
`Pointer[T]` (and its older sibling `atomic.Value`) is the tool Go gives you
for exactly this shape, and around it has grown a production idiom you will
find inside every feature-flag SDK, service-discovery client, and quota table:
publish an immutable snapshot behind an atomic pointer. Readers `Load` a
consistent view with a single atomic read; writers build a complete new value
and `Store` it. Correctness comes from immutability; atomicity only prevents
torn pointers. This file is the theory for the ten exercises that follow —
read it once and each module becomes a mechanical application of it.

## Concepts

### The idiom in one sentence

```
build a complete, immutable Config  ->  Store(&cfg)  ->  readers Load() a snapshot
```

The pointer swap is the *only* synchronized operation. Nothing about the
`Config` struct itself is atomic — its maps, slices, and strings are plain
memory. The entire safety argument rests on a discipline: no goroutine ever
writes to a `Config` after the pointer to it has been published. If that holds,
readers need no lock, because reading immutable memory cannot race with
anything. If it breaks — one field patched in place after `Store` — you have a
data race that `-race` will find and production will eventually corrupt.

### Why it is safe: the memory-model argument

The Go memory model (go.dev/ref/mem) guarantees that an atomic store is
*synchronized before* the atomic load that observes it. Concretely: every write
the updater performed while building the new `Config` — filling its maps,
setting its fields — happens-before any reader dereferences the pointer it got
from `Load`. The freshly built object is fully visible, including its interior
pointers, with no fences or locks in reader code. This is also precisely why
the discipline is "build, then store" and never "store, then keep building":
writes performed *after* the `Store` have no happens-before edge to readers
that already loaded the pointer, so they are a race, not a late update.

### atomic.Pointer[T] vs atomic.Value

`atomic.Pointer[T]` (Go 1.19) is the typed API: `Load` returns `*T`, `Store`
takes `*T`, and there is no way to store the wrong type or trip a runtime
panic. `atomic.Value` predates generics and is interface-typed: `Store` takes
`any`, `Load` returns `any` (which is `nil` before the first `Store`, so a
blind type assertion can panic), `Store(nil)` panics at runtime, and storing a
value of a different concrete type than the first `Store` panics with
"inconsistently typed value". Prefer `Pointer[T]` in all new code; know
`Value` because pre-1.19 codebases and parts of the standard library still use
it, and because migrating such a store is a real task (exercise 3 does it).

Both types contain a `noCopy` guard: embed them in structs that are reached by
pointer, and never copy a struct containing one after first use. `go vet`
flags violations.

### Snapshot consistency, not global coherence

Two goroutines may `Load` a microsecond apart and see different config
versions. That is the contract: lock-free reads buy per-reader snapshot
consistency, not fleet-wide or even process-wide coherence. A request that
`Load`s once at the top and threads the snapshot down sees one coherent
version for its whole lifetime, which is exactly right for flags, limits, and
endpoints. It is *not* right for authorization decisions or invariants that
span multiple readers — those need a lock or a versioned protocol, because "my
neighbor saw v4 while I saw v3" is a correctness bug there, not a curiosity.

The per-request corollary matters in practice: `Load` once per request and
pass the snapshot down. Re-`Load`ing mid-request can observe a reload landing
mid-flight and mix two config versions inside one request — a new endpoint
with old credentials. It is the same class of bug as reading a global twice.

### Load-then-Store is not read-modify-write

`Load`, derive a new value, `Store` — two updaters doing this concurrently
both read v3, both build a v4, and one `Store` silently overwrites the other.
An update lost with no error anywhere. When updates derive from the current
value, or must be ordered (monotonic versions, "never roll back"), you need
either a single designated writer goroutine or a `CompareAndSwap` loop:

```
for {
	cur := p.Load()
	if candidate.Version <= cur.Version { return ErrStale }
	if p.CompareAndSwap(cur, candidate) { return nil }
	// lost the race; re-read and re-decide
}
```

Exercise 6 builds this defense for a distributed control plane, where a
lagging config-service replica can deliver pushes out of order.

### The cost model: why not RWMutex

`RWMutex.RLock`/`RUnlock` are each an atomic read-modify-write on the lock
word — a shared cache line that every reader core must bounce between caches.
As core count and read rate rise, readers serialize on that line and
throughput flattens or degrades. An atomic pointer load is a plain load with
acquire semantics on mainstream hardware: no store, no cache-line ping-pong,
essentially flat scaling. The trade is on the write side — every update
allocates and fully copies a new `Config` — so the idiom fits read-mostly
workloads: reload rates measured in seconds-to-minutes against read rates in
millions per second. Exercise 10 benchmarks both under contention so you hold
a measured number, not folklore.

### Last-good-config is an availability decision

A hot-reload pipeline will eventually be handed garbage: a truncated file
mid-deploy, a typo in JSON, a config that fails validation. The production
answer is that a bad push is an observable *non-event*: keep serving the last
good snapshot, record the error, expose staleness so an alarm fires if fresh
config stops landing. Crashing on a parse error turns a config typo into a
fleet-wide outage; half-applying it is worse. Exercises 4 and 9 build the two
halves — swap-only-on-success, and the introspection endpoint an on-call
engineer curls to answer "did my push actually land on this pod".

### Reload triggers are an operational contract

Three triggers dominate, and they share one swap path:

- SIGHUP: the Unix daemon convention. Operators and orchestrators send
  `kill -HUP` to ask nginx, HAProxy, and most Go sidecars to re-read config.
  One subtlety: `signal.Notify` never blocks sending to your channel — if the
  channel is full the signal is dropped — so the channel must be buffered
  (capacity at least 1) or a HUP arriving mid-reload vanishes.
- File mtime polling: works everywhere, no inotify dependency, tolerant of
  bind-mounted ConfigMaps whose inodes are swapped underneath you.
- Control-plane push: an RPC delivering a candidate config, which needs the
  CAS version-ordering defense above because replicas lag.

### Fan-out: components that cannot re-read a snapshot

Some dependencies cannot consume snapshots passively. An `http.Client`
timeout, a log level, a TLS cert baked into a live listener — these were read
once at construction and must be *rebuilt* when config changes. That needs a
change-notification fan-out with one iron rule: the publisher never blocks on
a slow subscriber. A buffered `chan struct{}` of capacity 1 with a
`select`/`default` send gives coalescing wakeups — a stalled subscriber
accumulates at most one pending notification and catches up by re-`Load`ing
the *current* snapshot, never by replaying missed events. Subscribers rebuild
from the snapshot, not from an event payload; the event only says "something
changed".

### GC makes the pattern trivially safe

In C++ this design needs hazard pointers or RCU: after swapping the pointer,
when is the old snapshot safe to free, given readers may still hold it? Go's
garbage collector dissolves the question — old snapshots stay alive exactly as
long as any reader holds a reference and are collected afterward, and ABA
cannot bite because a live pointer is never recycled. Keep this in mind when
reading lock-free literature: much of its difficulty is memory reclamation
that Go gives you for free.

## Common Mistakes

### Mutating a Config after it is stored

Patching a field on the snapshot returned by `Get`, or on a config already
`Store`d, is a data race every other reader observes. Always build a complete
new value and `Store` it; treat every published `*Config` as frozen.

### Sharing a mutable map through the snapshot

The pointer swap is atomic, but if the new `Config` embeds the caller's map by
reference, later writes to that map race with readers of the old or new
snapshot and can fatal the process ("concurrent map read and map write").
Deep-copy maps and slices at construction — `maps.Clone` — so the trust
boundary is the constructor.

### atomic.Value's runtime panics

`Store(nil)` panics. Storing a second concrete type (a `*ConfigV2` after a
`*Config`) panics with "inconsistently typed value". `Load` returns a nil
`any` before the first `Store`, so a blind type assertion panics too. These
are runtime failures the typed `atomic.Pointer[T]` makes unrepresentable.

### Load+modify+Store for derived updates

Two concurrent updaters silently lose one update. Use `CompareAndSwap` in a
loop, or serialize all writes through one goroutine.

### Re-loading mid-request

Reading the pointer twice in one request can mix fields from two versions.
Load once at the top, pass the snapshot down.

### Locking around an already-loaded snapshot

Taking a mutex to "stabilize" a snapshot you already hold adds cost and zero
consistency — the snapshot is immutable and cannot change under you.

### Copying a struct that embeds atomic.Pointer or atomic.Value

Breaks the noCopy contract; `go vet` flags it. Pass such structs by pointer.

### Crashing (or storing a zero Config) on a failed reload

A parse or validation failure must keep the last good snapshot serving and
surface through metrics and readiness — never through a crash or an empty
config.

### Blocking the reloader on subscriber channels

One stalled subscriber on an unbuffered channel freezes every future reload.
Use non-blocking coalescing sends: buffered channel of 1, `select` with
`default`.

### Unbuffered signal channel

`signal.Notify` drops a signal if the channel cannot accept it at delivery
time. A SIGHUP arriving while the worker is mid-reload is lost forever with an
unbuffered channel. Buffer it (capacity 1 is enough; the reload coalesces).

Next: [01-atomic-pointer-config-manager.md](01-atomic-pointer-config-manager.md)
