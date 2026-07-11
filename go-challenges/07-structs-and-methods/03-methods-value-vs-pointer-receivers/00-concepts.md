# Methods: Value vs Pointer Receivers — Concepts

The receiver you put in front of a method — `func (c Counter)` versus
`func (c *Counter)` — looks like a stylistic detail and is anything but. It
silently decides four production-critical properties of a type: whether a
mutation you write actually persists, whether the type satisfies an interface at
compile time, whether a hot-path handler copies a fat struct on every call, and
whether a callback you registered captured a live pointer or a frozen snapshot.
Senior backend engineers meet the consequences daily: a repository whose `Save`
takes a value receiver drops every write; a middleware registered as a method
value captures a stale copy of its service; a session struct stored in a map
cannot be mutated in place; a config type with mixed receivers behaves
inconsistently under interface dispatch. Every rule below is tied to one of those
observable failure modes — lost writes, a compile error, a data race, or wasted
allocations under load. Read this once and each of the independent exercises that
follows is a single rule made concrete.

## Concepts

### A value receiver operates on a copy; a pointer receiver operates on the original

This is the one fact that drives everything else. When you call `c.Get()` and
`Get` is declared `func (c Counter) Get() int64`, Go evaluates the receiver and
passes a *copy* of it into the method. Any field the method writes is written to
that copy, which is discarded when the method returns. When `Get` is declared
`func (c *Counter) Get() int64`, Go passes the *address* of the receiver, so the
method reads and writes the original. A value receiver is therefore copy-safe and
race-free by construction, but it cannot mutate; a pointer receiver can mutate,
but it aliases shared state.

The copy is taken at *call time*, evaluated from the receiver expression. That
timing matters later for method values.

### When to choose a pointer receiver

Choose a pointer receiver when any of these holds, and it is the more common
choice for the structs a backend service builds:

1. The method must mutate the receiver. A counter's `Inc`, a config's `Set`, a
   store's `Revoke` — if the change must be visible to the next caller, the
   method needs a pointer receiver. A value receiver here compiles, runs, and
   silently loses the write.
2. The struct is large enough that copying it on every call is wasteful. A value
   receiver copies every field, including any fixed-size array, on each call. In
   a hot path (a validator run per request, a middleware per call) that copy cost
   is measurable even when no mutation is needed.
3. The type contains or embeds something that must not be copied — a
   `sync.Mutex`, a `sync.WaitGroup`, an `atomic.Int64`, anything with a
   `Lock`/state that a copy would corrupt. `go vet`'s copylocks analyzer flags a
   value receiver on such a type, and copying a used lock is a real bug: the copy
   protects nothing.

### When a value receiver is the right answer

Value receivers are not a beginner's mistake to grow out of; they are the correct
choice for a small, immutable value object. A money amount, a coordinate pair, a
duration-like quantity — these are *values*, and the idiomatic design is a value
receiver whose methods RETURN a new value instead of pretending to mutate:
`func (m Money) Add(other Money) (Money, error)` produces a new `Money` and leaves
the receiver untouched. This gives copy-safe, race-free semantics for free and,
crucially, keeps the type comparable, so it can be a map key and support `==`.
The discipline is the same one the standard library uses for `time.Time` and
`time.Duration`. The mistake to avoid is a value receiver that tries to mutate;
the correct value-receiver method never assigns to a receiver field.

### Be consistent: one receiver kind per type

All methods on a type should share a single receiver kind. If any method needs a
pointer receiver, give every method a pointer receiver, even the read-only ones.
Mixing kinds produces a type whose method set and interface behavior depend on
whether you hold a `T` or a `*T`, which confuses both the compiler's interface
checks and every reader trying to tell which calls mutate. The Go Code Review
Comments state the rule directly: do not mix. Consistency also means the
constructor for a mutable type returns `*T`, not `T` — see below.

### Method sets: why *T can satisfy an interface that T cannot

Every type has a *method set*, and interface satisfaction is checked against it.
The rule is asymmetric and is the source of one of Go's most-Googled compile
errors:

- The method set of `T` contains only the methods with *value* receivers.
- The method set of `*T` contains both value-receiver and pointer-receiver
  methods.

So if a type has even one pointer-receiver method, only `*T` satisfies an
interface that requires it; a value `T` does not. Assigning a value into an
interface slice — `[]Repository{InMemoryRepo{}}` — then fails with
`InMemoryRepo does not implement Repository (method Save has pointer receiver)`.
This is not a bug in the interface or a missing method; it is the method-set rule.
The fix is to store the pointer: `[]Repository{&InMemoryRepo{}}`. A
`var _ Repository = (*InMemoryRepo)(nil)` line at package scope turns this into a
compile-time assertion that catches the wiring mistake at build time.

### Addressability: you cannot call a pointer method on a non-addressable value

When you write `x.Inc()` and `Inc` has a pointer receiver, Go silently rewrites it
to `(&x).Inc()`. That only works if `x` is *addressable* — if `&x` is legal. Local
variables and struct fields are addressable; two important things are not: the
result of a function call, and an element read directly out of a map. So
`m["k"].Inc()` does not compile when `m` is a `map[string]T` and `Inc` has a
pointer receiver, and even for a value method, `s := m["k"]; s.Field = v` mutates
a throwaway copy that never goes back into the map. The production consequence:
if you need to mutate structs held in a map through pointer-receiver methods,
store pointers — `map[string]*Session` — so the map element *is* the pointer and
the value it points at is addressable. Choosing `map[K]*V` over `map[K]V` for
mutable elements is a deliberate, load-bearing design decision, not a style tic.

### Method values freeze or capture the receiver

A *method value* binds a method to a specific receiver and yields a plain func:
`f := svc.Handle` gives an `f` you can store and call later with no receiver.
What `f` captured depends entirely on the receiver kind:

- Bound from a *value* receiver (`f := v.M`), the receiver is *copied and frozen*
  at the moment of binding. Later mutations to `v` are invisible when `f` runs.
- Bound from a *pointer* receiver (`f := p.M`), `f` captures the *pointer*, so
  later mutations through `p` remain visible when `f` runs.

Register a handler as a method value on a value receiver and every config change
made after registration is silently ignored at call time — the handler is reading
a snapshot from registration day. This is the callback-capture trap, and its fix
is a pointer receiver (or an explicit closure over the pointer).

### Method expressions make the receiver an explicit parameter

A *method expression* — `T.M` or `(*T).M` — turns a method into a plain func whose
first parameter is the receiver: `Service.Greet` has type `func(Service) string`,
and `(*Service).Greet` has type `func(*Service) string`. This is useful for
higher-order wiring (passing a method as a `func` that the caller supplies the
receiver to) and, for reasoning, it makes the copy-vs-pointer semantics visible in
the type signature: the value form takes the receiver by value, the pointer form
by pointer.

### A nil pointer receiver does not automatically panic

A pointer-receiver method can be called on a `nil` receiver and will run fine as
long as it does not dereference the nil pointer. This lets you *design nil as a
valid, usable zero value*: a `nil *FlagSet` can legitimately mean "no flags
configured", with `IsEnabled` returning `false` for everything by guarding
`if fs == nil` before touching any field. This is the classic nil-safe recursion
pattern: a nil `*Tree` node whose `Sum`/`Insert` methods treat nil as the empty
tree and return early. It is a deliberate choice,
not a happy accident — and its inverse choice is equally valid: a type that is
*not* meant to be nil-safe guarantees non-nil in its constructor so callers never
have to check. What you must not do is assume "nil receiver always panics"; it
panics only at the dereference, so a type meant to be nil-safe must guard every
field access.

### Constructors for mutable types return *T

Tying the rules together: a mutable type's constructor returns `*T`, not `T`.
Returning a value forces every caller onto a copy — they either cannot call the
pointer-receiver mutators at all (a non-addressable returned value) or they mutate
short-lived copies that vanish. Returning `*T` preserves a single identity for the
state across the whole program and lets callers invoke the mutators. It is the
same reason `New` returns `*Counter` and not `Counter`.

## Common Mistakes

### Declaring a mutating method with a value receiver

Wrong: `func (c Config) Set(k, v string) { c.data[k] = v }` on a struct field, or
worse `func (c Counter) Inc() { c.value++ }`. It compiles, it runs, and the write
vanishes because it lands on a copy. This is the single most common receiver bug
and it produces no error, just missing data.

Fix: use a pointer receiver — `func (c *Counter) Inc()` — for any method that must
persist a change. (A `map` field is subtle: a value receiver can *mutate the map's
contents* because the map header is copied but points at the same backing store;
but it cannot reassign the field or grow behavior consistently, and mixing that
with pointer methods is exactly the inconsistency the next mistake warns about. Use
a pointer receiver for the whole type.)

### Mixing value and pointer receivers on one type

Wrong: `Inc` on `*Counter` but `Get` on `Counter`. Now `*Counter` and `Counter`
have different method sets, interface satisfaction becomes position-dependent, and
readers cannot tell at a glance which calls mutate.

Fix: pick one kind for the whole type. If anything mutates, everything is a
pointer receiver.

### Returning a value from a mutable type's constructor

Wrong: `func New() Counter`. Callers get a copy; `c := New(); c.Inc()` may not
even compile (if the result is not stored in an addressable variable) or mutates
throwaway copies.

Fix: `func New() *Counter`. The pointer is the canonical form for mutable state.

### Not understanding the "method has pointer receiver" interface error

Wrong: `[]Repository{InMemoryRepo{}}` when `Save` has a pointer receiver, then
treating the compile error as mysterious.

Fix: it is the method-set rule — only `*InMemoryRepo` is in the interface. Store
`&InMemoryRepo{}`, and add `var _ Repository = (*InMemoryRepo)(nil)` to assert it
at compile time.

### Calling a pointer method on a value fetched from a map

Wrong: `sessions["id"].Touch()` on a `map[string]Session`, or
`s := sessions["id"]; s.LastSeen = now` — the first does not compile, the second
mutates a copy that is thrown away.

Fix: use `map[string]*Session` so the element is an addressable pointer, or read,
modify, and write the whole element back into the map.

### Copying a struct that contains a lock or atomic

Wrong: a value receiver (or a by-value return) on a type embedding `sync.Mutex` or
`atomic.Int64`. The copy has its own, unrelated lock state; the original's
guarantees are gone. `go vet` warns via copylocks and the warning is often
ignored.

Fix: pointer receivers everywhere on such a type, constructor returns `*T`, and
never return it by value. Keep `go vet ./...` green.

### Binding a callback as a method value on a value receiver

Wrong: registering `svc.Handle` where `Handle` has a value receiver, then mutating
`svc` and wondering why the registered handler ignores the change.

Fix: give `Handle` a pointer receiver (or close over `&svc` explicitly) so the
method value captures the live pointer, not a frozen snapshot.

### Assuming a nil receiver always panics

Wrong: adding a field access with no `if fs == nil` guard to a type you advertised
as nil-safe, reintroducing the panic on the nil path.

Fix: in a nil-safe type, guard the nil receiver before touching any field. In a
type that is not meant to be nil-safe, guarantee non-nil in the constructor.

### Using a pointer receiver "for performance" on a tiny immutable value

Wrong: `func (m *Money) Add(...)` on a two-field value object, purely because
pointers "feel faster". You lose comparability (no `==`, no map-key use) and invite
aliasing bugs where two references share mutable state.

Fix: small immutable values take value receivers and return new values. Reserve
pointer receivers for mutation, large structs, and non-copyable types.

### Believing a value receiver on a big struct is free

Wrong: a value receiver on a struct with a fixed-size array or many fields,
assuming the compiler elides the copy. It copies every field on every call.

Fix: for a large struct in a hot path, use a pointer receiver even if the method
does not mutate — measure it with a benchmark and `b.ReportAllocs()`.

Next: [01-mutable-metrics-counter.md](01-mutable-metrics-counter.md)
