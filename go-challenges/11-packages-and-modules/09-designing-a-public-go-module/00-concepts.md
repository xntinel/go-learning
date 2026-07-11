# Designing a Public Go Module: API Surface, Docs, and Compatibility — Concepts

A public module — or the company-internal shared kit that every service in the
org imports — is not code you write once and forget. It is a long-lived contract.
The moment you export a symbol and tag a version, every consumer's build and every
downstream release is coupled to it. Refactor a signature and dozens of services
fail to compile on their next `go get`. Change what an error value is and the
callers who matched on it silently take the wrong branch. This lesson trains the
disciplines whoever owns that shared repository applies every day: keep the
exported surface as small as the job allows so future refactors stay
non-breaking; make documentation and errors part of the contract rather than an
afterthought; evolve behavior through options and config instead of signature
changes; gate releases with an API-compatibility check so a careless edit does not
ship as a minor bump and break the org; and know exactly when and how to pay the
cost of a `/v2` major version. Read this once and you have the model for the nine
independent exercises that follow, each of which builds one piece of that
discipline against the same small `publicstr` string library.

## Concepts

### The API surface is behavior, not just signatures

The public API surface is every exported identifier *plus its observable
behavior*. A consumer can — and eventually will — depend on far more than the
function signature: the order of a multi-value return, the identity of an error
value, the zero-value semantics of an exported struct, whether a method is
safe for concurrent use, even a documented panic. All of that is contract. If
`Slugify` returns `(string, error)` and today the error is always `ErrEmpty`,
some consumer somewhere writes `errors.Is(err, ErrEmpty)`, and now the *identity*
of that error is frozen too. "I only changed the internals" is the sentence that
precedes most accidental breaks; the internals a consumer can observe are not
internal.

### The governing rule: add, do not change or remove

There is no compatible way to change a signature or remove an exported symbol.
Adding a parameter, retyping a return, renaming a field, deleting a function — all
break consumers at compile time. The rule that makes a module evolvable is
therefore mechanical: *add, do not change or remove*. When behavior must grow, add
a sibling and let the old one delegate. This is exactly why Go's own standard
library grew `QueryContext` next to `Query`, `context`-aware variants everywhere,
rather than editing the existing methods — editing them would have broken every
program in existence. Your shared library follows the same discipline: the surface
only ever grows, and every old entry point keeps working.

### The three compatibility tiers map straight to SemVer

Every change lands in one of three tiers, and each tier dictates the version
component you may bump:

- No visible API change (a bug fix, an internal refactor) — patch: `v1.4.2` to
  `v1.4.3`.
- Backward-compatible addition (a new function, a new option, a new exported
  type) — minor: `v1.4.2` to `v1.5.0`.
- Any incompatible change (removed, renamed, or retyped symbol; changed
  signature or return tuple) — a new major version with a *new import path*:
  `example.com/publicstr` to `example.com/publicstr/v2`.

There is no fourth option. You cannot remove or retype an exported symbol
"compatibly" by being clever; the only compatible move is to leave it in place and
add alongside it. A release-compatibility tool exists precisely to classify a diff
into one of these tiers mechanically, so the decision is never left to a tired
reviewer's memory.

### Documentation is executable contract, not prose

Two conventions turn doc comments from decoration into contract. First, a doc
comment must start with the symbol name — `// Slugify converts ...`, not
`// This function slugifies ...` — because `go doc` and pkg.go.dev associate the
comment with the symbol by that prefix; get it wrong and the rendered docs simply
lose the comment. Second, an `Example` function whose body ends in an `// Output:`
comment is *compiled and run by `go test`*. The documented usage that appears on
pkg.go.dev is the same code the CI suite executes, so it cannot silently drift
from real behavior — if you change what `Slugify` returns, the example's
`// Output:` line fails the build until you fix it. Docs that are also tests are
the only docs that stay honest under maintenance.

### Errors are API

A sentinel created with `errors.New` promises *identity*: consumers match it with
`errors.Is`, so its value is frozen the moment it ships. A typed error promises
*fields*: consumers reach into it with `errors.As`, so its concrete type and the
fields they read are frozen too. Wrapping with `fmt.Errorf("...: %w", err)`
preserves both through the chain, so a caller can still `errors.Is` the sentinel
under three layers of context. Design the error surface as deliberately as the
function surface: decide which failures are sentinels (branchable identity), which
are typed (structured detail), and never change a sentinel's value or a typed
error's shape once consumers depend on it.

### The smallest public surface is the safest one

Anything exported is frozen the moment it ships; anything unexported can be
refactored freely forever. The `internal/` directory is the enforcement
mechanism: a package under `.../internal/normalize` is importable from within the
module (and its subtree) but the compiler *rejects* an import from any other
module. So you put the rune-classification and hyphen-collapse helpers there,
expose only the entry points consumers actually need, and keep the freedom to
rewrite the helpers on any afternoon. Export intent, not implementation. Every
helper you export "just in case" is a future refactor you have forbidden yourself.

### Options and config keep signatures stable forever

Functional options and struct-config exist for one reason: to add knobs without
ever touching a signature. `SlugifyWith(s string, opts ...Option)` can grow a new
`WithMaxLen`, `WithSeparator`, `WithFallback` for the rest of time, and each is a
purely additive, source- and binary-compatible change — a new `Option`
constructor, never a new positional parameter. The alternative — adding
parameters — forces a breaking signature change on every consumer every time a
feature is added. Keep the ergonomic one-liner (`Slugify`) as a stable shim that
delegates to the options path, and evolve through options underneath it.

### Deprecation is a migration signal, not a removal

When a symbol's design is wrong but consumers rely on it, you do not delete it —
deletion forces an avoidable major bump on everyone. You add the correct successor
and mark the old symbol with a `// Deprecated:` paragraph in its doc comment. That
exact convention is recognized by `gopls` and by staticcheck (which flags uses as
SA1019) and rendered specially on pkg.go.dev, so consumers see the migration
signal in their editor. The deprecated symbol stays as a working shim — often
delegating to the successor — so nobody is forced to migrate on your schedule.
Deprecation is how you steer a large consumer base off a mistake without a
flag-day.

### Semantic import versioning makes a major bump survivable

A breaking change is not forbidden; it is *expensive*, and Go makes the expense
payable incrementally. Semantic import versioning encodes the major version in the
path: `v2` and above live in a module whose `go.mod` declares
`module example.com/publicstr/v2` (conventionally in a `v2/` subdirectory), so
`example.com/publicstr` and `example.com/publicstr/v2` are *different packages*
that a single program can import side by side. That is what turns a major bump
from a flag-day into a gradual migration: a consumer updates call sites one at a
time, running both majors during the transition, instead of everyone cutting over
at once.

### v0 is the honest place to iterate; v1 is a commitment

A `v0.x` module carries no compatibility promise — `v0.4.0` to `v0.5.0` may break
anything. That is the honest place to iterate on a design you are still shaping.
The moment you tag `v1.0.0` you have committed to the whole discipline above.
Leaving v0 is therefore itself an API-governance decision, not a formality: you
are declaring the surface stable enough to freeze. Do not tag v1.0.0 to look
finished; tag it when you are prepared to keep every exported symbol working
indefinitely.

### An API-compat tool turns "did we break someone" into a gate

Relying on human review to catch API breaks does not scale past a handful of
symbols. `gorelease` (built on `golang.org/x/exp/apidiff`) compares the current
tree against a tagged base, classifies the diff into the three tiers, and *names
the correct next version* — refusing to let an incompatible change ship as a minor
bump. Wired as a CI step, it is the mechanical guardrail that protects dozens of
downstream services from a careless edit. The version-bump decision stops being a
judgment call and becomes a pass/fail check.

## Common Mistakes

### Exporting internal helpers

Wrong: exporting a `NormalizeRune` or `CollapseHyphens` helper "so it is reusable",
freezing it into the contract forever.

Fix: keep helpers unexported, or move them under `internal/` so the compiler
forbids outside imports. Export only the entry points consumers need; everything
else stays refactorable.

### Doc comments that do not start with the symbol name

Wrong: `// This function slugifies a string.` — `go doc` and pkg.go.dev cannot
associate the comment with `Slugify`, so the rendered docs lose it.

Fix: start every doc comment with the symbol name: `// Slugify converts ...`. A
parse-time doc-lint test can enforce this so it never regresses.

### Treating documentation examples as prose that rots

Wrong: a fenced usage snippet in a comment that no test ever runs; six months
later it no longer compiles or prints what it claims.

Fix: write an `Example` function ending in `// Output:`. `go test` compiles and
runs it, so the documented output cannot drift from real behavior.

### Changing a signature or return tuple in a v1.x release

Wrong: `Truncate(s string, n int) (string, error)` becomes
`Truncate(s string, n int) (string, bool)` in a patch or minor release — every
consumer's build breaks on the next `go get`.

Fix: add a new function and delegate, or pay for the break with a `/v2` module.
Never retype an exported symbol inside a major version.

### Adding configuration as new positional parameters

Wrong: `Slugify(s string, maxLen int, sep rune)` — every added knob is a breaking
signature change.

Fix: `SlugifyWith(s string, opts ...Option)` (or a `*Config` where nil means
defaults). New knobs are additive `Option` constructors, forever compatible.

### Changing a sentinel's value or a typed error's shape

Wrong: replacing `ErrEmpty = errors.New("empty string")` with a different value,
or renaming a field on a typed error, after consumers started matching with
`errors.Is`/`errors.As`. Their error handling silently breaks.

Fix: treat error identity and structure as frozen contract. Add new error values;
do not mutate the ones consumers branch on.

### Deleting a symbol instead of deprecating it

Wrong: renaming `Reverse` to `ReverseBytes` and deleting the old name, forcing a
major migration on everyone.

Fix: add the successor, mark the old symbol `// Deprecated:`, and keep it working
as a shim. Consumers migrate on their own schedule with no forced major bump.

### Shipping a breaking v2 at the same import path

Wrong: tagging `v2.0.0` on `example.com/publicstr` with a changed API. Consumers
cannot depend on v1 and v2 at once, so migration becomes a flag-day.

Fix: put v2 in a module whose `go.mod` says `module example.com/publicstr/v2`.
The two majors are distinct, independently resolvable packages.

### Relying on review instead of a tool to catch breaks

Wrong: trusting a human reviewer to notice that a return tuple changed, then
shipping it as a minor bump.

Fix: run `gorelease`/`apidiff` in CI against the released base. The tool
classifies the diff and refuses an incompatible change under a minor version.

### Reversing by bytes but documenting it as reversing "the string"

Wrong: `Reverse` swaps bytes and the doc says it reverses the string, corrupting
any multi-byte UTF-8 input.

Fix: either operate on `[]rune`, or document the byte-level behavior explicitly and
provide a rune-safe successor. Say exactly what the function does.

Next: [01-slug-library-core-and-sentinel-error.md](01-slug-library-core-and-sentinel-error.md)
