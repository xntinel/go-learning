# Exercise 1: Catch an Unchecked I/O Error with errcheck

A file writer that discards the error from `os.MkdirAll` compiles cleanly, passes
`go vet`, and passes its tests — and still hides a production outage. This exercise
builds that writer, runs `golangci-lint`, reads the `errcheck` finding, and fixes it
by wrapping the error with `%w`.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and test. Nothing here imports any other exercise.

## What you'll build

```text
writerlib/                    independent module: example.com/writerlib
  go.mod                      go 1.24
  internal/
    writer/
      writer.go               GreetFile, ErrEmptyName (checks os.MkdirAll)
      writer_test.go          success + sentinel-error table test
  cmd/
    demo/
      main.go                 writes a greeting to a temp dir and prints it
  .golangci.yml               version "2", errcheck enabled (shown in prose)
```

- Files: `internal/writer/writer.go`, `internal/writer/writer_test.go`, `cmd/demo/main.go`.
- Implement: `GreetFile(path, name string) error` that ensures the parent directory exists (checking `os.MkdirAll`), creates the file, and writes `Hello, <name>!\n`; the `ErrEmptyName` sentinel for empty input.
- Test: the success path writes the exact bytes; the empty-name path returns `ErrEmptyName` via `errors.Is`.
- Verify: `go test -count=1 -race ./...`, then `golangci-lint run ./...`.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/01-errcheck-unchecked-io-error/cmd/demo go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/01-errcheck-unchecked-io-error/internal/writer
cd go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/01-errcheck-unchecked-io-error
```

### The bug: a discarded os.MkdirAll error

The writer ensures the destination's parent directory exists before creating the
file. The naive version discards the `os.MkdirAll` return:

```go
func GreetFile(path, name string) error {
	trimmed := trim(name)
	if trimmed == "" {
		return ErrEmptyName
	}

	os.MkdirAll(filepath.Dir(path), 0o755) // discarded error — the bug

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
```

On a writable disk this is fine, which is exactly why it is dangerous. When the
target lives on a read-only filesystem or under a directory the process cannot
write, `os.MkdirAll` fails and returns a precise error — "permission denied",
"read-only file system" — and the code throws it away. The next line, `os.Create`,
then fails too, but with a *less* informative error, and the true cause is gone. The
discarded error is the difference between a one-line diagnosis and an hour of
guessing. `errcheck` exists to make this impossible to merge.

Note the two errors that are *not* the bug. `fmt.Fprintf`'s error is checked, and
`f.Close()` is deliberately ignored with a blank assignment inside the deferred
closure — `errcheck` does not flag `_ = f.Close()` because ignoring it is explicit.
Only the bare `os.MkdirAll` call is unchecked, so the linter reports exactly one
finding.

### The fix: check and wrap with %w

Create the corrected package. `os.MkdirAll` is now checked and its error wrapped
with `%w`, so a caller can still `errors.Is`/`errors.As` down to the underlying
`os.PathError`:

Create `internal/writer/writer.go`:

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
// parent directory if needed. Every I/O error is checked and wrapped with %w
// so callers can inspect the cause with errors.Is / errors.As.
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

// trim removes ASCII spaces, tabs, and newlines from s. Kept private so it can
// be replaced without breaking callers.
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

### Running the linter

Install a pinned version (v2), then run it on the module:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6
golangci-lint run ./...
```

On the *buggy* version the run reports exactly one finding. The exact
`line:col` depends on how you lay out the buggy file, so it is elided here; the
message and the offending source line are what matter:

```text
internal/writer/writer.go: Error return value of `os.MkdirAll` is not checked (errcheck)
	os.MkdirAll(filepath.Dir(path), 0o755) // discarded error — the bug
```

After the fix above, the run is silent and exits 0. `errcheck` is in v2's
`standard` set and is also enabled explicitly by the config below, so it runs
regardless of which default you choose.

Create `.golangci.yml` (shown here; it is validated in Exercise 4):

```yaml
version: "2"
linters:
  default: none
  enable:
    - errcheck
    - govet
run:
  timeout: 5m
```

### The runnable demo

The demo writes a greeting into a throwaway temp directory, reads it back, and
prints the contents, so the output is deterministic and leaves nothing behind.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/writerlib/internal/writer"
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

	path := filepath.Join(dir, "nested", "greeting.txt")
	if err := writer.GreetFile(path, "World"); err != nil {
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
Hello, World!
```

### Tests

The tests assert the two behaviors that define correctness: the success path writes
the exact bytes into a nested path (which forces `os.MkdirAll` to actually create a
directory), and the empty-name path returns the `ErrEmptyName` sentinel matched with
`errors.Is`. They pass on both the buggy and the fixed writer — which is the whole
point of the lesson: the latent `os.MkdirAll` bug is invisible to a test running on
a writable temp dir, and only the linter catches it.

Create `internal/writer/writer_test.go`:

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
		{name: "empty", input: "", wantErr: ErrEmptyName},
		{name: "only whitespace", input: " \t\n", wantErr: ErrEmptyName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A nested path forces os.MkdirAll to create a directory.
			path := filepath.Join(t.TempDir(), "sub", "out.txt")
			err := GreetFile(path, tc.input)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("GreetFile(_, %q) err = %v, want %v", tc.input, err, tc.wantErr)
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
```

## Review

The writer is correct when every I/O error is either checked and wrapped with `%w`
or explicitly ignored with a blank assignment — there is no third state where an
error silently vanishes. The `errcheck` finding on the naive version is the concrete
proof that a discarded `os.MkdirAll` is a real omission, not a stylistic preference:
the tests are green on that version, so without the linter the bug ships. The fix
preserves the cause chain, so a caller debugging a read-only-filesystem failure sees
`mkdir /ro/sub: mkdir /ro/sub: read-only file system` rather than a bare create
error. Wrapping with `%w` (not `%v`) is what keeps `errors.Is`/`errors.As` working
down the chain; a common mistake is to log-and-swallow or to wrap with `%v` and
break unwrapping. Run `go test -race` and then `golangci-lint run ./...` to see the
two gates disagree on the buggy version and agree on the fixed one.

## Resources

- [errcheck](https://github.com/kisielk/errcheck) — the analyzer that flags unchecked error returns.
- [golangci-lint: errcheck settings](https://golangci-lint.run/docs/linters/configuration/) — `check-blank`, `check-type-assertions`, and exclusions.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w`, `errors.Is`, `errors.As`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-hermetic-table-tests-for-io.md](02-hermetic-table-tests-for-io.md)
