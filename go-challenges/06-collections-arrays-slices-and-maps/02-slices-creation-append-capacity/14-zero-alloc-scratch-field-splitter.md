# Exercise 14: A Command-Line Field Extractor Backed by a Reused Scratch Slice

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A CSV or structured-log ingestion path can run millions of lines a second
through the same field splitter, and at that rate the cost per line is what
determines whether the pipeline keeps up. A splitter with the signature
`Split(line []byte) [][]byte` — the shape everyone reaches for first — makes
a fresh `[][]byte` on every single call, and the garbage collector pays for
every one of them. This module builds the version production log parsers
actually use as a small `cut -f`-style command-line tool: `Split(line
[]byte, sep byte, scratch [][]byte) [][]byte`, where the caller (here,
`main`'s own read loop) owns and reuses `scratch` across every line, and the
function never allocates as long as that buffer is already large enough.

The other half of the design is what makes it fast in the first place: the
returned fields are not copies of the line's bytes, they are sub-slices that
alias `line` directly. That is what makes splitting itself allocation-free —
no new byte storage is ever created — but it comes with a contract the
caller must honor: the fields are valid only as long as `line`'s bytes are
not mutated or overwritten. The tool's own read loop is where that contract
gets exercised for real, because `bufio.Scanner.Bytes()` returns a slice
into the scanner's own reused buffer, valid only until the next `Scan()`
call — the exact same rule, one layer further up the stack.

This module is fully self-contained: its own `go mod init`, an executable
tool, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fields/                   module example.com/fields
  go.mod                  go 1.24
  fields.go                ErrInvalidSeparator, ErrInvalidFieldIndex, ErrFieldOutOfRange;
                          ParseSeparator, Split, FieldSelector
  fields_test.go           split table, aliasing proof, scratch-reuse proof, AllocsPerRun == 0,
                          FieldSelector table, end-to-end run() table
  main.go                  flags, stdin/stdout, exit codes
```

- Files: `fields.go`, `fields_test.go`, `main.go`.
- Implement: `ParseSeparator(s string) (byte, error)` requiring exactly one byte; `Split(line []byte, sep byte, scratch [][]byte) [][]byte`, which resets `scratch[:0]` and appends `line[start:i]` sub-slices for every separator found plus the trailing field, growing normally only if the line has more fields than `cap(scratch)`; `NewFieldSelector(index int) (*FieldSelector, error)` validating a positive 1-based index; `(*FieldSelector).Select(fields [][]byte) ([]byte, error)` returning `ErrFieldOutOfRange` if the line is too short.
- Tool: `fields` reads lines from stdin, splits each on `-sep` (default `,`), and prints the `-f`-th field (default `1`, 1-based). It exits `2` for an invalid separator, a non-positive field index, or a line with too few fields, `1` if reading stdin or writing output fails, and `0` on success.
- Test: `Split` over comma- and space-separated lines including empty, leading, trailing, and consecutive separators; the aliasing contract proved by mutating `line` after the split; the scratch-reuse contract proved with `unsafe.SliceData`, alongside the case where `cap(scratch)` is too small; `testing.AllocsPerRun` asserting exactly zero allocations once `scratch` is sized correctly; `FieldSelector` construction and range checks; and an end-to-end `run()` table covering the default field, a custom field and separator, an invalid separator, and a short line.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/14-zero-alloc-scratch-field-splitter
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/14-zero-alloc-scratch-field-splitter
go mod edit -go=1.24
```

### Reusing scratch[:0] and aliasing into line at the same time

`Split` combines two of this lesson's ideas in one function. The first is
`s[:0]`: `scratch[:0]` keeps `scratch`'s existing backing array and its full
capacity while resetting its length to zero, the same idiom Exercise 3 uses
for batch reuse. Every field found in `line` is then appended into that
reset view; as long as the number of fields does not exceed `cap(scratch)`,
every append writes in place and the call allocates nothing for the
`[][]byte` itself. The tool's read loop keeps reassigning `scratch = fields`
after every line, so a wider line that ever forces growth still leaves the
buffer sized correctly for every line after it — the steady-state cost
becomes proportional only to `len(line)`.

The second idea is aliasing, and it is deliberately not softened here the
way a copying splitter would soften it. Each field `Split` appends is
`line[start:i]` — a sub-slice of the caller's own `line`, sharing its
backing array, not a `string(line[start:i])` conversion or a `bytes.Clone`
of it. That is what makes the split allocation-free on the byte-storage
side too: producing `n` fields costs zero new byte allocations, because no
bytes are copied, only slice headers pointing into memory that already
exists. The cost of this design is the contract every aliasing slice
carries: the fields are valid only until `line` is mutated or its buffer is
reused. In the tool this is not a hypothetical — `bufio.Scanner.Bytes()`
returns exactly that kind of slice, reused on every `Scan()` call, so the
read loop must fully consume the field it selects (here: write it) before
looping back to read the next line.

If a line has more fields than `cap(scratch)`, `append` inside `Split`
behaves exactly as it does anywhere else: it reallocates a larger backing
array and copies the fields already appended. That call is no longer
allocation-free, but it is still correct, and keeping the returned
(now-larger) slice as `scratch` means growth happens at most a handful of
times as line widths vary across the input, not once per line forever.

Create `fields.go`:

```go
// This file holds fields' domain logic: splitting a line on a separator
// byte into caller-owned scratch storage, selecting one field by its
// 1-based position, and validating the separator flag. main.go is the only
// file that touches flags, stdin/stdout, or exit codes.
package main

import (
	"errors"
	"fmt"
)

// ErrInvalidSeparator means the configured separator was not exactly one byte.
var ErrInvalidSeparator = errors.New("fields: separator must be exactly one byte")

// ErrInvalidFieldIndex means the configured field index was not positive.
var ErrInvalidFieldIndex = errors.New("fields: field index must be positive")

// ErrFieldOutOfRange means a line had fewer fields than index requires.
var ErrFieldOutOfRange = errors.New("fields: field index out of range")

// ParseSeparator validates that s is exactly one byte long and returns it,
// or ErrInvalidSeparator otherwise. Field separators in this tool are
// single bytes, not multi-byte sequences, so any other length is rejected
// before it can silently behave like the first byte of s and ignore the
// rest.
func ParseSeparator(s string) (byte, error) {
	if len(s) != 1 {
		return 0, fmt.Errorf("%w: got %q", ErrInvalidSeparator, s)
	}
	return s[0], nil
}

// Split splits line on sep into fields, appending each field into
// scratch[:0] instead of allocating a fresh [][]byte on every call. As long
// as cap(scratch) is at least the current line's field count, every append
// lands in scratch's own backing array and the call allocates nothing for
// the [][]byte itself. A line with more fields than cap(scratch) still
// splits correctly; append just grows scratch's backing array like it
// would anywhere else, and the caller should keep the returned (now
// larger) slice as its scratch for the next call.
//
// Aliasing contract: every returned field is a sub-slice of line, not a
// copy; the caller must not mutate line, or let it be reused, while the
// fields are still in use.
//
// Split holds no state of its own and is safe to call concurrently, as
// long as no two goroutines are ever given the same scratch slice -- Split
// mutates scratch in place.
func Split(line []byte, sep byte, scratch [][]byte) [][]byte {
	out := scratch[:0]
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == sep {
			out = append(out, line[start:i])
			start = i + 1
		}
	}
	out = append(out, line[start:])
	return out
}

// FieldSelector picks one field, by its 1-based position, out of the slice
// Split produces.
//
// A FieldSelector is immutable after construction and is safe for
// concurrent use.
type FieldSelector struct {
	index int
}

// NewFieldSelector returns a FieldSelector for the given 1-based field
// index. It returns ErrInvalidFieldIndex if index is not positive.
func NewFieldSelector(index int) (*FieldSelector, error) {
	if index < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidFieldIndex, index)
	}
	return &FieldSelector{index: index}, nil
}

// Select returns fields[index-1], or ErrFieldOutOfRange, wrapped with the
// field count actually present, if fields is shorter than the configured
// index requires.
//
// The returned slice aliases fields (and, transitively, whatever line
// Split built fields from); it is not a copy.
func (f *FieldSelector) Select(fields [][]byte) ([]byte, error) {
	if f.index > len(fields) {
		return nil, fmt.Errorf("%w: index %d, line has %d field(s)", ErrFieldOutOfRange, f.index, len(fields))
	}
	return fields[f.index-1], nil
}
```

### The tool

`run` takes the flag arguments, stdin, and stdout as parameters, which is
what makes it testable end to end with `strings.Reader` and `bytes.Buffer`
instead of spawning a subprocess. The read loop is where the module's whole
point lives: `scratch` is declared once, outside the loop, and every call to
`Split` reuses it via `scratch = fields`, so a stream of same-width lines
allocates a new `[][]byte` exactly once, on the very first line. `main`'s
only jobs are calling `run`, printing any error to `os.Stderr`, and mapping
it to an exit code: an invalid separator, a non-positive field index, or a
line with too few fields are all input problems, exit `2`; a failure
reading stdin or writing output is an execution failure, exit `1`.

Create `main.go`:

```go
// Command fields prints one column of a delimited text stream, the way
// `cut -f` does, but built to run cleanly on a hot log-ingestion path: it
// reuses one scratch [][]byte across every line instead of allocating a
// fresh one per line.
//
// Usage:
//
//	fields [-sep ,] [-f 1] < lines.txt
//
// Exit codes: 0 on success, 2 if a flag or a line was invalid (bad
// separator, non-positive field index, or a line with too few fields), 1
// if reading stdin or writing output failed.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("fields", flag.ContinueOnError)
	sepFlag := fs.String("sep", ",", "field separator (exactly one byte)")
	fieldFlag := fs.Int("f", 1, "1-based field index to print")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "fields: print one column of a delimited stream, reusing one scratch buffer")
		fmt.Fprintln(fs.Output(), "usage: fields [-sep ,] [-f 1] < lines.txt")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	sep, err := ParseSeparator(*sepFlag)
	if err != nil {
		return err
	}
	selector, err := NewFieldSelector(*fieldFlag)
	if err != nil {
		return err
	}

	sc := bufio.NewScanner(stdin)
	w := bufio.NewWriter(stdout)

	// scratch is reused across every line via Split's scratch[:0] reset, so
	// once it fits the widest line seen so far, no later call allocates.
	var scratch [][]byte
	lineNo := 0
	for sc.Scan() {
		lineNo++
		// sc.Bytes() aliases the Scanner's own buffer and is only valid
		// until the next Scan(); fields below alias it too, so this body
		// must consume the selected field before looping back to Scan().
		fields := Split(sc.Bytes(), sep, scratch)
		scratch = fields

		field, err := selector.Select(fields)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if _, err := w.Write(field); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("fields: reading input: %w", err)
	}
	return w.Flush()
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fields:", err)
		if errors.Is(err, ErrInvalidSeparator) || errors.Is(err, ErrInvalidFieldIndex) || errors.Is(err, ErrFieldOutOfRange) || errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '2024-01-01T00:00:00Z,api-gateway,200,12ms\n2024-01-01T00:00:01Z,auth-service,401,3ms\n' | go run .
printf '2024-01-01T00:00:00Z,api-gateway,200,12ms\n2024-01-01T00:00:01Z,auth-service,401,3ms\n' | go run . -f=2
printf 'GET /health 200\nPOST /users 201\n' | go run . -f=3 -sep=' '
```

Expected output:

```text
2024-01-01T00:00:00Z
2024-01-01T00:00:01Z
```

```text
api-gateway
auth-service
```

```text
200
201
```

A line with too few fields, `printf 'a,b\n' | go run . -f=5`, prints
`fields: line 1: fields: field index out of range: index 5, line has 2
field(s)` to stderr and exits `2`, naming both the line and the shortfall
instead of panicking on an out-of-range index.

### Tests

`TestSplitFields` is a table covering the field-boundary shapes that
matter: a normal comma-separated line, a line with no separator at all, an
empty line, and leading, trailing, and consecutive separators.
`TestSplitFieldsAliasIntoLine` is the aliasing proof: it splits, mutates
`line[0]`, and asserts the returned field changed with it — if `Split` ever
started copying instead of aliasing, this is the test that would catch it.
`TestSplitScratchBehavior` covers both sides of the reuse mechanism in one
test: `unsafe.SliceData` proves the returned fields share `scratch`'s
backing array when capacity suffices, and a second subtest confirms
correctness holds even when it does not. `TestSplitZeroAllocs` is the core
proof this module exists for: `testing.AllocsPerRun` over 200 repeated
calls with a correctly-sized `scratch` and a fixed `line`, asserting
exactly zero allocations; it skips `t.Parallel` because `AllocsPerRun`
panics if run from a parallel test. `TestFieldSelector` covers both of the
selector's validation paths, and `TestRunEndToEnd` drives `run` exactly the
way `main` does, over `strings.Reader` and `bytes.Buffer`.

Create `fields_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

func TestParseSeparator(t *testing.T) {
	t.Parallel()

	if sep, err := ParseSeparator(","); err != nil || sep != ',' {
		t.Fatalf("ParseSeparator(\",\") = (%q, %v), want (',', nil)", sep, err)
	}
	for _, bad := range []string{"", ",,", "tab"} {
		if _, err := ParseSeparator(bad); !errors.Is(err, ErrInvalidSeparator) {
			t.Errorf("ParseSeparator(%q) error = %v, want ErrInvalidSeparator", bad, err)
		}
	}
}

func TestSplitFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		sep  byte
		want []string
	}{
		{"comma separated", "alpha,beta,gamma", ',', []string{"alpha", "beta", "gamma"}},
		{"single field, no separator present", "solo", ',', []string{"solo"}},
		{"empty line", "", ',', []string{""}},
		{"leading separator produces empty first field", ",beta", ',', []string{"", "beta"}},
		{"trailing separator produces empty last field", "alpha,", ',', []string{"alpha", ""}},
		{"consecutive separators produce empty middle field", "a,,c", ',', []string{"a", "", "c"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scratch := make([][]byte, 0, 8)
			got := Split([]byte(tc.line), tc.sep, scratch)

			if len(got) != len(tc.want) {
				t.Fatalf("Split(%q) = %d fields %q, want %d fields %q", tc.line, len(got), got, len(tc.want), tc.want)
			}
			for i, f := range got {
				if string(f) != tc.want[i] {
					t.Fatalf("Split(%q)[%d] = %q, want %q", tc.line, i, f, tc.want[i])
				}
			}
		})
	}
}

// TestSplitFieldsAliasIntoLine proves the aliasing contract: mutating line
// after Split is visible through the returned fields.
func TestSplitFieldsAliasIntoLine(t *testing.T) {
	t.Parallel()

	line := []byte("alpha,beta")
	fields := Split(line, ',', make([][]byte, 0, 4))

	if string(fields[0]) != "alpha" {
		t.Fatalf("fields[0] = %q, want %q before mutation", fields[0], "alpha")
	}
	line[0] = 'A'
	if string(fields[0]) != "Alpha" {
		t.Fatalf("fields[0] = %q, want %q after mutating line (aliasing broke)", fields[0], "Alpha")
	}
}

// TestSplitScratchBehavior: with enough capacity Split reuses scratch's own
// backing array (proved with unsafe.SliceData); with too little, append
// grows it like it would anywhere else and the fields are still correct.
func TestSplitScratchBehavior(t *testing.T) {
	t.Parallel()

	t.Run("reuses backing array when capacity suffices", func(t *testing.T) {
		scratch := make([][]byte, 0, 4)
		scratchData := unsafe.SliceData(scratch[:cap(scratch)])
		got := Split([]byte("a,b,c"), ',', scratch)
		if unsafe.SliceData(got[:cap(got)]) != scratchData {
			t.Fatal("Split returned a different backing array than scratch")
		}
	})

	t.Run("still correct when scratch is too small", func(t *testing.T) {
		got := Split([]byte("a,b,c"), ',', make([][]byte, 0, 1))
		want := []string{"a", "b", "c"}
		if len(got) != len(want) {
			t.Fatalf("Split() = %d fields, want %d", len(got), len(want))
		}
		for i, f := range got {
			if string(f) != want[i] {
				t.Fatalf("Split()[%d] = %q, want %q", i, f, want[i])
			}
		}
	})
}

// TestSplitZeroAllocs is the core proof Split exists for: a pre-sized
// scratch and a fixed line, repeated, allocate nothing. No t.Parallel:
// testing.AllocsPerRun panics if run from a parallel test.
func TestSplitZeroAllocs(t *testing.T) {
	line := []byte("2024-01-01T00:00:00Z,api-gateway,200,12ms")
	scratch := make([][]byte, 0, 8)
	var sink [][]byte

	allocs := testing.AllocsPerRun(200, func() {
		sink = Split(line, ',', scratch)
	})
	if allocs != 0 {
		t.Fatalf("Split allocated %.1f times per call, want 0", allocs)
	}
	if len(sink) != 4 {
		t.Fatalf("len(sink) = %d, want 4", len(sink))
	}
}

func TestFieldSelector(t *testing.T) {
	t.Parallel()

	for _, index := range []int{0, -1} {
		if _, err := NewFieldSelector(index); !errors.Is(err, ErrInvalidFieldIndex) {
			t.Errorf("NewFieldSelector(%d) error = %v, want ErrInvalidFieldIndex", index, err)
		}
	}

	fields := Split([]byte("GET,/health,200"), ',', make([][]byte, 0, 4))

	sel, err := NewFieldSelector(2)
	if err != nil {
		t.Fatalf("NewFieldSelector: %v", err)
	}
	if got, err := sel.Select(fields); err != nil || string(got) != "/health" {
		t.Fatalf("Select() = (%q, %v), want (\"/health\", nil)", got, err)
	}

	outOfRange, err := NewFieldSelector(9)
	if err != nil {
		t.Fatalf("NewFieldSelector: %v", err)
	}
	if _, err := outOfRange.Select(fields); !errors.Is(err, ErrFieldOutOfRange) {
		t.Fatalf("Select() error = %v, want ErrFieldOutOfRange", err)
	}
}

// TestRunEndToEnd drives run over strings.Reader/bytes.Buffer, as main does.
func TestRunEndToEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantStdout string
		wantErr    error
	}{
		{name: "default field is the first column", stdin: "a,b,c\nd,e,f\n", wantStdout: "a\nd\n"},
		{name: "custom field and separator", args: []string{"-f=2", "-sep=|"}, stdin: "GET|/health|200\nPOST|/users|201\n", wantStdout: "/health\n/users\n"},
		{name: "invalid separator flag rejected before reading stdin", args: []string{"-sep=,,"}, stdin: "a,b\n", wantErr: ErrInvalidSeparator},
		{name: "a line with too few fields", args: []string{"-f=3"}, stdin: "a,b\n", wantErr: ErrFieldOutOfRange},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &out)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("run() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(): unexpected error: %v", err)
			}
			if out.String() != tc.wantStdout {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.wantStdout)
			}
		})
	}
}
```

## Review

`fields` is correct when it produces the right column for every separator
shape, and `Split` earns its "zero allocation" claim only under the
specific condition its doc comment states: `cap(scratch)` already covers
the line's field count. `TestSplitZeroAllocs` measures exactly that
condition, and `TestSplitScratchBehavior`'s second subtest measures the
other side of the same coin — correctness holds even when the
zero-allocation condition does not. The aliasing test matters as much as
the allocation test: a version of `Split` that copied each field with
`bytes.Clone` would still pass every correctness test in the table, would
still be allocation-heavier (which `TestSplitZeroAllocs` would catch), but
the deeper reason to prefer aliasing is architectural — it is what makes
splitting itself free of byte-copying. The read loop in `main.go` is where
that contract is exercised for real: `sc.Bytes()` is only valid until the
next `Scan()`, so the loop must select and write the field before it loops
back, which it does within the same iteration. `NewFieldSelector` and
`Select` reject a non-positive index and an out-of-range one respectively,
both checkable with `errors.Is` and both mapped to exit code `2`; a failure
reading stdin or writing stdout maps to `1`. Run
`go test -count=1 -race ./...`.

## Resources

- [`unsafe.SliceData`](https://pkg.go.dev/unsafe#SliceData) — the tool used here to prove two slices share a backing array.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the zero-allocation proof this module is built around.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Bytes()`'s reuse-until-next-`Scan()` contract, which the read loop must respect.
- [`bytes.Split`](https://pkg.go.dev/bytes#Split) — the standard library's allocating equivalent, useful as a contrast: it always returns freshly allocated fields.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-per-goroutine-append-then-concat.md](13-per-goroutine-append-then-concat.md) | Next: [15-chunked-upload-three-index-split.md](15-chunked-upload-three-index-split.md)
