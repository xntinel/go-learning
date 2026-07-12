# Exercise 2: A CLI Binary Wired to the Library

A binary that calls `os.Exit` deep in its logic is untestable, because
`os.Exit` kills the test process too. The fix is the `run` pattern: `main` does
nothing but call `run` and translate its error into an exit code, while `run`
takes its arguments and output writer as parameters and *returns* an error. This
exercise wires a CLI to the `hello` library that way, so the whole command is
exercised by an ordinary table-driven test.

This module is self-contained: it bundles its own copy of the `hello` library so
it builds and gates with no other exercise present.

## What you'll build

```text
greetcli/                       independent module: example.com/greetcli
  go.mod
  internal/hello/
    hello.go                    bundled: ErrEmptyName, Greet
  cmd/demo/
    main.go                     main -> run(args, out) error; os.Exit only in main
    main_test.go                table test of run() capturing output + errors.Is
```

Files: `internal/hello/hello.go`, `cmd/demo/main.go`, `cmd/demo/main_test.go`.
Implement: `run(args []string, out io.Writer) error` mapping `ErrEmptyName` to a user-facing message; `main` calls `os.Exit(1)` on error.
Test: call `run` directly with argument slices, capturing output in a buffer and asserting errors with `errors.Is`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/02-demo-cli-with-run-pattern/internal/hello go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/02-demo-cli-with-run-pattern/cmd/demo
cd go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/02-demo-cli-with-run-pattern
```

### The run pattern, and why output is a parameter

`main` is the only place `os.Exit` may appear. Everything a test wants to check
lives in `run(args []string, out io.Writer) error`: it derives the name from
`args`, calls the library, writes success output to `out`, and returns any error.
The test then calls `run` with a crafted `args` slice and a `bytes.Buffer` for
`out`, and asserts both the captured text and the returned error — no subprocess,
no stdout hijacking, no `os.Exit` blowing up the test binary. Passing `out` as an
`io.Writer` rather than hard-coding `os.Stdout` is what makes the success output
assertable; `main` supplies the real `os.Stdout`.

The error mapping matters too. The library returns the typed `ErrEmptyName`; the
CLI translates that *known* condition into a friendly message with
`errors.Is`, and passes anything unexpected straight through. `main` prints the
final error to `os.Stderr` (not stdout, so it does not pollute piped output) and
exits non-zero, which is how a shell or CI step detects failure.

Create `internal/hello/hello.go` (bundled copy):

```go
// internal/hello/hello.go
package hello

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greet when the name is empty after trimming.
var ErrEmptyName = errors.New("name must not be empty")

// Greet returns a greeting for name, trimming surrounding whitespace and
// rejecting an empty result with ErrEmptyName.
func Greet(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"example.com/greetcli/internal/hello"
)

func main() {
	if err := run(os.Args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run holds all the command's logic so it can be tested without os.Exit.
// It writes success output to out and returns any error to main.
func run(args []string, out io.Writer) error {
	name := "World"
	if len(args) > 1 {
		name = args[1]
	}
	g, err := hello.Greet(name)
	if err != nil {
		if errors.Is(err, hello.ErrEmptyName) {
			return errors.New("please pass a non-empty name")
		}
		return err
	}
	fmt.Fprintln(out, g)
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
go run ./cmd/demo Gopher
go run ./cmd/demo "  "
```

Expected output:

```text
Hello, World!
Hello, Gopher!
error: please pass a non-empty name
```

(The first two lines are the successful runs; the third line is printed to
stderr by the `"  "` run, which also exits with status 1.)

### The test

The test calls `run` directly. The happy cases assert the captured buffer; the
whitespace case asserts the user-facing error message. Because `run` deliberately
returns a fresh message (`please pass a non-empty name`) rather than propagating
the library sentinel, the test asserts that message rather than matching
`ErrEmptyName` — the translation to a friendly string is exactly the behavior
under test here.

Create `cmd/demo/main_test.go`:

```go
// cmd/demo/main_test.go
package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{name: "no args", args: []string{"demo"}, want: "Hello, World!\n"},
		{name: "with name", args: []string{"demo", "Gopher"}, want: "Hello, Gopher!\n"},
		{name: "whitespace", args: []string{"demo", "  "}, wantErr: "please pass a non-empty name"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			err := run(tc.args, &buf)

			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("run(%v) err = %v, want %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v) unexpected err = %v", tc.args, err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("run(%v) out = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
```

## Review

The command is correct when `main` is a three-line shim and every decision lives
in `run`: no `os.Exit` outside `main`, success text written to the injected
`out`, error text sent to `os.Stderr`, and a non-zero exit on failure. The trap
is hard-coding `os.Stdout` inside `run`, which forces the test to capture global
stdout and makes parallel tests race. Confirm with `go test -race ./...` (the
table runs in parallel) and by running the three documented invocations, checking
that the whitespace run exits non-zero (`echo $?` prints `1`).

## Resources

- [Command-line interfaces (Go blog: subcommands and the run pattern)](https://go.dev/talks/2014/testing.slide) — the testable `run`-returns-error shape.
- [`os.Exit`](https://pkg.go.dev/os#Exit) — why it bypasses deferred functions and must stay in `main`.
- [`io.Writer`](https://pkg.go.dev/io#Writer) — injecting the output sink to make it assertable.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-greet-library-with-sentinel-error.md](01-greet-library-with-sentinel-error.md) | Next: [03-go-get-third-party-dependency.md](03-go-get-third-party-dependency.md)
