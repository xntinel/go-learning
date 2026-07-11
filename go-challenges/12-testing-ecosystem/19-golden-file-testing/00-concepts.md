# Golden File Testing for Production Backends — Concepts

A golden (snapshot) test asserts that a serialized output equals a committed
reference artifact. Instead of writing dozens of field-by-field assertions
against an HTTP response body, a rendered template, a compiled SQL string, or a
JSON config document, you capture the output once, commit it as a fixture, and
have the test fail whenever the output drifts from that fixture. This is the
right tool precisely when the output is large and stable and the interesting
question is "did anything change?" rather than "is this one field correct?".
The senior skill is not writing a snapshot — that part is trivial — it is
managing the failure modes that make snapshot suites rot: non-determinism, an
update flag that launders bugs into the reference, byte-versus-semantic
comparison, goldens drifting out of sync with an evolving contract, and
orphaned fixtures nobody references anymore. This file is the conceptual
foundation for the nine independent exercises that follow.

## Concepts

### The golden file is source code

The single most important mental shift: a golden file is not test output, it is
a reviewed artifact under version control. It lives under `testdata/` — a
directory name the `go` tool treats specially, never compiling or vetting its
contents — it is committed alongside the `.go` files, and every change to it
must be scrutinized in code review exactly like a change to a function body. A
snapshot that no human ever read is worthless: it pins whatever the code
happened to emit the day someone ran the update flag, bugs included. The value
of the whole technique comes from the diff: when the golden changes, that change
appears in the pull request, and a reviewer decides whether it is an intended
contract change or a regression. If nobody looks, you have a test that can only
ever agree with the code, which tests nothing.

### The update-flag idiom

The canonical Go pattern is a package-level flag:

```text
var update = flag.Bool("update", false, "regenerate golden files in testdata/")
```

The `go test` binary registers custom flags on the standard `flag.CommandLine`
set and parses them before your tests run, so `go test -update ./...` works with
no manual `flag.Parse()` — calling `flag.Parse()` yourself in a test is a mistake
that can double-parse or fight the framework. When `*update` is set, each test
*writes* its produced bytes to the golden file with `os.WriteFile`; when it is
not set (the normal path, and always the CI path) each test *reads* the golden
with `os.ReadFile` and asserts equality. Some teams drive the same behavior from
an environment variable (`UPDATE=1`) instead of a flag; the flag is more
idiomatic because it composes with the rest of the `go test` flag set, but the
env-var form is a legitimate stepping stone and is where this lesson starts.

### The update flag is the biggest risk in the technique

The flag that makes goldens ergonomic is also the single largest way they fail.
Regenerating a golden and committing it without reading the resulting diff
blesses whatever the code emitted — a real regression becomes the new expected
output, silently. The discipline is non-negotiable: run `-update` deliberately,
only when you have intentionally changed the output, then read the `git diff`
line by line before committing. And `-update` must never run in CI: a pipeline
that regenerates and then compares against what it just wrote can never catch a
regression. A CI guard that fails the test when an update is requested under an
automation environment variable turns that rule into code.

### Determinism is the whole game

A golden test can only work if the output is a pure function of the input. Any
volatile field poisons it: a `time.Now()` timestamp, a UUID or random request
id, a measured duration, floating-point formatting, goroutine-dependent
ordering, or map iteration order in a non-sorting encoder. Each of these makes
the snapshot flake on the next run, and a suite that flakes trains the team to
ignore red, which is worse than having no test. There are two cures, in order of
preference. First, remove the non-determinism at the source: inject a fixed
clock and a deterministic id generator so the code emits stable output in tests.
Second, when you cannot control the source, normalize the volatile fields before
comparison — redact matched timestamp and UUID patterns to fixed placeholders,
or, on decoded structures, exclude them with a comparison option. Normalization
is a scalpel, not a hammer: scrub only genuinely non-deterministic fields and
keep everything else exact, or the golden stops pinning anything meaningful and
real regressions slip through.

### Byte-exact versus semantic comparison

Comparing the raw bytes and comparing the decoded structure are two different
contracts, and choosing between them is a deliberate design decision per
artifact. Byte comparison catches every formatting, whitespace, and line-ending
change; it is the correct contract for wire formats and for files consumed by
other tools that care about exact bytes. Semantic comparison decodes both sides
into the same type and diffs the values, ignoring insignificant formatting and
yielding a field-level diff; it is the better contract for API DTOs where you
care about the shape and values, not the indentation. A response body that is
semantically stable but formatting-volatile (map order in a non-sorting encoder,
float rendering) will flake under byte comparison — either canonicalize the
format or switch to semantic comparison. Pick per artifact; do not apply one
rule everywhere.

### encoding/json gives you two determinism guarantees

JSON is a friendly golden format because the standard library pins two things
for you. Map keys are always marshaled in sorted order, so a `map[string]T`
serializes deterministically. Struct fields follow declaration order, so the
field layout of the golden is the struct's contract — reorder the fields and the
golden changes, which is exactly the signal you want. `json.MarshalIndent` adds
stable indentation on top. What the standard library does *not* decide for you,
and you must decide explicitly, are two policy points: the trailing-newline
policy (marshal emits no trailing newline; most tools and editors expect a
single trailing LF, so append exactly one and apply the same rule in the
comparer) and HTML escaping (`Marshal` escapes `<`, `>`, and `&` into `<`
and friends, which makes goldens unreadable — use an `Encoder` with
`SetEscapeHTML(false)` when the golden should show those characters literally).

### go-cmp is the standard tool for reviewable diffs

`cmp.Diff(want, got)` from `github.com/google/go-cmp/cmp` returns an empty string
when the values are equal and a compact `-want +got` diff when they are not. On
structured data this is dramatically more readable than dumping two giant byte
blobs into `t.Fatalf`, because it names the exact field that changed. The
`cmpopts` subpackage supplies the surgical tools for volatile data:
`IgnoreFields` drops named struct fields from the comparison,
`IgnoreMapEntries` drops selected map keys, `SortSlices` canonicalizes slices
that represent sets, and `EquateApproxTime` tolerates clock skew. These let you
exclude non-determinism from a *decoded* value instead of scrubbing raw bytes,
which is often cleaner because it operates on typed fields rather than regular
expressions over text.

### Golden tests express contracts, not implementation

A response-body golden pins the public API shape; a compiled-SQL golden pins the
exact query sent over the wire; a rendered-template golden pins what the user
receives. When one of these goldens changes, that *is* the signal that a contract
changed — which is the entire point. The change becomes visible and reviewable
at exactly the boundary that matters. This is also why you golden the public
artifact, not an internal struct: you want the test to break when the thing your
consumers depend on changes, and to stay quiet when you refactor internals that
produce the same output.

### Table-driven, per-case goldens scale the pattern

One golden file per subtest is how the technique scales to many fixtures. A
table of named cases, each mapped to its own `testdata/<case>.golden` and run
under `t.Run`, gives you independently updatable and independently reviewable
snapshots, and isolates failures so one broken case does not mask another. The
case name must map to a filesystem-safe file name (replace slashes and spaces).
The alternative — one shared golden for many cases — is a trap: a change in case
A rewrites the shared fixture and hides case B entirely.

### Suites rot without maintenance guards

Left alone, a snapshot corpus accumulates orphaned goldens: files that no case
references anymore because the case was renamed or deleted. They are dead weight
that misleads reviewers into thinking a fixture is live. A test that walks
`testdata/` with `filepath.WalkDir`, collects every `.golden` file, and diffs
that set against the set of names the case table declares — failing on any extra
— keeps the corpus honest. Paired with the CI update guard, these two tests make
the suite self-policing: no un-reviewed updates, no dead fixtures.

### Line endings are the most common confusing failure

Trailing newlines, CRLF versus LF, and indentation cause more baffling
byte-golden failures than anything else. The writer emits no trailing newline
but an editor adds one on save; a Windows checkout converts LF to CRLF via
`.gitattributes`; two encoders indent differently. Decide an explicit policy —
a single trailing LF, no CRLF — and apply it identically in both the code that
writes the golden and the code that compares against it. When a byte golden
fails and the visible content looks identical, suspect the invisible bytes
first.

## Common Mistakes

### Committing volatile output

Wrong: a golden that embeds `time.Now()`, a UUID, a measured duration, or a
random seed. It fails on the very next run.

Fix: inject a fixed clock and id source so the output is deterministic, or
normalize the volatile field to a fixed placeholder before comparing.

### Treating the update flag as a rubber stamp

Wrong: running `-update`, committing the regenerated goldens, and never reading
the diff. A real regression is now the accepted expectation.

Fix: regenerate deliberately, then audit the `git diff` line by line before
committing. The diff is the test.

### Letting the update flag run in CI

Wrong: a pipeline that can regenerate goldens and pass. It can never fail.

Fix: guard with an environment check that fails the test if an update is
requested while a CI variable is set.

### Byte-comparing formatting-volatile output

Wrong: byte-comparing output whose map order or float formatting varies run to
run, producing flaky red.

Fix: canonicalize the format (sorted keys, `MarshalIndent`) or decode and
compare with `cmp.Diff`.

### Trailing-newline and CRLF mismatches

Wrong: the writer emits no trailing newline but the reader expects one, or a
Windows checkout rewrites LF to CRLF, so a byte compare fails on invisible
bytes.

Fix: pick a single explicit line-ending and trailing-newline policy and apply it
in both the writer and the comparer.

### Putting golden files outside testdata/

Wrong: fixtures in a package directory, where the `go` tool tries to build or
vet them, or where reviewers overlook them.

Fix: keep every fixture under `testdata/` and reference it by `filepath.Join`.

### Dumping raw bytes on mismatch

Wrong: `t.Fatalf` with a giant byte blob, so the failure is unreadable and the
reviewer cannot see what changed.

Fix: print the golden file path, and for structured data a `cmp.Diff`
`-want +got` block.

### Over-normalizing

Wrong: scrubbing so many fields that the golden no longer pins anything, so real
regressions pass.

Fix: normalize only the genuinely non-deterministic fields; keep the rest exact.

### One shared golden for many table cases

Wrong: every subtest writes the same file, so case A's change hides case B.

Fix: one golden file per subtest, named from the case.

### Forgetting to commit the golden

Wrong: gitignoring `testdata/` or never committing the regenerated fixture, so
the suite passes locally after `-update` and fails on a fresh clone or in CI
where the reference is missing.

Fix: commit the golden with the code change that produced it.

### Only snapshotting the body of an HTTP response

Wrong: golden the JSON body but never assert the status code or headers, missing
a 200-to-500 or a `Content-Type` regression.

Fix: assert status and the headers you care about explicitly, alongside the body
golden.

### Calling flag.Parse() in a test

Wrong: manually parsing flags in a test, fighting the `go test` binary's own
parsing of the package-level `-update` flag.

Fix: declare the flag at package scope and let the testing binary parse it.

Next: [01-inline-golden-string-update-env.md](01-inline-golden-string-update-env.md)
