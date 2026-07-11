# Go Workspace and Project Layout — Concepts

A Go project can lay its source out almost any way its authors like. The
toolchain enforces exactly one structural rule about directories — the
`internal/` rule — and treats everything else (`cmd/`, `pkg/`, however deep you
nest) as convention for humans, not law for the compiler. A senior engineer does
not stop at knowing those directory names exist; they treat layout as an
*enforceable contract*: which packages are private, which import edges are legal,
which files compile on which target, and which modules resolve locally versus
from a proxy. This file is the conceptual foundation for that view. Read it once
and you have what you need to reason through each of the ten independent
exercises that follow, which turn every one of these ideas into runnable,
asserted code — including a real architecture-fitness test and a hermetic
workspace-vendor build, the two things that actually bite backend teams.

## Concepts

### `internal/` is the one directory the compiler treats specially

`internal/` is the single directory name the Go toolchain reads as a rule rather
than a suggestion. A package whose import path contains `.../internal/...` may be
imported only by code rooted at the *parent* of that `internal/` directory. A
package at `github.com/example/myapp/internal/greeting` is importable by anything
under `github.com/example/myapp`, and by nothing else. Any other module that
tries gets a compile error: `use of internal package ... not allowed`. This rule
was added in Go 1.4 and is enforced by the compiler, not by review. Every other
layout name — `cmd/`, `pkg/`, `api/`, `service/` — is convention: the toolchain
does not know or care what they mean.

There is a crucial limit to internalize: the `internal/` rule is enforced
*across module boundaries*, but it says nothing about architectural layers
*within* one module. Everything under `myapp/` can freely import everything else
under `myapp/internal/`, including `internal/core` importing `internal/adapters`
the wrong way round. The compiler will not stop that. If you want layering
enforced inside a module, you write a guard test (a later exercise does exactly
that); you do not get it from `internal/` for free.

### The import path is the directory tree

Go packages are directories, and an import path mirrors the filesystem nesting
verbatim. `internal/services/greeting/v1/handlers/greeting.go` becomes the import
`github.com/example/myapp/internal/services/greeting/v1/handlers` — a path every
caller must type and every stack frame must print. Go favors wide, flat package
hierarchies. A subdirectory should earn its place with a distinct identity: a
separate binary, a genuinely different API surface, a real abstraction boundary.
`internal/greeting` covers the overwhelming majority of cases; the deep tree
above is a smell, not sophistication. Flat beats deep.

### `cmd/<name>` holds one thin `main` per binary

The community layout puts each executable in its own directory under `cmd/`, one
`package main` per binary, and keeps that `main` thin. `main()` wires the outside
world — reads `os.Args`, picks streams (`os.Stdout`/`os.Stderr`), decides the
process exit code — and immediately delegates to a `run(args, stdout, stderr)`
function or a library call whose logic lives in an importable, testable package.
The payoff is testability: you can unit-test the whole command by calling `run`
with crafted arguments and `bytes.Buffer` writers, asserting both the returned
error and the exact bytes written, without ever spawning a process. Logic stuck
inside `main()` can only be tested by exec-ing the binary, which almost always
means it is not tested at all.

### `pkg/` is a promise, not decoration

Putting code under `pkg/` signals "this is stable, public API, meant to be
imported by other modules." That is a commitment: the first external importer
freezes the surface, because you cannot change it without breaking them. Default
to `internal/`, which keeps the option to refactor freely. Reach for `pkg/` only
when you have deliberately decided to support external consumers. Small projects
need no `pkg/` at all. Dropping code into `pkg/` because it "looks official" is
how a private helper accidentally becomes a public contract.

### Nested `internal/` subtrees encode architecture

Inside a real backend service, the `internal/` subtree carries the architecture.
A hexagonal (ports-and-adapters) layout looks like `internal/core` — domain types
plus the port *interfaces*, importing zero infrastructure — with
`internal/adapters` depending *inward* on `core` to implement those ports, and
`internal/platform` for cross-cutting helpers (logging, config). The direction of
the import edges *is* the architecture: `core` must never import `adapters`.
Dependency inversion is expressed by `core` defining an interface and `adapters`
satisfying it, so `core` compiles with no knowledge of any database or HTTP
client. Flat-but-layered beats deep here too: three or four sibling packages
under `internal/` with a clear dependency direction are easier to police than a
towering tree.

### `./...` expands to packages, respecting the real build

Most `go` subcommands take package patterns, and `./...` means "this directory
and every package below it, recursively." It expands to *packages*, not files,
and it honors build constraints and `_test.go` handling exactly as the real build
does. Use it on every subcommand — `go build ./...`, `go vet ./...`,
`go test ./...` — so CI never silently skips a subdirectory. A bare `go build`
with no argument operates only on the current directory; a broken subpackage then
sails through CI green.

### Build constraints make layout itself conditional

Which `.go` files compile is not fixed — it depends on the target and on tags.
Two mechanisms decide it, and both are filename/comment rules, not code. A
`//go:build linux` line at the top of a file (before `package`, followed by a
blank line) gates that whole file on a boolean constraint expression. And a
filename *suffix* — `sysinfo_linux.go`, `store_darwin.go`, `fast_amd64.go` — is an
implicit constraint on `GOOS`/`GOARCH`, applied purely from the name. The `_test`
suffix is the same kind of structural rule. So file naming is part of the project
structure: `sysinfo_linux.go` and `sysinfo_darwin.go` provide the same symbols per
platform, and only one compiles for a given target. `go list -tags` and
`go/build`'s `Context.ImportDir` let you see and assert exactly which files a
given target selects.

### `go.work` is a per-checkout override

Sometimes you need to develop two modules in lockstep — a library and a tool that
consumes it — without publishing an intermediate version of the library after
every edit. `go.work` declares a workspace listing several modules with `use`
directives; inside it, imports resolve to those local working copies as if they
were the published versions. `go work init ./a ./b` creates it; `go work use ./c`
adds a module; `go env GOWORK` tells you whether workspace mode is active and
which file is in effect (empty means off). No `replace` directive is needed, and
no individual `go.mod` is touched.

### `go.work` versus `replace`

Both override resolution, but they differ in blast radius. A `replace` directive
lives in a module's `go.mod`, is committed, and affects *everyone* who builds that
module. `go.work` is a per-developer, usually-uncommitted override that affects
only your checkout. Commit `go.work` only when *every* checkout genuinely needs it
— a true monorepo where all modules are always built together. For a personal
fork or a short-lived local experiment, leave it out of version control:
committing a `go.work` that lists only the modules you happen to have on disk
hands every other developer and CI a build list that does not match their tree.

### The build list, sync, and vendor

`go work sync` reconciles the workspace's combined build list back into each
module's `go.mod` via Minimal Version Selection (MVS), so the individual modules
agree on dependency versions. `go work vendor` (Go 1.22+) then writes a single
workspace-level `vendor/` directory next to `go.work`, containing every external
package needed to build and test the workspace, plus a `vendor/modules.txt`
manifest. With that committed, CI builds hermetically: `go build -mod=vendor ./...`
(optionally with `GOPROXY=off`) succeeds with no module cache and no network. This
is exactly how platform teams make monorepo pipelines reproducible and offline. It
is workspace-scoped: `go work vendor` errors outside a workspace and writes one
`vendor/` beside `go.work`, not one per module the way `go mod vendor` does.

### The build graph is queryable

Layout invariants do not have to live in a wiki. The build graph is data you can
assert on. `go list` with `-f` templates, `-json`, and `-deps` enumerates
packages, their kinds, and their dependency closures; `go/build`'s
`Context.ImportDir` returns a `*build.Package` whose `.Imports`, `.GoFiles`, and
`.TestImports` expose the exact edges and per-target file selection. That is what
turns "core must not import adapters" or "the module has exactly these four
packages" from a code-review hope into an automated test that fails the build when
someone crosses the line.

## Common Mistakes

### Dumping every file at the module root in `package main`

Wrong: every `.go` file at the module root, all `package main`. You can then never
have a second binary, library code cannot be reused or tested in isolation, and
every change recompiles the world. Fix: one `cmd/<name>` directory per binary,
shared code under `internal/`.

### Creating `pkg/foo` for code only your module uses

Wrong: moving `internal/foo` to `pkg/foo` because it "looks official." You have
just advertised a stable public API, and the first importer holds you to it. Fix:
default to `internal/`; reach for `pkg/` only with a deliberate external-consumer
decision.

### Deep trees like `internal/services/greeting/v1/handlers`

Wrong: nesting for its own sake. The import path becomes long, repetitive, and
typo-prone, and it bloats stack traces. Fix: keep it flat; `internal/greeting`
covers almost everything.

### Running bare `go build` / `go vet` / `go test`

Wrong: no package argument, so the command touches only the current directory and
silently skips every subpackage. CI stays green while a subdirectory is broken.
Fix: always `./...`.

### Putting all the logic inside `main()`

Wrong: the command's behavior lives in `main()`, so nothing is testable without
exec-ing the binary — which means it is untested. Fix: `main()` only wires; a
`run(args, stdout, stderr)` (or a library call) holds the logic and gets the
tests.

### Assuming the `internal/` rule polices layers inside a module

Wrong: trusting `internal/` to stop `internal/core` from importing
`internal/adapters`. Cross-module, `internal/` is compiler-enforced; the *layers
inside one module* are not enforced by it at all. Fix: add an architecture guard
test that reads each package's imports and fails a forbidden edge.

### Committing a personal `go.work`

Wrong: committing a `go.work` that lists only the modules you happen to have
locally. Every other developer and CI then gets a build list that does not match
their checkout. Fix: commit `go.work` only for a monorepo every checkout needs;
otherwise keep it untracked.

### Expecting `go work vendor` to behave like per-module `go mod vendor`

Wrong: running it outside a workspace, or expecting one `vendor/` per module. It
errors without active workspace mode and writes a single `vendor/` beside
`go.work`. Fix: run it from the workspace root and build with `-mod=vendor`.

### Naming a file `main_linux.go` or `handler_test_amd64.go` and being surprised

Wrong: treating `_linux`, `_amd64`, and `_test` suffixes as cosmetic. They are
structural filename rules: `GOOS`/`GOARCH` suffixes exclude the file on other
targets, and `_test.go` files never ship in the package build. Fix: learn the
suffix semantics before naming files, and verify with `go list -tags` / `go build`
for the target.

Next: [01-shared-internal-packages.md](01-shared-internal-packages.md)
