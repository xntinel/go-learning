# Go Modules and Dependencies — Concepts

A Go module is the unit of versioning and the unit of reproducibility. A senior
backend engineer does not just write code inside a module; they own the build
contract that the module encodes: which exact bytes of which exact dependencies
compile into the binary that ships, and whether that answer is the same on a
laptop, on a CI runner, and on a rebuild two years from now during an incident.
That contract lives in two files at the module root — `go.mod` and `go.sum` — and
is enforced by a handful of tools whose behavior is worth understanding
precisely, because their defaults have changed across releases and their failure
modes are where production builds and supply-chain audits actually break. This
file is the conceptual foundation. Read it once and each of the ten independent
exercises that follow becomes a focused drill on one part of it.

## Concepts

### A module is the unit of versioning

Before modules (Go 1.11) code lived under `GOPATH` with no native way to pin
dependency versions; two projects that needed one library at different versions
collided. Modules replaced that with a versioning model built on Semantic
Versioning. A module is a tree of packages with a `go.mod` at its root. That file
declares three things: the module path (the import prefix all packages inside are
rooted at), the `go` directive (the language and minimum toolchain version), and
the minimum required version of every direct and indirect dependency. From
Go 1.21 the `go` directive is a hard floor: a toolchain older than the declared
version refuses to build the module rather than silently miscompiling it, and
with `GOTOOLCHAIN=auto` the `go` command will download the matching toolchain
instead. The module path must be one you control for anything published;
`example.com/...` is reserved for documentation and exercises, which is why every
exercise here uses it.

### go.mod records the build graph, not a runtime audit trail

A frequent misconception is that `go.mod` lists every version of every dependency
that was ever tried, or everything the process touches at runtime. It does
neither. It records the minimum set of modules that *provides* every package the
source and test files import, directly or transitively. Since Go 1.17 the `go`
command records an explicit `require` for every module reachable from the build,
marking the ones you do not import directly with `// indirect`. The list is a
description of the build, kept honest by the toolchain — not something you
hand-curate. Hand-editing `require` lines is how the graph drifts out of sync
with the imports.

### go.sum is a tamper-evident checksum log

`go.sum` records a cryptographic hash of every module zip and every `go.mod` the
build loads. On download the `go` command recomputes the hash and refuses to
proceed on a mismatch, and (by default) cross-checks new hashes against the
public checksum database `sum.golang.org`. The workflow is to commit *both*
`go.mod` and `go.sum`: any clone then reproduces byte-identical inputs, and a
tampered mirror or a mutated cache is caught at build time rather than shipping
silently. Putting `go.sum` in `.gitignore` throws that guarantee away and lets CI
and production diverge.

### Minimal Version Selection

Go resolves the build list with Minimal Version Selection (MVS): for each module
in the graph it selects the *maximum of the minimum versions* required anywhere
in the graph — not the newest version that exists. There is no separate lockfile
solver and no version-range SAT problem; the algorithm is deterministic and
low-churn, and the "lockfile" is just `go.mod` plus `go.sum`. A practical
consequence: an unpinned dependency does not float to the latest release on its
own. It stays at whatever floor the graph demands until some `require` raises that
floor. Two directives prune the computation: `exclude` bars a specific version
from selection (MVS then picks the next-highest that satisfies the graph), and
`retract` (declared by a library's own author) signals that a published version
must not be selected at all.

### go mod tidy is the reconciliation source of truth

`go mod tidy` walks the actual imports and rewrites the module graph to match: it
adds missing `require`s, drops unused ones, and reconciles `go.sum`. It is
idempotent — running it twice leaves `go.mod`/`go.sum` byte-identical — and it is
the correct last step after any change to imports. Do not reach for a manual
`go.mod` edit to add a dependency; add the import and run `go mod tidy`, or use
`go get`.

### go get manages the graph; go install installs binaries

Since Go 1.17 these are cleanly split. Inside a module, `go get pkg@version` edits
the module graph (adds or upgrades a `require`, downloads the source) and never
installs a command. `go install pkg@version` compiles a binary to `GOBIN`
(defaulting to `~/go/bin`) without touching your module graph. Version suffixes
matter: `@latest`, `@vX.Y.Z`, `@patch`, and `@none` (which removes a requirement)
are the forms, and any `go get`/`go install` on a package that is not already a
dependency *requires* a suffix. Running `go get` with no module and no version is
the classic "go.mod file not found" error.

### The replace directive redirects a module, only in the main module

`replace old => ../local/path` or `replace old => example.com/fork/x vX.Y.Z`
points a module requirement at a local checkout or a fork. It is how you develop
an application against un-published local changes to a library, or against an
internal fork in a monorepo. Two properties are load-bearing. First, `replace`
still needs a matching `require` — it redirects an existing requirement, it does
not create one. Second, `replace` takes effect *only in the main module*; it is
ignored when your module is consumed as a dependency of someone else's. A
`replace` pointing at `../greeterfork` that leaks into a published release leaves
every consumer's build pointing at a path that exists only on your machine — a
classic broken-consumer footgun. Add it for local work, drop it (`go mod edit
-dropreplace`) before you tag a release.

### The Go 1.24 tool directive replaces tools.go

Developer tooling (code generators, linters) must be version-pinned per module so
every engineer and CI runner runs byte-identical tools. The old pattern was a
`tools.go` file with blank imports behind a build tag, kept only to force the
tools into `go.mod`. Go 1.24 replaced it with a first-class `tool` directive:
`go get -tool golang.org/x/tools/cmd/stringer` adds a `tool` line plus a pinned
`require`, `go tool` lists the module's tools, and `go tool stringer ...` runs the
pinned build. `go tool` only sees tools declared in the *current* module's
`go.mod`, which is exactly the reproducibility property you want.

### Supply-chain hygiene is part of the build contract

The same `go.mod`/`go.sum` machinery is the front line of supply-chain security,
and a backend team wires it into CI. `GOFLAGS=-mod=readonly` (the default since
Go 1.16) makes a build *fail* when `go.mod` is out of date instead of silently
mutating it — so a pull request that adds an import without running `go mod tidy`
is rejected rather than quietly fixed on the runner. `go mod verify` re-checks the
cached module bytes against `go.sum`. `govulncheck` does call-graph-aware
vulnerability scanning: it reports a known CVE only when your code actually
reaches the vulnerable symbol, cutting false positives. And a cluster of
environment variables governs how private code is fetched and verified:
`GOPRIVATE` marks path prefixes as private (bypassing the proxy and checksum
database), with `GONOSUMDB`/`GONOSUMCHECK`, `GOSUMDB`, `GOINSECURE`, and `GOVCS`
tuning the checksum-DB, TLS, and version-control-tool rules for those paths.

### Vendoring produces hermetic builds

`go mod vendor` copies every dependency's source into a `vendor/` tree and writes
`vendor/modules.txt`. With `-mod=vendor` (auto-selected when `vendor/` exists and
matches `go.mod`), the toolchain builds *only* from that tree and never contacts a
proxy or the module cache — a hermetic, offline, auditable build. The trade is
maintenance: the tree must be regenerated (`go mod vendor`) after every
dependency change or `-mod=vendor` builds stale or inconsistent code. Vendoring
earns its keep for airgapped CI, regulated audits, and reproducibility guarantees;
for most services the proxy plus `go.sum` is enough.

### The structured data behind audits and SBOMs

`go mod graph` prints every edge of the module graph, `go mod why` explains why a
given package is in the build, `go list -m -json` emits `Path`/`Version`/`Dir`/
`Time`/`Replace`/`Indirect` for each selected module, and `go version -m <binary>`
reads the module versions embedded in an already-built binary. These four are the
exact inputs a pipeline reads to generate a Software Bill of Materials, and the
exact tools you reach for during post-incident dependency forensics — "which
version of this library is actually in the binary we shipped?"

## Common Mistakes

### Assuming go build will update go.mod

Wrong: adding `import "github.com/x/y"` and expecting `go build` to fix `go.mod`.
Since Go 1.16 the default is `-mod=readonly`, so the build fails with `go.mod file
is inconsistent with the contents of the build`. Fix: run `go mod tidy` (or
`go get github.com/x/y`) so the graph matches the imports.

### Gitignoring go.sum

Wrong: adding `go.sum` to `.gitignore` to reduce diff noise. That breaks
cross-machine verification and lets CI and production builds diverge silently on a
tampered mirror. Fix: commit both `go.mod` and `go.sum`.

### Using go get to install a binary

Wrong: `go get github.com/some/cmd` to install a tool. Since Go 1.17 `go get`
only edits the module graph. Fix: `go install pkg@version`, or for a
project-pinned tool, `go get -tool pkg` plus `go tool`.

### Running go get outside a module without a version

Wrong: `cd /tmp && go get github.com/foo/bar`. Result: `go.mod file not found in
current directory or any parent directory`. Fix: run inside a module, or use
`go install pkg@version` for a one-off binary.

### Branching on err.Error() instead of errors.Is

Wrong: `if err.Error() == "name must not be empty"`. The message is for humans; it
breaks the instant it is reworded or the error is wrapped with `%w`. Fix: export a
sentinel (`ErrEmptyName`) and branch with `errors.Is(err, ErrEmptyName)`.

### Shipping a replace directive to consumers

Wrong: tagging a release with `replace example.com/x => ../fork` still in
`go.mod`. `replace` is ignored in dependency modules, so consumers get the raw
`require` pointing at a version or path that does not exist for them. Fix: drop
the `replace` before release; use it only in the main module for local work.

### Expecting MVS to pick the newest version

Wrong: assuming an unpinned dependency floats to its latest release. MVS selects
the maximum of the required minimums, so it lags until some `require` raises the
floor. Fix: raise the floor deliberately with `go get pkg@version` when you want a
newer version, and read `go list -m -u all` to see what upgrades are available.

### Committing a drifted vendor tree

Wrong: changing a dependency and forgetting `go mod vendor`. A `-mod=vendor` build
then silently uses stale code or fails the consistency check against `go.mod`.
Fix: run `go mod vendor` after every dependency change and verify `git diff` is
clean; treat a dirty vendor diff in review as a blocker.

### Reaching for tools.go instead of the tool directive

Wrong: recreating the `tools.go` blank-import hack on Go 1.24+. Fix: use
`go get -tool` and `go tool`; remember `go tool` only sees tools declared in the
current module's `go.mod`.

Next: [01-library-module-and-contract-tests.md](01-library-module-and-contract-tests.md)
