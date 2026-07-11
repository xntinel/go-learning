# Exercise 5: Split into three modules tied together by go.work

When teams need independent release cadence, the single module splits into
several — here `platform`, `api`, and `worker` — each with its own `go.mod` and
version, tied together for local development by a `go.work` workspace at the repo
root. The workspace resolves the local `platform` module with **no `replace`
directive**. This exercise builds the real `platform` module and a pair of
helpers that verify the two invariants that make the split safe: the workspace
`use`s the local `platform`, and no member ships a filesystem `replace`.

## What you'll build

```text
mono-split/                   repo root (workspace lives here)
  go.work                     go work init + go work use (illustrated below)
  platform/                   module example.com/mono/platform (built here)
    go.mod
    platform.go               RequestID + ParseWorkspaceUses + ModuleHasReplace
    platform_test.go          workspace-invariant tests
    cmd/demo/main.go          parses a go.work and prints its members
  api/                        module example.com/mono/api        (illustrated)
  worker/                     module example.com/mono/worker     (illustrated)
```

- Files: `platform.go`, `platform_test.go`, `cmd/demo/main.go` (the `platform` module).
- Implement: `RequestID()`, `ParseWorkspaceUses(goWork []byte) []string` (the `use` directories), and `ModuleHasReplace(goMod []byte) bool`.
- Test: assert the workspace `use`s `./platform`, and assert a clean `api/go.mod` has no `replace` while one with a filesystem replace is detected.
- Verify: `go env GOWORK` non-empty inside the workspace; `go test ./...` from the workspace root resolves the local `platform`.

### The split, and the workspace that ties it back together

Splitting is three `go.mod` files, one initialized in each subdirectory so
that:

- `platform/go.mod` declares module path `example.com/mono/platform`.
- `api/go.mod` declares `example.com/mono/api` and requires `example.com/mono/platform`.
- `worker/go.mod` declares `example.com/mono/worker` and requires `example.com/mono/platform`.

On their own, `api` and `worker` would fetch a *tagged* `platform` from the proxy.
That is correct for release but useless for local development, where you are
editing `platform` and want the services to see your edits immediately. A
workspace solves exactly that. From the repo root:

```bash
cd ~/go-exercises/mono-split
go work init ./platform ./api ./worker
```

That writes a `go.work` at the root:

```text
go 1.26

use (
	./platform
	./api
	./worker
)
```

Now every go command run inside the tree is in workspace mode, and `api`'s import
of `example.com/mono/platform` resolves to the local `./platform` directory — with
**no `replace` in `api/go.mod`**. Confirm the mode with `go env GOWORK`: it prints
the path to `go.work` when a workspace is active and is empty otherwise. Add a
member later with `go work use ./newsvc`. The go command also maintains a
`go.work.sum` next to `go.work` for checksums the members' `go.sum` files do not
already cover.

The two invariants that keep this safe are testable, and that is what this
module's helpers assert:

1. The workspace must `use` the local `platform`, or the services silently fall
   back to a proxy-fetched version. `ParseWorkspaceUses` extracts the `use` set.
2. No member may carry a filesystem `replace` (the release leak from the concepts
   file). `ModuleHasReplace` flags one.

Set up the `platform` module (the one this exercise builds and gates):

```bash
mkdir -p ~/go-exercises/mono-split/platform/cmd/demo
cd ~/go-exercises/mono-split/platform
go mod init example.com/mono/platform
```

Create `platform.go`:

```go
package platform

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// RequestID returns a random 16-hex-character request identifier shared by every
// service that imports the platform module.
func RequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("platform: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ParseWorkspaceUses returns the directories a go.work file makes members via
// its use directives, handling both the single-line form (use ./dir) and the
// block form (use ( ... )).
func ParseWorkspaceUses(goWork []byte) []string {
	var uses []string
	inBlock := false

	sc := bufio.NewScanner(bytes.NewReader(goWork))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case inBlock:
			if line == ")" {
				inBlock = false
				continue
			}
			if line != "" {
				uses = append(uses, line)
			}
		case line == "use (":
			inBlock = true
		case strings.HasPrefix(line, "use "):
			uses = append(uses, strings.TrimSpace(strings.TrimPrefix(line, "use ")))
		}
	}
	return uses
}

// ModuleHasReplace reports whether a go.mod contains any replace directive,
// including inside a replace ( ... ) block. A filesystem replace is a release
// leak; the workspace exists so members need none.
func ModuleHasReplace(goMod []byte) bool {
	inBlock := false

	sc := bufio.NewScanner(bytes.NewReader(goMod))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case inBlock:
			if line == ")" {
				inBlock = false
			} else if line != "" {
				return true
			}
		case line == "replace (":
			inBlock = true
		case strings.HasPrefix(line, "replace "):
			return true
		}
	}
	return false
}
```

### The runnable demo

The demo parses a representative `go.work` and prints its members, then confirms a
clean service `go.mod` carries no `replace`. It uses in-line byte slices so it
runs without touching the filesystem.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mono/platform"
)

func main() {
	goWork := []byte("go 1.26\n\nuse (\n\t./platform\n\t./api\n\t./worker\n)\n")
	fmt.Printf("workspace members: %v\n", platform.ParseWorkspaceUses(goWork))

	apiMod := []byte("module example.com/mono/api\n\ngo 1.26\n\nrequire example.com/mono/platform v1.4.0\n")
	fmt.Printf("api go.mod has replace: %v\n", platform.ModuleHasReplace(apiMod))
	fmt.Printf("request-id length: %d\n", len(platform.RequestID()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workspace members: [./platform ./api ./worker]
api go.mod has replace: false
request-id length: 16
```

### Tests

The tests pin both invariants. `TestParseWorkspaceUses` covers the block form and
the single-line form and asserts `./platform` is a member. `TestModuleHasReplace`
proves a clean `api/go.mod` reports `false` while one carrying a filesystem
`replace` reports `true` — the exact check a reviewer applies when confirming the
workspace, not a `replace`, is what ties the modules together.

Create `platform_test.go`:

```go
package platform

import (
	"slices"
	"testing"
)

func TestParseWorkspaceUses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		work string
		want []string
	}{
		{
			name: "block form",
			work: "go 1.26\n\nuse (\n\t./platform\n\t./api\n\t./worker\n)\n",
			want: []string{"./platform", "./api", "./worker"},
		},
		{
			name: "single-line form",
			work: "go 1.26\n\nuse ./platform\n",
			want: []string{"./platform"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseWorkspaceUses([]byte(tc.work))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseWorkspaceUses = %v, want %v", got, tc.want)
			}
			if !slices.Contains(got, "./platform") {
				t.Error("workspace does not use the local ./platform module")
			}
		})
	}
}

func TestModuleHasReplace(t *testing.T) {
	t.Parallel()

	clean := "module example.com/mono/api\n\ngo 1.26\n\nrequire example.com/mono/platform v1.4.0\n"
	if ModuleHasReplace([]byte(clean)) {
		t.Error("clean go.mod reported a replace directive")
	}

	leaked := "module example.com/mono/api\n\ngo 1.26\n\nrequire example.com/mono/platform v1.4.0\n\nreplace example.com/mono/platform => ../platform\n"
	if !ModuleHasReplace([]byte(leaked)) {
		t.Error("filesystem replace was not detected")
	}

	block := "module example.com/mono/api\n\ngo 1.26\n\nreplace (\n\texample.com/mono/platform => ../platform\n)\n"
	if !ModuleHasReplace([]byte(block)) {
		t.Error("replace block was not detected")
	}
}
```

## Review

The split is correct when the workspace, not a `replace`, resolves the local
`platform`: `ParseWorkspaceUses` shows `./platform` among the members, and
`ModuleHasReplace` returns `false` for every member's `go.mod`. Inside the
workspace, `go env GOWORK` is non-empty and `go test ./...` from the root builds
`api` and `worker` against your live `platform` edits; outside it, each module
would fetch a tagged `platform` from the proxy.

The trap the second helper guards against is the whole reason a workspace exists:
developers reach for `replace example.com/mono/platform => ../platform` to develop
locally, it works on their machine, and the tagged release build — which has no
sibling `../platform` — breaks. Use `go.work` for development and tagged versions
for release, and let `ModuleHasReplace` fail CI if a filesystem replace ever lands
in a member. Do not commit `go.work` into a release artifact; it belongs to
development.

## Resources

- [Tutorial: Getting started with multi-module workspaces](https://go.dev/doc/tutorial/workspaces) — `go work init`/`go work use` end to end.
- [Go Modules Reference: Workspaces](https://go.dev/ref/mod#workspaces) — `go.work` semantics and `go.work.sum`.
- [`cmd/go`: Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — the `go work` subcommands.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-internal-boundary-across-services.md](06-internal-boundary-across-services.md)
