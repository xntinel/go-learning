# First-Class Functions and Closures for Production Backend Plumbing

Functions in Go are values. That single fact is the load-bearing beam under
almost every piece of backend plumbing you will ever write: the middleware
stack that wraps an `http.Handler`, the dependency injection you do without a
framework by passing a `func() time.Time` clock instead of calling
`time.Now()`, the retry/backoff wrapper that takes the operation to retry as an
argument, and the stateful primitives — rate limiters, circuit breakers,
memoizers, sequence generators — that keep their state private inside a captured
environment instead of a package-level global. This file is the conceptual
foundation for the independent modules that follow; read it once and you
have the model you need to reason through each artifact.

## The model: a function is a value with a type

A function value has a function type such as `func(int) int` or
`func(http.Handler) http.Handler`. You can assign it to a variable, pass it as
an argument, return it from another function, store it in a struct field, or put
it in a slice or a map value. The value carries the code to run; for a closure
it also carries a reference to the variables the code captured from its defining
scope. A named function type makes long signatures legible and makes the
function-passing style self-documenting:

```
type Middleware func(http.Handler) http.Handler
type Validator[T any] func(T) error
```

When you see `Middleware` in a signature you know exactly what shape of function
is expected, and the reader does not have to re-parse the same six-token type
every time.

### Function values are not comparable

Two function values cannot be compared with `==`; that is a compile error. The
only legal comparison is against `nil`. This has two practical consequences.
First, you cannot use a function as a map key or dedup a set of callbacks by
equality — you need some other identity (a name, an index). Second, calling a
nil function value panics at runtime, so any struct field or map entry of
function type must be checked (or guaranteed populated) before it is called.

## Closures capture by reference, and outlive the call

A function literal that references a variable from its surrounding scope becomes
a closure over that variable. The capture is by *reference*, not by value: the
closure and the enclosing scope share the same storage. A mutation the closure
makes is visible to the enclosing code and vice versa. To make this safe, the
compiler performs escape analysis and moves the captured variable to the heap so
it outlives the function call that created it. That is why a factory like
`NewSequence(start)` can return a `func() int64` that keeps incrementing a
counter long after `NewSequence` has returned — the counter lives on the heap,
kept alive by the returned closure.

Two facts follow directly and are the heart of this lesson.

First, **each call to an enclosing (factory) function creates a fresh, isolated
environment.** `NewSequence(0)` called twice yields two closures with two
independent counters. This is how closures give you per-instance private state
without declaring a struct: the captured variables *are* the private fields, and
the returned function *is* the only method. A rate limiter's token count, a
circuit breaker's failure count, a memoizer's cache map — each is a captured
variable owned by exactly one returned closure.

Second, **capture is late-binding.** The closure sees whatever value the
captured variable holds *at call time*, not the value it held when the closure
was created. Mutating the variable after taking the closure changes what the
closure observes. Engineers who expect a closure to freeze a snapshot of the
world are regularly surprised; if you want a snapshot, copy the value into a new
variable and capture that.

### Captured state pins memory

Because a closure keeps its captured variables reachable for as long as the
closure itself is reachable, capturing a large value pins that memory in the
garbage collector for the closure's whole lifetime. A closure stored in a
long-lived registry that captured a one-megabyte buffer keeps that megabyte
alive until the registry drops the closure. Capture only what you actually need,
and reach for a pointer only when the closure and the caller genuinely intend to
share mutation.

### Shared captured state across goroutines is a data race

The closure itself needs no lock. The *state it captures* does. If two
goroutines call a closure that reads and writes a captured counter or map, that
is a data race — one that a single-threaded test passes and `go test -race`
catches. The fix is to guard the captured state with a `sync.Mutex` or use the
`sync/atomic` types, and to prove it under the race detector. Every stateful
module in this lesson (metrics counter, rate limiter, circuit breaker, memoizer,
sequence generator, once-guard) is exercised by a `-race` concurrency test for
exactly this reason.

## Higher-order functions compose behavior

A higher-order function takes or returns functions. This is the tool that
replaces copy-pasted cross-cutting logic with composition. `Chain(mws...)`
composes a stack of middlewares into one. `Retry(ctx, policy, op)` wraps any
operation in attempt-and-backoff logic. `Memoize(load)` wraps a loader with a
cache. `Combine(validators...)` runs a pipeline of predicates and aggregates
their failures. In each case the cross-cutting concern — metrics, retries,
caching, validation — is written once as a wrapper and applied to many
operations, instead of being interleaved by hand into every handler.

Composition order matters and is easy to get backwards. A middleware chain is an
onion: for `Chain(a, b, c)` the request flows `a -> b -> c -> handler` and the
response unwinds `c -> b -> a`. The first-listed middleware is the outermost
layer, so it runs first on the way in and last on the way out. Put auth or
request-ID assignment early (outermost) and per-handler metrics late (innermost),
and get the nesting right, or a short-circuiting auth layer ends up running after
the work it was supposed to gate.

## Dependency injection through function parameters

The most valuable everyday use of first-class functions in backend code is
making time- and randomness-dependent logic deterministic in tests without
touching global state. Instead of calling `time.Now()` inside a rate limiter,
take a `now func() time.Time` parameter; instead of calling `time.Sleep` inside
a retry loop, take a `sleep func(time.Duration)`; instead of calling a global
RNG for jitter, take a `jitter func() float64`. Production wires in the real
`time.Now`, `time.Sleep`, and a real RNG; the test wires in a fake clock backed
by a mutable variable, a no-op sleeper, and a fixed jitter. The test asserts
"after advancing the clock one second the bucket refilled" in microseconds, with
no sleeping and no flakiness, and it does it without a package-level clock global
that every other test would have to coordinate on. This is the single technique
that turns slow, flaky time-based tests into fast, deterministic ones.

## Methods are functions with a receiver

A method is a function whose first parameter is the receiver, supplied with the
`receiver.Method` call syntax. Two ways to turn a method into a function value
are worth distinguishing precisely, because they power dispatch tables and
shutdown registries.

A **method value** — `c.Inc` — binds a specific receiver at the moment you take
it. The result is a `func()` (the receiver is already supplied) that you can
store and call later; the bound receiver stays reachable for as long as that
function value does. Registering `srv.Shutdown` and `pool.Close` into a shutdown
registry captures each method already bound to its instance, so calling them
later shuts down *those* specific objects.

A **method expression** — `(*Counter).Inc` — leaves the receiver as an explicit
first parameter. The result is a `func(*Counter)` you must call with a receiver
at the call site. Confusing the two (taking `(*Counter).Inc` and then trying to
call it with no arguments) is a compile error; reaching for a method expression
when you meant to bind a receiver is a common slip.

## The Go 1.22 loop-variable change

Before Go 1.22 the `for`/`for-range` loop variable was reused across iterations,
so a closure capturing it (a goroutine spawned in the loop, a handler stored in a
map) saw the loop variable's *final* value — the classic capture bug. Go 1.22
made the loop variable fresh per iteration, so the defensive `x := x` copy before
capturing is no longer required on 1.22 and later. On Go 1.21 and earlier you
still need it. Every module here targets modern Go, so no `x := x` shadowing
appears; know the history so you recognize the pattern in older code.

## Generics keep the wrappers type-safe

Higher-order helpers do not have to fall back to `any` or reflection. Generic
function wrappers — `Memoize[K comparable, V any]`, `Combine[T any]`,
`Validator[T any]` — stay fully type-checked and reusable across concrete types.
The memoizer caches `map[K]V` with the real key and value types; the validator
pipeline operates on your concrete request struct. Generics are what let a single
`Memoize` serve a config loader keyed by string and a user loader keyed by an
int without any type assertions.

## Common Mistakes

### Sharing captured mutable state without synchronization

Wrong: a closure captures a counter or map and is called from many goroutines
with no lock, on the theory that "the closure is just a function." The captured
*state* is shared mutable memory. A single-threaded test passes; `go test -race`
reports the data race. Fix: guard the captured state with a `sync.Mutex` or use
`sync/atomic`, and prove it under `-race`.

### Returning the internal map or slice from a getter

Wrong: a `Snapshot()` or getter returns the closure's (or struct's) internal map
directly, so a caller who mutates the returned map corrupts the private state.
Fix: return a defensive copy. A metrics snapshot must be a fresh map the caller
can scribble on without touching the live counters.

### Assuming closures capture by value

Wrong: taking a closure, then mutating the captured variable, and expecting the
closure to still see the old value. Capture is by reference and late-binding, so
the closure sees the new value. Fix: if you need a frozen snapshot, copy the
value into a fresh variable and capture the copy.

### Capturing more than you need (and, pre-1.22, the loop variable)

Wrong: capturing a large struct when the closure uses one field of it, pinning
the whole struct in memory for the closure's lifetime; or, on Go 1.21 and
earlier, capturing a range loop variable in a goroutine so every closure sees the
final value. Fix: capture only the fields you use; on old toolchains add `x := x`
before the capture.

### Confusing method value and method expression

Wrong: writing `var f func() = (*Counter).Inc` (a method expression whose
receiver is still an argument) and then calling `f()` with no receiver. Fix: take
a method value `f := c.Inc` to bind the receiver, or call the expression as
`(*Counter).Inc(c)`.

### Comparing function values or using them as map keys

Wrong: `if f == g` (compile error) or `map[func()]int` keyed by functions. The
only legal comparison is `f == nil`. Fix: identify callbacks by a name or index,
not by function equality; and never call a nil function value — it panics.

### Not injecting time and randomness

Wrong: a rate limiter, retry loop, or circuit breaker that calls `time.Now`,
`time.Sleep`, and a global RNG directly, so its test must sleep real seconds and
still flakes. Fix: inject a `now func() time.Time`, a `sleep func(time.Duration)`,
and a `jitter func() float64`; the test drives a fake clock and a no-op sleeper.

### Retrying permanent errors or ignoring cancellation

Wrong: a retry loop that keeps retrying a non-retryable (permanent) error, or
that ignores `ctx.Done()` and keeps spinning after the request was cancelled.
Fix: mark non-retryable failures with a `Permanent` sentinel the loop checks via
`errors.As`, and check `ctx.Err()` each iteration.

### Caching errors in a memoizer

Wrong: a memoizer that stores a transient failure and serves it forever. Fix:
decide and *document* whether errors are cached; the map memoizer here retries on
error (errors are not cached), while the `sync.Once` guard deliberately caches
its build error — and each module tests the behavior it documents.

### Getting the middleware chain order backwards

Wrong: composing a `Chain` so middlewares apply in reversed onion order, so auth
runs inside the handler it was meant to gate, or metrics count the wrong scope.
Fix: define and test the ordering explicitly — first-listed is outermost — with a
trace slice that records the before/after markers.

Next: [01-metrics-middleware-factory.md](01-metrics-middleware-factory.md)
