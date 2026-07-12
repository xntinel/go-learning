# Exercise 4: Package Patterns and Querying the Build Graph With go list

`go build ./...` answers "does everything compile?" but it does not tell you
*what* "everything" is. This exercise turns layout introspection into an
assertion: it builds a realistic four-package module, then uses `go list` — via
`os/exec` from a test — to enumerate the module's packages and fail CI if the set
ever drifts (a stray `main` package, a misplaced directory, a package that
vanished).

This module is fully self-contained: its own `go mod init`, a realistic layout,
and a test that shells out to the `go` tool to inspect its own build graph.

## What you'll build

```text
myapp/                         module github.com/example/myapp
  go.mod                       go 1.24
  internal/
    config/config.go           AppName, Version
    greeting/greeting.go       Greet
    layout/layout_test.go      go list -f '{{.ImportPath}}' -> asserted set
  cmd/
    cli/main.go                one binary
    server/main.go             another binary
    demo/main.go               runnable: prints the module's package list
```

- Files: `internal/config/config.go`, `internal/greeting/greeting.go`, `internal/layout/layout_test.go`, `cmd/cli/main.go`, `cmd/server/main.go`, `cmd/demo/main.go`.
- Implement: a realistic multi-binary layout; a test that resolves the module root and runs `go list -f '{{.ImportPath}}'` over `./...`.
- Test: `TestPackageSet` parses the `go list` output into a set and asserts it equals the exact expected import paths, so an accidental package fails the build.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The build graph is data you can assert on

`./...` expands to packages, recursively, honoring build constraints and the
`_test.go` rule exactly as the real build does — which is why `go build ./...`,
`go vet ./...`, and `go test ./...` are the CI-safe forms. But the same pattern
plugs into `go list`, which prints structured facts about each matched package
instead of building it. `go list -f '{{.ImportPath}}: {{.Name}}'` prints one line
per package (import path and package name); `go list -json` dumps a full record
per package; `go list -deps` prints the transitive dependency closure. These turn
"the module is laid out the way we agreed" from a hope into a check.

This exercise makes that check a test. `TestPackageSet` runs `go list -f
'{{.ImportPath}}'` over `./...`, parses stdout into a set, and compares it against
the exact expected import paths. If someone adds a stray `package main` at the
root, drops a package in the wrong directory, or deletes one, the set no longer
matches and the test fails — the layout invariant is now enforced by CI, not by
review.

Two details make it robust. First, `go test` runs each test with its working
directory set to that test's *package* directory (`internal/layout` here), not
the module root, so a bare `go list ./...` would list only `internal/layout`. The
test therefore resolves the module root first, with `go env GOMOD` (the path to
`go.mod`; its parent directory is the root), and runs `go list` from there.
Second, the expected set is the *complete* package set of this module, including
`internal/layout` itself and `cmd/demo` — six packages. Listing them all, rather
than a hand-wavy "the four app packages," is what makes the assertion exact: any
drift, in either direction, fails.

Create `internal/config/config.go`:

```go
package config

const (
	// AppName is the application name shared across binaries.
	AppName = "myapp"
	// Version is the application version.
	Version = "0.1.0"
)
```

Create `internal/greeting/greeting.go`:

```go
package greeting

import "fmt"

// Greet formats a fixed greeting for name.
func Greet(name string) string {
	return fmt.Sprintf("[%s %s] %s says hello", "myapp", "0.1.0", name)
}
```

Create `cmd/cli/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/myapp/internal/greeting"
)

func main() {
	fmt.Println(greeting.Greet("World"))
}
```

Create `cmd/server/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/myapp/internal/config"
)

func main() {
	fmt.Println("server", config.AppName, config.Version)
}
```

### The runnable demo

The demo runs the same `go list` query the test uses and prints the package list,
so you can see the build graph the assertion is guarding.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}}: {{.Name}}", "./...").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go list: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Print(string(out))
}
```

Run it from the module root:

```bash
go run ./cmd/demo
```

Expected output:

```
github.com/example/myapp/cmd/cli: main
github.com/example/myapp/cmd/demo: main
github.com/example/myapp/cmd/server: main
github.com/example/myapp/internal/config: config
github.com/example/myapp/internal/greeting: greeting
github.com/example/myapp/internal/layout: layout
```

### Tests

The test resolves the module root, runs `go list` there, parses the import paths
into a set, and asserts set equality against the expected six packages. It uses
`t.Context()` so the subprocess is cancelled if the test times out.

Create `internal/layout/layout_test.go`:

```go
package layout

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/example/myapp"

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		t.Fatalf("not in a module: GOMOD=%q", gomod)
	}
	return filepath.Dir(gomod)
}

func TestPackageSet(t *testing.T) {
	root := moduleRoot(t)

	cmd := exec.CommandContext(t.Context(), "go", "list", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}

	got := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			got[line] = true
		}
	}

	want := []string{
		modulePath + "/cmd/cli",
		modulePath + "/cmd/demo",
		modulePath + "/cmd/server",
		modulePath + "/internal/config",
		modulePath + "/internal/greeting",
		modulePath + "/internal/layout",
	}

	if len(got) != len(want) {
		t.Fatalf("package count = %d, want %d\ngot: %v", len(got), len(want), keys(got))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing expected package %q (got %v)", w, keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
```

## Review

The layout check is correct when the set `go list ./...` reports equals the set
the team agreed to — no more, no less. The test enforces both directions: a
missing package fails the `got[w]` loop, and an extra package fails the length
check, so a stray root `main` or a misplaced directory breaks CI immediately. The
subtlety to remember is the working directory: `go test` runs in the test's own
package directory, so the query must be anchored at the module root via `go env
GOMOD`, not run blindly. Prefer `go list` over hand-parsing the filesystem — it
sees exactly what the build sees, including constraint-excluded files, so your
assertion matches reality. Run `go test -race ./...` to confirm.

## Resources

- [`go list` documentation](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — `-f` templates, `-json`, and `-deps`.
- [`text/template`](https://pkg.go.dev/text/template) — the template language `go list -f` uses.
- [`go help packages`](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns) — how `./...` expands to a package list.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-internal-boundary-enforcement.md](05-internal-boundary-enforcement.md)
