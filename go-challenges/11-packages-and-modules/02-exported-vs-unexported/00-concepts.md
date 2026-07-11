# Exported vs Unexported: Designing a Go Package's Public Surface

Every capitalized identifier you ship in a shared library is a promise. Under
Go's semantic-import-versioning rules you cannot remove it, rename it, or change
its signature within a major version without breaking every downstream service
that imported it, including the ones written by people who have since left the
company. The export boundary is therefore not a syntax detail; it is API design
and blast-radius control. A senior engineer treats "should this be exported?" as
a design review question with a default answer of no, and reaches for the export
rule the way an architect reaches for load-bearing walls: deliberately, sparingly,
and with an eye on what happens years later when someone wants to change the thing
behind it.

This file is the conceptual foundation for the exercises that follow. Read it once
and you have the model you need to reason through all of them: the TTL cache with a
deliberate public surface, JSON field visibility, black-box contract tests, the
`export_test.go` seam, exporting an interface while hiding the implementation,
functional options, error contracts, constructor-enforced invariants, and the way
embedding silently leaks methods into your public API.

## Concepts

### The export rule is purely lexical

An identifier is exported if and only if its first rune is a Unicode uppercase
letter, and it is declared at package scope or is a field or method of an exported
type (Go spec, "Exported identifiers"). That is the entire rule. It applies
uniformly to package-level names, struct fields, methods, and interface methods.
`Cache` is exported; `cache`, `data`, `set`, `now` are not. There is no
`protected`, no `friend`, no `internal` visibility tier between package-private and
exported at the identifier level. The `internal/` package convention is a
directory-level mechanism and is a separate topic; within a single package the
choice is binary. Because the rule is lexical, changing a name's first letter is a
breaking change and also a no-op refactor everywhere else, which is exactly why the
compiler treats "capitalize this field" as a semantic decision rather than
cosmetics.

### Exported is a permanent contract, not a convenience

Anything capitalized is something every downstream caller can depend on, and given
enough callers and enough time, something they eventually will depend on, including
in ways you never intended. Hyrum's Law is the operational version of this: with a
sufficient number of users, every observable behavior of your interface will be
depended upon by somebody. The cost of exporting is not paid when you write it; it
is paid every time you later want to change or delete it and discover you cannot
within the current major version. This is why the default answer to "should this be
exported?" is no. You export the smallest surface that lets callers do their job,
and you keep everything else unexported so you retain the freedom to change it. A
library other teams can safely depend on and a library that generates a
breaking-change release every quarter differ almost entirely in how disciplined
this boundary was.

### Unexported names enforce invariants

Keeping mutable state unexported is how you make invariants hold. If the cache's
`data` map and its `set` helper are unexported, the only way a caller can mutate the
cache is through `Set`, which validates the key first and takes the lock. Export the
`data` map and you have handed every caller a way to write to it without the lock and
without validation, corrupting the very invariant the type exists to protect, and
you can never take that capability back without a breaking change. The same logic
applies to a required dependency held in an unexported field, to a helper that only
exists for internal sequencing, and to a clock seam used only by tests: unexported
is the mechanism that says "this is mine to change, and mutating it is my job, not
yours."

### Two test package modes: white-box and black-box

A test file may declare either the package it tests (`package cache`) or that
package's external test package (`package cache_test`), and both may live in the same
directory. A white-box test in `package cache` can reach unexported names; it is the
right tool for testing internals directly and for injecting seams, such as reassigning
the unexported `now` to a fixed clock to prove TTL expiry. A black-box test in
`package cache_test` sees only the exported surface, exactly as a real caller would,
which makes it the executable specification of your public contract: if a refactor of
the internals breaks a black-box test, it broke callers too. Mature packages use both.
The two files compile as two separate packages, and `go test` links them together, so
you get internal coverage and contract coverage from one `go test ./...`.

### export_test.go is the sanctioned seam

Sometimes a black-box test needs to reach an internal knob, most commonly a clock, to
drive a deterministic assertion, but you do not want that knob in the shipped API. The
standard-library answer is `export_test.go`: a file that declares the production
package (so it can touch unexported names) but, because its name ends in `_test.go`,
is compiled only under `go test`. In it you define a thin exported wrapper, for example
`func (c *Cache) SetClock(now func() time.Time) { c.now = now }`, that the black-box
test can call. The wrapper exists only during testing, so it never appears in the
package's public API and never shows up in `go doc`. This is how you keep test seams
completely out of the surface that callers and documentation see, while still letting
the contract test control time.

### encoding/json marshals exported fields only

`encoding/json.Marshal` serializes exported struct fields and silently ignores
unexported ones. This is simultaneously a safety property and a footgun. The safety:
internal bookkeeping, a password hash, an internal row id, a computed timestamp, held
in unexported fields cannot leak over the wire, because the marshaler literally cannot
see them. You do not need a hand-maintained allowlist of "safe to serialize" fields;
the language's visibility rule is the allowlist. The footgun: a field you meant to
serialize but forgot to capitalize is dropped with no error, producing a payload that
is silently missing data. The only defense is to assert the wire format in a test.
On the tag side, prefer `omitzero` (added in Go 1.24) over `omitempty` for value
structs and `time.Time`: `omitempty` omits only empty values (false, 0, nil, and
zero-length string/slice/map/array) and never omits a struct, so it does nothing on a
`time.Time`; `omitzero` honors an `IsZero() bool` method, which `time.Time` has, so a
zero timestamp is correctly omitted. Use `json:"-"` to force-drop an exported field
you never want on the wire.

### Prefer exporting an interface and hiding the implementation

The most durable public surface for a data-access or transport layer is a small
interface plus a constructor that returns it, with the concrete struct and its fields
kept unexported. Callers depend on `UserRepository`, not on `sqlUserRepository`, so you
can change the concrete type's fields, swap Postgres for a different backend, or wrap it
in caching, without a breaking change, because the concrete type never escaped the
package. It also inverts the dependency: call sites bind to the interface, so their
tests substitute an in-memory fake without touching a database. The discipline is to
keep the interface small (a fat interface is as hard to change as an exported struct)
and to return it from the constructor rather than exporting the struct and letting
callers name it.

### Functional options keep constructors backward-compatible

A constructor that takes a giant exported `Config` struct turns every new field into a
potential breaking change and forces callers to understand the zero-value meaning of
every field. The functional-options pattern avoids both problems: an unexported
`options` struct holds the configuration, an exported `Option` type is
`func(*options)`, and exported `With*` constructors return closures that set fields.
`New(opts ...Option)` applies defaults, then the options in order. Because `options` is
unexported, you can add a field to it and a matching `WithX` next year without changing
`New`'s signature or breaking a single existing call site. Later options override
earlier ones, defaults fill the gaps, and the zero-argument call is valid and
meaningful.

### Error contracts are part of the public surface

How a package reports failure is as much an API as its function signatures. Two
complementary tools: exported sentinel errors (`ErrNotFound`, `ErrConflict`) that
callers match by identity with `errors.Is`, and an exported error type with unexported
fields plus exported accessor methods that callers match by shape with `errors.As` to
pull out structured data (which field failed validation, what the offending value was).
A sentinel is cheap and carries no data; a typed error carries data but is heavier.
Whichever you return, wrap it with `fmt.Errorf("...: %w", err)` as it travels up the
call stack so the chain is preserved and `errors.Is`/`errors.As` can still find it at
the top. Returning bare `errors.New(fmt.Sprintf(...))` strings forces callers to
substring-match, which is the least stable contract you can offer.

### Zero-value design is a deliberate choice

Some types are built to be useful at their zero value: a `sync.Mutex`, a
`bytes.Buffer`, an `strings.Builder` all work correctly with no constructor, which is a
feature, it removes a `New` from the caller's path. Other types must be constructed
through `New` because a required dependency lives in an unexported field with no
exported setter, so the zero value is unusable by design. Choosing "illegal zero value"
converts a class of misuse from a nil-pointer panic in production into an error at
construction time or a guarded early return. Neither choice is universally right: pick
zero-value-usable when there is no required dependency and the zero state is
meaningful; pick constructor-enforced when a missing dependency would otherwise blow up
far from the mistake. The point is to choose, and to make the wrong path fail loudly and
early.

### Embedding leaks surface

Embedding an exported type promotes its exported methods into your own public API. If
your service struct embeds a `sync.Mutex`, then `Lock` and `Unlock` become methods of
your service, and callers can lock your internal mutex from outside the package. If it
embeds an `*http.Client`, every one of that client's exported methods becomes part of
your contract. Method promotion is a real, permanent expansion of your public surface,
and it is easy to trigger by accident because embedding is often used just to save
typing. The fix is to hold the dependency in a named unexported field
(`mu sync.Mutex`) unless you genuinely want the promoted methods to be part of your API.
Inspect the resulting surface with `go doc` on the type; if `Lock` shows up and you did
not mean it to, you have a leak.

## Common Mistakes

### Exposing internal state directly

Wrong: `func (c *Cache) Data() map[string]entry`. The returned map is the live internal
map; a caller can write to it behind the mutex and corrupt the cache's invariants, and
you can never remove the method. Fix: keep the state unexported and expose validated
methods (`Get`, `Set`, `Delete`, `Len`) that take the lock and check inputs first.

### Exporting a helper that only exists for internal use

Wrong: exporting `SetInternal` or `set` because a test or a neighboring type happened
to need it. It becomes a permanent public API that callers can invoke to bypass
validation. Fix: keep helpers unexported; if a test needs one, reach it from a
white-box test or expose it through `export_test.go`, not through the shipped surface.

### Using an unexported type for something callers must name

Wrong: `type cache struct{ ... }` returned from `New`. Callers can hold the value in a
variable via `:=` but cannot write its type in a field, a function signature, or a
`var` declaration. Fix: export the type, or return an exported interface the caller can
name.

### Forgetting to capitalize a field you intended to serialize

Wrong: a `total int` field you expected in the JSON payload. `encoding/json` silently
omits it and no error is raised, so the bug ships as an incomplete response. Fix:
capitalize it and assert the wire format in a test.

### Reaching for omitempty on a time.Time or a value struct

Wrong: `CreatedAt time.Time \`json:"createdAt,omitempty"\``. A struct is never
"empty", so the tag does nothing and a zero timestamp serializes as
`"0001-01-01T00:00:00Z"`. Fix: use `omitzero` (Go 1.24), which honors `IsZero()`.

### Putting test-only seams in the production file

Wrong: an exported `SetClock` or an exported `now` sitting in `cache.go` so tests can
reach it. It pollutes `go doc` and the public API forever. Fix: move the seam to
`export_test.go`, which compiles only under test.

### Exporting the concrete implementation instead of an interface

Wrong: returning `*sqlUserRepository` from the constructor. You have coupled every
caller to that concrete type and can no longer change its fields or swap the backend
without a breaking release. Fix: return a small exported interface and keep the struct
unexported.

### A giant exported Config struct as the only constructor input

Wrong: `New(cfg Config)` where `Config` is exported with a dozen fields. Every new
field risks a breaking change and callers must know each field's zero-value meaning.
Fix: functional options over an unexported `options` struct.

### Returning unstructured, string-only errors

Wrong: `return errors.New(fmt.Sprintf("user %s not found", id))` with no sentinel and
no type, forcing callers to substring-match the message. Fix: give them `ErrNotFound`
for `errors.Is` and a typed error for `errors.As`, and wrap with `%w`.

### Embedding a mutex or client to save typing

Wrong: `struct { sync.Mutex }`, which promotes `Lock`/`Unlock` into your package's
public API. Fix: a named unexported field, `mu sync.Mutex`, unless you deliberately
want the promoted methods.

Next: [01-ttl-cache-exported-api.md](01-ttl-cache-exported-api.md)
