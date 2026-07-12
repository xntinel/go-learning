# Exercise 3: A CLI That Surfaces Writer Errors

A binary is just another package the linter and the test runner care about. This
exercise splits `main` into a thin `main()` and a testable `run([]string) error`,
maps the writer's `ErrEmptyName` sentinel to a user-facing message, and drives it
from a `main_test.go` — so the CLI is linted and tested like any library.

This module is self-contained: its own `go mod init`, the writer inline, a
`cmd/demo` with the `main`/`run` split, and a test that exercises `run` directly.

## What you'll build

```text
greetcli/                     independent module: example.com/greetcli
  go.mod                      go 1.24
  internal/
    writer/
      writer.go               GreetFile, ErrEmptyName
  cmd/
    demo/
      main.go                 main() -> run([]string) error; sentinel -> message
      main_test.go            table test over run(): stdout, error, exit intent
```

- Files: `internal/writer/writer.go`, `cmd/demo/main.go`, `cmd/demo/main_test.go`.
- Implement: `run(args []string) error` that parses `args`, calls `GreetFile`, prints the destination on success, and translates `ErrEmptyName` into a friendly error; `main()` maps any error to stderr and a non-zero exit.
- Test: a table test that calls `run` with argument slices and asserts the returned error (via `errors.Is`) and the captured stdout.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/03-cli-error-surfacing/cmd/demo go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/03-cli-error-surfacing/internal/writer
cd go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/03-cli-error-surfacing
```

### Why split main() from run()

`func main()` takes no arguments, returns nothing, and its only way to signal
failure is `os.Exit`, which is untestable and unlintable in any useful way — a test
cannot call `main()` and observe an exit code without spawning a subprocess. The
standard Go idiom is to keep `main()` to three lines and push all logic into a
`run(args []string) (err error)` that returns an error and writes to an injected or
package-level writer. `main()` then does the one thing only it can: translate the
error to stderr and a process exit code.

This split is also what makes the CLI *lintable* the same way a library is. With the
logic in `run`, `errcheck` sees the `fmt.Fprintln`/`fmt.Println` calls in a normal
function and the flow is analyzable; there is no `os.Exit` buried mid-logic to cut an
analyzer's control-flow short. And because `run` returns an error instead of exiting,
a test can call it directly and assert on the returned error with `errors.Is`.

The sentinel translation matters for UX: `GreetFile` returns `ErrEmptyName`, a
programmatic value. The CLI catches it with `errors.Is` and returns a human message
("please pass a non-empty name") so the user sees guidance, not an internal error
string. Other errors pass through unchanged for the operator to see verbatim.

Create `internal/writer/writer.go`:

```go
package writer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrEmptyName is returned when the input name is empty after trimming.
var ErrEmptyName = errors.New("name must not be empty")

// GreetFile writes "Hello, <name>!\n" to a new file at path, creating the
// parent directory if needed.
func GreetFile(path, name string) error {
	trimmed := trim(name)
	if trimmed == "" {
		return ErrEmptyName
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintf(f, "Hello, %s!\n", trimmed); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func trim(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' {
			out = append(out, r)
		}
	}
	return string(out)
}
```

### The CLI: main() delegates to run()

`run` takes `args` (so a test can pass its own slice instead of `os.Args`) and an
`io.Writer` for stdout (so a test can capture output into a buffer). `main()` wires
the real `os.Args` and `os.Stdout`, then maps any error to stderr and exit code 1.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"example.com/greetcli/internal/writer"
)

func main() {
	if err := run(os.Args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run parses args, writes a greeting, and reports the destination. It returns
// an error instead of exiting so it can be tested directly.
func run(args []string, stdout io.Writer) error {
	name := "World"
	if len(args) > 1 && args[1] != "" {
		name = args[1]
	}
	// Default to a fixed, path-agnostic destination so the demo output is
	// byte-stable across platforms. Callers (and tests) can override with args[2].
	out := "greeting.txt"
	if len(args) > 2 {
		out = args[2]
	}

	if err := writer.GreetFile(out, name); err != nil {
		if errors.Is(err, writer.ErrEmptyName) {
			return errors.New("please pass a non-empty name")
		}
		return err
	}
	if _, err := fmt.Fprintln(stdout, "wrote greeting to", out); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	return nil
}
```

### The runnable demo

Run it three ways to see success, an explicit name, and the empty-name error:

```bash
go run ./cmd/demo
go run ./cmd/demo Gopher
go run ./cmd/demo "  "
```

Expected output (stderr shown after stdout). The default destination is a fixed
relative path, so the two success lines are byte-identical on every platform:

```
wrote greeting to greeting.txt
wrote greeting to greeting.txt
error: please pass a non-empty name
```

The first two commands write to the same default path (`greeting.txt` in the
working directory) and exit 0; the third writes nothing, prints the friendly
message to stderr, and exits 1.

### Tests

The test drives `run` directly with argument slices, captures stdout into a
`bytes.Buffer`, and writes into `t.TempDir()` so nothing leaks. It asserts the
returned error with `errors.Is` and checks the stdout line on the success paths.

Create `cmd/demo/main_test.go`:

```go
package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"example.com/greetcli/internal/writer"
)

func TestRun(t *testing.T) {
	t.Parallel()

	dst := filepath.Join(t.TempDir(), "out.txt")

	tests := []struct {
		name       string
		args       []string
		wantErr    error
		wantStdout string
	}{
		{
			name:       "default name",
			args:       []string{"demo", "", dst},
			wantStdout: "wrote greeting to " + dst + "\n",
		},
		{
			name:       "explicit name",
			args:       []string{"demo", "Gopher", dst},
			wantStdout: "wrote greeting to " + dst + "\n",
		},
		{
			name:    "empty name rejected",
			args:    []string{"demo", "   ", dst},
			wantErr: writer.ErrEmptyName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			err := run(tc.args, &buf)

			if tc.wantErr != nil {
				// run translates ErrEmptyName into a user-facing message,
				// so match on the message text it returns.
				if err == nil || !strings.Contains(err.Error(), "non-empty name") {
					t.Fatalf("run(%q) err = %v, want a non-empty-name message", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%q) unexpected err = %v", tc.args, err)
			}
			if got := buf.String(); got != tc.wantStdout {
				t.Fatalf("run(%q) stdout = %q, want %q", tc.args, got, tc.wantStdout)
			}
		})
	}
}
```

Note the empty-name case: `args[1]` is `"   "`, which `GreetFile` trims to empty and
rejects with `ErrEmptyName`; `run` catches that with `errors.Is` and returns the
friendly message, so the test matches on the message text.

## Review

The CLI is correct when `main()` holds no logic beyond wiring and exit mapping, and
`run` is a pure-ish function returning an error that a test can assert. The success
paths print exactly `wrote greeting to <path>` and return nil; the empty-name path
returns a message error and writes nothing to stdout. Injecting the `io.Writer` is
what makes stdout testable without hijacking `os.Stdout`, and passing `args`
explicitly is what lets one test drive many argument shapes. The mistakes to avoid:
calling `os.Exit` inside `run` (untestable, and it skips deferred cleanups), printing
the raw sentinel to the user instead of translating it, and discarding the
`fmt.Fprintln` error (checked here, so `errcheck` stays clean on the `cmd` package
too). Verify with `go test -race ./...`.

## Resources

- [Command Go: writing command-line programs](https://pkg.go.dev/cmd/go) — how a `package main` binary is built and run.
- [`testing`: capturing output with a buffer](https://pkg.go.dev/testing) — the standard pattern for testing writers.
- [Go blog: Errors are values](https://go.dev/blog/errors-are-values) — returning errors from `run` instead of exiting.

---

Back to [02-hermetic-table-tests-for-io.md](02-hermetic-table-tests-for-io.md) | Next: [04-reproducible-golangci-config-v2.md](04-reproducible-golangci-config-v2.md)
