# Exercise 2: Audit the module and its dependency graph

Every module ships build-introspection tooling whether the team writes it or not —
the `go` command already answers "what is my module path," "what is the build
list," and "who pulled this dependency in." Here you wrap those answers in a small
ops helper that shells out to `go list`, parses the JSON, and gives your service a
programmatic view of its own module identity.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
billing/                    independent module: example.com/billing
  go.mod                    the module under audit
  buildaudit/
    buildaudit.go           ModuleInfo() and Packages() over `go list`; ErrGoToolMissing
    buildaudit_test.go      runs the helpers against the current module; t.Skip if no go
  cmd/
    demo/
      main.go               runnable: print module path, Go version, and packages
```

- Files: `buildaudit/buildaudit.go`, `cmd/demo/main.go`, `buildaudit/buildaudit_test.go`.
- Implement: `ModuleInfo() (Module, error)` shelling out to `go list -m -json` and unmarshalling `Path`/`GoVersion`, plus `Packages() ([]string, error)` over `go list ./...`.
- Test: assert `ModuleInfo().Path == "example.com/billing"` and that `Packages()` enumerates the root package; skip gracefully with `t.Skip` when the `go` binary is absent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/billing/buildaudit ~/go-exercises/billing/cmd/demo
cd ~/go-exercises/billing
go mod init example.com/billing
go mod edit -go=1.26
```

### Why shell out, and what to parse

There is no in-process Go API that returns "my module path" for the module you are
*building* — `runtime/debug.ReadBuildInfo` (Exercise 4) reports the *linked* binary's
metadata, which is a different question and unavailable at `go test` time in a
meaningful way. The authoritative source for the source-tree view is the `go`
command itself, and it speaks JSON: `go list -m -json` prints a record for the main
module with `Path`, `Main`, `Dir`, `GoMod`, and `GoVersion`. Parsing that one call
gives you both the module path and the declared Go version — there is no need for a
second invocation, because `-json` already carries `GoVersion`.

Shelling out to a tool has one honest failure mode this helper must handle: the
`go` binary may not be on `PATH` (a stripped production image, a restricted CI
runner). The helper checks with `exec.LookPath("go")` and returns a sentinel
`ErrGoToolMissing` so callers — and tests — can branch on that cause with
`errors.Is` and skip rather than crash. That is the difference between an ops helper
and a script.

Beyond the single-module view, the toolchain exposes the full graph, and a senior
audit uses all three: `go list -m all` prints the entire build list (every module,
direct and indirect, at its MVS-selected version); `go list ./...` enumerates the
*packages* in the main module; and `go mod graph` prints the requirement edges as
`from@version to@version` pairs, which is how you answer "who requires this old
version of X" — grep the right-hand side for the module and read off who points at
it. The helper wraps the first and third of those (`-m -json` and `./...`); the demo
shows how the same shape extends to the rest.

Create `buildaudit/buildaudit.go`:

```go
package buildaudit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrGoToolMissing is returned when the go binary is not on PATH, so callers can
// skip introspection gracefully instead of crashing.
var ErrGoToolMissing = errors.New("buildaudit: go tool not found in PATH")

// Module is the subset of `go list -m -json` this helper cares about.
type Module struct {
	Path      string `json:"Path"`
	GoVersion string `json:"GoVersion"`
	Main      bool   `json:"Main"`
}

// ModuleInfo runs `go list -m -json` for the current main module and returns its
// path and declared Go version. It reports ErrGoToolMissing if go is absent.
func ModuleInfo() (Module, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return Module{}, ErrGoToolMissing
	}
	out, err := exec.Command("go", "list", "-m", "-json").Output()
	if err != nil {
		return Module{}, fmt.Errorf("buildaudit: go list -m -json: %w", err)
	}
	var m Module
	if err := json.Unmarshal(out, &m); err != nil {
		return Module{}, fmt.Errorf("buildaudit: decode module json: %w", err)
	}
	return m, nil
}

// Packages runs `go list ./...` and returns the import paths of every package in
// the main module. It reports ErrGoToolMissing if go is absent.
func Packages() ([]string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, ErrGoToolMissing
	}
	out, err := exec.Command("go", "list", "./...").Output()
	if err != nil {
		return nil, fmt.Errorf("buildaudit: go list ./...: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/billing/buildaudit"
)

func main() {
	m, err := buildaudit.ModuleInfo()
	if err != nil {
		if errors.Is(err, buildaudit.ErrGoToolMissing) {
			fmt.Println("go tool unavailable; skipping audit")
			return
		}
		fmt.Println("module audit failed:", err)
		return
	}
	fmt.Printf("module: %s\n", m.Path)
	fmt.Printf("go: %s\n", m.GoVersion)

	pkgs, err := buildaudit.Packages()
	if err != nil {
		fmt.Println("package audit failed:", err)
		return
	}
	fmt.Printf("packages: %d\n", len(pkgs))
	for _, p := range pkgs {
		fmt.Printf("  %s\n", p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
module: example.com/billing
go: 1.26
packages: 2
  example.com/billing/buildaudit
  example.com/billing/cmd/demo
```

### Tests

The tests run the helpers against the current module — `go test` can invoke `go
list` because the toolchain is on the runner. They skip with `t.Skip` when `go` is
absent (branching on the sentinel), so the suite is honest on a stripped image
rather than red.

Create `buildaudit/buildaudit_test.go`:

```go
package buildaudit

import (
	"errors"
	"slices"
	"testing"
)

func TestModuleInfo(t *testing.T) {
	t.Parallel()
	m, err := ModuleInfo()
	if errors.Is(err, ErrGoToolMissing) {
		t.Skip("go tool not on PATH; skipping module introspection")
	}
	if err != nil {
		t.Fatalf("ModuleInfo() error: %v", err)
	}
	if m.Path != "example.com/billing" {
		t.Fatalf("module path = %q, want example.com/billing", m.Path)
	}
	if !m.Main {
		t.Fatalf("expected the current module to report Main=true")
	}
	if m.GoVersion == "" {
		t.Fatalf("GoVersion is empty; expected the declared go directive")
	}
}

func TestPackages(t *testing.T) {
	t.Parallel()
	pkgs, err := Packages()
	if errors.Is(err, ErrGoToolMissing) {
		t.Skip("go tool not on PATH; skipping package enumeration")
	}
	if err != nil {
		t.Fatalf("Packages() error: %v", err)
	}
	if !slices.Contains(pkgs, "example.com/billing/buildaudit") {
		t.Fatalf("Packages() = %v, want it to contain example.com/billing/buildaudit", pkgs)
	}
}
```

## Review

The helper is correct when it treats the `go` command as the source of truth and
handles its one real failure mode — an absent binary — as a typed cause rather than
a panic. `ModuleInfo` parses the `Path` and `GoVersion` straight out of `go list -m
-json`, which is why a second call for the Go version is unnecessary; `Packages`
splits `go list ./...` into import paths. The tests assert the module path is
`example.com/billing` and that the enumeration contains the root package, and they
`t.Skip` (never fail) when `go` is missing, so the suite is honest on a runner
without the toolchain. When you need the *transitive* view — direct versus indirect,
or who requires a stale version — reach for `go list -m all` and `go mod graph`; the
same shell-and-parse shape extends to both.

## Resources

- [`go list` reference](https://go.dev/ref/mod#go-list-m) — `-m`, `-json`, and the module fields.
- [`os/exec`](https://pkg.go.dev/os/exec) — `Command`, `Output`, and `LookPath`.
- [`go mod graph`](https://go.dev/ref/mod#go-mod-graph) — the requirement-edge view used to find who pulled a version in.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-major-version-v2-suffix.md](03-major-version-v2-suffix.md)
