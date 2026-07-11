# Exercise 10: Produce a reproducible workspace vendor tree for hermetic CI

Hermetic CI builds from a checked-in `vendor/` tree and never touches the network.
For a monorepo built as a workspace, `go work vendor` (Go 1.22+) materializes one
`vendor/` directory covering every workspace module at once, and `vendor/modules.txt`
records the exact module set. This exercise builds the helper CI uses to verify that
tree — a parser for `modules.txt` that reports the vendored modules — and explains
when whole-workspace vendoring beats per-module vendoring.

## What you'll build

```text
vendorcheck/                  module: example.com/mono/vendorcheck
  go.mod
  vendorcheck.go              ParseModulesTxt(data), ModulePaths(data), Contains
  vendorcheck_test.go         parses a real-shaped modules.txt
  cmd/demo/main.go            prints the vendored module set
```

- Files: `vendorcheck.go`, `vendorcheck_test.go`, `cmd/demo/main.go`.
- Implement: `ParseModulesTxt(data []byte) []Module` (path + version, in file order), `ModulePaths(data []byte) []string`, and `Contains(data []byte, path string) bool`.
- Test: parse a `modules.txt` with several modules and annotation lines, asserting the exact set of module paths and that a required module is present.
- Verify: `go build ./...` and `go test -race -count=1 ./...` (pure stdlib, offline).

Set up the module:

```bash
mkdir -p ~/go-exercises/vendorcheck/cmd/demo
cd ~/go-exercises/vendorcheck
go mod init example.com/mono/vendorcheck
```

### Two ways to vendor, and when each fits

`go mod vendor` copies one module's dependencies into that module's `vendor/`
directory. `go work vendor` copies **the whole workspace's** dependencies into a
single `vendor/` at the workspace root — every member module's requirements
merged into one tree. With a `vendor/` present the build defaults to
`-mod=vendor` (you can force it with `GOFLAGS=-mod=vendor`), and in that mode the
build consults *only* the vendored copies: no proxy, no network, fully hermetic
and reproducible.

For a monorepo you build and release as a workspace, `go work vendor` is the fit:
one tree, one `-mod=vendor` build, all services hermetic together. Per-module
`go mod vendor` fits the opposite topology — each module released and built on its
own, each carrying its own `vendor/`. Mixing them is the trap: a workspace with
per-module vendor directories does not give the workspace build a single coherent
tree, and `go work vendor` at the root is what CI actually wants.

Either way, `vendor/modules.txt` is the manifest. It lists every vendored module
as a `# path version` header, followed by `## explicit`-style annotations and the
package import paths under that module. A CI check parses it to assert the vendor
tree contains exactly the modules the build requires — the artifact this exercise
builds.

### Parsing modules.txt

The format is line-oriented. A line beginning with `# ` (hash, space) is a module
header: `# golang.org/x/mod v0.37.0`, optionally with a `=> replacement`. Lines
beginning with `## ` are annotations (`## explicit; go 1.23`). Every other
non-blank line is a package import path. The parser keys on the single-hash module
headers and pulls out the path and version.

Create `vendorcheck.go`:

```go
package vendorcheck

import (
	"bufio"
	"bytes"
	"strings"
)

// Module is one vendored module recorded in vendor/modules.txt.
type Module struct {
	Path    string
	Version string
}

// ParseModulesTxt parses a vendor/modules.txt and returns the vendored modules in
// file order. It reads the "# path version" headers and ignores the "## ..."
// annotation lines and the package import paths between headers.
func ParseModulesTxt(data []byte) []Module {
	var mods []Module

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		// Module headers start with a single "# "; annotations start with "## ".
		if !strings.HasPrefix(line, "# ") {
			continue
		}
		fields := strings.Fields(line[len("# "):])
		if len(fields) == 0 {
			continue
		}
		m := Module{Path: fields[0]}
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "v") {
			m.Version = fields[1]
		}
		mods = append(mods, m)
	}
	return mods
}

// ModulePaths returns just the module paths, in file order.
func ModulePaths(data []byte) []string {
	mods := ParseModulesTxt(data)
	paths := make([]string, len(mods))
	for i, m := range mods {
		paths[i] = m.Path
	}
	return paths
}

// Contains reports whether the vendor tree records the given module path.
func Contains(data []byte, path string) bool {
	for _, m := range ParseModulesTxt(data) {
		if m.Path == path {
			return true
		}
	}
	return false
}
```

### The runnable demo

The demo parses a small, real-shaped `modules.txt` and prints the vendored module
set plus a presence check for one required dependency.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mono/vendorcheck"
)

func main() {
	data := []byte("# golang.org/x/mod v0.37.0\n" +
		"## explicit; go 1.23\n" +
		"golang.org/x/mod/modfile\n" +
		"golang.org/x/mod/semver\n" +
		"# golang.org/x/sync v0.8.0\n" +
		"## explicit; go 1.18\n" +
		"golang.org/x/sync/errgroup\n")

	for _, m := range vendorcheck.ParseModulesTxt(data) {
		fmt.Printf("%s %s\n", m.Path, m.Version)
	}
	fmt.Printf("has x/mod: %v\n", vendorcheck.Contains(data, "golang.org/x/mod"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
golang.org/x/mod v0.37.0
golang.org/x/sync v0.8.0
has x/mod: true
```

### Tests

The test parses a `modules.txt` shaped like a real one — module headers,
`## explicit` annotations, and package lines — and asserts the exact set of module
paths (so a stray package line is never mistaken for a module) and that a required
module is reported present.

Create `vendorcheck_test.go`:

```go
package vendorcheck

import (
	"slices"
	"testing"
)

const sample = `# golang.org/x/mod v0.37.0
## explicit; go 1.23
golang.org/x/mod/modfile
golang.org/x/mod/module
golang.org/x/mod/semver
# golang.org/x/sync v0.8.0
## explicit; go 1.18
golang.org/x/sync/errgroup
# golang.org/x/text v0.14.0
## explicit; go 1.18
golang.org/x/text/unicode/norm
`

func TestModulePaths(t *testing.T) {
	t.Parallel()

	got := ModulePaths([]byte(sample))
	want := []string{
		"golang.org/x/mod",
		"golang.org/x/sync",
		"golang.org/x/text",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ModulePaths = %v, want %v", got, want)
	}
}

func TestParseModulesTxtVersions(t *testing.T) {
	t.Parallel()

	mods := ParseModulesTxt([]byte(sample))
	if len(mods) != 3 {
		t.Fatalf("parsed %d modules, want 3", len(mods))
	}
	if mods[0].Path != "golang.org/x/mod" || mods[0].Version != "v0.37.0" {
		t.Errorf("first module = %+v, want golang.org/x/mod v0.37.0", mods[0])
	}
}

func TestContains(t *testing.T) {
	t.Parallel()

	if !Contains([]byte(sample), "golang.org/x/sync") {
		t.Error("Contains missed golang.org/x/sync")
	}
	// A package path under a module is not itself a module entry.
	if Contains([]byte(sample), "golang.org/x/mod/modfile") {
		t.Error("Contains matched a package path as a module")
	}
}
```

## Review

The parser is correct when it counts module headers, not packages: a `modules.txt`
with three `# path version` lines and many package lines yields exactly three
modules, and a package import path like `golang.org/x/mod/modfile` is never
mistaken for a module. That distinction is the whole value of the check — CI
asserts the vendored *module set* matches what the build requires, so a missing or
extra module in the tree fails before a hermetic build silently uses the wrong
dependency.

The strategic mistake is vendoring at the wrong granularity. In a workspace, reach
for `go work vendor` to build one tree at the root and one `-mod=vendor` build for
all services; per-module `go mod vendor` is for modules released independently. Run
`go work vendor` and then `go build -mod=vendor ./...` in CI to confirm the tree is
complete and the build never touches the network.

## Resources

- [`cmd/go`: Vendoring](https://pkg.go.dev/cmd/go#hdr-Vendoring) — `-mod=vendor` and the `vendor/modules.txt` manifest.
- [`cmd/go`: Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — `go work vendor` for a whole-workspace tree.
- [Go Modules Reference: vendoring](https://go.dev/ref/mod#vendoring) — how `modules.txt` is structured and consumed.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../../12-testing-ecosystem/01-your-first-test/01-your-first-test.md](../../12-testing-ecosystem/01-your-first-test/01-your-first-test.md)
