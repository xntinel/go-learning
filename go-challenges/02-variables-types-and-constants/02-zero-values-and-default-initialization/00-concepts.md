# Zero Values and Default Initialization in Production Types — Concepts

This looks like a beginner topic and it is not. The Go rule "every variable is
initialized to its type's zero value" is one sentence, but the senior payload is
a single hard idea: *zero is not unknown*. That distinction is exactly where
production data-integrity and API-contract bugs live. A PATCH endpoint that
cannot clear a boolean field. A DB layer that silently turns `NULL` into `""`. A
list endpoint that emits `null` and crashes a JavaScript client that did
`resp.map(...)`. A config where an omitted timeout quietly means "unlimited"
instead of "use the default". Every one of these is a zero-value modelling bug,
not a syntax mistake. Read this file once and you have the model behind all ten
exercises that follow.

## The zero-value guarantee

Go has no uninitialized memory. Declaring `var x T` — or leaving a struct field
unset, or growing a slice with `make` — gives you `T`'s zero value: numeric
types `0`, `bool` `false`, `string` `""`, and the reference-like family
(pointer, slice, map, channel, func, interface) `nil`. Structs and arrays are
zeroed recursively: every field gets its own type's zero value, all the way
down. This eliminates the entire class of C-style garbage-memory bugs — there is
no such thing as reading an uninitialized Go variable and getting whatever
happened to be on the stack.

What the guarantee does *not* give you is operational readiness. "Initialized"
and "usable for the operation you are about to perform" are different claims. The
reference-like types have deliberately asymmetric zero-value semantics, and
knowing them cold is table stakes for reviewing concurrency and serialization
code:

- A **nil slice** is a fully usable empty collection: `len` is 0, `range` does
  nothing, and `append` allocates and returns a real slice. You almost never
  need to pre-allocate a slice just to append to it.
- A **nil map** is read-only: reading a missing key returns the element type's
  zero value with `ok == false`, but *writing* to it panics with "assignment to
  entry in nil map". This asymmetry is the single most common nil-related panic
  in Go backends.
- A **nil channel** blocks forever on both send and receive. It is not "closed"
  and not "empty" — a receive or send simply parks the goroutine permanently.
  This is occasionally useful (disabling a `select` case by nil-ing its channel)
  and frequently a deadlock.
- A **nil interface** or **nil pointer** panics when you call a method that
  dereferences the receiver, or when you dereference the pointer directly.

## The usable zero value is a design property, not an accident

The standard library treats "the zero value is ready to use" as an API contract
you can rely on and should imitate. `sync.Mutex`, `bytes.Buffer`,
`sync.WaitGroup`, `sync.Map`, `sync.Once`, `strings.Builder`, and the
`sync/atomic` types (`atomic.Int64`, `atomic.Bool`, ...) are all documented as
ready at their zero value: `var mu sync.Mutex; mu.Lock()` works, `var b
bytes.Buffer; b.WriteString("x")` works, `var n atomic.Int64; n.Add(1)` works.

Designing your own types this way removes constructor ceremony and, more
importantly, removes an entire bug class: the "forgot to call `New()`" bug where
a struct is technically valid but semantically broken because some field was
never wired up. If `var c Collector` records requests correctly, a caller cannot
get it wrong. The mechanism that makes this work for internally-allocated state
is **lazy allocation behind a method that owns the invariant**: the classic
`if m == nil { m = make(...) }` guard, executed inside the writer, under the same
lock that guards the field. Callers keep a usable zero value; the type allocates
on demand the first time it actually needs to.

## Zero is not unknown — the senior takeaway

Overloading `0`, `false`, `""`, or the zero `time.Time` to mean "not provided"
works right up until the day that value becomes a legitimate domain value, and
then it fails silently and corrupts data. Status code `0`. `active = false`. An
empty nickname a user genuinely chose. The Unix epoch as a real timestamp. Each
of these is indistinguishable from "unset" if you modelled absence as the zero
value. When the distinction between "absent" and "present-but-zero" carries
meaning, you must model it explicitly. The Go toolbox for that is small and
worth memorizing:

- a **pointer field** (`*bool`, `*int`, `*string`): `nil` means absent, non-nil
  means present — including present-and-zero. This is how JSON PATCH bodies
  distinguish `{"active": false}` from an omitted `active`.
- an **accompanying bool** (`Value T; Valid bool`) — the shape of `sql.Null[T]`
  and `sql.NullString`, keeping `NULL` distinct from the zero value across a DB
  boundary.
- **`time.Time.IsZero()`** — the idiomatic "never set" test for timestamps, so a
  never-seen node is distinct from one seen long ago.
- a **typed optional** or a small sum type when the two states each carry data.

## JSON puts the nil-vs-empty distinction on the wire

Serialization makes the distinction observable and contractual. A **nil slice**
marshals to `null`; an **empty non-nil slice** `[]T{}` marshals to `[]`. A **nil
map** marshals to `null`; an **empty map** to `{}`. Clients that expect a JSON
array break on `null` (`null.length` is a TypeError; `for...of null` throws), so
an API that returns a possibly-nil result slice has a latent
consistency bug: it emits `[]` when the query matched rows and `null` when it did
not. The fix is to force an empty-but-non-nil value at the API boundary.
Conversely, `omitempty` is *defined* in terms of the zero value: it drops a field
whose value is the zero value, so a nil slice, `false`, `0`, and `""` all
disappear from the output — which is what you want for optional fields and
exactly what you do *not* want for a required array.

## Copying a used zero value is its own hazard

Types with a usable zero value almost always also carry the "do not copy after
first use" rule. Copying a `sync.Mutex` after it has been locked duplicates the
lock state; copying an `atomic.Int64`, `sync.WaitGroup`, `sync.Once`, or
`sync.Map` mid-flight duplicates coordination state and produces silent races or
lost updates. These types embed a `noCopy` marker so `go vet`'s `copylocks`
analyzer flags the copy at build time. The discipline is mechanical: give such
types pointer receivers, pass `*T`, and never return or range over a copy of a
struct that embeds one.

## Struct comparability underpins zero-value map keys

A struct is comparable with `==` — and therefore usable as a map key — iff every
one of its fields is comparable. Adding a slice, map, or func field silently
removes comparability: the code that used the struct as a map key or compared it
with `==` no longer compiles. The zero-value struct is itself a perfectly valid,
distinct key (`map[Key]V` with `Key{}` as a key is fine and common as a
sentinel). This is why a small `{Tenant, Resource, Version}` struct makes an
excellent cache or dedup key, and why bolting a `[]string` field onto it later is
a breaking change you feel at compile time, not runtime.

## Defaults-from-zero as a configuration strategy

A `Config` whose zero value normalizes to safe production defaults is ergonomic:
callers write `New(Config{})` or set only the two fields they care about. You
implement it with an internal `withDefaults`/`normalize` step, and `cmp.Or`
(Go 1.22+) — which returns its first non-zero argument — is the idiomatic
fill-the-default operator: `c.Timeout = cmp.Or(c.Timeout, defaultTimeout)`. The
catch is that this pattern *requires* documenting the "explicit 0 vs omitted"
policy, because in this design a caller-supplied `0` is indistinguishable from
omitted and both get the default. If `0` needs to mean "unlimited" rather than
"use the default", zero-value-defaults is the wrong pattern and you need a
pointer or a sentinel instead.

## Concurrency-safe zero values

The stdlib hands you coordination primitives with no constructor.
`atomic.Int64`/`atomic.Bool` give lock-free counters and flags. `sync.Once`,
`sync.OnceFunc`, `sync.OnceValue`, and `sync.OnceValues` give one-time lazy
initialization that is safe under concurrent first-callers. `sync.Map` is a
ready-to-use concurrent map. Their zero-value readiness is guaranteed by the
package contract, which lets you instrument a handler or lazily build a shared
resource without pulling in a metrics or DI library. `sync.OnceValue` is worth
special note: it returns a function that runs the builder exactly once and, if
the builder panics, re-panics with the same value on every subsequent call — so
a failed one-time init fails consistently rather than half-initializing.

## Common Mistakes

### Writing to a nil map

`var m map[string]int; m[k]++` compiles and panics at runtime with "assignment
to entry in nil map". Allocate before the first write, ideally inside the method
that owns the map and under its lock, so callers keep a usable zero value while
the type allocates on demand.

### Using a raw zero value to mean "not set"

`active == false`, `status == 0`, an empty nickname, or the zero `time.Time` all
read as "unset" the moment they become legitimate values. Use a pointer, an
accompanying bool, `time.Time.IsZero`, or `sql.Null[T]` to keep absent distinct
from present-and-zero.

### Shipping `null` where clients expect `[]` or `{}`

Returning a nil slice or nil map from an API marshals to `null` and breaks
consumers that iterate or index the result. Return an empty non-nil value when
the wire contract demands an array or object.

### Copying a struct that contains a mutex or atomic after use

Duplicating a used `sync.Mutex`, `atomic.Int64`, `sync.WaitGroup`, or
`sync.Once` copies its internal state and produces silent races. Use pointer
receivers, share `*T`, and let `go vet` `copylocks` catch the mistake.

### Assuming a nil channel is closed or empty

Sends and receives on a nil channel block forever. A `select` case reading a nil
channel is effectively disabled; a bare `<-ch` on a nil channel is a permanent
deadlock.

### Leaking internal state through a snapshot

Returning the internal map or slice directly from `Snapshot()`/`Failures()` lets
a caller mutate the owner's state. Return a defensive copy (`maps.Clone`, `copy`,
or a fresh `make`), and never assume map iteration is ordered.

### Adding an incomparable field to a struct used as a key

Adding a slice or map field to a struct that is a map key or compared with `==`
removes comparability and breaks the code at compile time — or forces a late
redesign.

### Collapsing NULL into the zero value in the DB layer

Treating `sql.NullString{}.Valid == false` the same as `""` corrupts data
semantics by merging "no value" with "empty value". Convert to a pointer or keep
the `Valid` flag across the domain boundary.

### Re-initializing on every call instead of once

Re-parsing templates or re-compiling regexps on every request wastes work; doing
that init in a package `init()` runs it even when the feature is never used.
`sync.Once`/`sync.OnceValue` gives genuinely one-time, lazy, concurrency-safe
initialization.

Next: [01-zero-value-metrics-collector.md](01-zero-value-metrics-collector.md)
