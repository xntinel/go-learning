# Initialized Allocation with new(expr) — Concepts

Every Go backend team eventually writes the same three lines:

```go
func Ptr[T any](v T) *T { return &v }
```

or reaches for a vendor equivalent — `aws.String`, `aws.Int64`, Kubernetes'
`ptr.To`. The helper exists for one reason: Go had no way to take the address of
a value that is not stored in an addressable variable. `&yearsSince(t)`,
`&(a + b)`, `&"literal"`, and `&someCall()` are all compile errors. Go 1.26 makes
`new` accept an expression, so `new(expr)` allocates storage initialized to that
expression and hands back its address. It is the language-blessed replacement for
the copy-pasted `Ptr[T]` helper and the vendor `String`/`Int64` functions.

That is the surface. The reason a senior backend engineer should care is not the
syntax; it is that optional-pointer fields are the correct model for a problem
that pervades real backend work — distinguishing *absent* from *explicitly zero* —
and `new(expr)` removes the friction that made those fields annoying to populate.
This file is the conceptual foundation for the three independent exercises that
follow: a JSON PATCH DTO layer, a cloud-SDK request builder, and a layered
configuration resolver.

## The problem is addressability, not sugar

In Go you may take the address of an *addressable* operand: a variable, a pointer
indirection, a slice index, a field of an addressable struct. Function results,
arithmetic, conversions, and literals are deliberately not addressable, because
there is no variable behind them to point at. So the compiler rejects `&f()`,
`&(a + b)`, `&"lit"`, and `&t.Method()`.

Before 1.26 there were exactly two workarounds. Introduce a named temporary and
take its address:

```go
v := yearsSince(t)
p := &v
```

or funnel every such case through a generic helper `Ptr[T](v T) *T`, whose whole
body is `return &v` — the temporary, hidden inside a call. `new(expr)` is the
language primitive that does this directly: it allocates a variable for the
expression's value and returns a pointer to it, no named temporary and no helper
package.

## Exact semantics

Go's `new` has always taken a type: `new(T)` allocates a variable initialized to
T's zero value and returns `*T`. Go 1.26 adds a second form. If the argument is
an expression `expr` of type `T`, then `new(expr)` allocates a variable of type
`T`, initializes it to the *value* of `expr`, and returns its address, a `*T`. So
`new(int)` yields a `*int` pointing at `0`, and `new(42)` yields a `*int` pointing
at `42`. The two forms are disambiguated by whether the argument denotes a type or
a value.

This generalizes a convenience that composite literals already had. `&Person{...}`
worked before because taking the address of a composite literal is explicitly
allowed; `new(Person{Name: "a"})` is equivalent. What had no `&` form at all is a
pointer to a scalar, a conversion, or a call result — `new(yearsSince(t))` has no
`&yearsSince(t)` equivalent, and that is the gap the feature closes.

`make` is unrelated: it is only for slices, maps, and channels, and it returns an
initialized *value*, not a pointer. `new` returns a pointer; the two do not
overlap.

## Tri-state optionality is the real motivation

A non-pointer field cannot tell "the caller did not mention this field" apart from
"the caller set this field to its zero value" — both are the zero value on the
wire and in memory. A pointer field encodes three distinct states:

- `nil` — absent / unset / inherit from a lower layer.
- non-nil, pointing at the zero value — explicitly set to `""`, `0`, or `false`.
- non-nil, pointing at a non-zero value — explicitly set to something.

This is exactly what HTTP PATCH and partial-update APIs need (a missing key must
leave the stored field untouched; a present key set to `0` must overwrite it),
what protobuf `optional` scalars encode, what cloud-SDK request structs use for
their hundreds of optional parameters, and what layered configuration needs where
`nil` means "inherit" and a pointer to `0` means "override to zero". `new(expr)`
is what makes populating those fields ergonomic in a struct literal:
`AccountPatch{SeatLimit: new(0)}` reads as "explicitly set the seat limit to
zero", with no helper and no temporary.

## Escape analysis, not the keyword, decides stack vs heap

`new(expr)` has the same allocation semantics as `&v` on a local or a `Ptr[T]`
call. Whether the pointed-at variable lives on the stack or the heap is decided
by escape analysis: if the pointer does not outlive the function, it stays on the
stack regardless of how it was created. Do not reach for `new(expr)` expecting a
speedup, and do not avoid it fearing a heap allocation — it is behaviorally
identical to the patterns it replaces. Choose it for clarity and let the compiler
place the storage.

## The untyped-constant width trap

Because an expression argument carries a type, an *untyped constant* argument
takes its default type. `new(123)` is `*int`, `new(3.14)` is `*float64`, and
`new('a')` is `*int32` (rune). This bites when a wire format or SDK field needs a
specific width: assigning `new(30)` to an `*int64` field does not compile, and
`new(1)` gives you `*int` where an `*int32` was required. The fix is to type the
expression at the call site: `new(int64(30))` produces `*int64`, `new(int32(1))`
produces `*int32`. The mismatch surfaces at compile time when the target type is
explicit, but in generic or `any`-typed code it can slip through to marshal or RPC
time, so form the habit of typing the constant whenever the width matters.

## nil has no type to allocate

`new(nil)` does not compile: `nil` is untyped and has no concrete type for `new`
to size and initialize. A *typed* nil expression is fine —
`new([]int(nil))` yields a `*[]int` pointing at a nil slice, and
`new([]byte(nil))` a `*[]byte`. If you need a pointer to a nil map, slice, or
interface, give the `nil` a type via a conversion.

## The marshal side: omitempty and omitzero

Pointer optionality pairs with encoding on the way out. `omitempty` on a pointer
field drops it from JSON when it is nil, which is precisely the "absent" state.
Go 1.24 added the `omitzero` struct tag, which omits a field when it is the zero
value (consulting an `IsZero() bool` method if the type has one). The two express
different intents and the choice is a real API decision: use *pointer +
`omitempty`* when an explicit zero must round-trip onto the wire, and use *value +
`omitzero`* when absent and zero are equivalent and you never need to represent an
explicit zero. Do not combine a value field with `omitempty` and expect explicit
zero to survive — `Count int json:"count,omitempty"` drops both an unset field and
a real `0`, collapsing exactly the distinction pointers exist to preserve.

## Migration and the modernizer

Standardizing on `new(expr)` lets a codebase delete its `Ptr[T]` helper and drop
the `aws.String`/`aws.Int64`/`ptr.To` calls, removing a utility package and a
layer of type-parameter noise. The Go 1.26 modernizer (the `go fix` / `go vet`
analyzers that are the subject of the next lesson) can rewrite the
`v := x; p := &v` temporary and known helper calls into `new(expr)` mechanically,
so the migration is largely automated.

## Common Mistakes

### Assuming new(expr) forces a heap allocation

Wrong: avoiding `new(expr)` in a hot path because "it allocates". It has the same
escape behavior as `&v` and `Ptr[T]`; if the pointer does not escape, the value
stays on the stack. Fix: write for clarity and benchmark before assuming heap.

### Untyped-constant width surprise

Wrong: `Age: new(30)` where the field is `*int64`, or `new(1)` where `*int32` is
needed — `new` of an untyped constant defaults to `int` (or `float64`, or `rune`).
Fix: type the expression at the call site: `new(int64(30))`, `new(int32(1))`.

### Confusing new(T) with new(T{})

Wrong: `new(MyStruct)` when you meant to seed fields — it allocates a *zero*
`MyStruct`. Fix: `new(MyStruct{Field: x})` (equivalently `&MyStruct{Field: x}`)
allocates one initialized to that literal.

### A value field with omitempty for explicit zero

Wrong: `Count int json:"count,omitempty"` and expecting a real `0` to serialize —
it drops both unset and zero. Fix: use `*int` with `omitempty` when explicit zero
must round-trip, or `int` with `omitzero` when absent and zero are equivalent.

### new(nil)

Wrong: `new(nil)` — no concrete type to allocate; it does not compile. Fix: give
the nil a type, e.g. `new([]byte(nil))` yields `*[]byte`.

### Collapsing tri-state pointers with cmp.Or

Wrong: resolving a layered config with `out.MaxConns = cmp.Or(deref(p), fallback)`.
`cmp.Or` returns the first *non-zero* argument, so a pointer that was explicitly
set to `0` is treated as unset and clobbered by the fallback — reintroducing the
very ambiguity the pointer removed. Fix: collapse on `nil`, not on zero: check
`if p != nil { out.MaxConns = *p }`.

### Comparing optional fields by pointer identity

Wrong: `got.Age == want.Age` on `*int` fields compares addresses, which differ
even when the values match. Fix: compare dereferenced values (guarding nil) or run
`reflect.DeepEqual`/`go-cmp` over the whole struct.

### Forgetting the nil check on read

Adopting tri-state pointers trades zero-value ambiguity for nil-panic risk: every
read of an optional field must nil-check before dereferencing. Fix: resolve to
concrete values behind an `Apply`/`Resolve` boundary, or centralize access in a
helper, so the rest of the code works with plain values.

Next: [01-optional-patch-fields.md](01-optional-patch-fields.md)
