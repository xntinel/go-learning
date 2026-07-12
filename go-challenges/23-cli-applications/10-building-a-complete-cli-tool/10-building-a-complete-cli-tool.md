# 10. Building a Complete CLI Tool

A complete CLI is not a pile of commands; it is a set of contracts around parsing, validation, storage, output, errors, and tests. This capstone builds a small file-backed note tool with standard-library flags so every part can be compiled and verified offline. Because it uses `flag`, command-specific flags appear before positional arguments, such as `add -tags=work First`.

## Concepts

### Keep the Binary Thin

`main` should adapt process I/O and exit codes. Command dispatch, storage, and formatting should be ordinary functions with injectable paths and writers.

### Storage Needs Atomic Boundaries

Even a JSON file store should read, mutate, and write through explicit functions. That makes error paths testable and avoids spreading file format assumptions through commands.

### Output Format Is a Public API

Human table output and JSON output are both contracts. Test both so refactors do not break scripts or documentation.

### Errors Should Be Actionable and Classified

Use sentinel errors for missing commands, missing notes, invalid formats, and bad arguments. Wrap them with `%w` where context matters.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/23-cli-applications/10-building-a-complete-cli-tool/10-building-a-complete-cli-tool
cd go-solutions/23-cli-applications/10-building-a-complete-cli-tool/10-building-a-complete-cli-tool
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Build the CLI and Store

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
	"strconv"
	"strings"
	"text/tabwriter"
)

var (
	ErrMissingCommand = errors.New("missing command")
	ErrUnknownCommand = errors.New("unknown command")
	ErrMissingTitle   = errors.New("title is required")
	ErrNoteNotFound   = errors.New("note not found")
	ErrBadFormat      = errors.New("format must be table or json")
	ErrBadID          = errors.New("id must be a positive integer")
)

type note struct {
	ID    int      `json:"id"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
}

type store struct {
	Notes []note `json:"notes"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	root := flag.NewFlagSet("notectl", flag.ContinueOnError)
	root.SetOutput(stderr)
	path := root.String("store", "notes.json", "store path")
	format := root.String("format", "table", "output format: table or json")
	if err := root.Parse(args); err != nil {
		return err
	}
	remaining := root.Args()
	if len(remaining) == 0 {
		return ErrMissingCommand
	}
	switch remaining[0] {
	case "add":
		return add(*path, remaining[1:], stdout, stderr)
	case "list":
		return list(*path, *format, stdout)
	case "show":
		return show(*path, remaining[1:], stdout, stderr)
	case "delete":
		return deleteNote(*path, remaining[1:], stdout, stderr)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownCommand, remaining[0])
	}
}

func add(path string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("notectl add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tags := fs.String("tags", "", "comma-separated tags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return ErrMissingTitle
	}
	s, err := load(path)
	if err != nil {
		return err
	}
	n := note{ID: nextID(s.Notes), Title: fs.Arg(0), Tags: splitTags(*tags)}
	s.Notes = append(s.Notes, n)
	if err := save(path, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "added id=%d\n", n.ID)
	return nil
}

func list(path, format string, stdout io.Writer) error {
	s, err := load(path)
	if err != nil {
		return err
	}
	switch format {
	case "table":
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTITLE\tTAGS")
		for _, n := range s.Notes {
			fmt.Fprintf(w, "%d\t%s\t%s\n", n.ID, n.Title, strings.Join(n.Tags, ","))
		}
		return w.Flush()
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s.Notes)
	default:
		return fmt.Errorf("%w: %s", ErrBadFormat, format)
	}
}

func show(path string, args []string, stdout, stderr io.Writer) error {
	id, err := parseID(args)
	if err != nil {
		return err
	}
	s, err := load(path)
	if err != nil {
		return err
	}
	for _, n := range s.Notes {
		if n.ID == id {
			fmt.Fprintf(stdout, "%d: %s [%s]\n", n.ID, n.Title, strings.Join(n.Tags, ","))
			return nil
		}
	}
	return fmt.Errorf("%w: %d", ErrNoteNotFound, id)
}

func deleteNote(path string, args []string, stdout, stderr io.Writer) error {
	id, err := parseID(args)
	if err != nil {
		return err
	}
	s, err := load(path)
	if err != nil {
		return err
	}
	for i, n := range s.Notes {
		if n.ID == id {
			s.Notes = append(s.Notes[:i], s.Notes[i+1:]...)
			if err := save(path, s); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "deleted id=%d\n", id)
			return nil
		}
	}
	return fmt.Errorf("%w: %d", ErrNoteNotFound, id)
}

func parseID(args []string) (int, error) {
	if len(args) != 1 {
		return 0, ErrBadID
	}
	id, err := strconv.Atoi(args[0])
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w: %q", ErrBadID, args[0])
	}
	return id, nil
}

func load(path string) (store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store{}, nil
	}
	if err != nil {
		return store{}, err
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		return store{}, err
	}
	return s, nil
}

func save(path string, s store) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func nextID(notes []note) int {
	max := 0
	for _, n := range notes {
		if n.ID > max {
			max = n.ID
		}
	}
	return max + 1
}

func splitTags(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
```

### Exercise 2: Test a Full Workflow

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestWorkflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	var stdout, stderr bytes.Buffer

	if err := run([]string{"-store", path, "add", "-tags=work,urgent", "First"}, &stdout, &stderr); err != nil {
		t.Fatalf("add first: %v", err)
	}
	stdout.Reset()
	if err := run([]string{"-store", path, "add", "Second"}, &stdout, &stderr); err != nil {
		t.Fatalf("add second: %v", err)
	}
	stdout.Reset()
	if err := run([]string{"-store", path, "-format=json", "list"}, &stdout, &stderr); err != nil {
		t.Fatalf("list: %v", err)
	}
	var notes []note
	if err := json.Unmarshal(stdout.Bytes(), &notes); err != nil {
		t.Fatalf("json invalid: %v", err)
	}
	if len(notes) != 2 || notes[0].ID != 1 || notes[1].ID != 2 {
		t.Fatalf("notes = %+v", notes)
	}
	stdout.Reset()
	if err := run([]string{"-store", path, "delete", "1"}, &stdout, &stderr); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, want := stdout.String(), "deleted id=1\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "notes.json")
	tests := []struct {
		name string
		args []string
		want error
	}{
		{"missing", []string{"-store", path}, ErrMissingCommand},
		{"unknown", []string{"-store", path, "bad"}, ErrUnknownCommand},
		{"missing title", []string{"-store", path, "add"}, ErrMissingTitle},
		{"bad format", []string{"-store", path, "-format=yaml", "list"}, ErrBadFormat},
		{"bad id", []string{"-store", path, "show", "abc"}, ErrBadID},
		{"missing note", []string{"-store", path, "show", "1"}, ErrNoteNotFound},
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

func TestSplitTags(t *testing.T) {
	t.Parallel()

	got := splitTags(" work, ,urgent ")
	want := []string{"work", "urgent"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("splitTags() = %#v, want %#v", got, want)
	}
}

func Example_nextID() {
	fmt.Print(nextID([]note{{ID: 2}}))
	// Output: 3
}
```

### Exercise 3: Add Search

Add `search <query>` that matches case-insensitively against note titles, prints the same formats as `list`, and returns an empty list instead of an error when nothing matches.

## Common Mistakes

### Using the Current Directory in Tests

Wrong: tests write `notes.json` in the module root.

What happens: tests conflict with each other and leave artifacts.

Fix: use `t.TempDir()` and a `-store` flag.

### Letting Format Errors Happen Late

Wrong: loading and partially printing before rejecting `-format=yaml`.

What happens: callers receive partial output and an error.

Fix: validate format before writing command output.

### Parsing IDs Without Validation

Wrong: treating failed `Atoi` as ID zero.

What happens: invalid input can target the wrong record.

Fix: return a wrapped `ErrBadID` for parse failures and non-positive IDs.

## Verification

From `~/go-exercises/notectl`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add `search` and rerun the same commands.

## Summary

- A complete CLI needs parsing, validation, storage, formatting, and tests.
- `main` should be a thin adapter over testable functions.
- File-backed stores are enough to practice real error paths.
- End-to-end tests should cover realistic command workflows.

## What's Next

Next: [Functional Options](../../24-design-patterns-in-go/01-functional-options-deep-dive/01-functional-options.md).

## Resources

- [Package flag](https://pkg.go.dev/flag)
- [Package encoding/json](https://pkg.go.dev/encoding/json)
- [Package text/tabwriter](https://pkg.go.dev/text/tabwriter)
- [Package os: ReadFile and WriteFile](https://pkg.go.dev/os)
