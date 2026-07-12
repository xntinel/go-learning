# Exercise 6: Streaming Rune Decoder Over a Log io.Reader

A log tokenizer reads from a socket or a pipe, where a single `Read` can return
any number of bytes — including one that ends in the middle of a multi-byte rune.
Decode each `Read`'s bytes independently and you miscount whenever a rune straddles
two reads. The fix is `bufio.Reader.ReadRune`, which fills more bytes until it has
a whole rune.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
logscan/                    independent module: example.com/logscan
  go.mod                    go 1.25
  internal/logscan/logscan.go CountRunes, FirstInvalidOffset
  internal/logscan/logscan_test.go OneByteReader boundary + invalid-byte tests
  cmd/demo/main.go          runnable: count runes over a byte-at-a-time reader
```

Files: `internal/logscan/logscan.go`, `internal/logscan/logscan_test.go`,
`cmd/demo/main.go`.
Implement: `CountRunes(r io.Reader) (int, error)` and
`FirstInvalidOffset(r io.Reader) (int, error)` using `bufio.Reader.ReadRune`.
Test: drive with `iotest.OneByteReader` over multi-byte input; assert the count
matches `utf8.RuneCountInString`, invalid bytes report the right offset, and clean
input ends on `io.EOF`.
Verify: `go test -count=1 -race ./...`

### Why ReadRune and not per-Read DecodeRune

`bufio.Reader.ReadRune` returns `(rune, size, error)`. Internally it guarantees a
*full* rune before returning: if the buffered bytes hold only part of a multi-byte
sequence, it fills more from the underlying reader until the rune is complete.
That is exactly the property a hand-rolled `Read`-then-`utf8.DecodeRune` loop
lacks — under a reader that hands back one byte at a time (the worst case, modeled
by `iotest.OneByteReader`, and a realistic stand-in for TCP fragmentation), a
naive decoder sees `\xe2` alone, calls it incomplete or invalid, and miscounts.
`ReadRune` never does.

`CountRunes` loops `ReadRune` until it returns `io.EOF`, which it treats as the
clean end (return the count, `nil`). Any other error is a real read failure and is
returned as is. Invalid *encoding* is not an error from `ReadRune`: it returns
`(utf8.RuneError, 1, nil)`, so `CountRunes` counts the replacement character as one
rune — matching what `utf8.RuneCountInString` does over the same bytes.

`FirstInvalidOffset` uses the same loop but tracks a running byte offset (summing
each `size`) and returns the offset of the first rune that decodes to
`utf8.RuneError` with `size == 1` — the signature of a genuinely invalid byte, as
opposed to a real `U+FFFD` in the stream (which decodes with `size == 3`). If the
stream is clean it returns `-1, nil` at `io.EOF`. This is the position a scanner
reports when it must point the operator at the exact byte where a log line went
bad.

Create `internal/logscan/logscan.go`:

```go
package logscan

import (
	"bufio"
	"errors"
	"io"
	"unicode/utf8"
)

// CountRunes counts the runes decoded from r, correctly reassembling multi-byte
// runes that straddle Read boundaries. A clean end (io.EOF) returns (count, nil);
// any other read error is returned with the count so far.
func CountRunes(r io.Reader) (int, error) {
	br := bufio.NewReader(r)
	count := 0
	for {
		_, _, err := br.ReadRune()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		count++
	}
}

// FirstInvalidOffset returns the byte offset of the first invalid UTF-8 sequence
// in r, or -1 if the whole stream is valid. A read error other than io.EOF is
// returned with the offset scanned so far.
func FirstInvalidOffset(r io.Reader) (int, error) {
	br := bufio.NewReader(r)
	offset := 0
	for {
		ru, size, err := br.ReadRune()
		if errors.Is(err, io.EOF) {
			return -1, nil
		}
		if err != nil {
			return offset, err
		}
		if ru == utf8.RuneError && size == 1 {
			return offset, nil
		}
		offset += size
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"testing/iotest"
	"unicode/utf8"

	"example.com/logscan/internal/logscan"
)

func main() {
	line := "héllo café — 2024"
	// OneByteReader forces every multi-byte rune to straddle a Read boundary.
	n, err := logscan.CountRunes(iotest.OneByteReader(strings.NewReader(line)))
	fmt.Printf("CountRunes=%d err=%v want=%d\n", n, err, utf8.RuneCountInString(line))

	bad := "ok\xffbad"
	off, _ := logscan.FirstInvalidOffset(iotest.OneByteReader(strings.NewReader(bad)))
	fmt.Printf("FirstInvalidOffset(%q)=%d\n", bad, off)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
CountRunes=17 err=<nil> want=17
FirstInvalidOffset("ok\xffbad")=2
```

### Tests

Create `internal/logscan/logscan_test.go`:

```go
package logscan

import (
	"fmt"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

func TestCountRunesStraddlesBoundary(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"hello",
		"héllo café",
		"日本語のログ行",
		"",
		"mixed café 日本 2024",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			// OneByteReader guarantees every multi-byte rune spans >1 Read.
			got, err := CountRunes(iotest.OneByteReader(strings.NewReader(in)))
			if err != nil {
				t.Fatalf("CountRunes(%q) err = %v", in, err)
			}
			if want := utf8.RuneCountInString(in); got != want {
				t.Fatalf("CountRunes(%q) = %d, want %d", in, got, want)
			}
		})
	}
}

func TestFirstInvalidOffset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"all valid café", -1},
		{"\xffstart", 0},
		{"ok\xffbad", 2},
		{"café\x80", 5}, // bad byte after a multi-byte rune: byte offset 5
		{"", -1},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := FirstInvalidOffset(iotest.OneByteReader(strings.NewReader(tc.in)))
			if err != nil {
				t.Fatalf("FirstInvalidOffset(%q) err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("FirstInvalidOffset(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleCountRunes() {
	n, _ := CountRunes(strings.NewReader("café"))
	fmt.Println(n)
	// Output: 4
}
```

## Review

`CountRunes` is correct when its count over an `iotest.OneByteReader` matches
`utf8.RuneCountInString` — if they diverge, a rune split across two reads was
miscounted, which is precisely the bug `ReadRune` prevents. `FirstInvalidOffset`
is correct when it points at the first genuinely invalid byte (`RuneError` with
`size == 1`) and returns `-1` for clean input. The mistake to avoid is decoding
each `Read`'s bytes in isolation; the second is treating `io.EOF` as an error
rather than the clean terminator. Run `go test -race`.

## Resources

- [`bufio.Reader.ReadRune`](https://pkg.go.dev/bufio#Reader.ReadRune)
- [`testing/iotest.OneByteReader`](https://pkg.go.dev/testing/iotest#OneByteReader)
- [`io.EOF`](https://pkg.go.dev/io#pkg-variables)
- [`utf8.RuneError`](https://pkg.go.dev/unicode/utf8#pkg-constants)

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-strip-unicode-control-and-bidi.md](07-strip-unicode-control-and-bidi.md)
