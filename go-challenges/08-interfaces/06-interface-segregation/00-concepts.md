# Interface Segregation for Production Backend Ports — Concepts

The Interface Segregation Principle (ISP) is usually taught as a warning against
"fat" interfaces. In Go the principle is more than a warning: the language turns
it into a tool. Because Go satisfies interfaces *structurally* and by convention
the CONSUMER declares the interface, a 15-method `Store` or a 10-method `Worker`
is never forced onto anyone unless a human deliberately writes it. The interface
at a call site is exactly the set of methods that call site invokes, and one
concrete type satisfies as many of those small interfaces as it happens to
implement, for free. This file is the conceptual foundation for the ten
independent exercises that follow: a segregated job queue, a god `Store` split
into role interfaces, a handler that depends on one method instead of a whole
service, an `io.Reader`/`io.Writer` streaming pipeline, a read-only cache view, a
narrow payment port, interface composition, a health aggregator, a split pub/sub
broker, and an audit of a fat object store. Read this once and you have the model
you need for all of them.

## Concepts

### ISP restated for Go: the consumer declares the interface

The Interface Segregation Principle is language-independent. Robert Martin's
original statement — "clients should not be forced to depend on methods they do
not use" — is the same in Java, C#, and Go. What differs is the enforcement
mechanism. In a nominal language you must explicitly write `implements Reader`,
so an interface is a contract the *implementer* opts into, and shrinking it later
is a coordinated change. In Go, satisfaction is structural: a type satisfies an
interface merely by having the right method set, with no declaration. That, plus
the strong community convention that interfaces live in the *consumer's* package
rather than the implementer's, means the natural unit of an interface is the call
site. A function that only reads declares a one-method reader; a function that
only writes declares a one-method writer; the same concrete `*pgStore` satisfies
both without ever naming either. The interface is not a property of the type. It
is a property of the place that uses the type.

### A fat interface transfers cost three ways

A large interface is not free even when it compiles. It imposes cost in three
directions at once. First, on *implementers*: every concrete type that must
satisfy a 12-method `Store` has to provide all 12, and a type that genuinely
needs only three ends up with nine stub methods that `panic` or return "not
implemented" — dead weight that is also a runtime hazard. Second, on *consumers*:
a reporting function that accepts the fat `Store` now transitively depends on
`Delete`, `WithinTx`, and `Migrate` even though it only ever calls `List`. That
inflates its blast radius (a change to the write path can force a recompile or a
re-review of the read path) and violates least privilege (the reporting code
*could* call `Delete`). Third, on *tests*: a fake for the fat interface must stub
every method, so a test that exercises one method carries a dozen panic-stubs as
noise. Narrowing the interface to the call site removes all three costs
simultaneously — the same edit that makes the fake trivial also makes the
consumer unable to mutate and frees the implementer from stubbing.

### Sizing the seam: the call site, not the domain

The single most useful heuristic in this lesson is that the right size of an
interface is the *call site*, not the *domain concept*. "Order storage" is a
domain concept with a dozen operations; it is the wrong unit for an interface. A
report generator that reads orders wants an `OrderReader` with `Get` and `List`.
A checkout path that creates orders wants an `OrderWriter` with `Create` and
`Update`. These are two different interfaces declared by two different consumers,
and it is correct and expected that one `*pgStore` satisfies both. Do not ask
"what is the interface for orders?" Ask "what methods does *this function* call?"
The answer is the interface, and it belongs in this function's package.

### One concrete type satisfies many small interfaces for free

Structural satisfaction means a single implementation lights up every small
interface whose method set it contains, with zero declarations. The job queue in
Exercise 1 is a `Producer` (it has `Submit`), a `Consumer` (it has `Take`), and
an `Inspector` (it has `Stats`) simultaneously — three unrelated interfaces
declared in three different consumers, all satisfied by the same `*Queue`. The
type never mentions any of them. This is what makes segregation cheap in Go:
splitting a fat interface into roles costs the implementer nothing, because the
concrete type already has all the methods; only the *parameter types* at the call
sites change.

### Composition is the counter-move to over-segregation

Segregation is not "make every interface one method." When a consumer genuinely
needs several roles, you compose small interfaces by *embedding* rather than
hand-writing a new fat interface. This is exactly what the standard library does:
`io.ReadWriteCloser` is not a fresh three-method interface, it is `Reader`,
`Writer`, and `Closer` embedded together. The discipline is: segregate into small
roles first, then compose at the call site as needed. A protocol handler that
needs to read, write, and close declares `ReadWriteCloser`; a drain-and-close
consumer that only reads and closes declares a smaller `ReadCloser`. Both are
built from the same atoms.

### io.Reader and io.Writer are the canonical proof of ISP payoff

The entire `io` ecosystem is the argument for minimal interfaces made concrete.
`io.Reader` and `io.Writer` have exactly one method each, and *because* they are
minimal an enormous set of composable helpers — `io.Copy`, `io.MultiWriter`,
`io.TeeReader`, `io.LimitReader`, `io.Pipe` — works uniformly over files, network
sockets, HTTP bodies, byte buffers, hashers, and compressors. A `CopyAndDigest`
function written against `io.Reader` and `io.Writer` (Exercise 4) runs unchanged
over all of them. Had `io.Reader` been a fat "file" interface with `Seek`,
`Stat`, and `Close`, none of that composition would exist. Minimality is what
made the ecosystem possible.

### Least privilege via types

A narrow interface is an access-control mechanism enforced by the compiler.
Handing a consumer a `Getter` (one method, `Get`) instead of a full
`Cache` (with `Set`, `Delete`, `Flush`) makes an entire class of bugs — a reader
poisoning shared cache state — *unrepresentable*: the code physically cannot call
`Set` because the type has no such method. The same move gives an ingestion
service a `Publisher` and a background worker a `Subscriber`, so the worker
cannot publish and the ingester cannot subscribe, checked at compile time rather
than caught in review. This is least privilege expressed as a type, and it is one
of the strongest reasons to segregate in a backend where a stray write is a
production incident.

### Migration without breakage

You can narrow a fat interface without a breaking change, precisely because
satisfaction is structural. Keep the concrete type exactly as it is — it still
has every method — and narrow only the *parameter types* at each call site, one
consumer at a time. A consumer that used to take the fat `OrderStore` and only
called `List` now takes an `OrderReader`; the concrete `*pgStore` passed in still
satisfies it, so nothing at the call site breaks. There is no coordinated
big-bang rename, no version bump, no "implements" declaration to update. You
migrate incrementally, consumer by consumer, and the old and new shapes coexist
during the transition.

### When NOT to segregate

Segregation has real costs, and a senior engineer knows when to stop.
Single-method-interface soup — a separate one-method interface for a type that
has exactly one implementation and exactly one caller — adds indirection and
naming churn with no testing or decoupling benefit. Premature abstraction is the
same mistake wearing a different hat: introducing an interface before a second
implementation or a test seam actually exists. YAGNI applies to interfaces as
much as to code. Introduce the interface at the moment a second implementation
appears, or a test needs a seam, or a consumer needs to be denied a capability —
not on speculation. The audit exercise (Exercise 10) is about doing this
deliberately: map real call sites, derive the minimal roles they justify, and
resist inventing roles no consumer asked for.

### Interfaces belong at the consumer's boundary

The corollary of "the consumer declares the interface" is that an interface
exported *from the implementation package* and returned by a constructor is
usually a smell — "interface pollution." When `NewOrderStore` returns an
`OrderStore` interface instead of `*pgStore`, every caller is coupled to a shape
they did not ask for, cannot extend the concrete type's API, and cannot declare
their own narrower view without an extra adaptation layer. The Go idiom is
"accept interfaces, return structs": constructors return the concrete type, and
each consumer declares the small interface it needs. The narrow ports in this
lesson are all defined where they are used, never handed down from the
implementation.

## Common Mistakes

### Defining a large interface first and splitting later

Wrong: exporting a 10-method `Store` interface, then discovering months later
that most consumers use three methods and trying to split it. Once the fat
interface is public, narrowing it is a breaking change to every implementer.

Fix: start with small interfaces sized to each call site and add methods only
when a specific consumer needs them. Growth is additive and local; shrinkage is
a break.

### Making a consumer depend on methods it never calls

Wrong: a `Consumer` interface that also requires `Submit` and `Stats` because
"the queue has those." The consumer never calls them, so they are pure blast
radius and a least-privilege leak.

Fix: the interface is exactly the methods invoked at that seam. A consumer that
only takes jobs declares one method, `Take`.

### Implementing a fat interface with panic/error stubs to use one method

Wrong: a type that provides ten stub methods returning "not implemented" just so
it satisfies a fat interface whose one real method it needs. The stubs are noise
and a runtime landmine.

Fix: adapt the concrete type to a narrow interface at the call site instead of
forcing the type to satisfy the whole fat shape.

### Returning an interface from the constructor

Wrong: `func NewStore() OrderStore` where `OrderStore` is declared in the
implementation package. This is interface pollution — callers are coupled to a
shape they did not choose and cannot narrow.

Fix: return the concrete `*pgStore`; let each consumer declare the small
interface it needs in its own package (accept interfaces, return structs).

### Over-segregating into single-method-interface soup

Wrong: splitting a type with one implementation and one caller into five
one-method interfaces "for cleanliness." This is indirection and naming churn
with no payoff.

Fix: wait for a real second implementation or a real test seam. Until then, the
concrete type is fine. Segregation earns its keep only when it removes a concrete
cost.

### Confusing composition with a fat interface

Wrong: hand-writing a fresh six-method interface when a consumer needs several
roles, duplicating method signatures that already exist as small interfaces.

Fix: embed the existing small interfaces (`Reader`, `Writer`, `Closer`) to
compose the shape you need, exactly as `io.ReadWriteCloser` does.

### Forgetting the compile-time satisfaction assertion

Wrong: relying on a distant call site to catch that `*pgStore` no longer
satisfies `OrderReader` after a refactor drops a method. The error surfaces far
from its cause.

Fix: add `var _ OrderReader = (*pgStore)(nil)` next to the type. A dropped method
now fails to compile at the type definition, where the mistake is.

### Misplacing context.Context in the narrowed signature

Wrong: putting `ctx context.Context` as the second parameter, or storing it in a
struct field, when you define the narrowed method signatures. It breaks the
standard port shape reviewers expect and defeats propagation.

Fix: `context.Context` is always the first parameter of a method that takes one,
and is never stored in a struct.

### Leaking write capability past the narrow handle

Wrong: handing a consumer a `Getter` but also exposing the underlying `*Cache`
somewhere it can reach, so the least-privilege intent is defeated.

Fix: the narrow type must be the *only* handle the consumer holds. If it can get
to the concrete type, the segregation is cosmetic.

Next: [01-segregated-job-queue.md](01-segregated-job-queue.md)
