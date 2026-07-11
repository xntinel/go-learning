# Build Constraints for Test Separation — Concepts

A senior backend test suite has two jobs that pull in opposite directions. It must
stay fast and deterministic so `go test ./...` runs in a pre-commit or PR loop in
under a second, and it must still *own* the slow, stateful tests that touch real
Postgres, real brokers, and real networks. Build constraints are the mechanism
that lets both live in one repository. `//go:build integration` is the gate that
keeps the default suite hermetic while a separate CI stage compiles and runs the
database-backed tier with `-tags=integration` and a live DSN. This file is the
conceptual foundation for the ten independent modules that follow; read it once
and you have the model you need to reason through every one of them.

## Concepts

### A build constraint is a compile-time gate, not a runtime skip

A `//go:build` line is evaluated by the `go` tool *before* the compiler ever sees
the file. When its boolean expression is false the file is not handed to the
compiler at all — it is as if the file did not exist. This single property is
what distinguishes a build tag from every runtime mechanism. Because the file is
never compiled, it may import packages that never enter the default build graph:
a Postgres driver, a testcontainers helper, an AWS SDK. `go test ./...` with no
tags does not fetch, compile, or link any of them. That is exactly why the tag is
the right tool when the integration file *imports something you do not want in the
default build*, and it is the property that a runtime skip can never give you.

### Where the constraint must sit, and the one-line rule

The constraint must appear near the top of the file, preceded only by blank lines
and other line comments. In a Go source file it must be followed by a blank line
before the `package` clause; without that blank line Go attaches the comment to
the package as documentation and the constraint silently stops gating — the file
is then compiled in *every* build. Only one `//go:build` line is permitted per
file; a second is a hard error. The canonical shape is three lines:

```text
//go:build integration

package repo
```

### The expression grammar is boolean

`//go:build` expressions use `&&` (AND), `||` (OR), `!` (NOT), and parentheses for
grouping. That is enough to target a precise slice of a CI matrix from a single
file: `//go:build integration && !race` compiles only in the integration tier and
never under the race detector, and `//go:build (linux && amd64) || (darwin && arm64)`
selects two exact platform pairs. Keep the whole condition on the one allowed
`//go:build` line.

### Some tags are satisfied automatically

You never pass these on the command line — the toolchain sets them from the build
environment: the `GOOS` value (`linux`, `darwin`, `windows`, …), the `GOARCH`
value (`amd64`, `arm64`, …), the aggregate `unix` tag (any Unix-like GOOS, since
Go 1.19), `cgo` when cgo is enabled, the compiler name (`gc`/`gccgo`), the
`go1.N` language-version tags, and `race` when the build runs under `-race`.
Custom tags — `integration`, `e2e`, and anything else — come only from `-tags` on
`go build`, `go test`, and `go vet`.

### File-name constraints are implicit

A source file whose name ends in `_GOOS.go`, `_GOARCH.go`, or `_GOOS_GOARCH.go`
carries the corresponding constraint with no comment at all. `filelock_windows.go`
compiles only on Windows; `pipe_linux_amd64.go` only on 64-bit Linux. The `_test`
suffix combines with them, so `store_linux_test.go` is a Linux-only test file. A
trap follows directly: if a file both is named `foo_windows.go` and carries a
`//go:build linux` line, the implicit and explicit constraints are ANDed together
and the file compiles on no platform at all.

### The two orthogonal gating axes

This is the distinction engineers conflate most often. There are two independent
ways to keep a test out of a run, and they operate at different times:

- The build tag is a *compile-time* gate: the file is included in the build or it
  is not. Choose it when the file must not compile by default — because it imports
  a driver or a heavy dependency you want out of the default build graph.
- Runtime skipping via `testing.Short()`, `t.Skip`, or an environment check is a
  *run-time* gate: the file compiles normally and the test itself decides, while
  running, whether to execute. Choose it when the code is cheap to compile but
  slow or environment-dependent to run.

The decision rule is mechanical: *if excluding it requires removing an import from
the build, use a build tag; if the code compiles fine in the default build and you
only want to skip slow or environment-gated execution, use `-short` or a skip.*
`-short` does not exclude a file from compilation, so an integration file that
imports `pgx` still drags `pgx` into the build graph under `-short` — only a build
tag removes it.

### TestMain owns the integration fixture lifecycle

`func TestMain(m *testing.M)` runs once per package. The canonical body is
`code := m.Run(); os.Exit(code)`, with fixture setup before `m.Run()` and teardown
after. This is where an integration package spins up or connects to Postgres once,
applies its schema/migrations once, runs the whole suite, and tears down once —
rather than paying that cost per test. Placing `TestMain` in a file gated by
`//go:build integration` scopes the entire heavyweight lifecycle to the integration
build; the default build has no `TestMain` and no driver. One subtlety: `os.Exit`
does not run deferred functions, so if teardown is a `defer`, put the body in a
helper that *returns* the code and call `os.Exit(run(m))` from `TestMain`, so the
defers fire before the exit.

### The `//go:build ignore` convention

`ignore` is a tag that is never satisfied, so a file carrying `//go:build ignore`
is excluded from `go build` and `go test` on the package — yet it remains runnable
directly with `go run file.go`. This is the idiom for seed programs, code
generators, and example commands: a `package main` file can sit in the same
directory as a library package without a package-name conflict, precisely because
the constraint keeps it out of the package build. `go generate` directives and
seed scripts lean on this constantly.

### Legacy `// +build` and gofmt canonicalization

Before Go 1.17 the syntax was `// +build`, with different and easily-misremembered
rules: on one line, space means OR and comma means AND, a leading `!` negates a
term, and multiple `// +build` lines are ANDed together. So `// +build linux,amd64 darwin`
means `(linux AND amd64) OR darwin`. Go 1.17 introduced `//go:build` with a normal
boolean grammar and made `gofmt` keep the two in sync: given a file with only a
`// +build` line, `gofmt` adds the equivalent `//go:build` line above it; given a
`//go:build` line, `gofmt` leaves it canonical. A lone modern `//go:build` line is
already canonical. During migration, run every touched file through `gofmt` so the
old and new forms never disagree.

### The default gate stays tag-free

The organizing principle for the whole scheme: the default PR / pre-commit gate
runs `go test ./...` with no tags so it is fast and hermetic, and the slow,
stateful, network-touching tiers move behind `-tags=integration` and `-tags=e2e`
in separate CI stages, each with its own DSN or endpoint, its own `-race` and
`-count=1` policy. `go vet` and `go build` also honor `-tags`, so each tagged tier
must be vetted and built under its own tag set — otherwise the tagged files are
never compiled in CI and bugs hide in them while the suite stays green.

## Common Mistakes

### Missing the blank line after the constraint

Wrong: `//go:build integration` on the line immediately above `package repo` with
no blank line between them. Go treats the constraint as the package doc comment,
the constraint stops gating, and the file compiles in every build — the opposite
of the intent.

Fix: always leave exactly one blank line between the `//go:build` line and the
`package` clause.

### Tagging a production file needed in the release binary

Wrong: putting `//go:build integration` on a non-test `.go` file that production
code depends on. The default build (and the release binary) excludes it, so the
binary fails to compile or loses behavior.

Fix: keep production build tags deliberate and rare; for test-only gating, put the
tag on a `_test.go` file so only the test build is affected.

### Assuming `go test ./...` covers the integration tier

Wrong: relying on the default CI `go test ./...` to run integration tests. Without
`-tags=integration` those files are never compiled, so an entire tier never runs
while the suite reports green.

Fix: run the integration tier in its own stage with `-tags=integration` (and a
DSN), and vet/build it under the same tag set.

### Reaching for `-short` when you needed a tag

Wrong: guarding an integration test that imports a driver with
`if testing.Short() { t.Skip() }`. The file still compiles under `-short`, so the
driver stays in the default build graph — you skipped the run but not the import.

Fix: when the goal is to keep an import out of the default build, use a build tag,
not `-short`.

### Two constraint lines that disagree

Wrong: leaving both a stale `// +build integration` and a new
`//go:build !integration`, or writing two `//go:build` lines. This is an error or,
worse, a constraint that means something other than intended.

Fix: run `gofmt` to canonicalize; keep exactly one `//go:build` line per file.

### Getting legacy `// +build` semantics backward

Wrong: hand-migrating `// +build linux,amd64` to `//go:build linux || amd64`. In
the legacy form the comma is AND, so the correct translation is
`//go:build linux && amd64`.

Fix: remember space is OR and comma is AND in `// +build`, and let `gofmt` do the
translation rather than doing it from memory.

### Forgetting that vet and build honor tags

Wrong: assuming a tagged file is checked by the default `go vet ./...`. It is not
compiled or vetted without its tag, so defects in tagged files hide.

Fix: run `go vet` and `go build` under each tag set your CI exercises.

### `t.Setenv` in a parallel test

Wrong: copying a parallel unit test into an integration file and keeping
`t.Parallel()` alongside `t.Setenv`. `t.Setenv` panics if the test — or any
ancestor — is parallel.

Fix: drop `t.Parallel()` from any test that calls `t.Setenv`.

### Per-test DB setup instead of a TestMain fixture

Wrong: connecting to and migrating the database in every integration test, making
the tier slow and flaky; or writing `TestMain` without `os.Exit(m.Run())` so
deferred teardown never runs.

Fix: do heavyweight setup once in `TestMain`, share the connection, and route the
exit through a helper so defers fire before `os.Exit`.

### File-name and `//go:build` constraints that contradict

Wrong: `foo_windows.go` carrying `//go:build linux`. The implicit `windows` and
the explicit `linux` are ANDed, so the file compiles nowhere.

Fix: keep the file name and the `//go:build` line consistent, or drop one of them.

Next: [01-inmemory-store-unit-tests.md](01-inmemory-store-unit-tests.md)
