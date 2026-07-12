# 3. Subcommands with FlagSet

Subcommands turn one binary into a family of related tools. The standard `flag` package supports this with `flag.NewFlagSet`: each command owns its flags, output, usage, parsing errors, and validation.

## Concepts

### Dispatch Before Parsing Command Flags

Global flags and subcommand flags have different lifetimes. Parse global flags first, inspect the remaining first argument as the command name, then pass only that command's arguments to its own `FlagSet`.

### `ContinueOnError` Makes Commands Testable

`ExitOnError` calls `os.Exit`, which is hostile to tests. Use `ContinueOnError` inside command handlers and let `main` decide the process exit code.

### Usage Is Part of the Contract

A subcommand should print usage for malformed input without polluting successful stdout. Set each `FlagSet` output to stderr and customize `Usage` when the default header is not enough.

### Validation Errors Need Identity

Missing required arguments, bad enum values, and invalid numeric ranges should be distinguishable with sentinel errors. Wrap them with `%w` while preserving useful details.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Dispatch Commands

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

var (
	ErrMissingCommand = errors.New("missing command")
	ErrUnknownCommand = errors.New("unknown command")
	ErrMissingName    = errors.New("name is required")
	ErrBadPriority    = errors.New("priority must be between 0 and 5")
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("tasks", flag.ContinueOnError)
	global.SetOutput(stderr)
	verbose := global.Bool("verbose", false, "enable verbose output")
	if err := global.Parse(args); err != nil {
		return err
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		return ErrMissingCommand
	}
	if *verbose {
		fmt.Fprintln(stdout, "verbose=true")
	}

	switch remaining[0] {
	case "list":
		return runList(remaining[1:], stdout, stderr)
	case "add":
		return runAdd(remaining[1:], stdout, stderr)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownCommand, remaining[0])
	}
}

func runList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show completed tasks")
	format := fs.String("format", "table", "output format: table or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "list all=%t format=%s\n", *all, *format)
	return nil
}

func runAdd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "task name")
	priority := fs.Int("priority", 0, "priority from 0 to 5")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		fs.Usage()
		return ErrMissingName
	}
	if *priority < 0 || *priority > 5 {
		return fmt.Errorf("%w: got %d", ErrBadPriority, *priority)
	}
	fmt.Fprintf(stdout, "add name=%s priority=%d\n", *name, *priority)
	return nil
}
```

### Exercise 2: Test Command Contracts

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestRunDispatchesSubcommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"list", []string{"list", "-all", "-format=json"}, "list all=true format=json\n"},
		{"add", []string{"add", "-name=docs", "-priority=3"}, "add name=docs priority=3\n"},
		{"global", []string{"-verbose", "list"}, "verbose=true\nlist all=false format=table\n"},
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
		})
	}
}

func TestRunReturnsSentinelErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want error
	}{
		{"missing", nil, ErrMissingCommand},
		{"unknown", []string{"remove"}, ErrUnknownCommand},
		{"missing name", []string{"add"}, ErrMissingName},
		{"bad priority", []string{"add", "-name=x", "-priority=9"}, ErrBadPriority},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func Example_run() {
	var stdout, stderr bytes.Buffer
	_ = run([]string{"add", "-name=groceries", "-priority=2"}, &stdout, &stderr)
	fmt.Print(stdout.String())
	// Output: add name=groceries priority=2
}
```

### Exercise 3: Add a `done` Command

Add `done -id=<number>` with a required positive ID, a wrapped `ErrMissingID`, and a test for both success and missing ID.

## Common Mistakes

### Parsing Subcommand Flags With the Global Set

Wrong: calling the package-level `flag.Parse` and expecting `list -all` to work.

What happens: command-specific flags are unknown or global flags leak across commands.

Fix: use a separate `FlagSet` per subcommand.

### Exiting Inside Command Handlers

Wrong: calling `os.Exit` from `runAdd`.

What happens: tests cannot assert the error and deferred cleanup does not run.

Fix: return errors from handlers and let `main` exit.

### Printing Errors to Stdout

Wrong: usage and parse errors go to stdout.

What happens: scripts consuming stdout receive diagnostics mixed with data.

Fix: set `fs.SetOutput(stderr)` and reserve stdout for successful command output.

## Verification

From `~/go-exercises/subcommands`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add the `done` command and at least one table-driven validation test before rerunning the same commands.

## Summary

- Use `flag.NewFlagSet` for isolated command flags.
- Parse global flags before dispatching to subcommands.
- Keep command handlers testable by returning errors.
- Send diagnostics to stderr and data to stdout.

## What's Next

Next: [Cobra Commands, Flags, and Args](../04-cobra-commands-flags-args/04-cobra-commands-flags-args.md).

## Resources

- [Package flag: FlagSet](https://pkg.go.dev/flag#FlagSet)
- [Package flag: ContinueOnError](https://pkg.go.dev/flag#ContinueOnError)
- [Package errors](https://pkg.go.dev/errors)
