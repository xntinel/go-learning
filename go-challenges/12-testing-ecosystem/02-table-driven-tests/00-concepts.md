# Table-Driven Tests for Backend Code: From Pattern to Production Suites — Concepts

Table-driven testing is the backbone of a maintainable Go backend test suite. A
growing contract — the HTTP status matrix of a handler, the rejection rules of a
validator, the mapping from domain error to status code, the classification of an
error as retryable or terminal, the parsing of a human-written byte size — is not
best expressed as a pile of `TestFooCaseA`, `TestFooCaseB` functions. It is best
expressed as one data structure: a slice of cases, each a row that names a
behavior and pins its expected outcome. The suite then documents the contract and
grows by one row instead of one function. The senior skill here is not the
`for`-loop. It is designing the case struct, choosing the right assertion shape,
keeping cases hermetic under `t.Parallel`, and knowing when a table beats a fuzz
target or a hand-written assertion. Every module that follows builds a real
backend artifact and tests it with a table; read this file once and you have the
model behind all of them.

## Concepts

### The case struct is the design surface

A table-driven test is a slice of anonymous structs. Each struct carries a `name`,
the inputs, and the expected outcome. The loop iterates the slice and runs each
element as a subtest. That much is mechanical. The design decision — the part that
separates a throwaway test from one that survives three years of the codebase — is
the *shape of the expected-outcome field*. There is no single right answer; there
is a right answer per contract:

- `want T` — the function returns a plain value and you assert equality. Right for
  pure functions (`Add`, `ParseByteSize`).
- `wantErr bool` — you only care *that* it failed, not *how*. Cheap, and often too
  weak: it passes even when the function fails for the wrong reason.
- `wantSentinel error` + `errors.Is` — you care *which* failure fired. This pins a
  specific rejection (`ErrMissingEmail` vs `ErrEmailFormat`) through a `%w` wrap
  chain. Use it whenever the identity of the error is part of the contract.
- `wantContains string` — the output is large or partly non-deterministic and you
  assert a substring (an HTTP body, a log line).
- `want T` + `cmp.Diff` with options — the output is a rich struct and you need a
  readable diff plus the ability to ignore volatile fields.
- `goldenFile string` — the output is large and structured (JSON, a rendered
  template, SQL); the expected value lives in a reviewable file under `testdata/`.

Choosing among these is the real senior decision. A table that asserts only a
`wantErr` bool where the sentinel matters is a table that will pass while the code
is subtly broken.

### t.Run creates a named, isolated, filterable subtest

`t.Run(tc.name, func(t *testing.T) { ... })` runs the case body as a subtest with
its own `*testing.T`. Three properties matter. First, the name appears in the
output, so a failure identifies itself (`--- FAIL: TestValidate/missing_email`)
without your reading the test source. Second, one case failing with `t.Fatalf`
aborts only that subtest, not the whole table — the remaining cases still run and
report. Third, you can run a single case with `go test -run
TestValidate/missing_email`. Spaces in a case name become underscores in that
`-run` path, so name cases with words that read cleanly under substitution.

A loop that calls the function directly and reports inline, without `t.Run`,
throws all three away: subtests are invisible, the first `t.Fatal` aborts the
rest, and you cannot filter.

### t.Parallel, loop variables, and hermetic cases

`t.Parallel()` on the parent test and again inside each subtest runs the cases
concurrently. Combined with `go test -race`, this surfaces bugs where cases share
mutable state. Since Go 1.22 the loop variable is scoped per iteration, so
`for _, tc := range tests` no longer needs the old `tc := tc` shadow — that line
is obsolete and should not appear in modern code. What has *not* changed is the
requirement that each case be hermetic: build a fresh `httptest.ResponseRecorder`,
a fresh `bytes.Buffer`, a fresh request inside the subtest. A single recorder
shared across rows makes cases order-dependent and breaks the instant they run in
parallel. Parallelism is not the source of the bug; it is the detector.

### Some tables must be serial: t.Setenv

Not every table can be parallel. `t.Setenv(key, value)` sets an environment
variable and restores it on cleanup, but it *panics* if called after
`t.Parallel()` — the two are mutually exclusive because a parallel test cannot own
a process-global like the environment. A config-loader table that sets `PORT`,
`TIMEOUT`, and `LOG_LEVEL` per row therefore runs serially by design. Recognizing
which tables must be serial — anything touching the environment, the working
directory, or another process global — is part of the pattern, not a limitation of
it. The trade-off is deliberate: `t.Setenv` buys you automatic, leak-free cleanup
in exchange for giving up parallelism.

### errors.Is vs errors.As in assertions

Two functions unwrap a `%w` chain, and they answer different questions.
`errors.Is(err, target)` asks *is this error, or does it wrap, that specific
sentinel value* — the right tool for `ErrNotFound`, `context.Canceled`,
`io.ErrUnexpectedEOF`. `errors.As(err, &target)` asks *does this chain contain an
error of this concrete or interface type*, and if so binds it so you can read its
fields — the right tool for extracting a `net.Error` to call `Timeout()`, or a
`*HTTPError` to read its status code. A table assertion should demand the weakest
property that is still sufficient: assert `errors.Is` when identity is enough,
reach for `errors.As` only when you must inspect the extracted value.

### cmp.Diff over reflect.DeepEqual and ==

`reflect.DeepEqual` and `==` are the wrong tools for real domain structs. They
report a bare `false` with no indication of *which* field differs; they treat a
nil slice and an empty slice as unequal even when your domain considers them the
same; they are order-sensitive on slices that represent sets; and `==` does not
even compile on structs containing slices or maps. `github.com/google/go-cmp/cmp`
fixes all of this. `cmp.Diff(want, got)` returns a human-readable `-want +got`
diff (empty string means equal), panics *explicitly* on unexported fields rather
than silently comparing them, and takes options from `cmpopts`:
`IgnoreFields(User{}, "CreatedAt", "ID")` to drop volatile fields,
`EquateEmpty()` to treat nil and empty as equal, `SortSlices(less)` to compare
set-like slices order-independently, `EquateApprox(frac, delta)` for floats. The
assertion form is `if diff := cmp.Diff(want, got, opts...); diff != "" {
t.Fatalf("mismatch (-want +got):\n%s", diff) }`.

### Golden files scale assertions on large output

When the expected value is a hundred lines of JSON or a rendered template,
inlining it in the test source is unreadable and unreviewable. A golden file puts
the expected bytes in `testdata/<name>.golden` (or `.json`), and the test reads
it and diffs. The `testdata/` directory is special to the `go` tool — it is
ignored by build and package resolution — so it is the conventional home for
fixtures. Golden files are regenerated *deliberately* behind a flag:
`var update = flag.Bool("update", false, "regenerate golden files")`, and
`go test -update` rewrites them. The discipline is that regeneration is a
reviewed step: you run `-update`, then read the diff in the pull request. A golden
test that is silently regenerated on every run asserts nothing.

### httptest turns a handler into a pure function

`httptest.NewRequest(method, target, body)` builds a `*http.Request` without a
real network, and `httptest.NewRecorder()` returns a `*httptest.ResponseRecorder`
that captures the response. Calling `handler.ServeHTTP(rec, req)` then makes the
handler a pure function of request to response: you read `rec.Code`,
`rec.Body.String()`, `rec.Header()`. The entire status-and-content-type matrix of
an endpoint — 200, 201, 400, 404, 405, 415 — collapses into one table with a
fresh recorder per row. No sockets, no ports, no flakiness.

### Table and fuzz are complementary

A table pins named, human-meaningful contracts: *this input produces exactly that
output*. A fuzz target explores the neighborhood of those contracts for panics and
invariant violations on inputs no human wrote down. They compose: seed the fuzzer
from the table by calling `f.Add(row.in)` for each representative row, then in
`f.Fuzz(func(t *testing.T, s string) { ... })` assert an invariant — round-trip
stability, no panic, no allocation blow-up. A fuzz body itself is *not*
table-driven; it receives one generated input per invocation. The table supplies
the seeds and the named contracts; the fuzzer supplies the breadth.

### Failure messages must echo the inputs

The whole point of naming cases is defeated by a bare `t.Fatal("mismatch")` inside
the loop: the output tells you a case named `overflow` failed but not what value
it produced. Every failure must print enough to diagnose without opening the test:
`t.Fatalf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)`, or the
`cmp.Diff` output for structs. The subtest name plus the echoed inputs is the
difference between a one-line fix and an afternoon of `printf` debugging.

## Common Mistakes

### One test function per case instead of one table

Wrong: `TestAddPos`, `TestAddNeg`, `TestAddZero` — three functions with the same
body and different literals. A new case means a new function, and the shared logic
drifts between copies.

Fix: one `TestAdd` with a table. A new case is one row. The table is the single
source of truth for the contract.

### Looping without t.Run

Wrong: `for _, tc := range tests { if Add(tc.a, tc.b) != tc.want { t.Fatalf(...) }
}`. Subtests are invisible in output, uncfilterable with `-run`, and the first
`t.Fatal` aborts every remaining case.

Fix: wrap each case in `t.Run(tc.name, func(t *testing.T) { ... })`.

### Sharing mutable state across cases

Wrong: one `httptest.ResponseRecorder`, one `bytes.Buffer`, or a package global
mutated by each row. Cases become order-dependent and break under `t.Parallel`.

Fix: construct a fresh fixture inside each subtest. Nothing crosses case
boundaries.

### Asserting only wantErr when the specific error matters

Wrong: `if (err != nil) != tc.wantErr { ... }` and nothing more, so a validator
that returns `ErrEmailFormat` where it should return `ErrMissingEmail` still
passes.

Fix: when the identity matters, add `errors.Is(err, tc.wantSentinel)` (or
`errors.As` to inspect the type). Assert the weakest sufficient property, but
assert it.

### reflect.DeepEqual or == on domain structs

Wrong: `reflect.DeepEqual(want, got)` fails on nil-vs-empty slices, on set-like
slices in a different order, and on volatile timestamps, and reports a bare
`false`.

Fix: `cmp.Diff(want, got, cmpopts.EquateEmpty(), cmpopts.SortSlices(less),
cmpopts.IgnoreFields(T{}, "CreatedAt"))` — readable diff, controllable equality.

### t.Setenv after t.Parallel

Wrong: calling `t.Parallel()` and then `t.Setenv(...)` in the same test — it
panics. Or mutating `os.Setenv` by hand and leaking the value into later tests.

Fix: env-dependent tables run serially. Use `t.Setenv` for automatic cleanup and
do not mark those tests parallel.

### Golden files regenerated blindly

Wrong: piping `-update` into every CI run, so the golden always matches whatever
the code produced and the assertion is a tautology.

Fix: `-update` is a local, deliberate step. Regenerate, then review the diff in the
PR before committing the new golden.

### Bare failure messages inside the loop

Wrong: `t.Fatal("fail")` — the subtest name says which case, but nothing says what
value it produced.

Fix: echo the inputs and the got/want, or print the `cmp.Diff`. The message must
be diagnosable on its own.

### Comparing JSON bytes without normalizing

Wrong: `cmp.Diff(goldenBytes, gotBytes)` where one side has different whitespace or
key order, producing a spurious diff on formatting noise.

Fix: normalize both sides through `json.Indent` (and, if key order is not stable,
unmarshal to a canonical form) before diffing.

### Forgetting -race on parallel tables

Wrong: running parallel tables without `-race`, so a data race in shared setup
stays hidden until it flakes in CI or corrupts data in production.

Fix: `go test -race` is the standing command for any suite with `t.Parallel`.

Next: [01-arithmetic-table-baseline.md](01-arithmetic-table-baseline.md)
