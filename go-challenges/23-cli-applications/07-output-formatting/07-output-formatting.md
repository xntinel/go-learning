# 7. Output Formatting

CLI output has two audiences: humans reading a terminal and programs consuming stdout. A good `--format` flag makes that contract explicit and keeps every format covered by tests.

## Concepts

### Tables Are for Humans

`text/tabwriter` aligns tab-terminated columns and buffers internally, so `Flush` is required. Tables should stay on stdout only for successful data output.

### JSON Is for Scripts

`json.NewEncoder` streams output to an `io.Writer`. `SetIndent` makes arrays readable; JSON Lines can encode one object per line for pipeline processing.

### Format Validation

Reject unknown formats before printing partial output. A sentinel error lets tests distinguish unsupported formats from writer failures.

### Stable Schemas

Use JSON tags so field names stay lowercase and predictable even if Go field names change.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/output-formatting
cd ~/go-exercises/output-formatting
go mod init example.com/outputformatting
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Implement Multiple Formats

Create `main.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

var ErrUnknownFormat = errors.New("unknown output format")

type task struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "table", "output format: table, json, jsonl")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return printTasks(stdout, sampleTasks(), *format)
}

func sampleTasks() []task {
	return []task{{1, "Buy groceries", "todo", 2}, {2, "Write docs", "done", 1}}
}

func printTasks(w io.Writer, tasks []task, format string) error {
	switch format {
	case "table":
		return printTable(w, tasks)
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(tasks)
	case "jsonl":
		enc := json.NewEncoder(w)
		for _, task := range tasks {
			if err := enc.Encode(task); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrUnknownFormat, format)
	}
}

func printTable(w io.Writer, tasks []task) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tPRIORITY")
	for _, task := range tasks {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\n", task.ID, task.Name, task.Status, task.Priority)
	}
	return tw.Flush()
}
```

### Exercise 2: Test Every Format

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPrintTasks(t *testing.T) {
	t.Parallel()

	tasks := []task{{1, "A", "todo", 3}}
	tests := []struct {
		name   string
		format string
		want   string
	}{
		{"table", "table", "ID  NAME  STATUS  PRIORITY\n1   A     todo    3\n"},
		{"json", "json", "[\n  {\n    \"id\": 1,\n    \"name\": \"A\",\n    \"status\": \"todo\",\n    \"priority\": 3\n  }\n]\n"},
		{"jsonl", "jsonl", "{\"id\":1,\"name\":\"A\",\"status\":\"todo\",\"priority\":3}\n"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if err := printTasks(&out, tasks, tc.format); err != nil {
				t.Fatalf("printTasks() error = %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrintTasksRejectsUnknownFormat(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := printTasks(&out, sampleTasks(), "yaml")
	if !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("err = %v, want ErrUnknownFormat", err)
	}
}

func TestRunJSONIsValid(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if err := run([]string{"-format=json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var decoded []task
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("json invalid: %v", err)
	}
	if len(decoded) != 2 || !strings.EqualFold(decoded[0].Name, "buy groceries") {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func Example_printTasks() {
	var out bytes.Buffer
	_ = printTasks(&out, []task{{1, "A", "todo", 3}}, "jsonl")
	fmt.Print(out.String())
	// Output: {"id":1,"name":"A","status":"todo","priority":3}
}
```

### Exercise 3: Add CSV

Add `-format=csv` using `encoding/csv`, include a header row, and test proper quoting for a task name containing a comma.

## Common Mistakes

### Forgetting `Flush`

Wrong: writing to `tabwriter` without calling `Flush`.

What happens: output can be empty or incomplete.

Fix: return `tw.Flush()` and test table output.

### Printing Before Format Validation

Wrong: writing a header, then discovering the format is invalid.

What happens: callers receive mixed partial data and an error.

Fix: switch on format before writing.

### Missing JSON Tags

Wrong: relying on exported Go names in JSON.

What happens: scripts consume `Priority` instead of `priority`.

Fix: add stable `json` struct tags.

## Verification

From `~/go-exercises/output-formatting`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add CSV support and rerun the same commands.

## Summary

- Tables are human-readable; JSON and JSON Lines are script-readable.
- `tabwriter` must be flushed.
- Validate output formats before writing.
- Tests should pin every supported format.

## What's Next

Next: [Config Loading](../08-config-loading/08-config-loading.md).

## Resources

- [Package text/tabwriter](https://pkg.go.dev/text/tabwriter)
- [Package encoding/json](https://pkg.go.dev/encoding/json)
- [Package encoding/csv](https://pkg.go.dev/encoding/csv)
