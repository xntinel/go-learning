# Internal Packages: Enforcing Boundaries In A Real Service — Concepts

The `internal` directory is the only encapsulation boundary Go enforces above the
level of a single identifier. Unexported names (lower-case) hide symbols inside
one package; `internal` hides whole packages from the wrong importers, and it does
so in the `go` command itself — `build`, `vet`, and `list` all reject an illegal
import with a hard error, not a lint warning. A senior engineer treats `internal`
as an architectural control: a compiler-checked contract about who is allowed to
depend on what. The questions that actually decide a design are not "what is
`internal`" but "at what depth do I place it", "what does it deliberately NOT
protect", and "how do I keep the contract from rotting once the team grows". This
file is the model; the exercises that follow each turn one facet of it into a real,
independently-buildable artifact.

## Concepts

### The rule, stated precisely

Code in directory `D` may import a package located inside some `.../internal/...`
path only if `D` is rooted in the subtree whose top is the `internal` directory's
immediate parent. Concretely: given `a/internal/x`, the parent of `internal` is
`a`, so `a` and everything under `a` (including `a/b/c`) may import
`a/internal/x`; nothing outside `a` can. Given a module-root `internal/x`, the
parent is the module root, so every package in the module may import it and no
other module can. The mental shortcut that never fails: find the directory that
contains `internal`; that directory and its whole subtree are the allow-list, and
that list is the entire universe of legal importers.

### `internal` composes with unexported identifiers

They operate at different granularities and you use both. Unexported identifiers
control which symbols escape a single package. `internal` controls which packages
can be imported at all. A robust boundary uses them together: put the package
under `internal` so the wrong modules cannot import it, and keep its dangerous
symbols unexported so that even legal importers only touch the surface you meant to
offer. Reaching for one when you needed the other is a common design slip — you
cannot hide a package from a sibling module by lower-casing identifiers, and you
cannot hide a field from your own package by moving the file under `internal`.

### Placement depth equals blast radius

This is the single most important design lever. An `internal` directory at the
module root hides its contents from every other module in existence — maximal
protection, used when you ship a library and want downstream code to be physically
unable to import your implementation. An `internal` directory deep in the tree,
like `pkg/handler/internal/render`, hides its contents from everything except
`pkg/handler` and its subtree — minimal scope, used when exactly one package owns a
helper and no one else, not even a sibling package in the same module, should
touch it. You choose depth by counting the callers you intend to allow: the higher
the `internal`, the larger the allow-list. Placing it too shallow leaks a helper to
the whole module; placing it too deep locks out a legitimate internal caller who
then routes around the boundary by copying code or exporting something they should
not.

### What the rule does NOT do: no sibling isolation

The most expensive misconception in practice: two packages under the SAME
`internal` parent are not isolated from each other. `internal/auth` and
`internal/billing` can import one another freely, because both are inside the
subtree rooted at the parent of `internal` (the module root), which is precisely
the allow-list. `internal` draws one boundary — outsiders versus the allowed
subtree — and inside that subtree everything is mutually visible. If you genuinely
need `billing`'s guts hidden from `auth`, you must introduce a DEEPER boundary:
move the private code to `billing/internal/secret`, whose parent is `billing`, so
now only `billing` and its subtree can import it and `auth` cannot. Over-trusting a
single shared `internal` for peer isolation is a real, shippable architecture bug.

### It is a build-time contract, not a linter suggestion

The `internal` rule lives in the package loader of the `go` command. `go build`,
`go vet`, and `go list` all fail on an illegal import with the exact text
`use of internal package <path> not allowed`. That stability is what makes it worth
building on: you can write a test that shells out to `go build` against a fixture
that violates a layering rule and assert the toolchain rejects it, turning the
architectural contract into an executable CI gate. A rule enforced only by code
review erodes the first busy week; a rule enforced by `go build` in CI cannot be
merged around.

### Tests never weaken the boundary

A test file compiled into a package that is legally allowed to import an
`internal` package can use it like any other import. A white-box test (a `_test.go`
file in the same package as the code) can reach the package's own `internal`
dependencies, and any allowed package's external test can import an `internal`
package it is permitted to see. So there is never a reason to duplicate logic into
a test because "internal cannot be imported" — from a legal position it imports
fine. The `internal` rule blocks illegitimate importers, not the legitimate
importer's tests.

### API-surface design: expose little, hide the moving parts

The strategic use of `internal` is to make refactoring free. Expose a small, stable
public package as the entry point and push every implementation detail —
validation, wiring, driver-backed adapters, helpers — under `internal`. Downstream
modules physically cannot import the parts you expect to change, so you can rewrite
them without a breaking change, because no external consumer was ever permitted to
depend on them. The corollary is the number-one placement mistake: do NOT put the
public entry point under `internal`, or nobody can import your library at all.

### Repository / hexagonal fit

The pattern maps cleanly onto ports-and-adapters. Define the port — the interface,
`UserRepo` — in the public package, and hide the adapter — the concrete
driver-backed store — under `internal/store`. Callers receive the interface from a
public constructor; the `database/sql` types, driver rows, and connection handling
never appear on any exported signature. If a `*sql.DB` or a driver-specific type
leaks onto a public method, the whole point of hiding the store is defeated: you
have coupled consumers to persistence details you meant to keep swappable.

### export_test.go complements internal

Sometimes you want to unit-test an unexported function from an external test
package (`package foo_test`) without permanently widening the production API. The
idiom is a file named `export_test.go` in the same package as the code: because it
ends in `_test.go` it is compiled only during `go test` and stripped from the
production build, and it can re-export an unexported symbol —
`var ComputeBackoff = computeBackoff`. This composes with `internal`: the whole
package can live under `internal/retry`, its real logic stays unexported in
production, and its tests still drive that logic directly. Private, hidden, and
fully tested, all at once.

### vendor/ does not open a back door

Vendoring a dependency copies its source into your `vendor/` tree, but its
`internal` packages remain internal to that dependency. You still cannot import
another module's `internal` package after vendoring it — the loader applies the
same parent-subtree rule to the vendored paths. The boundary another team drew
holds whether their code lives in the module cache or under your `vendor/`.

## Common Mistakes

### Putting the public API under internal

Wrong: placing the entry-point package under `internal` "to keep the repo tidy".
Consumers then cannot import it at all — `internal` is for the implementation you
want to hide, never for the front door.

Fix: keep the public package outside `internal` and push only the implementation
beneath it.

### Assuming internal isolates siblings under one parent

Wrong: expecting `internal/auth` to be unable to import `internal/billing` because
"they are both internal". They share the same allow-list and import each other
freely.

Fix: when you need real peer isolation, nest a deeper `internal` under the owner —
`billing/internal/secret` — so only `billing`'s subtree can reach it.

### Treating internal as a naming convention and mis-placing it

Wrong: thinking of `internal` as a style prefix and dropping it at an arbitrary
depth. Too shallow leaks the helper to the whole module; too deep blocks a
legitimate caller who then exports something they should not.

Fix: choose the depth by the allow-list you intend — count the callers, place
`internal` so its parent subtree is exactly them.

### Sharing a helper through a plain sibling directory

Wrong: moving a helper into `pkg/handler/render` (no `internal`) so two packages
can "share" it. It is now part of the public API and a downstream module can import
and pin it, freezing your helper's signature forever.

Fix: put shared-but-private helpers under an `internal` directory whose parent
covers exactly the packages that should share them.

### Duplicating logic into a test because "internal can't be imported"

Wrong: copy-pasting an `internal` package's logic into a test file to avoid the
import. A same-package or otherwise-allowed test may import the `internal` package
directly.

Fix: import it from the legal position (white-box test, or an allowed package's
test) instead of duplicating.

### Relying on review instead of a build-time gate

Wrong: trusting code review to catch layering violations. Reviewers miss imports,
especially in large diffs.

Fix: the violation is a `go build`/`go vet` error — add a CI test that shells out
to the toolchain against a fixture and asserts the `use of internal package`
diagnostic, so the contract fails the build, not a comment thread.

### Leaking persistence types through the public API

Wrong: hiding the concrete store under `internal/store` but returning `*sql.DB` or
driver rows from an exported method. The consumer is coupled to the driver anyway.

Fix: expose only the interface and domain types; keep every `database/sql` and
driver type on unexported signatures inside `internal`.

Next: [01-handler-internal-render.md](01-handler-internal-render.md)
