# Test Coverage Analysis — Concepts

Coverage is the most misread number in a Go CI pipeline. A green build that
reports "coverage: 92.4% of statements" feels like a quality gate, but it is not
one: it is a map of which lines your test suite executed, drawn without any
opinion about whether those lines produced correct answers. A senior engineer
treats coverage the way a doctor treats a fever chart — a signal that points at
where to look, never a diagnosis on its own. This file is the conceptual
foundation for the nine independent exercises that follow. Read it once and you
will know exactly what `go test -cover` measures, why `-covermode=atomic` exists,
how to attribute coverage across packages and across a running binary, how to
merge test tiers into one number, and — most importantly — why chasing that
number is the fastest way to make it meaningless.

## What statement coverage actually measures

The Go toolchain instruments code at the granularity of the *basic block*: a
straight-line run of statements with a single entry and a single exit. Before
compiling your test binary, the cover tool rewrites the source so that entering
each basic block bumps a counter, then it reports the fraction of instrumented
statements whose counter is non-zero. That fraction is *statement coverage* (the
Go documentation and output call it "coverage: N% of statements"). It is not
branch coverage, not condition coverage, not MC/DC, and not path coverage.

The distinction is not academic. Consider `if a || b { doThing() }`. Statement
coverage marks the condition and the body as covered as soon as *any* input makes
the branch taken — a test that only ever exercises `a == true` never evaluates
`b`, yet the line reports covered. A function with three independent booleans has
eight condition combinations and potentially many execution paths; a single test
input can drive statement coverage of that function to 100% while seven of the
eight combinations, and every interesting path through them, go untested. When
someone says "this function is 100% covered," the only guarantee is that every
statement ran at least once under some input. Boundary values, operand
combinations, and the actual correctness of the output are all outside what the
number can see.

## Coverage never asserts

This is the point that separates people who use coverage well from people who are
fooled by it. Instrumentation records *execution*, not *verification*. A test
that calls every exported function across a spread of inputs and contains zero
`t.Errorf`/`t.Fatalf` calls will report 100% coverage and prove absolutely
nothing about correctness — it demonstrates only that the code does not panic on
those inputs. Coverage measures the *absence of untested code*. It cannot measure
the *presence of correct tests*. Put precisely: coverage tells you what you did
not test; it can never tell you what you tested correctly. The eighth exercise
builds a subtly wrong function and two test suites that both reach 100% — one
green, one red — to make this concrete.

## `-covermode`: set, count, atomic

The counter the instrumentation bumps has three flavors, selected with
`-covermode`:

- `set` (the default for a normal `go test -cover`): the counter is effectively a
  boolean — did this block run at least once. Cheapest; loses frequency
  information.
- `count`: the counter is an integer incremented on every execution, so the
  profile records how many times each block ran. This enables hot/cold analysis:
  which branches are exercised thousands of times and which exactly once. It costs
  more than `set` because every block does a real increment.
- `atomic`: like `count`, but the increment is a `sync/atomic` add. This is the
  only mode that is correct when instrumented code runs concurrently across
  goroutines, and it is meaningfully more expensive because atomic operations
  serialize on the counter's cache line.

The reason `atomic` exists is a genuine data race. In `set`/`count` mode the
counter update is a plain memory write. If two goroutines execute the same
instrumented block at the same time — the norm for any concurrent code exercised
by parallel tests — those plain writes race, which both corrupts the count and,
under the race detector, is itself a reportable data race. For that reason the
toolchain automatically upgrades `-covermode` to `atomic` when you pass `-race`.
The trap is to *override* that by hardcoding `-covermode=set` alongside `-race`,
which reintroduces the very race the default was protecting you from. The second
exercise demonstrates this with a concurrent cache.

## The coverage profile format

`-coverprofile=cover.out` writes a plain-text profile. Its first line is the mode
(`mode: set`, `mode: count`, or `mode: atomic`). Every subsequent line is one
instrumented block:

```
example.com/svc/repo/repo.go:14.20,17.3 2 1
```

That reads: in `repo.go`, a block spanning line 14 column 20 to line 17 column 3,
containing 2 statements, executed 1 time (in `set` mode the last field is 0 or 1;
in `count`/`atomic` it is the real count). Two tools consume this file:

- `go tool cover -func=cover.out` prints one line per function with its coverage
  percentage, and a final `total:` line with the whole-profile percentage. This
  is what a CI gate parses.
- `go tool cover -html=cover.out` renders the source with covered lines in green
  and uncovered lines in red, which is how you *find* the specific untested
  branches rather than just reading a number.

The single most valuable thing a profile shows is not the headline percentage but
the red: the error branches — a failed DB call, a cancelled context, a rejected
validation, a retry that exhausted its budget — that no test ever drove. Those are
exactly the paths that fire in production at 3 a.m. and are the hardest to trigger
on purpose. The seventh exercise is entirely about reading a profile to find and
close those branches.

## `-coverpkg`: coverage across package boundaries

By default `go test` only instruments the package being tested and attributes
coverage to statements *in that same package*. That default is wrong for the shape
of a real service. In a layered service the test lives in the `handler` package
and drives an HTTP request, but the logic being exercised lives in `service/` and
`repo/`. With the default behavior, the handler test reports the handler
package's coverage and says nothing about the service and repo code it actually
ran — so a naive reading concludes those layers are untested when they are not.

`-coverpkg=pattern` fixes the attribution: it instruments every package matching
the pattern (commonly `-coverpkg=./...`) regardless of which package the test
lives in, so a request that flows handler → service → repo is credited to all
three. This both reveals the true cross-package coverage and, just as usefully,
surfaces packages and methods that *no* test touches — a repo method sitting at
0% because every test happens to take the happy path. The third exercise wires a
three-package service and shows the default number understating reality while
`-coverpkg=./...` reveals it and exposes an untouched branch.

## Coverage of a running binary: `go build -cover`, GOCOVERDIR, covdata

Unit-test coverage cannot see code exercised only by an end-to-end test that
drives a compiled server as a black box. Go 1.20 added a mechanism for exactly
that. `go build -cover -o app` produces an *instrumented binary*. When you run
that binary with the environment variable `GOCOVERDIR` pointing at an existing,
writable directory, the runtime writes raw coverage data files into that
directory as the process exits normally. Three points follow from "as it exits
normally": the process must terminate cleanly (a `kill -9` writes nothing), which
is why a graceful-shutdown path that returns from `main` is a prerequisite for
integration coverage; and the directory must already exist and be writable, or you
get no files and wrongly conclude the run covered nothing.

The raw files in GOCOVERDIR are *not* the text profile format above — they are a
binary format read by a different tool, `go tool covdata`:

- `go tool covdata percent -i=dir` prints the coverage percent from a data
  directory.
- `go tool covdata func -i=dir` prints per-function coverage, like `cover -func`.
- `go tool covdata textfmt -i=dir -o=cover.txt` converts the raw data into the
  standard text profile, so `go tool cover -func`/`-html` can consume it.
- `go tool covdata merge -i=d1,d2 -o=out` combines multiple data directories.

A hard, recurring confusion: `go tool cover` operates on the *text profile* from
`go test`, while `go tool covdata` operates on the *raw GOCOVERDIR data* from a
`-cover` binary. Passing a GOCOVERDIR directory to `go tool cover -func` fails;
they are different formats and different tools. The fourth exercise builds an
instrumented server, drives it over HTTP with a graceful shutdown, and converts
its GOCOVERDIR data into a profile.

## Merging unit and integration coverage into one number

Unit tests cover the small, fast, branch-heavy logic; integration tests cover the
wiring, the serialization, the middleware, the paths only reachable by driving the
real process. Neither number alone reflects what the suite as a whole exercised.
Because `go test -cover` can *also* write GOCOVERDIR data (set `GOCOVERDIR` when
running the test binary, or convert its `-coverprofile` output), and the
integration run already writes GOCOVERDIR data, you can `go tool covdata merge`
the two directories and `textfmt` the result into a single combined profile. The
merged `total:` is the honest whole-suite figure: it is at least as high as either
tier, and it credits branches reachable through only one tier. The fifth exercise
performs this merge.

## Gating a build on a coverage floor

The operational use of coverage is regression detection: a CI step parses the
`total:` line from `go tool cover -func` and fails the build if the percentage
dropped below a configured floor. Two design points matter. First, parse the
`total:` line by *finding* it and taking its last whitespace-separated field with
the `%` stripped — not by a fragile fixed line index, because the number of
functions above it varies. Second, set the floor deliberately *below* 100% (a
team might floor at 70-80%). The floor is a ratchet that catches a PR that deletes
tests or adds a large untested subsystem; it is not a target to maximize. The
sixth exercise builds exactly this parser as a table-tested unit.

## Goodhart's law: why the target destroys the metric

"When a measure becomes a target, it ceases to be a good measure." The instant a
coverage percentage becomes a hard number engineers must hit, the cheapest way to
hit it is to write tests that *execute* lines without *asserting* on their output:
loops that call every function, table tests with no expected values, `_ = result`
to silence the compiler. Coverage climbs; correlation with quality collapses. The
metric was only ever useful as a diagnostic — "here is code no test touches, go
look at it" — and mandating a target converts it into a ritual that produces
green builds over buggy code. The correct posture: use the profile to find
untested branches and write *asserting* tests for the ones that matter; set a
floor to catch regressions; never celebrate the number itself, and never let it
become the thing people optimize.

## Scoping the denominator

The reported percentage is a fraction, and both parts of the fraction are yours to
control. Generated code (protobuf stubs, mocks, `//go:generate` output), `main()`
wiring, and vendored code all land in the denominator under a naive `./...` and
distort the signal: they either drag the number down (encouraging low-value tests
that hit generated setters) or, if heavily executed, inflate it. The fix is to
scope the measured set with `-coverpkg` patterns and package selection — typically
`go list ./...` filtered to the packages that carry business logic — so the number
reflects the code that a unit test *should* cover, not the code that exists. The
ninth exercise scopes coverage to business packages and excludes a generated
package and a wiring `main`.

## Common Mistakes

### Treating the percentage as a target rather than a diagnostic

Wrong: mandating "100% coverage" (or any hard number) as a merge requirement.
Engineers respond by writing assertion-free tests that execute lines without
checking results; the metric climbs and stops correlating with quality — textbook
Goodhart's law.

Fix: use the profile to locate untested branches and write asserting tests for the
ones that matter. Set a *floor* well below 100% to catch regressions, and judge
tests by their assertions, not by the lines they touch.

### Believing 100% coverage means the code is correct

Wrong: reading "100.0% of statements" as "fully tested" or "correct." It means
only that every statement ran at least once under some input.

Fix: remember coverage is blind to branch combinations, boundary values, and
output correctness. A 100%-covered function can be arbitrarily wrong if its tests
never assert on what it returns.

### Running `-cover -race` while forcing `-covermode=set`

Wrong: `go test -cover -race -covermode=set`. The plain counter writes race across
parallel goroutines, corrupting counts and tripping the race detector on the
instrumentation itself.

Fix: let the toolchain default to `atomic` under `-race`, or pass
`-covermode=atomic` explicitly. Never hardcode `set` alongside `-race`.

### Concluding logic is untested because `-coverpkg` was omitted

Wrong: running a handler test without `-coverpkg`, seeing the service/repo layers
at 0% in the handler package's number, and reporting them untested.

Fix: use `-coverpkg=./...` so execution that flows into other packages is
attributed to them. The default only counts statements in the tested package.

### Forgetting GOCOVERDIR (or pointing it at a missing directory)

Wrong: running a `-cover` binary without `GOCOVERDIR` set, or with it pointed at a
directory that does not exist or is not writable, then finding no data files and
assuming the integration run covered nothing.

Fix: create the directory first, export `GOCOVERDIR=that/dir`, and ensure the
process exits cleanly so the runtime flushes coverage on exit.

### Confusing `go tool cover` with `go tool covdata`

Wrong: passing a GOCOVERDIR directory to `go tool cover -func`, or a text profile
to `go tool covdata`. They are different formats and different tools.

Fix: `go tool cover` consumes the text profile from `go test -coverprofile`;
`go tool covdata` consumes the raw GOCOVERDIR data from a `-cover` binary. Use
`covdata textfmt` to convert raw data into a profile when you need `cover`.

### Generating a profile and never reading it

Wrong: running `go test -coverprofile=cover.out` and looking only at the headline
percentage, never opening `-html` or reading `-func`, so the untested error
branches the profile reveals are never fixed.

Fix: read `-func` for the per-function gaps and `-html` for the red branches, then
write tests for the branches that break in production.

### Counting generated and wiring code in the denominator

Wrong: measuring `./...` over a module full of generated stubs and `main()`
wiring, then writing low-value tests to hit that code so the number goes up.

Fix: scope `-coverpkg` (or a filtered `go list`) to the packages that carry
business logic, so the number reflects logic coverage and does not penalize code
that should not be unit-tested.

Next: [01-coverage-basics-math-lib.md](01-coverage-basics-math-lib.md)
