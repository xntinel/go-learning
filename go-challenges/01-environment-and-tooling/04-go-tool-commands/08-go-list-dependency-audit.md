# Exercise 8: go list — Querying the Package and Module Graph

`go list` is the programmable view of the graph — the basis of dependency-audit
and import-guard scripts. This module enumerates packages with `go list ./...`,
walks the transitive imports with `-deps`, inspects the `-json` record, runs a
`-f` template, and builds a real import guard that fails when a forbidden package
appears in the graph.

## What you'll build

```text
list-audit/                    module example.com/list-audit
  go.mod
  internal/
    circle/
      circle.go                Area(radius) float64
    guard/
      guard.go                 Forbidden(deps, banned) []string
      guard_test.go            tests the guard on a clean and a dirty graph
  cmd/
    demo/
      main.go                  imports circle
```

- Files: `internal/circle/circle.go`, `internal/guard/guard.go`, `internal/guard/guard_test.go`, `cmd/demo/main.go`.
- Implement: a `circle` library, a `demo` that imports it, and a `guard.Forbidden` that returns the banned imports found in a dependency list.
- Test: `Forbidden` on a clean graph (no hits) and a dirty graph (one hit).
- Verify: `go list ./...` lists the packages; `go list -deps ./cmd/demo` includes `internal/circle` and `fmt`; an import guard exits non-zero when a banned path is present.

### The graph as data

`go list ./...` prints the import paths of every package in the module. Adding
`-deps` walks the transitive import set depth-first — every package the target
ultimately pulls in, standard library included — printing dependencies before the
package that needs them. `-json` dumps a rich record per package with fields like
`ImportPath`, `Imports` (direct), `Deps` (transitive), and `GoFiles`. `-f` runs a
`text/template` over that record so you print exactly the fields you want. In
module mode, `go list -m all` lists the module dependency graph, `-m -u all`
marks which modules have updates available, and `-m -retracted` surfaces
retracted versions.

The audit use is direct: a build that fails whenever a forbidden package appears
in `go list -deps` output is an import guard. The `guard` package holds the pure
decision — given the dependency list and a banned set, which banned paths are
present — so it is unit-testable without shelling out, and the shell wraps it
around real `go list` output.

Create `internal/circle/circle.go`:

```go
package circle

import "math"

// Area returns the area of a circle with the given radius.
func Area(radius float64) float64 { return math.Pi * radius * radius }
```

Create `internal/guard/guard.go`:

```go
package guard

// Forbidden returns the banned import paths that appear in deps, in the order
// they are listed in deps. An empty result means the graph is clean.
func Forbidden(deps []string, banned map[string]bool) []string {
	var hits []string
	for _, d := range deps {
		if banned[d] {
			hits = append(hits, d)
		}
	}
	return hits
}
```

Create `internal/guard/guard_test.go`:

```go
package guard

import (
	"reflect"
	"testing"
)

func TestForbidden(t *testing.T) {
	t.Parallel()
	banned := map[string]bool{"example.com/list-audit/internal/legacy": true}

	clean := []string{"fmt", "example.com/list-audit/internal/circle"}
	if got := Forbidden(clean, banned); len(got) != 0 {
		t.Fatalf("clean graph flagged: %v", got)
	}

	dirty := []string{"fmt", "example.com/list-audit/internal/legacy"}
	want := []string{"example.com/list-audit/internal/legacy"}
	if got := Forbidden(dirty, banned); !reflect.DeepEqual(got, want) {
		t.Fatalf("Forbidden = %v, want %v", got, want)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/list-audit/internal/circle"
)

func main() {
	fmt.Printf("%.5f\n", circle.Area(5))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
78.53982
```

### Walking the graph

Enumerate the module's packages:

```bash
go list ./...
```

```text
example.com/list-audit/cmd/demo
example.com/list-audit/internal/circle
example.com/list-audit/internal/guard
```

Walk the transitive imports of the command (filtered to the interesting lines):

```bash
go list -deps ./cmd/demo | grep -E 'example.com|^fmt$'
```

```text
example.com/list-audit/internal/circle
fmt
example.com/list-audit/cmd/demo
```

`internal/circle` and `fmt` are printed before `cmd/demo`, the package that
imports them — depth-first, dependencies first.

Inspect the JSON record:

```bash
go list -json ./cmd/demo
```

Its `Imports` array is `["example.com/list-audit/internal/circle", "fmt"]` and
its `GoFiles` array is `["main.go"]`.

Run a template over the record. `-f` takes any `text/template` against the
package struct:

```bash
go list -f '{{.ImportPath}} {{.Name}}' ./...
```

```text
example.com/list-audit/cmd/demo main
example.com/list-audit/internal/circle circle
example.com/list-audit/internal/guard guard
```

The `.Stale` field (`go list -f '{{.ImportPath}} {{.Stale}}'`) reports whether the
install target is out of date relative to the build cache, so its value depends on
what you have already built.

### The import guard

Wrap `go list -deps` in a check that fails when a banned path is in the graph.
This is the shell form of what `guard.Forbidden` decides in Go:

```bash
BANNED="example.com/list-audit/internal/legacy"
if go list -deps ./cmd/demo | grep -qx "$BANNED"; then
	echo "FORBIDDEN import present: $BANNED"
	exit 1
fi
echo "import guard: clean"
```

```text
import guard: clean
```

The graph does not import `internal/legacy`, so the guard is clean and exits 0.
Add such an import anywhere in the transitive graph and `grep -qx` matches, the
guard prints the offending path, and it exits non-zero — the CI hook that keeps a
banned dependency out of the build.

## Review

The module is correct when `go test ./...` passes (covering `guard.Forbidden` on
both a clean and a dirty list) and `go list -deps ./cmd/demo` includes
`internal/circle` and `fmt`. The teaching point is that the graph is *data*:
`-deps`, `-json`, and `-f` expose it to scripts, and a few lines turn that into a
supply-chain guard. In real module mode, layer `go list -m -u all` and
`-m -retracted` on top to flag upgradable and retracted dependencies. The trap is
treating dependency hygiene as a manual review task when `go list` makes it a
mechanical, enforceable gate.

## Resources

- [Command go — go list](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — `-deps`, `-json`, `-f`, and the package/module fields.
- [Command go — module queries](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — `-m all`, `-m -u all`, `-m -retracted`.
- [text/template](https://pkg.go.dev/text/template) — the template language `go list -f` evaluates.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-go-env-and-cross-compile-matrix.md](07-go-env-and-cross-compile-matrix.md) | Next: [09-version-injection-and-reproducible-builds.md](09-version-injection-and-reproducible-builds.md)
