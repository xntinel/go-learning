# Exercise 2: Hermetic Table Tests for a File Writer

Tests that write files are only trustworthy if they cannot see each other's output
or leak into the real filesystem. This exercise builds a rigorous, parallel table
test for the writer around `t.TempDir()`, and uses it to make a sharp point: the
same suite passes on both the buggy and the fixed writer, so the test is *not* what
catches the latent unchecked-error bug — the linter is.

This module is self-contained: its own `go mod init`, the writer inline, and a
thorough `*_test.go` with an `Example`.

## What you'll build

```text
hermetictests/                independent module: example.com/hermetictests
  go.mod                      go 1.24
  writer.go                   GreetFile, ErrEmptyName
  writer_test.go              parallel table test on t.TempDir + Example
  cmd/
    demo/
      main.go                 writes into a temp dir and prints the bytes
```

- Files: `writer.go`, `writer_test.go`, `cmd/demo/main.go`.
- Implement: the same `GreetFile` / `ErrEmptyName` contract as Exercise 1, kept here so the module stands alone.
- Test: a `t.Parallel()` table test using a per-case `t.TempDir()`; assert exact bytes on success and `ErrEmptyName` via `errors.Is` on failure; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...`

### Why t.TempDir, and why per-case

`t.TempDir()` returns a fresh directory unique to the calling test (or subtest) and
registers a cleanup that removes it when the test finishes. Calling it *inside* each
subtest — rather than once in the parent — gives every case its own isolated
directory, which is what makes `t.Parallel()` safe: two cases writing `out.txt`
cannot collide because they write into different temp roots. It also makes the suite
*hermetic*: nothing is written outside the per-test directory, so a test run leaves
the working tree untouched and cannot be perturbed by files left over from a
previous run.

The failure-path assertion is the part people get wrong. Comparing the returned
error against `ErrEmptyName` with `==` breaks the moment anyone wraps it; comparing
its `.Error()` string is brittle. The correct assertion is `errors.Is(err,
ErrEmptyName)`, which walks the `%w` chain and matches the sentinel wherever it sits.
The writer returns `ErrEmptyName` directly today, but writing the test against
`errors.Is` means it keeps passing if a future refactor wraps it with context.

### The subtle point this exercise makes

Run this suite against the buggy writer from Exercise 1 (the one that discards
`os.MkdirAll`) and it is green. Run it against the fixed writer and it is green. The
test exercises the success path on a writable temp dir where `os.MkdirAll` never
fails, and the error path on empty input that returns before any I/O. Neither path
can observe the discarded error. That is not a gap in the test — it is a fact about
what tests can prove. Tests verify behavior on the paths you drive; they cannot prove
the *absence* of an unchecked-error class across paths you did not drive. That
absence is the linter's job. Green tests here are precisely why the lint gate is not
redundant.

Create `writer.go`:

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

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/hermetictests"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "greet")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "greeting.txt")
	if err := writer.GreetFile(path, "Gopher"); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	fmt.Print(string(data))
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, Gopher!
```

### Tests

Create `writer_test.go`. The success and failure cases live in one table; each
subtest runs in parallel against its own `t.TempDir()`, and an `Example` documents
and verifies the exported API.

```go
package writer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGreetFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "plain", input: "Gopher", want: "Hello, Gopher!\n"},
		{name: "trims spaces", input: "  Go  ", want: "Hello, Go!\n"},
		{name: "trims tabs and newlines", input: "\tA\nB ", want: "Hello, AB!\n"},
		{name: "empty", input: "", wantErr: ErrEmptyName},
		{name: "only whitespace", input: " \t\n", wantErr: ErrEmptyName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "sub", "out.txt")
			err := GreetFile(path, tc.input)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("GreetFile(_, %q) err = %v, want %v", tc.input, err, tc.wantErr)
				}
				// Hermeticity: a rejected write must not create the file.
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatalf("GreetFile(_, %q) created a file on the error path", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("GreetFile(_, %q) unexpected err = %v", tc.input, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%q) err = %v", path, err)
			}
			if string(got) != tc.want {
				t.Fatalf("GreetFile(_, %q) wrote %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func ExampleGreetFile() {
	dir, _ := os.MkdirTemp("", "greet")
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "hi.txt")
	_ = GreetFile(path, "World")

	data, _ := os.ReadFile(path)
	os.Stdout.Write(data)
	// Output: Hello, World!
}
```

## Review

The suite is correct when each case is isolated by its own `t.TempDir()`, the
success path asserts exact bytes, and the failure path asserts the sentinel with
`errors.Is` *and* confirms no file was created. Running with `-race` and `-count=1`
(no cached results) proves the parallelism is genuinely safe and the temp-dir
isolation holds. The lesson to carry out of here is the one the green suite teaches
by omission: it passes identically on the buggy and fixed writer, so a passing test
run is not evidence that the code is free of unchecked errors — that evidence comes
from `golangci-lint`. Use tests to pin behavior and the linter to forbid whole bug
classes; neither replaces the other. The classic mistakes are asserting the error
with `==` (breaks under wrapping), sharing one `TempDir` across parallel subtests
(reintroduces collisions), and forgetting `-count=1` (a cached pass hides a
regression).

## Resources

- [`testing`: T.TempDir](https://pkg.go.dev/testing#T.TempDir) — per-test temp directories with automatic cleanup.
- [`testing`: T.Parallel](https://pkg.go.dev/testing#T.Parallel) — how parallel subtests interleave and why isolation matters.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is` against a sentinel.

---

Back to [01-errcheck-unchecked-io-error.md](01-errcheck-unchecked-io-error.md) | Next: [03-cli-error-surfacing.md](03-cli-error-surfacing.md)
