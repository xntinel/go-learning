# Pointers and Function Parameters: Passing Semantics in Backend Code — Concepts

This lesson looks like it is about a language feature. It is actually about one
question that separates a correct backend service from one riddled with aliasing
bugs and data races: who owns this data, and who is allowed to mutate it. Every
time you write a function signature you answer that question, whether you mean to
or not. A senior engineer chooses `*T` versus `T` for an observable contract —
does my caller see the mutation? — for allocation cost in a hot path, and for
concurrency safety, not because "pointers are faster" folklore said so. Read this
file once; it is the conceptual foundation for the nine independent exercises that
follow, each a real production artifact: a config loader, a PATCH handler, the
functional-options constructor every Go server library uses, a repository
row-scanner, a normalization pipeline, a lock-free hot-reloading config, an
optional-dependency no-op, a pooled encoder, and a measured copy-cost benchmark.

## Concepts

### Pass-by-value is Go's only calling convention

Go has exactly one way to pass an argument: it copies. A `func f(c Config)` copies
the whole `Config`; a `func f(c *Config)` copies the *pointer* — the address, one
machine word. That copied address still points at the caller's value, which is the
entire mechanism by which a callee mutates something the caller can see. There is
no "pass by reference" keyword; a pointer parameter *is* the reference. So the
first thing to internalize is that "pointer parameter" is not a special passing
mode — it is an ordinary by-value copy of an address, and the shared mutation is a
consequence of the address being shared, not of any special calling convention.
The same reasoning explains shared identity: two pointers to the same value compare
equal and see each other's writes; two copies of a value are independent.

### Pointer versus value is an API contract, not a micro-optimization

The most common mistake senior engineers still make is treating `*T` as a
performance dial. It is first a contract. A `*T` parameter announces to the reader:
"this function may mutate my data, and I will observe the change." A `T` parameter
promises: "your original is untouched; take my return value if you want the
result." When you reach for `*T` on a small struct purely to avoid a copy, you have
silently weakened that immutability promise, and you may not even have made the code
faster — a pointer indirection plus a possible heap escape can cost more than
copying a few words. Decide the contract first. Optimize second, and only with a
measurement.

### Three ways to hand data back to the caller

There are exactly three shapes, and each encodes a different ownership story.
`func() T` — return a value — is the cleanest for transform-without-mutation: the
caller's input (if any) is untouched and it takes a fresh result. `func(*T)` —
mutate through a pointer parameter — changes the caller's value in place with no
new allocation, and is the right tool when the caller *wants* that side effect
(applying defaults, scanning a row, folding an option). `func() *T` — return a
pointer — hands back a heap value the caller will mutate or share; use it when the
result has identity that must be shared, not merely a value that is copied. Picking
among the three is a design decision about who owns the resulting value, not a
style preference.

### Optional and PATCH fields need pointer fields

A partial-update handler has to distinguish "the client did not mention this field"
from "the client explicitly set this field to its zero value." A plain `string`
field cannot: after `json.Unmarshal`, an omitted `email` and an `email: ""` both
leave the field as `""`. A `*string` field distinguishes them — omitted stays
`nil`, present decodes to a non-nil pointer (even one pointing at `""`). This is why
PATCH DTOs use `*string`, `*bool`, `*int`: the pointer's nil-ness carries the
"present or absent" bit that a value field collapses. Get this wrong and your PATCH
handler silently overwrites stored data with zero values on every request.

### Functional options: the idiomatic Go constructor

Almost every Go server or client library constructs with variadic options:
`New(addr string, opts ...Option)` where `type Option func(*config)`. Each option
is a closure that mutates a config through a pointer parameter; the constructor
starts from defaults and folds the options over a pointer to its internal config.
This gives extensible, backward-compatible construction with sane defaults, without
a twelve-argument signature or a separate builder object. The pointer parameter is
load-bearing: the options must mutate the *one* internal config, not copies, or the
last option would win by accident and earlier ones would vanish.

### database/sql Rows.Scan structurally requires pointers

`Rows.Scan(dest ...any)` is the canonical real-world reason a stdlib API demands
pointers. Scan decodes each column and writes it *back through* the address you
pass: `rows.Scan(&u.ID, &u.Name)`. Write-through is impossible with a value copy —
if you passed `u.ID` instead of `&u.ID`, Scan would receive a copy of the int and
have nowhere to put the decoded value (in practice a runtime error, because Scan
requires a pointer). Nullable columns take a `*string` (or `sql.NullString`) so a
SQL NULL can map to a nil pointer distinct from an empty string. This is the same
"pointer as write-target" idea as the mutate-in-place parameter, made concrete by
the standard library.

### Ranging over a slice copies each element

`for _, r := range recs` copies each element into the loop variable `r`. Mutating
`r` writes to the copy and is lost — the slice never changes. This is the single
most common silent-write-drop bug in normalization and enrichment pipelines. To
mutate in place you index (`recs[i].Field = ...`) or take the element's address
(`p := &recs[i]`). A slice of pointers `[]*T` sidesteps it: the loop variable is
itself a pointer, so mutating through it reaches the underlying value. Note that
Go 1.22 made the loop variable per-iteration, which fixed the notorious
closure-capture aliasing bug — but it did NOT change that ranging hands you a copy
of each element. Per-iteration scoping and element-copying are different things;
conflating them is a trap.

### For concurrency, swap an immutable snapshot behind an atomic.Pointer

When multiple goroutines read a config while one goroutine reloads it, mutating the
shared struct's fields in place is a data race — even for a single field, even if it
"feels atomic." The race detector will flag it, and readers can observe a torn
state where some fields are new and others old. The fix is to never mutate the
shared value: build a fresh, immutable `*Config` and publish it with
`atomic.Pointer[Config].Store`. Readers call `Load()` for a consistent snapshot with
no lock; the writer swaps the whole pointer atomically. `CompareAndSwap(old, new)`
gives a compare-and-set reload guard (publish only if nobody else reloaded first),
and it compares pointer identity, not field values. Snapshot-and-swap turns a
shared-mutable-state problem into an immutable-value problem.

### A nil pointer receiver is callable

A method with a pointer receiver can be called on a nil pointer and runs fine, as
long as it does not dereference the receiver. `func (m *Metrics) Inc()` can begin
`if m == nil { return }` and then callers may inject a nil `*Metrics` to disable
instrumentation without nil-guarding every call site. The method set of `*T`
includes nil; the panic only comes when you read a field of the nil receiver. This
"optional dependency" pattern — a nil logger, metrics sink, or tracer that no-ops —
removes a forest of `if m.metrics != nil` guards from your business logic. The
boundary to respect: any method that touches a field must either guard nil first or
document that nil is not a valid receiver for it.

### Copy cost is real but measurable, and escape analysis complicates it

Large structs genuinely are cheaper to pass by pointer: passing a `Big` with an
embedded 256-element array by value copies two kilobytes on every call. But the
folklore "always pass big structs by pointer" hides a caveat: returning a pointer to
a local can force a heap allocation that the value form would have kept on the
stack, and a pointer parameter can inhibit optimizations the compiler would apply to
a value. The honest answer is to measure with a benchmark — `for b.Loop()` (Go 1.24)
plus `b.ReportAllocs()` — and to read the escape decisions with
`go build -gcflags=-m`, rather than deciding by reflex. Fold the result into a sink
so the compiler cannot delete the call you are trying to measure.

### sync.Pool must hold pointers, not values

`sync.Pool` stores `any`. If you `Put` a value type, it is boxed — re-allocated into
an interface — which defeats the point of pooling. So a buffer pool stores
`*bytes.Buffer`, not `bytes.Buffer`: `Get`/`Put` move a pointer with no copy. The
second discipline is Reset-on-checkout: a pooled buffer still holds the previous
user's bytes, so you `Reset()` it right after `Get` and before writing, or you leak
one request's data into the next response. New returns `*bytes.Buffer`; Get, Reset,
use, then `defer Put`.

## Common Mistakes

### Reaching for *T as a performance reflex on a small struct

Wrong: making every parameter `*T` "to avoid copies," even a three-word struct. The
pointer indirection plus a possible heap escape can be slower than copying a few
words, and it weakens the immutability contract of the API.

Fix: default to `T` for small values; switch to `*T` when you need mutation
visibility, shared identity, or a benchmark proves the copy matters.

### Using a value field where a pointer field is required for optionality

Wrong: a PATCH DTO with a plain `Email string`. After unmarshal it cannot tell
"client omitted email" from "client cleared email to `""`," so it overwrites stored
data with the zero value.

Fix: `Email *string`. `nil` means absent (skip it); a non-nil pointer means present
(apply it, even if it points at `""`).

### Mutating the loop variable and expecting the slice to change

Wrong: `for _, r := range recs { r.Email = norm(r.Email) }`. The write lands on the
per-iteration copy and is discarded; the slice is unchanged.

Fix: index with `recs[i].Email = ...` or take `p := &recs[i]`, or range a `[]*Record`
whose loop variable is already a pointer.

### Calling Scan with values instead of addresses

Wrong: `rows.Scan(u.ID, u.Name)`. Scan has nowhere to write the decoded columns; it
fails at runtime (or the write reaches a throwaway copy).

Fix: pass addresses — `rows.Scan(&u.ID, &u.Name)` — so Scan writes through into your
struct. Nullable columns take `*string` or `sql.NullString`.

### Returning a pointer into your private state

Wrong: a getter that returns `&s.internal`, letting callers reach in and mutate your
unsynchronized private field.

Fix: return a value (a copy) or a freshly built pointer the caller may own. Hand out
identity only when you intend shared mutation.

### Mutating shared fields from multiple goroutines

Wrong: reloading config by assigning `cfg.Host = newHost` while handlers read
`cfg.Host`. That is a data race even for one field; `-race` flags it and readers can
see a half-updated struct.

Fix: build a fresh immutable `*Config` and publish it with
`atomic.Pointer.Store`/`CompareAndSwap`; readers `Load()` a consistent snapshot.

### Dereferencing a pointer parameter that may legally be nil

Wrong: a method that reads `m.field` when nil is a valid input (a disabled metrics
sink, an uninitialized `*Config`). Classic production nil-pointer panic.

Fix: begin methods that accept a nil receiver with `if m == nil { return }` (or a
zero value), so nil is a safe no-op.

### Storing values in a sync.Pool or forgetting Reset

Wrong: `pool.Put(buf)` where `buf` is a `bytes.Buffer` value (boxing re-allocates on
every Put), or checking a buffer out without `Reset()` (the previous request's bytes
leak into this response).

Fix: pool `*bytes.Buffer`; `Reset()` on checkout, before writing.

### Benchmarking value-vs-pointer with the old loop and no allocs

Wrong: `for i := 0; i < b.N; i++` with no `ReportAllocs`, letting the compiler
optimize the whole call away because the result is unused.

Fix: `for b.Loop()` with `b.ReportAllocs()`, and accumulate the result into a
package-level sink so the measured work is not deleted.

### Assuming Go 1.22 loop-variable scoping removed the need to index

Wrong: believing per-iteration loop variables mean `for _, r := range recs` can now
mutate the slice.

Fix: 1.22 fixed closure *capture*; ranging still hands you a *copy* of each element.
Index or take `&recs[i]` to mutate in place.

Next: [01-config-defaults-mutate-vs-return.md](01-config-defaults-mutate-vs-return.md)
