# Nil Pointers and Guard Checks in Production Backends — Concepts

Nil-pointer defense sounds like a beginner topic and is not. In a real backend
it is where several independent sources of "no value" collide with live request
traffic: a PATCH payload that carries only the two fields the client changed, a
database column that is genuinely NULL, an optional dependency (logger, metrics,
tracer) that was never injected in this deployment, a hot-reloaded config
snapshot that has not been stored even once, and a webhook whose nested objects
are each independently optional. A senior engineer does not "avoid nil"; they
know exactly which nil operations are safe and which panic, they know where the
guard belongs, and they know what the guard should return so it does not erase a
signal the caller needed. This file is the conceptual foundation for the nine
independent exercises that follow; read it once and each exercise becomes an
application of one rule.

## Concepts

### A nil pointer is a valid value; only dereferencing it is dangerous

A nil pointer is a first-class value. You can assign it, pass it, return it,
store it in a struct field, compare it with `==`, and put it in a map or slice.
None of that panics. The single dangerous operation is reaching *through* it: a
dereference `*p`, a field access `p.Field` (which is sugar for `(*p).Field`), or
a method call that dereferences the receiver. So the mental model is not "nil is
poison" but "nil is fine until the exact instant you follow it." That instant is
where a guard must sit, and nowhere else.

### A method can run on a nil receiver

Because a method call does not dereference the receiver until the body touches a
field, a method with a pointer receiver may legally execute with a nil receiver,
as long as it guards before the first field access:

```go
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	return len(c.data)
}
```

This is not a trick; it is the foundation of nil-safe APIs and of the null-object
pattern. `var c *Cache; c.Len()` returns 0 without panicking. The guard has to be
the very first thing the body does — if any statement before it reads a field,
the guard is dead code and the method still panics.

### Interface values are a (type, value) pair — the number-one nil trap

An interface value is not a single word; it is a pair `(dynamic type, dynamic
value)`. An interface is `nil` only when *both* halves are nil. This produces the
most expensive nil bug in Go:

```go
var p *AppError  // p == nil
var err error = p
// err != nil  --  because err is (type=*AppError, value=nil), not (nil, nil)
```

A function whose signature returns `error` but that returns a concrete
`*AppError` variable — even a nil one — hands every caller a non-nil interface.
The success path now looks like a failure to `if err != nil`, or a real failure
gets a nil-looking wrapper, silently across every call site. The fix is never
`reflect`; it is to return the untyped `nil` literal (or an `error` variable that
was never assigned a typed nil) from the success path. Return concrete pointers
only on the paths that actually failed.

### Nil maps and nil slices have asymmetric safety

The zero value of a map is a nil map; the zero value of a slice is a nil slice.
Their safety rules differ and you must know which you hold before the first
write:

- Nil map: reading (`v := m[k]`, comma-ok), `len`, and `range` are all safe and
  behave as empty. Writing (`m[k] = 1`) panics. A nil map is read-only until you
  `make` it.
- Nil slice: `len`, `range`, index-within-bounds (there are none), and `append`
  are all safe. `append` to a nil slice allocates a fresh backing array and
  returns a real slice. A nil slice is a perfectly good empty slice.

So an accumulator can `append` to a nil slice from the start with no
initialization, but must lazily `make` its map before the first insert.

### "Absent" and "zero" are different facts

`false`, `0`, and `""` are values. "The client did not send this field" is not a
value — it is the absence of one. A plain value field cannot represent both: a
decoded `Active bool` that is `false` cannot tell you whether the client sent
`"active": false` or omitted the key. Only a pointer field (`*bool`), a
`sql.Null[T]`, or a discriminated option type can carry the extra bit. This
distinction is the entire basis of correct PATCH semantics (nil field = leave
unchanged; non-nil = set, even to the zero value) and of correct NULL handling
(NULL = unknown, not empty string). Decode partial updates and nullable columns
into pointer or `sql.Null` fields, never into bare value fields.

### Guard placement is an architectural decision

Where you normalize is a design choice with a cost. Validating and defaulting at
the trust boundary — the moment you decode a request, load config, or scan a row
— lets the entire interior assume non-nil, well-formed data. Scattering
`if x != nil` checks through inner loops and hot paths instead adds branches
where they buy nothing and obscures intent, and it only takes one missed spot to
panic in production. Normalize once at the edge: `Load(cfg)` returns a
fully-populated config so downstream code never nil-checks a timeout again; a
row-mapper turns NULL into an absent field once so the domain layer sees a clean
`*string`.

### The null-object pattern for optional dependencies

Cross-cutting collaborators — logger, metrics, tracer — are frequently optional:
present in production, nil in a unit test or a minimal deployment. Two strategies
handle a possibly-nil collaborator. Guard at each call site (`if s.metrics != nil
{ s.metrics.Inc(...) }`) — explicit but easy to forget and noisy. Or inject a
no-op implementation once in the constructor (the null-object pattern) so every
call site is unconditionally safe. The null-object trades one tiny allocation for
the elimination of every downstream nil check and every "forgot to guard" panic;
for optional dependencies it is usually the better choice. A nil-receiver method
that guards and returns is itself a null-object (the nil pointer *is* the no-op).

### atomic.Pointer[T] and the nil-before-first-store window

`atomic.Pointer[T]` (Go 1.19+) gives a lock-free read path for a hot-swappable
snapshot: a background reloader `Store`s new config while request goroutines
`Load` it with no mutex on the read side. The trap is its zero value: a
never-stored `atomic.Pointer[T]` `Load`s as a nil `*T`. Every accessor must guard
that window and fall back to a baked-in default snapshot, so callers get a usable
config from the very first request, before any reload has happened.

### sql.Null[T] and cmp.Or

`sql.Null[T]` (Go 1.22+) generalizes `NullString`/`NullInt64`: a struct
`{ V T; Valid bool }` that implements both `sql.Scanner` and `driver.Valuer`, so
NULL round-trips as `Valid=false` in and out of the database with no sentinel
values. `cmp.Or` (Go 1.22+) returns the first non-zero of its arguments,
collapsing "use the set value, else the default" chains into one call and
complementing nil-guarded config normalization without an if-ladder.

## Common Mistakes

### Guarding after the first dereference

Wrong: `func (c *Cache) Len() int { n := len(c.data); if c == nil { return 0 };
return n }`. The `c.data` access already panicked; the guard is dead code. Fix:
the `if c == nil` check must be the first statement in the body.

### Returning a bare nil to mean "not found"

Wrong: returning `(nil, true)` or a bare nil pointer to signal "not found",
which cannot be told apart from "the stored value is nil". Fix: return comma-ok
`(value, ok bool)` or a typed sentinel error asserted with `errors.Is`, so the
caller can distinguish "no value" from "value is nil".

### Returning a typed nil pointer as an error

Wrong: `var err *AppError; ...; return err` from a function typed `error`,
producing a non-nil interface on the success path and breaking every
`if err != nil` caller. Fix: `return nil` (the untyped literal) on success;
return the concrete pointer only on real failure paths.

### Writing into a nil map

Wrong: `var m map[string]int; m[k] = 1` panics. Fix: `make` the map (or use a
composite literal) before the first write; a lazy `if m == nil { m = make(...) }`
in the write path is the idiom for a struct field.

### Collapsing absent and zero

Wrong: decoding a PATCH body or a nullable column into value fields, so a missing
key and an explicit zero become indistinguishable and PATCH/NULL semantics
corrupt. Fix: decode into `*T` or `sql.Null[T]` fields and branch on nil/Valid.

### Dereferencing an atomic.Pointer without the nil-before-store guard

Wrong: `store.p.Load().MaxConns` before the first `Store`, dereferencing nil.
Fix: an accessor that checks for nil and returns a default snapshot.

### Straight-line walking a nested pointer chain

Wrong: `p.Customer.Address.CountryCode` when any hop can be nil. Fix: a guarded
accessor that early-returns `("", false)` the moment any hop is nil.

### Scattering nil checks through hot paths

Wrong: repeating `if x != nil` in inner loops instead of normalizing once at the
boundary, hurting readability and adding branches. Fix: default and validate at
decode/load/scan; let the interior assume non-nil.

### "Fixing" a typed-nil interface with reflect

Wrong: reaching for `reflect` to detect and null out a typed-nil interface at the
call site. Fix: return untyped nil at the source; the bug is in the returning
function's signature discipline, not the caller.

Next: [01-nil-safe-cache-nil-receiver.md](01-nil-safe-cache-nil-receiver.md)
