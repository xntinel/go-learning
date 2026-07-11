# sync.Once: Exactly-Once Initialization, Idempotent Teardown, and the Panic Contract

`sync.Once` is the primitive behind two patterns that saturate real backend
code: the lazy singleton and the idempotent teardown. One database pool per
process, built the first time a request needs it. One metrics registration, no
matter how many `init` paths run. One `Close()` on a resource shared by hundreds
of request goroutines during shutdown. One deprecation log line, however many
callers hit a legacy route. Each of these is "run this exactly once, safely,
under concurrency," and `Once` is the correct, minimal tool for it.

The senior skill here is not "call `Do`." `Do` is three lines. The skill is
knowing the sharp edges that turn a naive `Once` into a production outage: a
failed init that is cached forever with no retry, an unsynchronized read that the
race detector rightly flags, a copied `Once` that silently re-runs, and the
subtle, deliberate difference between `Once.Do` (which swallows a panic as
"done") and `OnceValue`/`OnceValues` (which replay it on every call). This file
is the model; each exercise that follows is an independent, self-contained
artifact that drills one edge.

## The exactly-once contract

`var once sync.Once; once.Do(f)` runs `f` exactly one time for the lifetime of
that specific `Once` value, across every goroutine that calls `Do`. If ten
goroutines call `once.Do(f)` at the same instant, `f` runs in exactly one of
them; the other nine block until `f` returns, then return themselves. Completion
is tracked by the `Once` *instance*, never by the function value you pass. This
is the first trap in miniature: `Do` keys on the receiver, so calling
`once.Do(a)` then `once.Do(b)` runs only `a` — `b` never executes. The standard
library documentation states it directly: "A new instance of Once is required
for each function to execute."

Because no call to `Do` returns until the single call to `f` returns, calling
`Do` recursively from inside `f` on the same `Once` deadlocks. Initialization
closures must not re-enter their own guard.

## The happens-before edge is the whole point

The Go memory model gives `Once` a specific guarantee: the return from `f`
"synchronizes before" the return from any call to `once.Do(f)`. That single
happens-before edge is the reason the lazy-singleton pattern is legal without any
further synchronization. Consider the canonical shape:

```
func GetDB() *DB {
	once.Do(func() { db = connect() })
	return db
}
```

The read `return db` is unsynchronized in the plain sense — it touches a shared
variable with no lock and no atomic. It is nonetheless race-free, but *only*
because it follows a `Do` call on the same `Once`, and the memory model
publishes the write to `db` (performed inside `f`) to every goroutine that
returns from `Do`. Move that read onto any code path that does not go through
`Do` first, and you have a genuine data race. It may pass a thousand runs and
then tear a pointer on the thousand-and-first, or the race detector will (rightly)
fire under `-race`. The discipline is absolute: every read of once-initialized
state must be dominated by a `Do` call on the guarding `Once`, or must be
promoted to an `atomic` of its own.

Internally this is double-checked locking done correctly. The fast path is a
single atomic load of a `done` flag — no mutex on the common, already-initialized
case. Only the first callers take an internal `Mutex` to elect exactly one
runner. Double-checked locking is notoriously hard to hand-roll correctly across
memory models; `Once` is the vetted implementation, which is a large part of why
you reach for it instead of a `Mutex` plus a `bool`.

## The panic contract: failure is cached forever

Here is the edge that causes real outages. If `f` panics, `Do` considers the
call to have *returned*. The `done` flag is set. Every future `Do` on that
instance returns immediately without ever running `f` again. There is no
built-in retry.

Now picture a naive lazy singleton around a network dial:

```
func GetConn() *Conn {
	once.Do(func() { conn = mustDial(addr) }) // panics if the dial fails
	return conn
}
```

If the first dial fails during a transient network blip and `mustDial` panics
(or even just leaves `conn` nil), the process is dead on arrival: `conn` is nil
forever, `Do` will never re-run, and every subsequent request gets a nil pointer.
The primitive gives you no retry, by design — retrying would risk running `f`
twice on later calls, violating exactly-once. If you need retry-after-failure,
that is a policy you build on top, and it requires a *new* `Once` instance for
each attempt (see the resettable-dialer exercise). Contrast this with a
hand-rolled `Mutex` + `bool`, where you own the retry semantics completely and
can choose to leave `done` false after a failure.

## Surfacing errors: capture in a field, or use OnceValues

`Do` takes a `func()` and returns nothing, so a fallible init cannot return an
error the normal way. The classic pattern captures the error into a struct field
(or a package variable) set inside the closure; every caller reads that field
after `Do` returns, and the happens-before edge makes the read safe:

```
func (s *Service) Connect() error {
	s.once.Do(func() { s.conn, s.err = dial(s.addr) })
	return s.err
}
```

Go 1.21 added `sync.OnceFunc`, `sync.OnceValue[T]`, and `sync.OnceValues[T1,T2]`,
which collapse that capture-in-closure boilerplate and return typed values. But
choosing between them is a deliberate error-semantics decision, not just a style
preference, because of one asymmetry the documentation states explicitly: if the
init function panics, `Once.Do` swallows the panic as "done" (future calls no-op),
whereas the function returned by `OnceValue`/`OnceValues` *replays the same panic
value on every call*. So `OnceValues` is the right migration target when your
parse/build function returns an `error` rather than panicking — you get typed
`(T, error)` with no field boilerplate. If your init can panic and you want the
panic captured and swallowed once, stay with `Once.Do` plus a `recover` inside
the closure. Migrating one to the other without accounting for this flips your
failure behavior.

## Idempotent teardown

Wrapping cleanup in a `Once` makes `Close()`/`Shutdown()` safe to call any number
of times from any goroutine — the canonical `io.Closer` robustness pattern.
During graceful shutdown, dozens of in-flight request goroutines may all reach
for the same connection's `Close()` at once; a `Once`-guarded teardown runs the
real flush-and-release exactly once, records the underlying error in a field, and
returns that same error to every caller. This is not merely tidy: guarding the
close with a bare `bool` and checking-then-setting it without synchronization is
itself a data race. The reason `Once` fits is precisely that its check-and-set is
atomic.

## Do not copy a Once

A `Once` holds a `done` flag and a `Mutex`. Copy it and the copy has its own
zeroed `done` flag, so `f` runs again on the copy — the identical class of bug as
copying a `Mutex`. Copies happen in ways that are easy to miss: embedding a
`Once` in a struct that you then return by value, storing that struct into a map
(map writes copy the value), or ranging over a slice of such structs and mutating
the loop copy. `go vet`'s `copylocks` analyzer is the guard and it is not
optional; the rule is to always hold a `Once` behind a pointer and pass the
containing struct by pointer. A method with a value receiver on a struct that
embeds `Once` receives a zeroed copy and re-runs init — a favorite silent bug.

## Recover inside the closure

The closure runs in whichever goroutine "won" the election to run it — which is
whichever goroutine's `Do` call got there first, not necessarily the goroutine
you would expect. If `f` panics and nothing recovers, that panic unwinds *that*
goroutine's stack, and an uncaught panic in a goroutine crashes the whole
process. For init that runs fallible or third-party code — a driver's
`Register`, a plugin's setup, a config parse over untrusted input — you defer a
`recover` inside the closure and convert the panic into a captured error. Callers
then get a controlled `ErrInitFailed` instead of a dead process, and you have
deliberately opted into the panic-is-done trap in exchange for not crashing. That
recover-inside-`Do` pattern, together with the resettable-`Once` retry wrapper,
are the two things you cannot get from the primitive alone, and they are what
separate a robust init path from an outage.

## Do is uncancelable: the cancellation policy is yours to build

`Do` takes a `func()` and returns when that function returns — full stop. There
is no context parameter, no timeout, no way for a blocked caller to give up.
Under normal init that is invisible; under a *hanging* init it is the outage
pattern: the first request of the day triggers a lazy dial to a database whose
network path is black-holing packets, the dial sits in TCP retry for minutes,
and every other request goroutine that touches the getter parks inside `Do`
behind it. The service is not down because the database is down; it is down
because the exactly-once guard turned one hung dial into a process-wide
convoy, and no request deadline can free the waiters.

The production fix keeps the election and moves the waiting. The `Do` closure
does exactly one cheap thing — spawn the init in a dedicated goroutine that
writes the result and error into fields and then closes a `done` channel — so
`Do` returns immediately for everyone. Waiters then `select` between `done` and
their own `ctx.Done()`. A caller whose budget expires walks away with
`ctx.Err()` while the init keeps running for later callers, and the
happens-before edge that used to come from `Do` now comes from the channel:
fields are written *before* `close(done)`, and a close happens before any
receive that completes because of it. The crucial semantic to internalize (and
to document at the call site) is that cancellation detaches the *waiter*, never
the *work* — `ctx.Err()` from such a getter does not mean init failed, was
skipped, or was rolled back.

## Granularity: one Once, a map of Onces, or singleflight

"Run this once" is underspecified until you say once per *what*, and the three
answers are three different tools with three different failure modes.

Once per *process* is `sync.Once` (or `OnceValue`): one pool, one TLS config,
one registration. Once per *key* per process is a mutex-guarded
`map[string]*entry` where each entry carries its own `Once` and its own cached
error — per-tenant migrations, per-shard warmup. The implementation detail that
separates it from a global lock is releasing the map mutex *before* calling the
entry's `Do`, so distinct keys initialize concurrently and only same-key
callers coalesce; hold the lock across `Do` and every key serializes behind the
slowest. Once per *in-flight call* is `golang.org/x/sync/singleflight`:
concurrent callers of a key share one execution, but the result is not cached —
after the flight lands, the next caller runs the function again.

Choosing wrong in either direction is a real bug. `singleflight` where you
meant once-per-process re-runs init after every completion (re-registering,
re-migrating, re-dialing). `Once` where you meant per-call dedupe caches a
transient failure forever — a cache-fill that hit one timeout returns that
timeout to every caller for the life of the process. And one global `Once`
where you meant per-key silently starves every key after the first: the first
tenant migrates, all others never do, and the error surfaces far away as
missing tables.

## Registration-panic ecosystems

Some of the most-used registries in the ecosystem panic on duplicates by
design: `prometheus.MustRegister` panics if a collector with the same
descriptor is already registered, and `database/sql.Register` panics on a
duplicate driver name. The contract is reasonable — duplicates are programmer
errors — but it interacts badly with constructors that run more than once per
process, which is most of them: a handler constructor called once per route
that mounts it, once per test that builds the mux, once per hot-reload cycle.
Any registration that lives inside such a constructor must be guarded by a
`Once` (or hoisted to a truly once-per-process location). An unsynchronized
`bool` check is not a substitute — concurrent route wiring makes the
check-then-register a race — and `init()` is a blunt substitute with costs of
its own, covered next.

## Publish-then-freeze for shared objects

The happens-before edge of `Do` publishes everything written *inside* the
closure to every goroutine that returns from `Do`. It says nothing about writes
performed *after* publication. A `*tls.Config`, a parsed configuration struct,
a compiled router — any shared object built under a `Once` — must therefore be
built completely inside the closure and treated as frozen from the moment `Do`
returns. Appending one more root CA to the published pool or flipping one field
"just for this debug session" is a data race against every concurrent reader,
even though every one of those readers obtained the pointer safely. If the
object must change at runtime, that is not mutation but *replacement*: build a
fresh instance and swap it through an `atomic.Pointer[T]`, which is a different
lesson's primitive (see the config hot-reload lesson later in this chapter).

## init() versus lazy Once

Package `init()` also runs exactly once, so it is the obvious rival. The
trade-offs are concrete. `init()` runs unconditionally at import time: you pay
its cost even on code paths that never use the resource (a CLI subcommand that
imports the package for one type still dials the pool), it has no error
channel (failure means panic or a package-level error variable someone must
remember to check), it cannot take parameters, and its ordering is dictated by
the import graph rather than by you. A lazy `Once` defers the cost to first
use, gives you a real `(T, error)` shape, and lets the caller decide policy.
The rule of thumb: `init()` is acceptable for cheap, infallible wiring
(registering an encoding, populating a static table); anything fallible,
expensive, or configurable initializes lazily behind a `Once`.

## Common Mistakes

### Calling Do with different functions in a loop

Passing a different closure to `Do` on each loop iteration and expecting each to
run. Only the first ever runs, because `Do` keys on the `Once` instance, not the
function value. Use one `Once` per distinct operation (for example, a
`map[string]*sync.Once`), or a plain `Mutex` + `bool` loop where you control the
semantics.

### Expecting Do to retry after a panic or an error

The failure is permanently cached. A dial that panics once leaves the singleton
dead forever; a closure that sets an `err` field once will keep returning that
same error on every subsequent `Do`. Retry-after-failure requires installing a
*fresh* `Once` instance for the next attempt, guarded by a `Mutex`, with a bound
on attempts — not re-calling `Do` on the same instance.

### Reading once-initialized state off the Do path

The happens-before edge only covers a read that *follows* a `Do` call on the same
`Once`. Reading the initialized field from any other code path is a data race —
one the detector will flag under `-race`, or, worse, one that silently corrupts
on an unlucky scheduling. Keep every read dominated by a `Do`, or wrap the field
in an `atomic`.

### Copying a struct that embeds sync.Once

Returning such a struct by value, storing a copy into a map, or ranging and
mutating a loop copy each gives you a fresh `Once` with a zeroed `done` flag, so
init re-runs. `go vet copylocks` is the gate. Always hold `Once` behind a pointer
and pass the struct by pointer; never give a value receiver to a method that
calls `Do`.

### Being surprised that OnceValue/OnceValues replays a panic

If the function you wrapped panics, the function returned by
`OnceValue`/`OnceValues` re-panics with the same value on every call. That is by
design and is the opposite of `Once.Do`, which swallows the panic as done. Pick
the primitive based on whether you want the panic swallowed once or replayed,
and make fallible init return an `error` rather than panic when you use
`OnceValues`.

### Putting a Once value (not a pointer) as a struct field, then passing the struct by value

The receiver's copy has a zeroed `done` flag and re-runs init. This is the
copy-lock bug wearing a different hat; the fix is the same — pointer receivers
and pointer-held structs.

### Guarding Close with a bare bool

Checking and setting a `bool` to make teardown idempotent, without
synchronization, is itself a read/write data race. The entire reason `Once` fits
the teardown pattern is that its check-and-set is atomic; a `bool` is not.

### Assuming the closure runs in a goroutine that can safely panic

The closure runs in whichever goroutine won the election, and an uncaught panic
there unwinds that goroutine and crashes the process. If init can panic, defer a
`recover` inside the closure so the panic becomes a captured error on a
controlled path.

### Letting a hanging init block every caller forever

`Do` cannot be canceled: a network dial or cert fetch that hangs inside the
closure parks every goroutine that calls `Do` with no context escape, turning
one stuck dependency into a process-wide convoy. Wrap slow init in a goroutine
plus a `done` channel and have waiters `select` against `ctx.Done()`.

### Using one global Once for a per-key operation

The first tenant's migration runs; every other tenant's silently never does,
and the failure surfaces far from the guard as missing tables or cold shards.
Per-key exactly-once needs a mutex-guarded `map[string]*entry` with a `Once`
and a cached error per key.

### Holding the map mutex across the per-key Do

That serializes every key behind the slowest migration — a global lock in
per-key clothing. Find-or-insert the entry under the lock, release, then call
`Do` on the entry.

### Registering metrics or drivers in an unguarded constructor

`prometheus.MustRegister` and `database/sql.Register` panic on duplicates, and
handler constructors run once per route, per test, per hot reload. The second
construction panics in production. Guard the registration call with a `Once`
owned by the instrumentation.

### Picking singleflight when you meant Once, or Once when you meant singleflight

`singleflight` dedupes only *in-flight* calls and caches nothing, so using it
for once-per-process init re-runs the init after every completion. `Once`
caches the first outcome forever, so using it for per-call dedupe pins a
transient failure for the life of the process. Match the tool to the contract:
per process, per key, or per in-flight call.

### Mutating a once-published shared object

The `Do` edge makes the pointer read safe; it does not license writes after
publication. Appending to a published `tls.Config`'s pool or flipping a field
on a published config struct races with every reader. Build fully inside the
closure, freeze on publication, and replace via `atomic.Pointer` if it must
change.

### Closing the done channel before writing the result fields

In the goroutine-plus-channel variant, the channel close is the happens-before
edge. Close first and assign after, and every waiter's read of the result
fields is a race. Fields first, `close(done)` last.

### Treating a waiter's ctx.Err() as a rolled-back init

Cancellation detaches the waiter, not the work: the init goroutine keeps
running and its outcome is still cached for later callers. Logging a waiter's
`context.Canceled` as an init failure invents incidents that never happened —
and any resources init acquired are still held.

Next: [01-once-init-service.md](01-once-init-service.md)
