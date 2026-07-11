# Exercise 10: Test And Vet Every Module (./... Does Not Cross Modules)

The sharpest multi-module gotcha: `go test ./...` and `go vet ./...` stop at the
first nested `go.mod`. Run them from the workspace root and they exercise exactly
one module and silently skip every sibling — so a broken module passes CI while
the gate reports green. A correct multi-module gate enumerates the modules from
`go.work` and runs test-and-vet *inside each one*. This exercise builds that
enumeration and the per-module sweep a CI script must run.

## What you'll build

```text
ci/                            module: example.com/monorepo/ci
  go.mod                       go 1.26
  ci.go                        parse go.work json; enumerate modules; build the sweep
  ci_test.go                   proves the root ./... misses siblings; the sweep covers all
  cmd/
    demo/
      main.go                  prints the per-module test+vet commands CI runs
```

- Files: `ci.go`, `ci_test.go`, `cmd/demo/main.go`.
- Implement: `ModuleDirs(wf)` (sorted use paths), `RootSweepCovers(wf)` (what a root `./...` reaches), and `SweepCommands(wf)` (one test+vet command per module).
- Test: a root `./...` covers no sibling module; the per-module sweep covers every module exactly once.
- Verify: the enumerated loop runs test+vet in each module; a deliberately failing test in a non-root module is caught by the loop, missed by the root sweep.

Set up the module:

```bash
mkdir -p ~/monorepo/ci/cmd/demo
cd ~/monorepo/ci
go mod init example.com/monorepo/ci
go mod edit -go=1.26
```

### Why the root sweep lies, and what CI must do instead

`./...` is a package pattern, and package patterns never cross a module boundary.
From the workspace root, `go test ./...` expands to the packages of the module
rooted at the current directory only; the sibling modules listed in `go.work`
each have their own `go.mod`, so `./...` treats them as out of scope and walks
right past them. Concretely, drop a failing test into `./services/billing`:

```text
# services/billing/billing_test.go
func TestAlwaysFails(t *testing.T) { t.Fatal("boom") }
```

Then from the workspace root:

```bash
go test ./...              # exits 0 — never descended into services/billing
```

The gate is green and the broken module ships. The fix is to iterate the modules
`go.work` lists and run the sweep inside each:

```bash
# enumerate the use paths and sweep each module
for dir in $(go work edit -json | jq -r '.Use[].DiskPath'); do
	( cd "$dir" && go vet ./... && go test -race -count=1 ./... ) || exit 1
done
```

Now `services/billing`'s failing test runs and the loop exits non-zero. The gated
artifact builds exactly that plan from the parsed workspace: the module directories
to visit and the command to run in each. `RootSweepCovers` encodes the trap — a
root `./...` reaches only a module rooted at `.`, so with sibling-only `use` paths
it covers nothing — while `SweepCommands` produces the per-module commands that do
cover everything.

Create `ci.go`:

```go
// ci.go
package ci

import (
	"encoding/json"
	"fmt"
	"sort"
)

// WorkFile mirrors the fields of `go work edit -json` this tool needs.
type WorkFile struct {
	Go  string
	Use []Use
}

// Use is one go.work use entry.
type Use struct {
	DiskPath   string
	ModulePath string
}

// ParseWorkFile decodes `go work edit -json` output.
func ParseWorkFile(data []byte) (*WorkFile, error) {
	var wf WorkFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parsing go.work json: %w", err)
	}
	return &wf, nil
}

// ModuleDirs returns the workspace's module directories, sorted.
func (wf *WorkFile) ModuleDirs() []string {
	dirs := make([]string, len(wf.Use))
	for i, u := range wf.Use {
		dirs[i] = u.DiskPath
	}
	sort.Strings(dirs)
	return dirs
}

// RootSweepCovers returns the module directories a single `go test ./...` run
// from the workspace root actually reaches: only a module rooted at "." Sibling
// modules are skipped because ./... never crosses a module boundary.
func (wf *WorkFile) RootSweepCovers() []string {
	var covered []string
	for _, dir := range wf.ModuleDirs() {
		if dir == "." || dir == "./" {
			covered = append(covered, dir)
		}
	}
	return covered
}

// SweepCommands returns the per-module command CI must run so every module is
// vetted and tested, since the root ./... does not descend into siblings.
func (wf *WorkFile) SweepCommands() []string {
	dirs := wf.ModuleDirs()
	cmds := make([]string, len(dirs))
	for i, dir := range dirs {
		cmds[i] = fmt.Sprintf("(cd %s && go vet ./... && go test -race -count=1 ./...)", dir)
	}
	return cmds
}
```

### The demo

The demo parses a three-module workspace (none at the root) and prints what the
root sweep covers versus the per-module commands CI must run.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/monorepo/ci"
)

func main() {
	const jsonOut = `{
		"Go": "1.26",
		"Use": [
			{"DiskPath": "./text"},
			{"DiskPath": "./services/greeter"},
			{"DiskPath": "./services/billing"}
		]
	}`
	wf, err := ci.ParseWorkFile([]byte(jsonOut))
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("root ./... covers %d modules\n", len(wf.RootSweepCovers()))
	for _, c := range wf.SweepCommands() {
		fmt.Println(c)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
root ./... covers 0 modules
(cd ./services/billing && go vet ./... && go test -race -count=1 ./...)
(cd ./services/greeter && go vet ./... && go test -race -count=1 ./...)
(cd ./text && go vet ./... && go test -race -count=1 ./...)
```

### Tests

The first test pins the gotcha: with sibling-only `use` paths, a root `./...`
covers zero modules. The second proves the sweep covers every module exactly once —
so the failing test in `services/billing` is caught by the loop even though the
root sweep misses it.

Create `ci_test.go`:

```go
// ci_test.go
package ci

import (
	"strings"
	"testing"
)

const workspace = `{
	"Go": "1.26",
	"Use": [
		{"DiskPath": "./text"},
		{"DiskPath": "./services/greeter"},
		{"DiskPath": "./services/billing"}
	]
}`

func TestRootSweepMissesSiblings(t *testing.T) {
	t.Parallel()
	wf, err := ParseWorkFile([]byte(workspace))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if covered := wf.RootSweepCovers(); len(covered) != 0 {
		t.Fatalf("root ./... covers %v; want none (siblings are skipped)", covered)
	}
}

func TestSweepCoversEveryModule(t *testing.T) {
	t.Parallel()
	wf, err := ParseWorkFile([]byte(workspace))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cmds := wf.SweepCommands()
	if len(cmds) != 3 {
		t.Fatalf("got %d commands, want 3", len(cmds))
	}
	for _, dir := range []string{"./text", "./services/greeter", "./services/billing"} {
		n := 0
		for _, c := range cmds {
			if strings.Contains(c, "cd "+dir+" ") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("module %s appears in %d commands, want exactly 1", dir, n)
		}
	}
}

func TestRootCoveredWhenRootIsModule(t *testing.T) {
	t.Parallel()
	const rooted = `{"Go":"1.26","Use":[{"DiskPath":"."},{"DiskPath":"./sub"}]}`
	wf, err := ParseWorkFile([]byte(rooted))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if covered := wf.RootSweepCovers(); len(covered) != 1 || covered[0] != "." {
		t.Fatalf("RootSweepCovers = %v, want [.]", covered)
	}
}
```

## Review

The load-bearing fact is that `./...` is a package pattern that stops at module
boundaries, so a single `go test ./...`/`go vet ./...` at the workspace root
exercises at most the module rooted there and silently skips every sibling — a
green gate over broken code. `RootSweepCovers` makes the trap explicit (zero
modules when the `use` paths are all siblings), and `SweepCommands` builds the
correct gate: one `go vet`/`go test -race` invocation per module enumerated from
`go.work`. In CI, drive the loop from `go work edit -json` so the module list is
derived from the workspace rather than hand-maintained; the deliberately failing
test in `services/billing` is caught by the loop and missed by the root sweep,
which is the whole reason the loop exists.

## Resources

- [`go help packages`](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns) — the `./...` pattern and why it does not cross module boundaries.
- [go command — Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — `go work edit -json` for enumerating the workspace's modules.
- [`go test`](https://pkg.go.dev/cmd/go#hdr-Test_packages) — running tests per package pattern within a single module.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../06-dependency-management/00-concepts.md](../06-dependency-management/00-concepts.md)
