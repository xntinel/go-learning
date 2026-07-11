# Pointer Receivers, Method Sets, and Interface Satisfaction — Concepts

Whether a type "implements an interface" is not a matter of opinion or intent —
it is a mechanical fact decided by the type's method set, and the method set of
`T` is not the same as the method set of `*T`. Get this wrong and the failures
are quiet and expensive: a struct that you believe satisfies your repository port
does not, so the wiring fails to compile — or worse, it compiles because you
happened to pass `*T` in one place and `T` in another and only one of them works.
`json.Unmarshal` silently leaves a config field at its zero value because your
`UnmarshalJSON` has the wrong receiver. A function returns a nil `*MyError` typed
into the `error` interface, and the caller's `err != nil` check fires even though
nothing went wrong, paging an on-call engineer at 3am. A shared HTTP handler's
dependencies get copied on every request because the handler was stored by value.
Every one of these is the same small rule about method sets and addressability,
viewed from a different production angle.

This file is the conceptual foundation. Read it once and you have the model you
need to reason through all ten independent exercises that follow. Each exercise is
a production-shaped artifact — a counter, a handler, a config decoder, a
repository, a worker pool, a sorter, a metering writer — with a real `*_test.go`
and, where it matters, a `-race`-clean concurrency angle.

## Concepts

### Method sets: T versus *T

The method set of a type determines which interfaces it satisfies. The rule is
asymmetric and worth memorizing exactly:

- The method set of `T` contains only the methods declared with a **value**
  receiver (`func (t T) M()`).
- The method set of `*T` contains **both** value-receiver methods and
  **pointer**-receiver methods (`func (t *T) M()`).

So a pointer-receiver method is in the method set of `*T` but not of `T`.
Interface satisfaction is checked against the method set of the *exact type you
assign to the interface variable*. If `Settable` requires `Set` and `Set` has a
pointer receiver, then `var s Settable = &c` compiles and `var s Settable = c`
does not — `c` (a `Counter` value) simply lacks `Set` in its method set. This is
the single most common source of "but my type has that method!" confusion.

### Addressability is the reason

Why can `*T` have both kinds of methods but `T` only the value kind? Because
calling a pointer-receiver method `p.M()` is shorthand for `(&x).M()`, and taking
`&x` requires `x` to be **addressable**. The compiler inserts the `&` for you, but
only when the value is addressable — and many values are not. Addressable:
ordinary variables, elements of a slice, the result of a pointer dereference,
struct fields of an addressable struct. **Not** addressable: map elements
(`m[k]`), the return value of a function call, a composite literal written inline
(`Counter{}`), and — critically — a value stored inside an interface. On a
non-addressable value the compiler cannot synthesize the pointer, so it rejects
the pointer-receiver call: `Counter{}.Inc()` and `m[k].Inc()` do not compile. When
the value *is* held in an interface, the method set that was fixed at assignment
time is what counts, which is why `T` in an interface can never reach a
pointer-receiver method.

### An interface value is a (type, value) pair — the typed-nil trap

An interface value is two words: a concrete **type** descriptor and a **value**
(a pointer to, or an inline copy of, the concrete data). An interface is nil only
when *both* words are nil. This produces the notorious typed-nil trap: if you
declare `var e *ValidationError` (a nil pointer) and `return e` from a function
whose return type is `error`, the returned interface has type = `*ValidationError`
and value = nil. The type word is non-nil, so the interface is **not** nil, and
the caller's `if err != nil` is true even though there was no error. The fix is
never to return a typed nil into an `error`: return an explicit `nil`, or only
build the concrete error on the actual failure path. The Go FAQ documents this as
one of the language's sharpest edges.

### The convention: if one method needs a pointer, all methods take pointers

Go Code Review Comments states the rule plainly: if any method of a type needs a
pointer receiver, give *all* of that type's methods pointer receivers, so the
method set is uniform and `*T` implements every interface consistently. Mixing
receivers is a smell: it means `T` and `*T` implement different interfaces, which
surprises readers and can silently break satisfaction for `T`. Value-only types
(immutable domain values) keep all value receivers; mutable/stateful types
(anything holding a mutex, an atomic, a map, or a DB handle) keep all pointer
receivers.

### When a pointer receiver is *required*

Three independent reasons force a pointer receiver: (a) the method must **mutate**
the receiver and have the change persist; (b) the struct is large and you want to
avoid **copying** it on every call; (c) the struct **must not be copied** because
it contains a `sync.Mutex`, an `sync/atomic` type, or is otherwise registered with
`go vet`'s copylocks analyzer. Reason (c) is enforced: `go vet` flags a copy of a
lock. A value receiver on such a type copies the lock on every call, which either
trips vet or, worse, silently gives each call its own lock and destroys the
mutual exclusion.

### encoding/json: UnmarshalJSON must be a pointer receiver

`json.Unmarshal` decodes *into* a destination, so it needs to mutate it; it looks
for `UnmarshalJSON` on an addressable target (it always has the field's address).
`UnmarshalJSON` therefore must have a **pointer** receiver — it mutates `*d`. If
you write it with a value receiver, the code still compiles and json still *calls*
it (a value method is promoted into `*T`'s method set), but the method operates on
a **copy**, so the parsed value is written to the copy and discarded. The result
is a config field silently left at its zero value: no error, no log line, just a
30-second timeout that is actually zero. `MarshalJSON`, which only reads, is
conventionally a value receiver so that both `T` and `*T` marshal.

### Method values bind the receiver; method expressions do not

`x.M` (with no call parens) is a **method value**: it binds the receiver `x` at
the point of the expression and yields a function of the remaining arguments.
`T.M` or `(*T).M` is a **method expression**: it leaves the receiver as an
explicit first parameter. The binding semantics follow the receiver kind. Binding
a *pointer*-receiver method (`p.Record` where `Record` has a `*RateMeter`
receiver) captures the pointer, so the bound callback shares the same underlying
state — perfect for handing to a worker-pool of goroutines that must all update
one meter. Binding a *value*-receiver method captures a **copy** of the receiver,
so mutations through that callback are lost. This distinction is invisible until
you fan a callback out across goroutines and wonder why the total is wrong.

### fmt uses the method set of the exact value it receives

`fmt` calls `String()` (or `Format`, `Error`) only if the value it is handed
implements the interface — checked against that value's method set. If `String`
has a pointer receiver, a plain `Status` value does not implement `fmt.Stringer`,
so `fmt.Sprintf("%v", s)` prints the raw underlying representation instead of your
formatted string. Ranging over a `[]Status` or a `map[K]Status` and printing the
elements hands fmt *copies* (interface conversion copies the value), none of which
implement Stringer, so none get formatted. For uniform formatting everywhere,
define `String` on a **value** receiver. (Map elements are additionally
non-addressable, so you cannot even work around it with `&m[k]`.)

### Interfaces are dependency inversion; the adapter is almost always *T

A port interface (`UserStore`, `http.Handler`, `io.Writer`) lets callers depend on
behavior, not on a concrete type. The concrete adapter that satisfies the port is
almost always `*T`, because it holds and mutates state: a map guarded by a mutex,
a DB handle, an atomic counter. The standard compile-time assertion that the
adapter satisfies the port is `var _ Port = (*Adapter)(nil)` — it costs nothing at
runtime and turns a satisfaction bug into a compile error at the definition site
rather than a mysterious failure at the call site.

### Value receivers give value semantics for read-only domain types

A value receiver operates on a copy, so a read-only domain value — `Amount`,
`Money`, a `time.Time`-like type — is safe to pass by value and satisfies a
read-only interface from *both* `T` and `*T` (because `*T`'s method set includes
the value methods). Copying such a value is cheap and correct precisely because
its methods do not mutate. This is the mirror image of the mutable case: uniform
value receivers for immutable data, uniform pointer receivers for stateful
adapters.

## Common Mistakes

### Mixing pointer and value receivers on one type

Wrong: `func (c Counter) Inc()` alongside `func (c *Counter) Get()`. Now `Counter`
and `*Counter` implement different interfaces, and readers cannot predict which.
`go vet` may also flag a copied lock if the type holds one.

Fix: pick one kind for the whole type. Stateful/mutable types get all pointer
receivers; immutable value types get all value receivers.

### A value-receiver mutating method (UnmarshalJSON, Scan, ...)

Wrong: `func (d Duration) UnmarshalJSON(b []byte) error { ... *?... }`. It compiles
and is even called, but it mutates a copy; the destination field stays at its zero
value with no error. The same trap applies to `sql.Scanner.Scan` and any method
meant to write into the receiver.

Fix: give every mutating method a pointer receiver: `func (d *Duration)
UnmarshalJSON(b []byte) error`.

### Returning a typed nil as an error

Wrong: `var e *ValidationError; ...; return e`. When no error occurred, the caller
still sees `err != nil` because the interface carries a non-nil type word.

Fix: return an explicit `nil` on success, or build the `*ValidationError` only on
the failure path and `return nil` otherwise.

### Calling a pointer-receiver method on a non-addressable value

Wrong: `Config{}.Reload()`, `m[key].Mutate()`, or calling a pointer method on a
value pulled out of an interface. The compiler rejects the addressable ones and
the interface case never had the method in its set.

Fix: bind to a variable first (`c := Config{}; c.Reload()`); for maps, read the
value out, mutate it, and write it back, or store `*T` in the map.

### Copying a struct that holds a lock or atomic

Wrong: passing a `struct{ mu sync.Mutex; ... }` by value, or ranging over a
`[]T` of such structs with copy semantics. Each copy gets its own lock; the mutual
exclusion is gone. `go vet` copylocks catches many, not all, of these.

Fix: hold and pass such types as `*T` only; give their methods pointer receivers.

### Expecting fmt to call a pointer-receiver String on a value

Wrong: defining `func (s *Status) String()` and expecting `fmt.Sprintf("%v", s)`
or printing slice/map elements to use it. It prints the raw representation.

Fix: define `String` on a value receiver so every value — in a slice, a map, or
standing alone — formats through it.

### Binding a value-receiver callback and fanning it out

Wrong: `cb := meter.Record` where `Record` has a value receiver, then calling
`cb` from many goroutines. Each call mutates a copy; the aggregate is lost.

Fix: give `Record` a pointer receiver so the bound method value shares one
underlying meter across all goroutines.

### sort.Interface Swap that mutates a copy

Wrong: implementing `Swap` such that it swaps elements of a copy of the backing
storage; the sort appears to do nothing. This happens when the receiver type does
not share the backing array.

Fix: the sortable type must carry a mutable view of shared backing storage (a
slice header shares its backing array by value semantics, so a value receiver on a
slice type works), and `Swap` must write through it.

Next: [01-counter-service-pointer-receivers.md](01-counter-service-pointer-receivers.md)
