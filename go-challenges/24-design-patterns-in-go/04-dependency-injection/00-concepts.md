# 4. Dependency Injection — Concepts

Dependency injection is the discipline of having an object receive the things it
collaborates with rather than constructing them itself. Stated that plainly it
sounds trivial, and in Go it nearly is: there is no container framework you must
adopt, no XML, no annotations. A dependency is "injected" when it arrives as a
constructor argument, and the whole pattern is the decision to push the choice of
concrete implementation outward — out of the type that uses a database, a clock,
or a payment gateway, and up to the single place in the program that assembles the
whole graph. This file is the conceptual foundation for the three exercises that
follow: constructor injection with consumer-defined interfaces, injecting the
hidden seams (time and randomness) that otherwise make code untestable, and a
hand-rolled provider container that wires a small graph with lazy, shared
singletons. Read it once and you will have the reasoning you need for all three.

## Concepts

### What "Injection" Actually Means, and What It Buys

A type has a dependency whenever it calls out to something it does not own: a
database handle, an HTTP client, the system clock, a random source, a logger. The
naive style hard-codes the choice inside the type — a service that opens its own
`sql.DB`, reads its own environment variables, calls `time.Now()` directly. That
type is now welded to those exact implementations. You cannot run it without a
real database; you cannot make its output deterministic because it reads the wall
clock; you cannot point it at a second backend without editing it. Dependency
injection inverts the direction of that decision. The type declares what it needs
and accepts it from outside; the caller decides what to pass. In production the
caller passes the real database and the real clock; in a test the caller passes an
in-memory store and a clock frozen at a fixed instant.

The payoff is not abstract architecture for its own sake. It is three concrete,
measurable properties. Tests become fast and deterministic because every external
effect is replaced by an in-memory double. The same type serves many
configurations because the choice of backend moved out of it. And the program
gains a single readable map of how its parts connect, because the wiring is no
longer scattered across dozens of constructors but concentrated in one place.

### Constructor Injection Is the Default Form

Go has three places a dependency could enter a type: the constructor, a method
parameter, or a setter/exported field. Constructor injection — passing every
collaborator to `New...` and storing it in an unexported field — is the right
default and the others are narrow exceptions. Constructor injection makes the
dependency mandatory and the object valid the instant it exists: there is no
half-constructed state where the store has been set but the notifier has not.
Method injection (passing a dependency to the one method that needs it) is correct
only when the dependency genuinely varies per call, the canonical example being
`context.Context`, which is why it rides as the first method parameter rather than
a constructor field. Setter injection — a `SetStore` method or an exported field
the caller is expected to fill — reintroduces exactly the invalid intermediate
state constructor injection removes, and should be reserved for genuinely optional
collaborators with a working default.

A constructor that takes dependencies should also validate them. A `nil` store
passed by mistake is a programming error, and the kind choice is to fail loudly at
construction — return `(*T, error)` and reject the `nil` — rather than store the
`nil` and panic later on the first method call, far from the site of the mistake.
The construction-time check turns a deferred, hard-to-locate panic into an
immediate, obvious one.

### Interfaces Belong to the Consumer, Not the Provider

The single most important Go-specific rule of dependency injection is where the
interface is declared. Because Go's interfaces are satisfied implicitly — a type
satisfies an interface by having the right methods, never by naming it — the
consumer can declare the exact interface it needs, and any existing concrete type
already satisfies it without modification. So the interface lives next to the code
that calls it, not next to the code that implements it. A service that needs to
persist an order declares `type orderStore interface { Save(...) error }` listing
only the one or two methods it actually invokes. The concrete `*sql.DB`-backed
store, sitting in another package, never imports the service and never declares
that it implements anything; it simply has a `Save` method, and that is enough.

This inverts the instinct carried over from languages with explicit interface
implementation, where the provider package exports a wide `Database` interface
and every consumer depends on all of it. Consumer-defined interfaces stay small —
one to three methods — because they describe one caller's needs, not a backend's
full surface. Small interfaces are trivial to implement as test doubles, they
decouple the consumer from methods it never calls, and they let a method change on
the concrete type without breaking consumers that did not use that method. The
related maxim "accept interfaces, return structs" follows directly: a constructor
accepts the narrow interface so any implementation fits, but returns the concrete
type so callers keep full access to its methods and the compiler keeps full type
information.

### Seams: Time, Randomness, and the Outside World

The dependencies people remember to inject are the obvious ones — databases,
network clients. The ones they forget are the implicit calls to global state
buried inside otherwise pure logic: `time.Now()`, `rand.Intn`, `crypto/rand`,
`os.Getenv`, `uuid.New()`. Each of these is a hidden dependency on something the
test cannot control, and each is a place the code could be cut and a double
slipped in — what Michael Feathers named a "seam". A function that stamps a record
with `time.Now()` cannot be tested for the exact timestamp it writes, because the
timestamp is different on every run. A function that generates an ID with a random
source produces a different ID every time, so no assertion on the output can be
exact.

The fix is to treat time and randomness as injected dependencies like any other.
Declare a tiny `Clock` interface with a `Now() time.Time` method and an `IDGen`
interface with a `NewID() string` method. Production wires a `systemClock` whose
`Now` calls `time.Now()` and a random generator; tests wire a `fakeClock` that
returns a fixed, configurable instant and a counter-based generator that returns
`id-1`, `id-2`, and so on. Now the very same code path that is nondeterministic in
production becomes exactly assertable in a test, because the test owns the clock.
This is the highest-leverage application of dependency injection: it is the
difference between a test that asserts "an ID was produced" and one that asserts
"the ID was exactly `id-1` and the timestamp was exactly the epoch".

### Stubs, Fakes, and Mocks Are Not the Same Thing

The doubles you inject in tests come in distinguishable flavors, and using the
precise word clarifies intent. A stub returns canned answers and holds no logic:
a payment processor that always returns success, or always returns
`ErrDeclined`. A fake is a real, working, simplified implementation: an in-memory
map standing in for a database, with genuine `Save` and `FindByID` behavior, just
without persistence. A mock additionally records how it was called and lets the
test assert on those interactions: "Send was called exactly once, with this
recipient". In Go the idiomatic default is a fake or a hand-written stub defined
as a few-line struct in the test file itself, not a generated mock from a
framework. The double lives next to the test that needs it precisely because a
shared `testutil` package of mocks drifts: it accumulates methods for every
consumer and slowly encodes assumptions from tests it was never written for. A
local double stays honest because it can only express what its one test requires.

### The Composition Root: Wire in One Place

If every type accepts its dependencies, something must eventually choose the
concrete ones and assemble the graph. That place is the composition root, and in a
Go program it is `main` (or, in this lesson, `cmd/demo/main.go`). The composition
root is the only part of the program that names concrete implementations: it opens
the real database, constructs the real notifier, and threads them through the
constructors in dependency order. Everything below it deals only in interfaces.
Keeping the wiring in one location means the entire shape of the program — what
depends on what — is readable in a single function, and swapping an implementation
is a one-line edit in a known place rather than an archaeology expedition.

The composition root also fixes the order of construction. A dependency must exist
before the thing that depends on it, so the root builds leaves first (the logger,
the config) and roots last (the top-level service that needs them). For a graph of
two or three nodes this is a handful of lines and reads top to bottom.

### Manual DI, a Hand-Rolled Container, and Frameworks

As a graph grows, the linear wiring in `main` grows with it, and three responses
exist on a spectrum. Manual injection — call the constructors by hand — is the
right answer for the overwhelming majority of programs, and it is where you should
start and usually stay; it has no magic, no reflection, and the compiler checks
every edge. A hand-rolled container is the next step when several services share
the same expensively-constructed dependencies (one database pool, one logger) and
you want each built once and reused: a small struct that holds the config and
exposes memoized provider methods, each lazily constructing its node on first
request and caching it, so the shared dependency is a true singleton across the
graph. This is still ordinary Go — a struct with methods and a `sync.Once` per
node — and it is exactly the shape that code generators emit. The third response is
a framework: Google's Wire generates the wiring code at compile time from provider
sets, so there is no runtime reflection and the result is the same plain function
you would have written by hand; Uber's Fx resolves the graph at runtime with
reflection and adds lifecycle management for long-running services. Reach for a
framework only when the graph is large enough that hand-wiring genuinely hurts;
for three dependencies it is pure ceremony.

The hand-rolled container deserves one more note on correctness: if it is to be
safe for concurrent first-use, each lazy provider must construct its node exactly
once even when several goroutines request it simultaneously. A `sync.Once` per
node gives precisely that guarantee — the first caller constructs, every other
caller blocks until construction finishes and then observes the same instance —
and it is what makes the container's singletons correct under the race detector.

## Common Mistakes

### Defining Wide Interfaces in the Provider Package

Wrong: a storage package exports `type Store interface { Save; FindByID; Delete;
Query; Begin; Commit; Rollback; Ping }` and every consumer depends on the whole
thing.

What happens: a consumer that only saves is now coupled to seven methods it never
calls; its test double must stub all of them; a change to `Query` breaks consumers
that never query.

Fix: let each consumer declare the one-to-three-method interface it actually uses,
next to its own code. The concrete provider satisfies all of them implicitly
without importing any consumer.

### Calling `time.Now()` or `rand` Directly Inside Logic Under Test

Wrong: a function computes an expiry as `time.Now().Add(ttl)` or tags a record
with `rand.Int63()` inline.

What happens: the output changes on every run, so no test can assert the exact
value; the timestamp and the ID become permanently unassertable, and tests degrade
to checking only that the field is non-empty.

Fix: inject a `Clock` and an `IDGen` interface. Production passes the system
implementations; tests pass a fake clock pinned to a fixed instant and a
deterministic counter generator, making the formerly nondeterministic path exactly
assertable.

### Treating Constructor Failure as Impossible

Wrong: `NewService(store, clock)` performs no validation and stores whatever it is
given, including a `nil` dependency.

What happens: the `nil` is dereferenced later, on the first method call, producing
a panic far from the construction site with no hint of which dependency was
missing.

Fix: validate in the constructor and return `(*Service, error)`, rejecting `nil`
dependencies. The failure then surfaces immediately, at the place the mistake was
made, and a test can assert each rejection.

### Reaching for a DI Framework Before You Need One

Wrong: pulling in Wire or Fx to wire three dependencies because dependency
injection "needs" a container.

What happens: a build-time code generator or a runtime reflection graph,
an initialization order you no longer control by reading the code, and a layer of
indirection that earns nothing for a graph this small.

Fix: wire by hand in the composition root. Graduate to a memoized container only
when several services share expensive singletons, and to a framework only when the
graph is genuinely large.

### Sharing One `testutil` Package of Mocks Across Every Service

Wrong: a `pkg/mocks` package holding `MockStore`, `MockClock`, `MockNotifier`
imported by every test in the codebase.

What happens: each mock accumulates methods and behavior to satisfy every consumer;
a change for one test ripples through all of them; the mocks slowly encode
assumptions from tests they were never written for.

Fix: declare the double — usually a few-line fake or stub — in the test file that
needs it. A little duplication across test files is far cheaper than one shared
package that ages into a liability.

---

Next: [01-constructor-injection.md](01-constructor-injection.md)
