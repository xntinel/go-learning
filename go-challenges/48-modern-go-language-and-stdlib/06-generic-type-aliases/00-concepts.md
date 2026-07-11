# Generic Type Aliases — Concepts

Go has had type aliases since 1.9 (`type Byte = uint8`), but for fifteen years an alias could not take type parameters: you could give a second name to `int`, never to "a map keyed by string". Go 1.24 completes the feature. An alias may now carry its own type-parameter list, and it may fix some of its target's type parameters while leaving others open. That small change turns aliases into a real tool for two everyday jobs: giving an ergonomic name to an awkward generic instantiation, and migrating a generic type to a new name or package without forcing every caller to change. This file is the conceptual foundation; read it once and the exercise that follows will be a matter of typing.

## Concepts

### An alias is the same type, not a new one

`type StringStore[V any] = KV[string, V]` declares `StringStore[V]` as another *name* for `KV[string, V]`. The `=` is the whole story: there is no new type here. A `*StringStore[int]` and a `*KV[string, int]` are interchangeable, assignable in both directions with no conversion, and `reflect.TypeOf` reports them as one and the same `reflect.Type`. Contrast a *defined* type, `type StringStore[V any] KV[string, V]` (no `=`), which mints a brand-new type that needs an explicit conversion to move between the two and that can carry its own distinct identity and methods. The rule of thumb: reach for an alias when you want a second *name* for the *same* type; reach for a defined type when you want a genuinely *different* type.

This transparency is exactly why an alias is useful and exactly why it is limited. Because the alias and its target are the same type, a value flows across an alias boundary for free — no conversion, no copy, no wrapper. And because they are the same type, the alias cannot give you anything the target does not already have: not a new identity, not a relaxed constraint, and (with the one nuance below) not its own methods.

### Partial application: fixing some type parameters

The new power in 1.24 is that the alias's own type-parameter list can be *shorter* than the target's, with the remaining slots filled by concrete types. `type StringStore[V any] = KV[string, V]` pins `K` to `string` and leaves `V` open. This is partial application at the type level, and it is the ergonomic win: callers write `StringStore[int]` instead of `KV[string, int]`, and because it is still the same underlying type, a `StringStore[int]` flows anywhere a `KV[string, int]` is expected. The target direction works too — an alias whose target is a plain map type, `type Set[T comparable] = map[T]struct{}`, makes a bare map literal `Set[string]{"x": {}}` a valid `Set` with no conversion, because `Set[T]` *is* `map[T]struct{}`.

### Methods and aliases: the precise rule

It is tempting to say "you cannot put methods on an alias," but that is too coarse and it is wrong. An alias is just an alternate spelling, so a method declared with an alias as its receiver base type is really a method on the underlying defined type — and Go allows that exactly when the alias names a *non-generic* defined type *in this same package*. Given `type Meter struct{ n int }` and `type Gauge = Meter`, the declaration `func (g *Gauge) Inc()` compiles and is identical to declaring `Inc` on `Meter`.

What you cannot do is attach a method through an alias that introduces type parameters or instantiation. All three of these are rejected by the compiler:

- a *generic* alias receiver — `func (s *StringStore[V]) Bad()` fails with "cannot define new methods on generic alias type";
- an *instantiated* alias receiver — `type Counter = KV[string, int]` then `func (c *Counter) Bump()` fails with "cannot define new methods on instantiated type";
- an alias to a type defined in another package, or to a non-defined type like `map[T]struct{}`, because there is no same-package defined type to attach to.

The restriction, stated precisely, is on *generic and instantiated* aliases, not on aliases as such. In practice the consequence is the same as the folk rule: you declare the methods once on the generic defined type `KV`, and every alias of it — `StringStore`, the legacy `Cache`, whatever — exposes those methods for free. If you find yourself wanting behavior that the target does not have, you do not want an alias at all; you want a defined type.

### The migration use case

Aliases shine when a generic type *moves*. Suppose `KV` is renamed, or relocated into a new package; you leave behind `type Cache[K comparable, V any] = newpkg.KV[K, V]` at the old name. Every caller that referenced `Cache` keeps compiling untouched, and — the part a defined type could never do — because the alias is the *same* type, values built through `Cache` cross the package boundary into APIs that expect `newpkg.KV` with no conversion. The alias is a zero-cost compatibility shim you delete once callers have migrated. A defined type would force a conversion at every boundary and defeat the purpose.

One structural gotcha falls out of this. The test that proves the shim works must not live in a package that would import both sides into a cycle: if the canonical `KV` is in the root package and the alias is in a subpackage that imports the root, a test placed in the root package importing the subpackage forms an import cycle. The fix is to test the alias from *within* the subpackage's own test file (an internal test in the `legacy` package), or from an external `_test` package, so the import graph stays acyclic.

### Constraints must line up

Because the alias is transparent, the target's constraints still bind. `KV` requires `K comparable`; an alias that fixes `K = string` is fine because strings are comparable, and an alias `Cache[K comparable, V any]` is fine because it forwards the same `comparable` bound. What you cannot do is use an alias to *weaken* a constraint — there is no aliasing your way to a `comparable`-keyed map that accepts a non-comparable key. The alias adds no rules and removes none; it only renames, and the original type's rules apply through it unchanged.

### Alias versus defined type, side by side

The whole decision collapses to a single question: do you want the same type or a different one?

- Same type — use an alias (`=`). Interchangeable with the target, no conversion, identical under `reflect`, inherits the target's methods, cannot carry its own methods when it is generic/instantiated, cannot change constraints. For ergonomic specialization and for migration.
- Different type — use a defined type (no `=`). Distinct identity the compiler enforces, requires an explicit conversion to/from the underlying type, can carry its own methods, can be used to keep `UserID` and `OrderID` from mixing. For type safety and new behavior.

## Common Mistakes

### Saying "you cannot add methods to an alias" and stopping there

Wrong, in both directions: the blunt rule forbids the legal case (a method on a plain alias to a non-generic same-package defined type, which compiles) and gives no reason for the illegal one. What actually fails is a method on a *generic* or *instantiated* alias — `func (s *StringStore[V]) Clear()` and `type Counter = KV[string,int]; func (c *Counter) Clear()` both produce "cannot define new methods on generic alias type" / "instantiated type".

Fix: declare the method on the underlying generic defined type (`KV`), where it is available through every alias. Remember the rule precisely — the restriction is on generic/instantiated aliases, not on aliases in general.

### Reaching for an alias when you need a distinct type

Wrong: using an alias to stop callers from mixing `UserID` and `OrderID` strings. An alias is the same type, so the compiler will happily assign one to the other; it separates nothing.

Fix: use a defined type (`type UserID string`) when you want the compiler to enforce distinct identity. Use an alias only when you genuinely want a second name for one type.

### Assuming an alias can relax a constraint

Wrong: aliasing a `comparable`-keyed map and expecting to key it with a non-comparable type, or aliasing a constrained generic and hoping the alias loosens the bound. The alias is transparent; the target's constraints still apply.

Fix: satisfy the original constraint. If you need different bounds, you need a different (defined) type, not an alias.

### Testing a cross-package migration alias from the wrong package

Wrong: putting the test for a subpackage alias `legacy.Cache = root.KV` in the root package's own test file while `legacy` imports the root — that closes an import cycle and will not compile.

Fix: test the alias from inside the `legacy` package (an internal `package legacy` test) or from an external `_test` package, so the import graph stays acyclic.

---

Next: [01-generic-aliases.md](01-generic-aliases.md)
