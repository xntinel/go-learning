# Code Modernization with go:fix and go fix — Concepts

You own an internal Go module — a client SDK or platform library — imported by
dozens of services across the company. Sooner or later you need to evolve its
API: rename a helper, split a package, move a constant, ship a v2. Every option
used to be bad. A breaking release strands every consumer until each team finds
time to migrate. A hand-written codemod (`sed`, `gofmt -r`, a one-off AST
rewriter) is not semantics-aware and routinely corrupts argument evaluation
order, name shadowing, or imports; you cannot hand a `sed` script to forty teams
and promise it will not miscompile their code. So the deprecation sits in a
`Deprecated:` comment that nobody actions, and the old symbol lives forever.

Go 1.26 changes the economics. The venerable `go fix` command was rebuilt as the
home of Go's *modernizers*, and it gained a source-level *inliner* driven by a
new directive, `//go:fix inline`. Together they turn a deprecation from a comment
into an executable, semantics-preserving migration. You annotate the old symbol
once, in the library, with the path to its replacement. Every downstream team
then runs `go fix ./...` to rewrite their own call sites — reviewable as a unified
diff, gate-able in CI with `go fix -diff ./...`. The senior mental model to carry
through this lesson: `//go:fix inline` is a versioned, compiler-aware migration
*contract* you ship with the library, not a throwaway script; and `go fix` is
built on the same analysis framework as `go vet`, so it slots into the CI gating
you already run.

## Concepts

### What go fix became in Go 1.26

The historical `go fix` fixers — the ones that rewrote pre-Go-1 API churn — were
all obsolete and have been removed. The command was rewritten atop
`golang.org/x/tools/go/analysis`, the exact framework that powers `go vet`. This
is the load-bearing fact: an analyzer written against that framework can both
*diagnose* (the `go vet` role) and *rewrite* (the `go fix` role). The same
analysis that reports "this could use `min`/`max`" can also apply the edit. That
shared foundation is why `go fix` inherits `go vet`'s package-loading, type
information, and build-tag handling, and why it fits the same CI gate you use for
`vet`.

### Two capabilities under one command

`go fix` now does two distinct things:

1. A built-in suite of modernizers — a couple dozen analyzers that update code to
   newer idioms and standard-library APIs. Representative names: `minmax`
   (`if a > b { m = a }` becomes `m = max(a, b)`), `rangeint`
   (`for i := 0; i < n; i++` becomes `for i := range n`), `slicescontains` (a
   manual search loop becomes `slices.Contains`), `slicessort`, `fmtappendf`
   (`[]byte(fmt.Sprintf(...))` becomes `fmt.Appendf(nil, ...)`),
   `stringsbuilder`, `stringscutprefix`, `forvar` (drops the now-redundant
   `x := x` loop-variable copy), `omitzero`, `any`, and more.
2. A source-level inliner that applies user-authored `//go:fix inline`
   directives. This is the self-service API-migration half: the library author
   writes the directive; the consumer's `go fix` performs the rewrite.

The first is consumption (you modernize a codebase you did not necessarily
write); the second is authorship (you ship migrations for code others depend on).
A senior engineer drives both.

### The //go:fix inline directive

`//go:fix inline` is a doc-comment directive placed on the line immediately before
a declaration. Like every `//go:` directive it is spelled exactly: two slashes,
no space, `go:fix inline`. There must be no space after `//`, no blank line
between the directive and the declaration it annotates, and it must sit directly
above the target. Get any of that wrong and it is an ordinary comment that does
nothing.

Marking a symbol tells `go fix` to rewrite every *use* of that symbol — at call
sites and references throughout the current package and any package that imports
it. The directive does not change the annotated declaration; it changes the code
that refers to it.

### The three legal targets and their restrictions

Only three kinds of declaration may carry `//go:fix inline`, each with a precise
restriction:

1. A function whose body is inlinable. Calls are replaced by the substituted
   body. This is the common case: a thin forwarder to the preferred function.
2. A constant, but only if its value refers to another *named* constant — not a
   computed expression. `const Warn = severity.Warn` is eligible;
   `const Timeout = 30 * time.Second` is not, because its value is a computed
   expression rather than a reference to a named constant, so the directive is
   silently not applied.
3. A type, but only if it is an *alias* (`type X = Y`), never a defined type
   (`type X Y`). An alias is genuinely the same type as its target, so replacing
   references is safe; a defined type is a distinct type, and substituting it
   could change program meaning.

For constants there are three legal placements of the directive: before a
single `const` declaration, before one member inside a `const (...)` group, or
before an entire group (in which case it applies to every constant in the group).

### The inliner is semantics-preserving by construction

This is the property that makes the inliner trustworthy where `sed` is not. It is
built on the type checker, and it refuses any rewrite it cannot prove safe. In
particular it:

- preserves argument evaluation order. If arguments have side effects and simple
  substitution would reorder them, it performs a hazard analysis and, when
  needed, emits an explicit binding declaration (`var x = f()`) so each argument
  is evaluated once, in order.
- handles name shadowing between caller and callee. If pasting the body would
  capture or be captured by a local name in the caller, it introduces a binding
  or a block to keep every identifier bound to what it meant in the callee.
- rewrites the callee's imports into the caller. The canonical illustration is
  the standard library's own migration: a call to `io/ioutil.ReadFile` inlines to
  `os.ReadFile` and the `io/ioutil` import is swapped for `os`. When you inline
  across packages, the target package's imports are pulled into the caller and
  rewritten accordingly.

When a parameter cannot be safely eliminated by substitution, the inliner keeps
correctness by binding it: `var p = arg`. The gofix analyzer exposes an
`allow_binding_decl` setting to control whether such binding declarations are
permitted; when they are disallowed, a call that would need one is simply left
alone rather than rewritten unsafely.

### Where the inliner refuses

Because it will not risk a behavior change, the inliner declines a rewrite
whenever it cannot prove the result is both correct and compilable:

- Bodies containing `defer`. A deferred call runs at function return; pasted
  inline it would run at the wrong time, so the inliner will not inline it. (It
  could wrap the body in a `func(){ ... }()` literal to preserve the `defer`
  timing, but it unconditionally discards such "literalization" rewrites for
  stylistic reasons — so some functions simply will not inline.)
- Rewrites that would leave a caller's local variable unused (removing its last
  use), which would be a compile error.
- Substitutions that would themselves fail to compile, for example folding a
  constant expression that is out of range.
- Bodies that reference identifiers the consumer package cannot access —
  unexported symbols from the library. Pasted into a consumer that cannot see
  them, the result would not compile, so an inlinable forwarder must reference
  only things a caller can also reference.

### Relationship to the Deprecated: convention

`//go:fix inline` complements the `Deprecated:` doc convention; it does not
replace it. A `Deprecated:` paragraph documents *why* the symbol is going away and
gives a human-readable migration for a person reading docs or an IDE hint.
`//go:fix inline` makes that migration *executable*. Ship both: the paragraph for
humans and tooling that surfaces deprecations, the directive for the mechanical
rewrite. Editors backed by gopls surface annotated call sites as a hint ("call of
X should be inlined") with a quick fix, so consumers see the migration without
even running the command.

### Workflow and CI integration

`go fix ./...` applies fixes in place. `go fix -diff ./...` prints a unified diff
of what it *would* change and writes nothing — this is the CI-gating form: run it
and fail the build if the output is non-empty, which means unmigrated call sites
(or un-applied modernizations) remain. You start from a clean git state so the
only edits a reviewer sees are `go fix`'s.

Analyzers are selectable by name, exactly like `go vet`: `go fix -rangeint .`
runs only the `rangeint` modernizer; `go fix -omitzero=false .` runs everything
except `omitzero`. `go tool fix help` lists the registered analyzers and
`go tool fix help forvar` prints one analyzer's documentation. A `-fixtool` flag
lets you point `go fix` at an alternative analysis tool built on the same
unitchecker framework, so a team can add its own house migrations. Because one
modernization can create the opportunity for another, running `go fix` twice
(until it reaches a fixed point) is normal.

### The migration lifecycle for a library owner

The directive is the contract; deleting the old symbol is a separate, later
breaking change. The sequence is: add the replacement, turn the old symbol into a
thin forwarder, annotate it with `Deprecated:` plus `//go:fix inline`, and
release. Keep that annotated forwarder in place for at least one release so
consumers have a window to run `go fix`. Only after they have migrated do you
delete the old symbol in a subsequent major version. Delete it the moment you add
the directive and you break every consumer who has not yet run the tool — the
directive only *enables* the rewrite, it does not perform it on anyone's behalf.

### Why this beats ad-hoc codemods

`sed` and regular expressions have no idea what a scope or a type is. `gofmt -r`
understands syntax but not semantics — it will happily reorder side-effecting
arguments or shadow a name. Both silently corrupt evaluation order, shadowed
names, and imports, and you discover it in production. The `go fix` inliner is
built on the type checker and *refuses* an unsafe rewrite rather than guessing.
That is what makes it a low-risk, reviewable, fan-out-safe migration path: you
can hand `go fix ./...` to forty teams because the tool itself, not your
discipline, guarantees the rewrite is semantics-preserving.

## Common Mistakes

### Writing the directive incorrectly

Wrong: `// go:fix inline` (space after the slashes), a blank line between the
directive and the declaration, or attaching it to the wrong line. Any of these
makes it an ordinary comment.

Fix: spell it exactly `//go:fix inline`, with no space after `//`, and place it on
the line immediately above the target declaration — no blank line in between, as
with every other `go:` directive.

### Marking a computed constant

Wrong: `//go:fix inline` on `const Timeout = 30 * time.Second`. The value is a
computed expression, not a reference to a named constant, so the directive is
silently not applied and consumers are never migrated.

Fix: mark a constant whose value *is* another named constant, for example
`const Timeout = defaults.RequestTimeout`. If the old constant was a literal, move
the literal to a named constant in the new location first, then forward to it.

### Marking a defined type instead of an alias

Wrong: `//go:fix inline` on `type Celsius float64` (a defined type). Only aliases
are eligible.

Fix: make it an alias — `type Celsius = units.Celsius` — so it is genuinely the
same type as its target and references can be safely replaced.

### Expecting go fix to change behavior

Wrong: assuming `go fix` can perform arbitrary logic rewrites or "fix" a bug. It
is strictly semantics-preserving; faced with anything it cannot prove safe (a
body with `defer`, a rewrite that would not compile) it refuses rather than risk
a behavior change.

Fix: treat `go fix` as a mechanical, safe migration tool. Behavior changes are
your job, in ordinary code, not the inliner's.

### Forwarding through unexported identifiers

Wrong: an inlinable forwarder whose body references package-internal, unexported
symbols. The inlined result is pasted into a consumer package that cannot see
those names, and it fails to compile.

Fix: an inlinable body must reference only what a caller can also access —
exported symbols and imported packages. Keep forwarders thin and export what the
replacement needs.

### Deleting the deprecated symbol too early

Wrong: adding `//go:fix inline` and deleting the old symbol in the same release.
Every consumer who has not yet run `go fix` no longer compiles.

Fix: keep the annotated forwarder for at least one release. The directive enables
migration; deletion is a separate, later breaking change once consumers have
migrated.

### Assuming call sites migrate themselves

Wrong: adding the directive and expecting downstream code to update on its own.

Fix: consumers must actually run `go fix` (or a migration bot must). The directive
only makes the rewrite available; it does not perform it.

### Forgetting the imports side effect

Wrong: inlining a cross-package forwarder without making the target package an
importable dependency of the consumer. Inlining pulls the callee's imports into
the caller, so the new target package must be resolvable there.

Fix: as part of the migration, ensure the replacement package is a dependency the
consumer can import (add it to `go.mod`); then the inliner's rewritten import
resolves.

### Rewriting in place in CI

Wrong: running `go fix ./...` inside CI and then being surprised by churn or by CI
mutating the tree.

Fix: gate with `go fix -diff ./...` and fail on non-empty output. Keep the actual
rewrite an explicit developer or bot action against a clean git state, so every
edit is reviewable.

Next: [01-deprecate-and-inline-a-function-api.md](01-deprecate-and-inline-a-function-api.md)
