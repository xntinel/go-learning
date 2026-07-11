# Struct Comparison and Equality — Concepts

Equality looks like the most boring thing in the language and is quietly one of
the most load-bearing. In a backend service, "are these two values equal?" is the
question underneath request de-duplication, idempotency guards, cache
change-detection, config-reload skipping, per-dimension metrics keyed on a tuple,
and every golden-file API test you own. Get it right and those systems are cheap
and obvious. Get it wrong and you get a class of bug that compiles, passes the
happy-path test, and then panics in production the first time an interface field
holds a slice — or silently treats two things as different when your domain says
they are the same. This file is the model. Read it once and the nine exercises
that follow are each a concrete production artifact where one equality decision is
the whole point.

## Concepts

### Comparability is a static property of the type

In Go, whether a value can be compared with `==` is decided by its type at compile
time, not by its contents at run time. The rule for structs is exact and worth
memorizing: a struct is comparable with `==` **iff every one of its fields is
comparable**. Comparable types are the booleans, numerics, strings, pointers,
channels, interfaces, and — recursively — arrays and structs built only from
comparable types. The three types that are **never** comparable are slices, maps,
and functions; the only thing you may write for them is `x == nil`.

The transitive consequence is the one that bites: a single slice, map, or func
field makes the *entire* enclosing struct non-comparable. You cannot `==` it, you
cannot use it as a map key, and you cannot pass it as a type argument to anything
constrained by `comparable`. Adding a `history []int` field to a struct that was a
map key is a source-breaking change that the compiler will reject at the map
declaration site — not at the field.

```go
type A struct{ id int; name string }        // comparable
type B struct{ id int; tags []string }       // NOT comparable: has a slice field
var m map[A]int  // legal
var n map[B]int  // compile error: invalid map key type B
```

### `==` on structs: fast, field-by-field, compile-checked

For a fully comparable struct, `==` is a field-by-field comparison: it is `O(number
of fields)`, needs no imports, allocates nothing, and — crucially — is the only
option the compiler checks for you. If someone adds an incomparable field, the
`==` site stops compiling; you find out at build time, not from a 2 a.m. page.
Prefer `==` whenever the type is fully comparable. It is the default, and every
other tool below is a considered exception to it.

Arrays follow the same rule and are comparable when their element type is: `[3]int`
compares element by element. This is why a fixed-size `[16]byte` hash is a perfectly
good comparable struct field and map key, while a `[]byte` is not.

### Interface comparison is dynamic — and can panic at run time

Two interface values (including `any` and `error`) are equal under `==` when their
dynamic types are identical *and* their dynamic values are `==`. The catch is that
this check is performed at run time, and if the dynamic type turns out to be
non-comparable — a slice, a map, a func, or a struct containing one — the runtime
**panics** with "comparing uncomparable type". This compiles cleanly. It is a
latent crash that only fires when the interface happens to hold the wrong concrete
type.

```go
var x, y any = []byte("a"), []byte("a")
_ = x == y // compiles; panics at run time: comparing uncomparable type []uint8
```

This is why comparing cache values of type `any`, or two `error` values, or any
struct with an interface field, with a naked `==` is a bug waiting for the right
input. Guard it: check `reflect.TypeOf(v).Comparable()` first, or use a type switch,
and fall back to `reflect.DeepEqual` / `bytes.Equal` for the incomparable branch.

### `reflect.DeepEqual`: the reflection hammer, for tests only

`reflect.DeepEqual` walks two values recursively via reflection and returns whether
they are "deeply equal". It handles slices, maps, and pointers that `==` cannot.
It is also the source of the most common flaky-test surprises, and you must know
its semantics cold:

- A `nil` slice is **not** deeply equal to a non-nil empty slice; likewise `nil`
  map vs empty map. A JSON round-trip that turns `nil` into `[]` will fail a naive
  `DeepEqual`.
- `NaN` is not deeply equal to `NaN`, because it compares floats with `==` and
  `NaN != NaN`. So `DeepEqual` is not even reflexive on a struct holding a `NaN`.
- It follows pointers, comparing what they point at, not their addresses.
- It does **not** honor a type's `Equal` method. It compares fields, period.
- It is slow (reflection) and not type-safe (its signature is `(any, any) bool`, so
  comparing a `Foo` to a `Bar` compiles and just returns false).

Use it in tests where its cost is irrelevant and its quirks are understood. Never
put it on a request path.

### `slices.Equal` / `maps.Equal`: the type-safe replacements

When what you actually have is a slice or a map of comparable elements,
`slices.Equal` and `maps.Equal` (plus their `…Func` variants) are the right tool:
type-safe, allocation-free, no reflection. `slices.Equal(a, b)` reports whether two
slices have the same length and `==`-equal elements in order — and, unlike
`DeepEqual`, it treats `nil` and empty as equal (both have length zero). `maps.Equal`
does the same for maps. Reach for these instead of `DeepEqual` the moment the shape
is "a slice/map of comparable things"; they say what you mean and cost nothing.

### A custom `Equal` method encodes the domain contract

`func (t T) Equal(other T) bool` puts the equality rule in the type, type-safely,
with no `reflect` import. It is the tool when domain equality is not "all fields
identical": when you must *ignore* a derived/cached field, or *add* an invariant
(a `Money` type that refuses to call USD equal to EUR even when the amounts match).
The cost is one method per type, and the discipline that the method must respect
the equality contract — reflexive, symmetric, transitive. A value-receiver `Equal`
compares *values* regardless of pointer identity, which is exactly what you want:
`a.Equal(*b)` is about state, whereas `pa == pb` on two `*T` compares addresses.

### `time.Time` must be compared with `.Equal`, not `==`

`time.Time` is the canonical trap. A `Time` carries three things: a wall-clock
reading, an optional monotonic-clock reading, and a `*Location`. `==` compares all
three. So the *same instant* observed in two zones is not `==` (different
`Location`), and a `time.Now()` (which has a monotonic reading) is not `==` to a
wall-clock-only copy of the same instant. `t1.Equal(t2)` compares only the instant
and is the correct test. Before you use a `time.Time` as a map key or a DB key,
normalize it: `.Round(0)` strips the monotonic reading and `.UTC()` canonicalizes
the location, so two representations of one instant collapse to one key.

### `comparable` is the bridge to generics

The predeclared constraint `comparable` is satisfied by exactly the types you can
use with `==` — which is exactly the types you can use as map keys. That is what
lets you write `Set[T comparable]` or a generic dedup helper whose `T` is stored in
a `map[T]struct{}`. The constraint is checked at compile time, so a caller who tries
to instantiate `Set[[]byte]` is rejected at the instantiation site. `comparable`
carries all the way through: a comparable struct is a legal `T`, a struct with a
slice field is not.

### google/go-cmp: the production test comparator

`github.com/google/go-cmp/cmp` is what real Go tests use instead of `DeepEqual`.
`cmp.Equal(x, y, opts…)` returns a bool; `cmp.Diff(x, y, opts…)` returns a
human-readable diff string that is **empty exactly when the values are equal** — so
the idiomatic assertion is `if diff := cmp.Diff(want, got); diff != "" { t.Error(diff) }`.
Two behaviors make it the right default. First, it automatically dispatches to a
type's `Equal` method: if `T` has `Equal(T) bool`, `cmp` uses it, so your domain
equality is honored for free (and `DeepEqual` would ignore it). Second, it panics
by design on unexported fields unless you tell it what to do (`cmpopts.IgnoreUnexported`,
`AllowUnexported`, or an `Equal` method) — forcing an explicit decision rather than
silently comparing private state.

`cmpopts` encodes real test policy without weakening the assertion:
`EquateEmpty()` makes `nil` and empty slices/maps equal (kills the `DeepEqual`
nil-vs-empty flake), `IgnoreFields(T{}, "UpdatedAt")` drops volatile fields,
`EquateApproxTime(d)` allows a bounded timestamp tolerance, and `SortSlices` makes an
order-insensitive comparison. These are exactly the knobs that make golden API tests
stable.

### The decision tree

Put together, choosing an equality tool is mechanical:

- Fully comparable type, and/or a hot path: use `==`. It is fastest and
  compile-checked.
- A slice or map of comparable elements: `slices.Equal` / `maps.Equal`.
- Domain rules, an ignored/derived field, or an added invariant: a custom
  `Equal` method.
- A test that needs a readable diff, tolerance, or nil-vs-empty leniency:
  `google/go-cmp` with `cmpopts`.
- A dynamic `any`/interface value whose type you do not control, in a test:
  `reflect.DeepEqual`, behind a comparability guard, last resort.

## Common Mistakes

### `==` on an interface/any whose dynamic type may be incomparable

Wrong: comparing two `any` cache values, or a struct with an `any` field, with a
naked `==`. It compiles and then panics at run time the first time the dynamic type
is a slice or map. Fix: guard with `reflect.TypeOf(v).Comparable()` (or a type
switch) and fall back to `reflect.DeepEqual`/`bytes.Equal` on the incomparable
branch.

### Comparing `time.Time` with `==`

Wrong: `stored == time.Now()` or using a raw `time.Now()` result as a map key.
`==` compares the monotonic reading and `Location`, so it is "not equal to itself"
after a JSON round-trip or a zone change. Fix: compare instants with `t1.Equal(t2)`;
before keying, normalize with `.Round(0).UTC()`.

### Comparing two pointers and expecting a content comparison

Wrong: `pa == pb` on two `*T`, thinking it looks at the fields. It compares
addresses. Fix: dereference (`*pa == *pb`) or call a value `Equal` method
(`pa.Equal(*pb)`).

### Leaning on `reflect.DeepEqual` in tests

Wrong: asserting with `DeepEqual` and then getting flaky failures from nil-vs-empty
slices/maps or a `NaN` field — or paying its reflection cost on a request path
because it leaked out of the test. Fix: use `slices.Equal`/`maps.Equal` for concrete
shapes and `go-cmp` (with `cmpopts.EquateEmpty`) for structs.

### Adding a slice/map field to a type used with `==` or as a map key

Wrong: dropping a `[]string` field into a struct that was a map key, and being
surprised the map declaration no longer compiles. Fix: this is the compiler doing
its job; decide deliberately between keeping the type comparable (store a
comparable summary, e.g. a hash or a fixed array) and switching that type to a
custom `Equal`.

### Writing an `Equal` that compares the wrong subset of fields

Wrong: an `Equal` that includes a derived/cache field, or forgets one, breaking
reflexivity or symmetry and corrupting the sets and dedup maps that rely on it.
Fix: compare exactly the fields that define identity, and add a reflexivity test
(`a.Equal(a)` must hold).

### Assuming `reflect.DeepEqual` honors an `Equal` method

Wrong: expecting `DeepEqual(a, b)` to call your `Money.Equal` and refuse
cross-currency equality. It does not; it compares fields, so it will call two
different-currency amounts equal if the numbers match. Fix: use `==` semantics or
`go-cmp` (which dispatches to `Equal`); reserve `DeepEqual` for values without a
domain `Equal`.

### Using `cmp` on unexported fields without a decision

Wrong: `cmp.Equal` / `cmp.Diff` on a type with unexported fields and no `Equal`
method — go-cmp panics on purpose. Fix: add an `Equal` method, or pass
`cmpopts.IgnoreUnexported(T{})` / `cmp.AllowUnexported(T{})` to state intent.

### Treating `cmp.Diff` as a bool

Wrong: `if cmp.Diff(a, b) { … }` — it returns a string, and the empty string means
equal, so this is both a type error and, if coerced, an inverted assertion. Fix:
`if diff := cmp.Diff(want, got); diff != "" { t.Error(diff) }`.

Next: [01-token-bucket-comparable-equal.md](01-token-bucket-comparable-equal.md)
