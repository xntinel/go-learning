# Exercise 7: Fuzz A CSV/Log Line Splitter For Byte-Preservation

A log-ingest pipeline splits each line on a separator byte into fields. The
splitter is trivial to write and easy to get subtly wrong on the edges — trailing
separators, consecutive separators, an empty line. This module builds
`SplitFields` and fuzzes two invariants that together pin the field logic exactly:
joining the fields back reconstructs the line byte-for-byte, and the field count
is always the separator count plus one.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
logsplit/                  independent module: example.com/logsplit
  go.mod                   module path
  split.go                 SplitFields(string, byte) []string
  cmd/
    demo/
      main.go              split a couple of log lines, print the fields
  split_test.go            TestSplitFieldsTable, FuzzSplitFields, Example
```

Files: `split.go`, `cmd/demo/main.go`, `split_test.go`.
Implement: `SplitFields(line string, sep byte) []string`.
Test: a table test for the edge cases; `FuzzSplitFields` asserting the two
invariants over `(string, byte)` inputs.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzSplitFields -fuzztime=2s`.

### The one-byte-separator trap, and the two invariants

The natural implementation is `strings.Split(line, string(sep))` — and it hides a
classic Go gotcha. `string(sep)` where `sep` is a `byte` is an *integer-to-string
conversion*: it produces the UTF-8 encoding of the code point whose value is
`sep`. For a byte above 127 that is *two* bytes, not the single byte you meant, so
splitting a line on `string(byte(200))` looks for a two-byte sequence that is not
there. The correct way to turn a single byte into a one-byte separator string is
`string([]byte{sep})`. Getting this wrong is exactly the kind of edge a fuzzer
that generates high byte values surfaces instantly.

With the separator built correctly, two invariants hold for *every* line and
*every* separator byte, and they are the complete specification of a correct
splitter:

- **Round-trip:** `strings.Join(SplitFields(line, sep), string([]byte{sep})) ==
  line`. `strings.Split` and `strings.Join` are exact inverses for any non-empty
  separator, so any deviation means the split dropped or duplicated bytes.
- **Count:** `len(SplitFields(line, sep)) == strings.Count(line, string([]byte{sep})) + 1`.
  n separators produce n+1 fields — always, including zero separators (one field,
  the whole line) and a trailing separator (a final empty field). This is the
  invariant that catches off-by-one field logic and any misguided "skip empty
  fields" behavior.

Create `split.go`:

```go
package logsplit

import "strings"

// sepString converts a single separator byte into a one-byte string. Using
// string(sep) directly would UTF-8-encode the byte and produce two bytes for any
// value above 127; string([]byte{sep}) preserves the exact byte.
func sepString(sep byte) string {
	return string([]byte{sep})
}

// SplitFields splits line on sep into fields. n occurrences of sep always yield
// n+1 fields, and joining the fields on sep reconstructs line exactly.
func SplitFields(line string, sep byte) []string {
	return strings.Split(line, sepString(sep))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logsplit"
)

func main() {
	lines := []struct {
		line string
		sep  byte
	}{
		{"2026-07-02,INFO,started", ','},
		{"a::b:", ':'},
		{"single", ','},
	}
	for _, tc := range lines {
		fields := logsplit.SplitFields(tc.line, tc.sep)
		fmt.Printf("%q on %q -> %d fields %q\n", tc.line, string(tc.sep), len(fields), fields)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"2026-07-02,INFO,started" on "," -> 3 fields ["2026-07-02" "INFO" "started"]
"a::b:" on ":" -> 4 fields ["a" "" "b" ""]
"single" on "," -> 1 fields ["single"]
```

### Tests

Create `split_test.go`:

```go
package logsplit

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitFieldsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		sep  byte
		want []string
	}{
		{"", ',', []string{""}},
		{"a", ',', []string{"a"}},
		{"a,b,c", ',', []string{"a", "b", "c"}},
		{"a,,b", ',', []string{"a", "", "b"}},
		{"trailing,", ',', []string{"trailing", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			got := SplitFields(tc.line, tc.sep)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("SplitFields(%q, %q) = %q, want %q", tc.line, string(tc.sep), got, tc.want)
			}
		})
	}
}

func FuzzSplitFields(f *testing.F) {
	seeds := []struct {
		line string
		sep  byte
	}{
		{"", ','},
		{"a,b,c", ','},
		{"a,,b,", ','},
		{"no-sep-here", ';'},
		{"high\xc8byte", 0xc8},
		{`quoted,"a,b",c`, ','},
	}
	for _, s := range seeds {
		f.Add(s.line, s.sep)
	}
	f.Fuzz(func(t *testing.T, line string, sep byte) {
		fields := SplitFields(line, sep)
		sepStr := string([]byte{sep})

		if got := strings.Join(fields, sepStr); got != line {
			t.Fatalf("Join(SplitFields(%q, %q)) = %q, want original", line, sepStr, got)
		}
		if want := strings.Count(line, sepStr) + 1; len(fields) != want {
			t.Fatalf("SplitFields(%q, %q) produced %d fields, want %d", line, sepStr, len(fields), want)
		}
	})
}

func Example() {
	fmt.Printf("%q\n", SplitFields("id,name,", ','))
	// Output: ["id" "name" ""]
}
```

## Review

The splitter is correct when both invariants hold for every line and every
separator byte: joining reconstructs the input exactly, and the field count is the
separator count plus one. The single most important line in the module is
`string([]byte{sep})` rather than `string(sep)` — the fuzzer's high-byte inputs
(the `0xc8` seed) are precisely what exposes the integer-to-string conversion bug,
where a byte above 127 becomes two bytes and the round-trip breaks. The count
invariant is what stops a well-meaning refactor from silently dropping empty
fields, which would corrupt every downstream column index. Run
`go test -race ./...`, then `go test -fuzz=FuzzSplitFields -fuzztime=2s`.

## Resources

- [`strings.Split`](https://pkg.go.dev/strings#Split) and [`strings.Join`](https://pkg.go.dev/strings#Join) — the exact-inverse pair the round-trip relies on.
- [`strings.Count`](https://pkg.go.dev/strings#Count) — the separator count behind the n+1 invariant.
- [Go spec: conversions to string type](https://go.dev/ref/spec#Conversions_to_and_from_a_string_type) — why `string(byteValue)` UTF-8-encodes rather than preserving the byte.

---

Back to [06-regression-corpus-from-crash.md](06-regression-corpus-from-crash.md) | Next: [08-path-traversal-safe-join.md](08-path-traversal-safe-join.md)
