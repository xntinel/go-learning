# Go Toolchain Through a Real CLI — Concepts

A senior backend engineer's first contact with a Go codebase is almost never
"hello world". It is a small operational binary: a health checker, a migration
runner, a config linter. Something that must build reproducibly, test
deterministically, stamp its own version so an on-call engineer can tie a running
process back to a commit, and cross-compile for a container base image. This
lesson treats the `go` command as what it actually is in production: the owner of
the package graph and the single interface to build, test, vet, race-detect,
benchmark, fuzz, and stamp artifacts. Every exercise builds a real, whole-task
piece of one `urlcheck`-style CLI, so you leave with the toolchain reflexes that
separate someone who can write Go from someone who can ship and operate it.

Read this once. It is the conceptual foundation for all ten independent exercises
that follow; each exercise is self-contained and can be built and tested on its
own, but the mental model below is shared by all of them.

## The go command owns the package graph

A **module** is the dependency boundary: one `go.mod`, one unit of versioning and
release. A **package** is the compilation unit: one directory of `.go` files
sharing a `package` clause. A **command** is nothing special — it is just a
package named `main` that the linker turns into an executable. The `go` command
operates on packages and package patterns, not on files:

```bash
go build ./cmd/urlcheck
go test ./...
go vet ./...
```

`./...` expands to every package below the current directory; `./cmd/urlcheck`
names one command package. You almost never name a single `.go` file to `go`.

### Why `go run ./cmd/x` beats `go run main.go`

Single-file mode (`go run main.go`) silently drops the sibling files, the
build-tagged files, and the generated files that belong to the same package. It
can compile and run something that is not your real program. The failure mode is
insidious: a green local run that diverges from CI, because CI builds the package
and you built one file. Always operate on the package (`go run ./cmd/urlcheck`),
never on a file.

### Three names that are independently chosen and routinely confused

`go.mod` declares the **module path** (for example `example.com/urlcheck`) that
roots every import path in the module. The **import path** of a package is that
module path plus its directory (`example.com/urlcheck/internal/check`); it
locates the code. The **package name** is the identifier in the `package` clause
(`package check`); it is the selector you type in source (`check.URL`). These
three are chosen separately. A directory named `internal/check` conventionally
holds `package check`, but nothing forces the last path element and the package
name to match, and the module path has nothing to do with either — it is just a
string you control.

## internal/ is the only enforced architectural boundary

`internal/` is special to the compiler: a package under `.../internal/...` can be
imported only by code rooted at the parent of that `internal` directory, inside
the same module. Another module physically cannot import it. That makes
`internal/` the one architectural boundary Go enforces for you: implementation
packages shared across your own commands, invisible as public API.

Everything else about layout — `cmd/`, `pkg/`, the whole
`golang-standards/project-layout` tree — is community convention, not a language
rule. A single-binary tool with a flat layout is completely valid Go.
Over-structuring a small tool with `cmd/`/`pkg/`/`internal/` scaffolding because
it "looks professional" is a common early mistake; reach for the structure when
the package graph actually needs the boundary.

## Adapter versus logic: main() is glue

`main()` is deliberately hard to unit-test: it parses flags, constructs
process-level dependencies (an `*http.Client`, a logger), calls into real
packages, and decides the process exit status. Keep it thin. Every piece of
*branchable* behavior — the exit-code policy, URL classification, the version
selection — lives in ordinary functions in ordinary packages, behind small
interfaces, where a test can reach it without spawning a process. A `main` that
contains an `if` you care about is a `main` that should have delegated that `if`
to a tested function like `shouldFail(status int) bool`.

## Dependency inversion through a one-method interface

The cleanest way to make HTTP logic testable is a tiny interface:

```go
type Client interface {
	Do(*http.Request) (*http.Response, error)
}
```

`*http.Client` already satisfies it, so production passes `http.DefaultClient`.
Tests pass `httptest.Server.Client()`, which is a real `*http.Client` wired to a
real in-process server. No hand-written mocks, and therefore no mock drift: the
test exercises the same `Do` path production does. Calling `http.Get` directly
inside business logic is the anti-pattern this replaces — it forecloses testing
without the network.

## Error contracts: identity for code, strings for humans

An error's `Error()` string is for a human reading a log. Its *identity* is for
code. A useful error contract exposes:

- **Sentinels** (`var ErrEmptyURL = errors.New(...)`) for conditions callers are
  expected to branch on, matched with `errors.Is`.
- **Typed errors** (`type StatusError struct{ Code int }`) when the caller needs
  structured data out of the error, retrieved with `errors.As`.
- **Wrapping with `%w`** (`fmt.Errorf("get %s: %w", url, err)`) so the cause
  chain is preserved and both `errors.Is` and `errors.As` can walk it.
- **`errors.Join`** to aggregate independent failures from a batch or fan-out
  into one error whose tree `errors.Is` can still search.

Comparing `err.Error()` text is the mistake this contract prevents: it breaks the
moment the wording changes or the error is wrapped one layer deeper.

## The HTTP response body lifecycle

Two rules, both about connection reuse. First, always `Close` the response body
(`defer resp.Body.Close()`); an unclosed body leaks the connection. Second,
*drain* it before closing (`io.Copy(io.Discard, resp.Body)`) so an HTTP/1.1
keep-alive connection is returned to the pool instead of being torn down. Skip
the drain and you get connection churn — a fresh TCP handshake per request — and
under load, ephemeral port exhaustion. This is a real production failure, not a
style nit.

## Context propagation

Build requests with `http.NewRequestWithContext` and give them a context that can
be cancelled: `context.WithTimeout` in `main`, `t.Context()` in a test. The
cancellation then flows all the way into the transport, so a timeout actually
aborts the in-flight request and no goroutine outlives the work it was doing.

## The test execution model

`go test` compiles and runs, but a successful result is **cached** — a second run
that touched nothing prints `(cached)` and does not execute. That is a feature,
until you are trying to observe real behavior; then use `-count=1` to force
execution (`go clean -testcache` clears the whole cache). `-race` enables the
data-race detector, which is a *runtime* instrument: it finds a race only on a
code path that actually runs concurrently during the test. A race-free serial
test says nothing about concurrent correctness. Both `-count=1` and `-race`
belong in CI. A passing test suite that never ran under `-race` is a data race
waiting for production.

## The modern testing surface (Go 1.22 to 1.26)

- Per-iteration loop variables (Go 1.22) removed the `tc := tc` dance; the loop
  variable is a fresh binding each iteration, safe to capture in a parallel
  subtest.
- `t.Parallel()` with a per-case `httptest.Server` gives real isolation between
  table-driven subtests.
- `t.Context()` (Go 1.24) is a per-test context cancelled when the test finishes.
- `for b.Loop()` (Go 1.24) replaces the manual `for i := 0; i < b.N; i++` loop
  and gets timer and setup-exclusion semantics right automatically.
- `f.Add` seeds plus `f.Fuzz` (Go 1.18) find the inputs your table forgot; the
  contract of a fuzz target is usually "must never panic" plus an invariant.

## Build metadata is observable without ceremony

`runtime/debug.ReadBuildInfo()` returns a `*debug.BuildInfo` whose `Main.Version`
is the module version and whose `Settings` carry `vcs.revision`, `vcs.time`, and
`vcs.modified` when the build happened inside a VCS checkout — no build flags
required. `go version -m ./bin` reads the same metadata back out of a finished
binary, which is how CI logs and incident responders learn what commit a process
came from. `-ldflags '-X main.version=...'` is the explicit override for values
that are simply *not* in the module graph: a build number, a release channel, a
container image digest. The classic trap is targeting the wrong symbol — `-X`
against a package other than `main`'s variable silently does nothing.

## Build constraints and cross-compilation

`//go:build linux` constraint comments and `_GOOS`/`_GOARCH` filename suffixes
(`dial_linux.go`, `cache_darwin.go`) select which files compile for which target,
at compile time. Setting `GOOS`, `GOARCH`, and `CGO_ENABLED` produces a binary
for another platform from your one dev host — which is exactly how a Linux
container image gets its binary built on a macOS laptop. For a `scratch` or
`distroless` base image you almost always want `CGO_ENABLED=0` so the binary has
no libc dependency to be missing at runtime.

## go vet is a compiles-but-suspicious gate

`go vet` is distinct from the type checker: it accepts nothing the compiler
rejects, but it flags valid Go that is almost certainly wrong. The `printf`
analyzer catches a `%d` handed a string; `copylocks` catches a `sync.Mutex`
copied by value; `lostcancel` catches a `context.CancelFunc` that is never
called; `structtag` catches a malformed struct tag. These are real bugs the
compiler is happy to build. A senior wires `gofmt`, `go vet`, and
`go test -race -count=1` into one non-negotiable quality command that a change
must pass before it merges.

## Common Mistakes

### Running a file instead of the package

Wrong: `go run cmd/urlcheck/main.go URL`. This omits sibling files, build-tagged
files, and generated files. Fix: `go run ./cmd/urlcheck URL`, which builds the
package the way CI does.

### Calling http.Get inside business logic

Wrong: reaching for `http.Get(url)` in the function that does the checking, which
makes the function impossible to test without the network. Fix: accept a
one-method `Client` interface and let production pass `http.DefaultClient` and
tests pass `server.Client()`.

### Injecting the version into the wrong symbol

Wrong: `-ldflags '-X example.com/urlcheck/internal/build.version=1.2.3'`, which
silently does nothing because the linker only rewrites a string variable that
exists at that exact path. Fix: target the `main` package's variable,
`-X main.version=1.2.3`.

### Comparing error strings

Wrong: `if err.Error() == "url is required"`, which breaks the moment the wording
changes or the error is wrapped. Fix: `errors.Is(err, ErrEmptyURL)` for identity
and `errors.As(err, &target)` for structured data.

### Closing the body without draining it

Wrong: `defer resp.Body.Close()` with no read, which defeats HTTP/1.1 connection
reuse and causes connection churn under load. Fix: drain first with
`io.Copy(io.Discard, resp.Body)` and then close.

### Trusting a cached green test

Wrong: assuming a green `go test` means the code just ran, when it was served
from cache. Fix: use `-count=1` when you need real execution, and put it in CI.

### Shipping concurrent code that never ran under -race

Wrong: relying on a serial test to vouch for concurrent code, so a data race
hides until production. Fix: the race detector is a runtime instrument — exercise
the concurrent path in a test run under `-race`.

### Writing a manual b.N benchmark loop

Wrong: `for i := 0; i < b.N; i++` with hand-placed `b.ResetTimer` calls, which is
easy to get wrong. Fix: `for b.Loop()`, which excludes setup and manages the
timer correctly in Go 1.24+.

### Over-structuring a single-binary tool

Wrong: treating `cmd/`/`internal/`/`pkg/` as a language rule and scaffolding all
of it for a one-file tool. Fix: only `internal/` is compiler-enforced; use a flat
layout until the package graph needs a boundary.

### Forgetting CGO_ENABLED=0 for a scratch image

Wrong: cross-compiling for a distroless or `scratch` container with cgo enabled,
producing a binary that fails at runtime for want of libc. Fix: build with
`CGO_ENABLED=0` (or match the base image's libc).

### Leaving vet findings unaddressed

Wrong: ignoring vet because the code compiles. Fix: vet catches valid-but-wrong
Go — a bad `Printf` verb, a copied lock, a lost cancel — that the type checker
allows; drive its findings to zero.

Next: [01-check-package-and-error-contract.md](01-check-package-and-error-contract.md)
