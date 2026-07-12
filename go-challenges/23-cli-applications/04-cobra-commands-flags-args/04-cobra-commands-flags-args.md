# 4. Cobra Commands, Flags, and Args

Frameworks such as Cobra popularized command trees, persistent flags, local flags, and argument validators. This lesson builds those same ideas with the standard `flag` package so the mechanics are visible and the code stays offline-verifiable. One important difference is that `flag` stops parsing at the first positional argument, so local flags appear before positional arguments: `add -priority=2 write-docs`.

## Concepts

### Command Trees Without Hidden State

A command tree is a dispatcher plus handlers. Global options are parsed once, then each handler parses its local options from the remaining argument slice.

### Persistent and Local Flags

Persistent flags apply to all subcommands; local flags apply only to one handler. With `flag`, the distinction is explicit: global flags live on the root `FlagSet`, local flags live on the command `FlagSet`.

### Argument Validators

Validators should run after flag parsing and before side effects. Return sentinel errors for missing, extra, or malformed arguments so tests can assert the command contract.

### Help and Errors

Good CLIs separate data from diagnostics. Successful command output goes to stdout; usage and parsing diagnostics go to stderr.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Build the Command Tree

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
	ErrNeedOneArg     = errors.New("command requires exactly one argument")
	ErrNeedAnyArgs    = errors.New("command requires at least one argument")
	ErrForceRequired  = errors.New("force flag is required")
	ErrBadPriority    = errors.New("priority must be between 0 and 5")
)

type rootOptions struct {
	verbose bool
	format  string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	var opts rootOptions
	root := flag.NewFlagSet("tasks", flag.ContinueOnError)
	root.SetOutput(stderr)
	root.BoolVar(&opts.verbose, "verbose", false, "enable verbose output")
	root.StringVar(&opts.format, "format", "text", "output format")
	if err := root.Parse(args); err != nil {
		return err
	}
	remaining := root.Args()
	if len(remaining) == 0 {
		return ErrMissingCommand
	}
	if opts.verbose {
		fmt.Fprintln(stdout, "verbose=true")
	}
	switch remaining[0] {
	case "add":
		return add(opts, remaining[1:], stdout, stderr)
	case "done":
		return done(remaining[1:], stdout, stderr)
	case "delete":
		return deleteTask(remaining[1:], stdout, stderr)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownCommand, remaining[0])
	}
}

func add(opts rootOptions, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	priority := fs.Int("priority", 0, "task priority from 0 to 5")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return ErrNeedOneArg
	}
	if *priority < 0 || *priority > 5 {
		return fmt.Errorf("%w: got %d", ErrBadPriority, *priority)
	}
	fmt.Fprintf(stdout, "add title=%q priority=%d format=%s\n", fs.Arg(0), *priority, opts.format)
	return nil
}

func done(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks done", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return ErrNeedAnyArgs
	}
	for _, id := range fs.Args() {
		fmt.Fprintf(stdout, "done id=%s\n", id)
	}
	return nil
}

func deleteTask(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "confirm deletion")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return ErrNeedOneArg
	}
	if !*force {
		return fmt.Errorf("%w: delete %s", ErrForceRequired, fs.Arg(0))
	}
	fmt.Fprintf(stdout, "delete id=%s\n", fs.Arg(0))
	return nil
}
```

### Exercise 2: Test Flags and Args

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestRunCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"add", []string{"-format=json", "add", "-priority=2", "write-docs"}, "add title=\"write-docs\" priority=2 format=json\n"},
		{"verbose", []string{"-verbose", "done", "1", "2"}, "verbose=true\ndone id=1\ndone id=2\n"},
		{"delete", []string{"delete", "-force", "9"}, "delete id=9\n"},
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

func TestRunValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want error
	}{
		{"missing command", nil, ErrMissingCommand},
		{"unknown", []string{"archive"}, ErrUnknownCommand},
		{"add missing arg", []string{"add"}, ErrNeedOneArg},
		{"done missing arg", []string{"done"}, ErrNeedAnyArgs},
		{"delete needs force", []string{"delete", "1"}, ErrForceRequired},
		{"bad priority", []string{"add", "-priority=8", "x"}, ErrBadPriority},
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
	_ = run([]string{"add", "-priority=4", "ship"}, &stdout, &stderr)
	fmt.Print(stdout.String())
	// Output: add title="ship" priority=4 format=text
}
```

### Exercise 3: Add a `list` Command

Add `list -all` and `list -status=<todo|done>` with a sentinel `ErrBadStatus`. Test the default, `-all`, and invalid status paths.

## Common Mistakes

### Reusing One FlagSet for Every Command

Wrong: adding all local flags to the root set.

What happens: unrelated commands accept flags they should reject.

Fix: each command constructs its own `FlagSet`.

### Validating Before Parsing

Wrong: checking `priority` before `fs.Parse(args)`.

What happens: validation sees defaults, not user input.

Fix: parse, check parse error, then validate.

### Hiding Global Options in Package Variables

Wrong: storing root options in mutable globals.

What happens: tests become order-dependent.

Fix: pass a small options struct into handlers.

## Verification

From `~/go-exercises/command-tree`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add the `list` command and rerun the same checks.

## Summary

- Command trees are dispatch plus per-command `FlagSet` parsing.
- Persistent flags can be modeled as root options passed to handlers.
- Argument validators should be explicit, testable, and side-effect free.
- Standard-library command trees are verbose but transparent.

## What's Next

Next: [Interactive Prompts](../05-interactive-prompts/05-interactive-prompts.md).

## Resources

- [Package flag](https://pkg.go.dev/flag)
- [Package flag: FlagSet.Parse](https://pkg.go.dev/flag#FlagSet.Parse)
- [Package errors: Is](https://pkg.go.dev/errors#Is)
