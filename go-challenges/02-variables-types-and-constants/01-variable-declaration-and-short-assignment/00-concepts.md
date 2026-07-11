# Variable Declaration and Short Assignment in Production Go — Concepts

In real backend code, choosing between `var`, `:=`, and `=` is never a matter of
syntax taste. It is a set of decisions about where a value lives, who is allowed
to mutate it, whether the zero value is a safe starting policy, whether an outer
error survives an inner branch, whether callers can match a sentinel by identity,
and whether a global leaks untestable mutable state into a process. A config
loader, a repository's sentinel errors, a transaction commit path, a cache lookup,
an interface guard, an address parser, a request router, and a deferred-cleanup
close all bite differently depending on which declaration form you reach for. This
file is the conceptual foundation; read it once and each of the independent
exercises that follow becomes an application of one of these decisions to a real
artifact.

## Concepts

### `var` declares an identifier independent of any runtime initializer

`var` is the only declaration legal at package scope, and it is the only form
that can name a value with no initializer at all (giving you the zero value), name
a value whose *type* is part of the API contract, or name a value produced by a
function call. That last point is why sentinel errors are `var` and not `const`:
`errors.New("...")` is a function call, and a `const` in Go may only be a compile
-time constant expression. You cannot write `const ErrNotFound = errors.New(...)`.

```go
var ErrNotFound = errors.New("user not found") // function call: must be var
```

A `var` at package scope with an explicit type also freezes the contract you
expose. `var buf bytes.Buffer` names a ready-to-use zero value; `var w io.Writer`
names an interface a caller must fill. The explicit type is documentation the
compiler enforces.

### `:=` is a statement, and its defining property is scope

Short variable declaration is legal only inside a function body. It both declares
and infers the type from a nearby expression, which reads cleanly for local
derived values. But its most important property is not brevity — it is *lifetime*.
A `:=` in the body of an `if`, `for`, or `switch`, or in the init clause of one of
those statements, creates variables that cease to exist at the end of that block.
Choosing `:=` in an init clause is choosing to confine a value to exactly the
branch that consumes it, so it cannot be reused stale later.

```go
if id, err := parseID(r); err != nil { // id and err die at the closing brace
	http.Error(w, "bad id", http.StatusBadRequest)
	return
}
```

That narrow lifetime is usually exactly what you want for a parsed intermediate.
It becomes a hazard only when an *outer* variable of the same name is also in
play — the shadowing case below.

### `=` reassigns; confusing it with `:=` is how errors get shadowed

`=` assigns to an already-declared variable. The single most damaging mistake in
this whole topic is writing `:=` where you meant `=`. Inside an inner block,
`x, err := f()` declares a *new* `err` local to that block. If your function's
control flow later inspects an outer `err`, it inspects the outer one, which is
still `nil`, and the inner failure vanishes. A failed `Commit`, a failed
`Rollback`, a failed second write — silently dropped. The fix is to declare the
destination once and use `=` for the reassignment, or to return immediately from
the narrow scope so there is no outer variable to desynchronize from. `go vet`'s
shadow analysis (`go vet -vettool=$(which shadow)`) exists precisely to catch this
in review.

### In a multi-name `:=`, at least one name must be new

`a, b := f()` requires that at least one of `a`, `b` be a fresh identifier; any
name already declared *in the same block* is assigned to, not redeclared. This is
what lets you write `x, err := f()` and then `y, err := g()` in one block and
reuse the same `err`. The trap is the phrase "in the same block": across a block
boundary — inside an `if` body, a `for` body — *all* the names on the left are
fresh, so the inner `err` is a brand-new variable even though an outer `err`
exists. Same source text, opposite meaning, depending on whether a new block was
entered.

### The zero value is a starting policy, not a final one

Every `var cfg Config` starts with every field at its zero value: `""`, `0`,
`false`, `nil`. That is a deliberate, defined starting point, but it is almost
never the intended operational default. A missing `REQUEST_TIMEOUT` that silently
becomes `0` is not "no configured timeout" — it is "no timeout at all", which is a
production incident waiting to happen. A boolean feature flag defaulting to `false`
may be the opposite of the safe default. So a loader sets explicit operational
defaults *before* applying overrides, and never assumes zero equals intended.

### Comma-ok is the only way to tell absent from present-but-zero

Three lookups return an optional second boolean: `v, ok := m[k]`,
`v, ok := x.(T)`, and `v, ok := <-ch`. That `ok` is the only correct way to
distinguish "the key is absent" from "the key is present and its value happens to
be the zero value". Drop the `ok` and a feature flag that was explicitly set to
`false` is indistinguishable from one that was never set; a cached count of `0` is
indistinguishable from a cache miss; an empty string in a map is indistinguishable
from an absent key. This is one of the most common latent bugs in cache and
feature-flag code, and it is invisible until the zero value means something.

### Sentinel errors are package-level `var` matched by identity

A sentinel error is a package-level `var` created with `errors.New` so that
callers match it by *identity* with `errors.Is`, never by comparing
`err.Error()` strings. String comparison is brittle: it breaks the moment someone
rewords the message or wraps it with context. The contract is: declare
`var ErrUserNotFound = errors.New("user not found")` at package scope, wrap it on
the way out with `fmt.Errorf("get user %s: %w", id, err)` to add context while
preserving the chain, and let the HTTP layer map `errors.Is(err, ErrUserNotFound)`
to a 404. `errors.Join` composes several sentinels into one error whose chain
matches all of them.

### The blank identifier `_` is an intentional tool

`_` is not a lint silencer; it is a way to state, in code, that a value is
deliberately unused. Its two production uses here are the compile-time interface
guard and the provably-irrelevant discard. `var _ Store = (*PostgresStore)(nil)`
costs nothing at runtime and forces the compiler to prove `*PostgresStore`
satisfies `Store` — so an incomplete implementation fails to *build* rather than
failing at some call site later. Discarding a specific return, `n, _ := buf.Write(b)`
on an `io.Writer` that cannot fail (`bytes.Buffer`, `strings.Builder`), is honest;
the same on a real network writer hides a real failure and is a bug.

### Package-level string `var`s are the only `-ldflags -X` targets

Build metadata is injected at link time with `go build -ldflags "-X pkg.Version=..."`.
`-X` can only patch a package-level `var` of type `string`. Not a `const` (it is
baked in), not a non-string, not a local. Grouping the set in one `var` block —
`var ( Version = "dev"; Commit = "none"; BuildTime = "unknown" )` — communicates
that they are a cohesive unit and gives each a sane fallback. When no `-ldflags`
are supplied (a plain `go run`, a `go install` of a VCS-tracked module), the
fallback is `runtime/debug.ReadBuildInfo()`, whose `Settings` carry
`vcs.revision`, `vcs.time`, and `vcs.modified` for modules built from a checkout.

### Named returns earn their place for deferred error cleanup

Named return values — `func exportReport() (result Report, err error)` — are worth
the readability cost in exactly one common situation: a deferred closure must
observe and possibly overwrite the returned error. The canonical case is closing a
file or flushing a writer, where a `Close` error must surface if the body
succeeded but the flush failed. Only a named `err` is visible to a deferred
closure and assignable there. Outside that pattern, prefer explicit returns; and
avoid *naked* returns (`return` with no operands) in any function long enough that
the reader cannot see all the named results at once.

### Multiple assignment evaluates the whole right-hand side first

In `a, b = expr1, expr2`, both right-hand expressions are fully evaluated before
any assignment happens. That is what makes `a, b = b, a` a correct swap with no
temporary, and what makes tuple parsing like `host, port, err := net.SplitHostPort(addr)`
clean and total. It also means a normalization such as
`if lo > hi { lo, hi = hi, lo }` reliably yields `lo <= hi` without a scratch
variable.

### Mutable package-level state is untestable and order-dependent

Loading environment variables into mutable package-level variables during `init()`
is one of the most common architecture mistakes in Go services. It is untestable
(a test cannot supply a different environment without mutating global state and
racing every other test), it is order-dependent (which `init` ran first?), and it
hides the flow of configuration into the process. The fix is structural: return a
`Config` *value* from a function and let each command decide how configuration
enters. A value passed explicitly is testable, parallelizable, and honest about
its dependencies.

## Common Mistakes

### Shadowing the error you meant to return

Wrong: `x, err := f()` inside an `if` or `for` body redeclares `err`, so a later
top-level `if err != nil` inspects the outer variable, which is still `nil`, and
the inner failure is silently dropped.

Fix: declare the destination once and use `=` for the reassignment, or return
immediately from the narrow scope. Run `go vet`'s shadow analyzer in review.

### Using `:=` at package scope

Wrong: `ErrMissing := errors.New("...")` at package scope does not compile. Short
assignment is a function-body statement.

Fix: package-level identifiers use `var` (or `const` for true constants).

### Dropping the `ok` in a map or type-assertion lookup

Wrong: `v := m[key]` or `v := x.(T)` conflates an absent key with a key set to the
zero value — a classic feature-flag and cache bug where an unset flag reads as an
explicit `false`.

Fix: use the two-result form `v, ok := m[key]` and branch on `ok`.

### Loading configuration into globals in `init()`

Wrong: reading env vars into mutable package-level variables at init time.
Untestable, order-dependent, hides configuration flow.

Fix: return a `Config` from a function; pass it explicitly.

### Declaring a sentinel locally or comparing errors by string

Wrong: a per-call `errors.New` or `if err.Error() == "not found"`. Callers cannot
match a local by identity, and string matching breaks on rewording or wrapping.

Fix: a package-level `var` sentinel plus `%w` wrapping, matched with `errors.Is`.

### Setting build metadata on a non-string or non-package-level variable

Wrong: `-ldflags "-X main.buildTime=..."` against a `const`, a non-string, or a
local. `-X` silently does nothing.

Fix: target a package-level string `var`; provide a `ReadBuildInfo` fallback.

### Overusing named returns and naked returns

Wrong: named results with naked `return`s scattered through a long function, so
the reader cannot tell what is being returned.

Fix: reserve named returns for the deferred-cleanup error-mutation pattern; return
explicitly elsewhere.

### Assuming the zero value is the intended default

Wrong: skipping explicit defaults, so a missing `REQUEST_TIMEOUT` becomes `0` (no
timeout) instead of a safe operational value.

Fix: set explicit operational defaults before applying overrides.

### Discarding an error with `_` where it is not provably irrelevant

Wrong: `n, _ := conn.Write(b)` on a real network writer hides partial writes and
connection failures.

Fix: only discard where the writer cannot fail (`bytes.Buffer`, `strings.Builder`);
otherwise check the error.

### Wide-scoped temporaries instead of init-statement scope

Wrong: declaring a parsed value at function scope, letting a later branch reuse it
stale.

Fix: put the declaration in the `if`/`switch` init clause so it dies with the
branch that consumes it.

Next: [01-config-loader-declaration-choices.md](01-config-loader-declaration-choices.md)
