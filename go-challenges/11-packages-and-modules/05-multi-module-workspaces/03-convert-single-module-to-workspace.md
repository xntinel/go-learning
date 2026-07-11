# Exercise 3: Split One Module Into Two And Tie Them With go.work

When a shared package inside one module needs its own release cadence, you split
it out and reconnect the pieces with a workspace. This exercise performs that
conversion with the real `go work` tooling — `go work init`, `go work use`,
`go env GOWORK` — rather than hand-writing the file, and builds a small validator
that confirms the generated `go.work` carries a valid `go` directive.

## What you'll build

```text
platform/                      gated module: example.com/platform
  go.mod                       go 1.26
  text/
    text.go                    package text; Greet (the library after the split)
  workspace/
    workspace.go               package workspace; GoDirective parses go.work's go line
    workspace_test.go          asserts the go directive is present and valid
  cmd/
    demo/
      main.go                  prints the go version parsed from a sample go.work
```

- Files: `text/text.go`, `workspace/workspace.go`, `workspace/workspace_test.go`, `cmd/demo/main.go`.
- Implement: `GoDirective(gowork string) (string, error)` that returns the version from the `go` line and errors (`ErrNoGoDirective`) when it is missing.
- Test: a valid `go.work` yields its version; a `go.work` with no `go` line returns `ErrNoGoDirective` via `errors.Is`.
- Verify: the library and the validator both pass `go test`; a generated `go.work` reports through `go env GOWORK`.

Set up the gated module:

```bash
mkdir -p ~/platform/text ~/platform/workspace ~/platform/cmd/demo
cd ~/platform
go mod init example.com/platform
go mod edit -go=1.26
```

### The conversion, with real tooling

Start from a single module that contains both the library and a service. To give
the library its own release cadence, split it into two modules and tie them with
a workspace. Do it with the tools, not a text editor:

```bash
# 1. carve the library into its own module
mkdir -p ~/mono/text ~/mono/service
cd ~/mono/text && go mod init example.com/platform/text     # move text.go here
cd ~/mono/service && go mod init example.com/platform/service  # move service.go here

# 2. generate go.work listing both modules
cd ~/mono
go work init ./text ./service

# 3. confirm the workspace is active
go env GOWORK        # -> /Users/you/mono/go.work
```

`go work init ./text ./service` writes a `go.work` with a `go` line and a `use`
entry per directory:

```text
go 1.26

use (
	./text
	./service
)
```

`go work use [-r] [dirs]` maintains that list without rewriting the file by hand:
`go work use ./worker` appends a module, `go work use -r ./services` recurses to
find every nested `go.mod` under `./services` and adds them all, and passing a
directory that no longer contains a module removes its `use` entry.
`go env GOWORK` reports the active file (empty when there is none), which is how a
script confirms it is operating in workspace mode. The `go` line in `go.work` is
required — a `go.work` without it is malformed — which is exactly the invariant
the validator below enforces.

Create `text/text.go` — the library after the split:

```go
// text/text.go
package text

// Greet builds a greeting for name; an empty name yields "Hello, ".
func Greet(name string) string {
	return "Hello, " + name
}
```

### A validator for the generated go.work

Monorepo tooling that regenerates `go.work` must reject a file missing its `go`
directive, because that file will not build. `GoDirective` scans the `go.work`
text for a line of the form `go X.Y` (optionally `X.Y.Z`) and returns the
version, or a wrapped `ErrNoGoDirective` sentinel when absent. It parses the
directive by structure, not by trusting the tool blindly, so a malformed
generation is caught in CI.

Create `workspace/workspace.go`:

```go
// workspace/workspace.go
package workspace

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrNoGoDirective is returned when a go.work file has no valid go line.
var ErrNoGoDirective = errors.New("go.work: no go directive")

// goLine matches a go.work "go" directive: "go 1.26" or "go 1.26.1".
var goLine = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*$`)

// GoDirective returns the version declared by the go.work file's go directive.
// It wraps ErrNoGoDirective when the directive is missing or malformed.
func GoDirective(gowork string) (string, error) {
	m := goLine.FindStringSubmatch(gowork)
	if m == nil {
		return "", fmt.Errorf("parsing go.work: %w", ErrNoGoDirective)
	}
	return m[1], nil
}
```

### The demo

The demo parses a representative `go.work` and prints the version it found.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/platform/workspace"
)

func main() {
	const gowork = "go 1.26\n\nuse (\n\t./text\n\t./service\n)\n"
	v, err := workspace.GoDirective(gowork)
	if err != nil {
		fmt.Println("invalid go.work:", err)
		return
	}
	fmt.Println("go.work go directive:", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
go.work go directive: 1.26
```

### Tests

The valid case pins that `go work init` produces a parseable `go` line; the
missing case pins that a malformed regeneration is rejected via the sentinel.

Create `workspace/workspace_test.go`:

```go
// workspace/workspace_test.go
package workspace

import (
	"errors"
	"testing"
)

func TestGoDirective(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		gowork  string
		want    string
		wantErr error
	}{
		{
			name:   "init output",
			gowork: "go 1.26\n\nuse (\n\t./text\n\t./service\n)\n",
			want:   "1.26",
		},
		{
			name:   "patch version",
			gowork: "go 1.26.1\n\nuse ./text\n",
			want:   "1.26.1",
		},
		{
			name:    "missing directive",
			gowork:  "use (\n\t./text\n)\n",
			wantErr: ErrNoGoDirective,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := GoDirective(tc.gowork)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("GoDirective error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("GoDirective = %q, want %q", got, tc.want)
			}
		})
	}
}
```

## Review

The conversion is done correctly when `go work init ./text ./service` produces a
`go.work` whose `go env GOWORK` resolves to the new file and whose `go` directive
`GoDirective` accepts, and when `go test` still passes inside each split module.
The reason to use `go work use`/`go work init` rather than a text editor is
determinism: the tooling always writes a well-formed file, whereas a
string-append script drifts into malformed `use` blocks. The validator encodes
the one invariant the tools guarantee and a bad generator would violate — a
present, valid `go` line — and asserts the failure path through a wrapped sentinel
so callers can branch on `errors.Is(err, ErrNoGoDirective)` rather than string
matching.

## Resources

- [Tutorial: Getting started with multi-module workspaces](https://go.dev/doc/tutorial/workspaces) — `go work init` and `go work use` in a real split.
- [go command — Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — `go work init`, `go work use`, and the `-r` flag.
- [Go Modules Reference — go.work files](https://go.dev/ref/mod#workspaces) — the required `go` directive and the `use` syntax.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-gowork-init-and-use-resolution.md](04-gowork-init-and-use-resolution.md)
