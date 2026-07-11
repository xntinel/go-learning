# Accept Interfaces, Return Structs — Concepts

"Accept interfaces, return structs" gets quoted as a style tip, but it is really a
statement about dependency direction and testability. A function that takes a
dependency should accept the narrowest interface that names the methods it calls;
a function that produces a value should hand back the concrete type. Say it that
way and three separate senior-level concerns fall out of the same rule: who owns
an interface, how you segregate a fat port into role interfaces, and how you stack
caching / retry / metrics decorators without touching a single call site. The same
rule, read from the return side, is also what saves you from the typed-nil trap —
a real production incident class where a concrete nil pointer returned through an
`error` breaks `if err != nil`. This file is the conceptual foundation for the
independently-gating exercises that follow; read it once and you have the model you
need to reason through each one.

## Concepts

### Dependency direction: the consumer owns the interface

The naive habit, imported from Java-style design, is to put the interface next to
the implementation: the `postgres` package exports both `Store` (interface) and
`PGStore` (struct). Go inverts this. Because interface satisfaction is structural
and implicit — a type satisfies an interface just by having the methods, with no
`implements` keyword — the interface belongs in the *consumer's* package, defined
in terms of what the consumer needs. The producer exports only the concrete type.
`*PGStore` satisfies the consumer's `ItemGetter` without importing it, without
knowing it exists.

Because satisfaction is structural, the consumer's interface and the producer's
struct can even share a name across packages without any conflict or coordination.
The consumer can define `store.Repository` (an interface) while the producer exports
`postgres.Repository` (a struct), and `*postgres.Repository` satisfies
`store.Repository` purely because the method sets line up — the two names live in
different packages and the compiler never confuses them. Nothing in the language
ties the interface's name to the concrete type's name; the only thing that binds
them is the shape of the methods. That is worth internalizing, because it means you
name each type for its own package's vocabulary and let structural matching do the
rest, rather than inventing `IRepository` / `RepositoryImpl` disambiguators that a
nominal type system would force on you.

This is not a cosmetic preference. When the producer owns the interface, every
consumer is transitively coupled to the producer's package, the interface grows to
the union of everything any consumer might need, and a test double for one consumer
has to stub methods that consumer never calls. When the consumer owns the
interface, the arrow of dependency points from concrete implementation toward
abstract need, the interface stays as small as one consumer's use, and the producer
is free of any knowledge of who consumes it.

### Accept interfaces so callers can substitute

You accept an interface as a *parameter* so the caller controls what gets passed.
In production that is the real Postgres-backed store. In a unit test it is an
in-memory fake. In a hardened deployment it is a decorator that adds retry and
metrics around the real store. All three satisfy the same interface; the function
under construction neither knows nor cares which one it got. The moment you accept
a concrete `*PGStore` instead, you have welded the function to Postgres: you cannot
test it without a database, and you cannot wrap it without editing it.

### Return structs so callers keep the concrete type

You return a *struct* (usually a pointer) so the caller keeps full access to the
concrete type's methods — including methods you add later. If a constructor returns
an interface, the surface is frozen at whatever that interface declares; a method
added to the concrete type is invisible until you also widen the interface and
break everyone who embedded it. Returning the concrete type lets the type evolve
additively. The caller can always assign the result to a narrower interface
variable at its own boundary if it wants abstraction; that is the caller's choice,
not one you impose by returning an interface.

There are genuine exceptions — a factory that must return one of several
implementations chosen at runtime, or a package that deliberately hides its
concrete type behind an interface for API-stability reasons. But those are chosen
deliberately for a reason, not the default. The default is: return the struct.

### Interface segregation: depend on the two methods you call

A `Repository` with `Get`, `Put`, `Delete`, `List`, `Count`, `BeginTx`... is a fine
concrete surface, but a poor dependency. A `PriceReporter` service that only ever
calls `Get` should depend on a one-method `ItemGetter`, not the whole repository.
The payoff is concrete: the reporter's test fake implements one method instead of
six, an alternate read-only backend can satisfy the reporter without pretending to
support writes, and the reporter's true dependency is legible from its type. Small
role interfaces — `io.Reader`, `io.Writer`, `io.Closer` and their compositions —
are the canonical Go example. Prefer several one- to three-method interfaces,
composed where you need more, over one wide interface reused everywhere.

### The decorator pattern is the rule's payoff

A type that both *accepts* an interface and *satisfies* the same interface can wrap
any implementation transparently. `CachingRepository` holds a `Repository`, adds a
read-through cache, and is itself a `Repository`. `RetryRepository` holds a
`Repository`, retries transient failures, and is a `Repository`. `ObservedRepository`
holds a `Repository`, records latency and call counts, and is a `Repository`.
Because every layer has the same type as what it wraps, they compose in any order —
`Observed(Retry(Cache(Memory)))` — and the service that depends on `Repository`
sees no change at all. This is the whole reason the rule earns its keep: cross-
cutting concerns become wrapping layers you assemble at construction, not edits
smeared through business logic.

Order matters for behavior, not for compilation. Caching outside retry means a
cache hit skips retries entirely; retry outside caching means every retry re-checks
the cache. Metrics outermost measures the whole stack including cache hits; metrics
innermost measures only the real backend. You decide the order at wiring time, and
because each layer is a struct that accepts the interface, changing the order is a
one-line change where you assemble them.

### The typed-nil trap: an interface value is a (type, value) pair

An interface value is not a pointer; it is a pair of (dynamic type, dynamic value).
It is nil only when *both* halves are nil. If you have a concrete pointer
`var p *ValidationError = nil` and you return it through a function whose result
type is `error`, the interface you produce has dynamic type `*ValidationError` and
dynamic value nil — the type half is not nil, so the interface is not nil. The
caller's `if err != nil` is unexpectedly true, and it acts on an error that logically
is not there. This is a recurring production incident: a validator that returns its
concrete `*ValidationError` type, returns a nil one on the happy path, and every
caller reports a validation failure that never happened.

The fix is exactly the return-structs discipline read carefully. Either return the
error through the `error` interface as a literal `nil` (`return nil`), or declare
the happy path to return the interface type and assign nil to *that*. Do not build
a concrete typed nil and let it slip through an interface return. `reflect.ValueOf
(err).IsNil()` can *diagnose* the condition after the fact, but it is a smell, not a
fix; the fix is not to create the typed nil in the first place.

### Compile-time satisfaction assertions

`var _ Repository = (*MemoryRepository)(nil)` compiles to nothing but forces the
build to fail if `*MemoryRepository` ever stops satisfying `Repository` — for
instance after you add a method to the interface or change a signature. Put one of
these next to each implementation of a port you care about. It converts "does this
type still satisfy the interface" from a runtime surprise, discovered when some
assignment finally exercises the path, into a build error the compiler hands you
immediately. It costs one line and zero runtime.

### Sentinel errors are the contract decorators map on

Decorators and HTTP layers need to branch on *what kind* of outcome the wrapped
call produced without depending on its concrete error type. A caching layer must
not cache `ErrNotFound` as a positive entry; a retry layer must retry a transient
error but return a permanent one immediately; an HTTP handler must map `ErrNotFound`
to 404 and everything else to 500. Package-level sentinel errors (`var ErrNotFound
= errors.New(...)`), wrapped with `%w` and matched with `errors.Is` / `errors.As`,
are the contract that makes this possible across layers that do not share concrete
types. This is why the exercises define sentinels and always wrap: the sentinel is
the seam every decorator reads.

### Testability is the operational reason

None of this is aesthetics. An injected interface is what lets a unit test drive the
success path, the `ErrNotFound` path, a transient-failure-then-success path, a
context-cancellation path, and a malformed-input path — all deterministically, with
no real datastore and no network. A handler tested against a fake `ItemService`
exercises its 200 / 404 / 400 branches in microseconds under `httptest`. That is the
concrete return on the abstraction: not "cleaner code" in the abstract, but the
ability to pin every failure mode in a fast, hermetic test.

## Common Mistakes

### Returning an interface where a struct would do

Wrong: a constructor returns `Repository` instead of `*MemoryRepository`. Callers
must type-assert to reach any concrete method, and any method you add later is
invisible until you widen the interface.

Fix: return the concrete `*MemoryRepository`. Let callers narrow to an interface at
their own boundary if they want to.

### Accepting a concrete pointer where an interface would do

Wrong: `NewService(r *MemoryRepository)`. The service is now untestable without the
real store and cannot be wrapped by a decorator.

Fix: accept the interface, `NewService(r Repository)`, so fakes and decorators
substitute freely.

### Defining the interface in the implementation's package

Wrong: the `postgres` package exports the `Repository` interface next to
`*PGStore`. Every consumer is now coupled to `postgres`, and the interface grows to
serve all of them.

Fix: define the interface in the consumer, sized to that consumer's needs. The
producer exports only the concrete type; it satisfies the interface structurally.

### Fat interfaces

Wrong: a six-method `Repository` injected into a service that calls two methods, so
every fake and every alternate backend must stub four irrelevant methods.

Fix: depend on a one- to three-method role interface (`ItemGetter`) in the consumer;
compose small interfaces where a consumer genuinely needs more.

### Returning a concrete typed nil through an interface

Wrong: `func Validate(...) error` whose body returns a `(*ValidationError)(nil)` on
success. The returned `error` has a non-nil dynamic type, so `err != nil` is true at
every call site even when validation passed.

Fix: `return nil` through the `error` interface, or declare and assign nil to the
interface type. Never let a concrete typed nil escape through an interface return.

### Caching negative results as positive entries

Wrong: a read-through cache stores the `ErrNotFound` outcome as a real cache entry,
so a later `Put` of that key is never observed — the cache keeps answering "absent".

Fix: only populate the cache on a successful `Get`; propagate `ErrNotFound` each
time and let a `Put` seed the entry.

### Swallowing context cancellation in a retry loop

Wrong: a retry loop keeps sleeping and retrying after the caller's context is
already cancelled, burning through every attempt for a caller who has left.

Fix: check `ctx.Err()` before each attempt and select on `ctx.Done()` during the
backoff; return `ctx.Err()` the moment it is cancelled.

### Passing nil as a required dependency

Wrong: constructing a service with `nil` for its repository and relying on a
deferred nil-pointer panic somewhere downstream to signal the mistake.

Fix: treat "the dependency is required" as a contract — fail fast, and pin it with a
test that asserts the panic (or a returned error), so the contract is explicit and
does not regress silently.

### Testing decorators only on the happy path

Wrong: a decorator test that never asserts the wrapped dependency's call count, so a
broken cache that always misses, or a retry that never retries, passes green.

Fix: inject a counting fake and assert the underlying call count — hit-once on a
cache hit, exactly N attempts on retry — so the decorator's actual behavior is
pinned, not just its output.

Next: [01-implement-repository.md](01-implement-repository.md)
