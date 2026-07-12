# 9. Shell Completion Generation

Shell completion is a command contract: the binary must be able to list commands, flags, and dynamic values without performing the main action. You can implement that contract with the standard `flag` package by adding a dedicated `completion` command and a hidden `complete` query command.

## Concepts

### Generation and Querying Are Different

`completion bash` prints a shell script. `complete <context>` prints possible values for tests and for the script to call. Keeping those separate makes dynamic completion testable.

### Dynamic Completion Must Avoid Side Effects

Completion can run many times while a user presses Tab. It must not mutate data, prompt, or print diagnostics to stdout.

### Directives Are Policy

Completions should state whether file completion is allowed. This lesson uses plain text output and `no-file` as a directive line so behavior is visible without framework magic.

### Stable Ordering

Completion output should be sorted. Unstable suggestions make tests flaky and shells harder to use.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/23-cli-applications/09-shell-completion-generation/09-shell-completion-generation
cd go-solutions/23-cli-applications/09-shell-completion-generation/09-shell-completion-generation
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Add Completion Commands

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

var (
	ErrMissingCommand = errors.New("missing command")
	ErrUnknownCommand = errors.New("unknown command")
	ErrUnknownShell   = errors.New("unknown shell")
	ErrUnknownContext = errors.New("unknown completion context")
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return ErrMissingCommand
	}
	switch args[0] {
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	case "complete":
		return runComplete(args[1:], stdout, stderr)
	case "add":
		return runAdd(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownCommand, args[0])
	}
}

func runCompletion(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks completion", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return ErrUnknownShell
	}
	shell := fs.Arg(0)
	if shell != "bash" && shell != "zsh" && shell != "fish" {
		return fmt.Errorf("%w: %s", ErrUnknownShell, shell)
	}
	fmt.Fprintf(stdout, "# %s completion for tasks\n", shell)
	fmt.Fprintln(stdout, "# call: tasks complete <context> <prefix>")
	return nil
}

func runComplete(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks complete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return ErrUnknownContext
	}
	context := fs.Arg(0)
	prefix := ""
	if fs.NArg() > 1 {
		prefix = fs.Arg(1)
	}
	items, err := complete(context, prefix)
	if err != nil {
		return err
	}
	for _, item := range items {
		fmt.Fprintln(stdout, item)
	}
	fmt.Fprintln(stdout, "directive:no-file")
	return nil
}

func complete(context, prefix string) ([]string, error) {
	sets := map[string][]string{
		"command":  {"add", "completion", "list"},
		"priority": {"critical", "high", "low", "medium"},
		"task":     {"buy-groceries", "deploy-v2", "fix-bug", "write-docs"},
	}
	items, ok := sets[context]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownContext, context)
	}
	var out []string
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out, nil
}

func runAdd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	priority := fs.String("priority", "medium", "priority")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "added priority=%s\n", *priority)
	return nil
}
```

### Exercise 2: Test Static and Dynamic Completion

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		context string
		prefix  string
		want    []string
	}{
		{"commands", "command", "", []string{"add", "completion", "list"}},
		{"priority prefix", "priority", "h", []string{"high"}},
		{"task prefix", "task", "d", []string{"deploy-v2"}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := complete(tc.context, tc.prefix)
			if err != nil {
				t.Fatalf("complete() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("complete() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRunCompleteCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if err := run([]string{"complete", "priority", "c"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "critical\ndirective:no-file\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want error
	}{
		{"missing", nil, ErrMissingCommand},
		{"bad command", []string{"bad"}, ErrUnknownCommand},
		{"bad shell", []string{"completion", "powershell"}, ErrUnknownShell},
		{"bad context", []string{"complete", "status"}, ErrUnknownContext},
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

func Example_complete() {
	items, _ := complete("task", "w")
	for _, item := range items {
		fmt.Println(item)
	}
	// Output: write-docs
}
```

### Exercise 3: Add Status Completion

Add a `status` context with `todo`, `in-progress`, `done`, and `blocked`; test full output and prefix filtering.

## Common Mistakes

### Doing Work During Completion

Wrong: completion creates or deletes records while loading suggestions.

What happens: pressing Tab changes application state.

Fix: completion functions must be read-only.

### Unsorted Suggestions

Wrong: returning suggestions from map iteration.

What happens: shells and tests see unstable order.

Fix: sort suggestions before printing.

### Mixing Diagnostics With Suggestions

Wrong: logging warnings to stdout during completion.

What happens: the shell treats warnings as completion candidates.

Fix: only suggestions and directives go to stdout; diagnostics go to stderr.

## Verification

From `~/go-exercises/completion-cli`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add status completion and rerun the same commands.

## Summary

- Completion generation and completion querying are separate operations.
- Dynamic completion should be fast, read-only, and deterministic.
- Sorted output makes shell behavior predictable.
- Tests should call the completion function directly and through the CLI path.

## What's Next

Next: [Building a Complete CLI Tool](../10-building-a-complete-cli-tool/10-building-a-complete-cli-tool.md).

## Resources

- [GNU Bash Programmable Completion](https://www.gnu.org/software/bash/manual/html_node/Programmable-Completion.html)
- [Zsh Completion System](https://zsh.sourceforge.io/Doc/Release/Completion-System.html)
- [Package sort](https://pkg.go.dev/sort)
