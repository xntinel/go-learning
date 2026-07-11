# Your First Test: The Anatomy Of a Go Test — Concepts

A senior engineer's first test is never about proving that `2 + 3 = 5`. It is
about installing a discipline that everything downstream depends on: the CI gate
that blocks a bad merge, the refactor you dare to make six months later because a
test pins the contract, the on-call debugging session where a failure message is
the only thing standing between you and a production incident at 3am. The
mechanics of a Go test are small and worth mastering exactly, because you will
write tens of thousands of them and every one rests on the same handful of rules.

This file is the conceptual foundation for the ten independent modules that
follow. Each module builds a real unit a backend actually ships — an HTTP retry
classifier, a money parser, a URL slugifier, a pagination guard, a backoff
calculator, a config-defaults filler, a byte humanizer, a feature-flag evaluator,
a manifest validator — and tests it with nothing more than the raw testing
mechanics described here. Table-driven tests, subtests, helpers, and parallelism
are introduced only in their lightest form so the mechanics stay visible; the
structured patterns are deliberately owned by later lessons (02 tables, 03
helpers, 04 subtests, 14 parallel, 16 clock injection). Read this once and you
have everything you need to reason through all ten.

## What `go test` actually is

`go test` is not a special runtime. When you run it, the `go` tool compiles an
*ephemeral test binary*: it takes the package under test, links in every
`_test.go` file in the same directory, generates a tiny synthetic `main` package
that registers every `func TestXxx(*testing.T)` it found, runs them in sequence,
and sets the process exit code. If any test called `Fail` or `FailNow` (directly
or through `Errorf`/`Fatalf`), the binary exits non-zero; otherwise it exits
zero. Then the temporary binary is discarded.

This is the whole foundation of a CI gate. CI does not understand Go semantics —
it runs a command and reads the exit code. `go test` returning non-zero is the
single signal that fails the pipeline and blocks the merge. Everything else in
this lesson — how you name a test, which failure method you call, whether you
defeat the result cache — is in service of making that one exit code trustworthy.

## The file, the package, and the load-bearing signature

Three rules govern whether the runner even sees your test:

A test file must end in `_test.go` and live in the same directory as the code it
tests. The suffix is how the toolchain separates test-only code from the shipped
package: `_test.go` files are compiled into the test binary and never into a
normal `go build`.

Inside a `_test.go` file, two package identities are allowed. `package foo`
(white-box) compiles the test into the same package as the code, so it can reach
unexported identifiers. `package foo_test` (black-box) is a separate external
package compiled into the same test binary, which sees only `foo`'s exported API.
Choosing black-box is a design decision, not a formality: it forces the test to
consume the package exactly as a real caller would, which pins the *public
contract* and surfaces awkward exported surfaces early. Reach for white-box only
when you genuinely must test an internal invariant that has no exported witness.

The test function signature is load-bearing and unforgiving:
`func TestSum(t *testing.T)`. The name must begin with `Test` followed by an
uppercase letter or a digit — `TestSum` and `Test1` are seen, `Testsum` and
`testSum` are not. `SumTest` (suffix, not prefix) is invisible. A function that
returns a value, or takes the wrong parameter, is invisible. The dangerous part
is that an invisible test still *compiles* and the package still *passes* — it
simply never runs your assertion. A green build that never executed the check is
the most insidious first-test bug there is, because nothing tells you it
happened.

## Errorf versus Fatalf: a deliberate choice

`testing.T` offers two failure verbs and choosing between them is a design act.

`t.Errorf(format, args...)` is `Logf` plus `Fail`: it records a formatted
message, marks the test failed, and *keeps executing*. Use it to accumulate every
independent assertion, so a single run reports every diff at once. When you check
five unrelated statuses in a classifier, five `t.Errorf` calls mean one run shows
you all five misclassifications, not just the first.

`t.Fatalf(format, args...)` is `Logf` plus `FailNow`: it records, marks failed,
and *stops the test immediately* by calling `runtime.Goexit`. Use it when
continuing would be unsafe or meaningless — you got a non-nil error and the value
you were about to inspect is garbage, or a constructor returned nil and the next
line would panic dereferencing it. The rule of thumb: `Fatalf` when the rest of
the test cannot run correctly, `Errorf` to gather more signal.

There is a sharp edge here. `FailNow` (and therefore `Fatalf`) stops *only the
goroutine that called it*, via `runtime.Goexit`. If you call `t.Fatalf` from a
goroutine you spawned inside the test, it kills that goroutine and the test
goroutine keeps running — the assertion never aborts the test, and the run can
report PASS while the real check failed. This is a classic false green. Always
assert on the test goroutine: send results back over a channel and check them in
the function that received `t`.

## The shape of an assertion

For a pure function the pattern is mechanical: compute `got`, name the expected
`want`, compare, and on mismatch emit a message ordered got-then-want so a reader
scanning CI output parses it the same way every time. Choose the comparison by
type. Comparable values (ints, strings, comparable structs) use `!=`. Slices,
maps, and structs with unexported fields cannot be compared with `==` (it is a
compile error for the former, and `==` compares identity not contents you want)
— use `reflect.DeepEqual` for those.

Format-verb hygiene is an operational artifact, because the future reader of that
line has no debugger. Use `%d` for integers, `%q` for strings (it shows the
quotes, escapes, and — critically — an empty string, which `%s` renders as
nothing), `%v` or `%#v` for structs (`%#v` prints the type and field names so a
struct diff is legible), and `%s` for an `error`. A message like
`Slugify(%q) = %q, want %q` tells you the input, the wrong output, and the
expectation in one glance; `slugify failed` tells you nothing.

## Testing a (value, error) contract

A function returning `(T, error)` is a two-step assertion, and the order matters.
For the success path, check the error *first* and fatally: `if err != nil {
t.Fatalf(...) }`. The value is meaningless when the error is non-nil, so
continuing to assert on it would produce a confusing second failure or a panic.
Only after the error is confirmed nil do you assert the value, and there `Errorf`
is fine. For the error path, the error itself is the assertion — check `err !=
nil` (and, once you have sentinels and `errors.Is`, check *which* error). Never
assign the returned error to `_` and test only the value; that passes on inputs
that should have failed.

## The gate is more than `go test`

A single `go test` is not a gate; it is one of four commands a senior gate runs,
and the other three catch the failures `go test` alone hides:

- `-count=1` forces re-execution and bypasses the test result cache. Go caches a
  PASS keyed on the inputs; after a change that the cache key does not capture,
  `go test` can print an instant cached PASS for code that is now broken.
  `-count=1` guarantees the tests actually ran.
- `-race` builds with the race detector and fails the run on any data race. A
  test can pass deterministically and still hide a race that corrupts production
  under load; `-race` is how you surface it.
- `go vet ./...` statically catches exactly the mistakes this lesson warns about:
  `Printf`-verb mismatches (`%d` on a string) and malformed test signatures.
- `test -z "$(gofmt -l .)"` fails on any unformatted file. Formatting is not
  cosmetic in a gate — it keeps diffs reviewable and is trivially machine-checked.

The full baseline every module in this lesson reuses is
`gofmt -l`, `go vet ./...`, and `go test -count=1 -race ./...`.

## What makes a first test trustworthy

The units worth testing first are deterministic and side-effect-free: no
`time.Now()`, no `rand`, no environment reads, no network. A test that reaches for
the wall clock or a random source flakes, and one flaky test erodes trust in the
entire suite — people start re-running until green, which defeats the gate.
Injecting a clock or a random source to make time-dependent code testable is a
real technique, deferred to lesson 16; the first tests you write should be over
pure functions where the same input always yields the same output.

Finally, know that `testing.Short()` exists. The `-short` flag sets it, and a
test can call `t.Skip("...")` to opt its slow path out of the fast local loop
while still running under the full gate. Even a first test benefits from knowing
the flag: it lets you keep an expensive exhaustive check in the suite without
paying for it on every save.

## Common Mistakes

### Misnaming the test so the runner ignores it

Wrong: `func SumTest(t *testing.T)` (suffix, not prefix), `func testSum(t
*testing.T)` (lowercase), or `func TestSum()` (no `*testing.T`). Each compiles,
the package passes, and the assertion never runs.

Fix: exactly `func TestXxx(t *testing.T)` with an uppercase letter or digit after
`Test`. `go vet` catches the wrong-signature variants.

### Reporting a failure without a `t.` failure method

Wrong: `if got != want { fmt.Println("FAIL") }` or `println(...)` or a bare
`return`. The test prints something and still exits 0 — it passes.

Fix: every failure path must call `t.Errorf` or `t.Fatalf`. Printing is not
failing.

### Using the wrong failure verb

Wrong: `t.Fatalf` on the first of several independent value checks, so the run
aborts and hides the other diffs; or `t.Errorf` after a non-nil error check,
then dereferencing the meaningless value and panicking, which masks the real
message.

Fix: `Fatalf` when continuing is unsafe (nil pointer, non-nil error you were
about to use), `Errorf` to accumulate independent assertions.

### Calling `t.Fatalf` from a spawned goroutine

Wrong: starting a goroutine inside the test and calling `t.Fatalf` from it. It
stops only that goroutine via `runtime.Goexit`; the test goroutine continues and
the run can report PASS.

Fix: send the result back over a channel and assert on the test goroutine.

### Wrong comparison or a message that hides the input

Wrong: comparing slices/maps/structs-with-unexported-fields with `==` (compile
error or identity comparison), or writing `t.Errorf("failed")` with no input and
no got/want.

Fix: `reflect.DeepEqual` for composite values, `%#v` in the message for structs,
and a fixed got-then-want order with the input included.

### Ignoring the returned error

Wrong: `got, _ := Parse(s)` and asserting only `got`. The test passes on inputs
that actually error.

Fix: check `err` first with `t.Fatalf` on the success path, and assert `err !=
nil` on the error path.

### Trusting a cached PASS or skipping `-race`

Wrong: `go test ./...` after a change, seeing an instant cached result, and
calling it green; or never running the race detector.

Fix: gate with `go test -count=1 -race ./...`, plus `go vet ./...` and
`gofmt -l .`.

### Making the first test non-deterministic

Wrong: a first test that calls `time.Now()`, `rand`, reads an env var, or hits
the network. It flakes and erodes trust in the whole suite.

Fix: keep unit-one tests pure and deterministic; inject clocks and randomness
later (lesson 16).

Next: [01-sum-library-first-test.md](01-sum-library-first-test.md)
