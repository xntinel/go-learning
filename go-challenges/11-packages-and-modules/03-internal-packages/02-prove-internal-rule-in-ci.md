# Exercise 2: Turn The internal Rule Into An Executable CI Guard

A layering contract that only lives in a code-review comment erodes the first busy
week. Because the `internal` rule is enforced by the `go` command itself, you can
make the contract executable: write a test that shells out to `go build` against a
fixture package that violates the rule and assert the toolchain rejects it with the
exact `use of internal package ... not allowed` diagnostic. This is how a senior
engineer keeps a boundary from silently rotting.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
archguard/                    module example.com/archguard
  go.mod
  guard.go                    GoAvailable, Fixture, BuildFixture (shell out to go build)
  guard_test.go               assert illegal import is rejected; legal import builds
  cmd/demo/main.go            runnable demo printing both build outcomes
```

- Files: `guard.go`, `guard_test.go`, `cmd/demo/main.go`.
- Implement: a `BuildFixture` helper that writes an in-memory fixture module to a directory and runs `go build ./<pkg>`, returning combined output; a `Fixture` that lays out an `internal` package plus a legal and an illegal importer.
- Test: assert `go build` of the illegal importer returns a non-nil error whose output contains `use of internal package`, and that the legal importer builds clean; skip if no `go` binary is on `PATH`.
- Verify: `go test -count=1 -race ./...`

### Why shell out to the toolchain

You cannot assert the `internal` violation by compiling the fixture into your own
module — if the illegal import were part of the module under test, your own
`go build ./...` would fail before any test ran. The violation must be built in
isolation. So the test materializes a tiny throwaway module in a temp directory and
invokes `go build` on it as a subprocess, capturing the result. The subprocess is
the real `go` toolchain making the real decision; the test only asserts the outcome.
That is what makes this a trustworthy gate rather than a re-implementation of the
rule.

The fixture is deliberately minimal so the nested build is fast: the `internal`
package exports a single constant, and the two importers do nothing but a
blank-import of it. One importer (`a/reader`) sits under `a`, inside the allow-list
of `a/internal/secret`, so it builds. The other (`b`) sits at the module root,
outside that allow-list, so the toolchain rejects it. The test asserts both
outcomes — a boundary test that only checks the negative case is half a test,
because it would also pass if `internal` blocked everything.

The fixture's `go.mod` pins a low language version (`go 1.21`) so the nested build
never triggers a toolchain download: any modern `go` satisfies it. The fixture
imports only its own packages, so the build needs no network.

Create `guard.go`:

```go
// Package archguard builds throwaway fixture modules with the go toolchain to
// prove the internal-package rule is enforced at build time.
package archguard

import (
	"os"
	"os/exec"
	"path/filepath"
)

// GoAvailable reports whether a go toolchain is on PATH. Tests skip when it is
// absent so the suite stays hermetic on machines without go installed.
func GoAvailable() bool {
	_, err := exec.LookPath("go")
	return err == nil
}

// Fixture returns a minimal module (relative path -> file content) with an
// internal package plus a legal importer (a/reader) and an illegal one (b).
func Fixture() map[string]string {
	m := map[string]string{}
	m["go.mod"] = "module example.com/fix\n\ngo 1.21\n"
	m["a/internal/secret/s.go"] = "package secret\n\nconst Token = \"s3cr3t\"\n"
	m["a/reader/reader.go"] = "package reader\n\nimport _ \"example.com/fix/a/internal/secret\"\n"
	m["b/b.go"] = "package b\n\nimport _ \"example.com/fix/a/internal/secret\"\n"
	return m
}

// BuildFixture writes files under dir and runs `go build ./<pkg>` there,
// returning the command's combined output and its error (non-nil on a build
// failure, which is the point for the illegal case).
func BuildFixture(dir string, files map[string]string, pkg string) (string, error) {
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

The demo runs both builds against a temp directory and reports the two outcomes and
whether the diagnostic string is present. It is the same logic the test asserts,
printed for a human.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strings"

	"example.com/archguard"
)

func main() {
	if !archguard.GoAvailable() {
		fmt.Println("go toolchain not found on PATH")
		return
	}
	dir, err := os.MkdirTemp("", "archguard-demo-")
	if err != nil {
		fmt.Println("mkdir temp:", err)
		return
	}
	defer os.RemoveAll(dir)

	files := archguard.Fixture()

	_, legalErr := archguard.BuildFixture(dir, files, "a/reader")
	badOut, illegalErr := archguard.BuildFixture(dir, files, "b")

	fmt.Println("legal import (a/reader) builds:", legalErr == nil)
	fmt.Println("illegal import (b) rejected:", illegalErr != nil)
	fmt.Println("diagnostic present:", strings.Contains(badOut, "use of internal package"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
legal import (a/reader) builds: true
illegal import (b) rejected: true
diagnostic present: true
```

### Tests

`TestIllegalInternalImportRejected` is the CI guard: it asserts the illegal build
fails AND that the failure is the `internal` diagnostic (not some unrelated compile
error). `TestLegalInternalImportAllowed` proves the fixture is otherwise sound, so
the negative result is attributable to the boundary and nothing else. Both skip
cleanly when no `go` binary is present.

Create `guard_test.go`:

```go
package archguard

import (
	"strings"
	"testing"
)

func TestIllegalInternalImportRejected(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}

	out, err := BuildFixture(t.TempDir(), Fixture(), "b")
	if err == nil {
		t.Fatalf("go build of illegal importer succeeded; want failure\noutput:\n%s", out)
	}
	if !strings.Contains(out, "use of internal package") {
		t.Fatalf("build failed but without the internal diagnostic; got:\n%s", out)
	}
}

func TestLegalInternalImportAllowed(t *testing.T) {
	t.Parallel()
	if !GoAvailable() {
		t.Skip("no go toolchain on PATH")
	}

	out, err := BuildFixture(t.TempDir(), Fixture(), "a/reader")
	if err != nil {
		t.Fatalf("go build of legal importer failed: %v\noutput:\n%s", err, out)
	}
}
```

## Review

The guard is correct when it distinguishes the two outcomes: the illegal importer
must fail with the `use of internal package` text, and the legal importer must
build. Asserting only the failure would be a weaker test that a rule blocking
everything could also satisfy; the paired positive case pins the result to the
boundary. Matching on the stable diagnostic substring — rather than an exit code
alone — guards against a coincidental compile error masquerading as enforcement.

The mistakes to avoid: do not try to compile the illegal fixture inside your own
module (your build breaks before the test runs) — build it as an isolated
subprocess. Do not forget the `t.Skip` guard on `GoAvailable`, or the suite becomes
non-hermetic on a machine without `go`. And keep the fixture's `go.mod` at a low
language version so the nested build never reaches for a toolchain download. Extend
this pattern to any layering rule you can express as an import: a fixture that
violates it plus a `go build` assertion is a contract CI cannot merge around.

## Resources

- [cmd/go: Internal Directories](https://pkg.go.dev/cmd/go#hdr-Internal_Directories) — the toolchain's own statement of the rule it enforces.
- [`os/exec`](https://pkg.go.dev/os/exec) — `Command`, `Cmd.Dir`, and `Cmd.CombinedOutput`.
- [`testing.T.TempDir`](https://pkg.go.dev/testing#T.TempDir) — per-test temp directories cleaned up automatically.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-handler-internal-render.md](01-handler-internal-render.md) | Next: [03-module-root-internal-api-surface.md](03-module-root-internal-api-surface.md)
