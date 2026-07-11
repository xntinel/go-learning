# Designing Pointer-Safe APIs — Concepts

Pointers are where Go APIs quietly go wrong in production. Not because the syntax
is hard — `*T` and `&x` are learned in an afternoon — but because a pointer in a
signature is a *contract about ownership and lifetime*, and most bugs come from
two parties disagreeing about that contract without ever saying so. A repository
that returns its internal `*Entity` has handed the caller a live handle to shared
state; an innocent caller-side mutation then corrupts another request's data and
surfaces as a heisenbug that only appears under load. A function that returns
`(nil, nil)` to mean "not found" forces every caller into a defensive nil-check,
and the day one caller forgets, a nil dereference panics the handler into an HTTP
500. This lesson treats pointers as design, not micro-optimization. At every API
boundary you will decide two things and encode them in the type signature so the
caller can see them: *who owns this memory*, and *who is allowed to mutate it*.
Read this file once; each of the exercises that follows is an independent
production artifact — a repository, a PATCH endpoint, a config hot-reloader, an
HTTP handler, a buffer-reuse encoder — that pins one facet of the contract with a
real `*_test.go`.

## Concepts

### A pointer is an ownership and lifetime contract, not a performance knob

The first instinct many engineers have about `*T` versus `T` is "pointers avoid a
copy, so they are faster." Sometimes true, usually irrelevant, and always the
wrong first question. The load-bearing question is about *aliasing and mutation*.
When you return a `*T`, you are telling the caller: this is a handle to state that
may be shared and may be mutated, and mutations through this handle are visible to
whoever else holds it. When you return a `T` (a value, or a defensive copy), you
are telling the caller: this is a snapshot you own outright; mutate it freely, it
will not affect the source. The signature is the contract, and the contract is
about *who may write*, not *how many bytes get copied*. Choose the return type to
say the true thing, then let performance follow from a benchmark, not a guess.

### The `(T, error)` miss contract: never `(nil, nil)`

A lookup that can miss should return the zero value together with a non-nil,
`errors.Is`-matchable sentinel — `(nil, ErrNotFound)`, `("", ErrNotFound)`,
`(0, ErrNotFound)` — never `(nil, nil)`. The `(nil, nil)` design pushes the
distinction between "absent" and "present" onto every single call site as a
pointer-nil check, in addition to the error check they already have to write.
That is two checks where one would do, and the redundant one is the one that gets
forgotten. Worse, `(nil, nil)` cannot express "a real error occurred while
looking up," so the moment your store grows a backend that can fail, the signature
is already wrong. Return `(zero, ErrNotFound)`; callers branch on
`errors.Is(err, ErrNotFound)` and touch the value only on the success path.

### Pointer to share, value (or defensive copy) to snapshot

Returning your internal `*Entity` from a repository `Get` leaks a live handle into
the caller. The caller did not ask to co-own your storage, but now it does: a
line as innocent as `e.Data["seen"] = "true"` reaches through the pointer and
mutates the object still sitting in your map, visible to the next request that
reads the same key. If a read path must not let callers mutate the source, return
a value or a defensive copy instead. The value return says "snapshot"; the pointer
return says "shared, mutable." Pick the one that matches what the caller is
actually allowed to do.

### Aliasing is the silent killer, and clones are shallow

Aliasing means two references point at the same backing array, map, or struct, so
a write through one is seen through the other. `maps.Clone` and `slices.Clone`
break aliasing — but only *one level deep*. `maps.Clone(m)` gives you a new map
header with the same key/value pairs; if those values are themselves maps, slices,
or pointers, the clone's entries still alias the originals. "Deep enough" is a
deliberate decision you make per type, not a property you get for free. For a
`map[string]string` a single `maps.Clone` is a true deep copy; for a
`map[string][]string` it is not, and you must clone each slice too.

### Constructors that validate return `(*T, error)`

A constructor that can reject its inputs returns `(*T, error)`: a non-nil pointer
on success, `(nil, err)` on failure. This mirrors the miss contract and gives the
same payoff — the caller branches on `err` alone and never has to nil-check the
pointer. A constructor that returns `(*T, nil)` on success and `(nil, nil)` on
failure re-introduces the redundant pointer check; one that returns a
half-constructed value on error hands the caller a landmine. Validate first,
build second, and make the returned pointer non-nil exactly when `err == nil`.

### Nil-receiver methods are legal, and that is a design tool

`e.Get(k)` where `e` is a nil `*Entity` does not panic *by itself*. A method call
is sugar for passing the receiver as the first argument: `Get(e, k)`. The call
only panics if the *body* dereferences the nil receiver. So a method can guard
`if e == nil { return zero }` at the top and become a documented, intentional
no-op on nil. This is not a party trick — it lets a caller hold a possibly-nil
handle and call read methods without a nil-check at every call site, as long as
the "nil means empty" behavior is part of the type's contract. Use it
deliberately, document it, and test it.

### Pointer struct fields model tri-state (absent vs explicit-zero)

A value-typed field cannot distinguish "the client did not send this field" from
"the client set this field to its zero value." A `bool` that is `false` might mean
"set to false" or "omitted." A pointer field solves this: `nil` means absent, a
non-nil pointer means explicitly set — even when it points at the zero value.
This is precisely why partial-update DTOs and JSON PATCH bodies use `*string`,
`*int`, `*bool` instead of the value types. `Apply` writes only the fields whose
pointer is non-nil, so an omitted field is left unchanged while a field explicitly
set to `""`, `0`, or `false` is written.

### Method sets and addressability decide interface satisfaction

A value of type `T` has only the value-receiver methods in its method set; `*T`
has both value- and pointer-receiver methods. So if `Validate()` is defined on
`*T`, then `*T` satisfies a `Validator` interface but `T` does not. The usual
rescue — "just take the address" — fails in exactly the places you hit this: a
value stored in a map, or returned from a function, is *not addressable*, so you
cannot write `&m["k"]` to promote it. The fix is structural: store pointers
(`map[string]Validator` holding `*T` values) when the interface's methods have
pointer receivers. A compile-time assertion `var _ Validator = (*T)(nil)` documents
and enforces which form satisfies the interface.

### `atomic.Pointer[T]` publishes immutable snapshots lock-free

For read-heavy shared state like a hot-reloadable config, an
`atomic.Pointer[Config]` beats a mutex. Readers call `Load()` and get a complete,
consistent `*Config` with no lock and no blocking; a writer builds an entirely new
`Config` value, then `Store()`s the pointer, swapping the whole thing atomically.
The invariant that makes this safe is *immutability after publication*: once a
snapshot is stored, it is never mutated again. A reader either sees the old
pointer or the new one, never a half-updated struct, because the only thing that
changed was a single pointer word. `CompareAndSwap` extends this to conditional
updates (swap only if the current pointer is still the one you based your new
value on).

### Returning a pointer costs a heap escape; `sync.Pool` amortizes it

Escape analysis will often move a value to the heap precisely because you return a
pointer to it — the compiler cannot prove the pointer does not outlive the stack
frame, so it heap-allocates, adding GC pressure. On a hot path that returns many
short-lived pointers (buffers, request objects), this allocation shows up in
profiles. `sync.Pool` amortizes it by reusing objects across calls: `Get` a
buffer, `Reset` it, use it, `Put` it back. The discipline is strict — a pooled
object must be `Reset` before reuse and must *never* be retained or handed to a
caller after `Put`, or a later `Get` hands the same object to two owners and you
have a use-after-free-style data race. Copy out what the caller needs before you
`Put`.

### Immutable-plus-atomic beats mutex-guarded mutation for read-heavy state

Tie the last threads together: for state that is read constantly and written
rarely, "build a new immutable snapshot and atomically swap the pointer" is
better than "hold a lock and mutate in place." Readers never block and never
observe a torn write, because they only ever dereference a pointer to a finished
value. The cost is an allocation per write, which is exactly the case where you do
not care, because writes are rare.

## Common Mistakes

### Returning `(nil, nil)` for "not found"

Wrong: `func Get(id) (*Entity, error)` that returns `(nil, nil)` on a miss.
Callers cannot tell a miss from a real error, and must add a redundant nil-check
that eventually gets forgotten. Fix: return `(nil, ErrNotFound)` and match with
`errors.Is`.

### Leaking your internal `*Entity` from a read path

Wrong: `Get` returns the exact `*Entity` stored in the map; a caller mutates it
and corrupts stored state under concurrent load. Fix: for read paths that must not
be mutated, return a value or a defensive copy (`maps.Clone` the maps).

### Believing `maps.Clone`/`slices.Clone` is deep

Wrong: cloning a `map[string][]string` with `maps.Clone` and mutating a slice in
the copy — the slices still alias the original. Fix: clone each nested reference
level you actually need isolated; "deep enough" is a per-type decision.

### Modeling optional PATCH fields with value types

Wrong: an update DTO with `string`/`int`/`bool` fields; `Apply` cannot tell
"omitted" from "set to zero," so a PATCH silently overwrites fields the client
never sent. Fix: use `*string`/`*int`/`*bool` and write only non-nil fields.

### Dereferencing a lookup result before checking the error

Wrong: an HTTP handler that reads `e.Name` before checking `errors.Is(err,
ErrNotFound)`; an expected 404 miss becomes a nil-pointer panic and a 500. Fix:
check the error first, write 404 on a miss, and touch pointer fields only on the
non-nil hit path.

### Storing value types under an interface whose methods have pointer receivers

Wrong: `map[string]Validator` holding `T{}` values when `Validate` is on `*T`; the
type does not satisfy the interface, and you cannot take `&m["k"]` to fix it
because map values are not addressable. Fix: store `*T`.

### Mutating an `atomic.Pointer` snapshot after publishing it

Wrong: `Load()` the config, then mutate the struct it points at — every other
reader sees the mutation, torn or not. Or: `Load()` once and hold the pointer
across a reload, expecting to see updates. Fix: never mutate a published snapshot;
re-`Load()` to observe a reload.

### Handing a `sync.Pool` object to a caller, or forgetting to `Reset`

Wrong: returning `buf.Bytes()` (a view into the pooled buffer) and then `Put`ting
the buffer — a later `Get` reuses it and corrupts the caller's slice; or reusing a
pooled buffer without `Reset`, leaking stale bytes. Fix: copy out what you need
before `Put`, and always `Reset` on reuse.

### Publishing a config built by mutating a still-shared struct

Wrong: `Store`/`Swap` a value you assembled by mutating a struct other readers can
already see, so they briefly observe a partially built config. Fix: build the
whole snapshot in a fresh value, then publish it in one atomic store.

### Copying a struct that embeds a `sync.Mutex`/`sync.Pool`/`atomic.Pointer`

Wrong: returning such a struct by value, or ranging over a slice of them by value —
these types must not be copied after first use, and `go vet` flags it. Fix: hold
and pass them by pointer.

Next: [01-pointer-safe-repository.md](01-pointer-safe-repository.md)
