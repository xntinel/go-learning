# Exercise 1: An internal ops CLI with clean dependency hygiene

Every dependency-management gate in this chapter reasons about a module, so the
chapter starts by building one: a small internal operations CLI, `depsvc`, whose
logic is extracted into a testable `Run` function. This is the original lesson's
service, reframed as a real tool and given the test contract the first version left
optional.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
depsvc/                    independent module: example.com/depsvc
  go.mod                   go 1.26
  depsvc.go                func Run(args []string, out, errOut io.Writer) int
  cmd/
    demo/
      main.go              wires Run to os.Stdout/os.Stderr and os.Exit
  depsvc_test.go           table test over arg slices with bytes.Buffer writers
```

- Files: `depsvc.go`, `cmd/demo/main.go`, `depsvc_test.go`.
- Implement: `Run(args []string, out, errOut io.Writer) int` using a `flag.FlagSet`, `strings.Join`, and `strings.ToUpper`; empty input prints a usage line and returns exit code 2.
- Test: call `Run` with argument slices and `bytes.Buffer` writers; assert stdout, the uppercase path, and the exit-code-2 usage path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/06-dependency-management/01-dep-hygiene-cli/cmd/demo
cd go-solutions/11-packages-and-modules/06-dependency-management/01-dep-hygiene-cli
```

### Why extract Run instead of writing main

The original version put everything in `main`, read `os.Args` through the package
`flag` default set, and called `os.Exit` inline. That is untestable: `os.Exit` ends
the process, the global `flag.CommandLine` carries state between calls, and there is
no way to capture what was printed. The senior move is to push all logic into a
`Run(args []string, out, errOut io.Writer) int` function — arguments in, writers in,
exit code out — and keep `main` as a three-line adapter that supplies `os.Args[1:]`,
`os.Stdout`, `os.Stderr`, and hands the returned code to `os.Exit`. Now a test can
drive `Run` with any argument slice, capture output in a `bytes.Buffer`, and assert
the exit code as an ordinary integer.

Using a fresh `flag.NewFlagSet` per call (rather than the global `flag.CommandLine`)
is part of the same discipline: each invocation is isolated, so a test does not leak
flag state into the next. `flag.ContinueOnError` makes `Parse` return an error
instead of calling `os.Exit` on a bad flag, which is what keeps `Run` a pure
function. Pointing the flag set's output at `errOut` routes usage text to the same
writer the test captures.

Create `depsvc.go`:

```go
package depsvc

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// Run executes the depsvc CLI: it joins its positional arguments into a line,
// optionally uppercases it with -upper, and writes it to out. It returns a
// process exit code (0 on success, 2 on a usage error) instead of calling
// os.Exit, so it is testable. On empty input it writes a usage line to errOut.
func Run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("depsvc", flag.ContinueOnError)
	fs.SetOutput(errOut)
	upper := fs.Bool("upper", false, "uppercase the output")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	text := strings.Join(fs.Args(), " ")
	if text == "" {
		fmt.Fprintln(errOut, "usage: depsvc [-upper] TEXT...")
		return 2
	}
	if *upper {
		text = strings.ToUpper(text)
	}
	fmt.Fprintln(out, text)
	return 0
}
```

### The runnable demo

`main` is the thin adapter: it forwards the real process arguments and streams into
`Run` and exits with the returned code. So the demo run is deterministic, it drives
two fixed invocations — one plain, one uppercased — and exits with the last code.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/depsvc"
)

func main() {
	depsvc.Run([]string{"cache", "warmed"}, os.Stdout, os.Stderr)
	os.Exit(depsvc.Run([]string{"-upper", "deploy", "done"}, os.Stdout, os.Stderr))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cache warmed
DEPLOY DONE
```

### Tests

The test drives `Run` directly with argument slices and `bytes.Buffer` writers,
which is only possible because `Run` returns a code instead of exiting. The table
covers the plain path, the `-upper` path, and the empty-input path that must return
2 and print the usage line to stderr. The `Example` doubles as executable
documentation verified by `go test`.

Create `depsvc_test.go`:

```go
package depsvc

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"plain", []string{"deploy", "started"}, 0, "deploy started\n", ""},
		{"upper", []string{"-upper", "deploy", "done"}, 0, "DEPLOY DONE\n", ""},
		{"single", []string{"ok"}, 0, "ok\n", ""},
		{"empty", nil, 2, "", "usage: depsvc [-upper] TEXT...\n"},
		{"upper-but-no-text", []string{"-upper"}, 2, "", "usage: depsvc [-upper] TEXT...\n"},
		{"bad-flag", []string{"-nope"}, 2, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := Run(tc.args, &out, &errOut)
			if code != tc.wantCode {
				t.Errorf("Run(%q) code = %d, want %d", tc.args, code, tc.wantCode)
			}
			if out.String() != tc.wantOut {
				t.Errorf("Run(%q) stdout = %q, want %q", tc.args, out.String(), tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(errOut.String(), tc.wantErr) {
				t.Errorf("Run(%q) stderr = %q, want to contain %q", tc.args, errOut.String(), tc.wantErr)
			}
		})
	}
}

func ExampleRun() {
	Run([]string{"-upper", "hello", "world"}, os.Stdout, os.Stderr)
	// Output: HELLO WORLD
}
```

## Review

`Run` is correct when it is a pure function of its arguments: same `args` in, same
code and same bytes out, with no reliance on process globals. The two structural
traps are calling `os.Exit` inside `Run` (which would kill the test process) and
reusing `flag.CommandLine` instead of a fresh `flag.NewFlagSet` (which would leak
flag state between calls). Both are avoided here. The bad-flag case returning 2 is a
consequence of `flag.ContinueOnError`: `Parse` reports the error, `Run` maps it to
the usage exit code rather than terminating. Run `go test -race` to confirm; the
`Example` verifies the uppercase path against a real `// Output:` line.

## Resources

- [`flag` package](https://pkg.go.dev/flag) — `flag.NewFlagSet`, `ContinueOnError`, and why a per-call flag set beats the global one.
- [`strings` package](https://pkg.go.dev/strings) — `strings.Join` and `strings.ToUpper`.
- [How I write HTTP services in Go](https://grafana.com/blog/2024/02/09/how-i-write-http-services-in-go-after-13-years/) — Mat Ryer on the `run(ctx, args, io.Writer) error`/`int` shape that separates `main` from testable logic.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-gomod-inspect-and-tidy-gate.md](02-gomod-inspect-and-tidy-gate.md)
