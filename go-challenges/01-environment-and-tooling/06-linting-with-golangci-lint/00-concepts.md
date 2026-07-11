# Linting with golangci-lint (v2) — Concepts

On a backend team, `golangci-lint` is not a tool you run locally when you remember
to. It is a required CI gate: a pull request that fails the linter does not merge.
That framing changes what matters about it. The interesting skills are not "how do
I run it once" but "how do I make it produce the *same* verdict on every laptop and
every CI runner", "how do I adopt it on a 200k-line service that has never been
linted without drowning the team in ten thousand findings", and "how do I let a
genuine exception through without quietly turning off a whole class of checks on
production code". This file is the conceptual foundation for the ten independent
exercises that follow; each exercise is a self-contained Go module you can build,
test, and reason about on its own.

The whole lesson is anchored on two finding classes — unchecked errors (`errcheck`)
and unclosed resources (`bodyclose`) — because those are the linter findings that
correlate with real production incidents in backend services. A discarded
`os.MkdirAll` error masks a permission-denied or read-only-filesystem failure; an
unclosed `http.Response.Body` leaks file descriptors until the process exhausts
them under load. These are not style nits. They are latent outages that a `go test`
run happily reports as green.

## The model: a meta-linter composing many analyzers

A single linter covers one class of bug. `errcheck` flags discarded error returns.
`staticcheck` flags a large family of suspicious-but-valid constructs. `bodyclose`
flags an `http.Response.Body` that is never closed. `unused` flags dead code.
`ineffassign` flags assignments whose value is never read. Running each of these as
its own binary is slow — every tool re-parses and re-type-checks the whole program —
and produces inconsistent output formats.

`golangci-lint` is a *meta-linter*: it runs dozens of these analyzers in one
process, in parallel, and — crucially — shares the parsed AST and typed program
state across all of them, so the marginal cost of adding another analyzer is small.
It deduplicates overlapping findings and emits one unified report. On a warm cache a
whole-repo run is a few seconds. That shared-state architecture is why "run the
meta-linter" is cheaper than "run these fifteen tools", not more expensive.

### go vet is the floor; golangci-lint is the ceiling

Every check `go vet` performs is available inside `golangci-lint` through the
`govet` linter. The reverse is not true: the meta-linter adds entire bug classes
`go vet` was never designed to catch — unchecked errors, unclosed bodies, dead
code, ineffectual assignments. The right mental model is that `go vet` is the floor
and `golangci-lint` is the ceiling. Keep both in CI. `go vet` is fast and rock
stable; the meta-linter is cheap on warm caches and catches far more. Treating
`go vet` as a *replacement* for the meta-linter leaves every added class undetected.

## v2 is the current line, and it changed the config schema

`golangci-lint` v2 (v2.0 shipped March 2025) is the current major line, and it
reshaped the configuration file. The file now begins with `version: "2"`. Instead
of the old `disable-all: true` / `enable-all: true` toggles you now write
`linters.default:` with one of four values — `none`, `standard`, `all`, or `fast`.
Per-linter tuning lives under `linters.settings`. Issue filtering moved under
`linters.exclusions` (with `rules`, `presets`, `generated`, and `paths` subkeys).
And formatting became its own first-class `formatters` section.

Three v2 changes are classic migration footguns:

- `gosimple` and `stylecheck` were **merged into `staticcheck`**. Listing them
  separately is now an error. A v1 config that enabled all three must collapse to
  just `staticcheck` — and if you delete `gosimple`/`stylecheck` without confirming
  `staticcheck` still covers them, you have silently lost coverage.
- The formatters `gofmt`, `gofumpt`, `goimports`, and `gci` moved out of `linters`
  and into `formatters`. A v1 config that listed `gofmt` under `linters` quietly
  stops formatting after a naive migration, because the linter name no longer means
  anything there.
- `linters-settings` split into `linters.settings` and `formatters.settings`;
  `issues.exclude-rules` became `linters.exclusions.rules`.

There is a `golangci-lint migrate` command that mechanically applies these
mappings. It writes a backup (`<name>.bck.<ext>`) and, importantly, **does not
carry over your comments** — you re-add them by hand. The discipline that makes a
migration trustworthy is to diff the enabled-linter set before and after and confirm
nothing was dropped except the intended merges.

### Formatting is part of the same gate

In v2 the formatters (`gofmt`, `gofumpt`, `goimports`, `gci`, `golines`) are a
proper section driven by `golangci-lint fmt` (with `--diff` to preview). Any
formatter you *enable* also runs as a check during `golangci-lint run` — so a file
that is not `gofumpt`-clean fails the same gate that catches an unchecked error.
Formatting and linting share one gate; there is no separate "did you run gofmt"
step to forget.

## Reproducibility is the entire point

The reason to commit a config and pin the tool version is that the *default* linter
set changes between releases. If you rely on defaults, "passes on my laptop" and
"fails in CI" become the normal state of the world the moment the runner has a
different version. Two rules make a run reproducible:

1. Commit `.golangci.yml` at the module root.
2. Pin the tool version — `go install .../golangci-lint/v2/cmd/golangci-lint@v2.x.y`
   or a pinned CI action — so every environment resolves the same binary.

`linters.default: none` plus an explicit `enable` list is the most predictable
style, because the enabled set is *exactly* what you listed. No version-dependent
default set can leak in and change the verdict under you. Validate the file itself
with `golangci-lint config verify`, which checks it against the JSON schema and
rejects a typo like a misspelled linter name instead of silently ignoring it.

## Adopting on a real codebase: start small, gate new code

Two failure modes kill linting adoption, and both come from turning on too much at
once.

The first is enabling every linter (`linters.default: all`) on an existing project.
You get hundreds of low-value stylistic findings, developers learn to scroll past
the output, and the two real bugs in the batch get buried. The fix is discipline:
start with a short, high-value list (`errcheck`, `govet`, `staticcheck`, `unused`,
`ineffassign`, `bodyclose`) and add linters one at a time as the team agrees.

The second is big-bang adoption on a legacy codebase — turning the full gate on and
drowning in thousands of pre-existing findings. The fix is *incremental* linting.
`golangci-lint run --new-from-merge-base=main` (or `--new-from-rev`) uses the git
diff to report only issues on lines that changed relative to a baseline; findings on
untouched lines are skipped. This lets you require clean linting on *new* code
immediately, while you burn down the legacy backlog separately. It is the single
most important technique for rolling linting onto a service that predates it.

## Suppression is an auditable exception, not an escape hatch

`//nolint` silences a finding on the next line. Used carelessly it hides the bug
while pretending to fix it — a bare `//nolint` on an `os.Open` leaves the unchecked
error exactly where it was. The policy that keeps suppression honest: a suppression
must name the *specific* linter and carry a *reason*. The `nolintlint` linter
enforces exactly this — `require-specific` rejects a bare `//nolint`,
`require-explanation` rejects one with no comment, and `allow-unused: false` flags a
stale suppression that no longer matches any finding. With `nolintlint` on, every
`//nolint` in the tree is specific, justified, and reviewable, and dead ones get
cleaned up.

## Scope exclusions narrowly, never module-wide

Sometimes a finding is acceptable in one context — an unchecked error in a
`_test.go` helper, a `staticcheck` complaint in generated or vendored code. The
wrong reaction is to disable the linter for the whole module, which strips coverage
from production code to silence a test. The right reaction is a scoped rule under
`linters.exclusions.rules`, keyed on `path`, `linters`, and optionally `text` or
`source`, so the relaxation applies only where you intend. v2 also ships exclusion
*presets* (`comments`, `std-error-handling`, `common-false-positives`, `legacy`)
and explicit generated-code handling (`generated: lax | strict | disable`), so you
opt into curated exclusions deliberately rather than inheriting a mystery default.

## Why tests do not catch these bugs

The uncomfortable truth threaded through this lesson: a green `go test -race` run
does not mean the code is clean. The writer package built in the first exercises has
a discarded `os.MkdirAll` error, and its table tests pass on both the buggy and the
fixed version — because the tests use writable temp directories where `os.MkdirAll`
never fails. The latent bug only surfaces on a read-only filesystem in production.
The linter is what catches it. Lint and tests are complementary gates: tests prove
behavior on the paths you exercised; the linter proves the absence of whole bug
classes on paths you did not.

## Common Mistakes

### Enabling every linter at once on an existing project

Wrong: `linters.default: all` on a codebase that has never been linted. The output
floods with stylistic noise, the team learns to ignore the gate, and real bugs hide
in the churn. Fix: start with a short high-value list and expand one linter at a
time as the team agrees on each standard.

### A bare //nolint that hides the bug

Wrong: `f, _ := os.Open(path) //nolint:errcheck` with no reason, or a bare
`//nolint` that disables every linter on the line. The finding goes away; the
unchecked-error bug stays. Fix: handle the error, or — if the suppression is
genuine — name the specific linter and add a justification, and turn on `nolintlint`
(`require-specific`, `require-explanation`) so the policy is enforced, not hoped for.

### Relying on the default linter set with no committed config

Wrong: running `golangci-lint run` with no `.golangci.yml` and no pinned version.
The default set drifts between releases, so CI diverges from local runs and a green
laptop becomes a red pipeline after an unrelated upgrade. Fix: commit the config and
pin the tool version so every environment resolves an identical binary and ruleset.

### Treating go vet as good enough

Wrong: running only `go vet` in CI because it is fast and built in. Every bug class
the meta-linter adds — `errcheck`, `staticcheck`, `bodyclose` — goes undetected.
Fix: run both. `go vet` is the fast, stable floor; the meta-linter is the ceiling
and is cheap on warm caches.

### Carrying a v1 config forward unchanged

Wrong: assuming a v1 `.golangci.yml` still works. v2 rejects `disable-all`/
`enable-all` and the separate `gosimple`/`stylecheck` entries, and it quietly stops
formatting because `gofmt`/`goimports` must now live under `formatters`. Fix: run
`golangci-lint migrate`, re-add the comments it dropped by hand, and diff the
enabled-linter set before and after to prove no coverage was lost.

### Big-bang adoption on a legacy codebase

Wrong: turning the full gate on across a service that predates linting and trying to
merge a ten-thousand-finding cleanup. Fix: gate only new code with
`--new-from-merge-base` so changed lines must be clean, and burn the legacy backlog
down separately on its own schedule.

### Disabling a whole linter to silence a test-only finding

Wrong: dropping `errcheck` from the enabled set because a `_test.go` helper trips
it. Production code loses the check to quiet a test. Fix: write a scoped
`linters.exclusions.rule` keyed on `path: _test\.go` and `linters: [errcheck]` so
production coverage is untouched.

### Not running the linter in CI at all

Wrong: trusting developers to run it locally. Findings slip through review, the gate
is theoretical, and the codebase drifts. Fix: wire `golangci-lint run ./...` as a
required CI check with a pinned version — local runs are for fast feedback, CI is
the gate that actually blocks a merge.

### Assuming green tests mean clean code

Wrong: shipping because `go test -race` is green. The writer's discarded
`os.MkdirAll` passes every test that runs on a writable temp dir; only the linter
catches it. Fix: treat lint and tests as complementary gates — neither subsumes the
other.

Next: [01-errcheck-unchecked-io-error.md](01-errcheck-unchecked-io-error.md)
