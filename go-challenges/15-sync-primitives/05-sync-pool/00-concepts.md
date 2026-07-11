# sync.Pool: GC-Pressure Relief on Hot Paths — Concepts

Allocations are not free. In a high-RPS Go service the garbage collector runs
more often and burns more CPU as allocation pressure climbs, and the objects
that dominate that pressure are usually the small, per-request temporaries a
handler churns through: a `bytes.Buffer` to encode a JSON response, a
`gzip.Writer` to compress it, a `bufio.Reader` to scan a request body, a
scratch `[]byte` to stage a copy. `sync.Pool` is the standard tool for reusing
exactly those objects across requests so they do not have to be allocated and
collected millions of times a minute.

It is also one of the sharpest footguns in the standard library. It is not a
cache, it is not a connection pool, and it only pays off when you have measured
that it does. A senior engineer reaches for `sync.Pool` last, not first: after
`go test -benchmem` shows the allocations, after a pprof alloc profile or
`GODEBUG=gctrace=1` shows the GC cost, and only on a path hot enough that the
saving is real rather than assumed. This file is the conceptual foundation for
the ten independent exercises that follow; read it once and you have the model
you need to reason through every one of them.

## The model: a free-list of short-lived temporaries, not a cache

`sync.Pool` is a set of temporary objects that may be reused. `Get` removes an
object from the pool and returns it (calling the optional `New` function if the
pool is empty); `Put` adds an object back. The single most important sentence in
the documentation is the guarantee it does *not* give you: "any item stored in
the Pool may be removed automatically at any time without notification." A pool
is a place to *stash* an object you are done with in case someone wants it soon,
not a place to *store* an object you will need later. If you put your only copy
of some state into a pool, a later `Get` is free to hand you back a brand-new
object from `New` instead — your state is simply gone.

That is why the intended contents are per-operation scratch objects with no
identity: this request's encode buffer, this connection's read buffer. When the
operation ends you `Put` the object so the next operation can skip the
allocation, and you never again care about that specific instance. Every value
you get from the pool must be treated as opaque and possibly dirty until you
have reset it (see below).

## The GC interaction, and why a benchmark shows 0 allocs/op

The runtime clears pools as part of garbage collection, which is what keeps a
pool from turning into an unbounded memory leak. Since Go 1.13 the clearing is
softened by a *victim cache*: instead of dropping every pooled object on each
GC, objects survive one collection in a secondary "victim" list before being
fully released on the next. This smooths reuse across a single collection and
avoids an allocation cliff the instant a GC finishes.

The practical consequence shows up in benchmarks. A steady-state
`BenchmarkPool` reports `0 allocs/op` because after warmup every `Get` finds a
recycled object; but a *cold* pool, or the first iteration after a GC that
drained even the victim cache, allocates. This is also why you must never assert
an exact allocation count in a correctness test (more on that under Common
Mistakes) — reuse is a statistical property here, not a guarantee about any
single call.

## Internals: per-P sharding is why it scales and why counts are fuzzy

`sync.Pool` is sharded per-P (per logical processor, i.e. per `GOMAXPROCS`
slot). Each P has a private single-object slot plus a lock-free shared queue
that other Ps can steal from. `Get` first tries its own P's private slot (no
atomics, no contention), then its shared queue, then steals from other Ps, and
only then calls `New`. This is why the pool scales to many cores without a
central mutex — the fast path touches only the current P's private slot.

It is also why you cannot assert "exactly one object was allocated." Under
`GOMAXPROCS > 1` you may have up to one live object per P simultaneously, and
`go test -race` adds extra scheduling that changes how many Ps are in play. The
honest test asserts a *range* — "allocated far fewer than the total number of
operations, so reuse clearly happened" — or measures `allocs/op` with
`-benchmem`, never a fixed integer.

## Store pointers, not values: the SA6002 allocation

`New` and `Put` both traffic in `any`. Putting a *pointer* into an interface
value is free — the interface just holds the pointer. Putting a *non-pointer*
value (a `[]byte`, a struct, an array) into an interface value forces the
runtime to box it: it heap-allocates a copy so the interface has something to
point at. So a pool of `[]byte`-by-value allocates on every single `Put`, which
is precisely the allocation the pool was supposed to eliminate — the
optimization inverts into a pessimization. The rule from the docs is blunt: "the
Pool's New function should generally only return pointer types." Store `*[]byte`,
not `[]byte`; store `*T`, not `T`. `staticcheck` flags the violation as
[SA6002](https://staticcheck.dev/docs/checks/#SA6002); `go vet` does not, so this
is a bug the compiler happily ships.

## The reset contract is the sharpest correctness edge

A pooled object arrives carrying whatever state its previous user left in it. The
buffer still holds the last response's bytes; the gzip writer is mid-stream; the
hasher has already absorbed someone else's payload. Reusing it without clearing
that state is not a cosmetic bug — in an HTTP handler it leaks one request's data
into another request's response, which is a data-integrity failure and often a
security one. Every poolable type has a reset entry point, and calling it before
reuse is mandatory:

```text
bytes.Buffer.Reset()   clears the buffer (keeps capacity)
gzip.Writer.Reset(w)   clears state AND rebinds to a new destination w
bufio.Reader.Reset(r)  clears state AND rebinds to a new source r
hash.Hash.Reset()      restores initial state (for HMAC, the keyed state)
```

Note the two flavors. `bytes.Buffer.Reset` and `hash.Hash.Reset` only *clear*.
`gzip.Writer.Reset(w)` and `bufio.Reader.Reset(r)` clear *and* rebind to a new
destination or source — and that rebinding is exactly what makes those types
poolable across different connections, because a single recycled writer can be
pointed at whichever `ResponseWriter` the current request owns. For a keyed HMAC
hasher, `Reset` returns it to the state right after the key was mixed in, so the
same secret is reused for free while the previous payload is wiped.

## Bounding memory: cap capacity on Put

Because the pool retains every `Put` object (until a GC) at whatever capacity it
grew to, an unbounded buffer pool has a nasty failure mode under load. A single
burst of huge payloads grows several buffers to megabytes; those buffers get
`Put` back and pinned, one live per P, until the next GC reclaims them. Your
steady-state memory footprint silently rises to "the largest thing ever pooled,"
which is a resource leak driven by your worst-case traffic. The defense is to
inspect capacity on `Put` and simply *drop* (do not return) any object larger
than a threshold, letting it be collected normally. A dropped-count metric makes
the cap observable so you can see how often a spike is hitting it.

## Measure before you pool

The only justification for a pool is reduced GC pressure, and that is
measurable: `allocs/op` and `B/op` from `go test -benchmem` for the micro
signal; GC CPU percentage, heap growth, and p99 latency under load for the macro
signal, via pprof alloc profiles and `GODEBUG=gctrace=1`. Pooling on a path that
is not hot adds reset bugs, SA6002 traps, and memory-cap hazards for no win at
all. The standard library validates *where* it pays: `fmt` pools its printer
buffers, `encoding/json` pools encoder state, `net/http` pools request-scoped
structures, and `compress/gzip` is built with `Reset` specifically so writers
can be pooled — every one of them a short-lived, per-operation object on a
genuinely hot path.

## Common Mistakes

### Forgetting to reset before reuse

Wrong: `Get` a buffer (or writer, or hasher) and use it without clearing the
previous user's state, or `Put` it back still dirty. In a handler this leaks one
request's bytes into another's response — a data-integrity and security bug that
surfaces as "spurious data in an unrelated operation," which is famously hard to
trace. Fix: call the type's reset (`Buffer.Reset`, `gzip.Writer.Reset(w)`,
`bufio.Reader.Reset(r)`, `hash.Hash.Reset`) at a single well-defined point —
immediately after `Get` or immediately before use — every time.

### Storing values instead of pointers

Wrong: a `sync.Pool` of `[]byte` or of a struct by value. Boxing the value into
`any` on every `Put` allocates, so the pool allocates more than it saves. Fix:
store `*[]byte` / `*T`. `staticcheck` SA6002 catches this; `go vet` does not.

### Treating the pool as a cache for long-lived state

Wrong: stash a config object, a session, or a database connection in a
`sync.Pool` to avoid recreating it. A GC cycle drops it and the next `Get`
returns a fresh (or `nil`) object with none of your state. Fix: use a
startup-initialized pointer or `sync.Map` for caches, and a real connection pool
(with its own lifecycle) for connections.

### Omitting New and then type-asserting

Wrong: a zero `sync.Pool` with no `New`, then `p.Get().(*bytes.Buffer)`. On an
empty pool `Get` returns `nil`, and the assertion panics with a nil dereference.
Fix: always set `New`, or guard the assertion with a comma-ok check.

### Pooling arbitrarily large objects with no cap

Wrong: `Put` every buffer back regardless of how large it grew. Steady-state
memory rises to the biggest payload ever seen and stays there until GC. Fix:
check `Cap()` on `Put` and drop oversized objects so they are collected instead
of pinned.

### Not Closing a gzip.Writer before Put

Wrong: `Put` a `*gzip.Writer` without calling `Close`. The GZIP footer (the
CRC-32 and length trailer) is only written on `Close`, so an un-Closed writer
yields a truncated, undecodable stream, and reusing it without `Reset` compounds
the corruption. Fix: `Close` to flush the footer before `Put`, then `Reset(w)`
to rebind on the next `Get`. `Close` on `gzip.Writer` does not close the
underlying writer, so this is safe with a pooled writer.

### Asserting a specific allocation count in a test

Wrong: "exactly one buffer was allocated." Per-P sharding, `GOMAXPROCS`, the
extra Ps under `-race`, and GC timing all make the count nondeterministic. Fix:
assert a reuse *range* (`allocated < total ops`) in correctness tests, and use
`-benchmem` for `allocs/op` when you need a number.

### Comparing signatures with == or bytes.Equal

Wrong: on a pooled-hash webhook verifier, compare the computed MAC to the
provided one with `==` or `bytes.Equal`. A length- or content-dependent compare
time is a signature oracle. Fix: use `hmac.Equal`, which is constant-time.

Next: [01-typed-buffer-pool.md](01-typed-buffer-pool.md)
