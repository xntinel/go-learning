# Exercise 2: A Thin, Testable CLI main With run() Indirection

The most important habit for a `cmd/<name>` package is keeping `main()` a
one-liner and putting the real work behind a `run(args, stdout, stderr)` function
that injects its streams. This exercise builds that CLI: `main()` only wires
`os.Args`, the process streams, and the exit code; a `Run` function holds all the
logic and gets all the tests, asserted through `bytes.Buffer` writers with no
process ever spawned.

This module is fully self-contained: its own `go mod init`, the greeting library
inline, its own demo and tests.

## What you'll build

```text
myapp/                         module github.com/example/myapp
  go.mod                       go 1.24
  internal/
    config/config.go           AppName, Version
    greeting/greeting.go       ErrEmptyName, Greet
    cli/
      cli.go                   Run(args []string, stdout, stderr io.Writer) error
      cli_test.go              table-driven over injected writers
  cmd/
    cli/main.go                thin main: Run(os.Args, os.Stdout, os.Stderr) + os.Exit
    demo/main.go               runnable: drives Run with a fixed args slice
```

- Files: `internal/config/config.go`, `internal/greeting/greeting.go`, `internal/cli/cli.go`, `internal/cli/cli_test.go`, `cmd/cli/main.go`, `cmd/demo/main.go`.
- Implement: `cli.Run(args []string, stdout, stderr io.Writer) error` that greets `args[1]` (default `"World"`), mapping `ErrEmptyName` to a user-facing message; a `main()` that only wires streams and the exit code.
- Test: `cli_test.go` calls `Run` with crafted args and `bytes.Buffer` writers, asserting the returned error (nil or the mapped message) and stdout content per case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/myapp/internal/config ~/go-exercises/myapp/internal/greeting ~/go-exercises/myapp/internal/cli ~/go-exercises/myapp/cmd/cli ~/go-exercises/myapp/cmd/demo
cd ~/go-exercises/myapp
go mod init github.com/example/myapp
go mod edit -go=1.24
```

### Why the logic lives in Run, not main

The rule is: `main()` wires, `Run` works. `main()` reads `os.Args`, chooses the
real process streams, calls `Run`, and translates the returned error into an exit
code. It contains no branching logic of its own, so there is nothing in it worth
testing that a compiler check does not already cover. Everything else — argument
handling, calling the library, mapping a sentinel error to a friendly message,
deciding what to print — lives in `Run(args []string, stdout, stderr io.Writer)
error`. Because `Run` takes its writers as `io.Writer` parameters instead of
reaching for `os.Stdout` directly, a test can hand it two `bytes.Buffer`s and
assert the exact bytes it produced, alongside the error it returned. That is the
whole point of the indirection: the command becomes unit-testable without
exec-ing a binary.

`Run` is exported and placed in `internal/cli` rather than left unexported inside
`package main` for one concrete reason: both the real binary (`cmd/cli`) and the
demo (`cmd/demo`) call the same function, and a `package main` symbol cannot be
imported. Moving the logic into an importable internal package is the natural
next step once more than one caller needs it, and it does not weaken the pattern —
`main()` is still a one-liner.

The error contract is the subtle part. `greeting.Greet` returns the sentinel
`ErrEmptyName` for a whitespace-only name. `Run` catches that specific case with
`errors.Is` and returns a *new*, user-facing error (`"please pass a non-empty
name"`) — the internal sentinel is an implementation detail, not something to
print raw at a user. Any other error is returned unchanged. `main()` then prints
whatever error `Run` returns to stderr and exits non-zero.

Create `internal/config/config.go`:

```go
package config

const (
	// AppName is the application name shared across binaries.
	AppName = "myapp"
	// Version is the application version.
	Version = "0.1.0"
)
```

Create `internal/greeting/greeting.go`:

```go
package greeting

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greet when the name is empty after trimming.
var ErrEmptyName = errors.New("name must not be empty")

// Greet formats a greeting for name, rejecting empty input with ErrEmptyName.
func Greet(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("[%s %s] %s says hello", "myapp", "0.1.0", trimmed), nil
}
```

Create `internal/cli/cli.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/example/myapp/internal/config"
	"github.com/example/myapp/internal/greeting"
)

// Run executes the CLI: it greets args[1] (defaulting to "World") and writes
// the result to stdout. It writes nothing to stderr itself; the caller decides
// how to render a returned error. Injecting the writers is what makes the whole
// command unit-testable without spawning a process.
func Run(args []string, stdout, stderr io.Writer) error {
	name := "World"
	if len(args) > 1 {
		name = args[1]
	}

	g, err := greeting.Greet(name)
	if err != nil {
		if errors.Is(err, greeting.ErrEmptyName) {
			// Translate the internal sentinel into a user-facing message.
			return errors.New("please pass a non-empty name")
		}
		return err
	}

	fmt.Fprintf(stdout, "%s %s\n", config.AppName, config.Version)
	fmt.Fprintln(stdout, g)
	return nil
}
```

Create `cmd/cli/main.go` — the thin wiring layer:

```go
package main

import (
	"fmt"
	"os"

	"github.com/example/myapp/internal/cli"
)

func main() {
	if err := cli.Run(os.Args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

### The runnable demo

The demo drives `Run` directly with a fixed argument slice and the real
`os.Stdout`, so its output is a real run of the command's logic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/example/myapp/internal/cli"
)

func main() {
	if err := cli.Run([]string{"cli", "Gopher"}, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
myapp 0.1.0
[myapp 0.1.0] Gopher says hello
```

### Tests

Because `Run` takes its writers, the test asserts behavior, not just the returned
error: it captures stdout in a `bytes.Buffer` and checks the exact text, and it
asserts the returned error's message on the rejection row. No binary is spawned.

Create `internal/cli/cli_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantOut    string
		wantErrMsg string
	}{
		{
			name:    "default name",
			args:    []string{"cli"},
			wantOut: "myapp 0.1.0\n[myapp 0.1.0] World says hello\n",
		},
		{
			name:    "explicit name",
			args:    []string{"cli", "Gopher"},
			wantOut: "myapp 0.1.0\n[myapp 0.1.0] Gopher says hello\n",
		},
		{
			name:       "whitespace rejected",
			args:       []string{"cli", "   "},
			wantErrMsg: "please pass a non-empty name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := Run(tc.args, &stdout, &stderr)

			if tc.wantErrMsg != "" {
				if err == nil || err.Error() != tc.wantErrMsg {
					t.Fatalf("Run(%v) err = %v, want %q", tc.args, err, tc.wantErrMsg)
				}
				if stdout.Len() != 0 {
					t.Fatalf("Run(%v) wrote to stdout on error: %q", tc.args, stdout.String())
				}
				return
			}

			if err != nil {
				t.Fatalf("Run(%v) unexpected err = %v", tc.args, err)
			}
			if got := stdout.String(); got != tc.wantOut {
				t.Fatalf("Run(%v) stdout = %q, want %q", tc.args, got, tc.wantOut)
			}
			if !strings.HasPrefix(stdout.String(), "myapp ") {
				t.Fatalf("Run(%v) missing version banner: %q", tc.args, stdout.String())
			}
		})
	}
}
```

## Review

The command is correct when `main()` contains no decisions and `Run` contains all
of them: `Run` writes the banner and greeting to its `stdout` writer for a valid
name, and returns the mapped `"please pass a non-empty name"` error (writing
nothing to stdout) for a whitespace-only name. The test proves this by injecting
buffers and asserting both the produced bytes and the returned error — the exact
thing you cannot do when logic hides inside `main()`. Two traps to avoid: do not
call `os.Stdout`/`os.Exit` from inside `Run` (that reintroduces the untestable
coupling the writer parameters exist to remove); and do not surface the internal
`ErrEmptyName` text to the user — map it to a friendly message and keep the
sentinel for `errors.Is` branching. Run `go test -race ./...` to confirm.

## Resources

- [Command Go](https://pkg.go.dev/cmd/go) — how `go build`/`go run` treat a `main` package under `cmd/`.
- [`io.Writer`](https://pkg.go.dev/io#Writer) — the interface that makes stream injection and buffer-based testing possible.
- [Testing techniques (Go blog / talks index)](https://go.dev/blog/) — background on injecting dependencies to keep `main` thin and testable.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-http-server-and-httptest.md](03-http-server-and-httptest.md)
