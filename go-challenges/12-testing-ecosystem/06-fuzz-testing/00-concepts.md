# Fuzz Testing Production Code Paths — Concepts

Every service has a perimeter where bytes it did not write cross into memory it
controls: an HTTP header line, a request body, a URL path, a wire frame, a
dotted-quad address in a log line. Example-based tests cover the inputs you
imagined. The inputs that page you at 3 a.m. are the ones you did not — a
truncated frame, a header with an embedded newline, a path with a `..` buried
three segments deep, an attempt counter large enough to overflow a shift. Go's
built-in coverage-guided fuzzer (`go test -fuzz`, stable since Go 1.18) exists to
generate exactly those inputs and drive them into your code until a property you
asserted breaks. This file is the conceptual foundation for the ten independent
exercises that follow; each builds one real perimeter component and fuzzes it
with one of the durable property patterns. Read this once and you have the model
you need for all of them.

## Concepts

### What a fuzz target actually is

A fuzz target is a function named `FuzzXxx(f *testing.F)` in a `_test.go` file
that makes exactly one call to `f.Fuzz`. The argument to `f.Fuzz` is itself a
function: `f.Fuzz(func(t *testing.T, a A, b B, ...) { ... })`. The fuzzing engine
calls that inner function over and over with generated values for `a, b, ...`.
Those argument types are not arbitrary — they come from a fixed allowed set:
`[]byte`, `string`, `bool`, `byte`, `rune`, `float32`, `float64`, and every
sized integer (`int`, `int8`…`int64`, `uint`, `uint8`…`uint64`). No structs, no
slices of anything other than `byte`, no maps. If your target needs a richer
input — an operation stream for a state machine, a struct — you fuzz one of the
allowed types (usually `[]byte`) and *decode* it into your richer input inside
the body. That decode-in-the-body technique is what turns a flat `[]byte` into a
sequence of rate-limiter operations in the stateful exercise.

### Seeds shape coverage; they are not the test

`f.Add(args...)` registers a seed input in the corpus. The argument list of every
`f.Add` must match the `f.Fuzz` function's arguments in both type and arity
exactly — a mismatch is a compile error, not a skipped seed. Seeds do two jobs:
they give the mutation engine good starting points (a valid frame is a better
place to start mutating than random noise), and, crucially, they run as ordinary
sub-tests under *plain* `go test` with no `-fuzz` flag. That second job is why
seeds double as a regression suite: a seed that once crashed the code, committed
to the corpus, runs on every CI build forever.

### The engine is coverage-guided, so the body must be fast and deterministic

The fuzzer instruments your code and keeps any generated input that reaches a new
code edge, mutating from there. This is what lets it discover, in seconds, the
one branch that a naive random generator would take an age to hit. The mechanism
has a hard prerequisite: the fuzz body must be deterministic and fast. If the
body reads `time.Now()`, iterates a map (whose order is randomized), spawns
goroutines, touches global state, or does network I/O, then the same input can
produce different behavior on replay. The engine cannot minimize what it cannot
reproduce, coverage guidance degrades into noise, and any "failure" it reports is
a flake you can never pin down. A fuzz body is a pure function from its arguments
to pass/fail. Keep it that way.

### Properties, not point assertions

The single hardest shift when you start fuzzing is that you no longer know the
expected output — the input is generated, so you cannot hard-code the answer. You
assert a *property* that must hold for every input. Four families cover almost
all production fuzzing:

- **Invariant** — the output always satisfies a predicate regardless of input.
  "The delay is always within `[0, max]`." "Tokens never exceed capacity."
- **Round-trip** — `Decode(Encode(x)) == x` for all `x`. This is the natural test
  for any codec, serializer, or framing layer; a mismatch means the pair is not a
  true inverse.
- **Differential** — a fast hand-written implementation must agree with a trusted
  reference oracle on both the accept-value and the reject decision. This is how
  you fuzz a hot-path parser you wrote to avoid allocations: `net/netip` is the
  oracle, your parser must never disagree.
- **No-panic / resource-bounded** — arbitrary bytes must never crash the process
  and never consume more than a stated budget. The body barely asserts anything
  explicit; a panic *is* the failure, and a counting reader pins the byte budget.

A point assertion checks one case; a property checks the infinite set the fuzzer
will explore. Write the property.

### Failure produces a minimized, committed regression asset

When an input fails, the engine does not hand you the megabyte of mutated garbage
that happened to trigger it. It *minimizes*: it shrinks the input to the smallest
one that still fails, then writes that reproducer to
`testdata/fuzz/FuzzXxx/<hash>` inside your package. That file is not a temporary
artifact — it is a version-controlled test asset. You commit it. From then on,
plain `go test` (no `-fuzz`) replays it as a deterministic sub-test, so the bug
you fixed stays fixed. The corpus file uses a small text format: a
`go test fuzz v1` header line, then one `type(value)` line per fuzz argument
(`string("…")`, `[]byte("…")`, `int(42)`). The regression exercise walks this
loop end to end: fuzz until crash, commit the minimized file, fix the bug, watch
plain `go test` replay the committed file green.

### The flags, and the CI split

`-fuzz=Regexp` selects the single target to fuzz — only one target is fuzzed per
`go test` invocation, so the regexp must match exactly one `FuzzXxx`. `-fuzztime`
bounds the run by wall-clock (`-fuzztime=30s`) or by executions
(`-fuzztime=1000000x`); without it, fuzzing runs until it finds a failure or you
kill it, which is why an un-bounded `-fuzz` in CI hangs the pipeline.
`-fuzzminimizetime` bounds the minimization phase. The operational split that
every team lands on: CI runs plain `go test` (fast, replays the committed seed
corpus, gates every PR) while actual mutation fuzzing runs as a separate,
time-boxed or continuous job that commits any new crashers back as seeds. The
generated corpus the engine accumulates while fuzzing lives in `$GOCACHE/fuzz`
and is *not* committed; only the minimized `testdata/fuzz` seeds are.

### Where fuzzing pays and where it does not reach

Coverage-guided mutation needs coverage instrumentation, which today means
`amd64`/`arm64`; on other platforms fuzzing degrades to running the seed corpus
only. Fuzzing shines on parsers, codecs, sanitizers, and any code at an
untrusted-input boundary — precisely the perimeter of a service. It does not
replace example-based tests: keep table tests for the known contract (the exact
error a truncated frame returns, the exact canonical form of a header) and fuzz
for the inputs you did not imagine. The two are complementary, and every exercise
here ships both.

## Common Mistakes

### An empty or always-true fuzz body

Wrong: `f.Fuzz(func(t *testing.T, s string) { _ = s })`. The engine dutifully
generates millions of inputs and asserts nothing, so the test can never fail. It
burns CPU and inflates the corpus while proving nothing.

Fix: assert a property. Even the weakest useful property — "this call does not
panic" — is a real test, because a panic fails it. Prefer a stronger invariant,
round-trip, or differential check when the code admits one.

### Mismatched `f.Add` type or arity

Wrong: `f.Fuzz(func(t *testing.T, s string, b byte) {…})` paired with
`f.Add("x")`. The seed supplies one argument; the fuzz function takes two.

Fix: every `f.Add` must mirror the fuzz argument list exactly —
`f.Add("x", byte(','))`. This is a build error, so it fails loudly, but it still
trips people who add an argument to the body and forget the seeds.

### Fuzzing forever in CI

Wrong: `go test -fuzz=FuzzX` in a pipeline. With no `-fuzztime` it never returns.

Fix: time-box every interactive and CI fuzz run (`-fuzztime=10s`), and keep
continuous fuzzing on a dedicated, separately-scheduled job. CI's per-PR gate is
plain `go test`, which replays the corpus in milliseconds.

### A non-deterministic or slow body

Wrong: reading `time.Now`, ranging a map, spawning goroutines, or calling the
network inside the fuzz body. The same input behaves differently on replay.

Fix: make the body a pure, fast function of its arguments. Decode any needed
randomness *from* the fuzzed bytes; never introduce your own.

### Trying to fuzz an unsupported argument type

Wrong: `f.Fuzz(func(t *testing.T, ops []Op) {…})` or a struct argument. The
engine rejects it — the allowed set is scalars, `string`, and `[]byte` only.

Fix: fuzz `[]byte` (or a scalar) and decode it inside the body into your richer
input: an operation stream via `binary.Uvarint`, a struct via a codec.

### Deleting or gitignoring `testdata/fuzz`

Wrong: treating the minimized crash files as scratch and adding
`testdata/fuzz/` to `.gitignore`. Those files are your regression suite; drop
them and a fixed bug can silently return.

Fix: commit every minimized seed under `testdata/fuzz`. They are source, not
cache. The engine's *generated* corpus in `$GOCACHE` is the part you do not
commit.

### Expecting more than one target per run

Wrong: assuming `-fuzz=Fuzz` fuzzes several matching targets, or writing two
`f.Fuzz` calls in one `FuzzXxx`.

Fix: exactly one `f.Fuzz` per fuzz function, and `-fuzz` must select exactly one
target per `go test` invocation. Fuzz them in separate runs.

### Comparing against a buggy or un-normalized oracle in differential fuzzing

Wrong: asserting your parser equals a reference without accounting for the
reference's own quirks — e.g. comparing against `netip` without first gating on
`Is4()`, so an IPv4-in-IPv6 form produces a false "divergence" that masks real
ones.

Fix: normalize both sides to the same representation before comparing, and gate
on the exact predicate your parser targets (a strict dotted-quad ⟺ `netip`
accepts *and* `Is4()`). A false positive that hides a true positive is worse than
no test.

Next: [01-fuzz-parseint-invariant.md](01-fuzz-parseint-invariant.md)
