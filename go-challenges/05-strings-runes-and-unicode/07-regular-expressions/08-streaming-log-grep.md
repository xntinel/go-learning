# Exercise 8: Streaming Grep over Large Log Files

A log search that loads a gigabyte file into memory is a bug waiting for the file
to grow. This module builds a memory-bounded grep: it scans line by line with
`bufio.Scanner`, reports each match's line number, byte offsets, and captured
groups via `FindStringSubmatchIndex`, and includes a reader-based variant using
`FindReaderSubmatchIndex` over an `io.RuneReader` — so the input is never held in
memory all at once.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
loggrep/                    independent module: example.com/loggrep
  go.mod                    go 1.26
  loggrep.go                type Match; GrepLines (Scanner+FindStringSubmatchIndex); GrepReader (RuneReader)
  cmd/
    demo/
      main.go               runnable demo: grep an in-memory multi-line log
  loggrep_test.go           line numbers, byte offsets, capture groups, long line, reader parity
```

- Files: `loggrep.go`, `cmd/demo/main.go`, `loggrep_test.go`.
- Implement: `GrepLines(r io.Reader, re *regexp.Regexp) ([]Match, error)` using `bufio.Scanner` with a raised buffer and `FindStringSubmatchIndex`; `GrepReader(r io.RuneReader, re *regexp.Regexp) ([]int, bool)` using `FindReaderSubmatchIndex`.
- Test: matching lines report correct line numbers and byte offsets; capture-group indices point at the right substring; a line longer than `bufio.Scanner`'s default 64 KiB buffer is handled; the reader variant reports the same first-match offsets.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/loggrep/cmd/demo
cd ~/go-exercises/loggrep
go mod init example.com/loggrep
```

### Byte offsets, a raised buffer, and the RuneReader variant

Three mechanics matter here. First, `FindStringSubmatchIndex` returns a `[]int` of
**byte** offsets — pairs of (start, end) where index 0/1 is the whole match and
2/3, 4/5, ... are the capture groups — or nil on no match. These are byte offsets
into the string you passed, so you slice with them directly (`line[loc[2]:loc[3]]`)
to get a group's text with no re-scan. They are *not* rune indices; on multibyte
UTF-8 you must not treat them as character positions. A group that did not
participate in the match is reported as `-1, -1`, so the extraction guards against
a negative start.

Second, `bufio.Scanner` reads one line at a time, so memory is bounded by the
longest line, not the file size — but its default maximum token is 64 KiB, and a
longer line makes `Scan` stop with `bufio.ErrTooLong`. Production log lines
(a serialized stack trace, a base64 blob) routinely exceed that, so `GrepLines`
raises the limit with `sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)`: a small
initial buffer that grows up to a documented cap. This is the honest fix — you
still bound memory, you just choose the bound deliberately instead of inheriting
64 KiB.

Third, `FindReaderSubmatchIndex(r io.RuneReader)` matches directly over a reader
that yields runes, for input too large to hold as a single string. It returns
byte offsets into the *stream* and finds the leftmost match; because the reader is
consumed as it goes, it reports one match rather than all of them. `bufio.NewReader`
wraps any `io.Reader` and satisfies `io.RuneReader` via `ReadRune`. `GrepReader`
demonstrates this path and, on a single-line input, returns the same offsets that
`GrepLines` reports for line 1 — the parity the test pins.

Create `loggrep.go`:

```go
package loggrep

import (
	"bufio"
	"io"
	"regexp"
)

// maxLineBytes caps the per-line buffer: memory stays bounded, but a line far
// larger than bufio's default 64 KiB token is still handled.
const maxLineBytes = 4 << 20 // 4 MiB

// Match is one regex hit within a line.
type Match struct {
	Line   int      // 1-based line number
	Start  int      // byte offset of the whole match within the line
	End    int      // byte offset just past the whole match
	Groups []string // capture groups 1..n; a non-participating group is ""
}

// GrepLines scans r line by line and returns every match, with byte offsets into
// each line. Memory is bounded by the longest line, never the whole input.
func GrepLines(r io.Reader, re *regexp.Regexp) ([]Match, error) {
	var matches []Match
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		loc := re.FindStringSubmatchIndex(text)
		if loc == nil {
			continue
		}
		var groups []string
		for i := 2; i+1 < len(loc); i += 2 {
			if loc[i] < 0 {
				groups = append(groups, "")
				continue
			}
			groups = append(groups, text[loc[i]:loc[i+1]])
		}
		matches = append(matches, Match{Line: line, Start: loc[0], End: loc[1], Groups: groups})
	}
	if err := sc.Err(); err != nil {
		return matches, err
	}
	return matches, nil
}

// GrepReader finds the leftmost match over an io.RuneReader without holding the
// input in memory, returning the byte offsets into the stream (as
// FindStringSubmatchIndex would) and whether a match was found.
func GrepReader(r io.RuneReader, re *regexp.Regexp) ([]int, bool) {
	loc := re.FindReaderSubmatchIndex(r)
	if loc == nil {
		return nil, false
	}
	return loc, true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"regexp"
	"strings"

	"example.com/loggrep"
)

func main() {
	log := "2026-01-01 boot ok\n2026-01-02 request code=503 failed\n2026-01-03 request code=200 ok\n"
	re := regexp.MustCompile(`code=(\d+)`)

	matches, err := loggrep.GrepLines(strings.NewReader(log), re)
	if err != nil {
		panic(err)
	}
	for _, m := range matches {
		fmt.Printf("line %d [%d:%d] status=%s\n", m.Line, m.Start, m.End, m.Groups[0])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
line 2 [19:27] status=503
line 3 [19:27] status=200
```

### Tests

Create `loggrep_test.go`:

```go
package loggrep

import (
	"bufio"
	"regexp"
	"strings"
	"testing"
)

var codeRe = regexp.MustCompile(`code=(\d+)`)

func TestGrepLines(t *testing.T) {
	t.Parallel()
	input := "alpha ok\nbeta code=42 mid\ngamma code=7 end\n"
	lines := strings.Split(strings.TrimRight(input, "\n"), "\n")

	matches, err := GrepLines(strings.NewReader(input), codeRe)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}

	if matches[0].Line != 2 || matches[0].Groups[0] != "42" {
		t.Fatalf("match0 = %+v, want line 2 group 42", matches[0])
	}
	if matches[1].Line != 3 || matches[1].Groups[0] != "7" {
		t.Fatalf("match1 = %+v, want line 3 group 7", matches[1])
	}

	// The byte offsets must slice the right substring out of the original line.
	m0 := matches[0]
	if got := lines[m0.Line-1][m0.Start:m0.End]; got != "code=42" {
		t.Fatalf("offset slice = %q, want %q", got, "code=42")
	}
}

func TestGrepLinesHandlesLongLine(t *testing.T) {
	t.Parallel()
	// A single line well past bufio.Scanner's default 64 KiB token.
	long := strings.Repeat("x", 100_000) + " code=999 " + strings.Repeat("y", 100_000)
	matches, err := GrepLines(strings.NewReader(long+"\n"), codeRe)
	if err != nil {
		t.Fatalf("long line: %v", err)
	}
	if len(matches) != 1 || matches[0].Groups[0] != "999" {
		t.Fatalf("matches = %+v, want one match group 999", matches)
	}
}

func TestGrepReaderParity(t *testing.T) {
	t.Parallel()
	line := "beta code=42 mid"

	fromLines, err := GrepLines(strings.NewReader(line+"\n"), codeRe)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromLines) != 1 {
		t.Fatalf("GrepLines got %d, want 1", len(fromLines))
	}

	loc, ok := GrepReader(bufio.NewReader(strings.NewReader(line)), codeRe)
	if !ok {
		t.Fatal("GrepReader found no match")
	}
	// Whole-match offsets must agree with the line-based scan.
	if loc[0] != fromLines[0].Start || loc[1] != fromLines[0].End {
		t.Fatalf("reader offsets [%d:%d] != lines [%d:%d]", loc[0], loc[1], fromLines[0].Start, fromLines[0].End)
	}
	if got := line[loc[2]:loc[3]]; got != "42" {
		t.Fatalf("reader group slice = %q, want 42", got)
	}
}

func TestGrepLinesNoMatch(t *testing.T) {
	t.Parallel()
	matches, err := GrepLines(strings.NewReader("nothing here\nplain text\n"), codeRe)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("got %d matches, want 0", len(matches))
	}
}
```

## Review

The grep is correct and memory-bounded when three things hold. `GrepLines` scans
with `bufio.Scanner`, so it holds one line at a time, and the raised
`sc.Buffer(...)` cap is what keeps a 200 KiB stack-trace line from tripping
`bufio.ErrTooLong` — `TestGrepLinesHandlesLongLine` proves it. The byte offsets
from `FindStringSubmatchIndex` slice the exact matched substring out of the
original line, which `TestGrepLines` verifies rather than trusting; treating them
as rune indices would be the multibyte off-by-many bug the concepts warn about.
`GrepReader` shows the `io.RuneReader` path for input too large to stringify, and
`TestGrepReaderParity` confirms it reports the same offsets as the line scan on a
shared input. The property never to lose sight of: neither path slurps the whole
input, so both scale to a file far larger than memory. Run `go test -race`.

## Resources

- [`regexp` package](https://pkg.go.dev/regexp) — `FindStringSubmatchIndex`, `FindReaderSubmatchIndex`, `FindAllStringIndex`.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Scan`, `Buffer`, and the default token-size limit.
- [`io.RuneReader`](https://pkg.go.dev/io#RuneReader) — the interface `FindReaderSubmatchIndex` consumes, satisfied by `bufio.Reader`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-config-placeholder-expander.md](07-config-placeholder-expander.md) | Next: [09-redos-complexity-guard.md](09-redos-complexity-guard.md)
