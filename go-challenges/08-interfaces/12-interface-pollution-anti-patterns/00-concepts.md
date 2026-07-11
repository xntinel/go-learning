# Interface Pollution and Interface Design Discipline — Concepts

Interface pollution is one of the most common structural defects in real Go
backends. A repository or service layer sprouts a fat, producer-side interface
"for mocking" or "for a future database"; every consumer takes an interface it
barely uses; a `NewThing` constructor returns an interface instead of a concrete
type. The codebase pays a permanent indirection cost — an extra hop the reader
must trace, no jump-to-definition on the concrete method, a second surface to
keep in sync — and gets no abstraction benefit in return, because there is only
ever one implementation and one caller. This lesson treats interface design as a
production concern: where interfaces belong, how narrow they should be, when a
generic replaces one entirely, and the concrete bugs that ship when the judgment
is wrong. Read this file once; each of the ten exercises that follows is a
self-contained backend artifact — a repository, an HTTP handler, an audit
shipper, a cache, a payment seam — where you make the concrete-vs-interface,
narrow-vs-fat, generic-vs-boxed call the way it actually comes up in review.

## Concepts

### What interface pollution actually is

An interface earns its place when it buys you something: a second real
implementation, a seam you must fake in a test (an external service, a clock, a
filesystem), or genuine runtime polymorphism over differing behavior. Interface
pollution is adding an interface when none of those hold. The tell is
mechanical, not aesthetic. If an interface has exactly one implementation and
exactly one caller and no test substitutes for it, it is pure indirection. You
have not decoupled anything — there is nothing on the other end to swap in — you
have only inserted a layer that the reader must step through and the compiler
must dispatch through. "Decoupling" is the word people reach for; until a second
implementation or a real test seam exists, it is a misnomer.

The cost is not hypothetical. With a concrete `*Store`, reading `s.Get(id)` and
pressing go-to-definition lands you on the code that runs. With an interface
`Store`, it lands you on a method signature, and you must then find which of the
(one) implementations is wired in. Every method call through the interface is a
dynamic dispatch the compiler cannot inline. And the interface is a second thing
to maintain: add a method to the concrete type and you must add it to the
interface, or the interface silently omits behavior callers might want.

### The bigger the interface, the weaker the abstraction

This Go proverb is the load-bearing idea. A one-method interface like `io.Writer`
is a powerful abstraction precisely because it demands almost nothing: anything
that can turn a `[]byte` into a side effect satisfies it, so files, buffers,
gzip streams, hashers, and network connections all qualify for free. A fat
interface with eight methods (`Get`, `Put`, `Delete`, `List`, `Stats`, `Reset`,
`Backup`, `Restore`) is a weak abstraction: almost nothing satisfies it without
effort, a second implementation must supply all eight even where two are
meaningless, and every consumer that accepts it is coupled to methods it never
calls. Fat interfaces breed do-nothing stub methods — a `Backup` that returns
`nil` and a `Restore` that returns `nil`, present only to satisfy the type. Those
stubs are noise that lies: they advertise a capability the type does not have.

### Interfaces belong in the consumer, not the producer

This is the single rule from Go Code Review Comments that prevents most
pollution. The package that USES a value should declare the interface, listing
only the methods it actually calls. The package that IMPLEMENTS the value should
return a concrete type. Because Go interfaces are satisfied implicitly, the
producer does not need to know the interface exists — it just returns `*Store`,
and any consumer is free to describe the subset it needs with its own tiny
interface. This inverts the usual object-oriented habit of defining the interface
next to the implementation, and it is better for two concrete reasons. First, the
consumer's interface is naturally minimal, because it lists exactly what the
consumer calls and nothing else. Second, the producer stays extensible: it can
add methods to `*Store` without breaking anyone, because no shared interface
enumerates the method set and no consumer is coupled to a method it does not use.

### Accept interfaces, return structs

The corollary for function and constructor signatures: take the narrowest
interface you operate on, and return a concrete type. Accepting an interface lets
callers pass any implementation, including a fake in a test, and coupling them
only to the methods you call. Returning a concrete `*Server` (not a `Server`
interface) means callers can use every method the type has, including ones you
add later, and they get real go-to-definition. A `NewThing` that returns an
interface hides the type's methods behind whatever the interface happens to
enumerate and freezes the API at that set. Return the struct; let each caller
narrow it if it wants to.

### Interface Segregation: split fat interfaces into roles

When two consumers use disjoint slices of a fat interface — a read handler calls
only `Get`, a write handler calls only `Put` and `Delete` — give each its own
role interface (`Reader`, `Writer`) and let each handler's constructor take just
its role. Compose roles by embedding when a consumer genuinely needs both:
`type ReadWriter interface { Reader; Writer }`. The concrete `*Store` still
implements everything; segregation is about what each consumer is allowed to
reach, not about splitting the implementation. The payoff is enforced least
privilege: a read handler wired to a `Reader` physically cannot call `Put`,
because the method is not in its view of the dependency, so a whole class of
mistake becomes a compile error rather than a code-review comment.

### Prefer stdlib interfaces over bespoke ones

Before writing `type LineSink interface { WriteLine(string) error }`, check
whether `io.Writer` already fits — it almost always does for anything that emits
bytes. Reusing a stdlib interface (`io.Reader`, `io.Writer`, `io.Closer`,
`fmt.Stringer`, `sort.Interface`) means the entire ecosystem already implements
it: you inherit files, `bytes.Buffer`, `gzip.Writer`, `bufio.Writer`, network
connections, and test spies as adapters at zero cost. A bespoke one-method
interface throws all of that away — nobody else implements `WriteLine`, so every
adapter and every fake must be written by hand, and your type cannot be composed
with the standard library. The bespoke interface is not more expressive; it is
just less connected.

### The typed-nil interface trap

An interface value has two words: a type and a value. It is `nil` only when BOTH
are unset. A nil pointer of a concrete type, stored into an interface, is not a
nil interface — it is `(type=*RepoError, value=nil)`, which is non-nil. So this
ships to production:

```go
func find() *RepoError { return nil }        // returns a nil *RepoError

func handler() {
	var err error = find()                   // err is (*RepoError, nil): NON-nil
	if err != nil {                          // fires on the SUCCESS path
		http.Error(w, "500", 500)            // returns 500 despite success
	}
}
```

The `err != nil` guard is true even though nothing failed, and a healthy request
gets a 500. The fix is to make the function's return type the interface and
return an explicit untyped `nil`:

```go
func find() error { return nil }             // explicit nil interface: truly nil
```

or, if you must build the concrete value conditionally, return `nil` explicitly
on the success path rather than a typed nil pointer. This is the canonical Go FAQ
bug; it costs a production incident the first time each team meets it.

### Generics vs interfaces

Sometimes the only reason an interface exists is to be generic over types — a
container, a cache, a result set that holds "some value type" and boxes it as
`any`. Before Go had type parameters, `map[string]any` with a runtime type
assertion at every `Get` was the only option, and it moved a type error from
compile time to a runtime panic. Since Go 1.18 (and comfortably in 1.24+), a type
parameter does the job with compile-time safety: `Cache[V any]` returns a `V`, no
assertion, no boxing, no panic. Use a type parameter when you are abstracting
over the TYPE of a value while the behavior is identical; use an interface when
you are abstracting over BEHAVIOR that genuinely differs between implementations
(that is real runtime polymorphism, which generics do not replace).

### The cost/benefit test for adding an interface

Before you write `type X interface`, ask three questions. Is there a second real
implementation today? Is there a boundary you must fake in a test — an external
service, a clock, a filesystem, a network client? Is there genuine polymorphism,
where different implementations do different things at runtime? If yes to any,
the interface earns its keep; make it as narrow as the consumer needs. If no to
all three, the concrete type is the better design, and it is trivially testable —
a single implementation needs no interface to be exercised directly. "In case we
swap the database later" fails this test: the speculative interface ages badly
because the real second implementation, when it finally arrives, rarely matches
the method set you guessed.

## Common Mistakes

### Defining a Repository interface with one implementation and calling it decoupling

Wrong: a `Repository` interface whose only implementer is `MemoryRepository`,
justified as "decoupling". Nothing is decoupled — there is no second thing to
swap in. Fix: return the concrete `*MemoryRepository`; introduce an interface
when a second implementation or a real test seam appears, and put it in the
consumer that needs it.

### Declaring the interface in the producer package "for mocking"

Wrong: the package that implements `Store` also declares `type Store interface`,
so the interface grows lockstep with the implementation and every consumer is
coupled to the full method set. Fix: the producer returns concrete
`*Store`; each consumer declares its own tiny interface with just the methods it
calls, so the producer can add methods without editing any consumer.

### Returning an interface from a constructor

Wrong: `func NewCache() Cache` where `Cache` is an interface, hiding the concrete
type's methods behind whatever the interface enumerates and freezing the API.
Fix: `func NewCache() *cache` returns the concrete type; callers use every method
and get real go-to-definition, and any caller that wants an abstraction narrows
it locally.

### One fat Service interface with every method

Wrong: a single `Service` interface with `Get/Put/Delete/List/Stats/Reset/Backup/
Restore`, so consumers depend on methods they never call and new implementations
stub out `Backup`/`Restore`. Fix: split into role interfaces (`Reader`,
`Writer`) declared by the consumers that use them; the concrete type still
implements everything.

### Returning a typed nil pointer as an error

Wrong: a function returning a concrete `*RepoError` (or assigning one to an
`error` variable) whose "no error" value is a nil `*RepoError` — the resulting
interface is non-nil and `err != nil` fires on success. Fix: make the return type
`error` and return an explicit `nil` on the success path.

### Writing a custom one-method interface when io.Writer fits

Wrong: `type LineSink interface { WriteLine(string) error }` when the thing is
"write bytes somewhere" — you discard every existing `io.Writer` adapter and must
hand-roll fakes. Fix: accept `io.Writer`; files, buffers, gzip, and network
connections plug in for free.

### Reaching for any + type assertions to make a container generic

Wrong: `map[string]any` with `v.(User)` at every read, moving a type error to a
runtime panic. Fix: a type parameter — `Cache[V any]` — gives the same
flexibility with compile-time safety and no assertion.

### Adding an interface "in case we swap the database later"

Wrong: a speculative abstraction for a hypothetical future. It ages badly because
the guessed method set rarely matches the real second implementation. Fix: wait
for the actual second implementation or the actual test seam; design the
interface from the real requirement, not a guess.

Next: [01-before-fat-service-interface.md](01-before-fat-service-interface.md)
