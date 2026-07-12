# 11. stdin/stdout Piping

Build a filter package that reads lines from an `io.Reader`, transforms them, and writes to an `io.Writer`. This is the same design used by command-line tools that work in shell pipelines: the library is testable, and `cmd/demo` only wires it to standard streams.

## Concepts

### Libraries Should Not Own os.Stdin

Accept `io.Reader` and `io.Writer` in the core function. That lets tests use strings and buffers while the command uses `os.Stdin` and `os.Stdout`.

### Scanner Has A Token Limit

`bufio.Scanner` is convenient for line-oriented input. Its default token limit is finite; increase the buffer if your tool accepts long lines.

### Exit Policy Belongs In main

The library returns errors. The command decides whether to log and exit. This keeps the package reusable.

## Exercises

### Exercise 1: Implement The Filter

Create `filter.go`:

```go
package linefilter

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type Options struct {
	Prefix string
	Upper  bool
}

func Run(in io.Reader, out io.Writer, opts Options) error {
	if in == nil {
		return fmt.Errorf("run filter: %w", ErrNilReader)
	}
	if out == nil {
		return fmt.Errorf("run filter: %w", ErrNilWriter)
	}
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		if opts.Upper {
			line = strings.ToUpper(line)
		}
		if _, err := fmt.Fprintln(out, opts.Prefix+line); err != nil {
			return fmt.Errorf("write line: %w", err)
		}
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("scan input: %w", err)
	}
	return nil
}
```

Create `errors.go`:

```go
package linefilter

import "errors"

var (
	ErrNilReader = errors.New("reader must not be nil")
	ErrNilWriter = errors.New("writer must not be nil")
)
```

### Exercise 2: Test The Filter

Create `filter_test.go`:

```go
package linefilter

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRunTransformsLines(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := Run(strings.NewReader("a\nb\n"), &out, Options{Prefix: "> ", Upper: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "> A\n> B\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestRunValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil reader", err: Run(nil, &bytes.Buffer{}, Options{}), want: ErrNilReader},
		{name: "nil writer", err: Run(strings.NewReader("x"), nil, Options{}), want: ErrNilWriter},
	}
	for _, tt := range tests {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, tt.err, tt.want)
		}
	}
}

func ExampleRun() {
	var out bytes.Buffer
	_ = Run(strings.NewReader("go\n"), &out, Options{Upper: true})
	fmt.Print(out.String())
	// Output:
	// GO
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"os"

	"example.com/linefilter"
)

func main() {
	if err := linefilter.Run(os.Stdin, os.Stdout, linefilter.Options{Prefix: "| "}); err != nil {
		log.Fatal(err)
	}
}
```

## Common Mistakes

### Testing Through os.Stdin

Wrong: mutate process-wide `os.Stdin` in tests.

Fix: test the library with `strings.Reader` and `bytes.Buffer`.

### Calling log.Fatal In The Library

Wrong: make `Run` exit the process on scan errors.

Fix: return an error and let `cmd/demo` decide how to handle it.

### Ignoring Writer Failures

Wrong: call `fmt.Fprintln(out, line)` and ignore the returned error.

Fix: return a wrapped write error.

## Verification

Run this from `~/go-exercises/linefilter`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
printf 'a\nb\n' | go run ./cmd/demo
```

Add one more test for a lowercase input with `Upper: false`.

## Summary

- Keep pipeline logic in a library that accepts `io.Reader` and `io.Writer`.
- Use `cmd/demo` to connect the library to `os.Stdin` and `os.Stdout`.
- Check scanner and writer errors.
- Increase the scanner buffer when long lines are valid input.

## What's Next

Next: [Archive Formats -- tar and zip](../12-archive-tar-zip/12-archive-tar-zip.md).

## Resources

- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner)
- [io.Reader](https://pkg.go.dev/io#Reader)
- [os.Stdin](https://pkg.go.dev/os#pkg-variables)
