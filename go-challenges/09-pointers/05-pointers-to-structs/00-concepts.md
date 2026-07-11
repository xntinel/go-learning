# Pointers To Structs: Sharing And Guarding Mutable State — Concepts

A `*T` is the currency of shared mutable state in a Go backend service. Almost
every non-trivial type a senior engineer ships — a repository, a cache, a config,
a request builder, a tree node — is handed around as a pointer so that many
callers can read and mutate one underlying struct instead of passing fat copies.
That power is exactly where the most expensive class of production bug lives.
Getting the value-vs-pointer boundary wrong leaks internal state out of a
repository, makes a PATCH endpoint unable to tell "leave this field unchanged"
from "set it to zero", corrupts a batch of records through loop-variable capture,
and turns a pointer-linked cache into a silent memory leak or a data race. This
file treats `*struct` not as a syntax feature but as an API-design and
memory-ownership decision: who owns the struct, who may mutate it, who must be
handed a copy, and when a `*T` field encodes an optionality that a value field
simply cannot represent. Read it once; the nine independent exercises that follow
each build a real backend artifact where these decisions bite.

## Concepts

### Ownership: a `*T` names one struct that many holders can mutate

A pointer is a small value (a machine word) that names the address of a single
struct. When you write `p := &s`, `p` does not contain a copy of `s`; it points
at the one `s` that already exists. Every holder of that pointer reads and writes
the same fields. This is the entire reason a service keeps `map[string]*User`
rather than `map[string]User`: the caller who does `u, _ := svc.Get(id)` and the
map both name the same `User`, so a later `u.Profile.DisplayName = "Alice"` is
seen by the store too. That shared visibility is a feature when it is intended
and a bug when it is not — which is why the first design question for any type is
not "value or pointer?" as a matter of taste but "who owns this struct, and who
is merely borrowing a view of it?" Ownership is the model; the pointer is only
the mechanism.

### A `*T` field is a reference; a `T` field is an embedded copy

When a struct has a field of type `T`, that field is stored inline and travels
with the parent: copying the parent copies the field, and mutating one copy's
field does not touch the other. When the field is `*T`, the parent stores only a
pointer, and every copy of the parent shares the one pointed-at `T` — a mutation
through any of them is visible through all of them. In a `User` type, a small
immutable `Profile` is a natural value field (it is cheap to copy and there is no
reason to share it), while a `Manager *User` is a pointer field because the
manager is a distinct, independently-mutable entity that other users also point
at. Choose by mutability and size, not by habit: a value field for small,
copy-cheap, self-contained data; a pointer field when the data is large, must be
shared, or must be nil-able (see optionality below).

### Copying a struct copies slice and map headers, not their backing storage

The single most surprising fact about "value semantics" in Go is that they are
shallow. A slice value is a three-word header (pointer, length, capacity); a map
value is a pointer to a runtime hash table. Copying a struct that contains a
`[]Item` field copies the header but not the backing array, so the original and
the copy share the same elements: `b := a; b.Items[0] = x` mutates `a.Items[0]`
too. A struct copy therefore does *not* give you an independent object when the
struct has slice or map fields — it gives you an object that still aliases the
originals' storage. To truly break the aliasing you must clone those fields
explicitly (`slices.Clone(a.Items)`, a fresh `map`, and recursively for nested
slices). This is the mechanism behind the classic repository bug: even a
`return *storedPtr` (a value copy) hands the caller a struct whose `Items` slice
still points into the store.

### A `*T` encodes three-state optionality a value field cannot

A value field has exactly the states its type allows: a `string` is either some
text or the empty string, an `int` is some number or zero. There is no room to
say "not provided". A pointer adds a third state: `nil` means absent, and a
non-nil pointer means present-and-here-is-the-value — including present-and-empty.
This is the canonical way to implement HTTP PATCH / partial-update semantics. A
`ProfilePatch{DisplayName *string}` distinguishes three JSON bodies: `{}` (field
omitted, pointer nil, leave unchanged), `{"display_name": ""}` (present and
empty, pointer to `""`, clear the field), and `{"display_name": "Alice"}` (set
it). A plain `string` field collapses the first two into "empty string", so a
PATCH that omits `display_name` would wipe the stored name. `encoding/json`
leaves a pointer field nil when its key is absent and allocates one when the key
is present, which is exactly the three-state behavior PATCH needs.

### Addressability: `&s.Field` and `&slice[i]` are stable; range values are copies

You can take the address of anything addressable: a variable, a struct field of
an addressable struct, an element of a slice (`&xs[i]`), or a pointer
dereference. Those addresses are stable — `&xs[i]` points into the slice's
backing array, so mutating through it changes the element in place. What you
cannot address usefully is a *copy*. In `for _, v := range xs`, `v` is a fresh
variable holding a copy of each element; `&v` is the address of that loop
variable, not of `xs[i]`. Go 1.22 gave `v` a new instance each iteration (so
`&v` values from different iterations differ), which fixed the old "every pointer
equals the last element" bug — but `&v` still points at a *copy* of the element,
not at the source in the slice. To build a `map[K]*T` index that points into the
slice, you must take `&xs[i]`, not `&v`. Map elements are the exception to
addressability: `&m[k]` is a compile error because rehashing can move entries, so
you cannot point into a map at all.

### Pointer receivers mutate in place; value receivers mutate a discarded copy

A method with a pointer receiver `func (c *Config) SetPort(p int)` operates on the
addressed struct, so its writes persist. A method with a value receiver
`func (c Config) SetPort(p int)` operates on a copy made at call time; its writes
vanish when the method returns and the caller sees nothing change. This is why a
mutating method must use a pointer receiver, and why a fluent builder whose
methods return the receiver for chaining must use pointer receivers — a value
receiver would mutate and return copies, and each link in the chain would lose
the previous one's work. It is also why a constructor for a type that will be
mutated after construction returns `*T`: `New() *Config` hands back one struct the
caller keeps mutating, rather than a copy that a mutating method could not affect.

### Escape and identity: returning `&local` is safe; `p1 == p2` means same struct

Returning the address of a local variable is safe in Go — the compiler's escape
analysis detects that the variable outlives the function and allocates it on the
heap instead of the stack, so `return &User{...}` never dangles the way it would
in C. Pointer identity is meaningful and testable: `p1 == p2` is true exactly
when both name the same underlying struct. That is why a repository contract like
"`Get` returns the same pointer that was `Add`ed" is worth a test asserting
`got == u`, and why comparing two structs with `==` (field-by-field equality,
which *panics* if any field is uncomparable like a slice or a func) is a
different question from comparing two pointers with `==` (address equality). Know
which one you mean.

### Pointer-linked data structures are built from `*node` fields

A doubly linked list node is `struct{ prev, next *node }`; a tree node is
`struct{ left, right *node }`. These self-referential pointer fields are how you
build LRU caches, linked lists, and trees. The discipline they demand is exact
prev/next/parent wiring on insert and — the part everyone forgets — nulling those
links on removal. An evicted LRU node whose `prev`/`next` still point into the
live list keeps those nodes reachable (a leak) or leaves a dangling reference the
next operation trips over. Correctness here is pointer bookkeeping, not algorithm
cleverness.

### Pooling a `*T` demands a Reset discipline

`sync.Pool` amortizes allocation by letting you borrow a `*T`, use it, and return
it for the next caller instead of letting the GC reclaim it. The catch is that a
pooled object arrives in whatever state the previous user left it. A
`*bytes.Buffer` fetched from a pool still holds the last response's bytes unless
you `Reset()` it; a pooled request struct still holds the last request's fields.
Forgetting the scrub leaks one request's data into the next — a correctness and
sometimes a security bug. The lifecycle is always: `Get`, type-assert, use, copy
out anything you need to keep, `Reset`, `Put`.

### The pointer itself provides no synchronization

A shared `*T` is a shared mutable memory location, and Go's memory model gives you
no free synchronization just because access goes through a pointer. If two
goroutines mutate the same struct's fields, you need a `sync.Mutex`, a
`sync.RWMutex`, or atomics around those mutations; the pointer only tells you they
are touching the same bytes, not that they do so safely. A repository's map of
pointers therefore needs a lock for the map *and* the code that mutates the
pointed-at structs needs its own guarding. And a `sync.Mutex` embedded in a struct
must never be copied — copying the struct copies the lock's state and breaks
mutual exclusion; `go vet`'s copylocks check catches the common cases.

## Common Mistakes

### Returning an internal `*T` straight from a repository Get

Wrong: `func (r *Repo) Get(id string) *Order { return r.m[id] }`. The caller now
holds the store's own pointer and any mutation — `o.Status = "shipped"` — silently
rewrites the stored order.

Fix: return a defensive deep copy (clone slice/map fields too) or a read-only
view. The caller mutates its own copy; the store is untouched.

### Treating a struct value copy as fully independent

Wrong: `b := a` where `a` has an `Items []Item` field, then mutating `b.Items[0]`
and expecting `a` to be untouched. The copy shares the backing array.

Fix: clone the composite fields explicitly — `b.Items = slices.Clone(a.Items)` —
after the shallow struct copy. Recurse for nested slices/maps.

### Using a zero value to mean "not provided" in a PATCH path

Wrong: `type Patch struct{ DisplayName string }` and applying it always. A body
that omits `display_name` decodes to `""` and wipes the stored name.

Fix: `DisplayName *string`; apply only when non-nil. `nil` means "leave
unchanged", non-nil means "set to this value (even empty)".

### Storing `&v` from a range loop to build a slice or map of pointers

Wrong: `for _, v := range xs { idx[v.ID] = &v }`. Every entry points at a *copy*
of the element (the loop variable), not the element in `xs`, so mutating through
the index never touches `xs`.

Fix: `for i := range xs { idx[xs[i].ID] = &xs[i] }`. `&xs[i]` addresses the real
element in the backing array.

### A value receiver on a method meant to mutate

Wrong: `func (c Config) SetPort(p int) { c.port = p }`. The write lands on a copy
and the caller sees no change.

Fix: `func (c *Config) SetPort(p int)`. A pointer receiver mutates the addressed
struct. This is also mandatory for a fluent builder that returns the receiver.

### Embedding a large or mutable struct by value in a hot path

Wrong: passing a big struct by value through a hot loop, copying every field on
every call — both an allocation/perf cost and, if the struct has slice fields, a
correctness trap because the copies still alias.

Fix: share a `*T`. Pass the pointer; mutate through it under a lock if concurrent.

### Forgetting to Reset a `*T` fetched from sync.Pool

Wrong: `buf := pool.Get().(*bytes.Buffer); buf.WriteString(...)` without a
`buf.Reset()` first. The buffer still holds the previous borrower's bytes.

Fix: `buf.Reset()` immediately after `Get` (or before `Put`), so every borrow
starts clean.

### Confusing struct `==` with pointer `==`

Wrong: comparing two struct values with `==` to test identity (it tests
field-by-field equality and panics on uncomparable fields), or comparing two
pointers with `==` when you meant to compare contents.

Fix: pointer `==` for identity (same underlying struct), value `==` for equality
(and only on comparable structs). Know which question you are asking.

### Forgetting to null links when removing a pointer-linked node

Wrong: unlinking an LRU or tree node by rewiring its neighbors but leaving the
removed node's own `prev`/`next`/`parent` pointing into the live structure. The
node stays reachable (leak) or a later traversal follows a stale link.

Fix: after rewiring neighbors, set the removed node's links to nil.

### Copying a struct that embeds a sync.Mutex

Wrong: `type Cache struct{ mu sync.Mutex; ... }` then passing a `Cache` by value
or returning one. The copy has a copy of the lock; two goroutines can each hold
"their" copy and mutual exclusion is gone.

Fix: always pass `*Cache`, never `Cache`. `go vet` copylocks flags the common
cases; keep the mutex behind a pointer receiver.

Next: [01-user-service-shared-mutable-state.md](01-user-service-shared-mutable-state.md)
