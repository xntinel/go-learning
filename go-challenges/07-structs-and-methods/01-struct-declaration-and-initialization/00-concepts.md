# Struct Declaration, Initialization, and Value Semantics for Backend Domain Types — Concepts

Structs are the backbone of every backend service. They are the domain entities
a repository loads and saves, the config objects that shape a server at startup,
the DTOs that cross the wire boundary, the keys in a cache map, and the hot
per-request state a handler threads through. The decisions a senior engineer
makes about them are almost never "how do I declare a field." They are: is this
type usable at its zero value or does it need a constructor; do I pass it by
value or by pointer, and does it hide a `sync.Mutex` that `go vet` will flag if I
copy it; is it comparable so it can be a map key, or must I write an `Equal`; how
do I share `ID` and timestamps across entities without inheritance; and does
field ordering matter when I allocate millions of these per second. This file is
the conceptual foundation for the modules that follow. Each module is a
production artifact — a repository entity, a server config, an in-memory
registry, a cache key, an event record, an LRU node — not a syntax demo.

## Concepts

### A struct is a composite value type, and value semantics are the whole story

A struct groups named fields. What matters far more than the syntax is that a
struct is a *value*: assigning it, passing it to a function, returning it, and
ranging over a slice of them all **copy the entire struct**, field by field. The
`==` operator compares two structs field by field. This single fact is the root
of both the safety and the bugs in this chapter.

The safety: because a copy is independent, a function that takes a `User` by
value cannot mutate the caller's `User`. There is no aliasing to reason about.

The bugs: if the struct contains a `sync.Mutex`, copying it copies the lock's
internal state, so the copy and the original guard *different* memory and mutual
exclusion silently breaks. If the struct contains a slice header, a copy shares
the same backing array, so mutating the slice's elements through the copy is
visible through the original even though the struct "was copied." And a function
that returns a copy, whose caller mutates it expecting the change to be seen
elsewhere, is chasing a bug that only exists because the semantics were never
decided on purpose.

### The zero value is a design decision, not an accident

Every struct has a zero value: each field set to its own zero. The question a
senior engineer answers deliberately is whether that zero value is *usable*.

`bytes.Buffer`, `sync.Mutex`, and `sync.WaitGroup` are the canonical examples of
types designed so that `var b bytes.Buffer` is immediately ready — no `New`
required — because the type has no invariant to establish. When your type is like
that (a counter registry, an accumulator, a set backed by a lazily-initialized
map), prefer a useful zero value and skip the constructor; forcing a `New` on a
type whose zero value is already correct is ceremony for nothing.

Require a constructor only when construction must *validate* an input or
*establish an invariant* the rest of the code then relies on. A `User` that must
have a non-empty ID, a `time.Time` stamped in UTC, whitespace trimmed — that type
has invariants, so `New` earns its place and callers must not skip it. The
failure mode is assuming a useful zero value for a type that actually has an
invariant, so callers construct it with a bare literal and operate on a
half-built value.

### Named-field literals over positional

`User{ID: "u1", Name: "Alice"}` names its fields; `User{"u1", "Alice"}` relies on
declaration order. The positional form is fragile: the day someone inserts a
field or reorders two of the same type, positional literals compile fine and
assign the wrong values silently. Named literals are self-documenting, let you
omit zero fields, and survive reordering. `go vet`'s `composites` analyzer flags
unkeyed literals of structs imported from other packages for exactly this reason.
Composite literals also initialize embedded and nested structs inline, which is
how you build a whole config tree or an entity-with-base in one expression.

### Exported fields are the public API; unexported fields are the internals

A field whose name starts with a capital letter is exported and visible to other
packages; a lowercase name is package-private. This is the encapsulation boundary
of a type. Exported fields are a contract you cannot change without breaking
callers, and any invariant an exported field participates in can be violated by
any package that can name the type. Keep fields that back an invariant (a
password hash, an internal cache map, a lazily-initialized structure) unexported
and expose behavior through methods, so the invariant is enforced at the package
boundary rather than trusted to every caller.

### Value vs pointer receivers is a semantic choice, not a micro-optimization

Choose a pointer receiver `(*T)` when the method must mutate the receiver, or when
`T` contains a `sync.Mutex` or any other type that must not be copied, or when `T`
is large enough that copying it per call is genuinely wasteful. Choose a value
receiver `(T)` for small, immutable types where copying is cheap and aliasing is
undesirable. The rule that trips people up: **do not mix** value and pointer
receivers on the same type. Mixing them muddies the method set and creates the
situation where a value of `T` satisfies an interface but `*T` behaves
differently (or vice versa). A type with a pointer-receiver method is only in the
method set of `*T`, not `T`, so a `T` value cannot satisfy an interface that
requires that method — you need a `*T`.

### Comparability decides whether a struct can be a map key

A struct is comparable — valid for `==`, usable as a map key — only if **every**
field is comparable. Slices, maps, and functions are not comparable: a struct
containing one of them cannot be compared with `==` at all (a direct `==` is a
compile error, and comparing it through an `interface{}` panics at runtime).
Floats are comparable but treacherous as keys because `NaN != NaN`: a key written
with a `NaN` field can never be read back, so the entry leaks forever. When the
natural key is uncomparable or float-bearing, derive a canonical string key (for
example by formatting the fields) instead of using the struct directly. When all
fields are comparable integers and strings, the struct itself is a perfect,
hashable composite key.

### Composition via embedding, not inheritance

Embedding a struct — declaring a field with a type but no name — promotes that
type's fields and methods to the outer struct. `Order` that embeds `Entity` can
be written `o.ID` and `o.Touch()` even though `ID` and `Touch` are declared on
`Entity`. This is how backend code shares a base of `ID` and timestamps across
domain types without an inheritance hierarchy. Two rules matter. First, you still
initialize the embedded struct explicitly in a composite literal:
`Order{Entity: Entity{ID: "o1"}}`. Second, an outer field with the same name as a
promoted one *shadows* it — the outer field wins on `o.Field`, and you reach the
inner one explicitly through the embedded type name, `o.Entity.Field`. Promotion
is a lookup rule, not copying, so a promoted pointer-receiver method mutates the
embedded value in place.

### Field order affects size because of alignment padding

The compiler aligns each field to a boundary that matches its size, inserting
padding bytes so that, for example, an `int64` starts on an 8-byte boundary. A
struct laid out `bool, int64, bool, int32` wastes bytes on padding between the
small fields and the large one; the same fields ordered largest-alignment-first
pack tightly and the struct is smaller. This matters only on hot,
high-cardinality structs — a request event allocated millions of times per second
— and the exact sizes are platform-dependent, so measure with `unsafe.Sizeof`,
`unsafe.Alignof`, and `unsafe.Offsetof` rather than guessing. For structs that
must match a C ABI across FFI or syscall boundaries, mark them with the
`structs.HostLayout` field so the compiler guarantees the host layout.

### Self-referential structures go through pointers

A linked list node, a tree node — any struct that refers to its own type — must
do so through a pointer. A field of the struct's own type *by value* would make
the type contain itself, giving it infinite size; the compiler rejects it as an
invalid recursive type. A `*Node` field is a fixed-size pointer, so the recursion
is fine, and `nil` is the natural zero value marking "no next" or "empty list."
This is the shape behind the intrusive doubly-linked list inside an LRU cache.

### Value equality is not identity

Two structs can be field-for-field equal (`a == b` is true) and still be distinct
instances at different addresses. Value equality asks "are the fields equal";
identity asks "are these the same object," which you answer by comparing
`*T` pointers, not the values. A cache that stores `*Node` cares about identity
when it unlinks a specific node; a test that checks a constructor is
deterministic cares about value equality. Know which question you are asking.

## Common Mistakes

### Using `==` on a struct that contains a slice, map, or func field

Wrong: `if a == b` where the struct has a `[]string` field. Direct `==` fails to
compile; the same comparison performed through an `interface{}` (for example as a
map key looked up dynamically) panics at runtime. Fix: write an `Equal` method or
use `reflect.DeepEqual` — and keep `DeepEqual` off the hot path, since it is
reflection-based and slow.

### Copying a struct that embeds a sync type

Wrong: passing a struct that embeds a `sync.Mutex` by value, or ranging over a
`[]T` of such structs by value. The lock is copied and mutual exclusion breaks
silently. `go vet`'s `copylocks` analyzer catches it. Fix: use a pointer receiver
and pass `*T` everywhere; range with an index (`for i := range s { use &s[i] }`)
rather than by value.

### Assuming a useful zero value for a type that has an invariant

Wrong: giving a type an invariant (must be validated, must be non-nil) but no
enforced constructor, so callers build it with a bare literal and skip the check.
Conversely, forcing a `New` on a type whose zero value is already correct adds
ceremony for nothing. Fix: decide explicitly — useful zero value *or* mandatory
constructor — and make the wrong one hard to reach (unexported fields force the
constructor).

### Positional struct literals

Wrong: `User{"u1", "Alice"}`. It breaks silently the day a field is inserted or
reordered. Fix: use named-field literals; heed `go vet`'s `composites` warning on
unkeyed literals of imported structs.

### A float or uncomparable field in a map key

Wrong: a struct with a `float64` field used as a map key, losing entries because a
`NaN`-written key can never be read back, or a struct with a slice field used as a
key at all (which will not compile). Fix: derive a canonical string key, or design
the key type out of comparable integer and string fields only.

### Accidentally shadowing a promoted embedded field

Wrong: giving the outer struct a field with the same name as one promoted from an
embedded struct, then reading the wrong value because the outer field wins. Fix:
rename to avoid the collision, or access the inner field explicitly via the
embedded type name (`o.Entity.ID`).

### Confusing value copy semantics with shared state

Wrong: mutating a returned struct *copy* and expecting other holders to see it, or
returning a `*T` and letting callers mutate state that was meant to be immutable
after construction. Fix: decide value-vs-pointer semantics on purpose and document
it at the constructor.

### Embedding a struct in itself by value

Wrong: a `next Node` field on `Node`, which hits an "invalid recursive type"
compile error because the type would have infinite size. Fix: make the field a
pointer, `next *Node`.

Next: [01-user-entity-and-validating-constructor.md](01-user-entity-and-validating-constructor.md)
