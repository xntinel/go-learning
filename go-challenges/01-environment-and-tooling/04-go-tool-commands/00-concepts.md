# Go Tool Commands: The Toolchain a Senior Runs Every Day — Concepts

A senior backend engineer does not "use the go tool" occasionally. The toolchain
IS the daily loop and, once wired into CI, the contract that keeps a repository
shippable. `go build`, `go test`, `go vet`, `gofmt`, `go doc`, `go list`,
`go env`, and `go version` are not eight separate utilities; they are eight views
of one thing — the package graph — and every one of them can catch, or hide, a
production defect. This file is the conceptual foundation. Read it once and you
have the model behind each of the ten independent exercises that follow: what the
command really operates on, the specific failure it prevents, and where teams
misuse it.

## The go command owns the package graph

Every source-touching subcommand — `build`, `test`, `vet`, `fmt`, `doc`, `list` —
takes package *patterns*, not file paths. A pattern like `./cmd/demo` names a
package by its import-path suffix; `fmt` names a standard-library package; and
`./...` means "the current directory and everything recursively below it". That
recursive pattern is the correct default for `build`, `test`, `vet`, `fmt`, and
`list`, because it picks up subpackages, test files, and build-tagged files
automatically. The habit to build is: pass `./...` from the module root unless
you have a reason to narrow it. A bare `go vet` (no argument) analyzes only the
current directory and silently skips every subpackage — a classic way to get a
green local run and a red one in CI, or worse, a green CI that never looked at
the package with the bug.

## Single-file mode is a different, smaller command

`go run main.go` is not `go run` with an argument; it is *single-file mode*. It
treats the listed `.go` files as one ad-hoc package, bypasses module-path
resolution, and therefore ignores sibling files in the same directory,
build-tagged files, and the real layout entirely. For a one-file scratch program
it is fine. For anything with more than one file in the package — which is every
real service — it silently compiles a different program than the one you ship.
The same file run as `go run ./cmd/demo` is *package mode*: the whole package,
every non-excluded file, exactly what `go build ./cmd/demo` would produce. Prefer
the package form always. A related trap: `go run` reports its own exit status,
flattening any non-zero program exit to `1`. To observe a program's real
`os.Exit(2)`, build the binary and run it directly, then read `$?`.

## gofmt is the canonical, zero-configuration formatter

`gofmt` has no options that change *what* canonical looks like — there is no
style debate, because after `gofmt` every Go file in the ecosystem is
byte-identical in layout. What it does have are four operational modes that map
onto CI and editing: `-l` lists the files that are *not* already canonical (the
read path a gate uses), `-d` prints a unified diff of what it would change (the
eyeball path in review), `-w` rewrites files in place, and `-s` applies safe
*simplifications* (for example `s[a:len(s)]` becomes `s[a:]`, and a redundant
element type inside a composite literal is dropped). `go fmt ./...` is a thin
wrapper that runs `gofmt -l -w` per package and prints what it touched. The gate
form is `test -z "$(gofmt -l .)"`: it succeeds only when the list of
non-canonical files is empty. Hand-rolling a formatter or import sorter that
wraps `gofmt` is a mistake; the output drifts from the ecosystem and every diff
becomes noise. `goimports` is the one sanctioned extension.

## go vet is the static-analysis floor, not the ceiling

`go vet` flags code that *compiles* but is almost always wrong — the gap between
"the compiler accepted it" and "it does what you meant". The canonical example is
a `printf` format mismatch: `fmt.Printf("%d", name)` where `name` is a string
builds cleanly, because format-string checking is a runtime reflection concern
the compiler does not make, and then prints garbage in production. `go vet`
performs that check statically. Other analyzers a backend hits routinely:
`copylocks` (copying a value that contains a `sync.Mutex`, which silently makes a
second, useless lock), and two added in Go 1.25 — `hostport`, which reports
`fmt.Sprintf("%s:%d", host, port)` addresses because they break for IPv6 literals
and suggests `net.JoinHostPort` instead; and `waitgroup`, which reports a
misplaced `sync.WaitGroup.Add` call (typically `Add` inside the goroutine instead
of before it launches). Go 1.24 added the `tests` analyzer, which flags malformed
test, fuzz, benchmark, and example declarations — including an `Example` that
documents an identifier that does not exist. The exit-code gap is the whole
reason vet is a separate required CI stage: `go build` returns 0 on the printf
bug while `go vet` returns non-zero. `go vet` also supports `-json` for machine
consumption and `-vettool` to plug in analyzers built on
`golang.org/x/tools/go/analysis`. It is the *floor*: unused variables, unchecked
errors, and shadowing need `golangci-lint` layered on top.

## go doc reads doc comments straight from the graph

`go doc` prints the documentation comments attached to declarations, resolved
through the package graph rather than by opening files. No argument shows the
current package overview; one argument is `package.Symbol` or a bare `Symbol` in
the current package; `-all` prints every declaration; `-src` prints the source of
one; `-u` includes unexported identifiers. It reads the standard library directly
from `$GOROOT` with no module present, so `go doc sync.WaitGroup` works from any
directory. Go 1.25 added `go doc -http`, which starts a local documentation
server and opens it in a browser — the built-in equivalent of pkgsite for
browsing your own workspace offline. Because documentation lives in comments that
travel with the code, `go doc` is always in sync with what you actually shipped.

## Testable examples are compiler-checked documentation

An `ExampleXxx` function with a trailing `// Output:` comment is executed by
`go test`, which compares its stdout against the comment; a
`// Unordered output:` comment compares the lines as a set, for output whose order
is not deterministic (a map range, for instance). These examples are also
rendered by `go doc` and pkgsite. The payoff is documentation that cannot rot:
if the code changes and the example's output drifts, the test suite fails. The Go
1.24 `tests` analyzer additionally catches an example named after a symbol that
no longer exists, so a rename does not leave dangling docs.

## go env is the toolchain's configuration surface

`go env` prints the settings that govern every build: `GOOS`, `GOARCH`,
`GOFLAGS`, `CGO_ENABLED`, cache locations, and dozens more. `go env -json` emits
them as JSON for machine parsing. `go env -w NAME=VALUE` persists a default into
the user's env file, and `go env -u NAME` removes it. The highest-leverage flag
for operations is `go env -changed` (Go 1.24): it prints only the settings whose
effective value differs from a clean default. When a CI runner behaves
differently from a developer's machine, diffing `go env -changed` on both is the
fastest way to isolate the one setting that diverges, instead of eyeballing the
entire environment.

## Cross-compilation is GOOS, GOARCH, and an authoritative matrix

Go cross-compiles by setting `GOOS` and `GOARCH` on the build command; adding
`CGO_ENABLED=0` produces a static, pure-Go binary with no libc dependency, which
is what you want for a scratch or distroless container image. The authoritative,
always-current list of supported target pairs is `go tool dist list` — the
release matrix should be derived from it, not from a hand-maintained list that
rots as new pairs are added. `file` on the resulting binary confirms the target
(an ELF for `linux`, a Mach-O for `darwin`).

## go list is the programmable view of the graph

`go list` exposes the graph to scripts. `go list ./...` enumerates the module's
packages. `go list -deps ./cmd/demo` walks the transitive import set
depth-first — every package the command ultimately pulls in, standard library
included. `go list -json` dumps a rich record per package (`ImportPath`,
`Imports`, `Deps`, `GoFiles`, `Stale`), and `-f` runs a `text/template` over that
record so you can print exactly the fields you need. In module mode,
`go list -m all` lists the module dependency graph, `go list -m -u all` marks
which have newer versions available, and `go list -m -retracted` surfaces
retracted versions. Together these are the basis of dependency-audit and
import-guard scripts: a build that fails if a forbidden package ever appears in
the transitive graph is a few lines of `go list` plus a grep.

## Release binaries are configured at link time

A shipped binary should be reproducible and self-describing. Three link-time
levers do this. `-ldflags "-X importpath.name=value"` injects a string into a
package-level `var` at link time — the standard way to stamp a build with its
version, commit SHA, and date without a code change. `-trimpath` removes absolute
filesystem paths from the binary, which both scrubs a build-machine path leak and
makes the build reproducible across machines. `-ldflags "-s -w"` strips the
symbol table and DWARF debug info to shrink the artifact. On the other side,
`go version -m binary` reads the `runtime/debug.BuildInfo` that the toolchain
embeds in every binary — module path, version settings, and the `vcs.*` stamps —
back out of any deployed artifact, and Go 1.25's `go version -m -json` emits it
as JSON. Ops can query what a running binary actually is without asking the
author.

## The build cache is content-addressed

Incremental builds are fast and correct because the cache is keyed by the content
of inputs, not timestamps: change nothing, rebuild nothing. `-a` forces a full
rebuild and `go clean -cache` empties the cache. Reaching for a blanket `-a` in
CI to "fix" a stale result is cargo-culting; it defeats the cache and slows every
build. `go clean -cache` is the right tool, and only when you have actual
evidence of cache corruption, which is rare.

## The senior deliverable is one reproducible gate

The end state that a senior owns for a repository is a single, network-free
script that composes these commands with correct exit-code propagation:
`test -z "$(gofmt -l .)"`, then `go vet ./...`, then `go build ./...`, then
`go test -count=1 -race -shuffle=on ./...`, then an import guard built on
`go list`. It fails on the first violation, names the stage that failed, and is
identical on a laptop and in CI. The final exercise builds exactly that.

## Common Mistakes

### Running go run main.go for anything real

Single-file mode drops sibling and build-tagged files and bypasses the module
layout, so it compiles a different program than the one you ship. Use
`go run ./cmd/demo`.

### Treating go vet output as advisory

Real bugs — a printf type mismatch, a copied lock, an IPv6-unsafe address, a
misplaced `WaitGroup.Add` — pass the compiler and fail only in production. Wire
`go vet ./...` as a required CI stage, not a suggestion.

### Assuming go vet is a complete linter

It is the floor. Unused variables, unchecked errors, and shadowing need
`golangci-lint` layered on top. Vet clean does not mean lint clean.

### Running a command with no package argument

A bare `go vet` or `go build` from the module root only sees the current
directory and silently skips subpackages. Always pass `./...`.

### Hand-rolling a formatter that wraps gofmt

The output drifts from the ecosystem and creates noisy diffs. `gofmt` is
canonical; `goimports` is the only sanctioned extension.

### Comparing floats with == in tests

Floating-point results are not exact; assert with an epsilon
(`math.Abs(got-want) > eps`). Assert sentinel errors with `errors.Is`, never with
`==` on the string or a substring match.

### Expecting go run to surface the program's exit code

`go run` flattens any non-zero exit to `1`. Build the binary and run it directly,
checking `$?`, to observe an `os.Exit(2)`.

### Building addresses with fmt.Sprintf("%s:%d", host, port)

This breaks for IPv6 literals, which contain colons of their own. Use
`net.JoinHostPort`; the Go 1.25 `hostport` analyzer now flags the anti-pattern.

### Shipping release binaries without -trimpath or -ldflags -X

Without `-trimpath` the artifact embeds absolute build-machine paths (a leak and
a reproducibility break); without `-ldflags -X` the deployed binary has no
queryable version or commit, so ops cannot tell what is running.

### Adding blanket -a to CI builds

This defeats the content-addressed cache instead of understanding it. Reach for
`go clean -cache` only when you have proven cache corruption.

### Maintaining a hand-written cross-compile target list

The list rots as new `GOOS/GOARCH` pairs land. Derive the matrix from
`go tool dist list`.

### Debugging a broken CI runner by dumping the whole go env

The full dump buries the signal. `go env -changed` prints only the non-default
settings and isolates the divergence immediately.

Next: [01-package-graph-and-package-mode.md](01-package-graph-and-package-mode.md)
