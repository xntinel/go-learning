# Anonymous Structs and Embedding in Production Go — Concepts

Embedding and anonymous structs are the two composition primitives a Go backend
engineer reaches for every day. Middleware that wraps `http.ResponseWriter` to
capture a status code, a metrics decorator that instruments only the write path
of a store, a base repository that shares `Exists`/`Count` helpers across every
concrete repository, a forward-compatible service stub that satisfies a growing
interface for free, a one-off webhook DTO that never deserves a package-level
name — all of these are embedding or anonymous structs doing quiet, load-bearing
work. They are also the source of a specific family of production failures:
`sync.Mutex` embedded into an exported type leaks `Lock`/`Unlock` onto your
public API, a nil pointer-embed panics the first time a promoted method is
called, two embeds that expose the same name silently refuse to compile, and
JSON marshaling flattens promoted fields into the wire shape whether you meant it
to or not. This file is the conceptual foundation; read it once and the ten
independent exercises that follow each become a shippable artifact you can reason
through end to end.

## Concepts

### An anonymous struct is a struct type with no name

`type Op struct { Op, Path string }` is a named struct. `struct { Op, Path string }`
written inline is an *anonymous* struct: a struct type with no identifier, usable
directly as a variable, a field, a function parameter, or a slice element. It is
the right tool for a one-off shape that would only add noise if it were promoted
to a package-level type: the local DTO a single HTTP handler decodes a request
into, a provider-specific webhook payload, the response envelope written back by
one endpoint, and the rows of a table-driven test. The discipline is scope: an
anonymous struct belongs *inside* a function or a narrow field, never on the
exported surface. If a caller in another package needs to name the type, it must
be a named type — an anonymous struct cannot be referred to by name, so returning
or exporting one leaves the caller unable to declare a variable of that type.

### Embedding places a type as a field with no field name

Write a type inside a struct with no field name and you have *embedded* it:

```go
type Server struct {
	UnimplementedService // embedded: no field name
	db *sql.DB           // named field
}
```

The compiler *promotes* the embedded type's exported fields and methods (and its
unexported ones, within the same package) so that `outer.X` is shorthand for
`outer.Embedded.X`. The embedded value never disappears: it is still reachable by
its type name as `outer.UnimplementedService`, which is how you disambiguate and
how you call the inner method when an outer method shadows it.

### Embedding is composition with syntactic sugar, not inheritance

This is the single most important thing to internalize, and the source of the
most bugs when it is misunderstood. Promotion is *only* a shorthand for reaching
through the embedded field. There is no subtype relationship: `*Server` is not a
`UnimplementedService`, you cannot assign one to the other, and there is no
virtual dispatch. If the outer type defines a method with the same name as a
promoted one, the outer method *shadows* — it does not *override*. Concretely: if
`Embedded.Do()` internally calls `e.Help()`, and the outer type defines its own
`Help()`, the embedded `Do()` still calls `Embedded.Help()`, never the outer
`Help()`. Go has no back-dispatch from an embedded method into the outer type. A
same-named outer method hides the promoted one at the outer type's own selectors;
the inner method remains callable through the embedded field.

### Embedding an interface into a struct is the decorator idiom

Embed an *interface* in a struct and the struct satisfies that interface for free
by forwarding every call through the embedded value. You then override only the
methods you care about, and the rest pass through unchanged. This is exactly how
a status-capturing `http.ResponseWriter` wrapper works (override `WriteHeader`
and `Write`, inherit `Header`), how a metrics decorator over a `Store` works
(override `Put`, forward `Get`/`Delete`), and how the standard library layers
readers and writers. The footgun is the flip side of the convenience: if the
embedded interface value is left nil, any *non-overridden* promoted call
dereferences a nil interface and panics. The decorator constructor must set the
embedded interface to a real value.

### Embedding a default implementation gives forward compatibility

The `grpc-go` `UnimplementedServer` pattern is embedding used deliberately for
API evolution. Define an interface with several methods and a default type whose
methods all return a "not implemented" error; a concrete handler embeds that
default and implements only the methods it supports. The promoted defaults fill
every gap, so the handler satisfies the whole interface. The payoff is forward
compatibility: when a new method is added to the interface, you add it to the
default type once, and every existing implementer keeps compiling because the
promoted default covers the new method. Without the embedded default, adding an
interface method is a breaking change for every implementer at once.

### Selector resolution is shallowest-depth-wins, ties are ambiguous

When you write `outer.X`, the compiler searches for `X` at increasing embedding
depth and takes the *shallowest* match. A name declared directly on the outer
type (depth 0) shadows any promoted one (depth 1+). But if two embedded types at
the *same* depth both expose `X`, a bare `outer.X` is an *ambiguous selector* and
is a compile error — not a silent pick, and not a runtime surprise. You resolve
it two ways: qualify explicitly (`outer.EnvConfig.Timeout`) to name which one you
mean, or declare `X` on the outer type itself so the depth-0 declaration wins and
imposes your precedence order. Merging two config sources that both carry a
`Timeout` is the canonical case.

### JSON marshaling flattens promoted fields by default

`encoding/json` treats an embedded struct's promoted fields as if they were
declared directly on the parent: they *flatten* into the top-level JSON object.
A `json` tag on the embedded field overrides this and forces the value to *nest*
under that tag's name. A named (non-embedded) struct field always nests under its
key. So three shapes give three wire formats, and the difference is deliberate
design, not an accident: embed with no tag to flatten, embed with a tag or use a
named field to nest. Promotion also causes JSON key *collisions* to resolve by
the same shallowest-wins rule — a parent field and a promoted field with the same
JSON name means the promoted one is dropped from the output. Decide the wire
shape on purpose.

### Value embedding copies; pointer embedding shares and can be nil

Embedding `T` by value stores an independent copy: mutating the inner value of
one outer value does not affect a copy of that outer value, and the zero value is
safe to use. Embedding `*T` stores a pointer: two copies of the outer struct
share one inner value, so a mutation through either is visible through the other —
and if the pointer is nil, calling a promoted method dereferences nil and panics.
Pointer embedding therefore demands a constructor that guarantees the embedded
pointer is non-nil before any promoted call can run. Choose value embedding for
independent, self-contained inner state; choose pointer embedding when the inner
value is genuinely shared, and pay for it with a constructor.

### Embedding a mutex leaks lock control onto the public API

`sync.Mutex` and `sync.RWMutex` have exported `Lock`/`Unlock` methods, so
embedding one promotes those methods. On an *unexported* type that is a common,
accepted shorthand. On an *exported* type it is a design error: external callers
can now call `yourValue.Lock()` and `yourValue.Unlock()` and violate the
invariants the mutex was meant to protect — deadlock it, unlock it from the wrong
goroutine, or hold it across a call that was supposed to be brief. The rule for
exported types is to make the mutex an *unexported named field*
(`mu sync.RWMutex`), never an embedded one, so the lock stays a private
implementation detail.

### Method sets differ between value and pointer embedding

The method set of an embedded `T` promotes `T`'s value-receiver methods to both
the outer value and the outer pointer, and `T`'s pointer-receiver methods only to
the outer pointer. Embedding `*T` promotes all of `T`'s methods (value and
pointer receiver) to both the outer value and the outer pointer, because you
already hold a pointer. The practical consequence: if an interface requires a
method that has a pointer receiver on the embedded type, only the *outer pointer*
satisfies that interface when you embed by value — a value of the outer type does
not. This bites when you pass `outerValue` where an interface is expected and the
compiler reports the value does not implement the interface, even though
`&outerValue` does.

## Common Mistakes

### Embedding several distinct collaborators into one struct

Wrong: `type Service struct { *Logger; *DB; *Cache }`. Three embedded pointers
promote a jumble of methods, and a reader of `s.Query(...)` cannot tell which
collaborator it came from — nor can they see the collision risk until it bites.

Fix: name the fields (`logger *Logger; db *DB; cache *Cache`). Reserve embedding
for the case where the inner type genuinely *is* the outer type's behavior (a
decorator over one interface, one shared base), not for wiring up a bag of
distinct dependencies.

### Exporting or returning an anonymous struct type

Wrong: a public function returning `struct { ID, Name string }`. The caller in
another package cannot name the type to hold the result cleanly.

Fix: give the type a name when it crosses a package boundary. Keep anonymous
structs inside functions and narrow field scopes where no one else needs to name
them.

### Expecting a same-named outer method to override via virtual dispatch

Wrong: defining `Outer.Validate()` and assuming an embedded method that calls
`Validate()` will now run the outer version. Go only shadows; the embedded method
calls the embedded `Validate()` and never dispatches back into the outer type.

Fix: if you need the outer behavior inside the inner method, pass it in
explicitly (a function, an interface parameter) — do not rely on inheritance-style
back-dispatch that Go does not have.

### Embedding sync.Mutex in an exported type

Wrong: `type Cache struct { sync.Mutex; ... }` exported from a package. Callers
can now `cache.Lock()`/`cache.Unlock()` and corrupt the cache's invariants.

Fix: `type Cache struct { mu sync.RWMutex; ... }` — an unexported named field.
The lock is an implementation detail, not part of the contract.

### Leaving a pointer-embedded field nil

Wrong: `w := &Wrapper{}` where `Wrapper` embeds `*Logger`, then `w.Log("x")`.
The promoted call dereferences a nil pointer and panics in production.

Fix: a constructor that always sets the embedded pointer to a real value, so
every promoted call is safe.

### Assuming two embeds with a shared name just work

Wrong: embedding `FileConfig` and `EnvConfig` (both with a `Timeout`) and then
writing `cfg.Timeout`. That bare selector is an ambiguous-selector compile error.

Fix: qualify (`cfg.EnvConfig.Timeout`) or declare `Timeout` on the outer type to
impose a precedence order.

### Being surprised that promoted fields flatten into JSON

Wrong: embedding a `Meta` struct and expecting `{"meta":{...}}`, then finding the
meta fields flattened to the top level (or colliding with a parent field).

Fix: control the wire shape deliberately — a `json` tag on the embedded field or
a named field forces nesting; flattening is the default and must be a choice.

### Embedding an interface for a decorator but forgetting to override

Wrong: embedding a `Store` to "instrument it" but never redefining `Put`, so the
wrapper forwards everything unchanged and records nothing.

Fix: the whole point of embedding the interface is to override the one or two
methods you care about; verify the override actually intercepts by testing that
the inner store still sees the call and the metric moved.

### Relying on a value-embed to satisfy a pointer-receiver interface

Wrong: passing `outerValue` where an interface is expected when the required
method is promoted from a value-embedded type but has a pointer receiver. Only
`*outer` has that method in its set.

Fix: pass `&outerValue`, or embed `*T` if the outer value must satisfy the
interface.

Next: [01-json-patch-anonymous-meta.md](01-json-patch-anonymous-meta.md)
