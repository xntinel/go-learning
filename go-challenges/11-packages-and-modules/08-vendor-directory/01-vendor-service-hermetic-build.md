# Exercise 1: Vendor a real service and build it hermetically with -mod=vendor

The canonical vendoring workflow, done as a real backend artifact: a log-line
normalizer CLI built three ways — from the module cache, then vendored, then in a
hermetic `-mod=vendor` build — with a test that proves the vendored build
produces byte-for-byte identical output. This is the preserved core of the
lesson, upgraded from a toy uppercaser into a testable package plus a real CLI,
with the vendoring workflow driven and asserted by an exec-based contract test.

This module is fully self-contained: its own `go mod init`, a library package, a
CLI in `cmd/demo`, and tests that both pin the output contract and drive a real
`go mod vendor` / `go build -mod=vendor` cycle in a temp directory.

## What you'll build

```text
normalizer/                    independent module: example.com/normalizer
  go.mod                       go 1.26
  normalizer.go                Normalize(field) string; Run(args, stdout) int
  cmd/
    demo/
      main.go                  the CLI: os.Exit(Run(os.Args, os.Stdout))
  normalizer_test.go           output contract + hermetic vendor-build exec test
```

- Files: `normalizer.go`, `cmd/demo/main.go`, `normalizer_test.go`.
- Implement: `Normalize`, which trims and uppercases a log field, and `Run`, the CLI core that reads one argument and prints the normalized value.
- Test: the preserved "CLI uppercases argument" contract on `Run`, plus a hermetic test that vendors and `-mod=vendor`-builds a copy of the module in `t.TempDir()` and asserts its output equals the cache build's, byte for byte.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/08-vendor-directory/01-vendor-service-hermetic-build/cmd/demo
cd go-solutions/11-packages-and-modules/08-vendor-directory/01-vendor-service-hermetic-build
```

### Why the artifact is split into a package and a CLI

A `main()` that reads `os.Args` and calls `os.Exit` cannot be unit-tested — you
cannot capture its output or assert its exit code from another test in the same
package. So the logic lives in a real package, `normalizer`, exposing two pure-ish
functions: `Normalize`, which is a total function on a string, and `Run`, which
takes an explicit `args []string` and an `io.Writer` and returns an exit code
instead of calling `os.Exit`. The actual CLI, in `cmd/demo/main.go`, is a
three-line shell that wires `os.Args` and `os.Stdout` into `Run` and passes the
returned code to `os.Exit`. This is the standard testable-CLI shape, and it is
also what makes vendoring demonstrable: the vendored build compiles `./cmd/demo`,
which imports `example.com/normalizer` from within the same module.

### Why the hermetic test uses `-mod=vendor` explicitly

The service imports only the standard library, so `go mod vendor` reports "no
dependencies to vendor" and creates no `vendor/` directory — and, importantly,
`go build -mod=vendor` then still succeeds, because with an empty dependency set
there is nothing a vendored tree would need to supply. That is not a degenerate
case to apologize for; it is the honest demonstration that the *workflow* is
correct independent of whether third-party code is present. The test materializes
a fresh copy of the module in a temp directory, builds it once from the cache,
runs `go mod vendor`, builds it again with `-mod=vendor`, and asserts the two
binaries emit identical bytes for the same input. If a future dependency were
added, the exact same sequence would exercise a populated `vendor/` with no
change to the test. The test `t.Skip`s when the `go` toolchain is not on `PATH`,
so the pure output contract still runs in a restricted environment.

Create `normalizer.go`:

```go
package normalizer

import (
	"fmt"
	"io"
	"strings"
)

// Normalize canonicalizes a single log field: leading and trailing whitespace
// is removed and the remainder is upper-cased. It is a total function; the
// empty string maps to the empty string.
func Normalize(field string) string {
	return strings.ToUpper(strings.TrimSpace(field))
}

// Run is the CLI core. It expects args in os.Args form (args[0] is the program
// name) with exactly one operand: the log field to normalize. On success it
// writes the normalized field plus a newline to stdout and returns 0; on misuse
// it writes a usage line and returns 2. Returning a code instead of calling
// os.Exit is what makes the contract testable in-process.
func Run(args []string, stdout io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stdout, "usage: normalize FIELD")
		return 2
	}
	fmt.Fprintln(stdout, Normalize(args[1]))
	return 0
}
```

### The runnable demo

Create `cmd/demo/main.go`. It is the deployable CLI: the only thing that touches
`os.Args` and `os.Exit`.

```go
package main

import (
	"os"

	"example.com/normalizer"
)

func main() {
	os.Exit(normalizer.Run(os.Args, os.Stdout))
}
```

Run it:

```bash
go run ./cmd/demo "  warn:disk full  "
```

Expected output:

```
WARN:DISK FULL
```

### Tests

`TestRunUppercasesArgument` is the preserved output contract: it drives `Run`
with a synthetic `os.Args` and a `bytes.Buffer`, asserting both the normalized
output and the exit code, with no process spawned. `TestRunUsage` pins the
misuse path (exit 2). `TestHermeticVendorBuildMatchesCache` is the vendoring
proof: it writes a copy of the module into `t.TempDir()`, builds it from the
cache and again under `-mod=vendor`, and asserts identical output — skipping if
the toolchain is unavailable.

Create `normalizer_test.go`:

```go
package normalizer

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"trims and upcases", "  warn:disk full  ", "WARN:DISK FULL"},
		{"already upper", "ERROR", "ERROR"},
		{"empty", "", ""},
		{"internal spaces kept", "a b c", "A B C"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunUppercasesArgument(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	code := Run([]string{"normalize", "hello"}, &out)
	if code != 0 {
		t.Fatalf("Run exit = %d; want 0", code)
	}
	if got, want := out.String(), "HELLO\n"; got != want {
		t.Fatalf("Run output = %q; want %q", got, want)
	}
}

func TestRunUsage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := Run([]string{"normalize"}, &out); code != 2 {
		t.Fatalf("Run with no operand exit = %d; want 2", code)
	}
}

// sources materialized into the temp module for the hermetic build test.
const goModSrc = "module example.com/normalizer\n\ngo 1.23\n"

const libSrc = `package normalizer

import (
	"fmt"
	"io"
	"strings"
)

func Normalize(field string) string {
	return strings.ToUpper(strings.TrimSpace(field))
}

func Run(args []string, stdout io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stdout, "usage: normalize FIELD")
		return 2
	}
	fmt.Fprintln(stdout, Normalize(args[1]))
	return 0
}
`

const mainSrc = `package main

import (
	"os"

	"example.com/normalizer"
)

func main() {
	os.Exit(normalizer.Run(os.Args, os.Stdout))
}
`

func writeModule(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "normalizer.go"), []byte(libSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	demo := filepath.Join(dir, "cmd", "demo")
	if err := os.MkdirAll(demo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(demo, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
}

func binOutput(t *testing.T, bin, arg string) string {
	t.Helper()
	out, err := exec.Command(bin, arg).Output()
	if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return string(out)
}

func TestHermeticVendorBuildMatchesCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable; skipping hermetic vendor build")
	}
	dir := t.TempDir()
	writeModule(t, dir)

	cacheBin := filepath.Join(dir, "cache.bin")
	goRun(t, dir, "build", "-o", cacheBin, "./cmd/demo")

	// Vendoring a stdlib-only module reports "no dependencies to vendor"
	// and creates no vendor/ tree; -mod=vendor still builds correctly.
	goRun(t, dir, "mod", "vendor")

	vendorBin := filepath.Join(dir, "vendor.bin")
	goRun(t, dir, "build", "-mod=vendor", "-o", vendorBin, "./cmd/demo")

	const field = "  warn:disk full  "
	cacheOut := binOutput(t, cacheBin, field)
	vendorOut := binOutput(t, vendorBin, field)

	if cacheOut != vendorOut {
		t.Fatalf("hermetic mismatch: cache=%q vendor=%q", cacheOut, vendorOut)
	}
	if want := "WARN:DISK FULL\n"; vendorOut != want {
		t.Fatalf("vendored output = %q; want %q", vendorOut, want)
	}
}

func ExampleNormalize() {
	fmt.Printf("%q\n", Normalize("  error:timeout  "))
	// Output: "ERROR:TIMEOUT"
}
```

## Review

The service is correct when `Normalize` is a total function of its input —
`strings.ToUpper(strings.TrimSpace(field))` and nothing environmental — and when
`Run` returns an exit code rather than calling `os.Exit`, so the contract is
testable in-process. The vendoring proof is `TestHermeticVendorBuildMatchesCache`:
if the vendored binary ever diverged from the cache binary, the build would be
consulting something outside the committed tree, which is precisely the property
vendoring is supposed to eliminate. Note the deliberate choice to keep the
service stdlib-only: the workflow — `go mod vendor` then `go build -mod=vendor` —
is identical whether or not third-party dependencies are present, and the test
exercises it either way. The one structural mistake to avoid is folding the CLI
back into a bare `main()`; you would lose both the unit contract and the ability
to drive a hermetic build against `./cmd/demo`.

## Resources

- [go mod vendor](https://go.dev/ref/mod#go-mod-vendor) — what the command copies and prunes.
- [Vendoring and `-mod=vendor`](https://go.dev/ref/mod#vendoring) — the auto-enable rule and the module-mode flags.
- [`os/exec`](https://pkg.go.dev/os/exec) — `Command`, `CombinedOutput`, and `LookPath` used by the hermetic test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-modules-txt-inventory-parser.md](02-modules-txt-inventory-parser.md)
