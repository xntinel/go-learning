# Implicit Interface Satisfaction: Designing Consumer-Owned Boundaries — Concepts

Interfaces are the seams a backend service is tested, mocked, decorated, and
swapped along. In practice, most of a senior engineer's leverage over a codebase
comes from *where* those seams are cut and *how wide* they are, not from the
mechanical fact that "an interface has methods". A `Store` that a handler talks
to, a `UserReader` a request path depends on, a `log` sink, a caching decorator,
a graceful-shutdown routine that must close only the dependencies that are
closable — every one of these is an interface-design decision with direct
operational consequences: how easy the code is to fake in a test, how many
methods a decorator must forward, whether a signature change is caught at compile
time or as a 3am pager, and whether wrapping a `*os.File` behind an abstraction
silently leaks the file handle on shutdown.

This file is the conceptual foundation for the ten independent exercises that
follow. Each exercise is a real service seam. Read this once and you have the
model you need to reason through all of them.

## Concepts

### Satisfaction is implicit and structural

A type satisfies an interface if and only if its method set is a superset of the
interface's method set. There is no `implements` keyword, no declaration linking
a concrete type to an interface, and no import edge required between them. A type
can satisfy an interface it has never heard of — the compiler checks the match
structurally, at the point where you assign the concrete value to the interface
variable or pass it to a function that expects the interface.

This is the single most important property to internalize, because it inverts the
usual dependency arrow. In a nominal language (`class Foo implements Bar`) the
implementer must import and name the interface, so the interface tends to live
with the implementation. In Go the *consumer* can declare exactly the interface
it needs and the producer never learns it exists. That is what makes "the
consumer owns the interface" not just a style preference but the natural grain of
the language.

### Method sets: pointer vs value receivers

The method set of a value of type `T` contains only the methods declared with a
value receiver `func (t T) M()`. The method set of `*T` contains both value-
receiver *and* pointer-receiver methods `func (t *T) M()`. So a method with a
pointer receiver is in the method set of `*T` only, never `T`.

The consequence trips up nearly everyone once:

```go
type Store interface{ Set(k, v string) error }

func (m *MemoryStore) Set(k, v string) error { /* ... */ }

var _ Store = MemoryStore{}   // does NOT compile: method has pointer receiver
var _ Store = &MemoryStore{}  // compiles: *MemoryStore has Set in its method set
```

The compiler error is `MemoryStore does not implement Store (method Set has
pointer receiver)`. The fix is to use the pointer. There is also a *semantic*
reason to use the pointer beyond the method set: a value receiver operates on a
copy, so a `MemoryStore{}` value passed around by copy would each carry its own
copy of the embedded `sync.RWMutex` and map header — mutating one does not affect
another, and copying a mutex after first use is itself a bug `go vet` flags. Types
with a mutex or that must share mutable state are pointer types, and their methods
take pointer receivers.

### Accept interfaces, return concrete structs

The idiom "accept interfaces, return concrete structs" is the operational rule
that falls out of implicit satisfaction. A function that *consumes* a dependency
should take the narrowest interface that covers the methods it actually calls,
declared in the consumer's own package. A constructor that *produces* a value
should return the concrete type, not an interface, so the caller keeps access to
the full API and the package does not force an abstraction on everyone.

Returning a concrete struct also avoids a subtle trap: a constructor that returns
an interface can accidentally return a typed nil (see below), and it hides fields
and methods callers may legitimately want. Return `*PostgresStore`; let each call
site decide which narrow interface to view it through.

### Interface width and segregation

An interface's width is a cost paid by every implementation, every fake, and every
decorator. A twelve-method `Repository` forces each test fake to stub twelve
methods even when the test exercises one, and forces each decorator (metrics,
cache, retry, tracing) to write eleven pass-through forwarders. Narrow interfaces
compose: `io.Reader` and `io.Writer` are each one method, and `io.ReadWriter` is
their composition. Prefer declaring `KeyReader`, `KeyWriter`, `KeyDeleter` and
composing `ReadWriter` where a call site needs two of them, over one fat `Store`.
The read-only cache-warmer then depends on `KeyReader` alone, and its fake
implements exactly one method.

The trade-off is real, not free: more, smaller interfaces mean more names. The
guidance is to let the *call site* drive it — declare the interface where it is
consumed, containing only what that consumer calls, and you naturally land on one-
to-three-method interfaces without over-engineering an abstraction nobody uses.

### An interface value is a (type, value) pair

An interface value is two machine words: a type descriptor and a data pointer. A
`nil` interface has both words nil. This representation is the source of the most
notorious bug in Go, the typed nil:

```go
type AppErr struct{ msg string }
func (e *AppErr) Error() string { return e.msg }

func do() error {
	var e *AppErr // nil pointer
	// ... nothing sets e ...
	return e // returns an interface whose TYPE word is *AppErr, VALUE word is nil
}

err := do()
if err != nil { // TRUE — the type word is non-nil, so the interface is non-nil
	// this branch runs even though the underlying pointer is nil
}
```

The interface is non-nil because its type word is set to `*AppErr`, even though
the pointer it wraps is nil. In an HTTP handler this manifests as a request
failing on the success path: `if err != nil { writeError(...) }` fires and returns
a 500 or 400 for a perfectly valid request. The fix is never to declare a typed
error variable and return it; return the literal `nil` on the success path, and
return a concrete non-nil error only when there genuinely is one.

### The empty interface `any`

`interface{}` is the empty interface: it has no methods, so every type satisfies
it. `any` is an alias for `interface{}` (Go 1.18+). Use it only for genuinely
opaque payloads — the value in a generic container, a JSON scratch value, a
variadic `...any` for logging. The moment the consumer needs to *call* a method,
prefer a typed interface: it documents the contract, gives compile-time safety,
and avoids a forest of type switches that push errors to runtime.

### Compile-time interface guards

A guard `var _ Iface = (*T)(nil)` forces the compiler to prove `*T` satisfies
`Iface` at build time, with no runtime cost (the blank identifier discards the
value; `(*T)(nil)` allocates nothing). Place one next to each implementation. When
someone changes a method signature on the interface or on `T`, or renames a
method, the guard fails to compile immediately — instead of the drift surfacing
later as a value that silently no longer satisfies the interface at some distant
call site, or as a `nil` route handler, or a runtime panic. It is a one-line
assertion that pays for itself the first time a contract changes.

### Optional interfaces via type assertion

Because satisfaction is structural, a value may implement *more* than the
interface it is currently typed as. You discover that extra capability with a type
assertion:

```go
if c, ok := dep.(io.Closer); ok {
	err := c.Close()
}
```

This is exactly how the standard library works: `net/http` probes whether a
`ResponseWriter` also implements `http.Flusher` or `http.Hijacker`;
`io.Copy` checks for `io.ReaderFrom`/`io.WriterTo` fast paths. It lets a graceful-
shutdown routine close only the dependencies that are closable, without widening
the primary interface (e.g. `Store`) to include `Close` on every implementation.
The comma-ok form is mandatory: a bare assertion `dep.(io.Closer)` panics when the
value does not implement it.

### Satisfying stdlib interfaces

Your domain types plug into the ecosystem by implicitly satisfying small stdlib
interfaces: `fmt.Stringer` (`String() string`) controls `%v`/`%s` formatting;
`error` (`Error() string`) makes a type usable as an error and puts it in
`errors.Is`/`errors.As` chains; `json.Marshaler` (`MarshalJSON() ([]byte, error)`)
controls the JSON wire format; `io.Writer` (`Write([]byte) (int, error)`) drops
your sink into `log.New`, `fmt.Fprintf`, and every stdlib API that writes bytes;
`http.Handler` (`ServeHTTP`) plugs into the router; `slog.Handler` plugs into
structured logging. Each is tiny, and a single concrete type can satisfy several
at once, changing how it behaves in `fmt`, in error handling, and in JSON with no
wrapper.

### Decoration over an interface

A decorator is a type that holds an interface value and also satisfies that same
interface, adding behavior (caching, metrics, retry, tracing) transparently
around the wrapped value. Implicit satisfaction is what makes this stack cleanly:
`metrics(cache(memory))` type-checks because each layer is a `Store` and accepts a
`Store`, and the base `MemoryStore` never learns it is being decorated. Decoration
is only clean when the interface is narrow — over a twelve-method interface every
decorator becomes eleven lines of boilerplate forwarding, which is where "just add
a method to the interface" quietly rots a codebase.

### Wrapping can erase capabilities

Once a `*os.File` is stored in an `io.Writer` field, the caller has lost `Close`,
`Sync`, and `Seek` unless the interface exposes them or the caller re-asserts with
a type assertion. This is the same mechanism as optional interfaces, seen from the
cost side: an abstraction that is narrower than the capabilities the consumer
needs will, at shutdown, leave you unable to close the connection or flush the
buffer, leaking file handles and sockets. Design the interface to carry the
capabilities the consumer actually needs (add `io.Closer`, or compose it in), or
keep a typed reference to the concrete value for the operations the interface does
not cover.

### Nil-receiver method calls

A method can be called on a typed-nil receiver without panicking, *if* the method
does not dereference the nil. This is the basis of the null-object pattern — a
`(*NopLogger)(nil)` whose `Log` method simply returns is a valid, allocation-free
no-op. The method call itself is safe because method dispatch uses the type word,
not the value word; the panic only comes when the body touches a field through the
nil pointer. Useful, but a sharp edge: it means `var l Logger = (*NopLogger)(nil)`
is a non-nil interface holding a nil pointer, and code that later checks
`if l == nil` will be wrong.

## Common Mistakes

### Declaring the interface in the producer's package

Wrong: `MemoryStore`'s package defines `Store`, and every consumer imports that
package to name the interface. Now the consumer depends on the implementation, the
interface grows to expose everything the producer can do, and swapping the
implementation means touching the interface's package.

Fix: the consumer package declares the interface it needs, containing only the
methods it calls. The producer returns a concrete struct and never imports the
consumer. The import arrow points from producer-free consumer to nothing.

### Value/pointer receiver mismatch

Wrong: expecting `MemoryStore{}` (a value) to satisfy an interface whose methods
are on `*MemoryStore`, then being surprised by `method has pointer receiver`, or
by passing the store by value and finding the copies do not share the mutex or
map.

Fix: use `&MemoryStore{}` (or a `New` constructor that returns `*MemoryStore`).
Methods that mutate shared state or hold a mutex take pointer receivers, and the
type is used through a pointer everywhere.

### Returning a typed nil as an error

Wrong: `func validate() error { var e *ValidationError; ...; return e }` where `e`
stays nil on the happy path. The caller's `if err != nil` is true and the request
fails on a success path.

Fix: return the literal `nil` on success. Only construct and return a concrete
error when there is a real error. Never `return e` where `e` is a typed nil
pointer to an error type.

### One fat interface

Wrong: a `Store`/`Repository` with ten-plus methods, so every test fake stubs
methods it never calls and every decorator forwards methods it does not touch.

Fix: split into single-purpose interfaces (`KeyReader`, `KeyWriter`,
`KeyDeleter`) and compose them where a call site needs more than one. Each fake and
decorator implements exactly what its consumer uses.

### `any` where a typed interface belongs

Wrong: passing values as `any` and type-switching at every use, pushing what could
be a compile error to runtime and erasing the method contract from the signature.

Fix: declare a typed interface with the methods the consumer calls. Reserve `any`
for genuinely opaque payloads.

### No compile-time guard

Wrong: no `var _ Iface = (*T)(nil)`, so a signature change compiles in the
producer and the type silently no longer satisfies the interface — discovered at a
distant call site, a missing route, or a runtime panic.

Fix: place a guard next to each implementation. The drift fails the build at the
type, immediately.

### Comparing an interface against a typed nil

Wrong: `if s == (*MemoryStore)(nil)` or assuming `s == nil` catches a wrapped nil.
The two-word representation makes both misleading: an interface holding a nil
`*MemoryStore` is not `== nil`, and comparing against a typed-nil interface is a
type-and-value comparison that rarely means what you think.

Fix: do not smuggle typed nils into interfaces. Represent "absent" as a genuinely
`nil` interface or a sentinel error, and check `s == nil` only where you know no
typed nil can have been stored.

### Erasing a capability by wrapping

Wrong: storing a `*os.File` or a pooled connection as a narrow `io.Writer`/`Store`
and then being unable to `Close`/`Flush` it at shutdown — leaking handles and
sockets.

Fix: design the interface to carry the capabilities the consumer needs, or probe
for them with an optional-interface assertion (`if c, ok := w.(io.Closer); ok`),
or keep the concrete reference for the operations the interface does not expose.

Next: [01-store-satisfies-consumer-interface.md](01-store-satisfies-consumer-interface.md)
