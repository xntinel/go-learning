# Exercise 9: An Architecture Fitness Test That Fails Illegal Imports

This is the real on-the-job artifact: a self-checking test that walks every
package under `internal/`, reads its imports statically, and fails the build if a
forbidden edge exists — `internal/core` importing an adapter, or a leaf helper
reaching back up the stack. The previous exercise guarded one edge; this turns the
whole layering rule into an enforced CI test rather than a wiki page that everyone
eventually ignores.

This module is fully self-contained: its own `go mod init`, a clean layered
subtree, the guard test itself, and a demo.

## What you'll build

```text
guarded/                       module github.com/example/guarded
  go.mod                       go 1.24
  internal/
    core/core.go               domain + port; imports only stdlib
    adapters/adapters.go       implements core; imports core
    platform/platform.go       leaf helper; imports only stdlib
    arch/arch_test.go          walks internal/, enforces the import ruleset
  cmd/demo/main.go             runnable: wires the layers, prints a greeting
```

- Files: `internal/core/core.go`, `internal/adapters/adapters.go`, `internal/platform/platform.go`, `internal/arch/arch_test.go`, `cmd/demo/main.go`.
- Implement: a clean three-layer subtree plus a guard test with a ruleset of forbidden import prefixes per layer.
- Test: `TestLayering` walks `internal/` with `filepath.WalkDir`, calls `build.ImportDir` per package, and `t.Errorf`s every import that violates the ruleset (with package and offending import).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/guarded/internal/core ~/go-exercises/guarded/internal/adapters ~/go-exercises/guarded/internal/platform ~/go-exercises/guarded/internal/arch ~/go-exercises/guarded/cmd/demo
cd ~/go-exercises/guarded
go mod init github.com/example/guarded
go mod edit -go=1.24
```

### Turning a layering rule into a test

The rules are a data structure, not prose. Each layer has a set of import-path
prefixes it may *not* depend on:

- `internal/core` — the domain — may not import `internal/adapters` or
  `internal/platform`. The domain depends on nobody inward.
- `internal/adapters` may not import `internal/platform`. Adapters implement core
  ports; they should not reach sideways into platform helpers.
- `internal/platform` — a leaf — may not import `internal/core` or
  `internal/adapters`. Cross-cutting helpers must not know the domain.

The guard test encodes that as a map from a package's import path to the list of
forbidden prefixes, then walks the tree and checks reality against it. The
mechanism is `go/build`: for each directory under `internal/`, `build.ImportDir`
returns a `*build.Package` whose `.Imports` (and `.TestImports`) are the literal
import strings the source uses — full paths like
`github.com/example/guarded/internal/core`. Because those are the exact strings
written in the source, they are reliable regardless of how `go/build` resolves the
package's *own* import path in module mode. The package's own identity is computed
structurally instead: module path plus its directory relative to the module root.

One supporting fact makes the walk work under `go test`, whose working directory
is the test's own package: the test finds the module root by walking up from its
working directory to the nearest `go.mod` and parsing the `module` line for the
path. Reading `go.mod` directly, rather than shelling out to `go list -m` / `go
env GOMOD`, keeps the guard fully self-contained — no nested `go` process, so it
does not depend on toolchain download or network timing. With the module path and
root in hand, the walk starts at `<root>/internal` and, for each package
directory, derives `self = modulePath + "/" + relpath` and looks up its forbidden
prefixes. Every import that starts with a forbidden prefix is a `t.Errorf` naming
both the package and the offending import — so a violation report tells you exactly
which edge to cut. The test uses `t.Errorf`, not `t.Fatalf`, so a single run lists
*all* violations, not just the first.

Create `internal/core/core.go`:

```go
package core

import "fmt"

// Greeter is the port the domain depends on.
type Greeter interface {
	Greet(name string) string
}

// Service orchestrates a Greeter. It imports no infrastructure.
type Service struct{ g Greeter }

// NewService injects the port implementation.
func NewService(g Greeter) *Service { return &Service{g: g} }

// Welcome produces the domain result.
func (s *Service) Welcome(name string) string {
	return fmt.Sprintf("welcome, %s", s.g.Greet(name))
}
```

Create `internal/adapters/adapters.go`:

```go
package adapters

import "github.com/example/guarded/internal/core"

// PrefixGreeter satisfies core.Greeter. It depends inward on core only.
type PrefixGreeter struct{ Prefix string }

// Greet implements core.Greeter.
func (p PrefixGreeter) Greet(name string) string { return p.Prefix + name }

var _ core.Greeter = PrefixGreeter{}
```

Create `internal/platform/platform.go`:

```go
package platform

import "fmt"

// Banner is a cross-cutting helper that knows nothing about the domain.
func Banner(name string) string { return fmt.Sprintf("=== %s ===", name) }
```

### The guard test

The test is the deliverable. It reads the ruleset, walks `internal/`, and reports
every forbidden edge. It is deterministic, needs no network, and runs under
`-race`.

Create `internal/arch/arch_test.go`:

```go
package arch

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbidden maps a package import path to import prefixes it must not depend on.
// Packages not listed have no restrictions.
func forbidden(module string) map[string][]string {
	core := module + "/internal/core"
	adapters := module + "/internal/adapters"
	platform := module + "/internal/platform"
	return map[string][]string{
		core:     {adapters, platform},
		adapters: {platform},
		platform: {core, adapters},
	}
}

// moduleInfo walks up from the test's working directory to the nearest go.mod,
// returning the module path (parsed from its first module line) and root
// directory. Reading go.mod directly keeps the guard self-contained: no nested
// go invocation, so nothing depends on toolchain or network timing.
func moduleInfo(t *testing.T) (module, root string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, rerr := os.ReadFile(gomod); rerr == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if mp, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return strings.TrimSpace(mp), dir
				}
			}
			t.Fatalf("no module line in %s", gomod)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found walking up from the test directory")
		}
		dir = parent
	}
}

func TestLayering(t *testing.T) {
	module, root := moduleInfo(t)
	rules := forbidden(module)

	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		pkg, ierr := build.ImportDir(path, 0)
		if ierr != nil {
			// Not a buildable package directory (no .go files); skip it.
			return nil //nolint:nilerr
		}

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		self := module + "/" + filepath.ToSlash(rel)

		banned := rules[self]
		if banned == nil {
			return nil
		}

		imports := append([]string{}, pkg.Imports...)
		imports = append(imports, pkg.TestImports...)
		for _, imp := range imports {
			for _, bad := range banned {
				if imp == bad || strings.HasPrefix(imp, bad+"/") {
					t.Errorf("layering violation: %s must not import %s", self, imp)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}
```

### The runnable demo

The demo wires the clean layers and prints a greeting, so there is something to
run alongside the guard that protects it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/guarded/internal/adapters"
	"github.com/example/guarded/internal/core"
	"github.com/example/guarded/internal/platform"
)

func main() {
	fmt.Println(platform.Banner("guarded"))
	svc := core.NewService(adapters.PrefixGreeter{Prefix: "Ms. "})
	fmt.Println(svc.Welcome("Grace"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== guarded ===
welcome, Ms. Grace
```

## Review

The guard is correct when it stays green on the clean tree and would go red the
instant a forbidden edge appears — add `import ".../internal/adapters"` to
`core.go` and `TestLayering` fails, naming the package and the import. The design
choices that make it trustworthy: read `.Imports` (the literal source strings, so
resolution quirks cannot hide an edge), compute each package's identity from its
path relative to the module root, and use `t.Errorf` so one run surfaces every
violation. This is strictly stronger than the `internal/` rule, which polices only
*cross-module* imports and says nothing about layers within a module. Keep the
ruleset next to the code it governs and it becomes the living definition of the
architecture. Run `go test -race ./...` to confirm the clean tree passes.

## Resources

- [`go/build`](https://pkg.go.dev/go/build) — `ImportDir`, `Package.Imports`, and `Package.TestImports` for reading a package's edges.
- [`path/filepath.WalkDir`](https://pkg.go.dev/path/filepath#WalkDir) — walking the `internal/` tree.
- [Organizing a Go module](https://go.dev/doc/modules/layout) — the layering conventions this test enforces.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-go-work-vendor-reproducible-ci.md](10-go-work-vendor-reproducible-ci.md)
