# Exercise 2: A Command Binary Importing an Internal Package

Real services are a `cmd/` binary over a library. This exercise builds that shape
with the library under `internal/`, so it is importable within the module but
invisible outside it, and a `run(args)` seam that makes the command testable
without spawning a process.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
greetcli/                   independent module: example.com/greetcli
  go.mod
  internal/
    greeter/
      greeter.go            package greeter: ErrEmptyName, Greeting
  cmd/
    demo/
      main.go               main() -> run(args); maps ErrEmptyName to a message
      main_test.go          run() over crafted argv slices
```

- Files: `internal/greeter/greeter.go`, `cmd/demo/main.go`, `cmd/demo/main_test.go`.
- Implement: a `run(args []string) error` seam that greets `args[1]` (default `World`) and maps `greeter.ErrEmptyName` to a user-facing error; `main()` prints it to stderr and exits non-zero.
- Test: call `run` with no-arg, name, and whitespace argv slices and assert the returned error with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/02-cmd-binary-imports-internal-package/internal/greeter go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/02-cmd-binary-imports-internal-package/cmd/demo
cd go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/02-cmd-binary-imports-internal-package
```

### Why internal/ and why a run seam

The `internal/` directory is a compiler-enforced visibility boundary: a package at
`example.com/greetcli/internal/greeter` can be imported by any package rooted at
`example.com/greetcli`, but by nothing outside that module. It is how you keep a
library private to one service — a consumer of your module physically cannot
import it, so you are free to change it without breaking anyone. Here the demo
imports `example.com/greetcli/internal/greeter`, which the module path makes legal.

The second design choice is the `run(args []string) error` seam. `main()` cannot
be unit-tested: it reads `os.Args`, writes `os.Stderr`, and calls `os.Exit`, none
of which a test can drive cleanly. So `main()` does nothing but wire the process
to `run`, and *all* logic lives in `run`, which takes its arguments as a slice and
returns an error. A test then calls `run([]string{"demo", "  "})` directly and
asserts the error, with no subprocess and no captured stderr. This "thin main over
a testable run" split is standard in production Go CLIs.

Create `internal/greeter/greeter.go`:

```go
package greeter

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned when the trimmed name is empty.
var ErrEmptyName = errors.New("name must not be empty")

// Greeting builds a greeting for name, trimming surrounding whitespace and
// rejecting an empty result.
func Greeting(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}
```

### The command

`run` defaults the name to `World` when no argument is passed, greets it, and
translates the library's `ErrEmptyName` into a message aimed at a human at a
terminal. It branches with `errors.Is`, not on the string, so the library is free
to reword its own error. `main()` is four lines: call `run`, and on error write to
stderr and exit 1.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/greetcli/internal/greeter"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run greets args[1] (default "World"), mapping a blank name to a user-facing
// error. It is the testable seam: main() only wires it to the process.
func run(args []string) error {
	name := "World"
	if len(args) > 1 {
		name = args[1]
	}

	g, err := greeter.Greeting(name)
	if err != nil {
		if errors.Is(err, greeter.ErrEmptyName) {
			return errors.New("please pass a non-empty name")
		}
		return err
	}
	fmt.Println(g)
	return nil
}
```

Run it three ways:

```bash
go run ./cmd/demo
go run ./cmd/demo Gopher
go run ./cmd/demo "  "
```

Expected output:

```
Hello, World!
Hello, Gopher!
error: please pass a non-empty name
```

The third line goes to stderr and the process exits with status 1.

### Testing run directly

The test lives in `package main` so it can call the unexported `run`. Each row is
an argv slice as `os.Args` would deliver it (element 0 is the program name). The
whitespace row asserts that `run` surfaces an error at all — its message is the
user-facing one, so the test checks that a non-nil error came back rather than
pinning exact text.

Create `cmd/demo/main_test.go`:

```go
package main

import "testing"

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "no args defaults to World", args: []string{"demo"}, wantErr: false},
		{name: "explicit name", args: []string{"demo", "Gopher"}, wantErr: false},
		{name: "blank name is an error", args: []string{"demo", "   "}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := run(tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("run(%q) = nil, want error", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("run(%q) = %v, want nil", tc.args, err)
			}
		})
	}
}
```

Run the gate:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

To prove `internal/` really is a boundary, note that another module cannot import
`example.com/greetcli/internal/greeter`; the compiler rejects it with `use of
internal package ... not allowed`. That is the guarantee that lets a service keep
implementation packages private.

## Review

The command is correct when `run` returns nil for a valid name (printing the
greeting) and a non-nil, user-facing error for a blank one, and `main` is the only
place that touches `os.Args`/`os.Exit`. The mistakes to avoid: do not put logic in
`main()` — it becomes untestable the moment it does more than call `run`. Do not
match the library's error text; branch on `greeter.ErrEmptyName` with `errors.Is`
so the mapping survives a reworded message. And do not try to import an
`internal/` package from outside the module to "reuse" it — that is the boundary
working as designed; promote the package out of `internal/` only when you
genuinely intend it as public API. Run `go test -race` to confirm the parallel
subtests share no mutable state.

## Resources

- [Go Modules Reference: internal packages](https://go.dev/ref/mod#module-path) — how the `internal/` boundary is scoped to the module.
- [Command Line Interfaces (CLIs)](https://go.dev/solutions/clis) — the standard thin-`main`-over-`run` shape for Go commands.
- [`os` package](https://pkg.go.dev/os) — `os.Args`, `os.Exit`, and `os.Stderr`.

---

Back to [01-library-module-and-contract-tests.md](01-library-module-and-contract-tests.md) | Next: [03-testable-examples-as-documentation.md](03-testable-examples-as-documentation.md)
