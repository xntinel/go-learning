# Function Types and Callbacks: First-Class Functions in Production Backends — Concepts

To a beginner, "functions are first-class values" is a syntax fact. To a senior
backend engineer it is the load-bearing seam of every serious Go service. The
`net/http` middleware chain that wraps your handlers, the functional-options
constructor that keeps your client library backward-compatible across releases,
the event router that maps a kind string to a handler without a thousand-line
type switch, the retry engine parameterized by an operation and a "is this
retryable" classifier, the validation pipeline assembled from small rules, the
exactly-once lazy initializer for a DB pool, the streaming transform over an
`iter.Seq`, the LIFO shutdown registry that closes your resources in the right
order — all of them are function types and callbacks. This file is the model you
need before the ten exercises; read it once and every module reads as a variation
on one idea.

## Concepts

### A function type is a named signature, and its values are first-class

A function type names a signature:

```go
type Less[T any] func(a, b T) int
type Middleware func(http.Handler) http.Handler
type Option func(*Config)
type CleanupFunc func(ctx context.Context) error
```

A value of a function type is first-class: you pass it as an argument, return it
from a function, store it in a struct field, put it in a map, and range over a
slice of them. The one operation you cannot do is compare two function values for
equality — the spec permits comparing a function value only against `nil`.
`f == g` does not compile; `f == nil` does. That single restriction is why a
"deregister this exact handler" API is awkward in Go and why registries key on a
name or an integer handle rather than on the function value itself.

The name matters as much as the signature. `func(http.Handler) http.Handler`
appearing raw in five signatures tells a reader nothing; `Middleware` tells them
everything. Named function types are the vocabulary of Go plumbing: `Middleware`,
`HandlerFunc`, `Option`, `Rule`, `Predicate`, `Operation`, `Retryable`,
`CleanupFunc` each encode intent in a type name so a signature reads as a domain
contract instead of an anonymous tangle of `func(...)`.

### A function type can have methods, and that is the adapter pattern

This is the surprise that unlocks half of idiomatic Go. A named function type is a
type, and a type can have methods. So a function type can *satisfy an interface*:

```go
type HandlerFunc func(http.ResponseWriter, *http.Request)

func (f HandlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f(w, r) // the method just calls the receiver
}
```

`http.HandlerFunc` satisfies `http.Handler` because it declares `ServeHTTP`, whose
body invokes the receiver. The payoff: any bare function of the right shape becomes
an interface value with zero struct boilerplate — `http.HandlerFunc(myFunc)` is an
`http.Handler`. You apply the same trick to your own domain ports: given
`interface JobProcessor { Process(ctx, Job) error }`, a `ProcessorFunc` adapter
lets a plain function stand in for the interface, and you assert satisfaction at
compile time with `var _ JobProcessor = ProcessorFunc(nil)`.

### Callbacks come in fixed conventional shapes

Go's standard library settled on conventions, and fighting them costs you.

- A comparator returns `int`, not `bool`: negative if `a < b`, zero if equal,
  positive if `a > b`. `slices.SortFunc`, `slices.BinarySearchFunc`, and
  `cmp.Compare` all speak this three-valued language. A `bool` "less" function is
  the old `sort.Slice` world and does not fit the new value-based APIs.
- An iterator (Go 1.23 range-over-func) is `func(yield func(V) bool)`. It calls
  `yield` once per element and *must stop* the moment `yield` returns `false` —
  that is how a consumer's `break` propagates upstream and cancels the producer.
- An operation that can block or do I/O takes a `context.Context` as its first
  parameter, so the caller owns cancellation and deadlines. Any callback you write
  that sleeps, retries, or performs I/O should follow the ctx-first convention.

These are not stylistic preferences; they are the contracts the stdlib and every
reader assume.

### Higher-order functions are the composition primitive

A higher-order function takes or returns a function. That is the whole mechanism
behind composition in a backend:

- Middleware is `func(http.Handler) http.Handler` — a function that wraps a
  handler in another handler. `Chain(mw...)` folds a list of them into one.
- A decorator wraps a `ProcessorFunc` in another `ProcessorFunc` that adds metrics
  or retry, then returns it — same interface in, same interface out.
- `Memoize(fn) func(K)(V,error)` returns a caching version of `fn`.
- Functional options are functions that mutate a `*Config`; `New` applies them.

Every one of these is "wrap one callable in another and return the result." Once
you see it, the middleware chain and the memoizer are the same shape.

### Closures capture the variable, not a snapshot

A closure captures a variable by reference: it closes over the *variable itself*,
not its value at capture time. If the variable changes after the closure is
created, the closure sees the new value. This is the source of the classic
loop-capture bug — and Go 1.22 changed the rules underneath it. Since Go 1.22 the
`for` loop variable is a fresh variable per iteration, so

```go
for _, v := range items {
	go func() { use(v) }() // Go 1.22+: each goroutine sees its own v
}
```

is now correct without the old `v := v` shadow. But this only fixes the loop
variable. Two closures that capture the *same* shared mutable variable (a counter,
a map, a slice you append to) still race if they run concurrently — per-iteration
loop variables do not make shared state thread-safe. A memoizer's cache map, a
shutdown registry's slice, a router's handler map all need a mutex regardless of
Go version.

### Functional options: a backward-compatible, self-documenting constructor

The alternative to a giant `Config` struct literal at every call site is
variadic functional options:

```go
func New(opts ...Option) (*Server, error) {
	c := defaultConfig() // sane defaults FIRST
	for _, opt := range opts {
		opt(&c) // options mutate
	}
	if err := c.validate(); err != nil { // validate ONCE, at the end
		return nil, err
	}
	return &Server{cfg: c}, nil
}
```

The ordering is the whole design: defaults are set before any option runs, options
apply in call order (so two options touching the same field are last-writer-wins),
and the assembled config is validated exactly once at the end. Adding a new
`WithX` option next year breaks no existing call site — that backward compatibility
is why every mature Go client library (gRPC, the AWS SDK, database drivers) uses
this pattern instead of exported config structs.

### Dispatch tables replace long type switches with data

A `map[string]HandlerFunc` turns a growing `switch kind { case ... }` into data.
Registration (`Register(kind, h)`) is decoupled from invocation (`Dispatch(kind)`),
new handlers are added without editing a central switch, and a plugin can register
its own kind. The trade-offs you must decide and document: duplicate registration
(reject, or last-writer-wins?), an unknown kind (sentinel error, or a default
handler?), and multiple subscribers per kind (a slice of callbacks whose errors you
aggregate with `errors.Join`). A `nil` handler must be rejected at registration,
not left to panic at dispatch time.

### cmp.Compare and slices.*Func are the modern value-based callbacks

The `sort.Slice(s, func(i, j int) bool)` API is index-based: your comparator
receives indices and reaches back into the slice, which makes it easy to compare
the wrong elements and offers no compile-time help. The modern APIs —
`slices.SortFunc`, `slices.BinarySearchFunc`, `slices.IsSorted` — are value-based:
the comparator receives the *values* and returns an `int`. `slices.SortFunc` is
documented as not guaranteeing stability, but the standard library additionally
offers `slices.SortStableFunc`; either way, if a caller depends on equal elements
keeping input order, pin it with a test rather than assuming it. Crucially,
`cmp.Compare` defines a *total* order including `NaN` (it treats `NaN` as less than
any non-NaN and equal to itself for ordering purposes), so sorting `float64` with
`cmp.Compare` is deterministic where a hand-written `a < b` comparator, which
returns `false` for every comparison involving `NaN`, breaks strict-weak-ordering
and yields a partial, non-deterministic order.

### sync.OnceFunc / OnceValue / OnceValues: exactly-once lazy values

An expensive resource — a parsed config, a DB pool, a compiled template set —
should initialize exactly once, lazily, even under concurrent first-callers.
`sync.OnceValue(f)` returns a function that runs `f` a single time and caches its
result; every later call returns the cached value without re-running `f`.
`sync.OnceValues` is the two-return `(T, error)` variant. Two subtleties define
correct use: the initializer runs at most once *even across many goroutines*
(that is the concurrency guarantee `sync.Once` provides), and a *panic* in the
initializer is memoized — the wrapper re-raises the same panic on every subsequent
call rather than retrying. That last point means `OnceValues` is not a retry
mechanism for a flaky initializer; if init can transiently fail, you handle that
inside `f`, not by hoping the next call re-runs it.

### Callbacks with side effects must define their error semantics

Any callback that owns a resource or performs a side effect forces a decision:
fail-fast or collect-all. A validation `First(rules...)` stops at the first
violation — cheap, right for a guard on a hot path. A validation `All(rules...)`
runs every rule and joins the violations with `errors.Join` — right for a signup
form where the user wants to see every field error at once. A shutdown registry
must run *every* cleanup hook even if an early one fails, aggregating errors with
`errors.Join`, because stopping at the first failure leaks the remaining resources.
`errors.Join` produces an error whose `Unwrap() []error` lets `errors.Is` match any
of the joined sentinels — that is how a caller asserts "all three of these
violations happened." Choosing fail-fast versus collect-all is a deliberate UX and
operational decision, not an implementation detail.

## Common Mistakes

### Returning bool from a comparator

Wrong: a `Less[T]` that returns `bool`. `slices.SortFunc`, `slices.BinarySearchFunc`,
and `cmp.Compare` all require the three-valued `int` (`-1/0/+1`) convention, and a
`bool` comparator simply does not fit their signatures.

Fix: return `int`; use `cmp.Compare(a, b)` for ordered types.

### Reaching for sort.Slice instead of slices.SortFunc

Wrong: `sort.Slice(s, func(i, j int) bool { return s[i].Age < s[j].Age })`. The
index-based form makes it easy to index the wrong slice or compare the wrong
elements, and offers no type safety on the element.

Fix: `slices.SortFunc(s, func(a, b Person) int { return cmp.Compare(a.Age, b.Age) })`
— value-based, and the comparator follows the standard convention.

### Sorting float64 with a hand-written a<b comparator

Wrong: `func(a, b float64) int { if a < b { return -1 }; if a > b { return 1 }; return 0 }`.
Every comparison involving `NaN` returns `false`, so `NaN` is reported equal to
everything, which violates strict-weak-ordering and produces a non-deterministic,
partial order.

Fix: `cmp.Compare(a, b)` defines a total order that places `NaN` deterministically,
so the sort is reproducible.

### Violating the yield contract in an iterator

Wrong: after `yield(v)` returns `false`, calling `yield` again, or ignoring its
`false` result and continuing to produce values. Both break the range-over-func
contract — the runtime panics on a second `yield` after `false`, and continuing
wastes work the consumer explicitly asked to stop.

Fix: check `yield`'s return and `return` from the iterator the instant it is
`false`. `if !yield(v) { return }` is the canonical shape.

### A giant config struct literal instead of options

Wrong: an exported `Config` struct that every caller fills with a literal; adding a
field forces (or silently changes) every call site, and required-versus-optional is
invisible.

Fix: variadic functional options — defaults first, options mutate, validate once.
New options are additive and break no existing caller.

### Registering nil callbacks

Wrong: `Register(kind, nil)` or a `nil` `Rule`/`CleanupFunc` accepted silently, then
a nil-pointer panic at dispatch or shutdown time, far from the buggy call.

Fix: reject `nil` at registration with a clear error, so the failure is immediate
and local.

### Caching error results in a memoizer

Wrong: a memoizer that stores whatever the underlying function returned, so a
transient failure is served from cache forever.

Fix: cache only successful results; on error, do not store, so the next call
retries. Concurrent first-callers for the same key need a mutex (or single-flight)
so the underlying function is not stampeded.

### Running cleanup hooks FIFO instead of LIFO

Wrong: running shutdown hooks in registration order, tearing down a dependency
(the DB pool) before its dependents (the workers still using it); or stopping at
the first hook error and leaking the rest.

Fix: run hooks in reverse of registration (LIFO, mirroring `defer`), and run all of
them even on error, joining failures with `errors.Join`.

### Composing middleware in the wrong order

Wrong: a chain where recovery or timeout ends up *inside* the handler instead of
wrapping it, so a panic escapes unrecovered; or middleware that mutates the shared
`*http.Request` fields instead of deriving a new request via `r.WithContext`.

Fix: define the chain so the first-listed middleware is outermost (recovery and
timeout belong near the outside), and pass data down through the request context,
never by mutating shared request state.

### Retry loops that ignore ctx.Done

Wrong: a retry that sleeps with `time.Sleep` between attempts and never watches
`ctx.Done()`, so a cancelled request keeps retrying and blocks graceful shutdown.

Fix: back off with a `time.NewTimer` and `select` on both the timer and
`ctx.Done()`, returning `ctx.Err()` the instant the context is cancelled.

Next: [01-comparator-sort-pipeline.md](01-comparator-sort-pipeline.md)
