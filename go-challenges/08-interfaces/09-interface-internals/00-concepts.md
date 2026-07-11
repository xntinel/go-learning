# Interface Internals: itab, eface, Dynamic Dispatch, and the Cost of Boxing — Concepts

Interfaces are the seam every Go backend is built on: the repository behind a
service, the `http.ResponseWriter` a middleware wraps, the `error` a handler
maps to a status code, the `any` a config loader decodes into. They feel free,
but under the hood every interface value is a two-word header, every method call
through one is an indirect jump, and every scalar you drop into an `any` may cost
a heap allocation. The bugs that come from not knowing this are the ones that
page you: a healthy repository that looks broken because it returned a typed nil,
a middleware that silently disables Server-Sent Events because it hid
`http.Flusher`, a dedup set that panics when someone puts a slice in an `any`
key, a custom `MarshalJSON` that never fires because it is on the wrong receiver.
This file is the model. Read it once and every exercise that follows is a
concrete production artifact you can reason about from the layout up.

## Concepts

### An interface value is two machine words

A non-empty interface value (one with methods, like `error` or `io.Reader`) is a
pair of pointers. The first word points at an `itab` — an interface table that
pairs the interface type with the concrete dynamic type and holds the resolved
method function pointers. The second word points at the concrete data (or holds
a word-sized value inline). Schematically:

```text
error value:  [ *itab ][ *data ]
                 |         |
                 |         +-- the *NotFoundError (or nil)
                 +-- (error interface, *NotFoundError, [ Error func ptr ])
```

The empty interface `any` is also two words, but it has no methods to dispatch,
so instead of an `itab` its first word is a plain `*_type` (the runtime calls
this layout `eface`, versus `iface` for the method-carrying kind). Both are two
words; the difference is whether the type word carries a method table.

This layout is the source of nearly every surprise below: nil-ness is decided by
the type word, comparison and assertion compare the type word, and boxing a value
is what fills in the data word.

### A method call through an interface is an indirect call

Calling `r.Find(id)` where `r` is an interface loads the `Find` function pointer
out of the `itab` and calls it, passing the data word as the receiver. The
compiler cannot see which concrete function that is at the call site, so it
generally cannot inline the call. This is why interface dispatch is more
expensive than a direct call and why *devirtualization* (the compiler proving the
dynamic type and turning the indirect call back into a direct, inlinable one)
matters on hot paths. Reach for a concrete type or a generic when a tight loop
calls through an interface millions of times.

### itabs are computed once per (interface, concrete) pair and cached

The runtime builds an `itab` the first time a given concrete type is assigned to
a given interface type, then caches it. Assigning the same concrete type to the
same interface again is cheap. Type assertions and type switches do not rebuild
anything — they compare the stored type pointer against the target type's pointer.
That comparison is a single pointer compare, which is why assertions are fast and
why a type switch's cost is simply the number of compares it performs.

### A type switch is a linear scan, not a hash or jump table

`switch v := x.(type) { case A: ...; case B: ... }` compiles to a sequence of type
comparisons: compare `x`'s type word to `A`, then to `B`, and so on. The first
match wins. There is no hash table and no computed jump — it is O(n) in the number
of cases. Two consequences follow. Case *order* affects performance: on a hot
path, put the production-dominant type first. And `default` matches everything, so
it must be last; a `default` placed first shadows every other case.

### A type assertion is a single comparison; the comma-ok form never panics

`x.(T)` is one type-word comparison. The two forms differ only in failure
handling: `v, ok := x.(T)` sets `ok=false` on mismatch and never panics; the
single-value `v := x.(T)` panics on mismatch. On untrusted input always use the
comma-ok form. Assertions are also how the standard library performs *optional
interface upgrades* at runtime: `io.Copy` asks `dst.(io.ReaderFrom)`, `net/http`
asks `w.(http.Flusher)` before streaming, `encoding/json` asks
`v.(json.Marshaler)`. Each is a runtime "does this value also satisfy that
interface?" check.

### Boxing a value into an interface can allocate

To put a value into an interface, the data word must reference it. For a
pointer-typed value that is free — the pointer already exists. For a non-pointer
value (an `int`, a `struct`), the compiler must find storage the data word can
point at; escape analysis decides whether that storage is the stack or the heap.
If the interface outlives the call (stored in a slice, returned, passed to a
function that escapes it), the value is copied to the heap: one allocation per
boxing. Small tricks reduce it — the runtime keeps a table of the first few
hundred small integers so boxing `any(3)` need not allocate, and word-sized
values can sometimes be held inline — but the classic hot-path leak is real:
converting scalars to `any` in a loop, especially through a variadic `...any`
(structured logging, generic helpers), allocates on every call and shows up as GC
pressure in a pprof profile.

### The typed-nil trap

An interface is `== nil` only when *both* words are nil. If you assign a nil
pointer of a concrete type to an interface, the type word is set (to that concrete
type) and only the data word is nil — so the interface is *not* nil. This is the
root cause of the "function returned a nil error but the caller saw a failure"
bug. It happens when a function's success path returns a concrete pointer type
that is nil, through a declared `error` return:

```go
func find() error {
	var e *NotFoundError // nil *NotFoundError
	return e             // interface is NON-nil: type=*NotFoundError, data=nil
}
```

The caller's `if err != nil` is true even though nothing went wrong. The fix is
to return the untyped `nil` literal (or the interface-typed variable), never a nil
concrete pointer.

### Interface comparison with == can panic

`==` on two interface values compares the type words, and if they match, compares
the concrete values. Comparing the concrete values panics at runtime if the
dynamic type is not comparable — slices, maps, and functions are not. So using an
`any` whose dynamic type is a slice/map/func as a map key, or comparing it with
`==`, panics with "runtime error: comparing uncomparable type" (or "hash of
unhashable type" for a map key). Guard open-ended values with
`reflect.TypeOf(x).Comparable()` before using them as keys.

### Method sets: T versus *T

The method set of a value type `T` contains only its value-receiver methods. The
method set of `*T` contains both value- and pointer-receiver methods. This
determines which `itab`s exist. It is why a pointer-receiver `MarshalJSON` or
`String` is silently skipped for a non-addressable value (a plain value passed by
interface, or a map element that cannot be addressed): only `T`'s method set
applies there, and the pointer method is not in it. `encoding/json` and `fmt`
check for the marshaler/stringer interface at runtime; if the value's method set
does not include the method, the check fails and the default formatting is used.

### errors.Is and errors.As walk the Unwrap chain dynamically

`errors.Is(err, target)` walks `err`'s `Unwrap` chain comparing each link to
`target` (using `==` or an `Is` method). `errors.As(err, &target)` walks the same
chain asking, at each link, "is this value assignable to `*target`'s element
type?" — an interface-satisfaction check — and when one matches it assigns the
concrete value through the target pointer using the same type machinery that backs
assertions. `errors.Join` builds a multi-error whose tree both functions
traverse. This is the runtime mechanism behind a clean error taxonomy mapped to
HTTP status codes.

### Reflection reads the same metadata the interface header points at

`reflect.TypeOf(x)` and `reflect.ValueOf(x)` read the runtime `*_type` that the
interface's type word already references — reflection is not magic, it is a typed
API over that metadata. It is strictly more expensive than a type switch (it
allocates, chases pointers, and defeats inlining), so reserve it for genuinely
open-ended input (walking an arbitrary struct's `validate` tags) and prefer a
closed type switch whenever the set of types is known.

### fmt checks Formatter, then error, then Stringer

When formatting a value, `fmt` first asks whether it implements `fmt.Formatter`
and, if so, hands off entirely to its `Format(State, verb)` method. Only if it is
not a `Formatter` does `fmt` (for the `v`, `s`, `q`, `x`, `X` verbs) check
`error` and then `fmt.Stringer`, before finally falling back to reflection-based
default formatting. So the precedence is Formatter over error over Stringer. A
type that implements both `error` and `Stringer` renders through `Error()`. A
type that implements `Formatter` controls every verb itself, including width and
precision, through the `fmt.State` it is handed.

## Common Mistakes

### Assuming a type switch is a constant-time lookup

Wrong: treating `switch x.(type)` as a hash/jump table and ordering cases for
readability on a hot path. It is a linear scan; the dominant case belongs first.

Fix: measure and order the production-dominant type first; accept that the switch
is O(number of cases).

### Putting the default case first

Wrong: a `default` before the concrete cases. `default` matches everything, so
the switch always takes it and the other cases are dead.

Fix: `default` last, always.

### Returning a nil concrete pointer as an error

Wrong: a success path that returns a nil `*MyError` through an `error` return,
then a caller checking `err != nil` and seeing a false failure.

Fix: return the untyped `nil` on success; never a typed nil pointer.

### Using == nil on an any that may hold a typed nil

Wrong: `if v == nil` to detect absence of a value that was assigned a nil pointer;
the interface is non-nil because its type word is set.

Fix: use a `case nil` in a type switch, or `reflect.ValueOf(v).IsNil()` for the
real pointer-nil check.

### Uncomparable dynamic types as map keys or in ==

Wrong: an `any` holding a slice/map/func used as a map key or compared with `==`;
it panics at runtime.

Fix: precheck with `reflect.TypeOf(x).Comparable()` and reject or hash the value.

### Boxing scalars in a hot loop

Wrong: `log("count", n, "ok", b)` with a `...any` signature on every request,
paying a heap allocation per scalar for the boxing.

Fix: typed field constructors that keep scalars out of `any`, or avoid the
variadic on the hot path.

### Wrapping http.ResponseWriter without re-exposing optional interfaces

Wrong: a status-capturing wrapper that embeds `http.ResponseWriter` and adds no
`Flush`/`Hijack`/`ReadFrom`, silently disabling streaming and connection upgrades
because the stdlib's optional-interface assertions now fail.

Fix: forward each optional interface explicitly via a runtime assertion on the
inner writer.

### Pointer-receiver MarshalJSON/String on non-addressable values

Wrong: implementing `MarshalJSON` or `String` on `*T` and expecting it to fire for
a `T` value or a map element, where only `T`'s method set applies.

Fix: use a value receiver when the value must render everywhere, or always pass a
pointer.

### Reaching for reflect when a type switch suffices

Wrong: `reflect` over a known, closed set of types.

Fix: a closed type switch is simpler, faster, and safer; keep reflect for
open-ended input.

### The panicking assertion on untrusted input

Wrong: `x.(T)` (single-value) on input you do not control.

Fix: the comma-ok form `v, ok := x.(T)`.

Next: [01-type-switch-dispatcher.md](01-type-switch-dispatcher.md)
