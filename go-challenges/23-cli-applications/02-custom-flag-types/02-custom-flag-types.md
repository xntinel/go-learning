# 2. Custom Flag Types

Built-in flags cover strings, numbers, booleans, and durations. Real tools often need constrained values, repeated values, or structured values; the `flag.Value` interface lets the parser call your code while still producing standard flag diagnostics.

## Concepts

### The `flag.Value` Contract

A custom flag type implements `String() string` and `Set(string) error`. `String` is used in diagnostics and defaults. `Set` is called every time the flag appears, so repeated flags can either replace or accumulate values.

### Validation Belongs in `Set`

If a value is syntactically invalid, reject it in `Set`. Return an error that wraps a sentinel with `%w`; the flag package will add context such as `invalid value "trace" for flag -level`, and your tests can still assert the underlying cause with `errors.Is`.

### Deterministic Output Matters

Maps are useful for key-value flags, but iteration order is not stable. Sort keys before formatting or printing so tests and user output do not flicker.

### Repeated Flags Are Different From CSV

`-tag=a,b` and `-tag=a -tag=b` are different interfaces. Repeated flags compose well with shell quoting and validation because each value has its own `Set` call.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/23-cli-applications/02-custom-flag-types/02-custom-flag-types
cd go-solutions/23-cli-applications/02-custom-flag-types/02-custom-flag-types
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Implement Custom Values

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
	ErrEmptyTag      = errors.New("tag must not be empty")
	ErrInvalidLevel  = errors.New("level must be debug, info, warn, or error")
	ErrInvalidHeader = errors.New("header must use key:value format")
)

type tagList []string

func (t *tagList) String() string {
	return strings.Join(*t, ",")
}

func (t *tagList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return ErrEmptyTag
	}
	*t = append(*t, value)
	return nil
}

type logLevel string

func (l *logLevel) String() string {
	return string(*l)
}

func (l *logLevel) Set(value string) error {
	switch value {
	case "debug", "info", "warn", "error":
		*l = logLevel(value)
		return nil
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidLevel, value)
	}
}

type headers map[string]string

func (h *headers) String() string {
	if h == nil || *h == nil {
		return ""
	}
	keys := make([]string, 0, len(*h))
	for key := range *h {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+":"+(*h)[key])
	}
	return strings.Join(parts, ",")
}

func (h *headers) Set(value string) error {
	key, val, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("%w: got %q", ErrInvalidHeader, value)
	}
	if *h == nil {
		*h = make(headers)
	}
	(*h)[strings.TrimSpace(key)] = strings.TrimSpace(val)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	var tags tagList
	level := logLevel("info")
	items := headers{}

	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var(&tags, "tag", "tag to attach; repeat for multiple tags")
	fs.Var(&level, "level", "log level: debug, info, warn, error")
	fs.Var(&items, "header", "header in key:value form; repeatable")
	if err := fs.Parse(args); err != nil {
		return classifyParseError(err)
	}

	fmt.Fprintf(stdout, "level=%s\n", level)
	fmt.Fprintf(stdout, "tags=%s\n", tags.String())
	fmt.Fprintf(stdout, "headers=%s\n", items.String())
	return nil
}

func classifyParseError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, ErrEmptyTag.Error()):
		return fmt.Errorf("parse tag: %w", ErrEmptyTag)
	case strings.Contains(msg, ErrInvalidLevel.Error()):
		return fmt.Errorf("parse level: %w", ErrInvalidLevel)
	case strings.Contains(msg, ErrInvalidHeader.Error()):
		return fmt.Errorf("parse header: %w", ErrInvalidHeader)
	default:
		return err
	}
}
```

### Exercise 2: Test Parsing and Validation

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestRunParsesCustomFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"defaults", nil, "level=info\ntags=\nheaders=\n"},
		{"repeated tags", []string{"-tag=blue", "-tag=green"}, "level=info\ntags=blue,green\nheaders=\n"},
		{"headers sorted", []string{"-header=B:two", "-header=A:one", "-level=debug"}, "level=debug\ntags=\nheaders=A:one,B:two\n"},
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

func TestRunRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want error
	}{
		{"empty tag", []string{"-tag="}, ErrEmptyTag},
		{"bad level", []string{"-level=trace"}, ErrInvalidLevel},
		{"bad header", []string{"-header=missing-separator"}, ErrInvalidHeader},
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
	_ = run([]string{"-tag=api", "-level=warn", "-header=Accept:json"}, &stdout, &stderr)
	fmt.Print(stdout.String())
	// Output:
	// level=warn
	// tags=api
	// headers=Accept:json
}
```

### Exercise 3: Add a New Custom Flag

Add a repeatable `-allow-cidr` flag that validates CIDR blocks with `net.ParseCIDR`, stores them in order, and returns a wrapped `ErrInvalidCIDR` on invalid input.

## Common Mistakes

### Returning Plain Text Errors

Wrong: `return fmt.Errorf("bad level")` from `Set`.

What happens: tests must match strings and callers cannot classify the failure.

Fix: declare `ErrInvalidLevel` and return `fmt.Errorf("%w: got %q", ErrInvalidLevel, value)`.

### Letting Map Order Leak Into Output

Wrong: ranging over a map directly when formatting headers.

What happens: the same command can print a different order across runs.

Fix: sort keys before building the string.

### Treating CSV and Repeated Flags as the Same

Wrong: accepting `-tag=a,b,c` while documenting `-tag` as repeatable.

What happens: users cannot pass a tag containing a comma and tests do not match the interface.

Fix: choose one interface and make `Set` implement it directly.

## Verification

From `~/go-exercises/custom-flags`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add the `-allow-cidr` flag and at least one failing test that asserts `errors.Is(err, ErrInvalidCIDR)`.

## Summary

- Custom flag values implement `String` and `Set`.
- `Set` is the right place for syntax validation.
- Sentinel errors keep flag validation testable.
- Deterministic output requires sorting maps.

## What's Next

Next: [Subcommands with FlagSet](../03-subcommands-with-flagset/03-subcommands-with-flagset.md).

## Resources

- [Package flag: Value](https://pkg.go.dev/flag#Value)
- [Package flag: Var](https://pkg.go.dev/flag#Var)
- [Package net: ParseCIDR](https://pkg.go.dev/net#ParseCIDR)
