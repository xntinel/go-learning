# Exercise 19: A Zero-Allocation NDJSON Line Splitter as an iter.Seq

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Newline-delimited JSON (NDJSON) is how a lot of backend systems stream
records without buffering a whole array in memory: Kafka Connect sinks,
`kubectl logs`, Elasticsearch bulk APIs, and countless internal event
pipelines all speak it. Splitting it the easy way -- `bytes.Split(data,
[]byte("\n"))` -- builds a brand-new `[][]byte` and copies every line
pointer into it up front, which is wasted work when a consumer is going to
range over the lines once and stop early half the time anyway (a health
check that only needs the first matching record, a `tail -f`-style reader
watching for one event type). This exercise builds the alternative: a
range-over-func iterator that walks the input with `bytes.IndexByte` and
hands out each line as a sub-slice, allocating nothing, and stopping
scanning the moment its caller stops consuming.

This exercise builds `ndjson`, a command-line tool that reads a whole NDJSON
stream from stdin and prints each line prefixed with its 1-based line
number, with an optional `-limit` flag that stops after a fixed number of
lines -- the CLI's own demonstration that a range loop over an `iter.Seq`
can bail out of a scan early without the splitter having done any wasted
work on the rest of the input.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ndjson/                         module example.com/ndjson
  go.mod                        go 1.24
  ndjson.go                     package main — Lines(data []byte) iter.Seq[[]byte]
  ndjson_test.go                package main — table over trailing/no-trailing newline, embedded/CRLF, empty input; view proof; early-break proof; AllocsPerRun == 0; run() end to end
  main.go                       package main — -limit flag, whole-stdin read, exit codes
```

- Files: `ndjson.go`, `ndjson_test.go`, `main.go`.
- Implement: `Lines(data []byte) iter.Seq[[]byte]`, whose returned function repeatedly finds the next `'\n'` with `bytes.IndexByte`, slices `data[:i]` as the line and `data[i+1:]` as the remainder, trims a trailing `'\r'` for CRLF input, `yield`s the line, and returns immediately if `yield` reports `false`.
- Tool: `ndjson` reads the whole of stdin (`io.ReadAll`, since `Lines` operates over an in-memory `[]byte`) and prints one `N: line` line per input line. `-limit` stops after that many lines without scanning the rest of the input, demonstrated by `break`ing out of the range loop early. Exit 0 on success, exit 2 for a bad flag or a negative `-limit`, exit 1 for a stdin read failure.
- Test: a table over a trailing newline, no trailing newline, embedded empty lines, CRLF line endings, and empty input; a test that a yielded line is a live view into `data` (mutating `data` after capturing a line is visible through it); a test that breaking out of the consuming range loop partway through stops the sequence exactly where expected; a `testing.AllocsPerRun` test proving a full range over `Lines` allocates zero times; `run` end to end over a `strings.Reader` and a `bytes.Buffer`, including the `-limit` flag and the usage-error paths.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/19-ndjson-splitter-no-alloc-window
cd go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/19-ndjson-splitter-no-alloc-window
go mod edit -go=1.24
```

### Why an iter.Seq over sub-slices, instead of building a [][]byte

`Lines` returns a value of type `iter.Seq[[]byte]` -- a function that takes
a `yield` callback and calls it once per element, stopping early if `yield`
returns `false`. Since Go 1.23, `for line := range Lines(data) { ... }`
compiles directly against that shape: no intermediate collection is ever
built. Inside the closure, the loop body is exactly the two-index
sub-slicing this lesson has practiced throughout: `bytes.IndexByte(data,
'\n')` finds the next separator, `data[:i]` is the line, `data[i+1:]`
becomes the new `data` for the next iteration. Every yielded line aliases
the original input; nothing is copied, and nothing is allocated beyond
whatever `data` itself already was. This is the same producer discipline as
`bufio.Scanner.Bytes()` -- a genuinely zero-copy view, valid for as long as
the caller does not mutate or discard `data` out from under it, and the
caller's responsibility to `bytes.Clone` a line the moment it needs to keep
one past the loop.

The early-termination behavior is not incidental; it is the other half of
why this is worth doing as an iterator instead of a slice-returning
function. If a consumer's `range` loop `break`s after the first matching
line, `yield` returns `false` on the next call, and `Lines` returns from its
closure immediately -- the rest of `data`, however large, is never scanned.
A function that returns `[][]byte` cannot offer that: it has already paid
the full `O(n)` scan cost before the caller sees a single line.
`TestLinesEarlyBreak` is built to catch a version of `Lines` that ignores
`yield`'s return value and keeps scanning regardless -- a mistake that would
still produce correct *output* for a full range but would defeat the entire
point of the design for a caller that stops early. The `-limit` flag on the
CLI itself exercises exactly this path: with a large enough input and a
small `-limit`, the tool visibly does less work than an implementation that
collected every line into a slice before printing any of them.

The zero-allocation claim needs the same discipline this lesson has applied
elsewhere: it is proven with `testing.AllocsPerRun`, not asserted from "well,
nothing here calls `make` or `append`". `bytes.IndexByte` does not allocate,
slicing does not allocate, and as of the current Go toolchain a
directly-ranged `for range Lines(data)` compiles without heap-escaping the
iterator's closure when the whole loop is visible to the compiler in one
function -- `TestLinesAllocsZero` measures exactly that shape.

Create `ndjson.go`:

```go
// Package main implements ndjson, a line splitter for newline-delimited
// JSON: it walks stdin with bytes.IndexByte and yields each line as a
// sub-slice view, allocating nothing, and stops scanning the moment its
// caller stops consuming. This file holds the splitting logic; main.go
// wires it to flags and stdio.
package main

import (
	"bytes"
	"iter"
)

// Lines returns an iter.Seq[[]byte] that yields each line of data, split on
// '\n', with a single trailing '\r' trimmed from each line so CRLF input is
// handled without a separate pass. Every yielded line is a sub-slice view
// into data -- Lines never copies and never allocates on its own. A final
// line with no trailing newline is yielded like any other line. The
// sequence honors early termination: if the consuming range loop's body
// returns false to yield (which a plain "for line := range Lines(data) {
// ... break ... }" does automatically), scanning stops immediately and the
// rest of data is never touched.
func Lines(data []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for len(data) > 0 {
			var line []byte
			if i := bytes.IndexByte(data, '\n'); i < 0 {
				line = data
				data = nil
			} else {
				line = data[:i]
				data = data[i+1:]
			}
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			if !yield(line) {
				return
			}
		}
	}
}
```

### The tool

`ndjson` needs the whole of stdin in memory before it can call `Lines`,
since `Lines` operates over an already-materialized `[]byte` rather than a
stream -- so `run` reads it all up front with `io.ReadAll` rather than
attempting to scan line by line off the wire. That one read is the tool's
only allocation of consequence; everything downstream of it, the actual
line splitting, is the zero-allocation path this module is about. `run`
takes the argument slice, an `io.Reader` for stdin, and an `io.Writer` for
stdout, so it can be driven from a test with a `strings.Reader` and a
`bytes.Buffer` without touching `os.Args` or a real terminal. A bad flag or
a negative `-limit` is a usage mistake the caller fixes by changing the
command line, so both wrap the `errUsage` sentinel and `main` maps that to
exit code 2; a stdin read failure maps to exit code 1. The `-limit` flag's
early `break` out of the `for line := range Lines(data)` loop is not just a
CLI convenience -- it is the visible proof, from outside the package, that
the range-over-func iterator genuinely stops work instead of merely
stopping output.

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
)

// errUsage marks a failure fixable by changing the command line: a bad flag
// or a negative -limit. main maps it to exit code 2; a stdin read failure
// maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, reads stdin whole, and prints each NDJSON line prefixed
// with its 1-based line number. -limit, if positive, stops after that many
// lines without scanning the rest of the input -- the point of driving
// Lines through a range loop that can break early rather than collecting
// every line up front. run never touches os.Stdin/os.Stdout/os.Exit, so it
// is testable against a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("ndjson", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 0, "stop after this many lines (0 means no limit)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *limit < 0 {
		return fmt.Errorf("%w: -limit must not be negative, got %d", errUsage, *limit)
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("ndjson: reading input: %w", err)
	}

	w := bufio.NewWriter(stdout)
	n := 0
	for line := range Lines(data) {
		n++
		if _, err := fmt.Fprintf(w, "%d: %s\n", n, line); err != nil {
			return fmt.Errorf("ndjson: writing output: %w", err)
		}
		if *limit > 0 && n == *limit {
			break
		}
	}
	return w.Flush()
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ndjson [-limit N] < input")
		fmt.Fprintln(os.Stderr, "splits stdin into NDJSON lines and prints each one numbered.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ndjson:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '{"event":"login"}\n\n{"event":"click"}\r\n{"event":"logout"}' | go run .
printf '{"event":"login"}\n\n{"event":"click"}\r\n{"event":"logout"}' | go run . -limit 2
printf 'x' | go run . -limit -1
```

Expected output:

```text
1: {"event":"login"}
2: 
3: {"event":"click"}
4: {"event":"logout"}
1: {"event":"login"}
2: 
ndjson: usage: -limit must not be negative, got -1
```

Line 2 in the first run is the empty line between the login and click
events, printed as `2: ` with nothing after the colon -- yielded as a
genuinely empty (not skipped) line. The final event has no trailing newline
at all and is still printed like every other line. The second command shows
`-limit 2` stopping after exactly two lines even though the input holds
four. The third command shows the exit-2 usage path for a negative limit.

### Tests

`TestLines` is the required table: a trailing newline, no trailing newline,
an embedded empty line, CRLF endings, empty input, a single line with no
newline anywhere, and input that is only a bare newline. `TestLinesAreViews`
is the aliasing proof this lesson keeps coming back to -- it captures a
line's slice header, mutates the source `data` afterward, and requires the
mutation to be visible through the captured line, ruling out an
implementation that accidentally copies. `TestLinesEarlyBreak` is the test
that would fail if `Lines` ignored `yield`'s return value and kept scanning
past a `break`. `TestLinesAllocsZero` measures the zero-allocation claim
directly and does not call `t.Parallel`, since `testing.AllocsPerRun` panics
inside a parallel subtest. `TestRun` drives the command end to end: numbered
output over a mixed CRLF-and-empty-line input, `-limit` stopping early, a
negative `-limit` and an unknown flag both producing an error that wraps
`errUsage`, and empty input producing no output at all.

Create `ndjson_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"trailing newline", "{\"a\":1}\n{\"a\":2}\n", []string{`{"a":1}`, `{"a":2}`}},
		{"no trailing newline", "{\"a\":1}\n{\"a\":2}", []string{`{"a":1}`, `{"a":2}`}},
		{"empty lines are yielded as empty slices", "{\"a\":1}\n\n{\"a\":2}\n", []string{`{"a":1}`, ``, `{"a":2}`}},
		{"CRLF line endings are trimmed", "{\"a\":1}\r\n{\"a\":2}\r\n", []string{`{"a":1}`, `{"a":2}`}},
		{"empty input yields nothing", "", nil},
		{"single line, no newline at all", `{"a":1}`, []string{`{"a":1}`}},
		{"only a newline yields one empty line", "\n", []string{``}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			for line := range Lines([]byte(tc.in)) {
				got = append(got, string(line))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Lines(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Lines(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLinesAreViews proves each yielded line is a sub-slice of the original
// data, not a copy: mutating the input byte slice after collecting a line's
// backing pointer must be visible through it.
func TestLinesAreViews(t *testing.T) {
	t.Parallel()

	data := []byte("hello\nworld\n")
	var first []byte
	for line := range Lines(data) {
		first = line
		break
	}
	data[0] = 'H'
	if first[0] != 'H' {
		t.Fatalf("first line = %q after mutating data, want it to reflect the mutation", first)
	}
}

// TestLinesEarlyBreak proves the sequence stops scanning the instant the
// consuming range loop breaks: only the lines actually visited are yielded.
func TestLinesEarlyBreak(t *testing.T) {
	t.Parallel()

	data := []byte("one\ntwo\nthree\nfour\n")
	var got []string
	for line := range Lines(data) {
		got = append(got, string(line))
		if string(line) == "two" {
			break
		}
	}
	want := []string{"one", "two"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// TestLinesAllocsZero proves the zero-allocation claim rather than asserting
// it. Not run in parallel: testing.AllocsPerRun needs exclusive control of
// GC accounting and panics inside a parallel subtest.
func TestLinesAllocsZero(t *testing.T) {
	data := bytes.Repeat([]byte("{\"k\":\"v\"}\n"), 100)

	allocs := testing.AllocsPerRun(200, func() {
		for range Lines(data) {
		}
	})
	if allocs != 0 {
		t.Fatalf("Lines: got %v allocations per run, want 0", allocs)
	}
}

// TestRun exercises the command end to end without os.Args/os.Stdin/os.Exit.
func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("numbers every line, CRLF and empty lines included", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		in := "{\"a\":1}\r\n\n{\"a\":2}\n"
		if err := run(nil, strings.NewReader(in), &stdout); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "1: {\"a\":1}\n2: \n3: {\"a\":2}\n"
		if stdout.String() != want {
			t.Fatalf("run stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("limit stops early", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		in := "one\ntwo\nthree\n"
		if err := run([]string{"-limit", "2"}, strings.NewReader(in), &stdout); err != nil {
			t.Fatalf("run: %v", err)
		}
		want := "1: one\n2: two\n"
		if stdout.String() != want {
			t.Fatalf("run stdout = %q, want %q", stdout.String(), want)
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"negative limit is a usage error", []string{"-limit", "-1"}},
		{"unknown flag is a usage error", []string{"-bogus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			if err := run(tc.args, strings.NewReader("data\n"), &stdout); !errors.Is(err, errUsage) {
				t.Fatalf("run error = %v, want it to wrap errUsage", err)
			}
		})
	}

	t.Run("empty input produces no output", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		if err := run(nil, strings.NewReader(""), &stdout); err != nil || stdout.Len() != 0 {
			t.Fatalf("run(empty) = (err=%v, stdout=%q), want (nil, \"\")", err, stdout.String())
		}
	})
}
```

## Review

`Lines` is correct when it recovers exactly the same lines a naive
`bytes.Split` would, for every newline-shape in the table, and it is worth
using in place of `bytes.Split` only if it actually delivers the two
properties `bytes.Split` cannot: zero allocation and early exit. Each has
its own dedicated proof rather than a comment claiming it. The trap to watch
for in any range-over-func iterator is quietly breaking one of those two
properties while every content-based test still passes -- an implementation
that appends each line into an internal slice "just to be safe" before
yielding would pass `TestLines` line for line while failing
`TestLinesAllocsZero`, and one that ignores `yield`'s boolean would pass
every table case while failing `TestLinesEarlyBreak` the moment a caller
actually breaks early. `ndjson` maps a bad flag or a negative `-limit` to
exit code 2, a stdin read failure to exit code 1, and its own `-limit` flag
is the CLI-level demonstration that the early-exit property is not just a
library curiosity. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the two-index sub-slices `data[:i]` and `data[i+1:]` `Lines` advances through on every iteration.
- [`iter.Seq`](https://pkg.go.dev/iter#Seq) and [range-over-func (Go 1.23 release notes)](https://go.dev/doc/go1.23#language) — the iterator shape and the language support this module is built on.
- [`bytes.IndexByte`](https://pkg.go.dev/bytes#IndexByte) — the O(n) scan `Lines` uses to find each separator without allocating.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the function this module uses to prove, not assert, the zero-allocation claim.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-three-index-plugin-api-guard.md](18-three-index-plugin-api-guard.md) | Next: [../04-maps-creation-access-iteration/00-concepts.md](../04-maps-creation-access-iteration/00-concepts.md)
