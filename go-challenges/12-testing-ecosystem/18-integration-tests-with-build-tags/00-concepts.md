# Integration Tests With Build Tags — Concepts

A senior backend engineer does not learn build tags as syntax. They learn them as
the load-bearing seam between two test tiers that pull in opposite directions: a
fast, hermetic PR gate — `go test ./...`, sub-second, zero external dependencies —
and a slow, stateful integration tier that owns real Postgres, real migrations,
and real network. Chapter 13 owns the constraint *mechanism* — where the
`//go:build` line sits, the boolean grammar, the automatic tags. This chapter is
where a real database-backed test tier is actually built, operated, and kept from
flaking. The tag is just the gate; the hard part is everything behind it: owning
the fixture lifecycle exactly once, isolating every test from every other test,
surviving the CI startup race, seeding deterministic fixtures, splitting tiers,
and never letting a tagged file rot silently because nothing in the default gate
ever compiles it. This file is the conceptual foundation for the nine independent
modules that follow.

## Concepts

### A build tag gates at compile time, not run time

A `//go:build integration` line is evaluated by the `go` tool *before* the
compiler runs. When the tag is not present in the build, the file is not handed to
the compiler at all — it is as if it did not exist. This one property is what
distinguishes a build tag from every runtime mechanism, and it is the whole reason
the integration tier uses tags rather than skips. Because the file is never
compiled in the default build, it may import a Postgres driver, a testcontainers
helper, or an AWS SDK that never enters the default build graph. `go test ./...`
with no tags does not fetch, compile, or link any of them. `testing.Short()` and
`t.Skip` can never give you that: they leave the file — and every one of its
imports — compiled and linked; they only decide, at run time, whether the test
body executes.

### The two orthogonal gating axes and the mechanical decision rule

There are two independent ways to keep a test out of a run, and conflating them is
the single most common error. The build tag is a *compile-time* gate: the file is
in the build or it is not. A runtime skip — `testing.Short()`/`-short`, `t.Skip`,
or an environment check — is a *run-time* gate: the file compiles normally and the
test itself decides, while running, whether to do work. The decision rule is
mechanical: if excluding the test requires removing an import from the build graph,
use a build tag; if the code compiles fine by default and you only want to skip
slow or environment-dependent *execution*, use `-short` or an env-gated `t.Skip`.
An integration file that imports `pgx` still drags `pgx` into the build under
`-short` — only the tag removes it. In practice the two are stacked: the tag keeps
the driver out of the default build, and an env-gated `t.Skip` (on `DATABASE_URL`)
lets a developer *compile* the integration tier locally for build-safety without a
live database on their laptop.

### The constraint placement rule that silently breaks gating

`//go:build integration` must sit near the top of the file, preceded only by blank
lines and other line comments, and it must be followed by a blank line before the
`package` clause. Without that blank line, Go attaches the comment to the package
as documentation, the constraint stops gating, and the file compiles in *every*
build — the exact opposite of the intent, and it fails silently because everything
still passes, just slower and with the driver linked into your fast tier. The
canonical shape is three lines: the `//go:build` line, a blank line, then
`package`. Only one `//go:build` line is permitted per file.

### TestMain owns the integration fixture lifecycle

`func TestMain(m *testing.M)` runs once per package. It is where an integration
package connects to Postgres once, applies its schema and migrations once, seeds
its baseline fixtures once, runs the whole suite via `m.Run()`, and tears down
once — rather than paying that cost per test. Place `TestMain` in a file gated by
`//go:build integration` and the entire heavyweight lifecycle is scoped to the
integration build; the default build has no `TestMain` and no driver. One subtlety
sinks naive implementations: `os.Exit` does not run deferred functions. If your
teardown is a `defer db.Close()`, writing `os.Exit(m.Run())` skips it and leaks the
connection or container. The fix is to route the exit through a helper that
*returns* the code so the defers fire first:

```go
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	db := setup()
	defer db.Close() // runs before run returns, so before os.Exit
	return m.Run()
}
```

### Test isolation is the central integration concern

The classic integration flake is a suite that passes when each test runs alone but
fails when they run together, or passes or fails depending on order, because one
test leaked state into the shared database. Isolation is not optional; it is the
main design decision, and there are three strategies with distinct trade-offs.
*Transaction-per-test* opens a `BeginTx`, runs every read and write against that
`*sql.Tx`, and registers `t.Cleanup(tx.Rollback)` so the writes vanish at the end
of the test — it is the fastest and it auto-cleans, but it forbids code under test
that itself commits, and it is not parallel-safe on a single shared connection.
*Truncate-between-tests* allows commits but must serialize, truncating the tables
in cleanup. *Fresh-schema-per-run* (a new database or schema per test) is the most
isolated and the slowest. Pick per what the code under test actually does: if it
commits its own transactions, rollback isolation cannot see its writes and you must
truncate.

### Readiness is a real production concern, not a sleep

In CI and with testcontainers, the test binary routinely starts before the
database accepts connections. The wrong fixes are a fixed `time.Sleep` — flaky when
the runner is busy, slow everywhere else — and an unbounded ping loop that hangs
the CI job forever when the database never comes up. The right fix is a bounded
readiness retry: call `PingContext` with a capped exponential backoff, under a
`context` deadline, so it returns success as soon as the database is up and returns
`context.DeadlineExceeded` (never hangs) when it is not. The deadline comes from
the test — `t.Context()` (Go 1.24) is cancelled just before cleanup, so a stuck
ping fails the test instead of wedging the tier.

### Deterministic seeding and idempotent migrations

The integration tests depend on a known baseline: a fixed set of rows that must be
there and nothing else. Establish it once, in `TestMain`, before `m.Run()`. Make
migrations idempotent with `CREATE TABLE IF NOT EXISTS` and seeds idempotent with
upserts (`INSERT ... ON CONFLICT ... DO UPDATE`), so re-running the suite — or
running `migrate` twice — is a no-op rather than a duplicate-key failure. A failed
migration must abort the whole suite with a non-zero exit *before* any test runs;
a suite that runs against a half-migrated schema produces noise, not signal.

### The default gate stays hermetic; slow tiers move behind tags

The default gate must stay tag-free and hermetic: `go test ./...` sub-second, no
external dependencies, so it runs in the pre-commit and PR loop. The slow, stateful
tiers move behind `-tags=integration` and `-tags=e2e` as *separate CI stages*, each
with its own DSN or base URL, its own `-race` and `-count=1` policy, and its own
env-gated skip. This is not just tidiness: a fast default gate is what makes the PR
loop usable, and a fast PR loop is what keeps engineers running the tests at all.

### Boolean tag expressions and automatic tags

A single `//go:build` line takes a boolean expression, which lets one file target a
precise slice of a CI matrix: `//go:build integration && !race` compiles in the
integration tier but never under the race detector (useful when a dependency is not
race-clean, or a test is too slow under `-race`). Some tags are set automatically
by the toolchain and never passed on the command line: `GOOS` (`linux`, `darwin`),
`GOARCH` (`amd64`, `arm64`), `unix`, `cgo`, `race`, and the `go1.N` language-version
tags. Custom tags — `integration`, `e2e` — come *only* from `-tags` on `go build`,
`go test`, and `go vet`.

### Context flows from the test

`t.Context()` (Go 1.24) returns a context that is cancelled just before the test's
`Cleanup` functions run. It is the right deadline source for `PingContext`,
`ExecContext`, and outbound HTTP calls in the integration and e2e tiers: a stuck
query or a dead service then fails the test with a deadline error instead of
hanging the stage. Thread it through every blocking database or network call.

### The silent-green trap: vet and coverage honor -tags

`go vet`, `go build`, and `go test` all honor `-tags`. The corollary is sharp:
every tagged tier must be vetted, built, and coverage-measured *under its own tag
set*, or its files are never compiled in CI and real defects hide while the default
suite reports green. A `Printf` format bug or a compile error in an
`//go:build integration` file is invisible to `go vet ./...`; only
`go vet -tags=integration ./...` sees it. A CI pipeline that runs only the default
gate against the integration tier is not testing that tier at all — it is reporting
the green of a suite that never compiled.

## Common Mistakes

### Omitting the blank line after the constraint

Wrong: `//go:build integration` immediately followed by `package repo` with no
blank line. Go reads the constraint as the package doc comment, gating silently
stops, and the integration file compiles in every build — the driver leaks into
your fast tier and nothing warns you.

Fix: always leave exactly one blank line between the `//go:build` line and the
`package` clause. Let `gofmt` canonicalize the file so it stays correct.

### Trusting the default `go test ./...` to cover the integration tier

Wrong: a CI pipeline that runs only `go test ./...` and assumes it exercises the
database tests. Those files carry `//go:build integration`, so they are never
compiled; an entire tier never runs while the suite reports green.

Fix: a dedicated CI stage that runs `go test -tags=integration` with a live DSN,
plus `go vet -tags=integration` and `go build -tags=integration` so the tagged
files are actually compiled and checked.

### Reaching for -short to keep a driver out of the default build

Wrong: gating an integration test with `testing.Short()` to avoid the slow path.
`-short` does not exclude the file from compilation, so the Postgres driver stays
in the build graph and links into the default binary.

Fix: use a build tag to remove the import; reserve `-short` for skipping slow
*execution* of code that compiles cheaply.

### os.Exit(m.Run()) with deferred teardown

Wrong: `func TestMain(m *testing.M) { db := setup(); defer db.Close(); os.Exit(m.Run()) }`.
`os.Exit` never runs the deferred `db.Close`, so the connection or container leaks.

Fix: route through a `run(m) int` helper that returns the code so its defers fire
before `os.Exit(run(m))`.

### Heavyweight connect+migrate in every test instead of TestMain

Wrong: opening a connection and running migrations at the top of every integration
test. The tier becomes slow and flaky under load and against connection limits.

Fix: do the connect, migrate, and seed exactly once in `TestMain`; each test just
opens a cheap transaction against the already-prepared database.

### t.Parallel on a test that shares one connection or transaction

Wrong: `t.Parallel()` on an integration test that reads and writes a single shared
`*sql.Tx` or connection, or that calls `t.Setenv` (which panics if the test or any
ancestor is parallel). Transaction-per-test isolation on one connection is not
parallel-safe.

Fix: keep transaction-per-test tests serial, or give each parallel test its own
connection and its own transaction.

### Committing writes with no rollback or truncate strategy

Wrong: integration tests that commit rows and never clean up. State leaks between
tests: they pass alone and fail together, or the result depends on run order.

Fix: choose an isolation strategy — rollback, truncate, or fresh schema — and apply
it uniformly through `t.Cleanup` so every test starts from the same baseline.

### Sleeping a fixed duration to wait for the database

Wrong: `time.Sleep(5 * time.Second)` before the first query, hoping Postgres is up.
It is simultaneously flaky (too short on a busy runner) and slow (too long
everywhere else); an unbounded retry loop is worse, hanging the CI job forever.

Fix: a bounded `PingContext` retry with capped backoff and a `context` deadline, so
it returns as soon as the database is ready and returns `DeadlineExceeded` — never
hangs — when it is not.

### Forgetting that vet and build honor -tags

Wrong: assuming `go vet ./...` covers the whole repository. A vet violation or a
compile error in a tagged file is never seen by the default vet run; it hides
behind a passing suite until the integration stage finally compiles the file.

Fix: run `go vet -tags=integration ./...` and `go build -tags=integration ./...` in
the integration CI stage — every tier vetted and built under its own tag set.

### Getting legacy // +build semantics backward

Wrong: hand-translating an old `// +build linux,amd64 darwin` line from memory and
mixing up that comma is AND and space is OR, producing a constraint that means
something other than intended.

Fix: let `gofmt` canonicalize `// +build` to `//go:build` rather than translating
by hand; the modern `//go:build` grammar uses ordinary `&&`, `||`, `!`.

Next: [01-store-and-unit-tests.md](01-store-and-unit-tests.md)
