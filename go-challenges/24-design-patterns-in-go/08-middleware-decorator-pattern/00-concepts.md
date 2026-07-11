# Middleware / Decorator Pattern — Concepts

Logging, metrics, caching, authentication, rate limiting, retries, and timeouts are cross-cutting concerns: they apply to many operations but belong to none of them. Hand-writing them inside every method or every HTTP handler produces duplication that drifts out of sync the moment one copy is fixed and the others are forgotten. The decorator pattern removes the duplication by wrapping a value with another value of the same type that adds behavior before and after delegating to the one it wraps. In Go this is not an exotic technique reserved for object-oriented frameworks; it is the everyday shape of `net/http` middleware, of instrumented repositories, and of generic helpers that add a retry or a timeout around any function. This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which build the three Go incarnations of the pattern as independent, self-contained modules.

## The one idea: wrap the type, keep the type

A decorator is a value that has the same type as the thing it decorates, holds that thing as a field, and forwards calls to it while adding behavior around the forwarding. The decisive property is that the decorator and the decorated are interchangeable: a caller that accepts the type cannot tell, and must not need to tell, whether it holds the bare implementation or one wrapped five layers deep. That interchangeability is what lets you add a concern without touching a single caller and without touching the implementation being wrapped. It is also what lets you stack concerns, because the wrapper of a wrapper is still the same type.

"The same type" is the whole discipline. If the wrapper stored a concrete struct instead of the interface, it could decorate exactly one implementation and nothing else, and a second decorator could not wrap the first. The pattern only composes when every layer is written against the abstraction. In Go the abstraction is usually an interface (a repository, a `http.Handler`) or a function type (`func(context.Context) (T, error)`). Functions are first-class values, so a function that takes a function and returns a function of the same signature is a decorator just as much as a struct that implements an interface is.

### Three Go incarnations of the same pattern

The first incarnation is the interface decorator. You define a domain interface, you write a real implementation, and then you write wrappers that each take the interface, store it, implement the interface themselves, and add one concern. A `LoggingRepository` logs each call and forwards it; a `CachingRepository` checks a map before forwarding; a `MetricsRepository` records counts and durations around the forwarding. Each is the interface, so each can wrap any other, and you assemble a stack by nesting constructors.

The second incarnation is HTTP middleware, which is the decorator pattern applied to the single-method `http.Handler` interface. A middleware is conventionally `func(http.Handler) http.Handler`: it takes the next handler, returns a new handler that does something before and after calling the next one. Authentication, request-ID injection, structured logging, panic recovery, and gzip compression are all middleware. They compose into a chain, and the chain is itself a `http.Handler`, so it slots into any router unchanged.

The third incarnation is the function decorator, which uses generics so that one wrapper works for every return type. A `WithRetry[T]` takes a `func(context.Context) (T, error)` and returns a function of the same shape that calls the original repeatedly until it succeeds or runs out of attempts. A `WithTimeout[T]` returns a function that bounds how long the original may run. Because both keep the signature, they compose: `WithRetry(WithTimeout(op, d), n, isRetryable)` retries an operation, each attempt bounded by its own timeout.

### Composition order is read inside-out

When you write `Logging(Metrics(Caching(Real)))`, a call enters `Logging` first because it is the outermost wrapper; `Logging` delegates to `Metrics`, which delegates to `Caching`, which finally calls `Real`. The outermost layer runs first on the way in and last on the way out; the innermost is the actual work. For HTTP middleware the same nesting holds: the first middleware in a chain is the outermost, so it sees the request first and the response last. This is why order is a behavioral decision, not a cosmetic one. Recovery must be outside logging if you want a panic to be logged rather than to crash the logger; authentication must be outside the business handler so an unauthenticated request never reaches it; a request-ID middleware must run before logging so the log line can include the ID. Get the order wrong and each layer still works in isolation while the whole behaves incorrectly.

The mirror-image structure of a decorator method makes the ordering concrete. Everything written before the delegating call runs on the way in, in outer-to-inner order; everything after it runs on the way out, in inner-to-outer order. A middleware that appends a label before calling `next` and another label after will, when three are chained, produce `A-in, B-in, C-in, handler, C-out, B-out, A-out`. That trace is the pattern's signature, and an exercise asserts exactly it.

### Cross-cutting concerns and what each layer must respect

A decorator earns its place by handling its concern correctly even though it knows nothing about the operation it wraps. Three recurring concerns set traps worth naming up front.

Metrics must count every call, not only the successful ones. The whole reason to instrument a layer is to see failures; incrementing the counter only inside the `err == nil` branch produces a dashboard that shows healthy traffic while users get errors. Record the count and the latency unconditionally, around the delegation, regardless of outcome.

Caching must store successes and never errors. A cache that remembers an error turns a transient failure into a permanent one: every later lookup returns the stale error until something invalidates the entry. Populate the cache only when the wrapped call returns `nil` error. Because a cache is read by many goroutines and written by some, it needs a `sync.RWMutex`: reads take the read lock so they proceed concurrently, writes take the exclusive lock. Without the lock, `go test -race` flags concurrent map access, and concurrent misses can clobber each other's writes.

Retry must distinguish transient failures from permanent ones. Retrying a not-found or a validation error is pointless work that amplifies load for a problem the next call cannot fix; retrying a permanent error can even mask it. The seam is a predicate: classify errors with a sentinel such as `ErrTransient` matched by `errors.Is`, retry only those, and let everything else propagate immediately. When retries are exhausted, wrap the last error with `%w` so callers can still inspect the underlying cause. A retry decorator must also honor cancellation: if the context is already done between attempts, stop and return the context error rather than burning the remaining attempts.

### Context propagation and timeouts

Every decorator on a request path takes a `context.Context` and must thread it through to the next layer unchanged, or wrap it deliberately. HTTP middleware that wants to attach a request ID does so with `context.WithValue` and passes a shallow-copied request via `r.WithContext(ctx)` so downstream handlers and log lines can read the value. A timeout decorator derives a child context with `context.WithTimeout`, runs the wrapped operation, and races the operation's completion against the context's `Done` channel. The subtle part is not leaking the goroutine that runs the operation: give the result channel a buffer of one so the operation's goroutine can always send and exit even after the decorator has already returned on the timeout branch. The wrapped operation, for its part, must select on `ctx.Done()` so the timeout actually stops the work rather than merely abandoning a goroutine that runs to completion in the background.

### Why narrow accessors instead of exported fields

A decorator measures things its callers may want to read: the cache exposes its size and hit count, the metrics layer exposes its counters, the retry layer exposes how many attempts an operation took. Exposing these through small read-only methods (`Hits()`, `Counts()`, `Attempts()`) rather than exported fields keeps the internal maps and counters unexported, so the locking discipline that protects them cannot be bypassed from outside and the validation a constructor performed cannot be undone later. Accessors return copies of internal maps so a caller iterating the result cannot race the decorator still mutating the original.

## Common Mistakes

### Wrapping a concrete type instead of the interface

Declaring the field as `next *MemoryRepository` instead of `next UserRepository` collapses the whole pattern: the wrapper can now decorate exactly that one concrete type, a second decorator cannot wrap the first, and the abstraction the callers depend on is gone. Always store the interface (or the function type). The decorator works on the abstraction, never on a concrete implementation.

### Counting or logging only on success

Putting the counter increment, the latency record, or the log line inside the `if err == nil` branch hides every failure from observability. The dashboard looks healthy while users see errors. Instrument unconditionally, around the delegating call, and include the error in the record.

### Retrying every error

A blanket `if err != nil { retry }` retries not-found, validation failures, and duplicates: errors the next attempt cannot possibly fix. It amplifies load and adds latency for nothing. Retry only transient errors identified by a sentinel and matched with `errors.Is`; let permanent errors propagate on the first attempt.

### Caching errors

Storing the error alongside successes turns a one-off transient failure into a sticky one: every subsequent lookup returns the cached error until the entry is invalidated, which hides a recoverable problem. Cache only successful results.

### Getting the chain order wrong

Each middleware compiles and works alone, so an order bug is invisible until behavior is wrong: recovery placed inside logging never catches a panic that unwinds through the logger; authentication placed inside the business handler lets unauthenticated requests through; logging placed before request-ID injection logs an empty ID. Order is behavior. Decide it deliberately and assert it with an ordering test.

### Leaking the goroutine in a timeout decorator

Running the wrapped operation in a goroutine that sends its result on an unbuffered channel leaks that goroutine forever whenever the timeout fires first: the decorator has returned, nothing will ever receive, and the goroutine blocks on the send. Buffer the channel with one slot so the send always succeeds, and have the wrapped operation select on `ctx.Done()` so the timeout actually cancels the work.

---

Next: [01-instrumented-repository.md](01-instrumented-repository.md)
