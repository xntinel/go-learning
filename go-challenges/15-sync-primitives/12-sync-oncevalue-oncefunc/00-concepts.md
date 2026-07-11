# sync.OnceValue, sync.OnceFunc, and sync.OnceValues: Lazy Initialization in Production Services — Concepts

Every long-lived Go service has a handful of expensive, share-everything
singletons: the `*sql.DB`, the tuned `*http.Client`, parsed templates, compiled
validators, registered metrics collectors. The bugs around them are on-call
classics — a client built per request exhausting ephemeral ports, a duplicate
Prometheus registration panicking on the second import path, a double `Close`
in a shutdown handler, a process that bricked itself by caching a transient
connect error forever. `sync.Once.Do(f)` has always been the primitive for
"run this exactly once", but the common shape — compute a value once, hand it
to every caller — is awkward with it: a separate `var` holds the result and the
closure has to capture it. Go 1.21 added three helpers that fold the result
into the closure itself. This file is the conceptual foundation for the nine
exercises that follow; each exercise is an independent, deployable module with
a race-enabled test suite proving the exactly-once contract under 50-100
concurrent goroutines.

## Concepts

### The three helpers and their exact signatures

The `sync` package defines, since Go 1.21:

```
func OnceFunc(f func()) func()
func OnceValue[T any](f func() T) func() T
func OnceValues[T1, T2 any](f func() (T1, T2)) func() (T1, T2)
```

Each returns a new function (a closure) that invokes `f` only once and may be
called concurrently. `OnceValue` caches the single return value; `OnceValues`
caches a pair, which in practice is almost always `(T, error)`; `OnceFunc` is
for side-effect-only initialization. The cached result lives in the closure's
hidden state, so the classic boilerplate — a package-level `var once sync.Once`
sitting next to a package-level `var cfg *Config`, coupled only by convention —
collapses into one declaration:

```
var getConfig = sync.OnceValue(loadConfig)
```

Concurrent callers during the first invocation block until `f` returns; that
is the single-flight property. There is no stampede: one goroutine runs `f`,
the rest wait and receive the same cached result.

### The panic contract: the helpers stay loud, Once.Do goes quiet

The documented contract (pkg.go.dev/sync): if `f` panics, the function
returned by `OnceFunc`/`OnceValue`/`OnceValues` panics **with the same value
on every call**, and `f` is never re-run. Compare that with raw
`sync.Once.Do`: a panic in `f` marks the `Once` as done, and every later
`Do` call returns silently, executing nothing. The difference matters in
production. With `Once.Do`, a panicking initializer can silently hand every
subsequent caller a half-initialized world — a nil logger, an empty
registry — and the failure surfaces far from its cause. The helpers keep the
failure loud and attributable: every caller that touches the broken singleton
sees the original panic value. Neither retries; if you want retry-after-panic
you must `recover` inside `f` and convert the panic into an error you control.

### Error caching is permanent — the senior judgment call

`OnceValues` caches the `(value, error)` pair forever. That is exactly right
for deterministic failures: a malformed DSN, a missing embedded file, an
unregistered driver — no number of retries will fix those, and re-running the
init just burns cycles hiding a deploy-time bug. It is catastrophic for
transient failures: if the database is unreachable for two seconds at the
moment of the first call, `OnceValues` latches that connection error and the
process can never recover without a restart, even though the database came
back. Classifying your init failure — permanent or transient — is the design
decision this lesson drills. When the failure class is transient, `Once` in
any form is the wrong tool; you build a mutex-guarded lazy value that re-runs
the init on the next call and only latches on success (exercise 05).

### The memory-model guarantee

The Go memory model (go.dev/ref/mem) specifies for `Once`: the completion of
`f()` is synchronized before the return of any call of the wrapped function.
That means everything `f` wrote — the struct it built, the map it filled, the
slices hanging off it — is fully visible to every caller, on every core, with
no additional locks or atomics. This is what makes the returned value safely
publishable: callers can read the cached `*http.Client` or `*template.Template`
freely because the once established the happens-before edge. The guarantee
covers reads that happen after the wrapped call returns; state you read
*outside* the once path (say, an "is it loaded yet?" flag checked without
calling the wrapper) still needs its own synchronization, typically an atomic
(exercise 02 makes this concrete).

### Lazy versus eager: where the cost and the failure move

`OnceValue` moves construction from startup (`init()`, a package-level `var`
initializer) to first use. The wins: faster boots, zero cost for code paths a
given deployment never exercises, construction that can read configuration
and environment that are not ready at package-init time, and testability —
a constructor function can be swapped in tests, an `init()` cannot. The costs:
the first request eats the construction latency, and a failing init surfaces
mid-traffic instead of at deploy time, when a crash loop would have been
caught by the rollout. The production pattern is to pair lazy init with a
warmup: call the wrapper once from `main` after configuration is loaded, or
hang it off the readiness probe, so the latency and any deterministic failure
land before user traffic does. Lazy init is a tool for decoupling construction
order, not an excuse to skip warmup.

### Reentrancy deadlocks

If `f` — directly, or through anything it calls — invokes the wrapped function
again, the program deadlocks, exactly as `once.Do(f)` calling `once.Do` does:
the inner call waits for a completion that can never happen because it is the
outer call. Initialization graphs must be acyclic. Keep init functions small
and dependency-free; if two singletons need each other, that is an
architecture smell to fix, not a synchronization puzzle to solve.

### The per-key once idiom

`OnceValue` is one once for one value. Production code often needs
once-per-key: parse each template exactly once, compile each regexp exactly
once, prepare each SQL statement exactly once — under concurrent request load.
The stdlib-only idiom is a `sync.Map` whose values are closures produced by
`sync.OnceValues`, installed with `LoadOrStore`. Two goroutines racing on a
cold key each build a closure, but `LoadOrStore` admits exactly one; the
loser's closure is discarded *unexecuted*, both goroutines call the winner's
closure, and its internal once guarantees a single parse. This is a miniature
`singleflight` for immutable results — simpler than the real
`golang.org/x/sync/singleflight` because the result never expires, so it can
be cached forever in the closure (exercise 06 builds it).

### Copy and identity rules

The returned closure owns hidden once-state, and the exactly-once guarantee
attaches to the closure *value*, not to `f`. Three consequences. First, every
caller must invoke the same closure: constructing the `OnceValue` inside a
handler or on every method call creates a fresh once per request, so the
expensive init runs every time and nothing is ever cached — the wrapper
silently degrades to a plain function call. Second, wrapping the same `f` in
two separate `OnceValue` calls yields two independent onces; `f` runs twice.
Third, the closures inherit the `sync.Once` "must not be copied" discipline:
store the closure once — at package level, or in a struct field set at
construction and never copied — and hand out pointers to the struct.

### Deduplication scope: the wrapper is not global idempotency

`OnceFunc` only deduplicates calls that go **through its returned closure**.
Code that calls the underlying function directly — a second bootstrap path, a
test helper, a library that registers the same collector on import — bypasses
the once entirely. If the underlying operation panics on duplication (as
`expvar.Publish` and Prometheus registries do), the wrapper only protects you
if it is the *only* entry point. Making an operation idempotent with
`OnceFunc` is therefore an architectural discipline — route every path through
the wrapper — not a property the API grants for free (exercise 09 proves both
sides with tests).

## Common Mistakes

### OnceValue for an init that can fail

Wrong: `var getDB = sync.OnceValue(func() *DB { db, _ := connectDB(); return db })`.
The error is dropped; every caller receives a nil handle with no explanation,
and the nil-pointer panic happens far from the failed connect.
Fix: use `sync.OnceValues(func() (*DB, error))` and propagate the error to
every caller.

### OnceValues for transient failures

Wrong: wrapping a database or broker connect in `OnceValues` when the first
call can fail on a network blip. The error is cached forever; the dependency
recovers, the process does not, and the only fix is a restart.
Fix: classify the failure. Deterministic failures (bad DSN, missing driver)
may be latched; transient ones need a mutex-based `Lazy[T]` that re-runs init
until it succeeds.

### Expecting retry or recovery after a panic

Wrong: assuming a panic in `f` will be retried on the next call. The wrapper
re-panics with the same value on every call and never re-runs `f`.
Fix: `recover` inside `f` and return an error (with `OnceValues`) if you need
different behavior; otherwise treat the panic as a permanent, loud failure.

### Confusing the helpers' panic behavior with Once.Do's

Wrong: porting code (or tests) between `once.Do(f)` and `sync.OnceFunc(f)`
assuming identical panic semantics. `Do` treats a panicking `f` as done and
later calls return silently — possibly observing half-initialized state; the
helpers re-panic on every call.
Fix: know which contract you are under; pin it with a test when it matters.

### Constructing the closure per call

Wrong: `func (s *Service) Config() *Config { return sync.OnceValue(s.load)() }`.
A fresh once is created on every call, so `s.load` runs on every call — the
cache never engages, and under load the "singleton" constructor becomes a
per-request cost.
Fix: create the closure once, in the constructor or at package level, and
store it in a field.

### Copy hazards

Wrong: storing the closure in a struct that gets copied (passed by value,
returned by value, put in a map). Each copy shares the closure — that part is
fine — but wrapping the same `f` twice, or rebuilding the wrapper in a copy's
setter, produces independent done-states and `f` runs more than once.
Fix: one closure, created once; pass the owning struct by pointer. `go vet`'s
copylocks check catches raw `sync.Once` copies but cannot see closure state,
so this one is on you.

### Calling the wrapped function from inside f

Wrong: `f` (or a dependency it calls) invokes the wrapped function again —
a cyclic initialization graph.
Fix: none at runtime; it is a guaranteed deadlock inherited from `sync.Once`.
Keep init functions leaf-like and break dependency cycles in the design.

### Assuming OnceFunc grants global idempotency

Wrong: wrapping `registerMetrics` in `OnceFunc` and believing duplicate
registration is now impossible. A second code path that calls
`registerMetrics` directly (or `expvar.Publish` with the same name) still
panics.
Fix: make the wrapper the only exported entry point; keep the raw function
unexported.

### OnceValue for reloadable configuration

Wrong: serving configuration from `sync.OnceValue(loadConfig)` in a service
that must pick up config changes without restarting. Once cached, the value is
immutable for the process lifetime.
Fix: reloadable state belongs in `atomic.Pointer[Config]` swapped by a
watcher; `OnceValue` is for values with process lifetime.

Next: [01-lazy-init-library.md](01-lazy-init-library.md)
