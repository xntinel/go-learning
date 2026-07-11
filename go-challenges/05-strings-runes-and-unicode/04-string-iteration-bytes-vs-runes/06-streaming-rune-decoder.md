# Exercise 6: Decode a Large Text Stream Rune-by-Rune Without Buffering It All

When a request body or an uploaded file is too large to hold in memory but you still
must operate per character — validate it, count it, split it — you decode it as a
stream. The trap is calling `utf8.DecodeRune` on each independent `Read` chunk: a
rune whose bytes straddle two reads is corrupted at the seam. This module uses
`bufio.Reader.ReadRune`, which reassembles UTF-8 sequences across read-buffer
boundaries.

## What you'll build

```text
runestream/                independent module: example.com/runestream
  go.mod                   go 1.26
  runestream.go            Counts, Count, CountRunes, SplitWords
  cmd/
    demo/
      main.go              counts a mixed stream, splits into words
  runestream_test.go       totals, seam-spanning reads, invalid bytes, error propagation
```

Files: `runestream.go`, `cmd/demo/main.go`, `runestream_test.go`.
Implement: `Count(r io.Reader) (Counts, error)`, `CountRunes(r io.Reader) (runes, bytes int, err error)`, `SplitWords(r io.Reader) ([]string, error)`.
Test: totals over ASCII/Latin/CJK; `iotest.OneByteReader` forces seam-spanning runes; invalid bytes yield the `(RuneError, size 1)` contract; clean EOF; a mid-stream read error propagates.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/runestream/cmd/demo
cd ~/go-exercises/runestream
go mod init example.com/runestream
```

### Why ReadRune, and its exact contract

`bufio.NewReader(r)` wraps any `io.Reader` in a buffer, and `(*bufio.Reader).ReadRune`
returns one decoded rune at a time as `(r rune, size int, err error)`. Its whole value
here is that it owns the buffer: if a three-byte CJC rune arrives as one byte in this
read and two in the next, `ReadRune` holds the partial sequence and completes it on the
following fill. A naive loop of `Read` into a chunk plus `utf8.DecodeRune` cannot do
that — it sees an incomplete sequence at the end of a chunk and emits a spurious
`RuneError`, corrupting the stream at every seam.

The contract has three outcomes the loop must handle:

- a valid rune: `(r, size, nil)` with `size` in `1..4`;
- an invalid byte: `(utf8.RuneError, 1, nil)` — note `err` is nil; the read succeeded,
  the byte was just not valid UTF-8, so it is reported as U+FFFD of size 1 and the
  stream continues. This is the same `(RuneError, size 1)` signal seen throughout the
  chapter, now arriving from a reader; distinguishing it from a legitimate U+FFFD
  (which returns size 3) is exactly the invalid-byte count;
- end or error: `(0, 0, err)` where `err` is `io.EOF` at a clean end, or an underlying
  read error mid-stream. `io.EOF` is the normal terminator and is swallowed into a nil
  return; any other error is propagated, never mistaken for EOF.

`SplitWords` is the small tokenizer on top: it accumulates runes into a `strings.Builder`
and flushes a word whenever it meets a `unicode.IsSpace` rune, streaming the whole input
without ever holding more than the current word.

Create `runestream.go`:

```go
// runestream.go
package runestream

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Counts reports what a streaming decode observed. Invalid counts bytes that were
// not valid UTF-8 (the (RuneError, size 1) signal), distinct from any legitimate
// U+FFFD characters, which are counted as ordinary runes.
type Counts struct {
	Runes   int
	Bytes   int
	Invalid int
}

// Count decodes r one rune at a time via bufio.Reader.ReadRune, correctly
// reassembling UTF-8 sequences that span read-buffer boundaries. A clean io.EOF
// terminates; any other read error is returned.
func Count(r io.Reader) (Counts, error) {
	br := bufio.NewReader(r)
	var c Counts
	for {
		ru, size, err := br.ReadRune()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return c, nil
			}
			return c, err
		}
		c.Runes++
		c.Bytes += size
		if ru == utf8.RuneError && size == 1 {
			c.Invalid++
		}
	}
}

// CountRunes is the narrow form: total runes and total bytes decoded from r.
func CountRunes(r io.Reader) (runes, bytes int, err error) {
	c, err := Count(r)
	return c.Runes, c.Bytes, err
}

// SplitWords streams r rune by rune and returns whitespace-separated words,
// never buffering more than the current word.
func SplitWords(r io.Reader) ([]string, error) {
	br := bufio.NewReader(r)
	var (
		words []string
		cur   strings.Builder
	)
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for {
		ru, _, err := br.ReadRune()
		if err != nil {
			if errors.Is(err, io.EOF) {
				flush()
				return words, nil
			}
			return words, err
		}
		if unicode.IsSpace(ru) {
			flush()
			continue
		}
		cur.WriteRune(ru)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"strings"

	"example.com/runestream"
)

func main() {
	const body = "café 中文 tokens here"
	c, _ := runestream.Count(strings.NewReader(body))
	fmt.Printf("runes=%d bytes=%d invalid=%d\n", c.Runes, c.Bytes, c.Invalid)

	words, _ := runestream.SplitWords(strings.NewReader(body))
	fmt.Printf("words=%d %q\n", len(words), words)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
runes=19 bytes=24 invalid=0
words=4 ["café" "中文" "tokens" "here"]
```

### Tests

The decisive test wraps the reader in `iotest.OneByteReader`, which hands out one byte
per `Read`, guaranteeing every multi-byte rune spans several reads; if `ReadRune`
failed to reassemble across the seam, the rune count would inflate and the invalid
count would be non-zero. A separate test feeds raw invalid bytes to pin the
`(RuneError, size 1)` contract, and a `failAfter` reader proves a mid-stream error is
propagated rather than swallowed as EOF.

Create `runestream_test.go`:

```go
// runestream_test.go
package runestream

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCountTotals(t *testing.T) {
	t.Parallel()
	const s = "café 中文" // 7 runes, 12 bytes
	c, err := Count(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if c.Runes != 7 || c.Bytes != 12 || c.Invalid != 0 {
		t.Errorf("Count = %+v, want {7 12 0}", c)
	}
}

func TestCountReassemblesAcrossReadSeams(t *testing.T) {
	t.Parallel()
	const s = "中文字abcé" // multi-byte runes that will straddle 1-byte reads
	// One byte per Read: every 3-byte rune spans three reads.
	c, err := Count(iotest.OneByteReader(strings.NewReader(s)))
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	runes, bytes, _ := CountRunes(strings.NewReader(s))
	if c.Runes != runes || c.Bytes != bytes {
		t.Errorf("one-byte-reader Count = %+v, whole-read = {%d %d}", c, runes, bytes)
	}
	if c.Invalid != 0 {
		t.Errorf("Invalid = %d, want 0 (seam reassembly failed)", c.Invalid)
	}
}

func TestCountInvalidBytes(t *testing.T) {
	t.Parallel()
	// One stray 0xff plus a legitimate U+FFFD; only the stray byte is Invalid.
	r := strings.NewReader("a\xffb�c")
	c, err := Count(r)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if c.Invalid != 1 {
		t.Errorf("Invalid = %d, want 1 (only the stray 0xff)", c.Invalid)
	}
	// a, U+FFFD(stray), b, U+FFFD(real), c = 5 runes.
	if c.Runes != 5 {
		t.Errorf("Runes = %d, want 5", c.Runes)
	}
}

var errBoom = errors.New("boom")

// failAfter serves data, then fails with errBoom instead of io.EOF.
type failAfter struct {
	data []byte
	pos  int
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, errBoom
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func TestCountPropagatesReadError(t *testing.T) {
	t.Parallel()
	_, err := Count(&failAfter{data: []byte("ok")})
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want errBoom propagated (not swallowed as EOF)", err)
	}
}

func TestSplitWords(t *testing.T) {
	t.Parallel()
	words, err := SplitWords(strings.NewReader("  café\t中文\n tokens "))
	if err != nil {
		t.Fatalf("SplitWords: %v", err)
	}
	want := []string{"café", "中文", "tokens"}
	if fmt.Sprint(words) != fmt.Sprint(want) {
		t.Errorf("SplitWords = %q, want %q", words, want)
	}
}

func ExampleCount() {
	c, _ := Count(strings.NewReader("中文"))
	fmt.Println(c.Runes, c.Bytes)
	// Output: 2 6
}
```

## Review

The decoder is correct when the totals from a whole-buffer read equal the totals from
a one-byte-at-a-time read — which `TestCountReassemblesAcrossReadSeams` asserts —
because equality proves `ReadRune` stitched every multi-byte rune back together across
the seam. The invalid-byte count must distinguish a stray `0xff` (`RuneError` of size 1)
from a legitimate U+FFFD (size 3), and `io.EOF` must terminate cleanly while any other
error propagates: `TestCountPropagatesReadError` guards against the classic bug of
treating every non-nil error as end-of-stream. The mistake this module exists to
prevent — decoding each `Read` chunk independently with `utf8.DecodeRune` — silently
corrupts exactly the multi-byte runes the one-byte-reader test hammers.

## Resources

- [bufio.Reader.ReadRune](https://pkg.go.dev/bufio#Reader.ReadRune) — the buffered, seam-safe rune decode and its invalid-byte contract.
- [testing/iotest.OneByteReader](https://pkg.go.dev/testing/iotest#OneByteReader) — forcing runes to span reads in a test.
- [io.EOF](https://pkg.go.dev/io#EOF) — the sentinel that means clean end, not error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-byte-offset-to-line-column.md](05-byte-offset-to-line-column.md) | Next: [07-rune-aware-pii-redaction.md](07-rune-aware-pii-redaction.md)
