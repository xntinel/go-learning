# Exercise 7: Build Constraints and Platform-Specific Files

Which `.go` files compile is not fixed by the directory — it depends on the
target OS/arch and on build tags, decided by filename suffixes and `//go:build`
lines. This exercise builds a platform layer where the same capability is
provided by `sysinfo_linux.go` and `sysinfo_darwin.go`, plus a tag-gated
`integration.go`, and then *asserts* the selection: using `go/build` with a
customized `Context` to see which file a given `GOOS` picks, and `go list -tags`
to prove the tagged file appears only when the tag is set.

This module is fully self-contained: its own `go mod init`, platform files, and a
test that inspects file selection two ways.

## What you'll build

```text
platform/                      module github.com/example/platform
  go.mod                       go 1.24
  internal/sysinfo/
    sysinfo.go                 OSName() -> osName()
    sysinfo_linux.go           osName() = "linux"   (GOOS suffix)
    sysinfo_darwin.go          osName() = "darwin"  (GOOS suffix)
    integration.go             //go:build integration  (custom tag)
    sysinfo_test.go            go/build custom Context + go list -tags checks
  cmd/demo/main.go             runnable: prints OSName() for the host
```

- Files: `internal/sysinfo/sysinfo.go`, `sysinfo_linux.go`, `sysinfo_darwin.go`, `integration.go`, `internal/sysinfo/sysinfo_test.go`, `cmd/demo/main.go`.
- Implement: `osName()` provided per platform by GOOS-suffixed files; `OSName()` exposing it; an `integrationTag()` behind `//go:build integration`.
- Test: `TestConstraintSelection` uses a `go/build.Context` with `GOOS` set to assert `Package.GoFiles` contains only the matching platform file; `TestTaggedFileSelection` runs `go list -tags integration` and asserts the tagged file appears only with the tag.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/07-build-constraints-and-platform-files/internal/sysinfo go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/07-build-constraints-and-platform-files/cmd/demo
cd go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/07-build-constraints-and-platform-files
go mod edit -go=1.24
```

### File naming is part of the layout

Go decides a file's inclusion in a build from two structural rules, both applied
before any code is read:

- A filename *suffix* `_GOOS`, `_GOARCH`, or `_GOOS_GOARCH` is an implicit
  constraint. `sysinfo_linux.go` compiles only when `GOOS=linux`;
  `sysinfo_darwin.go` only when `GOOS=darwin`. The two provide the *same* symbol
  (`osName`) with platform-specific bodies, and exactly one is selected per
  target, so `sysinfo.go` can call `osName()` on every platform.
- A `//go:build <expr>` line at the very top of a file (followed by a blank line,
  before `package`) gates the whole file on a boolean tag expression.
  `integration.go` carries `//go:build integration`, so it compiles only when you
  pass `-tags integration`. In a default build it is excluded entirely; its
  symbols do not exist, which is fine because nothing in the default build refers
  to them.

Because these are structural, you cannot verify them by reading the code — you
have to ask the toolchain what it selected for a given target. The test does that
two ways. First, `go/build`'s `Context` is the same package selection logic the
compiler uses, exposed as a library: set `ctxt.GOOS = "linux"` on a copy of
`build.Default`, call `ctxt.ImportDir(dir, 0)`, and the returned
`Package.GoFiles` lists exactly the files a Linux build would compile — asserting
`sysinfo_linux.go` is present and `sysinfo_darwin.go` is not, on any host. Second,
`go list -tags integration -f '{{.GoFiles}}'` shows the tag-gated file joining the
set only when the tag is supplied.

Create `internal/sysinfo/sysinfo.go`:

```go
package sysinfo

// OSName reports the operating system this binary was built for. The concrete
// osName is supplied by a GOOS-suffixed file selected at build time.
func OSName() string {
	return osName()
}
```

Create `internal/sysinfo/sysinfo_linux.go` (the `_linux` suffix gates it on `GOOS=linux`):

```go
package sysinfo

func osName() string { return "linux" }
```

Create `internal/sysinfo/sysinfo_darwin.go` (the `_darwin` suffix gates it on `GOOS=darwin`):

```go
package sysinfo

func osName() string { return "darwin" }
```

Create `internal/sysinfo/integration.go`. The `//go:build integration` line
(before `package`, with a blank line after) excludes it unless `-tags integration`
is set:

```go
//go:build integration

package sysinfo

// IntegrationTag reports the build tag that selected this file. It exists only
// when the module is built with -tags integration.
func IntegrationTag() string { return "integration" }
```

### The runnable demo

The demo prints the platform name selected for the host it is built on.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/platform/internal/sysinfo"
)

func main() {
	fmt.Printf("built for: %s\n", sysinfo.OSName())
}
```

Run it (output reflects the host GOOS; on a macOS build):

```bash
go run ./cmd/demo
```

Expected output:

```
built for: darwin
```

### Tests

The first test drives `go/build` with a Linux context and checks the selected
files without cross-compiling anything. The second shells out to `go list` to
prove the tag-gated file is included only under `-tags integration`.

Create `internal/sysinfo/sysinfo_test.go`:

```go
package sysinfo

import (
	"go/build"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestConstraintSelection(t *testing.T) {
	t.Parallel()

	// A copy of the default build context, retargeted at linux/amd64. Cgo off
	// keeps selection deterministic across hosts.
	ctxt := build.Default
	ctxt.GOOS = "linux"
	ctxt.GOARCH = "amd64"
	ctxt.CgoEnabled = false

	pkg, err := ctxt.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}

	if !slices.Contains(pkg.GoFiles, "sysinfo_linux.go") {
		t.Errorf("linux build should include sysinfo_linux.go, got %v", pkg.GoFiles)
	}
	if slices.Contains(pkg.GoFiles, "sysinfo_darwin.go") {
		t.Errorf("linux build should exclude sysinfo_darwin.go, got %v", pkg.GoFiles)
	}
	// integration.go is behind a tag not set on this context.
	if slices.Contains(pkg.GoFiles, "integration.go") {
		t.Errorf("integration.go should be excluded without -tags, got %v", pkg.GoFiles)
	}
}

func goFiles(t *testing.T, tags string) string {
	t.Helper()
	args := []string{"list", "-f", "{{.GoFiles}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".")
	out, err := exec.CommandContext(t.Context(), "go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list (tags=%q): %v\n%s", tags, err, out)
	}
	return string(out)
}

func TestTaggedFileSelection(t *testing.T) {
	if got := goFiles(t, ""); strings.Contains(got, "integration.go") {
		t.Errorf("default build should not list integration.go, got %s", got)
	}
	if got := goFiles(t, "integration"); !strings.Contains(got, "integration.go") {
		t.Errorf("-tags integration should list integration.go, got %s", got)
	}
}
```

## Review

The platform layer is correct when file selection matches the target: a Linux
context selects `sysinfo_linux.go` and rejects `sysinfo_darwin.go`, and the
`//go:build integration` file joins the build only under `-tags integration`. The
test proves both without cross-compiling, by treating selection as data —
`go/build.Context.ImportDir` for the GOOS suffix and `go list -tags` for the
build tag. The trap the concepts file warns about is real here: a suffix like
`_linux` or `_amd64`, and the `_test` suffix, are structural filename rules, not
comments, so naming a file `main_linux.go` silently excludes it everywhere but
Linux. Verify selection with the toolchain rather than assuming. Run `go test
-race ./...` to confirm the default build and both checks pass.

## Resources

- [Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — `//go:build` syntax and `GOOS`/`GOARCH` filename suffixes.
- [`go/build`](https://pkg.go.dev/go/build) — `Context`, `ImportDir`, and `Package.GoFiles` for asserting file selection.
- [`go/build/constraint`](https://pkg.go.dev/go/build/constraint) — parsing and evaluating `//go:build` expressions.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-hexagonal-internal-layering.md](08-hexagonal-internal-layering.md)
