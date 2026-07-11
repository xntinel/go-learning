# Exercise 6: Develop Two Modules in Lockstep With go.work

When you need to edit a library and a tool that consumes it at the same time,
without publishing an intermediate version after every change, `go.work` is the
tool: a per-checkout workspace that resolves imports to local working copies with
no `replace` directive in any `go.mod`. This exercise wires a workspace over two
modules, builds it, and asserts — from a test — that workspace mode is active and
resolves both modules locally.

This module is fully self-contained: its own `go mod init`, and a test that
constructs a real two-module workspace on disk and verifies its activation.

## What you'll build

```text
work/                          module github.com/example/work
  go.mod                       go 1.24
  work.go                      package marker
  work_test.go                 builds a go.work over two temp modules, asserts it
  cmd/
    demo/main.go               runnable: creates a workspace, prints GOWORK + build result
```

- Files: `work.go`, `work_test.go`, `cmd/demo/main.go`.
- Implement: a test that writes two modules (`app`, `lib`) in a temp dir, runs `go work init`, and inspects activation.
- Test: `TestWorkspace` asserts `go env GOWORK` points at the workspace file, `go build ./...` succeeds from the workspace, and `go work edit -json` parses to a struct listing both `Use` paths.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/work/cmd/demo
cd ~/go-exercises/work
go mod init github.com/example/work
go mod edit -go=1.24
```

### What a workspace actually does

A `go.work` file lists several modules with `use` directives. Inside a directory
covered by that file, imports of those modules resolve to their local working
copies as if each were the published version — so an edit to the library is
picked up by the tool on the next build, with no tag, no publish, and no `replace`
line polluting either `go.mod`. `go work init ./lib ./app` creates the file with
both `use` directives; `go work use ./more` adds another later.

Three commands let you verify a workspace is doing its job, and this exercise
asserts all three:

- `go env GOWORK` prints the path of the active `go.work` file, or empty if
  workspace mode is off. A non-empty value that points at your file is proof the
  workspace is engaged.
- `go build ./...` run from the workspace root builds every module together,
  resolving cross-module imports to the local copies. In the test, `app` imports
  `lib` with *no* `require`-satisfying network fetch — the workspace supplies it.
- `go work edit -json` prints the parsed workspace as JSON. Its `Use` array lists
  each module's `DiskPath`; asserting there are two entries confirms both modules
  are in the workspace.

The test builds the whole scenario in `t.TempDir()`: a `lib` module exposing
`Hello()`, an `app` module whose `main.go` imports `lib`, and then `go work init
./lib ./app`. Because everything is local and the workspace resolves the import,
the build needs no proxy. Crucially, the `app`'s `go.mod` does *not* need a
`replace` directive — that is the entire distinction from the previous exercise's
technique. The workspace is the override, and it lives outside both modules.

Create `work.go`:

```go
package work

// Marker documents that this module's real content is its workspace test.
const Marker = "go.work demo"
```

### The runnable demo

The demo performs the same workspace setup in a temporary directory and prints
whether workspace mode activated and whether the cross-module build succeeded, so
you can watch local resolution happen.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func mustWrite(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
}

func goCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	// Clear any inherited GOFLAGS; -mod=mod is invalid in workspace mode.
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func main() {
	root, err := os.MkdirTemp("", "workdemo")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	mustWrite(filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.24\n")
	mustWrite(filepath.Join(root, "lib", "lib.go"), "package lib\n\nfunc Hello() string { return \"hi\" }\n")
	mustWrite(filepath.Join(root, "app", "go.mod"), "module example.com/app\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n")
	mustWrite(filepath.Join(root, "app", "main.go"), "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Hello() }\n")

	if out, err := goCmd(root, "work", "init", "./lib", "./app"); err != nil {
		fmt.Printf("work init failed: %v\n%s", err, out)
		os.Exit(1)
	}

	gowork, _ := goCmd(root, "env", "GOWORK")
	fmt.Printf("workspace active: %v\n", strings.TrimSpace(gowork) != "")

	if out, err := goCmd(filepath.Join(root, "app"), "build", "./..."); err != nil {
		fmt.Printf("build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Println("cross-module build with no replace directive: ok")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workspace active: true
cross-module build with no replace directive: ok
```

### Tests

The test asserts the three activation signals — `GOWORK`, a successful workspace
build, and the two `Use` entries in `go work edit -json`.

Create `work_test.go`:

```go
package work

import (
	"encoding/json"
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

func resolve(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return r
}

func runGo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = dir
	// Clear any inherited GOFLAGS (e.g. -mod=mod): it is invalid in workspace
	// mode, where -mod may only be readonly or vendor.
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestWorkspace(t *testing.T) {
	root := t.TempDir()

	write(t, filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.24\n")
	write(t, filepath.Join(root, "lib", "lib.go"), "package lib\n\nfunc Hello() string { return \"hi\" }\n")
	write(t, filepath.Join(root, "app", "go.mod"), "module example.com/app\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n")
	write(t, filepath.Join(root, "app", "main.go"), "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Hello() }\n")

	runGo(t, root, "work", "init", "./lib", "./app")

	// 1. Workspace mode is active and points at our go.work. Resolve symlinks
	// on both sides: on macOS a temp dir under /var is reported as /private/var.
	gowork := strings.TrimSpace(runGo(t, root, "env", "GOWORK"))
	if gowork == "" {
		t.Fatal("GOWORK is empty: workspace mode is not active")
	}
	gotResolved := resolve(t, gowork)
	wantResolved := resolve(t, filepath.Join(root, "go.work"))
	if gotResolved != wantResolved {
		t.Fatalf("GOWORK = %q, want %q", gotResolved, wantResolved)
	}

	// 2. The cross-module build resolves locally, with no replace directive.
	// Built from inside the app module so ./... has a module to expand under;
	// the workspace still supplies example.com/lib from its local copy.
	runGo(t, filepath.Join(root, "app"), "build", "./...")

	// 3. Both modules appear in the workspace.
	var gw struct {
		Use []struct{ DiskPath string }
	}
	if err := json.Unmarshal([]byte(runGo(t, root, "work", "edit", "-json")), &gw); err != nil {
		t.Fatalf("parse go work edit -json: %v", err)
	}
	if len(gw.Use) != 2 {
		t.Fatalf("workspace Use entries = %d, want 2 (%+v)", len(gw.Use), gw.Use)
	}
}
```

## Review

The workspace is doing its job when three signals agree: `go env GOWORK` names
your `go.work`, `go build ./...` links `app` against the local `lib` with no
`replace` and no proxy, and `go work edit -json` reports both modules under `Use`.
The distinction to keep sharp is `go.work` versus `replace`: the previous exercise
used a committed `replace` inside a `go.mod`; here the override lives entirely in
a per-checkout `go.work` that neither module's `go.mod` mentions. That is why a
personal `go.work` is usually left untracked — committing one that lists only the
modules you happen to have locally hands everyone else a mismatched build list.
Run `go test -race ./...` to confirm.

## Resources

- [Tutorial: multi-module workspaces](https://go.dev/doc/tutorial/workspaces) — the introductory `go.work` walkthrough.
- [Go Modules Reference — workspaces](https://go.dev/ref/mod#workspaces) — `go work init`, `go work use`, `go work edit`, and `GOWORK`.
- [`go work` command](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — the workspace subcommands.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-build-constraints-and-platform-files.md](07-build-constraints-and-platform-files.md)
