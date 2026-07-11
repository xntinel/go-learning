# 5. Repository Pattern — Concepts

Business logic should not know whether its data lives in PostgreSQL, behind a REST API, or in a plain map. The repository pattern draws a single seam between the domain and persistence: a collection-shaped interface (`Create`, `GetByID`, `Update`, `Delete`, `List`) that the domain depends on, and an implementation that is chosen once, at the composition root. Every serious Go backend has something shaped like this even when the team calls it "store", "dao", or "gateway". This file is the conceptual foundation; read it once and you will have everything you need to reason through the exercises, which build the pattern as independent, self-contained Go modules: a contract-tested in-memory repository, a query layer built from composable specifications, and a pair of decorators that add caching and logging to any repository without touching its code.

## Concepts

### The Interface Is The Domain Contract, Not The Storage Mechanism

A repository interface is a collection: its methods read like the verbs of a map or a table, and nothing in the signature mentions SQL, HTTP, or any backend. That is the whole point. The domain layer imports the interface and the domain types; it never imports `database/sql`, a driver, or an HTTP client. A SQLite-backed implementation and an in-memory map implementation satisfy the same interface, pass the same test suite, and are interchangeable at the one place that constructs them.

This inversion is what makes the rest of the system testable. A service that depends on `UserRepository` can be unit-tested against the in-memory implementation in microseconds, with no database to spin up, because the interface is the only thing it ever sees. The production binary wires in the SQL implementation at `main`; the test wires in the memory one. Neither the service nor its tests change.

### Domain Errors Replace Storage Errors At The Boundary

`sql.ErrNoRows`, a driver's duplicate-key error, an HTTP 404 — these are storage details. Returning them verbatim leaks persistence into the domain: now the domain package must import `database/sql` just to compare against `sql.ErrNoRows`, and a test must reproduce the exact storage error to assert against it. The repository's job at its boundary is to translate. Define a small set of domain sentinels — `ErrNotFound`, `ErrDuplicateEmail`, `ErrDuplicateID` — and map every storage-specific failure onto them before returning. Callers then use `errors.Is(err, ErrNotFound)` and never learn what backend produced the error.

The translation must be done with sentinels and `%w` wrapping, not by matching on `err.Error()` text. Matching on a message string couples every caller to the wording of a library's error, so rewording it — a non-behavioral change — silently breaks tests. A sentinel is a stable identity that survives wrapping; `errors.Is` walks the chain and finds it.

### Context, Pointers, And Accessors: The Three Signature Habits

Three habits recur in every method signature and each prevents a specific class of bug.

Every method takes `context.Context` as its first parameter. Context is Go's standard cancellation and deadline handle; without it, an HTTP request that the client abandoned cannot signal the in-flight query to stop, and tracing spans cannot be threaded through. Even an in-memory implementation should accept and honor it (checking `ctx.Err()` at entry) so that swapping in a slow backend later needs no signature change.

Mutating methods accept a pointer to the domain object, not a value. `Create(ctx, *User)` lets the repository stamp `CreatedAt`/`UpdatedAt` on the very struct the caller holds; a by-value `Create(ctx, User)` mutates a copy, so the caller sees zero timestamps and a test that checks them fails in a way that looks like a storage bug.

Read-only accessors such as `Count()` and `Snapshot()` let tests and a `cmd/demo` inspect what a repository holds without exposing its internal maps. Keeping the fields unexported means the constructor's invariants (initialized maps, a live mutex) cannot be bypassed by a caller reaching in.

### One Contract, Many Implementations

The strongest argument for the pattern shows up in the test suite. Write the behavioral expectations once as a helper — `runRepositoryContract(t, repo)` — that exercises any value satisfying the interface: create sets timestamps, a duplicate email is rejected, a missing id returns `ErrNotFound`, update keeps secondary indexes in sync, list comes back sorted. Each implementation gets a thin test that simply calls the shared helper. When a second backend arrives, it inherits the entire contract for free, and any behavioral drift between implementations surfaces as a failing shared assertion rather than hiding in two divergent test files.

### Querying Without Leaking SQL: The Specification Pattern

`List` returns everything, but real callers want subsets: in-stock books under twenty dollars, users created this week. The naive fix multiplies the interface — `ListByCategory`, `ListByCategoryAndPrice`, `ListInStock` — until it is a combinatorial mess, and every new filter combination is a new method and a new SQL query. The specification pattern collapses that explosion into one method, `Find(ctx, spec)`, plus a small algebra of predicates.

A specification is an object that answers one question about a domain object: "does this satisfy me?" Each atomic spec (`InCategory("books")`, `PriceAtMost(2000)`, `InStock()`) is trivial, and three combinators — `And`, `Or`, `Not` — compose them into arbitrarily complex criteria as a tree. The repository walks its collection and keeps the items the spec is satisfied by. The caller expresses intent declaratively — `And(InCategory("books"), PriceAtMost(2000), InStock())` — and the interface never grows. The same spec tree that an in-memory repository evaluates by calling `IsSatisfiedBy` can, in a SQL-backed repository, be compiled into a `WHERE` clause; the domain expresses the criterion once and each backend interprets it. The trade-off is that an in-memory `Find` is a full scan filtered in Go: correct and perfectly fine for moderate collections, but a real database pushes the predicate down to an index, which is exactly why keeping the criterion as data (a spec tree) rather than code (a fixed method) is what lets you translate it later.

### Adding Behavior Without Touching Code: The Decorator

Caching, logging, metrics, retries, and authorization checks are cross-cutting: every repository wants them and none of them belong in the storage code. Threading a cache and a logger through the SQL implementation bloats it and forces the in-memory implementation to reimplement the same plumbing. The decorator pattern keeps them out. Because the repository is an interface, you can write a type that both implements the interface and holds another implementation of it, delegating each call to the inner one while adding behavior around the delegation.

A `LoggingRepository` logs each call and forwards it. A `CachingRepository` answers `Get` from an in-memory cache on a hit, falls through to the inner repository on a miss, and — this is the half that is easy to forget — invalidates the cached entry on every `Put` and `Delete` so a write is never shadowed by a stale read. Each decorator is a thin wrapper that satisfies the same interface as the thing it wraps, so they stack: `Logging(Caching(Memory))` is a fully functional repository where every call is logged, reads are served from cache, and the base storage is reached only on a miss. The composition root assembles the stack; no layer knows how many other layers surround it. The ordering of the stack is a real decision — putting logging outermost logs every call including cache hits, while putting it innermost logs only the calls that reach storage — and making that choice is a one-line change at the composition root precisely because each layer is independent.

## Common Mistakes

### Returning Database-Specific Errors From The Interface

Returning `sql.ErrNoRows` (or a driver's duplicate-key type) straight out of `GetByID` forces every consumer of the domain to import `database/sql` to interpret it, and forces tests to manufacture the exact storage error to assert against. Map at the boundary instead: `if errors.Is(err, sql.ErrNoRows) { return nil, ErrNotFound }`. The domain sees only domain sentinels.

### Accepting Domain Objects By Value In Mutating Methods

`Create(ctx context.Context, user User)` takes a copy. Any field the repository sets — timestamps, a generated id — lands on the copy and is invisible to the caller. The unit test that constructs the user and the repository in the same function may pass because it re-reads through `GetByID`, but a caller that trusts the struct it passed in sees zero values. Accept `*User` so the caller and the repository share one struct.

### Matching Storage Error Text With strings.Contains

`if strings.Contains(err.Error(), "duplicate")` couples the domain to the exact wording of a library's error message. A library upgrade that rewords the message — a change with no behavioral meaning — breaks the check. Define a sentinel, wrap the storage error with `%w`, and compare with `errors.Is`. Identity survives wrapping; text does not.

### One Test Suite Per Implementation Instead Of A Shared Contract

Writing `TestMemoryRepo_...` and `TestSQLiteRepo_...` with different shapes lets the two implementations drift: a behavior the memory repo enforces and the SQL repo does not hides in the asymmetry. Express the behavior once as `runRepositoryContract(t, repo)` and call it from each implementation's test. Every backend is then held to the identical standard.

### A Caching Decorator That Forgets To Invalidate

A cache that populates on `Get` but does not evict on `Put`/`Delete` will happily serve a value that has since been overwritten or removed. The write reaches the inner repository, the cache still holds the old entry, and the next `Get` returns stale data. Every mutating method on a caching decorator must invalidate (or update) the corresponding cache entry before returning.

### A Specification Repository That Mutates Caller State During A Scan

`Find` returns pointers into the repository's own storage. If callers are allowed to mutate those structs, a "read" silently rewrites stored data and races other readers. Either return copies, document the result as read-only, or — as these exercises do — guard the scan with a read lock and treat the returned pointers as immutable snapshots that callers must not write through.

---

Next: [01-user-repository.md](01-user-repository.md)
