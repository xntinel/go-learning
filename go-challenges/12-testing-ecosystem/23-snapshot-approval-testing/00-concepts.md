# Snapshot and Approval Testing for Serialized Output and API Contracts

Some outputs are miserable to assert field by field. A JSON API response body, an
error envelope, a rendered config file, a block of generated migration SQL, a
formatted log line — writing thirty `if got.Field != want` lines for one of these
is tedious, and worse, it silently stops testing the parts you forgot to name.
Snapshot testing (also called golden-file or approval testing) takes the whole
serialized byte stream and compares it against a stored expected value. The
headline benefit is not "less typing." It is that any change to an
externally-observable output becomes a reviewable diff in a pull request. A field
that quietly changed its JSON tag, an error code that flipped from 400 to 500, a
timestamp format that shifted — all of these show up as a red test and a concrete
before/after diff that a human reads before it ships.

That last clause is the whole discipline. A snapshot is only as good as the human
who reads the diff when it changes. The senior skill set here is four things:
making outputs deterministic before you can snapshot them at all; building the
update workflow so re-approving is a deliberate, reviewed act and never a reflex;
knowing where the boundary sits between what belongs in a snapshot and what
belongs in an explicit assertion; and treating an approved golden file as a
contract artifact. This file is the conceptual foundation; read it once and each
of the ten independent exercises that follow will make sense on its own.

## What a snapshot actually is

A snapshot is a stored expected output that a test compares against byte for byte.
It can live in three places, and the choice matters. An *inline* snapshot is a
literal held in the test file itself — a raw string constant the test compares
against. A *golden file* lives on disk under `testdata/` and is read at test time.
An *approved file* is the same idea with a human-in-the-loop promotion step layered
on top. Inline snapshots suit small, self-documenting outputs where seeing the
expectation next to the code is worth more than the noise it adds to the test file.
Golden files suit large outputs, many-case tables, and anything you want to
regenerate with a flag. Choosing between them is a readability-versus-scale
trade-off, not a matter of taste.

The comparison itself is byte equality. That is a feature: it catches a stray
space, a reordered key, a changed newline. It is also the source of every failure
mode in this lesson, because byte equality means every non-deterministic byte in
your output is a flake waiting to happen.

## Determinism is a precondition, not an afterthought

You cannot snapshot output that changes between runs. Timestamps, UUIDs and other
random identifiers, elapsed durations, hostnames, pointer addresses, and Go's
randomized map iteration order all produce a different byte stream each time, and
a byte-equality test over them fails on the second run — or worse, passes locally
and fails in CI on a different host. The naive reaction is to give up on snapshots
for "realistic" payloads, which throws away exactly the payloads snapshots are best
at. The correct move is to normalize the volatile parts before comparing.

There are two normalization strategies and they have different costs. *Redaction
by field* marshals the value into a `map[string]any` (or works with a typed
struct), overwrites the volatile keys with a stable placeholder, and re-serializes.
It is precise: it only touches the fields you name, so it never accidentally
rewrites a legitimate value that happens to look like a UUID. *Regex on bytes*
runs a pattern like an RFC3339 timestamp matcher over the serialized output and
replaces every match with `<TIMESTAMP>`. It is quick and works on output whose
shape you do not control, but it is fragile: a pattern that is too greedy redacts
real data, and a pattern that is too narrow misses a variant. Prefer field-level
redaction when you own the type; reach for regex when you are snapshotting an
opaque blob.

Map ordering deserves its own note because it is the trap that looks stable and
is not. `fmt.Sprintf("%v", someMap)` renders map keys in Go's randomized
iteration order, so a snapshot of it flakes. `encoding/json.Marshal`, by contrast,
*sorts map keys*, so JSON produced from a `map[string]any` is deterministic. When
your output derives from a map or from a struct whose serialization you do not
fully control, round-trip it through `json.Marshal` (or re-indent it with
`json.Indent`) to get a canonical form before snapshotting.

## The update workflow, done correctly

When the code legitimately changes, the snapshot must change with it, and you need
a mechanism to regenerate it. The Go-idiomatic mechanism is a package-level flag:

```go
var update = flag.Bool("update", false, "regenerate golden files")
```

The `go test` binary parses its own command-line flags before your tests run, so
`go test -update` sets this with no `flag.Parse()` call of your own. This is
preferred over an `UPDATE=1` environment variable because a flag is typed,
discoverable via `go test -args -h`, and lives in the same namespace as the other
test flags. Under `*update` the test writes the current output to the golden file;
otherwise it reads and compares.

Golden files belong under a directory named `testdata/`. The `go` tool ignores any
directory named `testdata`, so goldens are never compiled, vetted, or mistaken for
a package — they are treated purely as fixtures. Write them with `os.WriteFile`
after an `os.MkdirAll` of the `testdata` subdirectory, or the first `-update` run
fails with a no-such-file error.

The danger in the whole workflow is the reflex. The first time you see a red
snapshot test, the fastest way to green is to run `-update` and move on. If the
change was a real regression — a serialization bug, a dropped field — you have just
laundered it into an approved golden and shipped it green. The rule is: never run
`-update` without reading the diff it produces. The diff *is* the change
description. An unread re-approval is indistinguishable from shipping a bug.

The received/approved pattern (from the ApprovalTests family of tools) hardens this
into a workflow. On a mismatch, the test writes the actual output to a
`<name>.received` file and fails with instructions; a developer inspects the
received file and, if the change is intended, promotes it to `<name>.approved`.
Only the approved file is committed; the received file is git-ignored working
state. This makes approval a distinct, deliberate action rather than a side effect
of a flag, and it directly resists the reflexive re-approval failure mode.

## Where the boundary sits

Snapshot testing shines on stable serialized contracts: HTTP JSON response bodies,
error envelopes, rendered templates and config, generated SQL. These are outputs a
client or an operator depends on, they are small enough to read in a diff, and any
change to them is a change someone should notice. Approval-testing an HTTP
handler's response body — drive it with `httptest`, capture body plus status plus
content-type, normalize the volatile fields, pin the result — is the single
highest-value use of the technique, because it turns accidental API contract drift
into a failing test before it reaches a client.

It is a poor fit in three cases. Volatile output that you cannot fully normalize
will flake no matter what. Very large blobs — a multi-kilobyte response captured
whole — produce diffs so noisy that reviewers stop reading them and start
rubber-stamping, which defeats the entire purpose. And logic where an explicit
assertion communicates intent better: if the thing you care about is "the total is
the sum of the line items," a snapshot of the rendered total tells a future reader
nothing about *why* that number is right, whereas `if got.Total != want` does.
Scope every snapshot to the contract you actually care about; an over-broad
snapshot is worse than none because it trains reviewers to ignore it.

## A snapshot proves no-change, not correctness

This is the subtlest point and the one that most often gets a team into trouble. A
passing snapshot test tells you the output is the same as the last time someone
approved it. It does *not* tell you the output is correct. Snapshot tests are
characterization tests: they capture current behavior and guard against drift. When
you first write one, you approve whatever the code currently emits — including any
bug that is already there. Correctness still requires a human to look at the golden
and judge that the captured value is actually right. The snapshot then guards that
judgment against future accidental change. Treat a green snapshot as "nothing
drifted," never as "this is verified correct," and keep an explicit assertion for
the properties whose correctness you actually want a test to state.

## Common Mistakes

### Snapshotting volatile fields without normalizing

Wrong: capture a response body that contains `created_at`, a request ID, and a
latency, and compare it byte for byte. The test passes once and fails on every run
after, so someone disables it or blindly re-approves it.

Fix: normalize first. Redact timestamps, UUIDs, and durations to stable
placeholders (`<TIMESTAMP>`, `<UUID>`) — by field when you own the type, by regex
when you do not — and snapshot the normalized bytes. Run the test twice in the same
build to prove it no longer flakes.

### Reflexively running -update to make red go green

Wrong: a snapshot test fails, so you run `go test -update` and commit. If the
change was a regression you have just approved the bug.

Fix: read the diff the failure prints before regenerating. The received/approved
pattern makes this explicit: on mismatch, write `<name>.received`, fail with
instructions, and only promote to `<name>.approved` after a human inspects it.

### Storing goldens outside testdata/

Wrong: put golden files in a `fixtures/` directory. The `go` tool tries to treat
them as part of a package or trips over them during a build.

Fix: put every golden under `testdata/`. The tool ignores that directory name
specifically, so fixtures never get compiled.

### Committing the received (actual-output) file

Wrong: commit `<name>.received` alongside `<name>.approved`. The working artifact
leaks into version control and obscures which file is the real contract.

Fix: git-ignore `*.received`. Only the approved/golden file is the committed
contract.

### One giant snapshot of an entire response

Wrong: snapshot a whole multi-kilobyte response body. Any unrelated field change
produces a huge diff that reviewers stop reading.

Fix: scope the snapshot to the contract you care about — the fields that are
actually the API surface — so the diff stays small and every change in it is
meaningful.

### Assuming map serialization is stable without canonicalizing

Wrong: build a snapshot from `fmt.Sprintf("%v", m)` of a `map[string]any` and
trust it. Map iteration order is randomized, so the golden flakes.

Fix: serialize the map with `encoding/json.Marshal`, which sorts keys, or re-indent
through `json.Indent`, before snapshotting.

### Forgetting MkdirAll before the first -update write

Wrong: `os.WriteFile("testdata/x.golden", ...)` on a fresh checkout where
`testdata/` does not yet exist. The first `-update` run fails with no such file or
directory.

Fix: `os.MkdirAll(filepath.Dir(path), 0o755)` before `os.WriteFile`, and pick a
file mode (`0o644`) that matches the repo rather than fighting it.

### Treating a passing snapshot as proof of correctness

Wrong: read a green snapshot suite as "the output is right." It only means the
output is unchanged since the last approval, bug included.

Fix: judge correctness by reading the golden when you first approve it, keep an
explicit assertion for properties you actually want stated, and treat the snapshot
as a drift guard.

### Failure messages that do not name the golden or the fix

Wrong: fail a snapshot mismatch with a bare `not equal`. The developer has to
reverse-engineer which file to update and how.

Fix: print the golden path and the exact `go test -run ... -update` command in the
failure message so acting on it is one copy-paste.

Next: [01-implement-json-formatter.md](01-implement-json-formatter.md)
