# Exercise 5: Log Processing — `strings.Lines` and `FieldsSeq` into a Filtered `iter.Seq`

An observability pre-filter reads a log blob, keeps only the ERROR and WARN lines,
and hands them downstream — and it must not buffer the whole file to do it. This
exercise builds that stage with Go 1.24's string iterators: `strings.Lines` to
stream lines lazily and `strings.FieldsSeq` to tokenize each one, filtered and
capped with a `Take`, so parsing stops the instant the consumer has what it wants.

## What you'll build

```text
logproc/                  independent module: example.com/logproc
  go.mod                  module example.com/logproc
  logproc.go              LogEntry, Parse, Errors, Take
  cmd/
    demo/
      main.go             runnable demo: filter a log blob to ERROR/WARN
  logproc_test.go         parse+filter, trailing-newline, lazy-Take, malformed tests
```

Files: `logproc.go`, `cmd/demo/main.go`, `logproc_test.go`.
Implement: `Parse(lines iter.Seq[string]) iter.Seq[LogEntry]`, `Errors(blob string)` over `strings.Lines`, and a generic `Take`.
Test: filtered entries equal the expected slice; the trailing unterminated line is still yielded; `Take(2)` stops parsing early; a malformed line is skipped without panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/05-log-lines-seq-pipeline/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/05-log-lines-seq-pipeline
```

## The design

The pipeline has two layers. `Parse(lines iter.Seq[string])` is the reusable core:
it ranges a stream of raw lines, trims the trailing newline, tokenizes with
`strings.FieldsSeq`, keeps lines whose level is ERROR or WARN, and yields a
`LogEntry`. `Errors(blob string)` is the convenience entry point that feeds
`Parse` from `strings.Lines(blob)`. Splitting them this way lets the tests drive
`Parse` from an instrumented line source to prove laziness, while production code
calls `Errors`.

`strings.Lines` is the right primitive for an in-memory blob because it streams:
it yields each line *including* its terminating `\n`, and yields a final
unterminated line as-is, without ever building a `[]string`. Contrast
`slices.Collect(strings.SplitSeq(blob, "\n"))`, which materializes every line at
once — fine for a small config, wrong for a large log. Trimming the `\n` after
each yield is deliberate: it preserves the "last line without a newline is still a
line" semantics that a naive `Split` on `\n` would mangle into a trailing empty
element.

Tokenizing with `strings.FieldsSeq` splits on runs of whitespace and yields each
field lazily. Here the format is `TIMESTAMP LEVEL MESSAGE...`; `parseLine` collects
the fields, requires at least three, reads the level from field two, and rejoins
the rest as the message. A line with fewer than three fields is malformed and is
skipped — never a panic. Because `Parse` yields lazily and checks `yield`'s
return, wrapping it in `Take(2, ...)` stops the whole pipeline — including the
per-line parsing — after two matches.

Create `logproc.go`:

```go
package logproc

import (
	"iter"
	"slices"
	"strings"
)

// LogEntry is a parsed, filtered log line.
type LogEntry struct {
	Level   string
	Message string
}

// parseLine tokenizes "TIMESTAMP LEVEL MESSAGE..." into a LogEntry. It reports
// false for a line with fewer than three whitespace-separated fields.
func parseLine(line string) (LogEntry, bool) {
	fields := slices.Collect(strings.FieldsSeq(line))
	if len(fields) < 3 {
		return LogEntry{}, false
	}
	return LogEntry{Level: fields[1], Message: strings.Join(fields[2:], " ")}, true
}

// Parse streams raw log lines into filtered LogEntries, keeping only ERROR and
// WARN. It is lazy: a consumer that stops halts parsing immediately.
func Parse(lines iter.Seq[string]) iter.Seq[LogEntry] {
	return func(yield func(LogEntry) bool) {
		for line := range lines {
			line = strings.TrimRight(line, "\n")
			if line == "" {
				continue
			}
			entry, ok := parseLine(line)
			if !ok {
				continue
			}
			if entry.Level != "ERROR" && entry.Level != "WARN" {
				continue
			}
			if !yield(entry) {
				return
			}
		}
	}
}

// Errors filters an in-memory log blob, streaming lines with strings.Lines.
func Errors(blob string) iter.Seq[LogEntry] {
	return Parse(strings.Lines(blob))
}

// Take yields at most n values from src, then stops it.
func Take[T any](n int, src iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		remaining := n
		src(func(v T) bool {
			if remaining <= 0 {
				return false
			}
			remaining--
			return yield(v)
		})
	}
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logproc"
)

func main() {
	blob := "2026-07-02T10:00:00Z INFO service started\n" +
		"2026-07-02T10:00:01Z WARN cache miss ratio high\n" +
		"2026-07-02T10:00:02Z ERROR upstream timeout\n" +
		"2026-07-02T10:00:03Z INFO request served"

	for e := range logproc.Errors(blob) {
		fmt.Printf("%s: %s\n", e.Level, e.Message)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
WARN: cache miss ratio high
ERROR: upstream timeout
```

## Tests

Create `logproc_test.go`:

```go
package logproc

import (
	"reflect"
	"strings"
	"testing"
)

func TestFilterKeepsErrorAndWarn(t *testing.T) {
	t.Parallel()

	blob := "2026-01-01T00:00:00Z INFO up\n" +
		"2026-01-01T00:00:01Z WARN slow query\n" +
		"2026-01-01T00:00:02Z ERROR disk full\n" +
		"2026-01-01T00:00:03Z DEBUG trace\n"

	var got []LogEntry
	for e := range Errors(blob) {
		got = append(got, e)
	}

	want := []LogEntry{
		{Level: "WARN", Message: "slow query"},
		{Level: "ERROR", Message: "disk full"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTrailingLineWithoutNewlineYielded(t *testing.T) {
	t.Parallel()

	blob := "2026-01-01T00:00:00Z INFO up\n" +
		"2026-01-01T00:00:01Z ERROR no newline here"

	var got []LogEntry
	for e := range Errors(blob) {
		got = append(got, e)
	}

	if len(got) != 1 || got[0].Level != "ERROR" || got[0].Message != "no newline here" {
		t.Fatalf("got %v, want the final unterminated ERROR line", got)
	}
}

func TestTakeStopsParsingEarly(t *testing.T) {
	t.Parallel()

	blob := "t WARN one\nt ERROR two\nt ERROR three\nt ERROR four\nt ERROR five\n"

	var pulled int
	lines := func(yield func(string) bool) {
		for line := range strings.Lines(blob) {
			pulled++
			if !yield(line) {
				return
			}
		}
	}

	var got []LogEntry
	for e := range Take(2, Parse(lines)) {
		got = append(got, e)
	}

	if len(got) != 2 {
		t.Fatalf("yielded %d entries, want 2", len(got))
	}
	// Take needs one extra pull to learn it is full, so it stops after the
	// third line here rather than parsing all five. The point is early stop,
	// not a million lines parsed.
	if pulled > 3 {
		t.Fatalf("pulled %d lines, want <= 3 (Take must stop parsing early)", pulled)
	}
}

func TestMalformedLineSkipped(t *testing.T) {
	t.Parallel()

	blob := "garbage\n2026-01-01T00:00:00Z ERROR real one\n\n"

	var got []LogEntry
	for e := range Errors(blob) {
		got = append(got, e)
	}

	if len(got) != 1 || got[0].Message != "real one" {
		t.Fatalf("got %v, want a single ERROR entry (malformed skipped)", got)
	}
}
```

## Review

The stage is correct when it yields exactly the ERROR and WARN lines in order,
preserves the final unterminated line (the `strings.Lines` semantics the
trailing-newline test pins), and skips malformed input without panicking. The
laziness proof is the `Take(2)` test: an instrumented line source shows that
parsing stopped after the second match rather than tokenizing the whole blob —
which only holds because `Parse` yields lazily and honors `yield`'s `false`. For
an `io.Reader` source with a size cap, swap `strings.Lines` for a
`bufio.Scanner`; `Lines` is the in-memory choice and never buffers the whole
result.

## Resources

- [`strings.Lines`](https://pkg.go.dev/strings#Lines)
- [`strings.FieldsSeq`](https://pkg.go.dev/strings#FieldsSeq)
- [`iter` package documentation](https://pkg.go.dev/iter)
- [Go 1.24 release notes](https://go.dev/doc/go1.24)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-pull-merge-sorted-streams.md](04-pull-merge-sorted-streams.md) | Next: [06-batching-iterator-bulk-writer.md](06-batching-iterator-bulk-writer.md)
