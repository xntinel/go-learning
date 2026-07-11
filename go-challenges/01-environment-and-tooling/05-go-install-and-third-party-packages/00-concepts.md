# Go Install and Third-Party Packages — Concepts

A junior asks "how do I get `goimports`?" A senior owns the dependency and
tooling story for an entire service and its CI: which module versions the build
is pinned to, where they are fetched from when half of them live on a corporate
module server, how the build proves the bytes it compiled are the bytes the
author published, and how to reproduce yesterday's artifact byte-for-byte with
the network unplugged. `go install` and `go get` are the two commands that sit at
the center of that story, and everything around them — the `tool` directive,
`go.sum`, `GOPRIVATE`, `-mod=readonly`, `go mod vendor` — exists to make builds
deterministic and auditable. The connecting thread through this whole lesson is
one sentence: every artifact a build produces must be reproducible from a pinned,
verified source, whether it is a service binary or a code generator the team runs
in CI.

Read this once. The ten exercises that follow are independent modules; each
builds a small, real artifact and exercises one slice of the operational surface
described here.

## Concepts

### Two commands, two jobs: `go get` manages, `go install` compiles

Before Go 1.17 a single command, `go get`, did two unrelated things: it edited
your module's dependency requirements *and* it compiled-and-installed binaries
into your bin directory. Those jobs were split, and the split is now enforced.

- `go get` mutates `go.mod` and `go.sum`. It records that your module depends on
  some version of some other module. It never writes a binary.
- `go install pkg@version` compiles the named `main` package and writes the
  resulting binary to `GOBIN`. It never touches the current module's `go.mod`.

Using the wrong one leaves you with the wrong result and no error to explain why:
`go get golang.org/x/tools/cmd/goimports@latest` adds a `require` line and
produces no runnable tool; `go install` of a library package silently does
nothing useful because a library has no binary to build. The rule of thumb: if
you will `import` it from your code, `go get` it; if you will run it as a
command, `go install pkg@version` it.

### Why a version suffix is mandatory out of module

Inside a module, `go get some/dep` can resolve a version by walking the module
graph. Run `go install` or `go get` on a package that is *not* already in the
graph — the common case for a standalone tool — and there is no graph to walk,
so the `@version` suffix is the toolchain's only source of truth for which
release to build. Omit it outside a module and you get
`go.mod file not found in current directory or any parent directory`. The suffix
also decides reproducibility: `@latest` resolves to whatever is newest *at the
moment you run the command*, so two engineers running the same line a week apart
can get different binaries; `@v0.1.12` is reproducible; `@some-branch` yields an
unstable pseudo-version (a synthetic `v0.0.0-timestamp-commit`) that pins a
commit but carries no release guarantees.

### Where binaries land, where module source lives

`go install` writes to `GOBIN` if it is set, otherwise to `$GOPATH/bin` (default
`~/go/bin`). That directory must be on `PATH` for the shell to find the binary by
name — the single most common "the install worked but the command isn't found"
cause. Downloaded module *source* lives somewhere else entirely: `GOMODCACHE`
(default `$GOPATH/pkg/mod`), which is read-only by default so a stray write
cannot corrupt a shared cache; `GOFLAGS=-modcacherw` (or `go clean -modcache`
plus re-download) is how you deliberately touch it. Knowing these three
locations — `GOBIN` for binaries, `GOMODCACHE` for source, `go env` to print
either — is what lets you reason about "where did that come from" during an
incident.

### The `tool` directive: per-repo reproducible tooling (Go 1.24+)

Globally installed dev tools rot. Every teammate's `~/go/bin` drifts to a
different version of `goimports`, `stringer`, `mockgen`, and the day one person's
generator emits different output than another's, code review fills with phantom
diffs and CI disagrees with laptops. The pre-1.24 workaround was a `tools.go`
file with blank imports (`_ "golang.org/x/tools/cmd/stringer"`) that pinned the
tools as ordinary dependencies — a hack that abused the import graph.

Go 1.24 replaced it with a first-class `tool` directive in `go.mod`.
`go get -tool golang.org/x/tools/cmd/goimports` adds both a `require` (pinning
the version) and a `tool` line. From then on `go tool goimports` runs *that
pinned version* built from the module cache; `go tool` with no arguments lists
the pinned set; `go install tool` materializes all of them into `GOBIN` for a CI
image; `go get tool@upgrade` bumps them together; `go get some/tool@none` removes
the directive. The whole team, and CI, now run byte-identical tools straight from
a `git checkout`, with no global state.

### Direct versus `// indirect` requires

A `require` line carries a `// indirect` comment when *no* package in your main
module imports that module directly — it is present only because something you do
import depends on it. The marker is the toolchain's assertion about the import
graph, and `go mod tidy` keeps it honest: add a direct import and the comment
disappears; remove the last direct import and it reappears (or the line is
dropped if nothing needs it). When you need to know *why* a module is in your
graph at all, `go mod why -m path` names a chain from your code to it, and
`go mod graph` prints the full edge list. This is how you answer "who pulled in
this thing" before deleting it.

### `go.sum` and the checksum database: a trust boundary

`go.sum` is not a lockfile of versions (that is `go.mod`); it is a list of
cryptographic hashes — one `h1:` hash of each module's file tree and one of its
`go.mod` — computed exactly the way the checksum database computes them. On every
build the toolchain re-hashes what it fetched and compares against `go.sum`; a
mismatch aborts the build. The *first* time a version is ever downloaded there is
nothing in `go.sum` to compare against, so `GOSUMDB` (default `sum.golang.org`)
is consulted: a global, append-only, cryptographically-verifiable transparency
log that vouches for the hash. This is trust-on-first-use backed by a public
log — after the first download the hash is frozen in your `go.sum` and any later
tampering of the module cache is caught by `go mod verify`. Turning the checksum
database off globally (`GOSUMDB=off`) removes tamper detection for *every*
module, which is why the correct scope for a private module is `GONOSUMDB`/
`GOPRIVATE`, not a global kill switch.

### Private and corporate module servers

Public defaults assume every module is fetchable through `proxy.golang.org` and
verifiable through `sum.golang.org`. A private module is not, and leaking its
path to a public proxy is itself a disclosure. `GOPRIVATE` is a comma-separated
glob list that marks modules to *skip the public proxy and checksum database* and
fetch directly from version control. `GOPROXY` is the fetch chain itself — a list
like `https://proxy.corp,direct` where a comma means "fall through to the next
entry on a 404/410", a pipe (`|`) means "fall through on any error", and the
terminal words `direct` (go straight to VCS) and `off` (fail rather than reach
the network) end the chain. `GOINSECURE` permits plain HTTP or unverified TLS for
matching paths; `GOVCS` constrains which version-control systems may be used for
which paths. Crucially, the `go` command does not do authentication — that is
delegated to git, via `git config url."https://token@host/".insteadOf` or a
`.netrc` file. You configure all of this durably with `go env -w`, which writes
to the `go/env` file rather than relying on shell exports.

### Build-mode flags govern whether a build may mutate state

`-mod=readonly` is the default: a build that would need to add or change a
`require` fails loudly instead of silently editing `go.mod`. That is exactly what
you want in CI, where a surprise edit means the committed `go.mod` was wrong.
`-mod=mod` permits those edits (useful locally while adding a dependency).
`-mod=vendor` builds strictly from a committed `vendor/` directory and ignores
the module cache and network entirely. These are usually pinned once via
`GOFLAGS` in the CI environment so builds fail fast rather than drift.

### Hermetic and offline builds

The strongest determinism is a build that touches no network at all.
`go mod vendor` copies every dependency's source into `vendor/` and writes
`vendor/modules.txt` recording exactly which module@version each package came
from; building with `-mod=vendor` and `GOPROXY=off` then proves, by construction,
that the artifact was produced from the committed tree with the network unplugged
— reproducible and auditable from `git` alone. The alternative is a prefetched
module cache: `go mod download` populates `GOMODCACHE` in a single step that
makes an ideal cacheable Docker layer, so the expensive fetch happens once and
later builds reuse it. Both trade something — vendoring inflates the repo,
cache-warming needs a warm cache — for the same payoff: a build that does not
depend on the network being up or a proxy answering the same way twice.

### Binary provenance

`go version -m binary` and `runtime/debug.ReadBuildInfo` read metadata that the
toolchain embeds into every binary: the main module's path and version, every
dependency's version and hash, the build settings (`-race`, `GOOS`, compiler
flags), and — when built from a clean checkout with `-buildvcs=true` — the VCS
revision, time, and dirty flag. This is what lets you take a binary pulled off a
production host and trace it back to the exact commit and dependency set it was
built from. Provenance is the last link in the determinism chain: pin the source,
verify the hashes, build hermetically, and then prove after the fact what you
shipped.

## Common Mistakes

### Using `go get` to install a binary

Wrong: `go get golang.org/x/tools/cmd/goimports`. Since Go 1.17, `go get` only
manages dependencies: outside a module it errors on the missing `go.mod`, and
inside a module it adds a `require` line but produces no binary. Fix: use
`go install golang.org/x/tools/cmd/goimports@latest`.

### Omitting the version suffix out of module

Wrong: `go install golang.org/x/tools/cmd/goimports` from a directory with no
`go.mod`. It fails with `go.mod file not found ...` because there is no module
graph to resolve the version from. Fix: always pass `@latest` or a pinned
`@vX.Y.Z`.

### `GOPATH/bin` not on `PATH`

Wrong: the install succeeds but `which goimports` prints nothing. The binary sits
in `~/go/bin` and the shell cannot find it by name. Fix: add
`$(go env GOPATH)/bin` to `PATH` in your shell profile.

### Installing from a branch

Wrong: `go install some/tool@main`. It is accepted but resolves to an unstable
pseudo-version with no reproducibility guarantee. Fix: pin `@latest` (resolved
once) or, better, a concrete `@vX.Y.Z`.

### Relying on globally installed dev tools instead of the `tool` directive

Wrong: every teammate runs `go install golang.org/x/tools/cmd/stringer@latest`
whenever they remember to, and each `~/go/bin` drifts to a different version, so
generated code differs across machines. Fix: pin tools with `go get -tool` and
run them via `go tool`, so the version lives in `go.mod` and CI matches laptops.

### Disabling supply-chain checks globally to fix a private fetch

Wrong: reaching for `GOSUMDB=off` or `GOFLAGS=-insecure` because a private module
fails to fetch. That removes tamper detection for *all* modules. Fix: scope it —
`GOPRIVATE` (or `GONOSUMDB` for just those paths) tells the toolchain which
modules skip the proxy and checksum database while leaving public modules fully
verified.

### Committing `vendor/` but building without `-mod=vendor`

Wrong: a `vendor/` directory in the repo, but the build still reaches the network
because `-mod=vendor` was not set, and a stale `vendor/` silently diverges from
`go.mod`. Fix: set `-mod=vendor` (or `GOFLAGS=-mod=vendor`) and re-run
`go mod vendor` after every dependency change so the tree stays in sync.

### Expecting `go install` to edit `go.mod`

Wrong: assuming `go install pkg@upgrade` or `go install pkg` bumps a version in
`go.mod`. `go install` never edits the module; only `go get` (and `go get -tool`)
do. Fix: use `go get` when you want the version recorded, and `go install` only
when you want a binary produced.

Next: [01-greet-library-with-sentinel-error.md](01-greet-library-with-sentinel-error.md)
