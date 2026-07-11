# Method Sets and Addressability — Concepts

Method sets look like a footnote in the spec and turn out to be the single rule
that governs whether your code even compiles at the wiring layer, whether a
mutex-guarded type stays correct under load, and whether an error return means
what you think it means. Every bug in this lesson is the same rule wearing a
different costume: the "cannot use `*T` (method has pointer receiver)" failure
when you plug a `*Repository` into a `Store` interface, the `copylocks` vet
failure when a stateful type is copied, the no-op batch update when you range
over struct values, the typed-nil error that fools `if err != nil`, and the
`String()` method that silently does not fire in a log line. Read this file
once and you have the model behind all ten modules that follow.

## Concepts

### The one asymmetry everything hangs on

A method declared with a value receiver, `func (t T) M()`, is in the method set
of both `T` and `*T`. A method declared with a pointer receiver,
`func (t *T) M()`, is in the method set of `*T` only. That asymmetry is the
root of the entire lesson. The reason is mechanical: given a `*T` you can always
recover a `T` by dereferencing, so a `*T` can call value methods; but given only
a `T` value there may be no address to form the `*T` that a pointer method needs.
So `*T` has every method `T` has, plus the pointer-receiver ones; `T` has only
the value-receiver ones.

A useful mental image: `*T`'s method set is a superset of `T`'s. Anything a `T`
can do, a `*T` can do; the reverse is not true.

### Interface satisfaction is a method-set superset test

A type satisfies an interface exactly when its method set is a superset of the
interface's method set. Combine that with the asymmetry above and you get the
rule that bites at dependency-injection time: if any method the interface
requires is implemented on a pointer receiver, then only `*T` satisfies the
interface — never `T`. `var s Store = MemStore{}` fails to compile with
"cannot use MemStore{} (value of type MemStore) as Store value ... method Save
has pointer receiver"; `var s Store = &MemStore{}` compiles. The fix is not to
discover this at wiring time but to pin it at compile time with a guard line,
`var _ Store = (*MemStore)(nil)`, next to the type. If the method set ever
drifts, that line fails the build immediately, at the type's definition, instead
of three packages away where you assemble your dependencies.

### Addressability, precisely

The compiler will let you call a pointer method on a value only if that value is
addressable, because it needs to synthesize `&v` for you. What is addressable:
a variable, a field of an addressable struct, an element of an array that is
itself addressable, a slice index expression `s[i]` (always, regardless of how
the slice was obtained), and the result of a pointer dereference `*p`. What is
NOT addressable: a map index expression `m[k]`, the return value of a function
or method call, a composite literal like `T{}`, a constant, and the value stored
in an interface. Everything strange in this lesson follows from that list.

### The auto-address and auto-deref sugar

`v.PtrMethod()` is shorthand the compiler expands to `(&v).PtrMethod()`, and it
compiles only when `v` is addressable. `p.ValMethod()` is shorthand for
`(*p).ValMethod()` and always works because dereferencing a pointer yields an
addressable value. This sugar is why calling a pointer method usually "just
works" on a local variable — but it silently stops working the moment the operand
is not addressable, and the error message is about addressability, not about the
method. That is the whole story behind the map trap and the slice-range trap
below.

### Why a map value blocks a pointer method

A map is allowed to rehash and move its entries in memory when it grows, so the
language forbids taking the address of a map element: `&m[k]` is a compile error.
Because `m[k].PtrMethod()` would require exactly that address (the auto-address
sugar needs `&m[k]`), it also fails to compile: "cannot call pointer method on
`m[k]`" / "cannot take address of `m[k]`". A slice element is different: the
backing array does not move under you within a slice's lifetime, so `&s[i]` is
legal and `s[i].PtrMethod()` compiles. The production consequence is direct: a
`map[string]Counter` where `Counter.Inc()` is a pointer method cannot be
incremented in place; you store `map[string]*Counter` (or hand back a `*Counter`
from a getter) instead.

### An interface value is a (type, value) pair

An interface value is not a bare pointer; it is a pair of a dynamic type and a
dynamic value. It is nil only when BOTH halves are nil. Assign a nil `*AppError`
to an `error` and the interface carries the type `*AppError` with a nil value —
so the interface itself is non-nil, and `if err != nil` fires even though the
underlying pointer is nil. This is the typed-nil trap, and it is why a function
that can fail should be declared to return `error` and return a literal `nil`
on success, never a typed nil pointer widened to the interface.

### Value receivers copy; that is sometimes right and sometimes fatal

Every call to a value method copies the receiver. For a small, immutable value
object — a `Money` of an int64 and a currency code, a `time.Time`, a coordinate —
that copy is cheap and correct, and it is what gives value semantics: methods
return new values and never mutate the caller's. For a type that carries a
`sync.Mutex`, `sync.WaitGroup`, `sync.Once`, or a large buffer, that copy is a
bug: copying a mutex splits it into two independent locks that guard nothing in
common, so mutual exclusion is lost. Go's `go vet` ships a `copylocks` analyzer
that flags a value receiver on a lock-bearing type and flags passing such a type
by value. The rule that falls out: a type containing a lock (or that has identity
semantics) is non-copyable, must declare all its methods on pointer receivers,
and must be passed only as `*T`.

### Method values and method expressions

`x.M` (where `x` is a value or pointer) is a method value: it binds the receiver
`x` right then and evaluates it, producing a `func(args)` with the receiver baked
in. This is exactly what makes `mux.HandleFunc("/users", h.HandleGet)` work — the
handler method value closes over the receiver `h`, so the router gets a plain
function that still routes to your struct's state. `T.M` or `(*T).M` is a method
expression: it produces a `func(receiver, args)` that takes the receiver as an
explicit first parameter. The two are equivalent bindings — `h.HandleGet(w, r)`
and `(*UserHandler).HandleGet(h, w, r)` do the same thing — but the method value
captures the receiver at the moment you write `x.M`, so later reassigning `x`
does not change what the stored function operates on.

### Method-set promotion through embedding

When a struct embeds another type, the embedded type's methods are promoted onto
the outer type, and which promotions land where follows the same asymmetry.
Embedding a value `Base` promotes `Base`'s value methods onto both `Outer` and
`*Outer`, but promotes `Base`'s pointer methods onto `*Outer` only. Embedding a
pointer `*Base` promotes all of `Base`'s methods (value and pointer) onto both
`Outer` and `*Outer`. This is what decides whether `Outer` or only `*Outer`
satisfies a handler interface, and it is the bridge into the embedding chapter
that follows.

### Ranging over a slice of structs copies each element

`for _, r := range records` binds `r` to a fresh copy of each element every
iteration; `r.Activate()` mutates the copy and the mutation is discarded when the
loop moves on — a silent no-op batch update. `for i := range records` followed by
`records[i].Activate()` mutates in place, because `records[i]` is an addressable
slice index expression and the auto-address sugar can form `&records[i]` for the
pointer method. A `[]*Record` sidesteps the issue entirely: the loop copies the
pointer, not the struct, and the method mutates the shared pointee under either
loop form.

### fmt checks the concrete value's method set at runtime

`fmt` and other reflection-driven code test at runtime whether the argument's
concrete type satisfies `Stringer` or `error`. If `String()` is declared on the
pointer receiver only and you pass a value to `fmt.Sprint`, the value's method
set lacks `String`, the check fails, and `fmt` falls back to the reflection dump
of the struct's fields — your carefully formatted label never appears in the log.
Declaring `String()` on the value receiver puts it in the method set of both the
value and the pointer, so it fires either way.

### The consistency rule

Pick one receiver kind for all methods of a type. Mixing value and pointer
receivers gives the type an inconsistent method set and makes interface
satisfaction and copy behavior hard to predict: half the methods are on `T` and
`*T`, half only on `*T`, so whether a plain `T` satisfies an interface depends on
which methods that interface happens to require. If any method needs a pointer
receiver (it mutates, or the type holds a lock), make every method a pointer
method.

## Common Mistakes

### Mixing value and pointer receivers on one type

Wrong: `Save` on a pointer receiver and `Get` on a value receiver of the same
`Store`. The method set is now inconsistent and whether `Store` (value) satisfies
an interface depends on exactly which methods it lists.

Fix: choose one receiver kind for the whole type. If any method mutates or the
type holds a lock, make them all pointer methods.

### Returning T from a chainable or mutating method

Wrong: `func (b *Builder) Where(s string) Builder`. The call returns a copy, so
the next `.Where` in the chain mutates the copy and the earlier state is lost.

Fix: return `*Builder` so every call in the chain mutates the same builder.

### Assuming T satisfies an interface implemented on *T

Wrong: passing a `MemStore` value where a `Store` is wanted and discovering the
"method has pointer receiver" error only when you wire dependencies.

Fix: put a `var _ Store = (*MemStore)(nil)` guard beside the type so the mismatch
fails the build at the definition, and pass `*MemStore`.

### Calling a pointer method on a map index expression

Wrong: `m[name].Inc()` where `m` is `map[string]Counter` and `Inc` is a pointer
method — "cannot call pointer method on m[name]", because `&m[name]` is illegal.

Fix: store `map[string]*Counter`, or fetch the `*Counter` from a getter and call
`Inc` on that.

### Giving a lock-bearing struct value receivers

Wrong: value receivers on a type that embeds `sync.Mutex`, or passing it by value.
The lock is copied and mutual exclusion silently breaks; only `go vet` catches it.

Fix: pointer receivers everywhere, pass as `*T`, and run `go vet` (copylocks).

### Returning a typed-nil pointer as an error

Wrong: `var e *AppError; ...; return e` widened to `error`. The interface is
non-nil (it carries the type `*AppError`), so `if err != nil` fires on success.

Fix: declare the function to return `error` and `return nil` on the success path.

### Ranging over a slice of structs by value and mutating the copy

Wrong: `for _, r := range records { r.Activate() }` — mutates a copy each time,
leaving the slice untouched.

Fix: `for i := range records { records[i].Activate() }`, or range over `[]*Record`.

### Declaring String() or Error() on the pointer receiver only

Wrong: `func (s *Status) String() string`, then logging a `Status` value — `fmt`
does not find `String` in the value's method set and dumps the raw struct.

Fix: declare `String()` on the value receiver so both `Status` and `*Status`
format.

### Embedding a value when the promoted method has a pointer receiver

Wrong: embedding `Base` (value) but the interface needs a pointer method of
`Base`; `Outer` then fails to satisfy the interface, only `*Outer` does.

Fix: embed `*Base`, or use `*Outer` everywhere and guard with
`var _ Handler = (*Outer)(nil)`.

Next: [01-fluent-query-builder-pointer-chaining.md](01-fluent-query-builder-pointer-chaining.md)
