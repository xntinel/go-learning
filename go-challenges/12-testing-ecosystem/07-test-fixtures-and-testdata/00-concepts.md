# Test Fixtures and testdata: Managing Test Inputs and Golden Outputs Like Production Assets

The fastest way to spot a junior test suite is a wall of string literals: a fifty-line expected JSON body spliced together with `"\n" +`, a hand-typed HTML document inside an `if got != "..."`, a domain object rebuilt from scratch in every one of forty tests. It compiles, it passes, and it rots. Every intentional output change forces a manual edit of a brittle literal; every schema change breaks dozens of hand-built fixtures; and the moment an output contains a timestamp, the whole thing flakes. A senior backend engineer treats test inputs and expected outputs the way they treat migrations and config: as versioned, reviewed, deterministic assets that live in files, are discovered rather than enumerated, and are kept honest by explicit tooling. This chapter is about where a fixture lives, how it stays deterministic, how a case gets added without editing Go, and how a golden is safely regenerated.

## The testdata convention

`testdata` is a reserved directory name in the go command. `go build`, `go vet`, and package listing all ignore any directory named `testdata` — the toolchain will not compile, lint, or enumerate anything inside it. That is precisely why it is the canonical home for realistic inputs, golden outputs, and seed data: you can park a fifty-kilobyte webhook payload, a `.golden` reference output, or a JSON seed set right next to the package that uses them, and the toolchain leaves them entirely alone. The corollary bites the unwary: a `.go` file dropped in `testdata/` is invisible to the compiler, so helper code hidden there is unreachable. Helper code belongs in a normal `_test.go` or internal file; `testdata/` is for data.

The second guarantee that makes fixture files work is the working directory. A package's tests always run with the current directory set to that package's source directory, regardless of where `go test ./...` was invoked from. So a relative path like `filepath.Join("testdata", name)` resolves against the package directory every single time — no absolute paths, no runtime path discovery, no `runtime.Caller` tricks. This is a test-time guarantee only: a compiled binary running in production has no such promise, which is why demo `main` programs in this chapter use inline data rather than reading fixture files. Build fixture paths with `path/filepath.Join`, never string concatenation with a hardcoded `/`, so the path is correct on every platform and rooted at `testdata`.

## Fixtures, goldens, and the value of externalizing

A fixture is any externalized test input or expected output. A golden file is specifically the expected output that a function is asserted against — the "known good" reference. The two ideas are separable: a fixture might be an input payload with no golden at all (you assert a parsed field), and a golden might be generated with no input file (you assert a serializer's output). Externalizing large fixtures buys two things. It keeps tests readable — the assertion is `compare(got, golden)`, and the bulky data is elsewhere. And it lets non-code changes stay non-code: adding a new case becomes adding a file or two, which a reviewer reads as data, rather than editing a Go table they must audit line by line.

The cheapest golden is an inline `const` next to the test, and that is the right form when the expected output is a line or two — the contract is visible in one place. It stops scaling the instant the output grows past what a reader can hold in their head, at which point the golden moves into a `testdata/*.golden` file. The mechanics are identical in both forms: produce the output, compare against a reference, fail with both sides printed. Only the storage location changes.

## The -update flag and the discipline it demands

Hand-editing a multi-line golden on every intentional change is tedious and error-prone, so the standard ergonomic is a package-level `flag.Bool("update", ...)`. The same test compares against the committed golden by default and rewrites it when you pass `go test -update`. That read/write asymmetry is the whole trick: default runs assert, `-update` runs regenerate.

It carries a discipline that is easy to skip and dangerous to skip. A regenerated golden is a snapshot of whatever the code produced at that moment — bug included. Running `-update` and committing without reading the diff blesses the current output as correct by fiat; a golden you cannot explain is a bug you just blessed. The regenerated diff must be reviewed exactly like a code diff. The flag exists to save you from retyping fifty lines, not from thinking about them. Two mechanical points: write the golden with an explicit permission (`os.WriteFile(path, got, 0o644)`) so it is committable, and make the mismatch message actionable — print both sides and name the exact `-update` command — so a legitimate change is a ten-second regenerate rather than a hand-edit.

## File-backed versus embedded fixtures

Since Go 1.16 there are two ways to get fixture bytes into a test. The file-backed primitives — `os.ReadFile`, `os.WriteFile`, `os.ReadDir` — read from disk at test time, relying on the cwd-is-package-dir guarantee. The compiled-in alternative is `//go:embed`, which bakes the files into the test binary at build time and reads them through an `embed.FS`. Embedding makes a test hermetic and relocatable: `go test -c` produces a binary you can run anywhere, and the test cannot fail on a missing or misplaced loose file because there is no filesystem access at all.

`//go:embed` has rules worth memorizing. The directive attaches to a package-level variable declaration and requires the `embed` package imported. It silently excludes any file or directory whose name begins with `.` or `_` unless the pattern uses the `all:` prefix — so a `.env` fixture or a `_scratch` subdirectory is dropped without warning otherwise. A directory literally named `testdata` embeds normally; the exclusion is only about the leading `.`/`_` character. And `embed.FS` implements `io/fs.FS`, so `ReadFile`, `ReadDir`, and `Open` work uniformly — the identical iteration code runs against a real directory wrapped as an `fs.FS`, which is what makes embedded fixtures a drop-in for on-disk ones. Because `embed.FS` uses forward-slash paths on every platform, join embedded paths with `path.Join`, not `filepath.Join`.

## Discovery: adding a case without editing code

The point of externalizing fixtures is fully realized when the test *discovers* its cases instead of enumerating them. Two idioms cover it. `filepath.Glob("testdata/*.input")` finds flat input/golden pairs; a subtest runs per match, and the golden path is derived from the input path so the pairing cannot drift. `os.ReadDir("testdata/cases")` enumerates per-case *directories* when a case needs several related files — an input, an expected output, and maybe a config — grouping them explicitly rather than through a fragile shared-prefix naming scheme. Either way, adding coverage becomes dropping files, never touching Go.

Discovery has one signature failure mode: a glob that matches nothing produces an empty loop, and an empty loop asserts nothing, so the suite goes green while testing exactly none of your cases. Always fail explicitly when the discovered set is empty. For directory-based discovery, skip non-directory entries (`fs.DirEntry.IsDir`), treat a missing *required* file as a hard failure, and distinguish a missing *optional* file (`errors.Is(err, fs.ErrNotExist)`, use the default) from a genuine read error (fatal) — conflating the two hides real problems.

## Determinism is the golden-file tax

A golden comparison is byte equality against a committed reference, which only works if the output is deterministic. Serialized events, request logs, and audit records rarely are: they carry UUIDs, wall-clock timestamps, durations, and — for anything backed by a map — non-deterministic iteration order. Compared raw to a golden, they fail every run on fields that were correct, and a suite that is red for noise trains the team to ignore red, which is worse than no test.

There are two honest fixes. Make the serializer deterministic where you can — sort before emitting so map order stops mattering. Where you cannot (a UUID is genuinely fresh each time), normalize: run both the produced output and the golden through a pass that replaces each volatile field with a stable token (`<uuid>`, `<ts>`, `<dur>`) before comparing. The knife-edge is over-normalization: a regex broad enough to blank a field you actually assert would let a real regression pass silently. Keep a negative test that changes a meaningful field and proves the normalized output still differs from the golden — that test is the guardrail proving normalization erases only the volatile, never the asserted.

## Builders and round-trips

Two patterns round out the senior toolkit. The test-data builder (object-mother) centralizes construction of a valid domain aggregate: `newOrder(t)` returns a valid default, functional-option overrides mutate only the field a test cares about, and a schema change lands in one place instead of breaking forty hand-built fixtures. Mark the builder `t.Helper()`, and clone slices in append-style options so built objects never share a backing array. The round-trip fixture guards a wire contract in both directions at once: a versioned struct must marshal to a canonical JSON golden *and* unmarshal that golden back to the identical struct. Testing only one direction misses drift in the other — a renamed `json` tag can pass a marshal-only test (regenerate and it "matches") or an unmarshal-only test (decoding tolerates missing fields) while breaking every real consumer. Assert both, byte-compare the shape, structurally-diff the value.

## Common Mistakes

### Putting Go source in testdata/

Wrong: hiding a shared test helper as a `.go` file inside `testdata/` and expecting it to compile. The toolchain ignores `testdata/` entirely, so the code is unreachable and the package will not see it.

Fix: keep helper code in a normal `_test.go` file (or an internal package). Reserve `testdata/` for data — inputs, goldens, seeds.

### Hardcoding a long expected output inline

Wrong: embedding a fifty-line expected JSON or HTML body as a spliced string literal inside the assertion. Every output change forces a manual edit of a brittle literal, and the assertion is buried under noise.

Fix: move a large expected output into a `testdata/*.golden` file and regenerate it behind a reviewed `-update` flag. Keep only one- or two-line goldens inline as `const`.

### Building paths with string concatenation

Wrong: `"testdata/" + name` with a hardcoded slash. It is platform-fragile and easy to get subtly wrong.

Fix: `filepath.Join("testdata", name)` for on-disk fixtures (and `path.Join` for `embed.FS`, which is always forward-slash). Paths stay correct on every platform and rooted at `testdata`.

### A zero-match glob that passes silently

Wrong: `filepath.Glob("testdata/*.input")` returns an empty slice — a typo'd pattern, an empty directory — and the per-case loop simply never runs, so the test is green having asserted nothing.

Fix: fail explicitly when the discovered set is empty (`if len(matches) == 0 { t.Fatal(...) }`), and fail on a missing golden rather than skipping it.

### Comparing non-deterministic output to a golden

Wrong: byte-comparing serialized output that embeds timestamps, UUIDs, or map-iteration order against a fixed golden, producing flaky failures on correct data.

Fix: make the serializer deterministic (sort before emit) or normalize volatile fields to placeholders on both sides before comparing — and keep a negative test so normalization does not mask real regressions.

### Rubber-stamping an -update regeneration

Wrong: running `go test -update` and committing the regenerated golden without reading the diff, blessing whatever the (possibly broken) code produced.

Fix: review the golden diff exactly like a code diff. A golden you cannot explain is a bug you just blessed.

### Committing noisy or missing goldens

Wrong: writing a golden with `os.WriteFile` but forgetting to commit it, or committing one with trailing-whitespace or trailing-newline differences that fail on the next run.

Fix: commit the golden, normalize trailing whitespace, and trim consistently on both sides of the comparison so a stray newline never fails the test.

### Assuming //go:embed picks up dotfiles

Wrong: expecting `//go:embed testdata` to include a `.env` or `_fixtures` entry. Files and directories whose names begin with `.` or `_` are silently excluded.

Fix: use the `all:` prefix (`//go:embed all:testdata`) when you must embed such entries, and verify the embedded set with `fixtures.ReadDir`.

### Asserting only one JSON direction

Wrong: testing marshal *or* unmarshal but not both. A renamed `json` tag can still round-trip-fail in the untested direction, so drift slips through.

Fix: assert both directions against one canonical golden — struct to JSON to struct — so a tag change breaks at least one and shows up in review.

### Rebuilding a valid aggregate by hand in every test

Wrong: constructing a full valid domain object from scratch in each of forty tests. One schema change breaks all forty and invites copy-paste fixture drift.

Fix: use a shared builder — a valid default plus functional-option overrides — so a test states only what it varies and a schema change lands in one place.

Next: [01-render-golden-string.md](01-render-golden-string.md)
