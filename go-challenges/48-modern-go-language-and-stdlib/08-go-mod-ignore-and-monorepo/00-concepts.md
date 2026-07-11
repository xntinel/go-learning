# Excluding Directories with the go.mod ignore Directive — Concepts

A real backend monorepo's Go module root is rarely all Go. Alongside the packages
sit generated code (protobuf, OpenAPI, `sqlc` output), vendored frontend assets
(`node_modules`, a compiled single-page app under `static/`), Terraform, large
fixtures, and half-finished spikes that do not compile. Every one of those is a
landmine for the four commands CI actually runs — `go build ./...`,
`go vet ./...`, `go test ./...`, and `go list all`. A single non-buildable or
non-Go directory turns the wildcard red or floods the output with noise. The
Go 1.25 `ignore` directive is the surgical fix: it removes a subtree from the go
command's package-pattern expansion while leaving the files physically in the
module. The senior skill is knowing precisely what it does and, more importantly,
the three things it does *not* do — because getting it wrong either breaks the
build or lulls a team into a false belief that they shrank their module.

## Concepts

### The problem: wildcards walk the whole tree

`./...` and `all` are not literal directory globs the shell expands; they are
package patterns the go command expands by walking the module's directory tree.
`./...` matches every package rooted at the current directory downward; `all`, in
module mode, is the main module's packages plus their transitive dependencies —
and to enumerate "the main module's packages" the go command walks every
directory under the module root. The practical consequence is that a directory of
generated `.pb.go` that fails to compile, or a `node_modules` full of JavaScript,
is *seen* by these patterns. `go build ./...` tries to compile the broken
generated package and fails; `go list all` lists it; the wildcard that CI depends
on to mean "the whole module is healthy" goes red for a reason that has nothing to
do with your code.

### What the tool already ignores, for free

The go command already skips some directories during package matching,
independent of any directive. From `go help packages`: directories named
`testdata` are ignored, and directories whose names begin with `.` or `_` (a dot
or an underscore) are ignored. This is why you can put non-buildable helper files
under `testdata/` and never think about them. The trouble is that the directories
that plague a monorepo have *mandatory* normal names you cannot change:
`node_modules` is dictated by the JavaScript toolchain, `static/` by your asset
pipeline, `generated/` by convention across the team. You cannot rename them into
the `testdata`/`_`/`.` conventions. That gap is exactly what the `ignore`
directive was created to fill.

### Why the pre-1.25 workarounds are all unsatisfying

Three older techniques get reached for, and each is wrong for this job. First,
renaming: you cannot rename third-party output like `node_modules` without
breaking the tool that produced it. Second, a `//go:build ignore` constraint on a
file: that tag only drops the *file* from a particular build, and the file must
still parse as Go and is still visited by `go list`/`go vet` pattern matching — it
does not remove a directory from the wildcard, and it does nothing for a directory
that contains no Go at all. Third, carving the subtree into a nested module by
dropping a `go.mod` into it: this does remove it from the parent's `./...`, but it
creates a genuinely separate module with its own version, its own `go.sum`, and
its own release cadence. That is a heavyweight, semantically loaded move for what
is really just build hygiene — you do not want your `generated/` directory to
become a versioned, importable module just to keep it out of a wildcard.

### The directive, and its path semantics

The `ignore` directive arrived in **Go 1.25** (not 1.24 — a `go 1.24` module and
a go1.24 toolchain reject the line as an unknown directive). It has a single-line
form and a block form:

```
ignore ./node_modules

ignore (
	./generated
	static
	./third_party/js
)
```

The path semantics are the crux, and the two forms of path mean different things:

- A path that **starts with `./`** is interpreted relative to the module root and
  ignores *only that one subtree*. `ignore ./generated` ignores
  `<root>/generated` and everything under it, and nothing else.
- A **bare path** (no leading `./`), such as `ignore node_modules`, matches a
  directory with that name *at any depth* in the module and ignores each such
  subtree. This is what you want for `node_modules`, which the JS toolchain can
  scatter at multiple levels; it is a footgun for `generated`, because it will
  silently also drop `internal/foo/generated` if such a directory exists.

The rule of thumb: reach for the bare form only when you genuinely mean "every
directory of this name, wherever it is"; otherwise anchor with `./` so you ignore
exactly one place.

### The one thing it does: it changes package-pattern expansion, only

`ignore` affects nothing but how the go command expands package patterns. Ignored
directories and their recursive contents drop out of `./...` and `all`, so
`go build`, `go vet`, `go test`, and `go list` over those patterns skip them. That
is the entire mechanism. The Go 1.25 release notes state it precisely: files in
ignored directories "will be ignored by the `go` command when matching package
patterns, such as `all` or `./...`, but will still be included in module zip
files." That last clause is the hinge for everything below.

### Non-effect 1: it is not a publish or size filter

This is the single most common senior misconception. Ignoring a directory does
**not** remove it from the published module. The files remain in the module file
tree and are still packed into the module zip; `go mod download` fetches the same
bytes; the module is not one kilobyte smaller. `ignore` is a build-view filter,
not a distribution filter. You can confirm this empirically:
`golang.org/x/mod/zip.CreateFromDir` — the same code path the module proxy uses to
build a module zip — does not read the `ignore` directive and still packs the
ignored files (it only omits VCS directories like `.git` and files that belong to
nested modules). Shrinking what actually ships is a separate, later proposal
(golang/go #76208), not this feature. If your real goal is a smaller download,
`ignore` is the wrong tool.

### Non-effect 2: it does not remove a package from the import graph

`ignore` touches wildcard expansion, not `import`. If a package that is *not*
ignored imports a package that lives *inside* an ignored directory, the imported
package is still compiled — it is pulled in by the import edge, not by pattern
matching. So the directive is only safe for directories that are not part of your
build's import graph. Ignoring a directory you actually import will not make it
vanish from the build; it will just vanish from your CI's *test* coverage of it
while still being compiled into your binaries, which is the worst of both worlds.

### Non-effect 3: it is honored only in the main module

An `ignore` directive is read only from the main module's `go.mod`. A dependency
that ships an `ignore` line in its own `go.mod` has no effect on your build, and
your `ignore` lines have no effect inside a dependency. This mirrors how other
main-module-only directives (like `replace`) behave, and it means you cannot rely
on a library to pre-ignore its own junk on your behalf.

### Tooling reality: the guarantee is about the go command

The contract is with the `go` command. Editors and linters that do their own
directory walking — `gopls`, `golangci-lint` — do not automatically honor the
`ignore` directive; they have their own exclusion configuration
(`build.directoryFilters`, `.golangci.yml` `issues.exclude-dirs`, and so on). Do
not assume adding an `ignore` line silences your linter without also configuring
the linter. This matters in CI, where the linter and the go command are separate
steps.

### Editing it safely: there is no `go mod edit -ignore`

As of Go 1.25, `go mod edit` has no `-ignore` flag, so the directive is edited by
hand or programmatically. For any tool that must keep the ignore set correct and
deterministic — a repo scaffolder, a lint step that reconciles the set against the
directories actually present — the right approach is `golang.org/x/mod/modfile`:
`modfile.Parse` gives you a `*File` whose `Ignore []*Ignore` you can mutate with
`File.AddIgnore(path)` and `File.DropIgnore(path)`, then `File.Cleanup()` and
`File.Format()` to emit canonical, stable bytes. `AddIgnore` and `DropIgnore` are
idempotent (adding an existing path or dropping an absent one is a no-op), which
is what makes a reconciler safe to run repeatedly. These APIs were added in
`x/mod` v0.25.0.

### The decision framework

Use `ignore` for normally-named, non-imported subtrees you want out of the
wildcards — generated code, `node_modules`, a built SPA under `static/`,
non-buildable spikes. Rely on the free `testdata`/`_`/`.` conventions wherever the
naming permits. Carve out a nested module only when the subtree is genuinely a
separate versioned, importable unit — not merely to hide it. And when the real
goal is a *smaller published module*, wait for the zip-exclusion mechanism; do not
reach for `ignore`, which will not shrink anything.

## Common Mistakes

### Believing ignore shrinks the module zip

Wrong: adding `ignore ./generated` to make `go mod download` fetch fewer bytes.
The files still ship; the download is byte-for-byte identical. Fix: understand
`ignore` is a build-view filter only, and track golang/go #76208 for the actual
zip-exclusion feature.

### Ignoring a directory that is imported

Wrong: ignoring a directory whose packages are imported by non-ignored code and
expecting them to disappear from the build. They stay in the import graph and are
still compiled; you only lost their wildcard test coverage. Fix: only ignore
subtrees that are not part of the build's import graph.

### Using a bare name when you meant one directory

Wrong: `ignore generated` intending to drop only `<root>/generated`. The bare
form matches *every* directory named `generated` at any depth and silently drops
more than you meant. Fix: anchor with `./generated` unless you truly want the
match-anywhere behavior (as with `node_modules`).

### Silently removing real packages from CI

Wrong: ignoring a directory that still contains packages you want built and
tested, so they quietly fall out of `go test ./...` and stop being covered. Fix:
ignore only non-buildable or non-Go subtrees; keep buildable packages in the
wildcard.

### Forgetting the go 1.25 language version

Wrong: adding the directive under `go 1.24` (or building with a go1.24 toolchain).
The line is rejected as an unknown directive. Fix: set `go 1.25` (or later) in
`go.mod` and build with a Go 1.25+ toolchain.

### Assuming linters and editors honor it

Wrong: expecting `gopls` or `golangci-lint` to skip an ignored directory. They
walk the tree themselves. Fix: also configure the tool's own exclusion setting
(`directoryFilters`, `issues.exclude-dirs`).

### Hand-editing the block into a mess

Wrong: appending to the `ignore ( ... )` block by hand until it has duplicates,
inconsistent ordering, and merge conflicts. Fix: manage it with
`x/mod/modfile` — `AddIgnore`/`DropIgnore` then `Cleanup` and `Format` — so the
output is deduplicated and deterministic.

### Confusing ignore with exclude

Wrong: reaching for `ignore` to block a bad dependency version, or `exclude` to
drop a local directory. They are unrelated: `exclude` blocks a specific
module *version* from the build list; `ignore` drops local *directories* from
package matching.

Next: [01-monorepo-build-guardrail.md](01-monorepo-build-guardrail.md)
