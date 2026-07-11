# Common Standard Library Interfaces in Production Backends — Concepts

The interfaces the standard library defines are the wiring of every real Go
service. A request body arrives as an `io.Reader`; you cap it, decode it, and
sometimes forward it to an upstream as an `io.Writer`. A domain type crosses the
JSON boundary through `json.Marshaler`/`json.Unmarshaler` or, more often than you
expect, through `encoding.TextMarshaler`/`TextUnmarshaler`. It crosses the SQL
boundary through `driver.Valuer` and `sql.Scanner`. Errors classify through the
`error` interface plus `errors.Is`/`As`/`Join`. And the whole HTTP surface —
handlers, middleware, routers, test harnesses — is nothing but implementations of
`http.Handler`. A senior engineer's edge is not knowing that these interfaces
exist; it is knowing the exact contract of each: what a method may return, who
owns the buffer it was handed, when it must be idempotent, and precisely how it
fails when you implement it a little bit wrong. This file is that contract sheet.
Read it once and each of the following modules is a real artifact you can reason
through.

## Concepts

### An interface is an implicit, structural contract

A type satisfies `io.Reader` the moment it has a method
`Read(p []byte) (int, error)` with that exact signature — no `implements`
keyword, no declaration, no import of the interface at the definition site. This
structural typing is what makes the stdlib interfaces so composable: a
`*bytes.Buffer`, a `*strings.Reader`, an `*os.File`, and a `net.Conn` all became
readers without ever naming `io.Reader`. The cost of the implicit contract is
that getting the signature wrong fails silently at the definition. Write
`Read() ([]byte, error)` and the type simply is not an `io.Reader`; the compiler
says nothing until the assignment site (`var r io.Reader = myType{}`), and if you
never write that assignment, nothing complains at all. The discipline is to add a
compile-time assertion, `var _ io.Reader = (*MyReader)(nil)`, next to any type
whose whole job is to satisfy an interface.

### The io.Reader contract has rules you must internalize

`Read(p []byte) (n int, err error)` reads up to `len(p)` bytes into `p`. Four
rules trip up people who have only ever *called* readers and never *written* one.
First, `Read` may return `0 < n <= len(p)` together with a non-nil `err` in the
same call; the bytes are valid and the error describes what happens next. A
correct caller therefore processes the `n` bytes *before* it inspects `err` —
inspect `err` first and you drop the last chunk of a stream. Second, `io.EOF` is
not a failure; it is the normal end-of-stream signal, and it may arrive either
with the final bytes (`n > 0, err == io.EOF`) or on its own (`n == 0,
err == io.EOF`). Third, a `Read` implementation must not retain the slice `p`
after it returns — the caller owns that buffer and will reuse it, so stashing it
in a field corrupts your data on the next call. Fourth, returning `0, nil` is
discouraged: it means "no progress, no error", and callers are permitted to treat
a stream of them as a busy-loop.

### The io.Writer contract is about the byte count

`Write(p []byte) (n int, err error)` must write all of `p` and return
`n == len(p)` on success. Any `n < len(p)` MUST be accompanied by a non-nil
error, because that is the definition of a short write and callers like
`io.Copy` rely on it: `io.Copy` treats `n < len(p)` with a nil error as
`io.ErrShortWrite` and aborts. The subtle case is a *transforming* writer — one
that redacts secrets, compresses, or rewrites bytes before forwarding. Even
though the number of bytes it forwards downstream differs from `len(p)`, it must
still report `len(p)` (the count it consumed from the caller) as `n`, or every
`fmt.Fprintf` through it looks like a short write. Like `Read`, `Write` must not
retain `p`.

### io.Closer, and why Close matters more on the write side

`Close() error` releases a resource. Convention is to defer it, and on a *read*
path swallowing its error (`defer func() { _ = c.Close() }()`) is acceptable —
closing an already-drained reader rarely tells you anything. On a *write* path it
is the opposite: a `Close` that flushes a buffer can be the operation that
actually commits your bytes, and a failed flush surfaced only through `Close`'s
return value means data was silently lost if you ignored it. So: swallow `Close`
on reads if you like, but check it on writes. In practice make `Close`
idempotent-safe (a second `Close` should not panic), because deferred closes and
explicit closes often both run on error paths.

### fmt.Stringer and the infinite-recursion trap

`String() string` is dispatched automatically by `fmt` for the `%v` and `%s`
verbs: if the value implements `fmt.Stringer`, `fmt` calls it. This is how a
domain enum prints its human name and how a credential type can mask itself so it
never leaks into a log line. The classic self-inflicted wound is writing
`String()` on a value type and implementing it with
`fmt.Sprintf("%v", self)` — that re-enters `fmt`, which calls `String()` again,
forever, until the stack overflows. The fix is to convert to the underlying type
first: `fmt.Sprintf("%v", int(s))` or `string(k)`, never `%v` of the receiver.

### The error interface is a taxonomy, not a string

`error` is one method, `Error() string`, but the real power is the ecosystem
around it. `fmt.Errorf("load user %s: %w", id, err)` wraps `err` so that the
chain can be walked. `errors.Is(err, ErrNotFound)` reports whether a sentinel
appears anywhere in the chain; `errors.As(err, &target)` extracts the first error
of a concrete type in the chain; `errors.Join(a, b)` groups independent failures
so `Is` matches either. This is how an HTTP handler maps a domain failure to a
status code without string matching: `errors.Is(err, ErrNotFound)` gives 404, a
successful `errors.As(err, &ve)` for a `*ValidationError` gives 400, everything
else is 500. Compare with `==` against a wrapped error, or type-assert instead of
`errors.As`, and the classification breaks the instant someone adds a `%w` layer.

### The serialization boundary: JSON and Text marshalers

`json.Marshaler` (`MarshalJSON() ([]byte, error)`) and `json.Unmarshaler`
(`UnmarshalJSON([]byte) error`) let a type control its exact wire shape — an API
timestamp as an integer instead of RFC3339, an enum as its string name with
unknown values rejected on decode. A pointer-receiver `UnmarshalJSON` is called
only when the value is addressable, which it is for a struct field but is not for
a map value or a non-addressable temporary; that asymmetry is a real bug source.
`encoding.TextMarshaler`/`TextUnmarshaler` (`MarshalText`/`UnmarshalText`) are
the quieter, higher-leverage pair: `encoding/json` falls back to them for scalar
strings and map keys when there is no `MarshalJSON`, and so do most YAML, TOML,
and env-config libraries, and `flag.Value` semantics line up too. One
`UnmarshalText` on a `LogLevel` therefore serves JSON, YAML, env, and flags at
once. Go 1.24 added `encoding.TextAppender`/`BinaryAppender` (`AppendText`,
`AppendBinary`) so a type can append its representation to an existing buffer
without allocating a fresh slice, and the modern `encoding/json/v2` (with
`MarshalerTo`/`UnmarshalerFrom`) is the high-performance direction; the v1
interfaces remain the everyday tool.

### Sorting: sort.Interface then slices.SortFunc

`sort.Interface` is `Len() int`, `Less(i, j int) bool`, `Swap(i, j int)` — it
predates generics and drove `sort.Sort`/`sort.Stable`. The modern default is
`slices.SortFunc`/`slices.SortStableFunc` with a comparison built from
`cmp.Compare` and `cmp.Or`, which chains tiebreakers left to right and is far
less error-prone than a hand-written `Less`. But `sort.Interface` still earns its
place: a *named, reusable* ordering you attach to a slice type; sorting *parallel
slices* in lockstep (where `Swap` moves several arrays at once); and pairing with
`sort.Search` for binary lookup on the sorted result. Remember `sort.Sort` is not
stable — equal elements may reorder nondeterministically — so when input order
must survive ties, use `sort.Stable` or `slices.SortStableFunc`.

### The database boundary: driver.Valuer and sql.Scanner

`driver.Valuer` (`Value() (driver.Value, error)`) converts a domain value into
something the driver can store, and `driver.Value` is a *restricted* type: it
must be `nil`, `int64`, `float64`, `bool`, `[]byte`, `string`, or `time.Time`.
Return anything else — a custom struct, a plain `int`, a `uint64` that overflows
`int64` — and `database/sql` fails at runtime deep inside the query path. The
mirror, `sql.Scanner` (`Scan(src any) error`), reads a column back, and it must
handle the fact that different drivers hand you different concrete types for the
"same" column: a TEXT value can arrive as `string` from one driver and `[]byte`
from another, and a NULL arrives as `nil`. A correct `Scan` type-switches over
`string`, `[]byte`, and `nil` at minimum, mapping NULL to a defined zero or
invalid state rather than panicking.

### http.Handler composes the entire service

`http.Handler` is one method, `ServeHTTP(w http.ResponseWriter, r *http.Request)`,
and it is the single interface an entire Go service is built from. A leaf
handler, a middleware that wraps another handler, a router that dispatches to
handlers, and the test harness that drives them all speak `http.Handler`.
`http.HandlerFunc` adapts a plain function to the interface. Because the contract
is in-process, `net/http/httptest` (`NewRecorder`, `NewRequest`) exercises a
handler fully with zero sockets: you call `ServeHTTP` directly and read the
recorded status, headers, and body. A readiness probe, a middleware, and their
tests in this chapter never open a port.

### Accept interfaces, return structs; compose the small ones

Two idioms tie the rest together. Accept the *narrowest* interface a function
needs (take `io.Reader`, not `*os.File`) and return concrete types, so callers
are maximally free in what they pass and precise in what they get back. And build
larger contracts by *embedding* single-method interfaces: `io.ReadCloser` is
literally `interface { Reader; Closer }`. Single-method interfaces —
`Reader`, `Writer`, `Closer`, `Stringer`, `Handler` — are the Go idiom precisely
because they maximize the set of satisfying types and make test doubles trivial:
a fake `io.Reader` is one method.

## Common Mistakes

### Wrong signature, silent non-satisfaction

Writing `Read() ([]byte, error)` or `Write(p []byte) error` so the type never
satisfies `io.Reader`/`io.Writer`. The compiler only objects at an assignment to
the interface, not at the method. Fix: copy the signature exactly and add
`var _ io.Reader = (*T)(nil)` beside the type so a mismatch fails to compile.

### Inspecting err before processing n

Reading `err` from `Read` before handling the `n` returned bytes, dropping the
tail of a stream when the final bytes arrive together with `io.EOF`. Fix: always
use the `n` bytes first, then look at `err`.

### Retaining the caller's buffer

Stashing the `p` passed to `Read` or `Write` in a struct field. The caller reuses
`p` on the next call and corrupts your copy. Fix: copy out what you need before
returning.

### A transforming writer returning the wrong count

A redacting or compressing `io.Writer` returning the number of bytes it forwarded
downstream instead of `len(p)`. `io.Copy` and `fmt.Fprintf` read that as a short
write and report an error. Fix: return `len(p)` (bytes consumed from the caller)
on success.

### Reading an untrusted body with no cap

`io.ReadAll(r.Body)` on request input with no size limit is a memory-exhaustion
DoS. Fix: wrap with `http.MaxBytesReader` or a custom capped reader that returns
a sentinel at the boundary.

### Infinite recursion in String()

`func (s S) String() string { return fmt.Sprintf("%v", s) }` re-enters `fmt`,
which calls `String()` again forever. Fix: convert to the underlying type inside
`String()` (`int(s)`, `string(s)`), never format the receiver with `%v`/`%s`.

### Classifying errors with == or a type assertion

`if err == ErrNotFound` or `err.(*ValidationError)` breaks the moment a `%w`
wrap is added. Fix: `errors.Is` for sentinels, `errors.As` for typed errors.

### Marshaler on the wrong receiver

Implementing `UnmarshalJSON`/`UnmarshalText` on a pointer receiver but decoding
into a non-addressable value (a map element), so the custom method is never
called and you get default decoding. Fix: decode into addressable storage (a
struct field or a `*T`), and be deliberate about value vs pointer receivers.

### A Scan that handles only string

Handling `string` in `sql.Scanner.Scan` but panicking or erroring on `[]byte`
(or ignoring `nil`). Different drivers return different representations of the
same column, and NULL is `nil`. Fix: type-switch over `string`, `[]byte`, and
`nil`.

### Returning a disallowed type from Value()

`Value()` returning a custom struct, an `int`, or a `uint64` outside the
`driver.Value` set, which errors at runtime inside `database/sql`. Fix: return
one of `nil`/`int64`/`float64`/`bool`/`[]byte`/`string`/`time.Time`.

### Reaching for sort.Interface (or sort.Sort) by reflex

Hand-writing `Len`/`Less`/`Swap` where `slices.SortStableFunc` with `cmp.Or`
would be simpler, or using `sort.Sort` where ties must keep input order. Fix: use
`slices.SortStableFunc` for one-off multi-key sorts; reserve `sort.Interface` for
reusable named orderings, parallel-slice sorts, and `sort.Search`.

### Ignoring Close on the write side

Deferring `Close` and swallowing its error on a buffered *writer*, losing data
when the flush inside `Close` fails. Fix: on write paths, check the `Close`
error (or capture it into the function's named return).

Next: [01-io-copy-close-pipeline.md](01-io-copy-close-pipeline.md)
