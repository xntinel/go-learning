# Generic Error Matching with errors.AsType — Concepts

The error you catch at the edge of a service is almost never the error that was
raised. It is a wrapped tree: a `sql: no rows` at the bottom, wrapped by a
repository into a `*NotFoundError`, wrapped again by a service with
`fmt.Errorf("load account %s: %w", id, err)`, and handed to an HTTP handler. The
handler's job is translation: turn that tree into transport concerns — an HTTP
status and a problem+json body, a gRPC code, a retry-or-give-up decision, an
alerting label. For a decade the tool for that translation was `errors.As`, and
it carried a small tax on every call site: declare an out-variable, take its
address, and remember that passing the wrong kind of pointer panics at runtime.

Go 1.26 adds `errors.AsType`, the generic form of `errors.As`. It returns the
typed error directly instead of writing through an out-pointer, which makes those
boundary translators type-safe, one line each, and free of the panic foot-gun.
This file is the conceptual foundation for the three exercises that follow: an
HTTP error mapper, a retry classifier, and a study of where `errors.As` is still
the right call. Read it once and you have the full model.

## Concepts

### The signature and its semantics

```go
func AsType[E error](err error) (E, bool)
```

Added in Go 1.26. `AsType` finds the first error in `err`'s tree that matches the
type `E`, and if one is found returns that value and `true`; otherwise it returns
the zero value of `E` and `false`. "Matches" is defined exactly as it is for
`errors.As`: the type assertion `err.(E)` holds, or the error exposes a method
`As(any) bool` that returns true for a non-nil `*E` (in which case that method is
responsible for setting the target). The only structural difference from
`errors.As` is the calling convention: `As` takes `target any` and writes into
it; `AsType` takes the type as a parameter and returns the value.

The tree traversal is the same model shared by `As` and `Is`: `err` itself, then
the errors reached by repeatedly calling `Unwrap() error` or `Unwrap() []error`.
Multi-error wrappers built by `errors.Join` are visited depth-first. `AsType`
returns the *first* match in that order, so when a tree contains several
matchable types, ordering is a real semantic detail, not an accident.

### Why it exists over errors.As

`errors.As` forces a three-part dance at every call site:

```go
var perr *fs.PathError
if errors.As(err, &perr) {
	log.Printf("path: %s", perr.Path)
}
```

You declare the variable, you pass its address, and then you read it. `AsType`
collapses that to one expression whose result is already the concrete typed
value:

```go
if perr, ok := errors.AsType[*fs.PathError](err); ok {
	log.Printf("path: %s", perr.Path)
}
```

Two senior payoffs fall out of this. First, the returned value carries the
concrete type, so you immediately read `perr.Path` or `perr.Op` — or, when `E` is
`net.Error`, call `ne.Timeout()` — without a second assertion. Second, and more
important for correctness, it removes an entire class of runtime panic.
`errors.As` panics if its target is not a non-nil pointer to a type that
implements `error` or to any interface type; that check happens at run time.
`AsType` turns the same requirement into the compile-time constraint `E error`.
A bad type argument is now a build error, not a production panic.

### E can be an interface, and that is the high-value idiom

Because the constraint is `E error`, `E` may be a behavioral interface rather than
a concrete struct. `errors.AsType[net.Error](err)` matches any error in the tree
whose dynamic type implements `net.Error`, and hands you a value on which you can
call `Timeout()`. This is the idiom that pays off across packages you do not own:
you classify errors by *contract* (is it a timeout? is it retryable?) instead of
by depending on some third-party library's concrete error type, which you should
never import and which may change.

The catch is the constraint itself. `AsType` can only match interfaces that embed
`error`. A behavioral interface you want to match must therefore include the
`error` method:

```go
type Retryable interface {
	error
	Retryable() bool
}
```

`errors.AsType[Retryable](err)` compiles and works. A bare
`interface{ Temporary() bool }` — no `error` method — does *not* satisfy the
`E error` constraint and will not compile with `AsType`. Matching into an
interface that does not embed `error` is precisely the job that stays with
`errors.As`, whose target may be any interface type.

### The reflection contrast, stated precisely

It is folklore to say `AsType` "uses no reflection". The honest statement is
narrower and more useful. `errors.As` must inspect its `target any` with
reflection: it validates that the target is a non-nil pointer to an error-ish or
interface type, and it assigns the matched error through that pointer.
`AsType` knows `E` at compile time, so the common path — the plain type assertion
`err.(E)` — needs neither that reflective target validation nor a target to
allocate. What `AsType` does *not* eliminate is a user-supplied `As(any) bool`
method: if an error in the tree defines one, that method still runs arbitrary
code during matching. So the win is compile-time type safety and the removal of
target validation, not a magic zero-cost guarantee. Do not sell it as a free
performance win.

### Nil-safety and the typed-nil trap

`AsType` is nil-safe by construction: if `err == nil` it returns `(zero, false)`
without traversing anything. This is convenient but narrow, and it does *not*
rescue the classic typed-nil-in-interface trap. An `error` interface value that
is non-nil but holds a nil concrete pointer (a `*MyError` that is nil, boxed into
a non-nil `error`) still enters the tree and still participates in matching.
`AsType` handling a literal `nil` is not the same as handling a non-nil interface
carrying a nil pointer; the second still surprises callers who assumed "no real
error".

### Pointer versus value identity

The type argument must match how the error was wrapped, exactly. If the tree
holds an `*fs.PathError`, then `errors.AsType[*fs.PathError]` matches and
`errors.AsType[fs.PathError]` does not. When the error type's methods have
pointer receivers, only the pointer form satisfies `error` and the value form is
a compile error — a loud failure. The dangerous case is a type with *value*
receivers, where both `T` and `*T` satisfy `error`: wrap a `&T{}` and ask for
`AsType[T]`, and you get a silent `(zero, false)` with no diagnostic. This is the
single most common quiet no-match, and it is why large error structs are
conventionally wrapped and matched as pointers.

### Division of labor with errors.Is

`AsType` and `As` answer "is there a value of this type in the tree, and give it
to me". `errors.Is` answers a different question: identity against a sentinel —
`io.EOF`, `sql.ErrNoRows`, `context.Canceled`, `context.DeadlineExceeded`. When
you only need to know "is this that specific error", `errors.Is` is clearer and
cheaper; reaching for `AsType` there is a category error. A good boundary mapper
uses both: `Is` for sentinels, `AsType` for typed values whose fields you need.

### Toolchain reality

`errors.AsType` exists only under a Go 1.26 toolchain. In an older module the
symbol is simply undefined, producing a confusing `undefined: errors.AsType`
build error. Every module in this lesson therefore declares `go 1.26`; this is
genuinely modern-Go material, and the version directive is not optional.

## Common Mistakes

### Pointer/value mismatch in the type argument

Wrong: an error is wrapped as `*MyError`, and you write
`errors.AsType[MyError](err)`. When `MyError` has value receivers this compiles
and silently returns `(zero, false)`; the branch you expected never runs. Fix:
match the exact wrapped type, which for a struct error is almost always the
pointer form, `errors.AsType[*MyError](err)`.

### Assuming AsType can match any interface

Wrong: expecting `errors.AsType[interface{ Temporary() bool }](err)` to work the
way `errors.As` does with an arbitrary interface target. The `E error` constraint
forbids an interface that does not embed `error`, so this does not compile. Fix:
give the behavioral interface an embedded `error` (`type Retryable interface {
error; Retryable() bool }`), or keep using `errors.As` for a genuinely
non-error interface target.

### Believing AsType never touches reflection

Wrong: treating `AsType` as a guaranteed zero-reflection, zero-cost replacement.
The type-assertion path avoids the reflective target validation `As` does, but a
custom `As(any) bool` method on some error in the tree still executes user logic
during matching. Fix: frame the benefit as compile-time type safety and removed
target validation, and measure if performance actually matters.

### Migrating every errors.As call site reflexively

Wrong: a blanket find-and-replace of `As` with `AsType`. That breaks the cases
where `As` is deliberately better — matching into a non-error interface, reusing
one preallocated target across a hot loop, or a target type known only at run
time. Fix: migrate where the value-returning form is clearer and type-safer;
leave the retention cases alone. The decision rule, not the migration, is the
lesson.

### Using AsType where the intent is sentinel identity

Wrong: `if _, ok := errors.AsType[...](err); ok` when what you meant was "is this
`context.Canceled`". Fix: use `errors.Is(err, context.Canceled)`. Type extraction
and sentinel identity are different questions; pick the function that names the
one you are asking.

### Forgetting the typed-nil-in-interface trap

Wrong: assuming that because `AsType` handles a literal `nil err`, it also
protects you from a non-nil `error` holding a nil `*T`. It does not; that value
still enters the tree and can match. Fix: never box a nil concrete pointer into
an `error` return in the first place — return a literal `nil`.

### Misusing the ok result

Wrong: reading the returned error even when `ok` is false. When there is no
match, the returned value is the zero value of `E` (a nil pointer or a nil
interface), and dereferencing it panics. Fix: gate every use behind the `ok`
boolean, exactly as you would a map lookup.

### Building without a go 1.26 toolchain

Wrong: calling `errors.AsType` from a module whose `go.mod` says `go 1.25` or
older, then being confused by `undefined: errors.AsType`. Fix: declare `go 1.26`
in the module and run with a 1.26 toolchain.

Next: [01-http-error-mapper.md](01-http-error-mapper.md)
