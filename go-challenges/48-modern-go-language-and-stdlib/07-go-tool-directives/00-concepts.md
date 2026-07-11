# Tool Dependencies with go.mod tool Directives — Concepts

Every Go team that leaned on code generation carried the same fragile ritual for
years. A file named `tools.go`, guarded by a `//go:build tools` constraint so it
never entered a real build, blank-imported the executables the repo depended on:

```go
//go:build tools

package tools

import (
	_ "golang.org/x/tools/cmd/stringer"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
	_ "go.uber.org/mock/mockgen"
)
```

The blank imports forced those tools' modules into `go.mod` as ordinary
`require` lines so `go mod tidy` would not delete them. Then a Makefile ran the
tools out of band, usually `go install golang.org/x/tools/cmd/stringer@latest`
(unpinned, so every laptop and CI runner resolved a different version) or
`go run golang.org/x/tools/cmd/stringer@v0.30.0` (pinned but recompiled on every
invocation and not kept in lockstep with the module graph). The result was a
reproducibility hole hiding in plain sight: the source of truth for "what
executables does this build depend on, and at what version" was scattered across
a build-tagged Go file, a Makefile, and CI scripts, and nothing tied the tool
version to the module it was used from. Generated code differed between
developers; supply-chain review had no single manifest to audit.

Go 1.24 closed the hole. Tool dependencies became first-class members of the
module graph: a `tool` directive in `go.mod`, backed by a `require` that pins the
version, verified in `go.sum`, resolved by minimal version selection (MVS)
alongside every library dependency, run with `go tool`, and cached as a built
executable in the Go build cache. For a senior backend engineer this is not a
convenience feature; it turns `go.mod` into a machine-parseable manifest of build
executables that you can audit, gate in CI, and migrate to programmatically.

## Concepts

### Why the directive exists, precisely

The `tools.go` hack had two independent defects. First, blank-importing a tool
pulled the tool's *entire* dependency tree into your module's `require` set as if
those packages were part of your production code, inflating the graph and the
`go.sum`. Second, even with the tool's module pinned in `require`, actually
*running* the tool was an out-of-band command whose version was not derived from
`go.mod` — `@latest` re-resolved every time, and a hardcoded `@version` in a
Makefile drifted from the `require` line silently. The `tool` directive fixes
both: it names the tool as an executable dependency inside `go.mod`, its version
is the one MVS selects from the backing `require`, and `go tool <name>` runs
exactly that version with no separate install step.

### Anatomy: what `go get -tool` writes

`go get -tool <pkg>@<version>` adds two things to `go.mod`: a `tool <pkg>` line
whose argument is a *main package* import path, and a `require` line pinning the
*module* that contains it. The two are distinct: the tool line names a package,
the require pins a module version. If the tool package lives outside your current
module it MUST have a matching `require` or the directive is invalid — `go.mod`
will not parse into a usable state. Removal is `go get -tool <pkg>@none`, which
drops the tool line (and prunes the require if nothing else needs it). Doing the
edit by hand — adding a `tool` line without the backing `require` — produces an
invalid `go.mod`; always go through `go get -tool`, or, when scripting, add both
the tool directive and the require together.

### Running and name resolution

`go tool` with no arguments lists the distribution tools (`vet`, `cover`,
`pprof`, and friends) followed by the module's own tools. `go tool <name>` runs
one. The `<name>` you pass is the last path segment of the tool's package path
(with any `/vN` major-version suffix stripped) *if that short name is
unambiguous*; if two tools share a last segment, or the short name collides with
a builtin like `vet` or `cover`, you must give the full package path. `go tool -n`
prints the command it would run instead of running it — useful for debugging what
resolves to what. Assuming the short name always works is a common trap in shared
repos where two generators happen to end in the same segment.

### The `tool` meta-pattern

`tool` used as a package pattern (rather than a specific path) expands to every
tool directive in the current module, or, in workspace mode, the union across all
workspace modules. `go get tool` (equivalently `go get tool@upgrade`) upgrades
*all* of them; `go install tool` builds and installs every module tool into
`GOBIN`. This is where a subtle mistake lives: bare `go get tool` does not *add*
a tool — it upgrades the existing ones. Adding a single tool requires the `-tool`
flag plus a package path: `go get -tool <pkg>`.

Contrast module tools with `go install`ed global binaries. A module tool is
per-repo and reproducible: its version is pinned in the consuming module's
`go.sum`, so a fresh checkout on any machine builds the identical tool. A
`go install pkg@latest` binary is machine-global and unpinned, independent of any
consuming module — shipping CI that still uses `@latest` reintroduces exactly the
drift the directive was designed to remove.

### Caching and the cost model

Since Go 1.24 the executables produced by `go tool` (and by `go run`) are stored
in the Go build cache. The first invocation compiles the tool; subsequent ones
are near-instant cache hits. The trade-off is a larger build cache. This caching
is what makes `go tool` viable in the inner loop and in CI without a separate
install step — the old objection that "compiling the tool every time is too slow"
no longer holds.

### Interaction with MVS and module pruning

Because tool requirements participate in MVS, a tool with a large dependency tree
can *raise* your module's selected versions and grow `go.sum`. Module graph
pruning keeps most tool-only transitive dependencies out of *downstream*
consumers of your module, but the tool's own module and its direct dependencies
still appear in your graph. The senior takeaway: pin narrowly, prefer small
single-purpose generators, and audit the transitive impact of any heavyweight
tool you add rather than being surprised when an unrelated library version jumps.

### go.mod as a parseable manifest

The supported, gofmt-stable way to read or rewrite `go.mod` is
`golang.org/x/mod/modfile`, not regular expressions. It exposes the tool
directive as `File.Tool` (a `[]*modfile.Tool`, each with a `Path`), the backing
versions as `File.Require` (each `Require.Mod` is a `module.Version` with `Path`
and `Version`), and edit methods `File.AddTool`, `File.DropTool`,
`File.AddRequire`, `File.DropRequire`. After edits, `File.Cleanup` removes the
lazily-cleared entries and `File.Format` re-serializes the file exactly as the go
command would. Regex-editing `go.mod` breaks the block/paren grammar and the
canonical formatting; the modfile package is what the go command itself uses.

### The reproducibility guarantee and its limit

A tool directive pins the tool's *module version* through `go.sum`, so everyone
builds the same tool binary from the same source. It does not, and cannot, make
the tool's *output* deterministic. The pinned tool still runs against the local
toolchain and its own runtime behavior: if it iterates a map without sorting,
embeds a timestamp, or reads the environment, its generated files will still
differ run to run even though the tool itself is perfectly pinned. Determinism of
generated artifacts is the tool author's responsibility. This is why the second
exercise builds a generator that sorts its output — pinning and determinism are
two separate guarantees you need both of.

### Integration with go:generate

Tools pinned via directives are meant to be driven from
`//go:generate go tool <name> ...`, so that `go generate ./...` invokes the
pinned version rather than whatever happens to be on `PATH`. The legacy
`//go:generate stringer -type=...` form depended on a globally installed
`stringer`; `//go:generate go tool stringer -type=...` uses the one the module
pins. Migrating generate directives to `go tool` is the final step of adopting
tool directives.

## Common Mistakes

### Hand-editing a tool line without the backing require

Adding `tool example.com/cmd/foo` to `go.mod` by hand, with no `require` pinning
the module, produces an invalid `go.mod`. Use `go get -tool example.com/cmd/foo`,
or when scripting, call `File.AddTool` and `File.AddRequire` together so the
version is always pinned.

### Confusing `go install pkg@latest` with `go get -tool`

`go install pkg@latest` installs a global, unpinned binary that ignores the
consuming module; `go get -tool` plus `go tool` runs a module-scoped,
reproducible version. CI that still runs `go install ...@latest` for its build
tools defeats the entire purpose of the directive.

### Assuming the short name always resolves

`go tool <shortname>` fails when two tools share a last path segment or the name
collides with a builtin (`vet`, `cover`, `pprof`). When that happens, pass the
full package path instead.

### Keeping tools.go and tool directives at the same time

Leaving the legacy `//go:build tools` blank-import file in place *and* adding
tool directives counts the tool's dependencies twice and keeps the module graph
larger than necessary. Migrate, then delete `tools.go`.

### Regex-editing go.mod, or forgetting Cleanup

Editing `go.mod` with string substitution breaks its block/paren grammar and
canonical formatting. Use `x/mod/modfile`. And after `File.DropTool`, call
`File.Cleanup` before `File.Format`: `DropTool` only clears the entry lazily, so
without `Cleanup` a block-form directive is left with a blank line inside its
parentheses.

### Expecting the directive to make output deterministic

A perfectly pinned but non-deterministic tool — unsorted map iteration,
timestamps, environment reads — still produces diffs. Pinning fixes the tool
version, not the tool's behavior.

### Pinning a heavyweight tool without auditing the graph

A tool with a big dependency tree can bump your module's selected versions via
MVS and grow `go.sum`. Audit the transitive impact; prefer small generators.

### Using bare `go get tool` to add a tool

Bare `go get tool` upgrades ALL existing tools (the meta-pattern). Adding one
requires `go get -tool <pkg>`.

Next: [01-tool-directive-auditor.md](01-tool-directive-auditor.md)
