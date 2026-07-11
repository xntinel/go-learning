# Type Aliases vs Type Definitions: Domain Safety and API Migration

One character separates `type UserID string` from `type UserID = string`, and
that character is a design decision with production blast radius. A definition
mints a brand-new named type; an alias introduces a second spelling for a type
that already exists. In a senior backend codebase this distinction decides
whether the compiler catches a whole class of bugs for free, and whether you can
evolve a public API without breaking every team that depends on it. This file is
the conceptual foundation for the ten independent exercises that follow; read it
once and every one of them will make sense on its own.

## Concepts

### Definition creates identity; alias creates a synonym

`type X Y` is a *type definition*. It creates a new, distinct named type `X`
whose *underlying type* is `Y`. `X` and `Y` share a memory representation but are
not the same type: a value of one is not assignable to the other without an
explicit conversion. `type X = Y` is an *alias declaration*. It does not create a
type at all; `X` becomes an alternate name for the identical type `Y`. Anywhere
the compiler compares types, an alias is completely transparent, while a
definition is a wall. The difference is *identity*, not merely interchangeability:
two defined types with the same underlying type are still two types, whereas an
alias and its target are one type with two names.

### Defined types are the cheapest domain-safety tool Go gives you

Because two distinct defined types are not mutually assignable, modeling a domain
concept as its own type turns a category of bugs into compile errors that never
reach a test, let alone production. `type UserID string` and
`type AccountID string` are both strings underneath, but a function that takes a
`UserID` will refuse an `AccountID`, so swapping them is caught the instant you
type it. `type Cents int64` cannot silently absorb a raw item count or a
`float64` price; you must convert deliberately, which is exactly the moment to
think about rounding. `type SafeHTML string` cannot be built from raw user input
except through one audited constructor. This is not defensive ceremony — it is
the language doing static analysis you would otherwise pay for with reviews,
linters, and incidents. The cost is a few explicit conversions at the boundaries;
the payoff is that domain-mixing bugs become unrepresentable.

### The method-set rule is the sharpest edge

A defined type carries its own method set, and here is the trap: when the
*underlying type is itself a named type*, the new type's method set starts
**empty**. It does not inherit the methods of the type it was defined from.
`type LocalConfig ThirdPartyConfig` gives you a `LocalConfig` that has lost every
method `ThirdPartyConfig` had — a silent, surprising loss that compiles cleanly
until you try to call a method that vanished. An alias, being the same type, keeps
the identical method set. When you want to keep a third-party type's behavior
*and* add your own, the answer is neither a bare definition (loses methods) nor an
alias (cannot add methods) but *embedding*: put the third-party type inside a
struct, and its methods are promoted while you attach new ones to the wrapper.

### You can only define methods on a type declared in the same package

Go forbids declaring a method whose receiver type is defined in another package.
This is why `type Duration = time.Duration` followed by a method declaration does
not compile: the alias *is* `time.Duration`, which lives in `time`, so the method
is not yours to add. To attach behavior to something from another package you
must define a local type (new identity, new method set) or embed it. This rule is
what forces the definition-vs-alias choice to also be a "where do the methods
live" choice.

### Aliases exist for gradual, source-compatible API migration

The primary reason aliases were added to the language is evolving a public API
without a flag day. When you need to move an exported type to a new package or
rename it, leaving `type OldName = entities.NewName` (with a `// Deprecated:` doc)
in the old location keeps every downstream caller compiling: their variables,
function signatures, and struct fields still reference a type that is now
literally the same type as the new one. Values flow between old and new names with
no conversion. A plain redefinition would break this — `type OldName NewName`
makes the two types incompatible, forcing every caller to insert conversions in
lockstep with your release. The alias buys you a multi-release deprecation window;
the definition would demand a coordinated migration across teams you do not
control.

### Go 1.24 makes generic type aliases first-class

Before Go 1.24 a type alias could not have type parameters. As of 1.24 you can
write `type Set[T comparable] = map[T]struct{}`, a parameterized alias usable
interchangeably with the underlying generic shape across package boundaries. This
lets teams share a common generic spelling (a `Set`, a `Result`, an
`Optional`) without each package minting an incompatible defined type. The old
constraints still hold: an alias carries no methods, so when the shared shape
needs `Add`/`Has`/`Union` you must reach for a defined generic type
(`type SetT[T comparable] map[T]struct{}`) instead. The rule of thumb: alias for a
shared *shape*, definition for shared *behavior*.

### The MarshalJSON recursion guard is a definition, not an alias

The most cited "alias trick" in Go is not an alias at all. A custom
`MarshalJSON` that calls `json.Marshal(v)` on its own receiver type re-enters
`MarshalJSON` and recurses until the stack overflows. The fix is to declare a
*local type definition* — `type noMethods DomainType` — whose method set is empty
(the underlying type is a named type), then marshal a value converted to that
methodless type. Because it is a definition, `json.Marshal` sees no `MarshalJSON`
and serializes the fields directly. Writing `type noMethods = DomainType` would be
a real alias, keep the marshaler, and recurse exactly as before. The load-bearing
detail is that the guard *must* be a definition; the colloquial name "alias" is
misleading.

### Defined string types as trust boundaries

`html/template.HTML` is a `type HTML string`: a defined string type that means
"this text has already been escaped and is safe to emit". The pattern generalizes.
A defined string type can encode a *capability* — trusted, validated, canonical —
and because the compiler forbids implicit conversion from the raw underlying type,
the only way to obtain the trusted type is through a constructor you audit. That
constructor is the single chokepoint where untrusted becomes trusted; everything
downstream can rely on the type as a proof-carrying token. The type system
enforces "you cannot emit unescaped user input here" without a single runtime
check at the call site.

### Aliases you already depend on

`byte` is an alias for `uint8`, `rune` is an alias for `int32`, and (since Go 1.18)
`any` is an alias for `interface{}`. These are alias declarations in the
predeclared universe, which is exactly why `byte` and `uint8` interchange freely
and why `any` is not a distinct type you must convert to. Recognizing them as
aliases explains behavior you have relied on for years.

### Numeric units as defined types

`time.Duration` is the canonical model: a `type Duration int64` with a
human-readable `String()`, typed constants (`time.Second`), and a parse boundary
(`time.ParseDuration`). You can build the same shape for your own quantities — a
`ByteSize` for config limits, a `Cents` for money — attaching methods, named
constants, and a single parsing entry point to a raw integer while preventing
accidental mixing of unrelated quantities. The defined type is what makes
`5 * time.Second` self-documenting and `duration + rawInt` a compile error.

## Common Mistakes

### Reaching for an alias when you wanted safety

Wrong: `type UserID = string` and `type AccountID = string`. Both names are just
`string`, so the compiler cannot tell a user id from an account id and the safety
you wanted does not exist. Fix: use definitions (`type UserID string`) so the two
domains are distinct types.

### Using a definition for an API rename

Wrong: rename an exported type with `type NewName OldName` and expect existing
callers to stay assignment-compatible. They will not; every call site must now
convert. Fix: during a compatibility window use an alias
(`type OldName = NewName`) so both names describe the identical type.

### Adding methods to an alias of a non-local type

Wrong: `type Duration = time.Duration` then declaring a method on `Duration`. The
compiler rejects it because the alias is still `time.Duration`, owned by `time`.
Fix: define a local type or embed the foreign type in a struct.

### Being surprised that a wrapping definition lost its methods

Wrong: `type LocalConfig ThirdParty` and then calling a `ThirdParty` method on a
`LocalConfig`. The defined type's method set is empty. Fix: embed `ThirdParty` in
a struct to promote its methods, then add your own on the wrapper.

### Recursing in MarshalJSON

Wrong: `func (v T) MarshalJSON() ([]byte, error) { return json.Marshal(v) }` —
infinite recursion. Fix: define a local methodless type
(`type noMethods T; json.Marshal(noMethods(v))`). And do not "fix" it with
`type noMethods = T`; a real alias keeps the marshaler and still recurses.

### Pinning go.mod below 1.24 for generic aliases

Wrong: writing `type Set[T comparable] = map[T]struct{}` with `go 1.23` (or an
older toolchain) in go.mod and being surprised it fails to compile. Fix: require
`go 1.24` or newer.

### Scattering domain validation across handlers

Wrong: passing raw request strings straight through, with `switch` statements
re-deriving whether a status is valid or a transition is legal at every call site.
Fix: attach `Valid()` and `CanTransitionTo()` methods to the defined type and
funnel untrusted input through a single validating constructor at the boundary.

### Littering conversions instead of using constructors

Wrong: sprinkling `UserID(s)` conversions everywhere because two defined types
with the same underlying type still need an explicit conversion between them. Fix:
convert once, at the trust boundary, inside a validating constructor, and pass the
already-typed value inward.

Next: [01-domain-ids-and-legacy-alias.md](01-domain-ids-and-legacy-alias.md)
