# 1. Flag Package Basics

The standard `flag` package is small, but its defaults shape many Go command-line tools: flags are declared before parsing, parsed once from an argument slice, and then separated from positional arguments. The hard part is not calling `flag.String`; it is keeping parsing testable and validating values instead of trusting them.

## Concepts

### Parsing Is a Boundary

`flag.Parse` parses `os.Args[1:]` on the package-level command-line set. That is convenient for tiny programs, but tests need repeatable inputs and captured output. Put the real work behind a `run(args, stdout, stderr)` function and let `main` only adapt `os.Args`, `os.Stdout`, and `os.Stderr`.

### Pointer Flags and Var Flags

Functions such as `String`, `Int`, and `Bool` return pointers. The values behind those pointers are defaults until `Parse` succeeds. The `StringVar`, `IntVar`, and `BoolVar` forms bind parsed values into an existing struct, which is clearer once a command has more than one option.

### Validation Is Separate From Parsing

The parser knows that `-count=abc` is not an integer, but it does not know that your program rejects `-count=0`. Validate after `Parse` and return sentinel errors wrapped with `%w`, so tests can use `errors.Is` instead of comparing strings.

### Flag Syntax and Positional Arguments

The package accepts `-flag`, `--flag`, `-flag=value`, and `-flag value` for non-boolean flags. Parsing stops before the first non-flag argument or after `--`; remaining values are available through `Args` on the `FlagSet`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/23-cli-applications/01-flag-package-basics/01-flag-package-basics
cd go-solutions/23-cli-applications/01-flag-package-basics/01-flag-package-basics
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Build a Testable CLI Entry Point

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

var ErrInvalidCount = errors.New("count must be at least 1")

type config struct {
	name  string
	count int
	loud  bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	cfg := config{}
	fs := flag.NewFlagSet("greet", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.name, "name", "World", "name to greet")
	fs.IntVar(&cfg.count, "count", 1, "number of greetings")
	fs.BoolVar(&cfg.loud, "loud", false, "print uppercase greeting")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.count < 1 {
		return fmt.Errorf("invalid -count %d: %w", cfg.count, ErrInvalidCount)
	}

	greeting := fmt.Sprintf("Hello, %s!", cfg.name)
	if cfg.loud {
		greeting = strings.ToUpper(greeting)
	}
	for range cfg.count {
		fmt.Fprintln(stdout, greeting)
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stdout, "args: %s\n", strings.Join(rest, ","))
	}
	return nil
}
```

### Exercise 2: Pin Behavior With Tests

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunParsesFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"defaults", nil, "Hello, World!\n"},
		{"count", []string{"-name=Gopher", "-count=2"}, "Hello, Gopher!\nHello, Gopher!\n"},
		{"loud", []string{"--name", "Gopher", "-loud"}, "HELLO, GOPHER!\n"},
		{"rest", []string{"-name=Gopher", "file1", "file2"}, "Hello, Gopher!\nargs: file1,file2\n"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			if err := run(tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunRejectsInvalidCount(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run([]string{"-count=0"}, &stdout, &stderr)
	if !errors.Is(err, ErrInvalidCount) {
		t.Fatalf("err = %v, want ErrInvalidCount", err)
	}
}

func TestRunReportsParseErrors(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run([]string{"-count=abc"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() error = nil, want parse error")
	}
	if !strings.Contains(stderr.String(), "invalid value") {
		t.Fatalf("stderr = %q, want flag parse diagnostic", stderr.String())
	}
}
```

### Exercise 3: Add Your Own Case

Add a test proving that `--` stops flag parsing, so `run([]string{"--", "-name=NotAFlag"}, ...)` treats `-name=NotAFlag` as a positional argument and keeps the default greeting.

## Common Mistakes

### Reading Values Before Parse

Wrong: using `cfg.count` or `*name` before calling `Parse`; it still has the default value.

What happens: validation or output ignores the actual command line.

Fix: define all flags, call `Parse`, check the returned error, then read values.

### Using the Global Flag Set in Tests

Wrong: defining flags on `flag.CommandLine` in helper functions used by tests.

What happens: later tests panic because a flag name is registered twice, or parse state leaks between tests.

Fix: create a fresh `flag.NewFlagSet` inside `run`.

### Matching Error Text Instead of Sentinels

Wrong: testing `err.Error() == "invalid -count 0: count must be at least 1"`.

What happens: harmless wording changes break tests.

Fix: wrap `ErrInvalidCount` with `%w` and assert `errors.Is(err, ErrInvalidCount)`.

## Verification

From `~/go-exercises/flag-basics`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Then add the `--` test from Exercise 3 and run the same commands again. The CLI can be tried manually with `go run . -name=Gopher -count=2`, but `go test` is the verification.

## Summary

- Use a fresh `FlagSet` for testable parsing.
- Parse first, then validate, then run the command behavior.
- Keep `main` small and put behavior in `run(args, stdout, stderr)`.
- Use sentinel errors and `errors.Is` for validation failures.

## What's Next

Next: [Custom Flag Types](../02-custom-flag-types/02-custom-flag-types.md).

## Resources

- [Package flag](https://pkg.go.dev/flag)
- [Package testing](https://pkg.go.dev/testing)
- [Effective Go: Errors](https://go.dev/doc/effective_go#errors)
