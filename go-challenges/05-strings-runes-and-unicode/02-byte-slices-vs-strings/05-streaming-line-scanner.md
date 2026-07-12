# Exercise 5: Streaming Log Scanner Without Per-Line Allocation

A log-ingestion worker reads a byte stream line by line, splits each line into a
key and value, and keeps only some of them. This exercise builds that scanner on
`bufio.Scanner` and `bytes.Cut`, converting to `string` only where a token is
actually retained — and it makes the retained-token hazard concrete:
`Scanner.Bytes()` aliases the scanner's internal buffer and must be copied before it
outlives the next `Scan()`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
logscan/                    independent module: example.com/logscan
  go.mod                    go 1.25
  logscan.go                Pair; ScanPairs (copy-on-retain), CollectKeysBuggy (alias demo)
  cmd/
    demo/
      main.go               scans a multi-line log and prints parsed pairs
  logscan_test.go           parse cases, retained-token regression, big-line ErrTooLong
```

- Files: `logscan.go`, `cmd/demo/main.go`, `logscan_test.go`.
- Implement: `ScanPairs(io.Reader) ([]Pair, error)` splitting each `key=value` line with `bytes.Cut`, copying tokens before retaining them; a deliberately-buggy alias variant to contrast; and a large-line reader using `Scanner.Buffer`.
- Test: parse a multi-line reader; a regression proving the copy-on-retain path yields correct values where a naive alias path would corrupt; a line over the default 64 KiB limit that trips `ErrTooLong`, then succeeds with an enlarged `Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/02-byte-slices-vs-strings/05-streaming-line-scanner/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/02-byte-slices-vs-strings/05-streaming-line-scanner
go mod edit -go=1.25
```

### Why Scanner.Bytes aliases, and what that costs you

`bufio.Scanner` is built for exactly this job: read a large stream one line at a
time without allocating a slice per line. It achieves that by handing you a *view*
into its own internal buffer — `Scanner.Bytes()` returns a slice backed by the
scanner's storage, valid only until the next `Scan()`. The next `Scan()` may
overwrite that same memory with the following line. This is a deliberate, documented
performance decision, and it is a trap the moment you retain a token past the next
read: store `sc.Bytes()` in a slice or map field and keep scanning, and your stored
"token" silently becomes whatever line came later.

The rule that falls out: a token you *consume* before the next `Scan()` can alias
for free; a token you *keep* must be copied first. The copy idioms are
`bytes.Clone(tok)`, `append([]byte(nil), tok...)`, or `string(tok)` (which copies as
a side effect). `ScanPairs` retains its parsed keys and values in a returned slice,
so it copies — it splits each line with `bytes.Cut` (the `[]byte` twin of
`strings.Cut`), trims with `bytes.TrimSpace`, and converts each kept token to a
`string`, which is both the copy and the ownership boundary. The parsed `Pair`s own
their strings and outlive the scanner safely.

`CollectKeysBuggy` exists to make the hazard visible: it stores the raw
`sc.Bytes()` slice without copying and returns them. Because they all alias the same
buffer, after the scan they all read as the *last* line — the regression test
demonstrates exactly this corruption, then shows the copying `ScanPairs` producing
the correct distinct values.

The last piece is line size. `bufio.Scanner` caps a token at 64 KiB by default and
returns `bufio.ErrTooLong` (via `Scanner.Err()`) if a line exceeds it — a sane
guard against a malformed stream exhausting memory. When you legitimately expect
long lines (a big JSON log record), raise the cap with `Scanner.Buffer(buf, max)`
*before* the first `Scan()`. `ReadFirstLine` shows both: with the default buffer a
huge line trips `ErrTooLong`; with an enlarged buffer it reads.

Create `logscan.go`:

```go
package logscan

import (
	"bufio"
	"bytes"
	"io"
)

// Pair is a parsed key=value entry. Its fields are owned strings, safe to retain
// past the scan that produced them.
type Pair struct {
	Key   string
	Value string
}

// ScanPairs reads r line by line, splitting each non-empty line on the first '='
// into a trimmed key and value. Tokens are converted to string (a copy) before
// being retained, so the returned pairs do not alias the scanner's buffer.
func ScanPairs(r io.Reader) ([]Pair, error) {
	sc := bufio.NewScanner(r)
	var pairs []Pair
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		k, v, found := bytes.Cut(line, []byte("="))
		if !found {
			continue
		}
		pairs = append(pairs, Pair{
			Key:   string(bytes.TrimSpace(k)),
			Value: string(bytes.TrimSpace(v)),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return pairs, nil
}

// CollectKeysBuggy is a DELIBERATELY BROKEN contrast to ScanPairs: it retains the
// raw sc.Bytes() slices without copying. Because every returned slice aliases the
// scanner's single internal buffer, after scanning they all read as the LAST line.
// Never do this; the regression test proves the corruption.
func CollectKeysBuggy(r io.Reader) [][]byte {
	sc := bufio.NewScanner(r)
	var keys [][]byte
	for sc.Scan() {
		k, _, found := bytes.Cut(sc.Bytes(), []byte("="))
		if !found {
			continue
		}
		keys = append(keys, k) // BUG: aliases the scanner buffer, no copy
	}
	return keys
}

// ReadFirstLine returns the first line of r. maxToken sets the scanner's buffer
// cap; a line longer than it yields bufio.ErrTooLong via Err.
func ReadFirstLine(r io.Reader, maxToken int) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxToken)
	if sc.Scan() {
		return string(sc.Bytes()), nil
	}
	return "", sc.Err()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/logscan"
)

func main() {
	log := strings.NewReader(
		"level = info\n" +
			"msg=started\n" +
			"\n" +
			"user = alice\n" +
			"latency_ms= 12\n",
	)
	pairs, err := logscan.ScanPairs(log)
	if err != nil {
		fmt.Println("scan error:", err)
		return
	}
	for _, p := range pairs {
		fmt.Printf("%s -> %s\n", p.Key, p.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level -> info
msg -> started
user -> alice
latency_ms -> 12
```

### Tests

`TestScanPairs` parses a multi-line reader with blank lines and stray spaces and
asserts the trimmed pairs. `TestRetainedTokenRegression` is the lesson's core: it
runs the buggy alias collector and the correct `ScanPairs` over the same input and
shows the buggy one returning duplicated (corrupted) keys while `ScanPairs` returns
the distinct correct ones. `TestBigLine` feeds a line over 64 KiB: with a small
buffer cap it returns `bufio.ErrTooLong` (asserted with `errors.Is`), and with an
enlarged cap it reads the whole line.

Create `logscan_test.go`:

```go
package logscan

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestScanPairs(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("a = 1\nb=2\n\n  c =  three  \nnokv\n")
	got, err := ScanPairs(in)
	if err != nil {
		t.Fatalf("ScanPairs: %v", err)
	}
	want := []Pair{{"a", "1"}, {"b", "2"}, {"c", "three"}}
	if len(got) != len(want) {
		t.Fatalf("got %d pairs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pair %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRetainedTokenRegression(t *testing.T) {
	t.Parallel()
	// Values large enough (40 KiB each, still under the 64 KiB cap) that the
	// scanner shifts its buffer between lines, which overwrites the memory the
	// buggy collector's retained slices alias. This makes the corruption
	// deterministic rather than dependent on a small fully-buffered input.
	big := "k1=" + strings.Repeat("a", 40000) +
		"\nk2=" + strings.Repeat("b", 40000) +
		"\nk3=" + strings.Repeat("c", 40000) + "\n"

	// Buggy: retained sc.Bytes() slices alias one buffer, so after the scan the
	// keys are corrupted (they no longer read as three distinct values).
	buggy := CollectKeysBuggy(strings.NewReader(big))
	if len(buggy) != 3 {
		t.Fatalf("buggy: got %d keys, want 3", len(buggy))
	}
	distinct := map[string]struct{}{}
	for _, k := range buggy {
		distinct[string(k)] = struct{}{}
	}
	if len(distinct) == 3 {
		t.Fatalf("buggy collector unexpectedly kept 3 distinct keys %v; aliasing not observed", buggy)
	}

	// Correct: ScanPairs copies to string on retain, so the keys are distinct.
	pairs, err := ScanPairs(strings.NewReader(big))
	if err != nil {
		t.Fatalf("ScanPairs: %v", err)
	}
	gotKeys := []string{pairs[0].Key, pairs[1].Key, pairs[2].Key}
	wantKeys := []string{"k1", "k2", "k3"}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("ScanPairs key %d = %q, want %q", i, gotKeys[i], wantKeys[i])
		}
	}
}

func TestBigLine(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 100*1024) + "\n" // 100 KiB, over the 64 KiB default

	if _, err := ReadFirstLine(strings.NewReader(big), 64*1024); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("small buffer: err = %v, want bufio.ErrTooLong", err)
	}

	line, err := ReadFirstLine(strings.NewReader(big), 1<<20) // 1 MiB cap
	if err != nil {
		t.Fatalf("enlarged buffer: unexpected err %v", err)
	}
	if len(line) != 100*1024 {
		t.Fatalf("enlarged buffer: read %d bytes, want %d", len(line), 100*1024)
	}
}
```

## Review

The scanner is correct when its parsed pairs own their bytes and survive the scan,
which `ScanPairs` guarantees by converting each retained token to a `string`. The
regression test is the whole point of the exercise: `CollectKeysBuggy` retains
`sc.Bytes()` without copying, and once the scanner reuses its buffer those retained
slices all read as the last line — the exact corruption that copy-on-retain
prevents. If you take one thing from this module, it is that `Scanner.Bytes()` (and
`Reader.Peek`) are valid only until the next read.

The line-size handling is the operational guard. The default 64 KiB cap is a
feature, not a limitation — it stops a malformed stream from exhausting memory — and
`Scanner.Buffer` raises it deliberately when you expect long records, which
`TestBigLine` pins in both directions with `errors.Is(err, bufio.ErrTooLong)`. Set
the cap to bound the largest legitimate line, not higher.

## Resources

- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Bytes` aliasing, `Buffer`, and `ErrTooLong`.
- [`bytes.Cut`](https://pkg.go.dev/bytes#Cut) — the `[]byte` split used to parse each line.
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — the copy-on-retain helper.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-bytes-package-header-parser.md](06-bytes-package-header-parser.md)
