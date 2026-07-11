# Multi-Module Workspaces In A Monorepo — Concepts

A service platform is rarely one module. There is a shared domain/platform
library and the several services that consume it, and on any given day you are
editing the library and a service at the same time. Without workspaces you have
two bad options: publish and tag the library after every tiny change so the
service can `require` the new version, or scatter `replace ../lib` directives
across every service's `go.mod` and hope you never commit one. A `go.work` file
is the mechanism that removes both: it declares a set of on-disk modules as the
*main modules* of a local build, so a service resolves the sibling library from
your working tree instead of the proxy, with nothing tagged and no `replace`
polluting any committed `go.mod`.

The senior skill is not typing `go work init`. It is operating the workspace
safely: knowing that `go.work` is a development-only overlay that CI must not
see, keeping the workspace's selected versions in sync with each module's
committed `go.mod`, reproducing the CI build locally, and understanding the hard
build boundary that makes `go test ./...` silently skip sibling modules. Read
this file once; it carries everything the ten independent exercises need.

## Concepts

### A workspace is a `go.work` file listing on-disk modules

A workspace is declared by a `go.work` file whose `use` directive names
directories, each of which contains its own `go.mod`. Those modules become the
*main modules* of the build. During Minimal Version Selection (MVS) their code is
taken directly from disk before any proxy lookup, so an import of the sibling
library resolves to your working copy — edits are visible immediately, with no
tag, no publish, and no `require`-version bump. A single-repo platform typically
has one `go.work` at the root listing `./platform/text`, `./services/greeter`,
and so on.

### go.work has exactly five directives

`go`, `toolchain`, `use`, `replace`, and `godebug`. The `go` directive is
required and names the workspace language version. `use` lists the module
directories. `replace` behaves like a `go.mod` replace, except a *wildcard*
replace in `go.work` (no version on the left) overrides a version-specific
replace in an individual `go.mod` — the workspace wins. `godebug` sets GODEBUG
values for the whole workspace, and while a workspace is active the `godebug`
directives in individual `go.mod` files are *ignored* in favor of `go.work`'s.
`toolchain` suggests a toolchain when the default is older.

### The overlay is development-only; whether to commit it depends on topology

`go.work` overrides normal version resolution, so committing it is a topology
decision, not a default. In a single repository whose modules are developed and
released together, committing `go.work` lets everyone share one layout. In a
multi-repo setup it must be gitignored: a committed `go.work` forces every
consumer and every CI job into a local module layout they do not have, and the
build breaks because it can only see published versions. The rule of thumb: a
released artifact must depend on a tagged, published version; `replace` and
workspace overlays are for local iteration on a fork or an unreleased sibling.

### GOWORK controls workspace mode

Unset, the `go` command walks up from the working directory looking for a
`go.work`; if found, workspace mode is on. `GOWORK=off` disables the workspace
entirely — single-module mode, which is exactly what a CI runner sees.
`GOWORK=/abs/path.work` selects an explicit file. `go env GOWORK` reports the
active file (empty when off or none found). These two states — active overlay
versus `GOWORK=off` — are the local and CI builds, and the whole discipline is
keeping them producing the same graph.

### The `go work` subcommands

`go work init [dirs]` writes a `go.work` with a `go` line and a `use` entry per
directory. `go work use [-r] [dirs]` adds module directories (with `-r` it
recurses to find nested modules); passing a directory that no longer contains a
module removes its `use` entry. `go work sync` writes the workspace's
MVS-selected dependency versions back into each module's `go.mod`, so the next
`GOWORK=off` build selects the identical graph. `go work edit` does low-level
scripted edits (`-use`, `-dropuse`, `-replace`, `-dropreplace`, `-go`, and the
read-back flags `-print` and `-json`) — the deterministic way for monorepo
tooling to regenerate the file. `go work vendor` builds a workspace vendor
directory. A `go.work.sum` records checksums for workspace dependencies not
already covered by the individual modules' `go.sum` files.

### Module boundaries are hard build boundaries

`go build`, `go test`, and `go vet` with the `./...` pattern stop at the first
nested `go.mod`; they never recurse into a sibling module. A `go test ./...` run
from the workspace root therefore exercises exactly one module and silently skips
every other module in the `use` list. Multi-module CI must enumerate the modules
(from `go.work`) and run the test-and-vet sweep inside each one; trusting a
single root `./...` is how a broken module ships while the gate reports green.

### Why "works on my machine, fails in CI" happens — and the two fixes

Because the workspace can resolve an uncommitted or unpublished local version, a
build can succeed on your machine and fail in CI, which sees only committed
`go.mod` versions and no `go.work`. The service may compile only because it is
reading a symbol you added to the local library but have not published, or a
version MVS picked from the workspace that no module actually requires. Two tools
close the gap: `go work sync` reconciles each `go.mod` with the
workspace-selected versions, and `GOWORK=off` reproduces the CI resolution
locally so you catch the failure before pushing.

## Common Mistakes

### Forgetting to add a module to `use`

Wrong: a service imports the sibling library, but `go.work` never lists the
library's directory, so the import resolves through the proxy (or fails) instead
of the intended local copy.

Fix: `go work use ./path/to/lib`, or list it under `use`. Confirm with
`go list -m all`, where a workspace main module appears with no version.

### Committing `go.work` in a multi-repo setup

Wrong: a `go.work` checked into a repository whose consumers do not have the
sibling modules on disk. Every consumer and CI job is forced into a layout they
cannot satisfy and the build breaks.

Fix: gitignore `go.work` in a multi-repo setup. Committing it is only defensible
inside a single repository whose modules are released as a unit.

### A permanent `replace` pointing at a local path

Wrong: a `replace => ../lib` left in a module's `go.mod` and shipped in a
release. A downstream consumer of your module cannot resolve `../lib`.

Fix: `replace` and workspaces are development overlays. A released build depends
on a tagged, published version; use the overlay only while iterating.

### Assuming `go test ./...` from the root covers every module

Wrong: gating a multi-module monorepo with one `go test ./...` at the workspace
root. It stops at the first module boundary and skips the siblings, so a broken
module passes CI.

Fix: enumerate the modules from `go.work` and run `go test -race` and `go vet`
inside each one.

### Letting workspace-selected versions drift from `go.mod`

Wrong: the workspace's MVS selects a higher version of a shared dependency than a
lagging service's `go.mod` records, so the local build differs from CI's.

Fix: run `go work sync` to write the selected versions back into each `go.mod`,
then verify with a `GOWORK=off` build.

### Never testing with `GOWORK=off`

Wrong: shipping a service that compiles only because the local workspace overlay
supplies an unpublished symbol or version. CI is the first place it fails.

Fix: run the parity build with `GOWORK=off` before pushing; the passing state is
"the committed versions build with the overlay disabled".

### Expecting a `go.mod` godebug to apply under a workspace

Wrong: adding a `godebug` line to individual `go.mod` files during a migration
while a workspace is active and expecting it to take effect. The `go.work`
`godebug` wins and the `go.mod` one is ignored.

Fix: set the migration's GODEBUG in `go.work`; put it back into each `go.mod`
only once the workspace is retired.

### Hand-editing `go.work` from a script

Wrong: a generator that string-appends `use` lines to `go.work`, producing
malformed files or nondeterministic diffs.

Fix: drive it with `go work edit -use`/`-dropuse` and read it back with
`-json`/`-print`, which is deterministic and always well-formed.

### Expecting `go.work replace` to affect a published release

Wrong: assuming a `replace` in `go.work` changes what a downstream consumer of
your released module sees. It only affects builds run inside the workspace.

Fix: patch across the workspace for the incident, then land a real fix (a tagged
dependency version) for the release.

Next: [01-shared-platform-library-module.md](01-shared-platform-library-module.md)
