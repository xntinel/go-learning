# Monorepo Module Strategy in a Multi-Service Repo — Concepts

A senior backend engineer eventually owns a decision that every service in the
org inherits: the repository topology. One module or many? When do you cross
that line? Get it wrong in the simple direction and every team is forced into a
lockstep release; get it wrong in the complex direction and you pay tag
discipline, Minimal Version Selection reconciliation, and workspace tooling for
independence nobody asked for. This lesson frames the monorepo as a real
platform repo — shared platform libraries plus several deployable services — and
drives the operational failure modes that actually page you: a local `replace`
that leaks into a tagged release and breaks the build, an `internal/` boundary
that silently lets another team reach into your private package, two modules
drifting to different dependency versions under MVS, and the tag-prefix ritual
for a module that lives in a repo subdirectory. Read this once and you have the
model for the ten independent exercises that follow.

## Concepts

### One module vs many is a release-cadence decision, not a size decision

The instinct is to split a repo into many modules once it feels "big". That is
the wrong axis. Line count, number of packages, and number of services do not
force multi-module. The only thing that does is the need for **independent
release cadence**.

A single module gives you one atomic version for the whole repo, zero
cross-module coordination, and trivially correct local builds: every package
imports every other by a stable path, `go build ./...` compiles the world, and
there is exactly one `go.mod` to reason about. The price is lockstep: everything
ships together under one version tag. If service A needs a hotfix, the tag moves
for service B too, whether or not B changed.

Many modules buy independent version numbers and independent cadence — team A
tags `api/v1.4.0` while team B stays on `worker/v1.1.0`. You pay for that with
tag discipline, MVS reconciliation across modules, and a workspace to develop
against local code. Choose multi-module when teams genuinely need to release on
their own schedule. Until then, one module is strictly simpler and strictly
correct, and "we might need it later" is not a reason to pay now.

### A go.work workspace makes local modules the main modules

A `go.work` file makes several local modules the *main modules* of a build
without editing any of their `go.mod` files. When `go.work` is in effect the go
command is in **workspace mode**, and it resolves each member module's import
path to its local directory. That is the key property: a service that imports
`example.com/mono/platform` finds the sibling `platform/` directory with **no
`replace` directive** in its own `go.mod`.

You detect workspace mode with `go env GOWORK` — non-empty means a workspace is
active, and it prints the path to the `go.work` file. The subcommands you will
actually use: `go work init` creates the file, `go work use ./dir` adds a
member, `go work edit` scripts edits, `go work sync` pushes the MVS result back
into members, and `go work vendor` (Go 1.22+) builds one vendor tree for the
whole workspace. The go command also maintains a `go.work.sum` alongside
`go.work` for checksums not already covered by the members' individual `go.sum`
files.

### go.work must live at the repo root — and must never leak into a release

Two rules govern where `go.work` lives and what it may touch. First, it must sit
at the **repo root** so that every go command run anywhere in the tree discovers
it by walking up the directory tree. A `go.work` placed in a subdirectory is
invisible to commands run in a parent directory, which silently fall back to
non-workspace resolution — the build "works" for you and fails for the CI job
that runs one level up.

Second, and more dangerous: `go.work` is a **development tool** and must never
influence a production or release build. A released module has to build on its
own from the module proxy, with no workspace and no `replace` pointing at a
local path. The classic leak is `replace example.com/mono/platform => ../platform`
committed into a service's `go.mod`: it resolves fine on your laptop through the
sibling directory and breaks the instant a tagged release is built somewhere
that directory does not exist. Treat both a committed `go.work` and any
filesystem `replace` as release blockers, and guard against them in CI.

### The internal/ rule is a compiler-enforced boundary, not a convention

A package whose import path contains an `internal/` element may be imported only
by code rooted at the directory that is the *parent* of that `internal/`. This
is enforced by the compiler, not by code review. In a monorepo it gives teams a
hard, tooling-backed boundary:

- A `internal/` at the **repo root** (`example.com/mono/internal/...`) is shared
  by every package in that module — it is private to the module but common to
  all its services.
- A **service-local** `internal/` (`example.com/mono/api/internal/store`) is
  private to that one service. Another service that tries to import
  `example.com/mono/api/internal/store` does not compile: "use of internal
  package not allowed".

That single rule replaces an entire class of "please don't import our guts"
conventions with a build error. Put a package under the right `internal/` and
the boundary enforces itself.

### Minimal Version Selection, and how a workspace hides drift

Go's Minimal Version Selection picks, for each dependency, the **maximum of the
minimum versions** required across the build — the highest version anyone in the
graph asks for, and no higher. It is deterministic and needs no lockfile.

A workspace spanning multiple modules runs MVS over the **union** of the
members' requirements. That has a subtle consequence: inside the workspace a
member can build against a *higher* version of a shared dependency than it would
select standalone, because a sibling module required that higher version. The
service builds and tests green in the workspace, then fails or resolves a
different version when CI builds it alone. `go work sync` fixes this by writing
the reconciled workspace-wide MVS result back into each member's `go.mod`, so a
later standalone build agrees with the workspace build. Run it after changing any
member's dependencies.

### Subdirectory modules tag with a prefix, and /v2 changes the import path

A module that lives in a subdirectory of a repo does not tag bare `v1.4.0`. Its
release tags carry the **subdirectory as a prefix**: a module at
`example.com/mono/platform` tags `platform/v1.4.0`. The go command knows to look
for `platform/vX.Y.Z` tags when resolving that module path. This mirrors the
standard library ecosystem — `golang.org/x/tools/gopls` tags `gopls/vX.Y.Z`.

A breaking change is not just a bigger number. Under **semantic import
versioning**, major version 2 and above must appear as a suffix in the module
path itself: `example.com/mono/platform/v2`, declared in the `module` directive
of `platform/go.mod`, tagged `platform/v2.0.0`. Importers change their import
path to `.../platform/v2` to move to it, so v1 and v2 can coexist in one build.
The invariant to internalize: the tag prefix must match the subdirectory and the
`/vN` suffix in the module path must match the major version of the tag, or
`go get` resolves the wrong thing (or nothing).

### Hermetic CI: per-module vendor vs whole-workspace vendor

Vendoring copies dependencies into the repo so a build consults only the
vendored copies and never the network. With `-mod=vendor` (the default when a
`vendor/` directory is present) the build is hermetic and reproducible, and
`vendor/modules.txt` records the exact module set.

Two forms exist. Per-module `go mod vendor` produces a `vendor/` tree for one
module. `go work vendor` (Go 1.22+) produces a **single** `vendor/` tree at the
workspace root covering every module in the workspace at once. For a monorepo
built as a workspace, `go work vendor` plus `GOFLAGS=-mod=vendor` in CI gives one
hermetic tree for all services; per-module vendoring fits when each module is
released and built independently. Pick the one that matches how CI actually
builds the code.

## Common Mistakes

### Committing go.work or a local replace so it leaks into release

The workspace and filesystem `replace` directives are development-only. A tagged
release must build from the proxy with no `replace` pointing at a local path.
Wrong: committing `replace example.com/mono/platform => ../platform` into a
service's `go.mod` "so it works locally". Fix: use `go.work` for local
development, tagged versions for release, and reject relative-path replace
targets in CI.

### Putting go.work in a subdirectory

Wrong: a `go.work` under `services/` in the monorepo. Commands run from the repo
root do not see it and silently resolve modules the non-workspace way. Fix: the
`go.work` lives at the repo root so every command in the tree discovers it.

### Using replace for local dev instead of a workspace

Wrong: `replace example.com/mono/platform => ../platform` in a service to develop
against local library code. It works on your machine and breaks the production
build. Fix: that is exactly what `go.work` is for — a workspace resolves the
local module with no `replace` at all.

### Splitting into many modules before you need independent cadence

Wrong: splitting a repo into modules because it is large. Multi-module adds tag
discipline, MVS reconciliation, and workspace management. Fix: stay single-module
until teams actually need to release independently; size is not the trigger,
cadence is.

### Tagging a subdirectory module wrong

Wrong: tagging a subdirectory module bare `v1.4.0` instead of `platform/v1.4.0`,
or shipping a breaking change without the `/v2` suffix in the module path. Fix:
the tag prefix must match the module's subdirectory, and any major version ≥ 2
must appear both as a `/vN` suffix in the `module` directive and in the tag.

### Assuming a service-local internal package is reachable

Wrong: importing `example.com/mono/api/internal/store` from the worker service.
The internal rule is enforced by the compiler based on the parent of `internal/`,
so a cross-service import of a service-local internal package does not compile.
Fix: promote genuinely shared code to a repo-root `internal/` (or a public
package); keep service-private code under the service's own `internal/`.

### Forgetting go work sync after changing a member's dependency

Wrong: bumping a dependency in one module and relying on the workspace build to
"just work". The workspace's union MVS masks the drift; the standalone CI build
of another member resolves a different version and fails. Fix: run `go work sync`
after changing any member's requirements so standalone builds agree with the
workspace.

Next: [01-shared-platform-library.md](01-shared-platform-library.md)
