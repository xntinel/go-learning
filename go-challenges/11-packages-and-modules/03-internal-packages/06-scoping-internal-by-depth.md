# Exercise 6: Placement Depth Controls Blast Radius

The single most important design decision about an `internal` directory is how deep
to place it, because depth is the allow-list. An `internal` at the module root is
importable by the whole module; an `internal` nested under `service/` is importable
only within `service/`. This exercise proves the rule empirically: it builds fixture
packages at different depths and asserts, via the real toolchain, exactly who can
and cannot import each.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
depthprobe/                   module example.com/depthprobe
  go.mod
  probe.go                    GoAvailable, Fixture, BuildPkg (shell out to go build)
  probe_test.go               assert who can import a module-root vs a deep internal
  cmd/demo/main.go            runnable demo printing the three build outcomes
```

- Files: `probe.go`, `probe_test.go`, `cmd/demo/main.go`.
- Implement: a `Fixture` laying out a module-root `internal/platform` and a deep `service/internal/detail`, with importers at three positions; a `BuildPkg` that runs `go build ./<pkg>`.
- Test: assert the module-root internal is importable from a root-level package, the deep internal is importable from within `service/`, and the deep internal is rejected from a package outside `service/`.
- Verify: `go test -count=1 -race ./...`

### The rule stated as a tree

The allow-list of an `internal` directory is the subtree rooted at its immediate
parent. Read the fixture as two independent boundaries in one module
`example.com/fix`:

```text
fix/
  internal/platform/          parent = module root  -> importable by ALL of fix
  rootuser/                   imports platform      -> LEGAL (rootuser is under the root)
  service/
    internal/detail/          parent = service      -> importable only within service/
    api/                      imports detail        -> LEGAL (api is under service/)
  outsider/                   imports detail        -> ILLEGAL (outsider is not under service/)
```

`internal/platform` sits at the module root, so its parent is the root and the
allow-list is the entire module; `rootuser`, a plain root-level package, imports it
and builds. `service/internal/detail` sits two levels down, so its parent is
`service` and the allow-list is only `service` and its subtree; `service/api` is
inside that subtree and imports it fine, but `outsider` — a sibling of `service` at
the module root — is outside the subtree, so the toolchain rejects its import with
`use of internal package ... not allowed`.

The design lesson is direct: if you want a helper reachable everywhere in the module,
put its `internal` at the root; if you want it reachable only within one subsystem,
push the `internal` down into that subsystem. Too shallow leaks it wider than
intended; too deep locks out a caller who should have had it. The test below turns
each arrow in that tree into an assertion, so the rule is not a claim you memorize —
it is a fact the toolchain confirms.

Create `probe.go`:

```go
// Package depthprobe builds fixture modules with the go toolchain to demonstrate
// how internal-directory depth controls which packages may import it.
package depthprobe

import (
	"os"
	"os/exec"
	"path/filepath"
)

// GoAvailable reports whether a go toolchain is on PATH.
func GoAvailable() bool {
	_, err := exec.LookPath("go")
	return err == nil
}

// Fixture returns a module with a module-root internal (platform) and a deep
// internal (service/internal/detail), plus importers at three positions.
func Fixture() map[string]string {
	m := map[string]string{}
	m["go.mod"] = "module example.com/fix\n\ngo 1.21\n"
	m["internal/platform/platform.go"] = "package platform\n\nconst Region = \"us-east\"\n"
	m["rootuser/rootuser.go"] = "package rootuser\n\nimport _ \"example.com/fix/internal/platform\"\n"
	m["service/internal/detail/detail.go"] = "package detail\n\nconst Key = \"k\"\n"
	m["service/api/api.go"] = "package api\n\nimport _ \"example.com/fix/service/internal/detail\"\n"
	m["outsider/outsider.go"] = "package outsider\n\nimport _ \"example.com/fix/service/internal/detail\"\n"
	return m
}

// BuildPkg writes files under dir and runs `go build ./<pkg>` there, returning
// combined output and the command error (non-nil on a build failure).
func BuildPkg(dir string, files map[string]string, pkg string) (string, error) {
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("go", "build", "./"+pkg)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
```

### The runnable demo

The demo builds all three importers and prints who succeeded, making the depth rule
visible at a glance.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strings"

	"example.com/depthprobe"
)

func main() {
	if !depthprobe.GoAvailable() {
		fmt.Println("go toolchain not found on PATH")
		return
	}
	dir, err := os.MkdirTemp("", "depthprobe-demo-")
	if err != nil {
		fmt.Println("mkdir temp:", err)
		return
	}
	defer os.RemoveAll(dir)

	files := depthprobe.Fixture()

	_, rootErr := depthprobe.BuildPkg(dir, files, "rootuser")
	_, apiErr := depthprobe.BuildPkg(dir, files, "service/api")
	outsiderOut, outsiderErr := depthprobe.BuildPkg(dir, files, "outsider")

	fmt.Println("root package imports module-root internal:", rootErr == nil)
	fmt.Println("service/api imports service internal:", apiErr == nil)
	fmt.Println("outsider imports service internal:", outsiderErr == nil)
	fmt.Println("outsider rejected with diagnostic:", strings.Contains(outsiderOut, "use of internal package"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
root package imports module-root internal: true
service/api imports service internal: true
outsider imports service internal: false
outsider rejected with diagnostic: true
```

### Tests

Each test asserts one arrow of the tree. The two positive tests prove that both a
module-root internal and a deep internal are importable from inside their respective
allow-lists; the negative test proves the deep internal rejects a package outside
`service/`, with the exact diagnostic. All skip when no `go` binary is present.

Create `probe_test.go`:

```go
package depthprobe

import (
	"strings"
	"testing"
)

func TestModuleRootInternalImportableByRootPackage(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}
	if out, err := BuildPkg(t.TempDir(), Fixture(), "rootuser"); err != nil {
		t.Fatalf("root package could not import module-root internal: %v\n%s", err, out)
	}
}

func TestDeepInternalImportableWithinSubtree(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}
	if out, err := BuildPkg(t.TempDir(), Fixture(), "service/api"); err != nil {
		t.Fatalf("service/api could not import service internal: %v\n%s", err, out)
	}
}

func TestDeepInternalRejectsOutsider(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}
	out, err := BuildPkg(t.TempDir(), Fixture(), "outsider")
	if err == nil {
		t.Fatalf("outsider imported a deep internal it should not reach\n%s", out)
	}
	if !strings.Contains(out, "use of internal package") {
		t.Fatalf("build failed without the internal diagnostic; got:\n%s", out)
	}
}
```

## Review

The three tests together pin the depth rule from both sides: a module-root internal
reaches the whole module, a deep internal reaches only its subtree, and a package
outside that subtree is rejected with the toolchain's own diagnostic. If you can
predict each outcome from the tree before running the test, you have internalized
the one fact that matters — the allow-list is the subtree rooted at the parent of
`internal`.

The mistake this exercise inoculates against is choosing depth by habit rather than
by intended reach. Dropping `internal` at the root "because that is where I have
seen it" leaks a subsystem helper to the whole module; burying it too deep locks out
a sibling that legitimately needs it, and that sibling's author then exports
something or copies code to route around the wall. Choose the depth so the parent
subtree is exactly the set of packages you mean to allow.

## Resources

- [Go Modules Reference: Internal packages](https://go.dev/ref/mod#internal-packages) — the parent-subtree rule at any depth.
- [cmd/go: Internal Directories](https://pkg.go.dev/cmd/go#hdr-Internal_Directories) — the toolchain's statement of importability.
- [`os/exec`](https://pkg.go.dev/os/exec) — driving `go build` as a subprocess.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-internal-config-loader.md](05-internal-config-loader.md) | Next: [07-export-test-white-box-internal.md](07-export-test-white-box-internal.md)
