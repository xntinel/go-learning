# Exercise 11: Cloning a Trace ID Out of a Pooled Log Line

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A tracing collector -- the ingestion side of Jaeger or an OpenTelemetry
Collector -- reads log lines one at a time, pulls a short trace-ID token out
of each one, and files it into a long-lived index so spans can later be
grouped by trace. The lines themselves arrive through a `bufio.Scanner` or a
pooled read buffer: one backing array, reused and overwritten on every read,
because allocating a fresh buffer per line would be wasteful at ingestion
volume. That reuse is exactly what makes the extraction step dangerous. A
trace ID is 16 or 32 hex characters; the log line it comes from can be a
kilobyte or more. If `ExtractTraceID` returns a sub-slice of the line instead
of a copy, the tiny trace ID that lands in the long-lived index keeps the
*entire* line buffer reachable for as long as the index entry exists --
except the buffer is about to be overwritten by the next line the scanner
reads, so what actually gets pinned and eventually read back is either stale
or corrupted content, not a memory number that shows up in a diff.

This is not a data race in the `-race` sense -- there is no concurrent
access, just one goroutine handing a slice header to a map and reading it
back later after the thing it points at changed underneath it. It is a class
of bug that survives code review because the extracted string *looks*
correct at the point it is logged, and only shows its damage later, in a
different line, in a different request's trace. This module builds the
extractor the way a Jaeger or OTel collector needs it: copy on the way out,
every time, and prove the copy is real by mutating the source after the
fact.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
traceindex/               module example.com/traceindex
  go.mod                   go 1.24
  traceindex.go             Extractor, NewExtractor, ExtractTraceID; ErrEmptyMarker
  traceindex_test.go        extraction table, independence-from-reuse, the aliased
                            contrast, ExampleExtractor_ExtractTraceID
```

- Files: `traceindex.go`, `traceindex_test.go`.
- Implement: `NewExtractor(marker string) (*Extractor, error)` rejecting an empty marker with `ErrEmptyMarker`; `(*Extractor).ExtractTraceID(line []byte) (id []byte, ok bool)` finding `marker` in `line`, taking the whitespace-delimited token that follows it, and returning `bytes.Clone` of that region so `id` never aliases `line`.
- Test: the extraction table (marker in the middle, at the end, at the start, absent, followed by whitespace, followed by nothing, empty line, nil line); `NewExtractor` rejecting an empty marker; a pooled-buffer test proving `id` survives the line being overwritten and does not carry the pooled buffer's capacity with it; an `extractTraceIDAliased` contrast pinning both the corruption and the exact capacity a raw sub-slice inherits; and `ExampleExtractor_ExtractTraceID` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A small live slice pins the whole backing array it came from

Every slice expression -- `line[start:end]`, two-index or three-index --
aliases `line`'s backing array. It copies nothing. That is true even when
`end - start` is 33 bytes and `len(line)` is 900: the returned slice header
is three words (pointer, length, capacity), and the pointer still points
into the same multi-hundred-byte array. As long as that 33-byte slice is
reachable -- sitting in an index map, say -- the garbage collector cannot
reclaim the array it points into, no matter how much of that array the
caller actually still cares about. The naive extractor looks entirely
correct by inspection:

```go
func extractTraceIDAliased(e *Extractor, line []byte) ([]byte, bool) {
    start, end, found := e.findToken(line)
    if !found {
        return nil, false
    }
    return line[start:end], true   // aliases line; no copy happened
}
```

It passes every test that checks the returned bytes equal the expected
trace ID, because right after the call, they do. The bug is not about
correctness at the call site; it is about what happens *after*. A
`bufio.Scanner`-backed line reader overwrites its one internal buffer on
every `Scan`. A line buffer drawn from a `sync.Pool` gets handed back and
reused by an unrelated goroutine. Either way, the moment the extractor's
caller moves on to the next line, every previously "extracted" trace ID
silently starts showing whatever bytes now occupy that same memory --
sometimes garbage, sometimes, worse, a different request's trace ID. The fix
is not a bounds check; there is nothing out of bounds here. It is
`bytes.Clone`: copy the matched bytes into their own backing array before
the extractor returns, so nothing the caller does to `line` afterward can
reach `id`.

Create `traceindex.go`:

```go
// Package traceindex pulls a trace-ID token out of tracing log lines --
// the same job a Jaeger or OpenTelemetry collector's line scanner does
// before it can group spans by trace -- and hands back a right-sized,
// independent copy suitable for a long-lived index.
//
// The one detail this package exists to get right: a log line arrives in a
// buffer the caller is about to overwrite or that is otherwise many times
// larger than the token being extracted. Returning a sub-slice of that
// buffer instead of a copy keeps the entire buffer reachable for as long as
// the tiny trace ID is reachable -- a real production heap-retention leak,
// not a correctness bug, because the extracted bytes still read correctly.
// See ExtractTraceID.
package traceindex

import (
	"bytes"
	"errors"
)

// ErrEmptyMarker means NewExtractor was given an empty marker.
var ErrEmptyMarker = errors.New("traceindex: marker must not be empty")

// Extractor locates a trace-ID token that follows a fixed marker in a log
// line, e.g. the token after "trace_id=" in
// `level=info trace_id=4bf92f3577b34da6a3ce929d0e0e4736 msg="ok"`.
//
// An Extractor holds only its immutable marker after construction and is
// safe for concurrent use by multiple goroutines.
type Extractor struct {
	marker []byte
}

// NewExtractor returns an Extractor that looks for marker in each line
// passed to ExtractTraceID. It returns ErrEmptyMarker if marker is empty.
func NewExtractor(marker string) (*Extractor, error) {
	if marker == "" {
		return nil, ErrEmptyMarker
	}
	return &Extractor{marker: []byte(marker)}, nil
}

// ExtractTraceID scans line for the configured marker and returns the
// whitespace-delimited token that immediately follows it. It reports
// ok=false if the marker is absent, or if the marker is followed directly
// by whitespace or the end of line (an empty token).
//
// The returned slice is a fresh copy (bytes.Clone) and never aliases line:
// the caller may retain id in a long-lived index indefinitely, including
// past the point where line is overwritten by a scanner's next read or
// returned to a buffer pool. This is the property that keeps a small, live
// trace ID from pinning the much larger line buffer it came from.
func (e *Extractor) ExtractTraceID(line []byte) (id []byte, ok bool) {
	start, end, found := e.findToken(line)
	if !found {
		return nil, false
	}
	return bytes.Clone(line[start:end]), true
}

// findToken locates the marker in line and returns the half-open byte
// range of the token that follows it. found is false if the marker is
// absent or immediately followed by whitespace or end of line.
func (e *Extractor) findToken(line []byte) (start, end int, found bool) {
	idx := bytes.Index(line, e.marker)
	if idx < 0 {
		return 0, 0, false
	}
	start = idx + len(e.marker)
	end = start
	for end < len(line) && !isSpace(line[end]) {
		end++
	}
	if end == start {
		return 0, 0, false
	}
	return start, end, true
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t'
}
```

### Using it

Build one `Extractor` per marker at startup -- it is immutable after
`NewExtractor` returns, so a single value can be shared across every
goroutine reading log lines concurrently, which is what the type's doc
comment promises. Call `ExtractTraceID` per line; on `ok == true`, `id` is
independent of whatever buffer `line` came from and safe to store in a map,
append to a slice, or hand to another goroutine without a second thought.
On `ok == false` -- no marker, or the marker with nothing usable after it --
`id` is `nil`, so a caller can treat the two return values as a normal
comma-ok pattern.

`ExampleExtractor_ExtractTraceID` in the test file is the executable
demonstration of this module: `go test` runs it and compares its stdout
against the `// Output:` comment, so the usage below cannot drift from the
code. It indexes one line that carries a trace ID and skips one that
doesn't, then reports how many trace IDs made it into the index.

### Tests

`TestExtractTraceID` is the table: the marker in the middle of a line, at
its very end, at its very start, absent entirely, followed immediately by
whitespace, and followed by nothing at all -- plus an empty line and a nil
line, both of which must report `ok=false` with a nil `id`.
`TestNewExtractorRejectsEmptyMarker` pins the constructor's sentinel.

`TestExtractTraceIDIsIndependentOfLine` is the heart of the module. It
builds a line the way a pooled scanner buffer looks -- real content sitting
inside a much larger backing array -- extracts the trace ID, then overwrites
the *entire* backing array exactly as a scanner reusing that buffer would
before its next `Scan` call returns. The extracted `id` must still read
correctly afterward, and its capacity must stay far below the pooled
buffer's, which is the property that lets the large buffer actually get
collected once the scanner moves on. `TestAliasedExtractionPinsTheLeak`
runs the same scenario through `extractTraceIDAliased`, an unexported helper
that returns the raw sub-slice: its capacity is asserted to reach exactly
`cap(line) - start`, a guarantee of the slice-expression spec and therefore
exact rather than approximate, and after the same buffer-reuse step its
value has visibly changed -- the corruption the clone-based path never
exhibits.

Create `traceindex_test.go`:

```go
package traceindex

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// extractTraceIDAliased is ExtractTraceID as it is usually written the
// first time: it returns the matched region directly, a sub-slice of the
// caller's line rather than a copy. It is never exported and never
// reachable from Extractor; it exists only so the tests can pin the
// aliasing it leaves behind.
func extractTraceIDAliased(e *Extractor, line []byte) (id []byte, ok bool) {
	start, end, found := e.findToken(line)
	if !found {
		return nil, false
	}
	return line[start:end], true
}

func newExtractor(t *testing.T) *Extractor {
	t.Helper()
	e, err := NewExtractor("trace_id=")
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	return e
}

func TestExtractTraceID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		line   []byte
		wantID string
		wantOK bool
	}{
		{
			name:   "trace id in the middle",
			line:   []byte(`level=info trace_id=4bf92f3577b34da6a3ce929d0e0e4736 msg="ok"`),
			wantID: "4bf92f3577b34da6a3ce929d0e0e4736",
			wantOK: true,
		},
		{
			name:   "trace id at the very end of the line",
			line:   []byte(`level=info trace_id=abc123`),
			wantID: "abc123",
			wantOK: true,
		},
		{
			name:   "marker at the start of the line",
			line:   []byte(`trace_id=deadbeef level=info`),
			wantID: "deadbeef",
			wantOK: true,
		},
		{
			name:   "marker absent",
			line:   []byte(`level=info msg="no trace here"`),
			wantOK: false,
		},
		{
			name:   "marker followed immediately by whitespace",
			line:   []byte(`trace_id= level=info`),
			wantOK: false,
		},
		{
			name:   "marker at the very end with no token",
			line:   []byte(`level=info trace_id=`),
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   []byte(``),
			wantOK: false,
		},
		{
			name:   "nil line",
			line:   nil,
			wantOK: false,
		},
	}

	e := newExtractor(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, ok := e.ExtractTraceID(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (id=%q)", ok, tc.wantOK, id)
			}
			if !ok {
				if id != nil {
					t.Fatalf("id = %q, want nil when ok is false", id)
				}
				return
			}
			if string(id) != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

func TestNewExtractorRejectsEmptyMarker(t *testing.T) {
	t.Parallel()

	if _, err := NewExtractor(""); !errors.Is(err, ErrEmptyMarker) {
		t.Fatalf("NewExtractor(\"\") error = %v, want ErrEmptyMarker", err)
	}
}

// pooledLine simulates a scanner's reused read buffer: content of length n
// living inside a much larger backing array, the way bufio.Scanner reuses
// one internal buffer across every call to Scan.
func pooledLine(content string, backing int) []byte {
	buf := make([]byte, backing)
	copy(buf, content)
	return buf[:len(content)]
}

// TestExtractTraceIDIsIndependentOfLine is the heart of the module: it
// proves the returned id survives the line buffer being reused and
// overwritten, and that its capacity does not carry the huge backing array
// of a pooled line along with it -- the two faces of the same bug this
// package exists to avoid.
func TestExtractTraceIDIsIndependentOfLine(t *testing.T) {
	t.Parallel()

	const backing = 8192
	line := pooledLine("level=info trace_id=cafef00ddeadbeef00000000cafebabe msg=ok", backing)

	e := newExtractor(t)
	id, ok := e.ExtractTraceID(line)
	if !ok {
		t.Fatal("ExtractTraceID: want ok=true")
	}
	want := "cafef00ddeadbeef00000000cafebabe"
	if string(id) != want {
		t.Fatalf("id = %q, want %q", id, want)
	}

	// The scanner reuses the buffer for its next line: overwrite it exactly
	// as bufio.Scanner would before the next Scan call returns.
	for i := range line[:cap(line)] {
		line[:cap(line)][i] = 'X'
	}
	if string(id) != want {
		t.Fatalf("id changed after the line buffer was reused: %q, want %q", id, want)
	}

	// A clone of a 33-byte token must not carry an 8192-byte backing array;
	// the exact capacity the allocator grants a clone is not a contract
	// (see slices.Clone's own documentation), but staying far below the
	// pooled buffer's capacity is the property that keeps the buffer
	// collectible, and that property is deterministic here.
	if cap(id) >= backing/2 {
		t.Fatalf("cap(id) = %d, want well under the %d-byte pooled buffer", cap(id), backing)
	}
}

// TestAliasedExtractionPinsTheLeak contrasts the buggy path directly: the
// aliased result changes when the line buffer is reused, and its capacity
// reaches all the way to the end of line's backing array -- a guarantee of
// the slice-expression spec, not of the allocator, so it holds exactly.
func TestAliasedExtractionPinsTheLeak(t *testing.T) {
	t.Parallel()

	const backing = 8192
	line := pooledLine("level=info trace_id=cafef00ddeadbeef00000000cafebabe msg=ok", backing)
	start := bytes.Index(line, []byte("trace_id=")) + len("trace_id=")

	e := newExtractor(t)
	aliased, ok := extractTraceIDAliased(e, line)
	if !ok {
		t.Fatal("extractTraceIDAliased: want ok=true")
	}
	want := "cafef00ddeadbeef00000000cafebabe"
	if string(aliased) != want {
		t.Fatalf("aliased = %q, want %q", aliased, want)
	}

	// cap of a two-index slice expression is cap(parent) - low: a
	// language guarantee, not a runtime growth curve, so this holds
	// exactly rather than as an approximate property.
	if cap(aliased) != cap(line)-start {
		t.Fatalf("cap(aliased) = %d, want %d", cap(aliased), cap(line)-start)
	}

	// Reusing the buffer, exactly as in TestExtractTraceIDIsIndependentOfLine,
	// now corrupts the previously returned token: this is the bug.
	for i := range line[:cap(line)] {
		line[:cap(line)][i] = 'X'
	}
	if string(aliased) == want {
		t.Fatal("aliased id survived buffer reuse; want it corrupted, proving it aliased line")
	}
}

// ExampleExtractor_ExtractTraceID is the runnable demonstration of this
// module: go test executes it and compares its stdout against the Output
// comment below.
func ExampleExtractor_ExtractTraceID() {
	e, err := NewExtractor("trace_id=")
	if err != nil {
		panic(err)
	}

	lines := []string{
		`ts=1 level=info trace_id=4bf92f3577b34da6a3ce929d0e0e4736 msg="request handled"`,
		`ts=2 level=warn msg="no trace context propagated"`,
	}

	var index []string
	for _, line := range lines {
		id, ok := e.ExtractTraceID([]byte(line))
		if !ok {
			fmt.Println("no trace id found")
			continue
		}
		index = append(index, string(id))
		fmt.Printf("indexed trace id: %s\n", id)
	}
	fmt.Println("index size:", len(index))

	// Output:
	// indexed trace id: 4bf92f3577b34da6a3ce929d0e0e4736
	// no trace id found
	// index size: 1
}
```

## Review

`ExtractTraceID` is correct when the trace ID it returns keeps reading
correctly no matter what happens afterward to the line it was extracted
from -- `TestExtractTraceIDIsIndependentOfLine` pins exactly that by
overwriting the source buffer after extraction and checking the token still
matches. The mechanism is `bytes.Clone`: every slice expression aliases its
backing array, so `line[start:end]` alone hands the caller a live view into
memory the scanner is about to reuse, and `TestAliasedExtractionPinsTheLeak`
shows precisely what that costs -- a token that silently changes value once
the buffer is overwritten, and a capacity that reaches all the way to the
end of `line`'s backing array (`cap(line) - start`, exactly, by the
slice-expression spec) rather than the few dozen bytes the token actually
needs. `NewExtractor` rejects an empty marker with `ErrEmptyMarker`, an
`Extractor` is immutable after construction and therefore safe to share
across goroutines, and `ExtractTraceID` reports `ok=false` with a nil `id`
for every input that doesn't contain a usable token. Run
`go test -count=1 -race ./...` to confirm all of it, including
`ExampleExtractor_ExtractTraceID`, the runnable demonstration `go test`
checks against its `// Output:` comment.

## Resources

- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — the copy used to break aliasing with the source line.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the real-world reused-buffer source this module models; see its docs on `Bytes()` validity.
- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the exact capacity rule (`cap(a[low:]) == cap(a) - low`) `TestAliasedExtractionPinsTheLeak` relies on.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — background on why reslicing never copies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-frame-writer-copy-sized-by-length.md](10-frame-writer-copy-sized-by-length.md) | Next: [12-arena-allocator-reslice-not-isolation.md](12-arena-allocator-reslice-not-isolation.md)
