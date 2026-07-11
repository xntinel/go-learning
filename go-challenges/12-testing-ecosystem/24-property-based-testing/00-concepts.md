# Property-Based Testing for Backend Invariants — Concepts

A table-driven test is a list of point assertions: for *these* inputs, expect
*those* outputs. It is exactly as good as the rows a human thought to write, and
the bugs that reach production are almost never in the rows a human thought to
write. They live in the input you did not enumerate: the empty slice, the string
with a `&` in it, the amount at `math.MaxInt64`, the two operations whose
*interleaving* violates an invariant no single call could. Property-based testing
(PBT) is the tool a senior engineer reaches for when the input space is too large
to enumerate and the interesting failures are at the edges and in the
combinations. Instead of asserting one output, you assert a *property* — a
statement true of every valid input — and let a generator manufacture thousands
of inputs, including the ones you would never have typed, trying to break it.

This file is the model shared by all nine exercises that follow. Read it once and
you have the vocabulary — properties, the property catalog, generators, shrinking,
seeds — to reason through each exercise, every one of which is a real backend
artifact (a money type, a codec, a canonicalizer, a parser, a query pipeline, an
LRU cache, a config validator, a JSON canonicalizer, an ID codec) rather than an
algebra toy.

## The mental shift: a property is a universally-quantified statement

A property is a claim of the form "for all valid `x`, `P(x)` holds." It is not an
assertion about one case; it is an assertion about the whole (usually infinite)
set of valid inputs. The test's job splits cleanly in two: a *generator* produces
an `x`, and a *checker* evaluates `P(x)`. When `P(x)` is false, PBT has found a
counterexample — a concrete input that disproves the universal claim.

The reason this matters for backend work is that most correctness bugs are
violations of some invariant that was true for the cases in the test file and
false for a case that was not. "Decode of Encode gives back the original" is an
invariant of every serialization boundary. "Canonicalizing twice equals
canonicalizing once" is an invariant of every normalizer. "The fast parser agrees
with the standard library" is an invariant of every hand-rolled hot path. None of
these is naturally expressed as a fixed input/output pair, and all of them are
where the production incidents come from.

## The property catalog

You almost never invent a property from nothing. Backend code falls into a small
number of shapes, and each shape has a property pattern that fits it. Learning to
recognize the shape is most of the skill.

Round-trip. For any codec — a serializer, an encoder, a wire format — the
defining property is `Decode(Encode(x)) == x`. It is the single highest-value
property in backend testing because serialization boundaries are where data
silently corrupts. Note the direction: `Decode(Encode(x)) == x` (start from a
typed value) is stronger and easier than `Encode(Decode(s)) == s` (start from
bytes), because encoding is usually canonical while many byte strings decode to
the same value, so the byte-first direction fails on legal non-canonical input.
Exercises 2 and 9 build round-trip properties.

Idempotence. For any canonicalizer or normalizer — path cleaning, header
canonicalization, Unicode normalization, JSON canonicalization — the defining
property is `f(f(x)) == f(x)`. A canonical form must be a fixed point: applying
the transform to an already-canonical value must not change it. If it does, the
transform is not producing a canonical form and two "equal" values can have
different canonical representations. Exercises 3 and 8 build idempotence
properties.

Oracle / differential. When you have a trusted reference implementation and a
faster or cheaper one, the property is that the two agree — and agree on *both*
the value *and* the accept/reject decision, for every input. This is how you test
an allocation-free hot-path parser against `net.SplitHostPort`, or a new query
planner against the old one. The trap is comparing un-normalized outputs or
failing to gate on exactly the subset the fast path claims to handle. Exercise 4
builds a differential property.

Metamorphic. When there is no oracle for the whole function, you can still assert
a known *relation* between the outputs of *related* inputs. "Sorting an
already-sorted list changes nothing." "Filtering then sorting equals sorting then
filtering, for a stable sort." "Concatenating the pages reconstructs the full
result." You never need to know the right answer for any single input; you only
need to know how the answer must change when the input changes in a known way.
Exercise 5 builds metamorphic properties over a query pipeline.

Invariant. The output always satisfies a predicate, regardless of input: the
result of a paginator is always sorted and never longer than the page size; the
output of an encoder always uses only the alphabet and never exceeds the maximum
length. Invariants pair naturally with the other patterns as a cheap extra check.

Stateful / model-based. For an object whose correctness is a property of the
*whole history* of operations — a cache, a rate limiter, a connection pool, a
transaction log — you cannot test any single call in isolation. You run a random
sequence of operations against both the real object and a simple, obviously-correct
reference *model*, and assert they agree after every step. The bug is almost never
in one operation; it is in the interleaving. Exercise 6 builds a model-based test
of an LRU cache.

## The three pillars: generation, shrinking, reproducible seeds

Three capabilities are what make PBT worth its cost, and any tool you pick is
judged on how well it delivers them.

Generation covers the input space. A generator is a recipe for producing values
of a type, and its quality is the quality of your test. A generator that only ever
produces small positive integers will never find the bug at `MinInt64`. Good
generators deliberately over-sample the edges: zero, negatives, the maximum, the
empty collection, the string full of reserved characters. The edges are the whole
point; a generator that avoids them is testing the space where there are no bugs.

Shrinking (minimization) is what turns PBT from a curiosity into a debugging tool.
When the generator finds a 4 KB input that fails, shrinking automatically searches
for the *smallest* input that still fails — often a two-element slice or a
one-character string — so you debug the essence of the bug instead of the noise
around it. This is the single biggest practical difference between the two tools
below: one shrinks, one does not.

Reproducible seeds are what make PBT usable in a team. Every run is driven by a
pseudo-random seed. When a run fails, the tool prints the seed; pasting that seed
back reruns the *exact* failing sequence. That is what you attach to the CI ticket,
and that is what becomes the regression test. A property test whose failures cannot
be replayed is a flake generator, not a test.

## The three tools, and when each fits

`testing/quick` (standard library). Reflection-based generation, zero
dependencies. `quick.Check(f, cfg)` calls `f` — which returns `bool` — with
generated arguments; `quick.CheckEqual(f, g, cfg)` asserts two functions produce
equal output for the same generated input. Custom types implement the
`quick.Generator` interface (a single `Generate(rand *rand.Rand, size int)
reflect.Value` method; all struct fields must be exported to be generated). Its
decisive limitation: it has *no shrinking*. When it fails it hands you the raw
failing input, which can be enormous and nearly useless for debugging. Reach for
it when you want a property with no dependency and the inputs are simple enough
that a minimal counterexample does not matter.

`pgregory.net/rapid`. A third-party library built around the three pillars.
Values come from typed generators drawn inside the property: `gen.Draw(t,
"label")`. It ships `Int`/`IntRange`, `String`/`StringOf`/`StringMatching`,
`SliceOf`/`SliceOfN`, `MapOf`, `Bool`, `Uint64`/`Uint64Range`, and combinators
`Custom`, `Map`, `OneOf`, `Just`, `SampledFrom`, `Filter`, `Ptr`, `Deferred`.
It has integrated automatic shrinking, stateful/model-based testing via
`t.Repeat` and `StateMachine`, and a bridge to Go's native fuzzer. It is the
default choice for serious backend PBT and the tool most of these exercises use.

`go test -fuzz` (the native fuzzer). Coverage-guided *byte* mutation: it mutates
raw `[]byte`/`string` inputs, watching code coverage to steer toward new paths,
and minimizes failing inputs at the byte level. It excels at untrusted-input
parsers and anything that must not panic on hostile bytes. It is complementary to
PBT, not a competitor: PBT asserts rich semantic properties over *typed* generated
values; the fuzzer hammers *byte* inputs looking for crashes and coverage. Exercise
8 bridges the two with `rapid.MakeFuzz`, letting the coverage-guided engine drive
rapid's typed generators.

## The failure lifecycle

When a property fails under rapid the sequence is: generate an input, evaluate the
property, find a violation, *shrink* the input to a local minimum that still
violates, and print the reproducing seed. You read the minimized counterexample
(often small enough to reason about by hand), fix the bug, then pin the seed into
a regression test so the same bug cannot silently return. Under `testing/quick`
the shrink step is absent — you get the raw failing input and pin nothing unless
you capture it yourself.

## CI economics

PBT and fuzzing cost CPU, and an unbounded run never returns, so they must be
budgeted. The standard split: a bounded per-PR gate (`go test` with a modest
`-rapid.checks`, plus replaying any committed seed corpus) that finishes in
seconds; a nightly job with a much larger `-rapid.checks` (and `-rapid.steps` for
state machines) that explores far more; a separate time-boxed fuzz job
(`go test -fuzz -fuzztime=...`) that never blocks a merge; and, for every
counterexample ever found, a committed pinned-seed (or table-row) regression test.
The flags are `-rapid.checks=N` (number of generated cases), `-rapid.seed=N`
(replay an exact run), and `-rapid.steps=N` (average state-machine length).

## Why arithmetic-critical code demands properties

Money, decimals, counters, and offsets are exactly the code where a handful of
example rows is most dangerous, because the violating values — overflow at
`MaxInt64`, the asymmetry of `MinInt64`, zero, the boundary between saturate and
wrap — are precisely the values a hand-written table omits. Testing such code for
algebraic invariants (commutativity, associativity, identity, inverse,
distributivity) and for the deliberate *non*-properties (subtraction is not
commutative) catches the boundary bug that no example row was going to. And it
forces you to make overflow behavior explicit: a property must not silently rely
on two's-complement wraparound, so writing the property makes you decide whether
the type saturates or errors, which is a decision production code needs to make
anyway.

## Common Mistakes

### Asserting an implementation detail instead of an invariant

Wrong: a property that pins how the code works — "the output is exactly this
string", "the function acquires a mutex". The property breaks the moment the
implementation legitimately changes, and it never tested correctness in the first
place. Fix: assert what must hold for *any* correct implementation — the value
round-trips, the output is sorted, twice equals once.

### Reimplementing the function under test inside the property

Wrong: computing the "expected" answer with a copy of the same logic the code
uses, so the two agree by construction and the property can never fail. Fix:
derive the property from an *independent* source — a metamorphic relation, a
separately-trusted oracle (the standard library, the old implementation), or a
specification predicate written from the requirements, never from the code.

### Expecting a minimal counterexample from testing/quick

Wrong: using `testing/quick` and being surprised that the failing input is a
1000-element slice you cannot debug. `quick` has no shrinking. Fix: reach for
rapid (integrated shrinking) when a minimal counterexample matters, or accept that
with `quick` you must minimize by hand.

### Weak or trivial generators

Wrong: a hand-rolled `math/rand` loop with a fixed seed and a narrow range
(`Intn(1000)`) that never reaches the boundary values where the bug lives. Fix:
generators must cover zero, negatives, maximum and minimum, empty collections, and
adversarial characters. The edges are the point; a generator that avoids them
tests only the region with no bugs.

### Comparing against an un-normalized or buggy oracle

Wrong: in a differential test, comparing a fast parser against a reference without
normalizing both outputs, or without gating on exactly the subset the fast path
claims to handle — producing false divergences that bury the real ones. Fix:
normalize both sides and gate the comparison on the precise predicate the fast
path targets.

### Non-deterministic property bodies

Wrong: reading `time.Now`, ranging over a map, launching goroutines, or touching
global state inside the property. The engine cannot reproduce or shrink what it
cannot replay, so every failure becomes an unpinnable flake. Fix: keep the
property a pure function of its generated input.

### Over-aggressive Filter that starves the generator

Wrong: generating mostly-invalid values and using `Filter` to discard almost all
of them, so the generator spends its budget rejecting and eventually gives up.
Fix: build validity into the generator (`Custom` composing already-valid fields);
filter only for the rare last-mile constraint.

### Running an unbounded PBT or fuzz job in a per-PR gate

Wrong: putting `go test -fuzz` (which never returns) or a huge `-rapid.checks` on
the merge-blocking path. Fix: the per-PR gate is bounded `rapid.checks` plus the
committed seed corpus; heavy exploration is a scheduled nightly or time-boxed
fuzz job.

### Not pinning a discovered counterexample

Wrong: fixing the bug a property found and moving on. Without a committed seed (or
a table row capturing the minimized input), the same bug can regress silently and
the property might not re-find it for months. Fix: every counterexample becomes a
pinned-seed or table-row regression test.

Next: [01-money-algebraic-invariants.md](01-money-algebraic-invariants.md)
