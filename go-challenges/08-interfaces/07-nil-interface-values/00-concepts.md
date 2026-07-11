# Nil Interface Values: Typed Nils, Null Objects, and Boundary Normalization — Concepts

Nil-interface handling is where seemingly-correct Go panics in production. The
error-return that is "always non-nil", the logger that panics on the one code
path nobody exercised in staging, the plugin registry that accepts a dead
`*sqlDB` and blows up on first query — all of them trace back to one fact about
how an interface value is represented. This file is the conceptual foundation
for the nine independent exercises that follow. Read it once and you have
everything you need to reason through each of them: a repository layer, a DI
container, `log/slog` wiring, a graceful-shutdown cleanup stack, an HTTP
middleware, and an error-to-status mapper — every one a concrete artifact from
real service code, not a syntax demo.

## Concepts

### An interface value is a two-word pair

An interface value is represented as a pair of machine words: a pointer to a
*dynamic type* descriptor (the itab, which also carries the method table) and a
pointer to the *dynamic value* (the data). The single rule that explains every
surprise in this lesson is:

> An interface value equals `nil` if and only if BOTH words are nil.

`var l Logger` is a genuine nil interface: no dynamic type, no dynamic value,
both words nil, `l == nil` is true. The moment an interface carries any dynamic
type — even with a nil dynamic value — the type word is non-nil, so the
interface is not equal to nil. Internalize the two-word model and the rest of
this lesson is deduction, not memorization.

### A nil interface has no method table to dispatch to

Calling any method on `var l Logger` (a nil interface) panics with
`invalid memory address or nil pointer dereference`. There is no dynamic type,
so there is no method set and nothing to dispatch to. This is categorically
different from calling a method on a nil *pointer*: that can succeed. The fix is
never to sprinkle `if l != nil` at every call site; it is to normalize the nil
away once, at the boundary, so downstream code can dispatch unconditionally.

### A typed nil is NOT a nil interface

```go
var n *Null           // n == nil is true: a nil pointer
var l Logger = n      // l == nil is FALSE
```

Assigning the nil `*Null` to a `Logger` interface fills in the dynamic type
(`*Null`) while leaving the dynamic value nil. One word is non-nil, so
`l == nil` is false. Method calls on `l` dispatch fine to `*Null`'s method set;
they only panic if a method actually dereferences its nil receiver. A method on
a nil pointer receiver that never touches the receiver's fields is perfectly
valid Go — this is exactly how the Null Object pattern and many stdlib types
(e.g. a nil `*bytes.Buffer` is not usable, but a nil map reads fine) behave.

### The typed-nil error return bug

This is the highest-impact instance of the two-word rule and one of the most
common real outage causes in Go services. A repository method declares
`func (r *Repo) Find(id string) (*User, error)` and, on the happy path, returns
a *concrete* `*NotFoundError` variable that happens to be nil:

```go
func (r *Repo) Find(id string) (*User, error) {
	var e *NotFoundError        // nil concrete pointer
	// ... happy path ...
	return user, e              // BUG: error interface wraps (*NotFoundError)(nil)
}
```

The returned `error` interface now has dynamic type `*NotFoundError` and a nil
dynamic value, so it is non-nil forever. Every caller's `if err != nil` fires on
success, error handling runs when nothing failed, and one layer up someone
dereferences the "error" and panics or ships a wrong status. The Go FAQ has a
dedicated entry for this. The fix is to return the interface-typed variable
(declare `var err error`) or an explicit `nil` literal — never a concrete
pointer type through an interface return.

### Normalize nil at the boundary, not at every call site

A constructor that maps `nil -> NullObject` converts an unbounded number of
per-call nil checks into exactly one, and makes the hot path branch-free:

```go
func NewService(l Logger) *Service {
	if l == nil {
		l = Null{}          // one check, at the boundary
	}
	return &Service{logger: l}
}
```

Every downstream `s.logger.Info(...)` now dispatches unconditionally. The
alternative — a nil interface used as the "no logger" sentinel plus an
`if s.logger != nil` before every log line — re-introduces a panic the instant
someone forgets one guard.

### The Null Object pattern is Go's answer to optional dependencies

A Null Object is a type that satisfies the interface with no-op methods: the
zero-value-of-behavior. It is the idiomatic Go answer to optional dependencies —
loggers, metrics recorders, tracers, audit hooks. Production wires a real
implementation; tests and lower-tier deployments pass nil and the constructor
substitutes the no-op. The no-op path costs nothing (an empty method inlines
away) and, crucially, allocates nothing, so putting a Null Object on a hot path
is free.

### Interface `==` can itself panic

Comparing two interface values with `==` compares dynamic types first, then, if
the types match, dynamic values. If the dynamic type is *not comparable* — its
underlying kind is a slice, map, or function — the comparison panics at runtime
with `comparing uncomparable type`. This is not a compile error: the static type
is an interface, which is comparable at compile time; the panic surfaces only
when a slice-backed dynamic value shows up. Map keys and set membership inherit
the hazard, because inserting an incomparable key into a `map[Handler]struct{}`
panics the same way. A dedup set or cache keyed on an interface can therefore
take a random production panic the first time an incomparable implementation is
added.

### reflect is the only generic typed-nil detector

Inside a DI container or plugin registry, dependencies arrive as `any`, and a
plain `dep == nil` check misses a typed nil (a `(*sqlDB)(nil)` passed as a
`Store` is not equal to nil). The only way to detect a typed nil generically is
`reflect`:

```go
func isNilValue(v any) bool {
	if v == nil {
		return true            // genuine nil interface
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}
```

`reflect.Value.IsNil` panics unless the Kind is Chan, Func, Interface, Map,
Pointer, or Slice — calling it on an int, struct, or string is a panic, so the
Kind switch is mandatory. And `reflect.ValueOf(nil)` returns the zero Value
whose Kind is `Invalid`, which is why the `v == nil` guard has to come first:
the reflect path only ever sees non-nil interfaces.

### log/slog: normalize a nil Handler to DiscardHandler

`slog.New(nil)` compiles and constructs a `*slog.Logger`, then panics on the
first log call because it dereferences the nil Handler. Go 1.24 added
`slog.DiscardHandler`, the canonical no-op Handler whose `Enabled` always
returns false, so normalize a nil Handler to it at construction. The Handler
interface is four methods — `Enabled(context.Context, Level) bool`,
`Handle(context.Context, Record) error`, `WithAttrs([]Attr) Handler`, and
`WithGroup(string) Handler` — and `WithAttrs`/`WithGroup` must each return a
*new* Handler, not mutate the receiver, because slog builds up context by
cloning handlers.

### errors.As / errors.Is walk the chain, and typed nils leak through

`errors.As` and `errors.Is` walk the `Unwrap` chain. A typed-nil error wrapped
somewhere in that chain re-introduces the trap one layer up: `errors.As` can
report a match and populate the target pointer with a nil `*DomainError`, and
the mapping code that trusts the boolean then dereferences it. Treat a true
return from `errors.As` as "the target is now set", but if the source can return
a typed nil, fix the *source* to return a real nil interface rather than
guarding every `errors.As` call site.

### Cleanup stacks must skip nil Closers and aggregate errors

A graceful-shutdown cleanup stack collects `io.Closer` values pushed during
startup (db, cache, listener) and closes them in LIFO order. Optional resources
arrive as nil interfaces or typed nils; `Close` must guard each with a typed-nil
check before calling `Close()`, or an uninitialized optional resource panics and
leaves the remaining resources leaked. Aggregate every failure with
`errors.Join` so one bad closer does not abort the shutdown of the rest.

## Common Mistakes

### Comparing a typed nil to nil and skipping error handling

Wrong: `if err == nil` where `err` carries a concrete `*MyError` dynamic type —
always false, so the success branch never runs and a caller later panics or
ships a 500 on a request that actually succeeded.

Fix: never return a concrete pointer type through an `error` return. Declare
`var err error` and return that, or return an explicit `nil`.

### Returning a concrete pointer through an interface return

Wrong: `return user, e` where `e` is a `*NotFoundError` that is nil on success.
The interface is non-nil forever.

Fix: return the interface-typed variable or a `nil` literal — normalize at the
source, not at every caller.

### Calling a method on a nil interface

Wrong: a function takes a `Logger` and calls `l.Info(...)` with no boundary
normalization; a caller passes a nil interface and it panics with an invalid
memory address.

Fix: normalize `nil -> Null{}` in the constructor. Never a per-call
`if l != nil`.

### Calling reflect.Value.IsNil on a non-nilable Kind

Wrong: `reflect.ValueOf(x).IsNil()` where `x` is an int, struct, or string —
panics. And forgetting that `reflect.ValueOf(nil)` yields the zero Value with
Kind `Invalid`.

Fix: guard by Kind first (Chan/Func/Interface/Map/Pointer/Slice), and handle the
genuine nil interface with a `v == nil` check before touching reflect.

### Using a nil interface as the "absent dependency" sentinel

Wrong: storing a nil `Logger`/`MetricsRecorder` as "no dependency" and
nil-checking at every call site — one forgotten guard is a production panic.

Fix: a Null Object stored once at the boundary; the hot path branches on
nothing.

### Assuming interface `==` is always safe

Wrong: keying a dedup set or cache on an interface whose dynamic type might be
slice/map/func-backed — a random `comparing uncomparable type` panic.

Fix: know which implementations are comparable; if any are not, key on a stable
comparable identity (a name, an ID) instead of the interface value itself.

### slog.New(nil)

Wrong: constructing `slog.New(nil)` or storing a nil `slog.Handler` — no panic
at construction, panic on the first log line, often in a rarely-hit path.

Fix: normalize a nil Handler to `slog.DiscardHandler` in the constructor.

### Trusting errors.As's boolean over a typed-nil source

Wrong: dereferencing the target after `errors.As` returns true, when the wrapped
error was a typed nil.

Fix: fix the source to return a real nil interface; a healthy chain never
carries a typed-nil error.

### Skipping the nil-Closer guard in a shutdown stack

Wrong: closing every pushed `io.Closer` unconditionally, so an optional resource
that was never initialized panics `Close` and leaks the rest.

Fix: skip nil interfaces and typed nils, and aggregate failures with
`errors.Join`.

Next: [01-nil-safe-logger-null-object.md](01-nil-safe-logger-null-object.md)
