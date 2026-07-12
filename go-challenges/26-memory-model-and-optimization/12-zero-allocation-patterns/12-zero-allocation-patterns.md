# 12. Zero-Allocation Patterns

Zero-allocation code is not magic; it is API design. The caller must own the storage, the package must avoid converting between strings and byte slices in the hot path, and tests must pin both correctness and allocation behavior. This lesson builds a small log parser that returns slices into the caller's input instead of allocating new strings for every field.

```text
zerolog/
  go.mod
  parser.go
  parser_test.go
  cmd/demo/main.go
```

The package parses lines in the form `timestamp|level|module|message`. It exposes a reusable `ParseInto` API for hot paths, a `ParseAll` API that appends to caller-provided storage, and a `Parse` convenience wrapper for less critical paths.

## Concepts

### Allocation Is Often An API Boundary Problem

Many allocations in parsers come from convenience APIs: `strings.Split`, `string(line)`, `fmt.Sprintf`, and returning freshly allocated slices. A zero-allocation parser usually accepts `[]byte`, writes into caller-provided structs or buffers, and returns views into the input. That is fast, but it also means the returned fields are only valid while the original input bytes remain unchanged.

### Reuse Slices, Do Not Hide Ownership

`ParseAll` accepts an existing `[]Entry`, resets it to length zero, and appends parsed entries into the existing capacity. This makes ownership explicit: the caller decides whether to allocate once, reuse across batches, or copy results for long-term storage. Hidden package-level buffers are avoided because they introduce data races and surprising lifetime bugs.

### Stack Scratch Beats Heap Scratch

The parser uses a fixed `[3]int` array to record delimiter positions. A fixed-size array with local scope is easy for the compiler to keep on the stack. A dynamically growing `[]int` would be more flexible, but it would be unnecessary for a format that always has exactly three separators.

### Errors Still Need A Stable Contract

Hot paths often focus on success-path allocations, but malformed input still needs testable errors. The package exposes sentinel validation errors and wraps them with `%w` when adding context. Tests assert `errors.Is`, not error strings, so callers can depend on the error contract without depending on wording.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/26-memory-model-and-optimization/12-zero-allocation-patterns/12-zero-allocation-patterns/cmd/demo
cd go-solutions/26-memory-model-and-optimization/12-zero-allocation-patterns/12-zero-allocation-patterns
```

This is a library package. The demo is only a consumer of the exported API; verification is done with `go test`.

### Exercise 1: Implement The Parser

Create `parser.go`:

```go
package zerolog

import (
	"bytes"
	"errors"
	"fmt"
)

var (
	ErrMalformed  = errors.New("log line must contain timestamp, level, module, and message")
	ErrEmptyField = errors.New("log line field must not be empty")
)

type Entry struct {
	Timestamp []byte
	Level     []byte
	Module    []byte
	Message   []byte
}

func ParseInto(dst *Entry, line []byte) error {
	if dst == nil {
		return fmt.Errorf("parse log line: %w", ErrMalformed)
	}

	var marks [3]int
	count := 0
	for i, b := range line {
		if b != '|' {
			continue
		}
		if count == len(marks) {
			return fmt.Errorf("parse log line: too many separators: %w", ErrMalformed)
		}
		marks[count] = i
		count++
	}
	if count != len(marks) {
		return fmt.Errorf("parse log line: got %d separators: %w", count, ErrMalformed)
	}

	timestamp := line[:marks[0]]
	level := line[marks[0]+1 : marks[1]]
	module := line[marks[1]+1 : marks[2]]
	message := line[marks[2]+1:]
	if len(timestamp) == 0 || len(level) == 0 || len(module) == 0 || len(message) == 0 {
		return fmt.Errorf("parse log line: %w", ErrEmptyField)
	}

	*dst = Entry{
		Timestamp: timestamp,
		Level:     level,
		Module:    module,
		Message:   message,
	}
	return nil
}

func Parse(line []byte) (Entry, error) {
	var entry Entry
	if err := ParseInto(&entry, line); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func ParseAll(data []byte, entries []Entry) ([]Entry, error) {
	entries = entries[:0]
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = data[:i]
			data = data[i+1:]
		} else {
			data = nil
		}
		if len(line) == 0 {
			continue
		}

		var entry Entry
		if err := ParseInto(&entry, line); err != nil {
			return entries, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func AppendSummary(dst []byte, entry Entry) []byte {
	dst = append(dst, entry.Level...)
	dst = append(dst, ' ')
	dst = append(dst, entry.Module...)
	dst = append(dst, ':', ' ')
	dst = append(dst, entry.Message...)
	return dst
}
```

The important choices are deliberate: `ParseInto` overwrites a caller-provided `Entry`, `Entry` fields are byte slices into the original line, `ParseAll` reuses caller-provided slice capacity, and `AppendSummary` appends into caller-owned storage.

### Exercise 2: Test Validation, Reuse, Examples, And Allocations

Create `parser_test.go`:

```go
package zerolog

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseIntoValidLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want Entry
	}{
		{
			name: "info",
			line: "2026-06-21T12:00:00Z|INFO|api|started",
			want: Entry{[]byte("2026-06-21T12:00:00Z"), []byte("INFO"), []byte("api"), []byte("started")},
		},
		{
			name: "debug",
			line: "2026-06-21T12:01:00Z|DEBUG|worker|job complete",
			want: Entry{[]byte("2026-06-21T12:01:00Z"), []byte("DEBUG"), []byte("worker"), []byte("job complete")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got Entry
			if err := ParseInto(&got, []byte(tc.line)); err != nil {
				t.Fatalf("ParseInto() error = %v", err)
			}
			if string(got.Timestamp) != string(tc.want.Timestamp) || string(got.Level) != string(tc.want.Level) || string(got.Module) != string(tc.want.Module) || string(got.Message) != string(tc.want.Message) {
				t.Fatalf("entry = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseIntoValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want error
	}{
		{name: "missing separators", line: "INFO only", want: ErrMalformed},
		{name: "too many separators", line: "a|b|c|d|e", want: ErrMalformed},
		{name: "empty field", line: "a||c|d", want: ErrEmptyField},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got Entry
			err := ParseInto(&got, []byte(tc.line))
			if !errors.Is(err, tc.want) {
				t.Fatalf("ParseInto() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseAllReusesCallerSlice(t *testing.T) {
	t.Parallel()

	data := []byte("t1|INFO|api|started\nt2|ERROR|db|failed\n")
	entries := make([]Entry, 0, 4)
	got, err := ParseAll(data, entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || cap(got) != cap(entries) {
		t.Fatalf("len/cap = %d/%d, want 2/%d", len(got), cap(got), cap(entries))
	}
	if string(got[1].Level) != "ERROR" || string(got[1].Module) != "db" {
		t.Fatalf("second entry = %+v", got[1])
	}
}

func TestAppendSummaryUsesCallerBuffer(t *testing.T) {
	t.Parallel()

	entry, err := Parse([]byte("t1|INFO|api|started"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 64)
	buf = AppendSummary(buf, entry)
	if string(buf) != "INFO api: started" {
		t.Fatalf("summary = %q", buf)
	}
}

func ExampleParseInto() {
	var entry Entry
	_ = ParseInto(&entry, []byte("2026-06-21T12:00:00Z|INFO|api|started"))
	fmt.Printf("%s %s\n", entry.Level, entry.Message)
	// Output: INFO started
}

func BenchmarkParseInto(b *testing.B) {
	line := []byte("2026-06-21T12:00:00Z|INFO|api|started")
	var entry Entry
	b.ReportAllocs()
	for b.Loop() {
		if err := ParseInto(&entry, line); err != nil {
			b.Fatal(err)
		}
	}
}
```

The benchmark reports allocations for the success path without making the unit tests depend on benchmark timing. Correctness and validation behavior still matter more than a single allocation number.

### Exercise 3: Add A Demo That Uses Only Exported API

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"zerolog"
)

func main() {
	entry, err := zerolog.Parse([]byte("2026-06-21T12:00:00Z|INFO|api|started"))
	if err != nil {
		log.Fatal(err)
	}

	buf := make([]byte, 0, 64)
	buf = zerolog.AppendSummary(buf, entry)
	fmt.Println(string(buf))
}
```

The demo imports `zerolog` and uses only exported identifiers. It does not inspect package internals.

## Common Mistakes

### Returning Strings From The Hot Parser

Wrong: parse `[]byte`, convert every field with `string(field)`, and return a struct of strings. That allocates when the conversion must preserve data after the input changes.

Fix: return byte slices into the input for the hot path. Convert to strings only at the boundary that actually needs strings.

### Reusing Hidden Global Buffers

Wrong: store scratch buffers in package-level variables to avoid per-call allocation. Concurrent callers race on the same buffer.

Fix: keep scratch state local or caller-owned. This lesson uses a local `[3]int` and caller-provided slices.

### Matching Error Strings

Wrong: test `err.Error() == "parse failed"`. Adding context breaks the test even when the semantic error is the same.

Fix: expose sentinel errors, wrap with `%w`, and assert with `errors.Is`.

## Verification

Run this from `~/go-exercises/zerolog`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Then add one more table row to `TestParseIntoValidationErrors` for `"a|b|c"` and prove it reports `ErrMalformed`.

## Summary

- Zero-allocation parsing depends on explicit ownership: caller-provided input, output structs, and append buffers.
- Returning byte-slice views avoids field copies but ties the result lifetime to the input lifetime.
- Fixed local arrays are useful scratch space for formats with fixed structure.
- Sentinel validation errors should be wrapped with `%w` and tested with `errors.Is`.
- Allocation tests are useful only after correctness tests already pin behavior.

## What's Next

Next: [Performance Regression Testing](../13-performance-regression-testing/13-performance-regression-testing.md).

## Resources

- [Go FAQ: stack or heap allocation](https://go.dev/doc/faq#stack_or_heap)
- [bytes package](https://pkg.go.dev/bytes)
- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun)
- [Go Diagnostics](https://go.dev/doc/diagnostics)
