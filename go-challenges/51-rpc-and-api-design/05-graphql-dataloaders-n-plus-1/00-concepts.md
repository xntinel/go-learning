# Solving N+1 with GraphQL Dataloaders — Concepts

N+1 is the single most common way a correct GraphQL server melts a production
database. The query is valid, every resolver is individually correct, the tests
pass on small fixtures, and then a list query on real data fires hundreds of
single-row lookups and the datastore falls over. The dataloader is the standard
mitigation, but a senior engineer treats it as a concurrency primitive with sharp
edges, not a magic cache. This file is the conceptual foundation for the three
exercises that follow: first you build a request-scoped batch loader from scratch
so the mechanism is not a black box, then you wire the production library
(`dataloadgen`), then you scope it correctly per request inside a gqlgen server.

## Concepts

### What N+1 is, precisely, in GraphQL

A GraphQL executor resolves a response field by field. For a query like
`todos { title user { name } }`, the executor first runs the `todos` resolver
once and gets a list of N todos. Then, for the nested `user` field, it runs the
`Todo.user` resolver **once per todo** — N times — because a resolver is defined
per field on a per-parent-value basis. If each `Todo.user` call issues
`SELECT * FROM users WHERE id = ?`, the single client query becomes 1 (todos) + N
(users) database round-trips. That is the N+1 problem, and it is structural to the
resolver-per-field execution model, not a bug in any one resolver. You cannot fix
it by writing the `Todo.user` resolver more carefully; the fix has to change how
the N sibling calls talk to the datastore.

### The batching mechanism

A dataloader sits between the resolver and the datastore. Instead of fetching
immediately, `Load(ctx, key)` records the key and returns a promise (a *thunk*).
The loader buffers keys for a short time window — or until a maximum batch size —
and then issues exactly **one** batch fetch for all buffered keys, fanning the
results back out to each waiting caller. The N calls to `Todo.user` become N calls
to `Load`, which collapse into one `SELECT * FROM users WHERE id IN (...)`. The
1+N round-trips become 2.

The window is what makes this work, and it works *because* of how the executor
runs. gqlgen resolves the elements of a list concurrently: the N `Todo.user`
resolvers run on N goroutines at roughly the same instant. They each call `Load`
within the same brief window, so the keys pile up in one batch before the window
closes. Without concurrent sibling resolution there would be nothing to batch —
the loader would see one key, wait, fetch it, then see the next. Concurrency is
the enabling condition, not an implementation detail.

### The fetch contract, and why it is fragile

The batch fetch has a strict contract that is easy to get subtly wrong. In the
positional form, `fetch(ctx, keys []K) ([]V, []error)` must return a `[]V` that is
the **same length** as `keys` and **aligned by position**: `values[i]` is the
result for `keys[i]`. A datastore query does not naturally honor this. `SELECT ...
WHERE id IN (1,2,3)` returns rows in whatever order the engine likes, and if id 2
does not exist it returns two rows, not three. If you hand those rows back in
row-order, every value shifts and each todo gets the wrong user — a silent data
corruption, not an error. So the positional fetch must re-order results to match
the input keys and fill a slot (usually the zero value plus an error) for every
missing key.

This fragility is exactly why the *mapped* loader exists.
`NewMappedLoader(fetch func(ctx, keys) (map[K]V, error), ...)` lets the fetch
return a `map[K]V`, and the loader looks up each key itself; any key absent from
the map automatically resolves to `ErrNotFound`. The mapping cannot shift, and
missing keys cannot corrupt siblings. Prefer the mapped form unless you have a
reason not to.

### Error granularity

The positional fetch returns `[]error` precisely so that each key can fail
independently: `errors[i]` is the error for `keys[i]`. Returning a single
whole-batch error (a `[]error` of length one, or a top-level error from the mapped
fetch) fails *every* sibling that shared the batch — one bad key takes down the
whole list. When errors are per-key, gqlgen surfaces them as partial errors: the
response carries the todos that resolved plus a per-field error for the ones that
did not. Deciding between per-key and whole-batch failure is a real design choice:
a transient connection error probably should fail the batch; a single missing row
should not.

### Request scoping is correctness and security, not optimization

The most important property of a dataloader is one people mistake for a
performance tuning knob: its cache. A dataloader's cache is a **per-request
identity cache**, not a TTL cache. Its entire job is to give one request a
consistent snapshot — load user 7 twice in the same query and you get the same
object without a second fetch. It is meant to be constructed fresh at the start of
each request and thrown away when the request ends.

Make it a package-level singleton shared across requests and you have introduced
three bugs at once. First, stale reads: request B sees the rows request A loaded
minutes ago, with no TTL to evict them. Second, and far worse, **cross-tenant data
leakage**: if user Alice's request loads user 7 and Bob's later request reads user
7 from the same cache, Bob may receive data his authorization never permitted — an
access-control failure dressed up as a cache hit. Third, unbounded memory growth:
a cache that never resets accumulates every key ever loaded. Request scoping is
therefore a correctness and security requirement. The mechanics — an HTTP
middleware that builds a fresh loader bundle, stashes it in the request context
under an unexported key, and lets it fall out of scope when the request returns —
are the subject of Exercise 3.

### The latency/throughput trade of the wait window

`WithWait(d)` sets how long the loader buffers keys before dispatching. A larger
window batches more aggressively — more sibling keys accumulate, fewer queries hit
the datastore — but it adds a fixed latency floor to *every* request that uses the
loader, because even a request that only needs one key waits out the window. Too
small and batching degrades back toward N+1, because the window closes before the
siblings have registered. The default is small on purpose (single-digit
milliseconds). The right lever for protecting the datastore from a huge fan-out is
not a longer wait but `WithBatchCapacity(n)`, which caps how many keys go into one
fetch and splits the rest into further batches. Lengthen the wait only if
profiling shows siblings are missing the window.

### Single-flight within a request

Because the cache is keyed by identity, requesting the same key many times in one
request collapses to a single fetch and a single shared result. A query that
references user 7 from ten different todos costs one user lookup, not ten. This
"single-flight" behavior falls out of the cache automatically; it is why the cache
is not optional decoration but part of the batching contract.

### Where the real work goes

The dataloader is a coordination layer, not a query optimizer. It turns N calls
into one call — but that one call still has to be an efficient
`IN (...)`/`JOIN` at the batch boundary. If your batch fetch loops over the keys
and issues one query each, you have moved the N+1 inside the loader and gained
nothing but latency. The batch function is exactly where you write the good
datastore access. The loader's contribution is coordination: collecting the keys
so that a single efficient query is *possible*.

### Failure modes to internalize

Four failures recur. **Deadlock by self-reference**: a batch fetch that calls the
same loader (directly or transitively) can never complete — the batch is waiting
to fill, and the thing that would fill it is waiting on the batch. Fetch straight
from the datastore inside the batch function; never recurse through the loader.
**Loader reuse across requests**: covered above; the cache must not outlive the
request. **Stale-after-mutation**: if a request mutates a row and then reads it
back through the loader in the same request, the cached pre-mutation value comes
back; `Clear(key)` or `Prime(key, newValue)` after the write, or use
`WithoutCache()` on mutation-heavy paths. **Ignored cancellation**: a loader that
does not propagate `ctx.Done()` keeps running its batch after the client has hung
up, wasting a datastore round-trip on a dead request.

### When a dataloader is the wrong tool

Dataloaders shine when the schema graph is traversed field-by-field and the same
entity is fetched repeatedly across siblings. They are not the only answer to
N+1. A root-level query that plans a single `JOIN` (or a purpose-built read model
in a CQRS design) can eliminate the nested fetch entirely, with no per-request
loader machinery. `@defer`/`@stream` change *when* fields resolve rather than how
they batch. Persisted queries reduce parsing and transport cost, not fetch fan-out.
Reach for a dataloader when the traversal is genuinely field-by-field and a single
planned query is impractical; reach for query planning when the shape is known and
fixed.

## Common Mistakes

### A package-level or app-lifetime loader shared by all requests

Wrong: constructing the loader once at startup and sharing it. The cache then
outlives the request, serving stale rows, leaking data across tenants/users, and
growing without bound. Fix: build the loader bundle per request in middleware and
discard it when the request returns. The cache must never outlive one request.

### A batch fetch that returns rows out of order or drops missing keys

Wrong: returning the datastore's rows directly from a positional fetch, so
`values[i]` no longer corresponds to `keys[i]` once a row is missing or reordered.
Fix: re-order results to match the input keys and fill a slot for every missing
key, or use `NewMappedLoader` so missing keys become `ErrNotFound` and the mapping
cannot shift.

### Failing the whole batch when one key failed

Wrong: returning a single error for the whole batch when only one key could not be
loaded, which fails every sibling that shared the batch. Fix: return a per-key
`[]error` (or a `MappedFetchError[K]`) so only the failing field errors and the
rest of the list still resolves.

### Awaiting each Load before issuing the next

Wrong: calling `Load` and immediately blocking on its result inside a loop, one
key at a time. Nothing accumulates in the window, so batching never happens and you
are back to N+1. Fix: let the concurrent sibling resolvers each call `Load`, or
collect the keys and use `LoadAll`/thunks so they batch inside one window.

### A batch fetch that recurses through its own loader

Wrong: a batch function that calls the same loader (or waits on something that
does). The batch can never fill and the request deadlocks. Fix: fetch directly
from the datastore inside the batch function; never route the batch fetch back
through the loader.

### Reading a stale value through the loader after a mutation

Wrong: mutating a row and then reading it back through the loader in the same
request, getting the cached pre-mutation value. Fix: `Clear(key)` or
`Prime(key, newValue)` after the write, or use `WithoutCache()` on mutation paths.

### Cranking WithWait up "to batch more"

Wrong: setting the wait to tens of milliseconds to force bigger batches, adding
that latency to every request. Fix: keep the window small (single-digit
milliseconds, tuned to your concurrency) and cap fan-out with
`WithBatchCapacity` rather than lengthening the wait.

### An exported or string context key for the loader bundle

Wrong: stashing the bundle in the context under a string or an exported key,
risking collisions with other packages. Fix: use an unexported key type
(`type ctxKey struct{}`) and expose a `For(ctx) *Loaders` accessor.

Next: [01-batch-loader-from-scratch.md](01-batch-loader-from-scratch.md)
