# Anonymous Functions in Production Go — Concepts

An anonymous function is a function value with no declared name, written inline as
a function literal: `func(params) ret { body }`. In a senior backend codebase it
is not a syntax curiosity but the connective tissue of concurrency and control
flow: the goroutine bodies in a bounded worker pool and an `errgroup` fan-out, the
deferred closure that times a handler and turns a panic into a 500 instead of a
crashed process, the middleware constructor that returns a handler closing over
config, the unit-of-work callback handed to a transaction runner that owns
commit/rollback, the cleanup registered with `context.AfterFunc`, the
`sync.OnceValue`-wrapped lazy initializer for an expensive shared resource, and
the comparator handed to `slices.SortFunc`. This file is the conceptual
foundation; read it once and every one of the ten independent exercises that
follow is a variation on the same small set of rules.

## Concepts

### A function literal is a first-class value

`func(params) ret { body }` evaluates to a value of a function type. Because it is
a value you can assign it to a variable, pass it as an argument, return it from
another function, store it in a struct field, put it in a slice or map, and invoke
it. "Anonymous" only means the literal has no name at its definition site; in every
other respect it is an ordinary function. A named function `func f() {}` and the
literal `f := func() {}` compile to the same kind of thing — a function value — the
difference is where the name lives.

### A function literal is a closure: it captures by reference

A function literal closes over the free variables it references — the variables
from the enclosing scope that it uses but does not declare. The capture is by
*reference to the enclosing variable*, not by copying the value at the moment the
literal is defined. Reads inside the literal see whatever value the outer variable
holds when the literal actually runs, and writes inside the literal are visible to
the outer scope. This single rule explains almost everything else in the lesson:
it is why a deferred closure can observe (and rewrite) a named return value that is
assigned after the closure was registered, why an immediately-invoked literal can
compute a value using locals that then vanish, and why capturing one shared mutable
accumulator from many goroutines is a data race.

If you need a *snapshot* of a variable's value at a particular instant rather than
a live reference, do not capture it — pass it as an argument, or copy it into a
fresh local first. That distinction is load-bearing.

### Immediately-invoked function expression (IIFE)

`func() T { ... }()` defines a literal and calls it on the spot, yielding its
result. The trailing `()` is the invocation. Two production uses justify it. First,
scoping: a struct-field initializer or a one-shot validated build often needs
temporary locals (a builder map, an intermediate slice, an `err`) that must not
leak into the surrounding block; wrapping the work in an IIFE confines those locals
and hands back only the finished value. Second, localized error handling: an IIFE
lets you run `if err != nil { ... }` logic to produce a single value inline, and
the `must := func() T { if err != nil { panic(err) }; return v }()` idiom turns an
unrecoverable startup error into an immediate, loud panic at init time rather than
a zero value that fails mysteriously later. Reach for an IIFE only when it earns
its keep; a plain top-level assignment is clearer when there are no locals to hide.

### Deferred function literals and named returns

`defer` schedules a call to run after the surrounding function's return values have
been assigned but before the function actually returns to its caller. A deferred
function literal therefore observes the *final* values of everything it closes
over — including named return values. That is the mechanism behind three
production patterns that all live in the same `defer func() { ... }()`: reading the
named `err` to record success or failure and latency, wrapping or annotating that
`err` before it reaches the caller, and converting a panic into a returned error.
Because the closure captures the named result variable by reference, assigning to
it inside the deferred literal changes what the caller receives. A function with an
unnamed return cannot be instrumented this way — you cannot reach the value.

### recover() only works directly inside a deferred function

`recover()` stops a panic and returns the panic value, but only when it is called
*directly* inside a deferred function. Call it from a helper that the deferred
function invokes and it returns `nil` and the panic keeps unwinding. So the
canonical place to turn a panic into a returned error — keeping a request handler
or a pool goroutine alive instead of taking down the process — is a deferred
function literal that calls `recover()` in its own body and assigns the result to
the named `err`.

### Go 1.22 changed loop-variable scoping — but not shared-state safety

Before Go 1.22, the loop variable in a `for i := range xs` or three-clause `for`
was a single variable reused across iterations, so a closure that captured it saw
the final value; the fix was the defensive `i := i` shadow. Go 1.22 made each
iteration get a fresh copy of the loop variable, so that shadow is now redundant.
The `go` directive in `go.mod` selects the semantics for the whole module: a module
declaring `go 1.22` or later gets per-iteration variables.

This fix is narrow, and misreading its scope is a classic production bug. It makes
per-iteration *loop variables* safe to capture. It does *not* make goroutine
closures safe in general: if several goroutine literals capture and mutate one
*shared* variable — appending to the same slice, writing the same map, incrementing
the same counter — that is still a data race the race detector will flag, and the
1.22 change does nothing for it. The correct fix for shared state is
synchronization (a mutex, a channel, index-partitioned writes), not the loop-var
change.

### Passing per-item data as an argument

`go func(x T) { ... }(v)` evaluates `v` at the call site and hands each goroutine
its own copy through the parameter. Even after the 1.22 loop-var fix made bare
capture of the loop variable correct, passing the per-item value as an argument
remains the clearest way to express "this goroutine gets exactly this value," and
it is the only correct way when the value is not a loop variable but something you
derived inside the loop body.

### sync.WaitGroup: Add before, Done first

A `sync.WaitGroup` counts outstanding goroutines. The discipline is exact: call
`wg.Add(1)` *before* the `go` statement, make `defer wg.Done()` the *first*
statement inside the goroutine literal, and call `wg.Wait()` after the launch loop.
Adding inside the goroutine is a race: `Wait` can observe a zero counter and return
before the goroutine has even registered, silently dropping work. Deferring `Done`
first — before any fallible work — guarantees the counter is decremented even if
the body panics, so `Wait` cannot deadlock on a goroutine that died.

### Once-only memoization: sync.OnceFunc / OnceValue / OnceValues

Go 1.21 added three wrappers that take an initialization function literal and
return a memoized function that runs the body at most once, safely under
concurrency. `sync.OnceFunc(f func()) func()` runs `f` once for its side effect.
`sync.OnceValue[T](f func() T) func() T` runs `f` once and caches the value every
caller receives. `sync.OnceValues[T1, T2](f func() (T1, T2)) func() (T1, T2)` does
the same for a value and an error, caching *both* so a failed init returns the same
error on every subsequent call without re-running. These replace the older
`sync.Once` + package-global-pointer boilerplate for lazy resources — a hand-rolled
double-checked lock is easy to get subtly wrong (a visibility bug, a re-init race),
and the wrapper is shorter and correct. The init literal closes over whatever
configuration it needs.

### context.AfterFunc: cleanup tied to a context

`context.AfterFunc(ctx, f func()) (stop func() bool)` (Go 1.21) registers a
function literal to run in its own goroutine when `ctx` is done — cancelled or
past its deadline. It is the idiomatic way to tie a resource's cleanup to a
context lifetime: release a lease, close a stream, decrement a gauge. It returns a
`stop func() bool`: call `stop()` on the normal completion path to deregister the
literal so cleanup does not also run there. `stop()` returns `true` if it prevented
`f` from running, `false` if `f` had already been started (or already stopped).
Two facts must stay front of mind: `AfterFunc` does *not* wait for `f` to finish,
so tests and shutdown paths that assume cleanup already ran must synchronize
explicitly (a channel, an atomic); and if you ignore the returned `stop`, cleanup
runs on the normal path too, causing double cleanup or a leaked registration.

### errgroup: anonymous tasks with first-error cancellation

`golang.org/x/sync/errgroup` runs anonymous `func() error` tasks concurrently.
`errgroup.WithContext(ctx)` returns a group and a derived context that is cancelled
the moment the first task returns a non-nil error, so sibling tasks that watch the
context can bail out early. `(*Group).Go` submits a task literal, `(*Group).Wait`
blocks until all submitted tasks finish and returns the first non-nil error, and
`(*Group).SetLimit(n)` bounds how many tasks run at once. errgroup synchronizes
around `Wait`, but that does not make the tasks' *writes* safe: each literal must
write only to its own partition of shared data — its own index in a preallocated
slice — or you have a race despite errgroup.

### Callbacks: the caller supplies behavior, the callee owns control flow

Handing a function literal to a callee — a transaction runner, a retry loop, a
middleware constructor, a sort — lets the caller express *what* to do inline while
the callee owns *when and how*: the resource lifetime, the retry policy, the
begin/commit/rollback, the iteration. `WithinTx(ctx, db, func(tx *Tx) error { ... })`
is the archetype: the caller's literal captures local request data and describes
the unit of work; the runner owns the transaction boundary and guarantees rollback
on error or panic. This inversion is why anonymous functions show up wherever a
library wants to lend you a scoped resource safely.

### The comparator contract for slices.SortFunc

`slices.SortFunc[S, E](x S, cmp func(a, b E) int)` sorts using a comparator literal
that must return a three-way integer: negative if `a` sorts before `b`, zero if
they are equal, positive if `a` sorts after `b`. The comparator must define a
strict weak ordering — in particular it must return 0 for elements that are equal
on the sort keys, and it must be antisymmetric (`cmp(a,b)` and `cmp(b,a)` have
opposite signs). `cmp.Compare[T]` produces the three-way int for ordered types, and
`cmp.Or(vals...)` returns the first non-zero of a sequence, which composes
multi-key comparators cleanly: `cmp.Or(cmp.Compare(b.Sev, a.Sev), a.Time.Compare(b.Time))`.
`SortFunc` is *not* stable — equal elements may be reordered — so use
`slices.SortStableFunc` when elements equal on the keys must retain their input
order.

## Common Mistakes

### Calling wg.Add inside the goroutine

Wrong: `go func() { wg.Add(1); defer wg.Done(); ... }()`. `Wait` can run before the
goroutine schedules and registers, see a zero counter, and return — the job is
silently dropped and results are missing.

Fix: `wg.Add(1)` on the line before the `go` statement, so the count is raised
before any goroutine can be waited on.

### Deferring Done after real work

Wrong: doing the job first and calling `defer wg.Done()` (or a bare `wg.Done()`)
only after it. If the work panics, `Done` never runs, the counter never reaches
zero, and `Wait` deadlocks.

Fix: make `defer wg.Done()` the first statement in the literal, before anything
that can fail.

### Assuming Go 1.22 made all goroutine closures safe

Wrong: relying on the loop-variable fix and then appending to a shared slice or
writing a shared map from many goroutine literals. The fix gives each iteration its
own loop variable; it does nothing for a shared accumulator, which remains a data
race the detector flags.

Fix: partition writes by index into a preallocated slice, or funnel results through
a channel or a mutex-guarded structure.

### Expecting a deferred closure to snapshot a value

Wrong: capturing a variable in a deferred literal and expecting the value it had at
`defer` registration time. The literal observes the variable's *final* value (or
the named return), which surprises people who wanted an earlier snapshot.

Fix: if you need the earlier value, copy it into a fresh local (or pass it as an
argument to the deferred call) before the variable changes.

### Calling recover() from a helper

Wrong: a deferred literal that calls `myRecover()`, a helper which itself calls
`recover()`. `recover()` only works when called directly in a deferred function, so
this returns `nil` and the panic keeps unwinding.

Fix: call `recover()` directly in the body of the deferred function literal.

### Hand-rolling sync.Once instead of OnceValue

Wrong: a package-global pointer guarded by `sync.Once` with hand-written
double-checked locking where `sync.OnceValue` would be correct and shorter. The
hand-rolled version often hides a visibility or re-init bug.

Fix: wrap the initializer in `sync.OnceValue`/`OnceValues` and call the returned
function; it caches the result (and the error) and runs the body at most once.

### Assuming AfterFunc waited for the cleanup

Wrong: cancelling a context and immediately asserting the registered cleanup ran.
`context.AfterFunc` does not block until the literal completes, so the assertion
flakes.

Fix: synchronize explicitly — have the cleanup literal signal a channel or set an
atomic, and wait on that before asserting.

### Ignoring the stop func from AfterFunc

Wrong: discarding the `stop` return, so the cleanup runs when the context is
cancelled *and* again on the normal completion path (double cleanup), or the
registration leaks.

Fix: keep `stop` and call it on the success path; guard the cleanup with a
`sync.Once` or an atomic so it runs exactly once regardless of which path wins.

### Unpartitioned writes from errgroup tasks

Wrong: each `errgroup` task literal does `results = append(results, v)` on a shared
slice. errgroup only synchronizes around `Wait`, so this is a race.

Fix: preallocate the results slice and have each task write only `results[i]`, its
own index.

### A comparator that never returns 0 or is not antisymmetric

Wrong: a `slices.SortFunc` comparator that returns a `bool` cast, or only `-1`/`1`
and never `0`, or that is not antisymmetric. Equal elements then sort
nondeterministically and any stability assumption breaks.

Fix: return a true three-way int with `cmp.Compare`/`cmp.Or`, returning 0 for
equal keys; switch to `SortStableFunc` when input order must be preserved among
equals.

### Reaching for an IIFE that hides a temporary in a wide scope

Wrong: using an IIFE where a plain assignment is clearer, or the reverse —
declaring a builder local at package or function scope where it leaks far past its
one use.

Fix: use an IIFE only to scope one-shot locals or to produce a value with local
error handling; otherwise assign directly.

### Launching unbounded goroutines

Wrong: one `go func(){...}()` per job with no semaphore and no `SetLimit`, so a
burst of work spawns tens of thousands of goroutines and exhausts memory or
downstream connections.

Fix: bound concurrency — a buffered-channel semaphore in a hand-rolled pool, or
`errgroup`'s `SetLimit`.

Next: [01-worker-pool-goroutine-literals.md](01-worker-pool-goroutine-literals.md)
