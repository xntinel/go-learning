# new(T) vs Composite Literals: Allocation and Construction in Backend Services

Two expressions in Go both allocate a value and hand you a pointer to it:
`new(T)` and `&T{...}`. A junior reading of this topic stops at "they are the
same for structs, pick whichever." A senior reading is about the decisions that
sit underneath the syntax: whether your type is usable with no constructor at
all, whether a hot-path constructor forces a heap allocation, how you cut those
allocations back with `sync.Pool`, how you express a whole route or config tree
as one literal, how you encode PATCH semantics with pointer fields, how you keep
per-request state isolated instead of aliased, and how you publish an immutable
config snapshot to concurrent readers without a lock. Every one of those is a
correctness, performance, or concurrency-safety choice that a real service lives
or dies by. This file is the model; the exercises that follow each build one
real backend artifact around it.

## Concepts

### new(T) and &T{} are the same allocation

`new(T)` allocates storage for a value of type `T`, zeroes it, and returns a
`*T`. For a struct, `new(Conn)` is byte-for-byte equivalent to `&Conn{}`: same
allocation, same zeroed fields, same pointer type. There is no performance
difference and no semantic difference. The choice is one of intent. `new(T)`
reads as "give me a pointer to the zero value" — it says nothing about fields.
`&T{Host: h, Port: p}` reads as a constructor — it allocates and initializes in
a single expression, and the fields you omit fall to their zero value. Reach for
`new(T)` (or `var x T`) when the zero value is what you want; reach for the
composite literal the moment you have a field to set. Using `new(Conn)` followed
by three lines of `c.Field = ...` where one `&Conn{...}` would do is the
canonical "wrote more code than the language asked for" smell.

### Composite literals initialize, and inner types elide

A composite literal is the only construct that allocates and initializes in one
expression, and it composes: `[]Route{{Method: "GET", Path: "/"}}` builds a
slice of fully-formed structs, and the inner `Route{...}` type name is elided
because the element type is already known. The same elision works for pointers
inside a slice — `[]*Route{{...}}` means `[]*Route{&Route{...}}`. This is what
lets you write an entire routing table, a middleware chain, or a defaults tree
as a single readable literal. `new(T)` plus a run of assignments simply cannot
express a nested tree without drowning it in temporaries.

### Zero-value readiness is a design property

The most Go-idiomatic types are usable straight from `var x T` with no
constructor: `var mu sync.Mutex` locks fine, `var b bytes.Buffer` writes fine,
`var wg sync.WaitGroup` waits fine. That is not luck — it is a deliberate design
choice that the zero value is a valid, ready state. When you design a type this
way, `var b EventBatch`, `b := EventBatch{}`, and `new(EventBatch)` are all
immediately usable, and an exported constructor becomes optional rather than
mandatory. The usual technique is lazy initialization: the first `Add` allocates
the backing map or slice, and read methods tolerate the nil backing store
(ranging over a nil map or slice is legal and yields nothing, and `len` of a nil
map or slice is 0). Designing for the zero value removes a whole class of "forgot
to call New" nil-map panics from your callers.

### new versus make: the silent-bug line

`new` and `make` are not interchangeable, and confusing them is a quiet,
production-grade bug. `new(T)` always returns a `*T` to a zeroed `T`. For a
slice, map, or channel, the zeroed value is `nil`: `new([]int)` is a `*[]int`
pointing at a nil slice, `new(map[string]int)` is a pointer to a nil map (writing
through it panics), and `new(chan Job)` is a pointer to a nil channel — and a nil
channel blocks forever on send and receive. `make(T, ...)` is different in kind:
it does not return a pointer, it returns an initialized slice/map/channel header
that is ready to use. The rule is mechanical: `make` for slices, maps, and
channels; `new` (or `&T{}` or `var x T`) for everything else. A `new(chan Job)`
where you meant `make(chan Job, n)` compiles cleanly and then deadlocks at
runtime.

### Returning &T{} usually escapes to the heap

Escape analysis decides whether a value lives on the stack (free, reclaimed on
return) or the heap (costs an allocation and later GC work). Returning a value by
value — `func newMetric() Metric` — lets the result stay on the caller's stack.
Returning a pointer — `func newMetric() *Metric { return &Metric{...} }` — forces
the pointed-to value to escape to the heap, because its lifetime now outlives the
constructor's frame. On a cold path this is irrelevant. On a hot path — a metric
built per request, a DTO built per row — it is a measurable per-call allocation.
`go build -gcflags=-m` prints the escape decision ("&Metric{} escapes to heap"),
and `testing.AllocsPerRun` and a `b.ReportAllocs()` benchmark quantify it. The
senior instinct is: on a high-frequency path, prefer return-by-value for small
structs and only return a pointer when the value is large, must be shared, or
must be nil-able.

### Struct copy is shallow, not deep

Assigning a struct value copies it: `cfg := defaultConfig` gives you an
independent copy, and mutating `cfg.MaxConns` leaves the template untouched. This
is exactly what you want for per-request isolation. But the copy is shallow: any
slice, map, or pointer field in the struct still points at the *same* backing
array, map, or target as the original. So `cfg := defaultConfig; cfg.Tags[0] =
"x"` mutates the template's `Tags` too, because both share one slice header's
backing array. "Copy the struct" is not "deep copy." When a struct has reference
fields and you need true isolation, you must clone those fields explicitly. The
opposite mistake is sharing `&defaultConfig` across requests and mutating it in
place, which corrupts global state for every request at once.

### Pointer fields encode absent versus zero

In a JSON API, `{"name": ""}` and `{}` are different requests: the first sets the
name to the empty string, the second leaves the name unchanged. A `string` field
cannot tell them apart — both unmarshal to `""`. A `*string` field can: an
omitted key stays `nil`, a present key (even `""`) becomes a non-nil pointer to
the value. This is the backbone of correct PATCH / partial-update semantics.
`Apply` then mutates the target only for fields whose pointer is non-nil. Building
those pointers with `new(string)` plus an assignment everywhere is noise; a
generic helper `func Ptr[T any](v T) *T { return &v }` replaces it with one call
and returns a distinct heap pointer whose deref equals `v`.

### sync.Pool.New is production new(T)

The canonical production use of `new(T)` is the `New` field of a `sync.Pool`:
`sync.Pool{New: func() any { return new(bytes.Buffer) }}`. The pool amortizes
allocation of reusable objects across a load: `Get` returns a pooled buffer (or
calls `New` if the pool is empty), you `Reset` it, use it, and `Put` it back.
Under a JSON-encoding hot path this cuts the per-call buffer allocation to near
zero. Two disciplines are non-negotiable: always `Reset` before reuse (a pooled
object carries whatever the last user left in it), and never assume a `Put` and a
later `Get` correspond — the pool may drop an object at any GC, and `Get` may hand
you `New`'s fresh object or someone else's returned one.

### atomic.Pointer[T] publishes immutable snapshots

For live config reload you want readers to see a fully-formed config with no lock
on the read path. `atomic.Pointer[Config]` does this: each reload builds a fresh
`&Config{...}` snapshot and calls `Store`; readers call `Load` and get the
current immutable snapshot. Because each snapshot is a distinct allocation,
in-flight readers keep a consistent view of the config they loaded even as a new
one is published — there is no torn read, because nothing is mutated in place.
`CompareAndSwap` lets a reloader publish only if the config it is replacing is
still the one it read. The anti-pattern is mutating the fields of a shared
`*Config` in place, which races and hands readers half-updated state.

### Functional options start from a defaults literal

The idiomatic replacement for `new(T)` plus a long run of caller-side field
assignments is the functional-options constructor. It starts from a
composite-literal defaults struct — `&ServerConfig{ReadTimeout: 5 * time.Second,
MaxConns: 100}` — and applies a variadic list of `Option` closures, each of which
overrides exactly one field. Calling with no options yields the documented
defaults; later options win over earlier ones; and each call returns an
independent pointer so two constructions never share state. `cmp.Or` is handy for
a "use this unless it is zero" default inside an option. This keeps the defaults
in one place and the call sites readable, instead of scattering initialization
logic across every caller.

## Common Mistakes

### Using new(T) then assigning fields line by line

Wrong: `c := new(Conn); c.Host = "db"; c.Port = 5432`. Three statements and a
mutable intermediate for what a composite literal states atomically.

Fix: `c := &Conn{Host: "db", Port: 5432}`. One expression, initialized in place.

### Reaching for &T{} to mean the plain zero value

Wrong: `&Conn{}` scattered around where you only ever want the zero value.

Fix: `new(Conn)` or `var c Conn` states "zero value" as intent. It is identical
in effect and clearer in meaning.

### Confusing new with make

Wrong: `new(map[string]int)` (a `*map`, nil map, panics on write),
`new(chan Job)` (a nil channel that blocks forever), `new([]int)` (a `*[]int`
over a nil slice).

Fix: `make(map[string]int)`, `make(chan Job, n)`, `make([]int, 0, cap)`. Use
`make` for slices, maps, and channels; `new`/`&T{}`/`var` for everything else.

### Assuming a struct copy is a deep copy

Wrong: `cp := original; cp.Tags[0] = "x"` and expecting `original.Tags`
unchanged. The copy shares the slice's backing array.

Fix: clone reference fields explicitly — `cp.Tags = slices.Clone(original.Tags)`
— when you need true isolation.

### Sharing one &defaultConfig across requests

Wrong: handing every request `&defaultConfig` and mutating it per-request,
corrupting the template for all requests at once.

Fix: copy the value template (`cfg := defaultConfig`) so each request mutates its
own copy, cloning any reference fields it will mutate.

### Returning *T from a hot-path constructor without measuring

Wrong: `return &Metric{...}` on a per-request path, adding a heap allocation you
never checked for.

Fix: return `Metric` by value for small structs on hot paths; confirm with
`-gcflags=-m` and `testing.AllocsPerRun` before assuming either way.

### Forgetting to Reset a pooled object

Wrong: `Get` a `*bytes.Buffer` from a `sync.Pool` and encode into it without
`Reset`, leaking the previous user's bytes into your output.

Fix: `buf.Reset()` immediately after `Get`, before the first write. Never assume
a `Put`/`Get` pair corresponds to the same object.

### Collapsing absent and zero with a value field

Wrong: a `string` field for an optional PATCH parameter, which cannot distinguish
"omitted" from "set to empty string."

Fix: a `*string` field — nil for absent, non-nil (even to `""`) for present — and
apply only non-nil fields.

### Mutating a shared *Config for reload

Wrong: reloading config by writing new values into the fields of one shared
`*Config` while readers are reading it (racy, torn reads).

Fix: build a fresh `&Config{...}` snapshot and publish it with
`atomic.Pointer[Config].Store`; readers `Load` a whole immutable snapshot.

### Copying a struct after its Mutex has been used

Wrong: `new(sync.Mutex)` is fine, but copying a struct value that embeds a
`sync.Mutex` after first use copies the lock's state; `go vet` flags it.

Fix: hold such types behind a pointer, or never copy them after use. `go vet`'s
`copylocks` check is the guardrail.

Next: [01-conn-constructors-new-vs-literal.md](01-conn-constructors-new-vs-literal.md)
