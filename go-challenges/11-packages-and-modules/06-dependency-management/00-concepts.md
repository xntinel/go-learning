# Dependency Management for Production Go Services — Concepts

Dependency management for a production service is a supply-chain and
reproducibility problem, not a "run `go get`" problem. The thing a senior backend
engineer actually owns is the *build list*: the exact set of module versions that
compile into the binary that ships. When an unrelated transitive bump silently
moves a shared dependency, when a CI build fails with "missing go.sum entry", when
a release accidentally builds against a fork that exists only on one laptop, when
a CVE lands in a package you import but never call — those are the incidents this
lesson is about. The tools you build here are the CI gates and ops utilities that
keep the build list honest, and every one of them is written in Go against
`golang.org/x/mod` — the same library the `go` command and `gopls` use to parse
`go.mod`, order semantic versions, and validate module paths — so the concepts are
exercised as real code you can test offline, not documented as shell commands you
run and hope.

Read this file once; it contains the model behind all ten independent exercises
that follow.

## Concepts

### go.mod is the module's contract

`go.mod` records three things that matter: the module *path* (its identity and
import prefix), the `go` directive (the language version), and the *requirement
set*. The `go` directive is not a hint — it is a hard floor. It gates which
language and standard-library features are available, and since Go 1.21 the
toolchain will refuse to build a module that declares a newer `go` version than
the toolchain in use. It also changes `go mod tidy` behavior (module-graph pruning
arrived at `go 1.17`). Each `require` is either *direct* (imported by your code)
or marked `// indirect` (pulled in transitively, or a direct dependency's
dependency recorded for the pruned graph). Direct-vs-indirect is not cosmetic:
`go mod tidy` maintains it, and tools reason about it to decide what a human
actually chose to depend on.

### go.sum is a checksum database, not a lock file

The single most common misconception. `go.sum` does not pin versions — `go.mod`
does. `go.sum` is a set of *expected cryptographic hashes* for the specific
versions the build uses. Each module contributes two lines: the hash of the
module's file tree (`h1:...`) and the hash of that module's own `go.mod`
(`.../go.mod h1:...`). The two-hash structure is what lets Go verify the pruned
module graph without downloading every module's source. What breaks a
`-mod=readonly` CI build is not a *wrong* version — it is a *missing* `go.sum`
entry, which is exactly what you get when you hand-edit a `require` line instead of
letting `go get` update both files. `go.sum` is a required, committed artifact;
deleting or `.gitignore`-ing it removes the integrity guarantee entirely.

### Minimal Version Selection

Go does not build against the *latest* published version of anything. It builds
against the *highest version required by anyone in the module graph* — Minimal
Version Selection (MVS). This is the property that makes builds deterministic:
publishing a new upstream release does not silently change your build, because
nothing in your graph requires it yet. It is also the explanation for the
confusing incident where bumping one unrelated dependency moves a shared
transitive dependency: the newly-added dependency required a higher version of the
shared module than anything else did, so MVS now selects that higher version for
everyone. "Selected version" and "latest version" are different concepts;
conflating them is the root of most "why did this move?" confusion.

### Semantic Import Versioning

A module at `v2` or higher carries its major version in the import path:
`example.com/pkg` for v0/v1, `example.com/pkg/v2` for v2. This is Semantic Import
Versioning, and it has a sharp consequence for automation: a *major* upgrade is a
source-code change (every import statement moves to the new `/vN` path), not a
version-number edit. That is precisely why a major bump cannot be auto-merged the
way a patch can — editing the version in `go.mod` without changing the import path
just leaves the old path resolving to v1. A patch or minor bump within the same
major is a number change; a major bump is a migration.

### go get versus go mod tidy

`go get path@version` adds or updates a single requirement and records its
checksums in `go.sum`. `go mod tidy` is different: it reconciles `go.mod` and
`go.sum` with the *actual import graph* — adding requirements for packages you
import but do not require, dropping requirements nothing imports, and (since
`go 1.17`) recording the pruned module graph plus the one-version-back checksums
needed to verify it. Tidy is not an occasional cleanup; a clean checkout with an
untidy `go.mod` either fails to build (missing require) or carries dead weight
(unused require, bloated `go.sum`). In a mature pipeline `go mod tidy -diff` (Go
1.23+) is a CI gate that must produce no diff.

### replace, exclude, and retract

`replace` and `exclude` apply *only in the main module* and are ignored when your
module is someone else's dependency — so they are your local build policy, not a
property you export. A `replace` still needs a matching `require` to bring the
module into the graph; it redirects, it does not add. The dangerous form is the
local filesystem replace (`=> ./fork` or `=> ../other`): a development convenience
that builds against a path present on exactly one machine and invisible to the
proxy. Shipping one to production is a classic release-time incident, which is why
a pre-release audit gate must reject it. `retract` (Go 1.16+) is the *author* side:
a module's own author marks a bad release as unsuitable without deleting it;
retracted versions drop out of `@latest` selection and surface as warnings under
`go get`/`go list -m -u`.

### Reproducible builds: readonly and vendoring

Two mechanisms give a byte-reproducible build. The default, `-mod=readonly`, fails
the build if `go.mod` would need to change — so the committed `go.mod`/`go.sum` are
authoritative and CI cannot silently mutate them. The alternative is vendoring:
`go mod vendor` copies every needed module into `vendor/` alongside a
`vendor/modules.txt` manifest, and the build reads those copies. The catch is
consistency: `modules.txt` must stay in lockstep with `go.mod`, so the gate is
`go mod vendor && git diff --exit-code vendor/modules.txt` — a stale manifest means
the build is compiling code that no longer matches the declared dependencies.

### Private modules

`GOPRIVATE` is a comma-separated glob list matched against *path prefixes*. A
module whose path prefix matches is treated as private: the `go` command bypasses
both the public module proxy and the checksum database for it (GOPRIVATE is the
umbrella that sets the defaults for `GONOPROXY` and `GONOSUMDB`). Getting this
wrong is bidirectional and both directions are bad: too broad a pattern (adding
`github.com`) disables checksum verification for public code; too narrow (forgetting
your org) leaks private module paths to the public proxy and sumdb. `GOINSECURE`
additionally permits plain-HTTP fetches for matching paths. The prefix-match
semantics matter: `github.com/acme-corp/*` matches `github.com/acme-corp/svc` and
`github.com/acme-corp/svc/internal/db` alike.

### Supply-chain security and reachability

`govulncheck` does not just list vulnerabilities you import — it performs
reachability analysis and distinguishes a vulnerability whose vulnerable *symbol*
is actually called from your code from one that is merely present in an imported
package. A mature security gate fails the build only on *reachable* findings; a
gate that blocks on every import-level match becomes noise, and noise gets
ignored. The response to a reachable finding is an upgrade (to a fixed version), an
`exclude` of the bad version, or a vendored patch — and `retract`/`exclude` are the
`go.mod`-level levers for that response.

### Pinning the build toolchain

Build tools (`sqlc`, `mockgen`, `golangci-lint`) used to be pinned with a
`tools.go` file full of blank imports plus `go install tool@latest` in CI — which
drifts, because `@latest` moves. Go 1.24 replaced that with `tool` directives:
each tool is recorded in `go.mod` (backed by a `require`) and run via `go tool`,
giving a pinned, reproducible tool set shared by every developer and CI runner.

### golang.org/x/mod is the right foundation

Do not parse `go.mod` with regexes. `golang.org/x/mod/modfile` parses and edits
`go.mod` exactly as the `go` command does; `golang.org/x/mod/semver` orders
semantic versions correctly (including prereleases); `golang.org/x/mod/module`
validates module paths, splits the `/vN` suffix, and implements the GOPRIVATE
prefix-match algorithm. Every gate in this lesson is built on it.

## Common Mistakes

### Hand-editing a require directive

Wrong: pasting `require github.com/foo/bar v1.2.3` into `go.mod` by hand. The
matching `go.sum` entries are absent, so the next `-mod=readonly` build fails with
"missing go.sum entry". Fix: run `go get github.com/foo/bar@v1.2.3` and let the
toolchain update both files.

### go get with no version and calling it pinned

Wrong: `go get github.com/foo/bar` and assuming the build is now reproducible. With
no version it resolves `@latest` at that instant, so a colleague running it a week
later can get a different version. Fix: pin — `go get github.com/foo/bar@v1.2.3`.

### Treating go mod tidy as optional

Wrong: leaving unused requires in `go.mod` and adding missing ones "later". Unused
requires bloat the module graph and `go.sum`; a missing require breaks the build on
a clean checkout. Fix: treat `go mod tidy -diff` clean as a CI gate, not a chore.

### Confusing latest with selected

Wrong: expecting `go get` of one package to pull the newest release of everything.
MVS selects the highest *required* version, not the newest *published* one. Fix:
reason about the requirement graph, not the release feed.

### Bumping a v2+ dependency by editing the number

Wrong: changing `v1.9.0` to `v2.0.0` in `go.mod` and expecting it to work. Semantic
Import Versioning makes the major a source change; the old import path keeps
resolving to v1. Fix: change every import to the `/v2` path, then require the v2
module.

### Shipping a local replace to production

Wrong: leaving `replace example.com/pkg => ../fork` in the release `go.mod`. It
builds against a path only one machine has. Fix: a pre-release `go.mod` audit gate
that rejects filesystem replaces.

### Deleting or not committing go.sum

Wrong: `.gitignore`-ing `go.sum` "because the toolchain regenerates it". Without it
there is no integrity guarantee and `-mod=readonly` builds fail. Fix: commit it; it
is a required artifact.

### Vendoring without re-vendoring after a change

Wrong: changing a dependency in `go.mod` but not re-running `go mod vendor`, leaving
`vendor/modules.txt` inconsistent. The build then uses stale vendored code. Fix: CI
runs `go mod vendor` and fails on any diff.

### Getting GOPRIVATE wrong in either direction

Wrong: adding `github.com` to GOPRIVATE (disables checksum verification for all of
GitHub) or forgetting your private org (leaks paths to the public proxy). Fix:
match exactly your private prefixes, e.g. `github.com/acme-corp/*`.

### Blocking on every govulncheck finding

Wrong: failing CI on any import-level match. Without reachability triage the gate is
noise. Fix: fail only on findings whose vulnerable symbol is reachable from your
call graph.

### Managing tools with tools.go and @latest

Wrong: a `tools.go` blank-import file plus `go install tool@latest` in CI —
unpinned, drifting tool versions. Fix: Go 1.24 `tool` directives, pinned in
`go.mod` and run via `go tool`.

Next: [01-dep-hygiene-cli.md](01-dep-hygiene-cli.md)
