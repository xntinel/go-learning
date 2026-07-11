# 3. Buffered I/O With bufio In A Word Counter

Build a small `wordcount` package that uses `bufio` to read text in bulk and a custom `bufio.SplitFunc` to split on a delimiter the standard library does not provide. The lesson focuses on the rule that `bufio.Reader` and `bufio.Writer` reduce system calls by batching, the rule that `bufio.Scanner` is the canonical way to read text token-by-token, and the rule that a `SplitFunc` is the seam where you change the unit of iteration.

```text
bufiotx/
  go.mod
  wordcount/
    counter.go
    split.go
    counter_test.go
    split_test.go
  cmd/demo/main.go
```

The package exposes a `Counter` that scans an `io.Reader` for whitespace-delimited words (case-insensitive) and returns a `map[string]int`. The package also exposes a `Split` function that splits on `;`, so a caller can iterate `key=value` pairs from a cookie header or a CSV-like blob. The tests pin the counting contract, the case-insensitive contract, the empty-input contract, and the custom split contract; the demo shows both halves running on real text.

## Concepts

### Unbuffered I/O Pays A System Call Per Read

Every `Read` on an `*os.File` translates to a system call. Reading a file one byte at a time pays a syscall per byte. `bufio.Reader` allocates an internal buffer (default 4096 bytes), reads it once, and serves subsequent `Read` calls from memory until the buffer is exhausted. The same logic in reverse applies to `bufio.Writer`: it batches `Write` calls until `Flush` (or close) pushes the bytes to the underlying writer.

### `Scanner` Splits By Default On Lines

`bufio.NewScanner(r)` returns a `*bufio.Scanner` whose default split function is `bufio.ScanLines`. The other built-ins are `ScanWords` (whitespace-delimited, never empty), `ScanBytes` (one token per byte), and `ScanRunes` (one token per UTF-8 rune). A custom `SplitFunc` is the seam to add your own (`bufio.SplitFunc`).

### A `SplitFunc` Returns Advance, Token, And Error

```go
type SplitFunc func(data []byte, atEOF bool) (advance int, token []byte, err error)
```

`advance` is the byte count the scanner consumes from the input; `token` is the slice the caller sees; `err` is non-nil to stop scanning. Return `0, nil, nil` to ask for more data. Return `0, data, bufio.ErrFinalToken` to deliver the final empty token. Return `0, nil, err` to stop with an error.

### `Flush` Is Mandatory Before Closing The Underlying Writer

A `bufio.Writer` that is not `Flush`ed will lose buffered bytes. `bufio.Writer.Write` returns `nil` even when the bytes have not yet reached the underlying writer; only `Flush` (or `Close`, which calls `Flush`) makes the data visible.

### Failure Modes

- `Scanner.Scan` returns `false` at `io.EOF` AND on read errors. Without `scanner.Err()` you cannot distinguish them.
- `bufio.Scanner` has a default max token size of `MaxScanTokenSize = 64 * 1024`. A token longer than that trips `bufio.ErrTooLong`; use `Scanner.Buffer(buf, max)` to grow it.
- A `SplitFunc` that returns a negative `advance` triggers `bufio.ErrNegativeAdvance`; a `SplitFunc` that returns `advance` past the input length triggers `bufio.ErrAdvanceTooFar`.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/bufiotx/wordcount/cmd/demo
cd ~/go-exercises/bufiotx
go mod init example.com/bufiotx
```

### Exercise 1: The Counter

Create `wordcount/counter.go`:

```go
package wordcount

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrEmptyReader is returned by Counter.Count when the reader has
// no words. The caller can compare with errors.Is.
var ErrEmptyReader = errors.New("wordcount: reader produced no words")

// Counts holds the per-word totals produced by Counter.Count.
// The map is owned by the caller; Count mutates it in place.
type Counts map[string]int

// Counter reads an io.Reader and tallies word frequencies
// case-insensitively. It is safe to reuse a Counter across inputs.
type Counter struct {
	scanner *bufio.Scanner
}

// NewCounter returns a Counter that scans r with ScanWords and a
// per-token trim that strips common punctuation.
func NewCounter(r io.Reader) *Counter {
	sc := bufio.NewScanner(r)
	sc.Split(bufio.ScanWords)
	return &Counter{scanner: sc}
}

// Count populates out with the word frequencies read from the
// Counter's reader. It returns ErrEmptyReader (wrapped with %w) when
// the reader yields no words; callers can test with errors.Is.
func (c *Counter) Count(out Counts) error {
	if out == nil {
		out = make(Counts)
	}
	counted := 0
	for c.scanner.Scan() {
		word := strings.ToLower(strings.Trim(c.scanner.Text(), punct))
		if word == "" {
			continue
		}
		out[word]++
		counted++
	}
	if err := c.scanner.Err(); err != nil {
		return fmt.Errorf("wordcount: scan: %w", err)
	}
	if counted == 0 {
		return ErrEmptyReader
	}
	return nil
}

const punct = ".,!?;:\"'()[]{}<>"
```

`bufio.NewScanner` + `bufio.ScanWords` is the canonical idiom for "give me whitespace-separated tokens". `strings.Trim` strips punctuation before the lowercased word is recorded.

### Exercise 2: A Custom `SplitFunc` For `;`

Create `wordcount/split.go`:

```go
package wordcount

import "bytes"

// SplitSemi returns a bufio.SplitFunc that splits on the byte ';'.
// It returns each non-empty trimmed token (without the delimiter).
// The token is valid only until the next call to Scan.
func SplitSemi(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, ';'); i >= 0 {
		return i + 1, bytes.TrimSpace(data[:i]), nil
	}
	if atEOF {
		return len(data), bytes.TrimSpace(data), nil
	}
	return 0, nil, nil
}
```

The function looks for `;` in the buffered data; if found, it advances past the delimiter and returns the trimmed prefix as the token. If we are at end-of-input and there is no delimiter, the remainder is the final token. Returning `(0, nil, nil)` asks the scanner for more data.

### Exercise 3: Test The Counter

Create `wordcount/counter_test.go`:

```go
package wordcount

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCounterTalliesCaseInsensitive(t *testing.T) {
	t.Parallel()

	c := NewCounter(strings.NewReader("The the THE cat cat dog"))
	got := make(Counts)
	if err := c.Count(got); err != nil {
		t.Fatalf("Count: %v", err)
	}
	want := Counts{"the": 3, "cat": 2, "dog": 1}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %d, want %d", k, got[k], v)
		}
	}
}

func TestCounterStripsPunctuation(t *testing.T) {
	t.Parallel()

	c := NewCounter(strings.NewReader("Hello, world! Hello; world?"))
	got := make(Counts)
	if err := c.Count(got); err != nil {
		t.Fatalf("Count: %v", err)
	}
	want := Counts{"hello": 2, "world": 2}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %d, want %d", k, got[k], v)
		}
	}
}

func TestCounterEmptyInput(t *testing.T) {
	t.Parallel()

	c := NewCounter(strings.NewReader(""))
	if err := c.Count(make(Counts)); !errors.Is(err, ErrEmptyReader) {
		t.Fatalf("err = %v, want ErrEmptyReader", err)
	}
}

func TestCounterWhitespaceOnly(t *testing.T) {
	t.Parallel()

	c := NewCounter(strings.NewReader("   \n\t  \n"))
	if err := c.Count(make(Counts)); !errors.Is(err, ErrEmptyReader) {
		t.Fatalf("err = %v, want ErrEmptyReader", err)
	}
}

func TestCounterPropagatesReadErrors(t *testing.T) {
	t.Parallel()

	c := NewCounter(iotest.ErrReader(errors.New("disk on fire")))
	err := c.Count(make(Counts))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errors.Unwrap(err)) {
		t.Fatalf("err = %v, expected wrapped sentinel", err)
	}
}

func TestCounterReusesScannerError(t *testing.T) {
	t.Parallel()

	// iotest.ErrReader(io.EOF) is a reader that immediately signals EOF with no
	// data. bufio.Scanner treats io.EOF as a clean end-of-input (scanner.Err()
	// returns nil), so Count sees zero words and returns ErrEmptyReader.
	c := NewCounter(iotest.ErrReader(io.EOF))
	if err := c.Count(make(Counts)); !errors.Is(err, ErrEmptyReader) {
		t.Fatalf("err = %v, want ErrEmptyReader", err)
	}
}
```

### Exercise 4: Test The Custom Split

Create `wordcount/split_test.go`:

```go
package wordcount

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func TestSplitSemiTokens(t *testing.T) {
	t.Parallel()

	sc := bufio.NewScanner(strings.NewReader("key1=val1;key2=val2;key3=val3"))
	sc.Split(SplitSemi)

	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []string{"key1=val1", "key2=val2", "key3=val3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestSplitSemiTrailingDelimiter(t *testing.T) {
	t.Parallel()

	// SplitSemi returns non-empty trimmed tokens only: a trailing ';' does not
	// produce an empty final token. "a;b;" yields ["a", "b"].
	sc := bufio.NewScanner(strings.NewReader("a;b;"))
	sc.Split(SplitSemi)

	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestSplitSemiEmptyInput(t *testing.T) {
	t.Parallel()

	sc := bufio.NewScanner(strings.NewReader(""))
	sc.Split(SplitSemi)

	count := 0
	for sc.Scan() {
		count++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestSplitSemiSingleToken(t *testing.T) {
	t.Parallel()

	sc := bufio.NewScanner(strings.NewReader("only"))
	sc.Split(SplitSemi)

	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []string{"only"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}
```

The four tests cover the four states a custom `SplitFunc` has to handle: a delimiter in the middle, a trailing delimiter, an empty input, and a single token with no delimiter.

### Exercise 5: The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"example.com/bufiotx/wordcount"
)

func main() {
	text := "The quick brown fox jumps over the lazy dog. " +
		"The dog barks; the fox runs. Words, words, words!"

	fmt.Println("--- Word frequency ---")
	c := wordcount.NewCounter(strings.NewReader(text))
	totals := make(wordcount.Counts)
	if err := c.Count(totals); err != nil {
		fmt.Println("count:", err)
		os.Exit(1)
	}
	printTopN(totals, 5)

	fmt.Println("--- Semi-colon split ---")
	sc := bufio.NewScanner(strings.NewReader("session=abc;theme=dark;lang=en"))
	sc.Split(wordcount.SplitSemi)
	for sc.Scan() {
		fmt.Printf("token=%q\n", sc.Text())
	}
	if err := sc.Err(); err != nil {
		fmt.Println("scan:", err)
	}
}

func printTopN(totals wordcount.Counts, n int) {
	type pair struct {
		word string
		n    int
	}
	pairs := make([]pair, 0, len(totals))
	for w, c := range totals {
		pairs = append(pairs, pair{w, c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].word < pairs[j].word
	})
	for i, p := range pairs {
		if i >= n {
			break
		}
		fmt.Printf("  %d. %s = %d\n", i+1, p.word, p.n)
	}
}
