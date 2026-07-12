# 5. Interactive Prompts

Interactive CLIs still need a non-interactive core. The prompt layer should collect input; validation and execution should live in functions that tests can call directly.

## Concepts

### Prompts Are I/O, Not Business Logic

Use `io.Reader` and `io.Writer` instead of reading directly from the terminal. That lets tests provide scripted input and capture prompts.

### Flags Should Bypass Prompts

Automation and CI cannot answer questions. Provide flags for every required value and only prompt for missing values when interactive mode is enabled.

### Validation After Collection

Validate the final request, whether values came from flags or prompts. Sentinel errors keep validation independent from wording.

### Confirmation Is a Decision Point

Treat confirmation as data. A declined confirmation is not a crash; it is a controlled result that should be tested.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/23-cli-applications/05-interactive-prompts/05-interactive-prompts
cd go-solutions/23-cli-applications/05-interactive-prompts/05-interactive-prompts
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Build a Promptable Command

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

var (
	ErrEmptyName = errors.New("name must not be empty")
	ErrCancelled = errors.New("operation cancelled")
)

type request struct {
	Name    string
	Confirm bool
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "project name")
	yes := fs.Bool("yes", false, "confirm without prompting")
	interactive := fs.Bool("interactive", true, "prompt for missing values")
	if err := fs.Parse(args); err != nil {
		return err
	}

	req := request{Name: *name, Confirm: *yes}
	if *interactive {
		if err := prompt(&req, stdin, stdout); err != nil {
			return err
		}
	}
	if err := validate(req); err != nil {
		return err
	}
	if !req.Confirm {
		return ErrCancelled
	}
	fmt.Fprintf(stdout, "created %s\n", req.Name)
	return nil
}

func prompt(req *request, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	if req.Name == "" {
		fmt.Fprint(stdout, "Project name: ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		req.Name = strings.TrimSpace(scanner.Text())
	}
	if !req.Confirm {
		fmt.Fprint(stdout, "Create project? [y/N]: ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
		req.Confirm = answer == "y" || answer == "yes"
	}
	return nil
}

func validate(req request) error {
	if strings.TrimSpace(req.Name) == "" {
		return ErrEmptyName
	}
	return nil
}
```

### Exercise 2: Test Interactive and Non-Interactive Paths

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRunCreatesProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		input string
		want  string
	}{
		{"flags", []string{"-name=api", "-yes", "-interactive=false"}, "", "created api\n"},
		{"prompt", nil, "api\nyes\n", "Project name: Create project? [y/N]: created api\n"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if err := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunValidationAndCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		input string
		want  error
	}{
		{"empty", []string{"-interactive=false"}, "", ErrEmptyName},
		{"cancel", []string{"-name=api"}, "no\n", ErrCancelled},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func Example_validate() {
	fmtErr := validate(request{Name: "api"})
	if fmtErr == nil {
		fmt.Print("valid\n")
	}
	// Output: valid
}
```

### Exercise 3: Add a Language Choice

Add `-language`, prompt for it when missing, validate `go`, `rust`, or `python`, and return a wrapped `ErrBadLanguage` for invalid input.

## Common Mistakes

### Reading Directly From `os.Stdin`

Wrong: prompt helpers call `fmt.Scanln` directly.

What happens: tests hang or require a terminal.

Fix: accept `io.Reader` and `io.Writer` parameters.

### Prompting in CI

Wrong: missing flags always trigger prompts.

What happens: automation stalls forever.

Fix: support `-interactive=false` and fail fast on missing required values.

### Treating Cancellation as Success

Wrong: printing `created` even when the user answered no.

What happens: destructive operations can run after a declined confirmation.

Fix: return `ErrCancelled` and test it.

## Verification

From `~/go-exercises/interactive-prompts`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add the language choice and rerun the same commands.

## Summary

- Keep prompts as injectable I/O.
- Provide flags for non-interactive automation.
- Validate after all input sources are collected.
- Test confirmation, cancellation, and empty input.

## What's Next

Next: [Progress Bars and Spinners](../06-progress-bars-and-spinners/06-progress-bars-and-spinners.md).

## Resources

- [Package bufio: Scanner](https://pkg.go.dev/bufio#Scanner)
- [Package io](https://pkg.go.dev/io)
- [Package flag](https://pkg.go.dev/flag)
