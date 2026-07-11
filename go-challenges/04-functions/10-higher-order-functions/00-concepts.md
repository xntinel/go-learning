# Higher-Order Functions: Combinators, Decorators, and Policy Composition — Concepts

A higher-order function takes a function as an argument, returns a function, or
both. That one sentence understates how central the idea is to production Go.
Every resilience policy you ship — retry, timeout, circuit breaker, rate limit,
bulkhead — is a function that wraps another function. Every HTTP middleware stack
is a list of `http.Handler`-to-`http.Handler` transforms folded together. Every
read-through cache, every pluggable sort order, every accumulate-all validation
pipeline, every lazy resource loader is built the same way: behavior passed in,
behavior returned. The senior skill is not writing one decorator; it is choosing
composition order deliberately, keeping the policy generic while pushing domain
decisions out to injected predicates, respecting context cancellation on every
wait, and making the whole thing deterministically testable by injecting the
clock and the randomness. This file is the conceptual foundation for the nine
independent exercises that follow; read it once and each exercise becomes an
application of the same handful of ideas.

## Concepts

### Three recurring shapes

In backend Go, higher-order functions show up as three shapes, and naming them
makes the code legible.

The *decorator* has type `T -> T`: it takes a thing and returns the same kind of
thing with extra behavior wrapped around it. `Operation -> Operation`,
`http.Handler -> http.Handler`, `func(int) Duration -> func(int) Duration`. A
decorator composes with itself because its output is another valid input.

The *factory* has type `config -> behavior`: it takes parameters and returns a
function closed over them. `WithBackoff(base)` returns a `func(int) time.Duration`;
`NonEmpty(field, get)` returns a validation rule. The returned function carries the
configuration in its closure, so the call site holds a ready-to-use behavior with
no config threading.

The *strategy* (or callback) injects behavior into an algorithm: `slices.SortFunc`
takes a `cmp` function, `errors.Is` compares, a dispatcher looks up a `Handler`.
The algorithm is fixed; the pluggable part is a first-class value.

Most real code mixes all three. `WithRetry(op, 3, isRetryable)` is a factory (it
takes a count and a predicate) that returns a decorator (an `Operation`) whose
retryability strategy is an injected predicate.

### Composition order is a product decision, not cosmetics

`WithTimeout(WithRetry(op, 3, pred), d)` and `WithRetry(WithTimeout(op, d), 3, pred)`
are both valid and they are *different products*. The first puts one deadline
around the whole retry loop: three attempts must collectively finish within `d`,
and once `d` elapses no further attempt runs. The second gives each attempt its
own fresh deadline `d`, so the worst case is roughly `3*d` plus backoff. Which one
is correct depends on whether your SLA is per-operation ("this request must
complete within 300ms no matter how many retries") or per-call ("each attempt gets
300ms, retry up to three times"). Neither is more correct in the abstract. The
senior discipline is to always be able to state which one you built and why, and
to write a test that pins the elapsed-time behavior so a refactor cannot silently
flip it.

### Take the dependencies, return the behavior

The correct decorator signature parameterizes the policy. A `WithRetry` that
always retries three times, a `WithTimeout` that hardcodes 200ms, a backoff that
bakes in its base — each destroys reuse and, worse, testability, because the test
is forced to wait real wall-clock time it cannot shrink. Take the count, the
duration, the base as arguments; provide a default constant only at the call site,
never inside the wrapper. A policy that cannot be tuned per call site is a policy
you will copy-paste and fork.

### Retryability is a domain decision, injected as a predicate

The single most damaging retry bug is "retry on any non-nil error". Authentication
failures, validation errors, and 4xx responses are permanent: retrying them cannot
succeed, it only wastes the retry budget and amplifies load on a dependency that
is already telling you to stop. Whether an error is worth retrying is a *domain*
decision — the transport layer does not know that a 409 conflict is permanent but
a 503 is transient. Encode it as an injected `func(error) bool`, or classify with
sentinel errors checked via `errors.Is`/`errors.As`. The policy stays generic; the
caller supplies the knowledge.

### Every wait inside a decorator must select on ctx.Done()

A retry that sleeps between attempts with a bare `time.After(d)` will still block
for the full `d` after its context is cancelled. That turns a cancelled request
into wasted work and a shutdown into a stall. Every blocking wait inside a
decorator must be a `select` that also watches `ctx.Done()`:

```go
select {
case <-ctx.Done():
	return ctx.Err()
case <-time.After(delay):
}
```

This is the most common correctness bug in hand-rolled retry loops, and the reason
the retry exercise asserts context cancellation with an elapsed-time bound rather
than just "an error came back".

### errors.Join is the idiom for accumulate-all validation

At an API boundary, fail-fast validation is the wrong default: it makes the client
fix one field, resubmit, discover the next problem, resubmit again. `errors.Join`
(Go 1.20+) lets a validation pipeline run every rule and return a single error that
wraps all the failures at once; the caller pulls individual causes out with
`errors.Is`/`errors.As`. The pipeline is a fold: `Validate(v, rules...)` applies
each `Rule[T]` and joins the non-nil results. Fail-fast is still right deeper in
the stack where a first failure makes later work meaningless; at the boundary,
report everything.

### sync.OnceValue / OnceValues are higher-order lazy builders

The classic lazy singleton — a package global guarded by a nil check that assigns
on first use — is a data race the moment two goroutines hit it together, and the
race detector will find it. `sync.OnceValue` and `sync.OnceValues` (Go 1.21+) are
higher-order functions that take a builder and return a memoized accessor: the
builder runs at most once even under concurrent first access, and every caller
gets the identical result. They are the modern, race-free replacement for both the
nil-check singleton and eager `init()` side effects, and they let you defer an
expensive build (a parsed config, a connection pool) until first use. A panic in
the builder is memoized too and re-raised on every subsequent call — deliberate,
so a failed initialization does not silently look successful later.

### singleflight collapses a stampede; never cache the error

When many callers ask for the same expensive key at the same moment — a cold cache
miss on a hot config value under load — you want exactly one underlying call, with
all callers sharing its result. `golang.org/x/sync/singleflight` does exactly that:
`Group.Do(key, fn)` runs `fn` once per in-flight key and returns `(v, err, shared)`
to every waiting caller. It is the correct companion to a cache for read-through of
a slow dependency. The trap is caching the *error*: a memoize decorator that stores
a transient failure poisons the key so every future caller gets the stale error.
Cache only successes; let failures re-run.

### Comparators compose with cmp.Or

A multi-key sort — order by status ascending, then amount descending, then
createdAt ascending — is a fold of single-key comparators, not a nested if-ladder.
`cmp.Or` (Go 1.22+) returns its first non-zero argument, which is exactly
tie-break semantics: evaluate the primary comparator; if it is zero (a tie), fall
through to the secondary; and so on. `slices.SortFunc` and `slices.SortStableFunc`
take the composed `func(a, b T) int` directly. Descending is a comparator wrapped
to negate its result. Expressed this way the ordering reads top to bottom and the
tie-break order is impossible to get subtly wrong.

### Generic Map/Filter/Reduce are a tool, not a religion

Generic `Map`, `Filter`, and `Reduce` are genuinely useful for row-to-DTO
transforms and aggregates. They are also frequently the wrong call: a plain
`for range` loop is often clearer, allocation-free, and easier to debug, and the
standard library already ships `slices.DeleteFunc`, `slices.IndexFunc`,
`slices.Collect`, and the `iter.Seq` machinery that cover many cases. Reach for the
combinator when it removes a real branch or a real intermediate slice, not to make
the code look functional. And never mutate the source slice inside a `Map`/`Filter`
helper — return a fresh slice, or you will corrupt the caller's data.

### Inject the source of nondeterminism to make decorators testable

Full-jitter backoff needs real randomness in production to break up synchronized
retries and prevent a thundering herd. But a test that asserts an exact delay while
the code reads a package-global `rand` is flaky by construction. The fix is to
inject the source: pass a seeded `*math/rand/v2.Rand` (built from `rand.NewPCG`) so
the sequence is reproducible in tests and random in production. The same discipline
applies to the clock: injecting time (or virtualizing it) is what lets you assert a
timeout fires without sleeping real seconds. The nondeterministic dependency is a
parameter, not a global.

### Closures capture by reference

A decorator builds state in a closure, and the returned function may be called
concurrently. Be deliberate about what is shared across all invocations of the
returned function versus what is local to a single invocation. A counter declared
outside the returned function is shared (and needs synchronization if the function
runs concurrently); a variable declared inside it is per-call. Getting this wrong
produces either a data race or a "counter that resets when it should not". Go 1.22
fixed the classic loop-variable capture bug, but the broader question — is this
state per-decorator or per-call — is still yours to answer.

## Common Mistakes

### Hardcoding the policy inside the decorator

Wrong: `WithRetry` that always loops three times, or a wrapper with a baked-in
timeout. The call site cannot tune it for a slow versus a fast dependency, and the
test cannot shrink the wait. Fix: take the count, the duration, the backoff as
parameters; default only at the call site.

### Retrying on any non-nil error

Wrong: `if err != nil { retry }`. Permanent errors — auth, validation, 400/404 —
get retried, burning the budget and amplifying load on a struggling dependency.
Fix: inject a `func(error) bool` predicate, or classify with `errors.Is`/`As`.

### Sleeping between retries without watching the context

Wrong: `time.After(backoff)` with no `select` on `ctx.Done()`. A cancelled context
still blocks for the full delay. Fix: `select { case <-ctx.Done(): return ctx.Err();
case <-time.After(backoff): }`.

### Getting composition order wrong without noticing the semantic change

Wrong: swapping `WithTimeout(WithRetry(...))` for `WithRetry(WithTimeout(...))` and
assuming they are equivalent. They are a per-operation deadline and a per-attempt
deadline — different products. Fix: decide deliberately and pin the elapsed-time
behavior with a test.

### Dropping the cause from the final error

Wrong: returning `errors.New("all attempts failed")`, discarding the underlying
error. Callers' `errors.Is`/`As` and your metrics can no longer classify it. Fix:
wrap with `%w`, or `errors.Join` the attempts.

### A nil-check lazy singleton under concurrency

Wrong: `if instance == nil { instance = build() }` on a package global, read and
written from multiple goroutines. That is a data race the `-race` detector flags.
Fix: `sync.OnceValue`/`OnceValues`.

### Caching errors in a memoize/singleflight decorator

Wrong: storing whatever `fn` returned, error included, so a transient failure is
pinned and every future caller gets the stale error. Fix: cache only on `err == nil`;
let failures re-run.

### Using a package-global rand for jitter and asserting the exact delay

Wrong: `rand.Int64N` from the global source inside `FullJitter`, then a test that
checks the precise delay. Flaky. Fix: inject a seeded `*rand.Rand`
(`rand.New(rand.NewPCG(seed1, seed2))`) so the sequence is deterministic.

### Expressing a multi-key sort as a nested if-ladder

Wrong: `if a.Status != b.Status { ... } else if a.Amount != b.Amount { ... } else { ... }`
inside the `cmp` func — unreadable and easy to get the tie-break order wrong. Fix:
fold single-key comparators with `cmp.Or`.

### Double WriteHeader in status-capturing middleware

Wrong: a middleware writes a status code and then lets the inner handler also call
`WriteHeader`, producing "superfluous WriteHeader" and a wrong recorded status.
Fix: the wrapping `ResponseWriter` must record the first status and guard against a
second `WriteHeader`.

### Reaching for Map/Filter/Reduce where a loop is clearer

Wrong: a chain of generic combinators building three intermediate slices where one
`for range` would do, or hand-reimplementing `slices.DeleteFunc`. Fix: use the loop
or the stdlib helper; reserve the combinator for when it removes a real branch.

### Mutating the source slice inside Map/Filter

Wrong: returning the same backing array (or writing into the input) so the caller's
data changes unexpectedly. Fix: allocate a fresh result slice.

Next: [01-retry-pipeline-combinators.md](01-retry-pipeline-combinators.md)
