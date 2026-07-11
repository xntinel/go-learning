# Pointer Basics for Backend Services — Concepts

Pointers are where correctness and cost meet in Go service code, and a senior
engineer is expected to reason about both without hand-waving. A pointer is not
an advanced feature to be admired; it is the everyday tool that decides four
concrete things you build constantly: whether a function mutates the caller's
data or a private copy, whether a DTO can tell "the operator left this field
absent" apart from "the operator set it to zero", whether a repository can signal
"not found" without inventing a sentinel struct, and whether a fat request is
copied byte-for-byte on every call. Get these right and a service degrades
predictably; get them wrong and you ship nil-deref panics in a hot handler,
silent config defaults that clobber an explicit zero, lost metric updates from a
value copy, and the `&v`-in-range aliasing bug that survives even Go 1.22 loop
scoping. This file is the conceptual foundation; read it once and you have
everything you need to reason through each of the ten independent exercises that
follow.

## Concepts

### A pointer holds an address; & and * are inverses

A pointer holds the address of a value. The address-of operator `&` takes a `T`
and yields a `*T`; the dereference operator `*` takes a `*T` and reads or writes
the `T` at that address. On an addressable value the two operators are inverses:
`*(&x) == x`. Concretely, if `s := Service{Name: "api"}`, then `&s` is a
`*Service` holding `s`'s address, and `*(&s)` is `s` again. Writing `*p = v`
stores `v` at the address `p` points to, so a later read of the original variable
sees `v`. This round trip — take an address, dereference it, get the same value
back, write through it, see the original change — is the entire mechanical model.

### *T is a distinct, compiler-enforced type

`*Service` is not `Service`. It is a separate type, and the compiler refuses to
assign a `Service` to a `*Service` without `&`, or to use a `*Service` where a
`Service` is required without `*`. This is why `var p *Service; p = Service{}`
does not compile: you must write `p = &s`. Do not confuse the two syntactic uses
of `*`. In a *type* position — `var p *Service`, `func f() *Service` — the `*`
means "pointer to". In an *expression* position — `*p`, `*p = v` — the `*` means
"dereference". They are the same character but unrelated operations, and reading
`*Service` as "dereference Service" is a category error.

### Value semantics copy; pointer semantics share

This is the single most consequential pointer decision in backend code. Passing a
`T` to a function copies the whole struct; the callee mutates its own copy and the
caller's value is untouched. Passing a `*T` shares the original; the callee
mutates the caller's data through the pointer. `func incVal(s Service) Service`
increments a copy and hands it back — the original is unchanged. `func incPtr(s
*Service)` increments the caller's own `Service`. The same split governs whole
categories of design: an accumulator, a metrics counter, or a buffer that several
callers update *must* be shared by pointer, or updates land on throwaway copies
and silently vanish. That same sharing is exactly what later creates aliasing and
data races, so it is a tool to reach for deliberately, not reflexively.

### The zero value of a pointer is nil, and nil is a signal

The zero value of any pointer type is `nil`. Dereferencing a `nil` pointer panics
with a runtime nil-pointer-dereference — an unrecovered panic that takes down the
goroutine (and, in a naive handler, the request). Every dereference of a
possibly-nil pointer needs a guard: check `p != nil` before `*p`. But `nil` is not
only a hazard; it is idiomatic signal. A repository's `FindByID(id) (*Record,
error)` returns a `nil` `*Record` for a miss, and the caller nil-checks before
dereferencing. An optional config field is a `*int` that is `nil` when the
operator did not set it. Treating `nil` as "not found / not set" is standard Go,
and the discipline it demands is: guard before you deref, especially on repository
returns and deeply-nested optional config chains.

### nil vs zero is the reason pointer fields exist in DTOs

A plain `int` field cannot distinguish "the client omitted this" from "the client
sent 0". Both arrive as `0`. A `*int` can: `nil` means absent, and a non-nil
pointer to `0` means "explicitly zero". This is the backbone of correct PATCH
semantics (touch only the fields the client provided), of default merging (an
operator's explicit `max_conns: 0` must beat the built-in default, not be treated
as "unset"), and of idempotent config reloads. Model a field as `*T` precisely
when absent-vs-zero carries meaning; model it as `T` when it does not. Getting this
wrong means a service silently applies its default over an operator's deliberate
`0`, `false`, or `""`, which is a data-integrity bug, not a style nit.

### Escape and cost: measure, do not guess

Passing a small struct by value is often the *faster* choice: no indirection, better
cache locality, and it can avoid a heap escape that taking `&` would have forced.
Passing a large struct by pointer avoids copying `unsafe.Sizeof(T)` bytes on every
call. The crossover is a real, measurable thing, not folklore — decide it with
`unsafe.Sizeof` to know the struct's width and a benchmark (`for b.Loop()` with
`b.ReportAllocs()`, the Go 1.24 benchmark loop) to see the per-call cost, rather
than reflexively "passing everything by pointer for performance". Go's escape
analysis makes `&local` safe to return — it heap-allocates when the value outlives
the frame — so you never reason about stack placement by hand for correctness;
you reason about the copy for cost.

### Addressability drives map[string]*T vs map[string]T

You can take the address of a variable, a slice element (`&s[i]`), a struct field,
or an array element. You *cannot* take the address of a map element: `&m[k]` is a
compile error, and `m[k].Field = x` does not compile either, because a map value
is not addressable (the map may relocate its buckets). This is the design reason a
mutable-in-place map stores pointers: `map[string]*Stats` lets you fetch `m[k]` and
mutate `*m[k]` in place, while `map[string]Stats` forces you to read the whole
value, modify the copy, and write it back. When callers need to accumulate into a
shared per-key record, the map holds `*T`.

### Range variables are copies; Go 1.22 did not change that

`for i, v := range items` binds `v` to a *copy* of `items[i]` each iteration.
Taking `&v` gives you the address of that per-iteration copy, not the address of
the backing-array element — so a `[]*Item` built from `&v` points at loop copies,
not at the slice. To point into the backing array, use `&items[i]`. Go 1.22 made
the loop variable per-iteration (fixing the classic goroutine/closure capture bug
where every closure saw the last value), but it did *not* make `&v` alias the
element. Under 1.22+ each `&v` is a distinct address of a distinct copy — the bug
changed shape from "all pointers alias the same variable" to "every pointer aliases
its own disconnected copy", and it is just as wrong for building an index.

### Whole-value writes through a pointer

`*p = T{}` overwrites the entire pointed-to value in place. This is how you reset
an accumulator or swap a buffer for a fresh one without re-pointing the caller's
variable: `Flush(dst *Batch)` can copy `*dst` out and then execute `*dst = Batch{}`
so the caller's own `Batch` is now empty and ready to fill again. The caller keeps
using the same variable; only its contents changed. Because a slice field is a
header (pointer, len, cap), `*p = Batch{}` zeroes the header — the caller's variable
now sees an empty slice, while any copy taken *before* the reset still references
the old backing array.

### json.Unmarshal into a *T field

`encoding/json` gives pointer fields exactly the absent-vs-zero-vs-null semantics
you want. An absent key leaves the pointer field untouched (so, starting from a
zero struct, it stays `nil`). A JSON `null` sets the pointer to `nil`. A present
value allocates a new `T` (if the pointer was `nil`) and fills it. So `{}` leaves
`MaxConns *int` as `nil` (use the default), `{"max_conns": 0}` gives a non-nil
`*int` to `0` (explicit override to zero), and `{"max_conns": null}` gives `nil`.
This is the mechanism behind every absent-vs-zero config overlay and partial-update
handler in the exercises.

## Common Mistakes

### Confusing the two meanings of *

Wrong: reading `*Service` in `var p *Service` as a dereference. Fix: the `*` in a
type position declares a pointer type ("pointer to Service"); the `*` in an
expression (`*p`) dereferences. They are unrelated, syntactically-overloaded uses
of the same character.

### Dereferencing a nil pointer without a guard

Wrong: `*p` (or `p.Field`, which implicitly dereferences) when `p` may be `nil`,
producing a runtime panic instead of a handled not-found. Fix: nil-check before
every dereference of a possibly-nil pointer — repository returns and optional
config chains are where this bites in production.

### Forgetting & when a function takes *T

Wrong: `incrementPointer(s)` where `s` is a `Service` value and the function takes
`*Service`. The compiler rejects it. Fix: pass the address, `incrementPointer(&s)`.

### Using a value field where absent-vs-zero matters

Wrong: a plain `int MaxConns` field, so an operator's explicit `0` is
indistinguishable from "unset" and the default silently wins. Fix: use `*int`, and
treat `nil` as absent and a pointer-to-`0` as an explicit override.

### Taking &v of a range variable expecting it to alias the element

Wrong: `for _, v := range items { idx = append(idx, &v) }` expecting the pointers
to alias `items`. You get the addresses of per-iteration copies. Fix: index the
backing array with `&items[i]`.

### Storing structs (not pointers) in a map and mutating in place

Wrong: `map[string]Stats` with `m[k].Count++` or `&m[k]` — neither compiles,
because a map element is not addressable. Fix: `map[string]*Stats`, then
`m[k].Count++` mutates through the stored pointer; or read, modify a copy, and
reassign the whole value.

### Reflexively passing everything by pointer "for performance"

Wrong: taking `*T` on small structs everywhere. It adds indirection, hurts cache
locality, and can force a heap escape a value would have avoided. Fix: decide by
size and mutation need, backed by `unsafe.Sizeof` and a `for b.Loop()` benchmark.

### Reasoning about stack vs heap by hand for correctness

Wrong: "I can't return `&local`, it lives on the stack." Go's escape analysis
heap-allocates `local` when its address outlives the frame, so `return &local` is
safe. Fix: return the pointer freely; reason about the *copy* for cost, never about
placement for correctness.

### Comparing pointers when you meant to compare values

Wrong: `p1 == p2` to test whether two records are equal. That tests address
identity — true only if they are the same object. Fix: `*p1 == *p2` compares the
pointed-to values. Mixing these makes a test pass or fail for the wrong reason.

### Assuming Go 1.22 loop scoping fixed the &v-into-slice trap

Wrong: believing per-iteration loop variables mean `&v` now aliases the backing
array. It fixed shared-variable capture in closures/goroutines, not the fact that a
range value is a copy whose address is not the element's address. Fix: use
`&items[i]` when you need a pointer into the slice.

Next: [01-service-registry-pointer-mechanics.md](01-service-registry-pointer-mechanics.md)
