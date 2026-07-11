# Exercise 10: Reproducible Workspace Builds With go work vendor

Platform teams make monorepo pipelines hermetic by committing a workspace-level
`vendor/` tree so CI builds offline, with no module cache and no proxy. This
exercise walks that exact path: from a `go.work` over two modules, run `go work
sync` to reconcile versions via MVS, `go work vendor` (Go 1.22+) to write a single
`vendor/` next to `go.work`, and then build with `-mod=vendor` and `GOPROXY=off`
to prove the network is not needed.

This module is fully self-contained: its own `go mod init`, and a test that builds
the whole workspace-vendor pipeline in a temp directory and asserts the offline
build succeeds.

## What you'll build

```text
vendorci/                      module github.com/example/vendorci
  go.mod                       go 1.24
  vendorci.go                  package marker
  vendorci_test.go             go.work -> sync -> vendor -> offline -mod=vendor build
  cmd/demo/main.go             runnable: runs the pipeline, prints the result
```

- Files: `vendorci.go`, `vendorci_test.go`, `cmd/demo/main.go`.
- Implement: a test that builds a two-module workspace, runs `go work sync` and `go work vendor`, and rebuilds offline.
- Test: `TestWorkVendor` asserts `vendor/` and `vendor/modules.txt` exist next to `go.work`, then runs `go build -mod=vendor ./...` with `GOPROXY=off` and requires success.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/vendorci/cmd/demo
cd ~/go-exercises/vendorci
go mod init github.com/example/vendorci
go mod edit -go=1.24
```

### The hermetic-build pipeline

A reproducible CI build must not depend on a warm module cache or a reachable
proxy — a network hiccup should never turn a pipeline red, and a build must be
byte-reproducible from the committed source. The workspace-vendor pipeline
delivers that in three steps:

- `go work sync` reconciles the workspace's combined build list back into each
  member module's `go.mod` using Minimal Version Selection (MVS), so the modules
  agree on one version of every shared dependency.
- `go work vendor` (Go 1.22+) writes a *single* `vendor/` directory next to
  `go.work`, containing every package needed to build and test the workspace,
  plus a `vendor/modules.txt` manifest recording which module version each
  vendored package came from. This is workspace-scoped: unlike `go mod vendor`,
  which writes one `vendor/` per module, `go work vendor` produces one tree for
  the whole workspace and errors if run outside workspace mode.
- `go build -mod=vendor ./...` then builds from that tree instead of the module
  cache. Setting `GOPROXY=off` proves the point: with the proxy disabled, the
  build can only succeed if every dependency is already present in `vendor/`.

The test builds the full pipeline in `t.TempDir()`: a `lib` module and an `app`
module that imports it (with a `replace example.com/lib => ../lib` so `go work
sync` resolves the sibling locally instead of reaching for an unpublished version
on the proxy), a `go.work` over both, then `sync`, `vendor`, and an offline
`-mod=vendor` build. Two environment details matter and are handled
explicitly. First, any inherited `GOFLAGS=-mod=mod` is cleared for the workspace
commands — in workspace mode `-mod` may only be `readonly` or `vendor`, so a
stray `-mod=mod` is rejected. Second, the final build sets `GOPROXY=off` (and an
empty `GOFLAGS`) alongside the explicit `-mod=vendor` flag, which overrides any
inherited value. Because this workspace's only dependency is a sibling module
already inside it, the vendored tree is minimal; in a real repository `vendor/`
would hold the full external transitive closure, but the mechanism — and the
offline guarantee — is identical.

Create `vendorci.go`:

```go
package vendorci

// Marker documents that this module's real content is its vendor-pipeline test.
const Marker = "go work vendor demo"
```

### The runnable demo

The demo runs the same pipeline in a temporary directory and reports whether the
offline `-mod=vendor` build succeeded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func mustWrite(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
}

func goCmd(dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GOFLAGS="), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func main() {
	root, err := os.MkdirTemp("", "vendorci")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	mustWrite(filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.24\n")
	mustWrite(filepath.Join(root, "lib", "lib.go"), "package lib\n\nfunc Hi() string { return \"hi\" }\n")
	mustWrite(filepath.Join(root, "app", "go.mod"), "module example.com/app\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n")
	mustWrite(filepath.Join(root, "app", "main.go"), "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Hi() }\n")

	for _, step := range [][]string{
		{"work", "init", "./lib", "./app"},
		{"work", "sync"},
		{"work", "vendor"},
	} {
		if out, err := goCmd(root, nil, step...); err != nil {
			fmt.Printf("step %v failed: %v\n%s", step, err, out)
			os.Exit(1)
		}
	}

	if out, err := goCmd(filepath.Join(root, "app"), []string{"GOPROXY=off"}, "build", "-mod=vendor", "./..."); err != nil {
		fmt.Printf("offline build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Println("offline -mod=vendor build with GOPROXY=off: ok")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
offline -mod=vendor build with GOPROXY=off: ok
```

### Tests

The test asserts the vendor tree exists after `go work vendor` and that the
offline build succeeds with the proxy disabled.

Create `vendorci_test.go`:

```go
package vendorci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runGo clears any inherited GOFLAGS (invalid in workspace mode) and applies
// extraEnv, which can add GOPROXY=off for the offline build.
func runGo(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GOFLAGS="), extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestWorkVendor(t *testing.T) {
	root := t.TempDir()

	write(t, filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.24\n")
	write(t, filepath.Join(root, "lib", "lib.go"), "package lib\n\nfunc Hi() string { return \"hi\" }\n")
	write(t, filepath.Join(root, "app", "go.mod"), "module example.com/app\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n")
	write(t, filepath.Join(root, "app", "main.go"), "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Hi() }\n")

	runGo(t, root, nil, "work", "init", "./lib", "./app")
	runGo(t, root, nil, "work", "sync")
	runGo(t, root, nil, "work", "vendor")

	// The workspace-level vendor tree and its manifest must exist next to go.work.
	for _, want := range []string{
		filepath.Join(root, "vendor"),
		filepath.Join(root, "vendor", "modules.txt"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("expected %s after go work vendor: %v", want, err)
		}
	}

	// With the proxy disabled, the build proves it needs no cache or network.
	runGo(t, filepath.Join(root, "app"), []string{"GOPROXY=off"}, "build", "-mod=vendor", "./...")
}
```

## Review

The pipeline is correct when the final build is truly offline: after `go work
vendor`, the workspace-level `vendor/` and its `modules.txt` exist beside
`go.work`, and `go build -mod=vendor ./...` succeeds with `GOPROXY=off`. If that
build ever needs the network, the vendor tree is incomplete and CI would flake —
which is exactly what the assertion catches. Three traps to avoid, all covered by
the concepts file: `go work vendor` is workspace-scoped (run it from the workspace
root; it errors outside workspace mode and writes one tree, not one per module);
`-mod=mod` inherited via `GOFLAGS` is invalid in workspace mode, so clear it; and
commit the workspace `vendor/` and `go.work` only for a true monorepo every
checkout builds together. Run `go test -race ./...` to confirm.

## Resources

- [Go Modules Reference — workspaces](https://go.dev/ref/mod#workspaces) — `go work sync` and `go work vendor` semantics.
- [`go work vendor` (command documentation)](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — the workspace vendor subcommand.
- [Vendoring (`go mod vendor`, `-mod=vendor`)](https://go.dev/ref/mod#vendoring) — how `-mod=vendor` and `vendor/modules.txt` drive an offline build.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../04-go-tool-commands/00-concepts.md](../04-go-tool-commands/00-concepts.md)
