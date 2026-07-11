# Exercise 9: Pin the build toolchain with go 1.24 tool directives

Build tools (`sqlc`, `mockgen`, `golangci-lint`) used to be pinned with a `tools.go`
blank-import hack plus `go install tool@latest` in CI — which drifts, because
`@latest` moves. Go 1.24 replaced that with `tool` directives in `go.mod`. This
exercise builds the checker that enforces them: every required tool must be present
as a `tool` directive backed by a `require`, and it models `go get -tool` adding a
line via `modfile.AddTool`.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
toolgate/                  independent module: example.com/toolgate
  go.mod                   go 1.26; requires golang.org/x/mod
  toolgate.go              AuditTools(data, want); AddTool(data, path) ([]byte, error)
  cmd/
    demo/
      main.go              audits a go.mod tool block, then adds a tool directive
  toolgate_test.go         missing/unbacked detection; AddTool then Format emits the line
```

- Files: `toolgate.go`, `cmd/demo/main.go`, `toolgate_test.go`.
- Implement: `AuditTools` reporting wanted tools absent from the `tool` block and `tool` directives with no backing `require`; `AddTool` that parses, calls `File.AddTool`, and returns `Format()` bytes.
- Test: a fixture `tool` block; assert missing/unbacked detection and that `AddTool` then `Format()` emits the new `tool` directive.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/toolgate/cmd/demo
cd ~/go-exercises/toolgate
go mod init example.com/toolgate
go get golang.org/x/mod
```

### Tool directives, and what "backed by a require" means

A `tool` directive names an executable *package* path, and the module that supplies
it must be a `require`:

```text
require (
	github.com/sqlc-dev/sqlc v1.27.0
	go.uber.org/mock v0.5.0
)

tool (
	github.com/sqlc-dev/sqlc/cmd/sqlc
	go.uber.org/mock/mockgen
)
```

`go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0` adds both the `require`
(pinning the version) and the `tool` line (naming the package to run via
`go tool sqlc`). The checker enforces two invariants. First, every tool your team
depends on must actually be present as a `tool` directive — a *missing* one means a
developer will `go install @latest` instead and drift. Second, every `tool`
directive must be *backed by a require*: the tool package path must sit under some
required module's path (`github.com/sqlc-dev/sqlc/cmd/sqlc` under
`github.com/sqlc-dev/sqlc`), because without the require there is no pinned version.

`modfile.File.Tool` is the parsed `tool` block, each `*modfile.Tool` carrying a
`Path`. `File.AddTool(path)` appends a directive to a parsed file, and `File.Format`
renders the file back to bytes — that is the offline model of what `go get -tool`
does to `go.mod`.

Create `toolgate.go`:

```go
package toolgate

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// ToolAudit reports the state of the tool directives in a go.mod.
type ToolAudit struct {
	Present  []string // tool directives found
	Missing  []string // wanted tools with no tool directive
	Unbacked []string // tool directives with no backing require
}

// AuditTools parses go.mod and checks its tool directives against the wanted set:
// it reports wanted tools that are absent and tool directives not backed by a
// require. An audit with empty Missing and Unbacked is a pass.
func AuditTools(data []byte, want []string) (*ToolAudit, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}

	present := make(map[string]bool)
	audit := &ToolAudit{}
	for _, t := range f.Tool {
		present[t.Path] = true
		audit.Present = append(audit.Present, t.Path)
		if !backedByRequire(t.Path, f) {
			audit.Unbacked = append(audit.Unbacked, t.Path)
		}
	}
	for _, w := range want {
		if !present[w] {
			audit.Missing = append(audit.Missing, w)
		}
	}
	sort.Strings(audit.Present)
	sort.Strings(audit.Missing)
	sort.Strings(audit.Unbacked)
	return audit, nil
}

// backedByRequire reports whether the tool package path lies under some required
// module's path (so the tool has a pinned version).
func backedByRequire(toolPath string, f *modfile.File) bool {
	for _, req := range f.Require {
		if toolPath == req.Mod.Path || strings.HasPrefix(toolPath, req.Mod.Path+"/") {
			return true
		}
	}
	return false
}

// AddTool parses go.mod, adds a tool directive (as `go get -tool` would), and
// returns the reformatted go.mod bytes.
func AddTool(data []byte, toolPath string) ([]byte, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	if err := f.AddTool(toolPath); err != nil {
		return nil, fmt.Errorf("add tool %q: %w", toolPath, err)
	}
	out, err := f.Format()
	if err != nil {
		return nil, fmt.Errorf("format go.mod: %w", err)
	}
	return out, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/toolgate"
)

const goMod = `module example.com/toolgate

go 1.24

require github.com/sqlc-dev/sqlc v1.27.0

tool github.com/sqlc-dev/sqlc/cmd/sqlc
`

func main() {
	want := []string{
		"github.com/sqlc-dev/sqlc/cmd/sqlc",
		"go.uber.org/mock/mockgen",
	}
	audit, err := toolgate.AuditTools([]byte(goMod), want)
	if err != nil {
		panic(err)
	}
	fmt.Printf("present=%v\n", audit.Present)
	fmt.Printf("missing=%v\n", audit.Missing)
	fmt.Printf("unbacked=%v\n", audit.Unbacked)

	out, _ := toolgate.AddTool([]byte(goMod), "go.uber.org/mock/mockgen")
	fmt.Println("--- after go get -tool mockgen ---")
	fmt.Print(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
present=[github.com/sqlc-dev/sqlc/cmd/sqlc]
missing=[go.uber.org/mock/mockgen]
unbacked=[]
--- after go get -tool mockgen ---
module example.com/toolgate

go 1.24

require github.com/sqlc-dev/sqlc v1.27.0

tool (
	github.com/sqlc-dev/sqlc/cmd/sqlc
	go.uber.org/mock/mockgen
)
```

### Tests

Create `toolgate_test.go`:

```go
package toolgate

import (
	"fmt"
	"strings"
	"testing"
)

const goMod = `module example.com/orders

go 1.24

require (
	github.com/sqlc-dev/sqlc v1.27.0
	go.uber.org/mock v0.5.0
)

tool (
	github.com/sqlc-dev/sqlc/cmd/sqlc
	go.uber.org/mock/mockgen
)
`

func TestAuditToolsAllPresent(t *testing.T) {
	t.Parallel()
	want := []string{"github.com/sqlc-dev/sqlc/cmd/sqlc", "go.uber.org/mock/mockgen"}
	audit, err := AuditTools([]byte(goMod), want)
	if err != nil {
		t.Fatalf("AuditTools: %v", err)
	}
	if len(audit.Missing) != 0 {
		t.Errorf("Missing = %v, want none", audit.Missing)
	}
	if len(audit.Unbacked) != 0 {
		t.Errorf("Unbacked = %v, want none", audit.Unbacked)
	}
	if len(audit.Present) != 2 {
		t.Errorf("Present = %v, want 2", audit.Present)
	}
}

func TestAuditToolsMissing(t *testing.T) {
	t.Parallel()
	want := []string{
		"github.com/sqlc-dev/sqlc/cmd/sqlc",
		"go.uber.org/mock/mockgen",
		"github.com/golangci/golangci-lint/cmd/golangci-lint",
	}
	audit, err := AuditTools([]byte(goMod), want)
	if err != nil {
		t.Fatalf("AuditTools: %v", err)
	}
	if len(audit.Missing) != 1 || !strings.Contains(audit.Missing[0], "golangci-lint") {
		t.Fatalf("Missing = %v, want golangci-lint flagged", audit.Missing)
	}
}

func TestAuditToolsUnbacked(t *testing.T) {
	t.Parallel()
	// A tool directive with no matching require is unbacked (no pinned version).
	const gm = `module example.com/x

go 1.24

require github.com/sqlc-dev/sqlc v1.27.0

tool (
	github.com/sqlc-dev/sqlc/cmd/sqlc
	go.uber.org/mock/mockgen
)
`
	audit, err := AuditTools([]byte(gm), nil)
	if err != nil {
		t.Fatalf("AuditTools: %v", err)
	}
	if len(audit.Unbacked) != 1 || !strings.Contains(audit.Unbacked[0], "go.uber.org/mock/mockgen") {
		t.Fatalf("Unbacked = %v, want mockgen flagged", audit.Unbacked)
	}
}

func TestAddToolEmitsDirective(t *testing.T) {
	t.Parallel()
	const gm = `module example.com/x

go 1.24

require go.uber.org/mock v0.5.0
`
	out, err := AddTool([]byte(gm), "go.uber.org/mock/mockgen")
	if err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	if !strings.Contains(string(out), "tool go.uber.org/mock/mockgen") {
		t.Fatalf("Format output missing tool directive:\n%s", out)
	}
}

func ExampleAddTool() {
	const gm = "module example.com/x\n\ngo 1.24\n\nrequire go.uber.org/mock v0.5.0\n"
	out, _ := AddTool([]byte(gm), "go.uber.org/mock/mockgen")
	// The formatter appends a single tool directive backed by the require.
	fmt.Println(strings.Contains(string(out), "tool go.uber.org/mock/mockgen"))
	// Output: true
}
```

## Review

The checker is correct when a `go.mod` whose `tool` block covers every wanted tool
and whose every tool is backed by a `require` passes, and when a missing tool or an
unbacked tool is named. The unbacked case is the substantive one: a `tool` directive
with no `require` has no pinned version, which reintroduces exactly the drift the
directive was meant to eliminate. `AddTool` demonstrates the write side —
`File.AddTool` then `File.Format` is what `go get -tool` does to `go.mod` — and the
test asserts the emitted directive rather than trusting the call. Run `go test -race`.

## Resources

- [The `tool` directive](https://go.dev/ref/mod#go-mod-file-tool) — recording build tools in go.mod.
- [Managing tool dependencies](https://go.dev/doc/modules/managing-dependencies#tools) — `go get -tool`, `go tool`, and migrating off tools.go.
- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `File.Tool`, `Tool.Path`, `File.AddTool`, `File.Format`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-private-module-router.md](08-private-module-router.md) | Next: [10-govulncheck-reachability-gate.md](10-govulncheck-reachability-gate.md)
