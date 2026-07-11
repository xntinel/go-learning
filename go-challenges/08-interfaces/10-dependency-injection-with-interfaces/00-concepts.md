# Dependency Injection With Interfaces — Concepts

Dependency injection has a bad reputation in Go, mostly because engineers arrive
carrying the word from languages where it means a reflection-driven container, an
XML graph, or an annotation processor that wires objects at startup. Go needs
none of that. Dependency injection in Go is plain constructor passing: a function
receives the collaborators it needs as arguments instead of reaching out to build
or find them. The entire discipline reduces to a single question — *where* is each
dependency constructed? A senior backend engineer answers that question the same
way every time: at the edge, in the composition root (`main`), and nowhere else.
Everything below that root receives what it needs through interfaces and stays
ignorant of concrete types, global state, and the wall clock.

This is not academic. The payoff is the property that separates a service you can
operate from one you dread: deterministic, fast, hermetic tests. A retry loop that
sleeps on an injected clock is tested in microseconds; the same loop calling
`time.Sleep` forces the suite to actually wait. A client that takes an
`*http.Client` is tested against an `httptest.Server`; the same client reaching
for `http.DefaultClient` cannot be pointed anywhere and ships with no timeout. A
health endpoint whose checkers are injected can simulate a dead database without a
dead database. The interface is where the fake attaches, and construction-at-the-root
is what keeps that seam clean. This file is the conceptual foundation; read it
once and every one of the independent exercises that follow will make sense.

## Concepts

### Injection is about location, not machinery

There is no framework to learn. `NewService(clock, repo, logger)` *is* dependency
injection. The design work is deciding where the arguments come from. If a
constructor builds its own dependencies — `repo := NewMemoryRepository()` inside
`NewService`, or a method that calls `time.Now()` directly — those dependencies
are welded in place and a test can no longer control them. The fix is always to
push construction one level up, toward `main`, until it reaches the composition
root where the concrete choices are made exactly once. Business logic below the
root names only interfaces.

### A constructor should be a near-pure function of its inputs

When `NewService(deps...)` neither performs I/O nor captures ambient state, it
becomes a pure assembler: given the same fakes, it produces the same service. That
is what lets a test stand the whole system up in a single line and know there is
nothing hidden to mock out globally. A constructor that opens a database
connection, dials a socket, or reads `os.Getenv` has smuggled the composition root
into the middle of the program, and every test now pays for it.

### The default-constructor trap

The most seductive mistake is the convenience constructor that "just works":
`NewService()` that internally does `NewMemoryRepository()`, grabs `time.Now`, and
uses `http.DefaultClient`. It reads beautifully at the call site and is a
testability trap. The test cannot substitute storage, cannot pin the clock, cannot
intercept the network. If you want such a convenience, layer it: keep the honest
`NewService(deps...)` and, if you must, add a separate `NewDefault()` in the
composition-root package that wires the real dependencies and calls the honest
constructor. The trap is only sprung when the *only* constructor is the one that
hides its dependencies.

### The interface is the test seam, so keep it narrow

An interface exists at the boundary where a fake attaches. The narrower it is, the
smaller the fake. Go's structural typing lets the *consumer* declare the interface:
if a `UserService` only ever calls `UserByID`, it should depend on a one-method
`interface{ UserByID(ctx, id) (User, error) }` that it defines itself, not on the
provider's twenty-method `Store`. The concrete `*sqlStore` satisfies the narrow
interface implicitly — the provider need not even know the interface exists. This
is interface segregation applied through Go's most distinctive feature, and it is
why a good Go fake is often five lines. Defining fat interfaces in the provider
package is the anti-pattern: every fake then has to implement methods it never
calls.

### Accept interfaces, return concrete structs

The idiom has two halves and both matter. Take interface *parameters* so the seam
is abstract, but return a concrete `*Service`, not some `ServiceIface`. Returning
an interface needlessly narrows the API the caller can use, obstructs adding
methods later without editing the interface, and invites the typed-nil bug: a
`func() ServiceIface` that returns a nil `*Service` hands back a non-nil interface
wrapping a nil pointer, and `result == nil` is then false. Return the struct; let
callers depend on interfaces when *they* need a seam.

### Time is a dependency

`time.Now()` and `time.Sleep` are global reads of ambient state, exactly like
`http.DefaultClient`. Business logic that consults them directly cannot be tested
without either flaking or actually waiting. There are two disciplined answers.
First, inject time: a `Clock` interface (`Now() time.Time`) for reads and a
`Sleeper` (`Sleep(ctx, d) error`) for waits, so a retry's backoff schedule is
asserted against a fake that records durations instead of sleeping. Second, for
code that legitimately uses real `time.After`/`time.Ticker` and where an injected
clock would be noise, Go 1.25's `testing/synctest` virtualizes the `time` package
inside a bubble: `synctest.Test` runs the goroutines on a fake clock that advances
only when all of them are durably blocked, and `synctest.Wait` synchronizes with
background goroutines. Injection and synctest are complementary — injection when
you want to *control* time in production too, synctest when you only need
determinism in a test.

### Required versus optional dependencies

Not every dependency is equal. A repository is *required*: without storage the
service cannot do its job, so a nil repo should fail fast in the constructor —
return an error or panic at wiring time, never at 3 a.m. under load. A logger or a
metrics sink is *optional*: its absence should degrade gracefully, never crash.
The null-object pattern is the tool: substitute a no-op implementation for a nil
optional dependency so the calling code never has to nil-check. In modern Go the
idiomatic no-op logger is `slog.New(slog.DiscardHandler)` (Go 1.24+). The failure
mode to avoid is treating these backwards — dereferencing a nil logger and
panicking in production, or silently no-oping a nil repo so real failures are
swallowed without a trace.

### The composition root is the only place that imports adapters

Draw the import graph and it should point one way: adapters (a Postgres repo, an
HTTP client, a slog handler) import the domain's interfaces; the domain imports
nothing concrete. The composition root — `main`, or a `run(ctx) error` it calls —
is the single package permitted to import the concrete adapters, construct them,
and inject them downward. This is the dependency-inversion principle expressed as
an import rule, and it is what keeps the domain portable: swap Postgres for SQLite
by editing one file. When concrete-adapter imports leak into business packages,
the arrow reverses and the domain is welded to its infrastructure.

### Functional options keep constructors additive

A required dependency belongs in a positional parameter. The optional, tunable
knobs — clock, logger, timeout, retry policy — do not, because every new knob would
either grow the parameter list or force callers to pass a mutable `Config` struct
full of zero values. Functional options solve this: `NewService(repo, opts
...Option)` where `Option` is `func(*Service)`, and `WithClock`, `WithLogger`,
`WithTimeout` each override a documented default. Callers set only what they care
about, defaults fill the rest, and adding a `WithRetries` option next year breaks
no existing call site. Last writer wins when two options touch the same field, so
ordering is defined and testable.

### slog is the injected logger

`log/slog` is the standard structured logger and the natural thing to inject. In
tests, inject `slog.New(slog.DiscardHandler)` to exercise the logging code paths
without polluting test output. When the log *output* is itself a contract — a
service that must emit an audit event — inject a `slog.Handler` you control (or a
small fake) and assert on the records it captured. Injecting the logger, rather
than reaching for a package-level `log.Printf`, is what makes both of those
possible.

### Global mutable state is invisible coupling

Package-level loggers, `http.DefaultClient`, `time.Now`, singletons, a shared
`*sql.DB` in a global — each is a dependency the signature does not admit to. The
costs are concrete: parallel tests interfere through the shared state, the
per-request timeout that should live on the client cannot be set, and a fake
cannot be substituted for one test without leaking into the next. Every global you
inject instead is a seam that reappears. The whole of this lesson is the practice
of turning invisible coupling into explicit, injected, fakeable parameters.

## Common Mistakes

### Constructing dependencies inside the constructor

Wrong: `NewService` calls `NewMemoryRepository()` or captures `time.Now`. The test
is forced onto real storage and the wall clock.

Fix: the constructor takes the dependencies as parameters; only the composition
root constructs the concrete ones.

### Reading the wall clock or sleeping in business logic

Wrong: calling `time.Now()` or `time.Sleep` inside a retry or a service method, so
tests either flake on timing or must sleep for real seconds.

Fix: inject a `Clock` and a `Sleeper`, or test the real-time code under
`testing/synctest`.

### A fat, provider-side interface

Wrong: defining a twenty-method `Store` interface in the provider package, so
every fake must implement all twenty methods.

Fix: define a one- or two-method interface in the *consumer* package naming only
the methods it calls; the concrete store satisfies it implicitly.

### Returning an interface from a constructor

Wrong: `func NewService(...) ServiceIface`, which narrows the concrete API and
invites the typed-nil-in-interface bug.

Fix: return the concrete `*Service`; let callers introduce interfaces at their own
seams.

### Getting required-versus-optional backwards

Wrong: dereferencing a nil optional logger and panicking in production, or
silently no-oping a nil required repo so failures vanish.

Fix: fail fast on a nil required dependency in the constructor; substitute a
null-object (or `slog.DiscardHandler`) for a nil optional one.

### Reaching for http.DefaultClient

Wrong: an API client that calls `http.DefaultClient.Do` — no timeout, a shared
global transport, and nothing a test can point at a fake server.

Fix: inject a configured `*http.Client` or a narrow `Doer` interface; set the
timeout at the composition root.

### Leaking adapter imports into business packages

Wrong: a domain package that imports the Postgres driver or `net/http`
directly, re-inverting the dependency direction.

Fix: confine concrete-adapter imports to the composition root; the domain names
only interfaces.

### Growing a positional parameter list per knob

Wrong: adding a fifth, sixth, seventh positional parameter to `NewService` each
time a tunable is needed, or passing a giant mutable `Config`.

Fix: keep required dependencies positional and make optional knobs functional
options with documented defaults.

### The typed-nil interface gotcha

Wrong: assigning a nil `*T` to an interface variable and comparing it to `nil`;
the interface is non-nil because it carries the type. Checking `logger == nil`
after such an assignment silently misses the substitution.

Fix: compare before wrapping, or design the null-object substitution to happen in
the constructor where you still hold the concrete type.

### Sleep-based tests for retry and timeout logic

Wrong: testing a backoff or a deadline by sleeping longer than it and hoping,
producing a slow and nondeterministic suite.

Fix: an injected clock plus a recording `Sleeper`, or `testing/synctest` for
real-time code.

Next: [01-constructor-injection-service.md](01-constructor-injection-service.md)
