# The vendor Directory: Hermetic Builds, Consistency Guards, and Supply-Chain Policy — Concepts

On a real backend, `vendor/` is not a beginner curiosity you toggle on because a
tutorial said so. It is a supply-chain and reproducibility control that a
platform team owns and operates. The interesting work is not "run `go mod
vendor`" — the go command does that in one line. The interesting work is the
machinery built *around* the vendored tree: the CI gate that fails a merge when
`vendor/` drifts from `go.mod`, the license and CVE denylist policies enforced
over the vendored source, the reproducible-build attestation that proves the
committed bytes still match the `h1:` hashes in `go.sum`, and the exact `-mod`
resolution rule that decides — sometimes surprisingly — whether your CI build
even reads `vendor/` at all. This file is the conceptual foundation. Read it
once and you have what you need for the nine independent exercises that follow,
each of which builds one piece of the build/release tooling a platform team
actually ships.

## Concepts

### What `vendor/` actually is, and why it exists

`vendor/` is a self-contained copy of the exact dependency *source* used by the
build, committed into your own version control. It exists for two reasons that
matter in production. First, hermeticity: with a vendored tree the build reads
no module proxy, needs no network, and touches no shared module cache. The build
is a pure function of the files in your repo, which is exactly what an
air-gapped, FedRAMP-style, or reproducibility-audited pipeline requires. Second,
reviewability: because the dependency source lives in-repo, a dependency bump
shows up as a concrete diff in a pull request. A reviewer can read the lines that
changed in a transitive package, and a security scanner can walk the tree for a
known-vulnerable code path. The source of truth is the VCS state of `vendor/`,
not whatever happens to be in a developer's cache.

### The automatic `-mod=vendor` rule (the one people get wrong)

The go command auto-selects vendoring under one precise condition, documented at
go.dev/ref/mod: if the `go` directive in `go.mod` is `1.14` or higher *and* a
`vendor/` directory is present at the module root, the command behaves as if
`-mod=vendor` were passed. Below `1.14`, or with no `vendor/` present, that
auto-enable does not fire.

The mode itself has three values. `-mod=mod` uses the module cache and may add or
update requirements in `go.mod`. `-mod=readonly` (the default for build commands
since Go 1.16) uses the cache but refuses to modify `go.mod`, failing instead
with a "missing go.sum entry" style error. `-mod=vendor` uses the vendored copies
and never consults the cache or network. The trap is that the automatic rule is
*only* a default: an explicit `-mod` flag, a `GOFLAGS=-mod=mod` in the
environment, or workspace mode each override it silently. "It built fine on my
laptop" and "it failed in CI" is very often this: one environment auto-selected
vendor, the other did not.

### `vendor/modules.txt` is a manifest the go command trusts

`vendor/modules.txt` is not decoration; it is the manifest the go command reads
to reconstruct the build. For each vendored module it records the module path and
version, a per-module `## go <version>` annotation (the *dependency's own* `go`
directive, kept so build constraints resolve the way they would from the cache),
and an `## explicit` marker distinguishing modules the main module imports
directly from purely transitive ones. A single logical entry looks like:

```text
# golang.org/x/mod v0.37.0
## explicit; go 1.23
golang.org/x/mod/modfile
golang.org/x/mod/semver
```

The `# path version` line opens a module; `## ...` lines annotate it; the bare
lines below list the packages actually vendored from it. A replace shows up as
`# path v1 => path2 v2`. Every downstream policy tool in this lesson begins by
parsing this file, because it is the in-repo inventory of exactly what was
vendored.

### The vendor consistency check (what "inconsistent vendoring" means)

When vendoring is active, the go command cross-checks `vendor/modules.txt`
against `go.mod` before it builds. If `go.mod` changed since the last `go mod
vendor` — a `require` added, removed, or bumped — the explicit set recorded in
`modules.txt` no longer matches the declared dependency set, and the build stops
with an "inconsistent vendoring" error rather than compiling stale source. This
is not an optional nicety; it is the mechanism that keeps the vendored tree
honest. A CI stale-vendor gate reimplements exactly this comparison: the explicit
`require` set in `go.mod` must equal the `## explicit` set in `modules.txt`, or
the tree is stale and the merge must fail.

### Vendoring prunes; `vendor/` is build-graph-derived

`go mod vendor` does not copy whole dependency modules. It copies only the
packages actually needed to build and test the main module's packages, plus
embedded files (`//go:embed`) and each module's `LICENSE`. It strips the
dependencies' own `_test.go` files. So `vendor/` is smaller than a full cache
extraction and its exact contents are a function of your build graph. This is
*why* a change to `go.mod` invalidates it: adding a dependency or a new import
changes the set of packages that must be present, and the recorded manifest no
longer describes the tree the current graph needs. A corollary that surprises
people: a package that exists in the module cache can be legitimately *absent*
from `vendor/` if nothing in your build imports it.

### Reproducible-build attestation with `dirhash`

`go.sum` stores `h1:` hashes computed by `golang.org/x/mod/sumdb/dirhash`. The
`Hash1` algorithm hashes a sorted list of `sha256(file)␠␠name` lines and base64
-encodes the result, prefixed with `h1:`. You can recompute the hash of a
directory tree with `dirhash.HashDir(dir, prefix, dirhash.Hash1)`, where `prefix`
is the `module@version` string, and compare it to a recorded value to prove the
bytes were not tampered with after they were produced. This is the exact
primitive behind `go mod verify`. One honest subtlety for vendored trees: because
vendoring prunes and strips test files, the hash of a *vendored* subtree is not
byte-identical to the module's `go.sum` entry (which covers the full cached
module). So the release-gate pattern is to capture the vendored tree's hash at
`go mod vendor` time into an attestation manifest and re-verify against that,
using the same `dirhash` primitive — not to expect the pruned tree to reproduce
the full-module `h1:`.

### Applications vendor; libraries do not

Commit `vendor/` for the deployable at the top of your build — the binary, the
service, the CLI. Do not commit it for a library. A consumer of your library
never uses your library's `vendor/`; the consumer's own build resolves your
library's imports through the consumer's module graph. Committing a library's
`vendor/` therefore buys nothing, bloats the repository, and hides the real
dependency edges from anyone reading the library's `go.mod`. Vendor at the root
of the build graph, never mid-tree.

### Go 1.24 tool directives are vendored too

Go 1.24 replaced the old `tools.go` blank-import hack with first-class `tool`
directives in `go.mod` (added via `go get -tool`). `go mod vendor` copies the
packages those tool directives name into `vendor/`, and `go tool` then runs the
vendored copy — which is what lets linters, mock generators, and codegen tools
run fully offline in a hermetic pipeline. The failure mode: if `go mod vendor`
was last run *before* a `tool` directive was added, that tool is missing from
`vendor/` and `go tool` silently falls back to the network. Auditing that every
`tool` directive is present and marked `## explicit` in `modules.txt` is a real
hermetic-dev-env check, and one of the exercises here builds it.

### Workspaces change which `vendor/` counts

In workspace mode (a `go.work` covering multiple modules), a top-level module's
`vendor/` is ignored — the auto `-mod=vendor` rule does not apply. Go 1.22 added
`go work vendor` to produce a single workspace-level `vendor/` for the whole
`go.work`. Before you trust `-mod=vendor`, know whether the build runs in module
mode or workspace mode, because the answer decides whether the vendored tree is
consulted at all.

### The trade-off, stated plainly

Vendoring buys hermeticity, offline and air-gapped builds, faster cold CI (no
proxy round-trips on a fresh runner), and in-repo dependency review. It costs
repository size, noisy diffs on every dependency bump, and a permanent obligation
to keep `vendor/` synced with `go.mod`. The modern alternative is
`GOFLAGS=-mod=readonly` plus an internal module proxy (Athens, JFrog, or the
public proxy with `GOSUMDB` verification) plus `go.sum` checking, which delivers
reproducibility without committing source. Vendoring wins specifically when the
network cannot be trusted at build time, or when reviewers must see dependency
source directly in pull requests. Choose it for those reasons, and then own the
gates that keep it correct.

## Common Mistakes

### Committing `vendor/` for a library

Wrong: a reusable library that commits `vendor/`. The consumer never reads it; it
only bloats the repo and hides the real dependency graph in the library's
`go.mod`.

Fix: vendor only the deployable application or CLI at the root of the build.
Libraries ship a `go.mod`/`go.sum` and let consumers resolve.

### Forgetting `go mod vendor` after a `go.mod` change

Wrong: running `go get` or hand-editing a `require`, then hitting "inconsistent
vendoring" at build time because `vendor/modules.txt` still describes the old
dependency set.

Fix: re-run `go mod vendor` and commit the regenerated tree in the *same* change
as the `go.mod` edit. Any change to `require` lines invalidates the manifest.

### Using `-mod=vendor` in CI without a fully committed `vendor/`

Wrong: forcing `-mod=vendor` in CI while a stray `.gitignore` keeps part of
`vendor/` (or `modules.txt`) untracked. The build fails because vendored copies
are missing.

Fix: ensure the entire `vendor/` tree, including `modules.txt`, is tracked. The
whole point is that the committed bytes are the build input.

### Assuming `-mod=vendor` is always on

Wrong: believing the auto rule always fires. A `GOFLAGS=-mod=mod`, an explicit
`-mod` flag, a `go` directive below `1.14`, or workspace mode each silently
switches the build back to the cache.

Fix: make the effective mode explicit in CI, and know the resolution rule
(`go >= 1.14` AND `vendor/` present AND not workspace mode ⇒ auto `-mod=vendor`).

### Hand-editing `vendor/modules.txt` or vendored source

Wrong: patching a dependency by editing files under `vendor/` or tweaking
`modules.txt`. This desyncs the manifest from `go.mod` and breaks the consistency
check.

Fix: use a `replace` directive (to a fork or a local path) and re-vendor. The
generated tree is the only supported source of `vendor/`.

### Trusting `vendor/` for reproducibility without verifying it

Wrong: assuming committed vendored bytes are automatically trustworthy. Committed
is not the same as verified; the bytes can be altered after generation.

Fix: attest the tree with a `dirhash` comparison against a recorded hash (the
`go mod verify` primitive), and run that attestation as a release gate.

### Expecting whole dependency modules under `vendor/`

Wrong: treating a missing package as a bug because "the module is a dependency."
Vendoring is build-graph-pruned and strips dependency test files.

Fix: expect only the packages your build actually imports, plus embeds and
`LICENSE`. A package absent from `vendor/` but present in the cache is normal.

### Ignoring `tool` directives when vendoring for offline dev

Wrong: running `go mod vendor`, then adding a Go 1.24 `tool` directive, and
expecting `go tool` to work offline. The tool was never vendored, so `go tool`
reaches for the network.

Fix: re-vendor after any `tool` change, and audit that each tool's module is
present and `## explicit` in `modules.txt`.

Next: [01-vendor-service-hermetic-build.md](01-vendor-service-hermetic-build.md)
