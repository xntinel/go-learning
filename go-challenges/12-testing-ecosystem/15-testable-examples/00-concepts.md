# Testable Examples: Documentation That Runs as Tests — Concepts

An `Example` function is the only artifact in Go that is simultaneously three
things: living documentation rendered on `pkg.go.dev`, a compile-time guard that
fails the build if the API it demonstrates changes shape, and — when it carries
an `// Output:` comment — a regression test asserted on the package's public
contract. On a real backend this is how you make a package self-documenting and
self-verifying at once: the sample a consumer reads on `pkg.go.dev` is literally
the assertion your CI runs, so the docs can never drift from the behavior. Senior
engineers reach for examples to pin the things other teams read before they read
your code — the exact JSON an endpoint returns, the sentinel a repository returns
on a miss, how money rounds, how a duration parses. This file is the conceptual
foundation; read it once and each of the ten independent exercises that follow
becomes a focused drill on one facet of the mechanism.

## Concepts

### What makes a function an example

An example is any function whose name begins with `Example`, takes no
parameters, and returns nothing. The toolchain discovers it by name alone,
exactly the way it discovers `Test*` and `Benchmark*` — there is no registration,
no build tag, no interface to satisfy. It lives in a `_test.go` file, so it is
compiled only under `go test`, never linked into your production binary. Because
it is ordinary Go compiled by the test build, an example that references a
renamed or deleted symbol fails to compile, which is the mechanism behind its
first job: a doc snippet that cannot rot, because a broken snippet breaks the
build.

### Naming attaches the example to a documentation symbol

The suffix after `Example` is not decoration; it is an address into the package's
documentation graph. `Example` (bare) attaches to the package. `ExampleParse`
attaches to the function `Parse`. `ExampleMoney` attaches to the type `Money`.
`ExampleMoney_Add` attaches to the method `Add` on `Money` — the single
underscore is what separates the type name from the method name. Get the
underscore wrong and the example silently detaches: write `ExampleMoneyAdd`
(no underscore) and instead of documenting `Money.Add` it renders as a stray,
mislabeled package-level example next to nothing. `go doc example.Money.Add`
is the ground-truth check for whether an example landed on the symbol you
intended.

You can attach several examples to the same symbol by appending a further
underscore-separated suffix that starts with a lower-case letter:
`ExampleParse_valid`, `ExampleParse_invalid`, `ExampleParse_empty` all document
`Parse`, each rendered as its own labeled block. The lower-case rule is strict:
`ExampleParse_Valid` (upper-case V) is not treated as an example variant at all,
because an upper-case letter after the underscore reads as an exported identifier
name, not a scenario label.

### The `// Output:` comment turns a doc into a test

A concluding `// Output:` comment is what promotes an example from
compiled-only-documentation into an executed test. When it is present, the test
runner captures everything the function writes to `os.Stdout` during its
execution and compares that text against the comment body, ignoring leading and
trailing whitespace on the whole block and on each line. A multi-line expectation
is just a comment block:

```go
func ExampleAdd_multiple() {
	fmt.Println(Add(10, 20))
	fmt.Println(Add(-1, 1))
	// Output:
	// 30
	// 0
}
```

Two properties are easy to get wrong. First, only a comment at the *end* of the
function body — after the last statement — is recognized as the expected-output
marker; an `// Output:` placed mid-function is an ordinary comment and the
example runs with no assertion. Second, capture sees `os.Stdout` and nothing
else. `fmt.Println`/`fmt.Printf` write there and are matched; the `println`
builtin, the `log` package's default logger, and any direct `os.Stderr` write go
to standard error, are never captured, and leave the matcher comparing against
empty stdout — so an example that "prints" via `log.Println` and expects output
fails mysteriously.

### `// Unordered output:` is a separate primitive for sets

Go deliberately randomizes map iteration order. An example that ranges a
`map[string]string` and pins the lines with a plain `// Output:` passes on your
machine and flakes in CI, because the order is not stable. `// Unordered output:`
is the tool built for exactly this: it matches the same set of lines in any
order. It is the correct primitive whenever the output is set-shaped — response
headers, an enabled-feature-flag dump, the members of a map — and their order
carries no meaning.

The alternative, when you want a *stable, reviewable* block rather than mere
order-independence, is to sort the keys yourself before printing: collect the
keys, `slices.Sort` them, then print in order and pin a plain `// Output:`. This
costs one sort but buys a godoc block that reads top-to-bottom in a meaningful
order and a diff that stays stable across runs when a reviewer inspects it. The
senior judgment call is between these two: `// Unordered output:` says "this is a
set, order is not part of the contract"; sort-then-`// Output:` says "the order
is arbitrary but I am pinning a canonical one for readability." Choose the first
for genuine sets, the second when a human will read the diff.

### An example without any output comment is compiled, not executed

Omit the output comment entirely and the example is compiled but never run. This
is not a mistake to avoid; it is a deliberate mode. It is how you ship a usage
snippet whose output is genuinely non-deterministic — a request-ID generator, a
`time.Now`-stamped event — without asserting on the flaky value. The snippet
still cannot rot, because it must compile against the current API, so it keeps
its API-drift guarantee; it simply forgoes the runtime assertion. The trade-off
to internalize: because it does not execute, any panic or wrong behavior *inside*
it is never caught at test time — only its compilation is checked. If you want
the assertion, you must engineer determinism (or write a `Test`, not an example);
if you cannot, omit the comment and accept compile-only coverage.

### Determinism is the core design constraint

Every executed example is a bet that its stdout is reproducible. Knowing which
stdout is reproducible is the skill. `json.Marshal` of a `map[string]T` sorts the
keys, so its bytes are stable and safe to pin — pinning the exact JSON a struct
marshals to is a legitimate wire-contract regression guard. But `fmt.Println` of
that same raw map prints in randomized order and must not be pinned. `time.Now`,
UUIDs, and goroutine-scheduling order are non-deterministic and belong either in
a no-output example or behind a determinism-preserving refactor. The three tools
of the trade map onto three situations: sorted structured output (`json.Marshal`,
or your own `slices.Sort`) for a stable ordered block; `// Unordered output:` for
a set; and no output comment for the irreducibly non-deterministic.

### The whole-file example is the package's headline sample

`pkg.go.dev` promotes one kind of example to the top of a package's page as its
canonical "how to use this" block: a *whole-file* example. A file qualifies when
it contains exactly one example function, at least one other top-level
declaration (a `const`, `var`, `type`, or helper func), and no `Test*`,
`Benchmark*`, or `Fuzz*` functions at all. Under those conditions the renderer
shows the entire file — imports and all — as the package's headline usage
sample, which is exactly the shape of the "getting started" block a well-shipped
SDK leads with. Drop a throwaway `TestX` into that file and it silently loses
whole-file promotion; keep the test funcs in a separate `_test.go` file to
preserve it.

### The Example-vs-Test boundary

Reach for an `Example` when the value of the artifact *is* the input-to-stdout
mapping: "call it like this, get output like that." Reach for a `Test` when you
need anything an example cannot express — arbitrary assertions beyond stdout,
error injection, `t.Cleanup`, table-driven cases, `t.Parallel`, subtests. An
example can do real setup (build structs in-line, seed an in-memory store) as
long as its stdout stays deterministic; the moment you need to assert on
something other than printed text, or to fast-forward a clock, or to register
cleanup, you have crossed into `Test` territory. The two are complementary, and
most packages ship both: examples that document the contract and tests that
exercise the corners.

## Common Mistakes

### Omitting `// Output:` when you meant to write a test

Wrong: an example that should assert its output but carries no output comment. It
compiles and renders as documentation but never executes, so a regression in the
behavior it "tests" sails through CI unnoticed. Fix: add the concluding
`// Output:` block; if the output is non-deterministic, that is a signal to
refactor for determinism or move to a `Test`.

### Placing `// Output:` anywhere but the end

Wrong: an `// Output:` comment before the final statements, or in the middle of
the body. Only a comment concluding the function body is recognized as the
expected-output marker; a mis-placed one is an ordinary comment and the example
runs with no assertion. Fix: put the block after the last statement.

### Pinning map-derived output with a plain `// Output:`

Wrong: ranging a map and pinning the lines with `// Output:`. Passes locally,
flakes in CI because map iteration is randomized. Fix: use `// Unordered output:`
for genuine set semantics, or `slices.Sort` the keys and pin a stable ordered
block.

### Printing with `println` or `log` instead of `fmt`

Wrong: `println(x)` or `log.Println(x)` inside an example with an `// Output:`
comment. Both write to stderr, the matcher only captures stdout, and the example
fails comparing against empty output. Fix: use `fmt.Println`/`fmt.Printf`.

### Mis-naming a method example

Wrong: `ExampleMoneyAdd` for a method example. Without the underscore it detaches
from `Money.Add` and renders as a stray package-level example. Fix:
`ExampleMoney_Add` — the underscore separates the type from the method. Verify
with `go doc example.Money.Add`.

### Using an upper-case scenario suffix

Wrong: `ExampleParse_Valid`. An upper-case letter after the underscore is read as
an identifier name, not a scenario label, so the toolchain does not treat it as a
`Parse` example variant. Fix: lower-case the suffix — `ExampleParse_valid`.

### Pinning non-deterministic output

Wrong: `// Output:` on a `time.Now`- or UUID-derived value, or on output whose
order depends on goroutine scheduling. The example flakes. Fix: omit the output
comment (compile-only doc) or refactor the artifact so its printed output is
deterministic.

### Breaking a whole-file example with a stray test

Wrong: adding a `Test*`, `Benchmark*`, or `Fuzz*` function into the file you
wanted rendered as the package's headline sample. It silently loses whole-file
promotion. Fix: keep that file to a single example plus other declarations; put
tests in a separate `_test.go`.

### Assuming a no-output example still runs

Wrong: believing an example without an output comment executes and would catch a
panic inside it. It does not run, so only its compilation is checked. Fix: if you
need the behavior exercised, add an `// Output:` comment (accepting the
determinism requirement) or write a real `Test`.

Next: [01-math-library-examples.md](01-math-library-examples.md)
