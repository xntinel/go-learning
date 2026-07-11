# Exercise 5: Prove the internal/ Boundary Is Compiler-Enforced

The `internal/` rule is not documentation; it is enforced by the compiler across
module boundaries. This exercise turns that claim into a runnable negative test:
it constructs an outsider module that tries to import
`github.com/example/myapp/internal/greeting`, shells out to `go build`, and
asserts the compiler *rejects* it with `use of internal package ... not allowed`.
A passing build would be the bug.

This module is fully self-contained: its own `go mod init`, the internal package,
a demo that uses it legally from inside the module, and a test that proves an
outsider cannot.

## What you'll build

```text
myapp/                         module github.com/example/myapp
  go.mod                       go 1.24
  internal/
    greeting/greeting.go       Greet (the protected package)
    boundary/boundary_test.go  builds an outsider module, asserts rejection
  cmd/
    demo/main.go               runnable: uses internal/greeting legally
```

- Files: `internal/greeting/greeting.go`, `internal/boundary/boundary_test.go`, `cmd/demo/main.go`.
- Implement: a normal `internal/greeting` package; a test that writes a sibling `github.com/example/outsider` module (with a local `replace`) importing the internal package.
- Test: `TestInternalRuleEnforced` runs `go build ./...` in the outsider module, requires a non-nil exit error, and asserts the output contains `use of internal package` and `not allowed`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/myapp/internal/greeting ~/go-exercises/myapp/internal/boundary ~/go-exercises/myapp/cmd/demo
cd ~/go-exercises/myapp
go mod init github.com/example/myapp
go mod edit -go=1.24
```

### Negative compilation as the assertion

Most tests assert that correct code produces a correct result. This one asserts
that *incorrect* code fails to compile — and fails for the specific reason we
expect. The `internal/` rule says a package under `.../internal/...` is importable
only by code rooted at the parent of that `internal/`. Within `myapp`, the demo
imports `github.com/example/myapp/internal/greeting` freely, because `cmd/demo`
is rooted at `github.com/example/myapp`. A *different* module cannot, and the
compiler says so at build time.

To prove it hermetically, the test builds a second module on the fly in
`t.TempDir()`:

- `myapp/` — a minimal copy of the module with the internal package. This is the
  target whose internal package the outsider tries to reach.
- `outsider/` — module `github.com/example/outsider` with a `main.go` that
  imports `github.com/example/myapp/internal/greeting`, plus a `replace
  github.com/example/myapp => ../myapp` so the import resolves locally with no
  network.

Then it runs `go build ./...` inside `outsider/` and asserts two things: the
command exits non-zero, and its combined output contains both `use of internal
package` and `not allowed`. Requiring the *specific* message, not just any
failure, ensures the test fails for the right reason — a typo that broke the
build differently would not accidentally pass. This is the honest way to test an
enforcement rule: make the illegal thing and confirm the toolchain stops it.

Create `internal/greeting/greeting.go`:

```go
package greeting

import "fmt"

// Greet formats a fixed greeting. It lives under internal/, so only code rooted
// at github.com/example/myapp may import it.
func Greet(name string) string {
	return fmt.Sprintf("[myapp] %s says hello", name)
}
```

### The runnable demo

The demo imports the internal package from inside the module — the legal case —
to show the boundary only blocks *outsiders*, not same-module code.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/myapp/internal/greeting"
)

func main() {
	fmt.Println(greeting.Greet("Gopher"))
	fmt.Println("internal import from inside the module: allowed")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[myapp] Gopher says hello
internal import from inside the module: allowed
```

### Tests

The test materializes both modules in a temp directory and asserts the outsider
build is rejected with the exact enforcement message.

Create `internal/boundary/boundary_test.go`:

```go
package boundary

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

func TestInternalRuleEnforced(t *testing.T) {
	dir := t.TempDir()

	// A minimal copy of the target module, with an internal package.
	write(t, filepath.Join(dir, "myapp", "go.mod"),
		"module github.com/example/myapp\n\ngo 1.24\n")
	write(t, filepath.Join(dir, "myapp", "internal", "greeting", "greeting.go"),
		"package greeting\n\nfunc Greet(name string) string { return name }\n")

	// An outsider module that tries to import the internal package. The local
	// replace makes the import resolve without a network; the internal/ rule
	// must still reject it.
	write(t, filepath.Join(dir, "outsider", "go.mod"),
		"module github.com/example/outsider\n\ngo 1.24\n\n"+
			"require github.com/example/myapp v0.0.0\n\n"+
			"replace github.com/example/myapp => ../myapp\n")
	write(t, filepath.Join(dir, "outsider", "main.go"),
		"package main\n\n"+
			"import \"github.com/example/myapp/internal/greeting\"\n\n"+
			"func main() { _ = greeting.Greet(\"x\") }\n")

	cmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	cmd.Dir = filepath.Join(dir, "outsider")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("expected the internal import to be rejected, but build succeeded:\n%s", out)
	}
	s := string(out)
	if !strings.Contains(s, "use of internal package") || !strings.Contains(s, "not allowed") {
		t.Fatalf("build failed for the wrong reason; want internal-package error, got:\n%s", s)
	}
}
```

## Review

The test is correct when it fails for exactly one reason: the compiler refuses the
cross-module import of an `internal/` package. A non-nil exit error alone is not
enough — a broken temp module could fail for an unrelated reason — so the
assertion requires the specific `use of internal package ... not allowed`
substrings. The demo's legal same-module import is the contrast that makes the
rule concrete: `internal/` blocks outsiders, not neighbors. One caveat worth
carrying forward from the concepts file: this enforcement is *cross-module* only;
it says nothing about layering *within* a module, which is what the next exercises
address with a guard test. Run `go test -race ./...` to confirm the negative test
holds.

## Resources

- [Go 1.4 internal packages release note](https://go.dev/doc/go1.4#internalpackages) — the rule and its exact error text.
- [`go build` documentation](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies) — the command the test drives.
- [Module `replace` directive](https://go.dev/ref/mod#go-mod-file-replace) — resolving the target module locally without a proxy.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-go-work-multi-module.md](06-go-work-multi-module.md)
