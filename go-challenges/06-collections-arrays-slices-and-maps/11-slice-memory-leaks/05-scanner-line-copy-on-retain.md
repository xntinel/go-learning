# Exercise 5: Aggregate Log Lines From bufio.Scanner Without Aliasing Its Buffer

A log ingest step scans lines off a reader, keeps the ones matching a predicate,
and holds them for later shipping. `bufio.Scanner.Bytes()` returns a slice into the
scanner's own buffer, valid only until the next `Scan` — the moment the buffer
refills or shifts, every retained slice reads corrupted data and pins the buffer.
This module builds the collector correctly (clone each retained line into a
bounded ring) and reproduces the aliasing corruption deterministically to prove
the contract.

## What you'll build

```text
logcollect/                  independent module: example.com/logcollect
  go.mod                     go 1.24
  logcollect.go              type Collector; New, Collect (clone), CollectBuggy, Lines
  cmd/
    demo/
      main.go                keep ERROR lines into a bounded ring, print them
  logcollect_test.go         clone-correct, buggy-corrupts, bounded-ring, predicate; -race
```

Files: `logcollect.go`, `cmd/demo/main.go`, `logcollect_test.go`.
Implement: a `Collector` with a bounded ring; `Collect` clones each kept line, `CollectBuggy` retains `Scanner.Bytes()` directly.
Test: assert `Collect` matches the input lines and `CollectBuggy` corrupts (forced by a small scanner buffer), and that the ring keeps only the last N.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Why retaining Scanner.Bytes corrupts and pins

`bufio.Scanner` reads into a single reusable buffer. Each `Scan` finds the next
token and `Bytes()` returns a sub-slice of that buffer pointing at the token. The
documented contract is blunt: the slice "may be overwritten by a subsequent call
to Scan." While the whole input fits in the buffer, retained slices at distinct
offsets happen to survive — which is exactly what makes this bug hide in tests on
small inputs. The moment the input exceeds the buffer and the scanner slides
unread data down to refill, the offsets your retained slices point at are reused
for later lines. Every retained line silently becomes some other line's bytes.

There are two failures in one here. The obvious one is *corruption*: the retained
data is wrong. The quiet one is a *pin*: every retained slice points into the
scanner's buffer, so the buffer cannot be freed while any retained line lives — and
if you keep the lines, you keep the buffer.

The fix is a copy at the point of retention. `Collect` calls `slices.Clone(line)`
before storing, giving each kept line its own small array that neither aliases nor
pins the scanner buffer. (`Scanner.Text()` is the other correct option; it
allocates a fresh string.) `CollectBuggy` stores `Scanner.Bytes()` directly and
exists only to reproduce the corruption.

Two more production details are baked in. The scanner buffer is *bounded*
(`sc.Buffer` with a max line size), because a log line from an untrusted source is
attacker-controlled length and an unbounded scanner buffer is itself a memory
risk. And the collector stores into a *bounded ring* of the last N kept lines, so
the aggregator cannot grow without bound no matter how long the stream runs — the
ring overwrites its oldest slot in place, which drops the old line's reference
without leaving a dead pointer behind.

Create `logcollect.go`:

```go
// Package logcollect aggregates matching log lines into a bounded ring, cloning
// each retained line so it does not alias the scanner's reusable buffer.
package logcollect

import (
	"bufio"
	"io"
	"slices"
)

// Collector keeps the last cap kept lines in a ring buffer.
type Collector struct {
	buf     [][]byte
	head    int
	count   int
	maxLine int
	keep    func([]byte) bool
}

// New returns a Collector holding at most ringCap kept lines, scanning with a
// maxLine-byte line limit, retaining lines for which keep returns true.
func New(ringCap, maxLine int, keep func([]byte) bool) *Collector {
	if ringCap < 1 {
		ringCap = 1
	}
	if maxLine < 1 {
		maxLine = 1
	}
	return &Collector{buf: make([][]byte, ringCap), maxLine: maxLine, keep: keep}
}

func (c *Collector) scanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, c.maxLine), c.maxLine)
	return sc
}

// Collect scans r and retains a clone of each kept line. The clone is what makes
// the retained data safe: it neither aliases nor pins the scanner's buffer.
func (c *Collector) Collect(r io.Reader) error {
	sc := c.scanner(r)
	for sc.Scan() {
		line := sc.Bytes()
		if c.keep(line) {
			c.add(slices.Clone(line))
		}
	}
	return sc.Err()
}

// CollectBuggy retains Scanner.Bytes() without copying. The retained slices alias
// the scanner buffer and corrupt once it refills. Kept only to contrast.
func (c *Collector) CollectBuggy(r io.Reader) error {
	sc := c.scanner(r)
	for sc.Scan() {
		line := sc.Bytes()
		if c.keep(line) {
			c.add(line)
		}
	}
	return sc.Err()
}

// add stores b as the newest line, overwriting the oldest slot when full.
func (c *Collector) add(b []byte) {
	if c.count < len(c.buf) {
		c.buf[(c.head+c.count)%len(c.buf)] = b
		c.count++
		return
	}
	c.buf[c.head] = b // overwrite oldest; assignment drops the old reference
	c.head = (c.head + 1) % len(c.buf)
}

// Lines returns the retained lines, oldest first.
func (c *Collector) Lines() [][]byte {
	out := make([][]byte, 0, c.count)
	for i := range c.count {
		out = append(out, c.buf[(c.head+i)%len(c.buf)])
	}
	return out
}
```

## The runnable demo

The demo scans a small log, keeps only the lines containing `ERROR` into a
three-slot ring, and prints them. Because more than three error lines arrive, the
ring holds only the most recent three.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"strings"

	"example.com/logcollect"
)

func main() {
	log := `INFO  starting up
ERROR disk full on /var
INFO  request handled
ERROR upstream timeout
ERROR db connection reset
ERROR rate limit exceeded`

	c := logcollect.New(3, 4096, func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("ERROR"))
	})
	if err := c.Collect(strings.NewReader(log)); err != nil {
		fmt.Println("collect:", err)
		return
	}

	fmt.Printf("kept %d error lines (last 3):\n", len(c.Lines()))
	for _, line := range c.Lines() {
		fmt.Printf("  %s\n", line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
kept 3 error lines (last 3):
  ERROR upstream timeout
  ERROR db connection reset
  ERROR rate limit exceeded
```

## Tests

The corruption test feeds enough lines through a deliberately small scanner buffer
(64 bytes) to force the scanner to refill and shift: `Collect` (clone) reproduces
the input exactly, while `CollectBuggy` (retain) does not, because its retained
slices were overwritten. The ring test feeds more kept lines than the ring holds
and asserts only the last N survive. All are parallel-safe.

Create `logcollect_test.go`:

```go
package logcollect

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// makeLog builds n distinct short lines and returns the joined input and wants.
func makeLog(n int) (string, [][]byte) {
	var sb strings.Builder
	want := make([][]byte, 0, n)
	for i := range n {
		line := fmt.Sprintf("line-%04d-%s", i, strings.Repeat("x", i%7))
		sb.WriteString(line)
		sb.WriteByte('\n')
		want = append(want, []byte(line))
	}
	return sb.String(), want
}

func equalLines(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestCollectCloneIsCorrect(t *testing.T) {
	t.Parallel()

	input, want := makeLog(60)
	c := New(100, 64, func([]byte) bool { return true })
	if err := c.Collect(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if !equalLines(c.Lines(), want) {
		t.Fatal("clone path did not reproduce the input lines")
	}
}

func TestCollectBuggyCorrupts(t *testing.T) {
	t.Parallel()

	input, want := makeLog(60)
	c := New(100, 64, func([]byte) bool { return true })
	if err := c.CollectBuggy(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	// The small scanner buffer forces refills, so the retained sub-slices alias
	// overwritten memory. The buggy result must NOT match the input.
	if equalLines(c.Lines(), want) {
		t.Fatal("buggy path unexpectedly matched; aliasing corruption did not occur")
	}
}

func TestBoundedRingKeepsLastN(t *testing.T) {
	t.Parallel()

	input, want := makeLog(10)
	c := New(3, 64, func([]byte) bool { return true })
	if err := c.Collect(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	got := c.Lines()
	if len(got) != 3 {
		t.Fatalf("ring kept %d lines, want 3", len(got))
	}
	if !equalLines(got, want[7:]) {
		t.Fatalf("ring kept %q, want last three %q", got, want[7:])
	}
}

func TestPredicateFilters(t *testing.T) {
	t.Parallel()

	input := "keep-1\ndrop\nkeep-2\ndrop\nkeep-3\n"
	c := New(10, 64, func(b []byte) bool { return bytes.HasPrefix(b, []byte("keep")) })
	if err := c.Collect(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("keep-1"), []byte("keep-2"), []byte("keep-3")}
	if !equalLines(c.Lines(), want) {
		t.Fatalf("predicate kept %q, want %q", c.Lines(), want)
	}
}

func ExampleCollector() {
	c := New(2, 4096, func([]byte) bool { return true })
	_ = c.Collect(strings.NewReader("a\nb\nc\n"))
	for _, line := range c.Lines() {
		fmt.Printf("%s\n", line)
	}
	// Output:
	// b
	// c
}
```

## Review

The collector is correct when `Collect` reproduces the kept lines byte-for-byte
regardless of scanner-buffer refills, and the bounded ring holds only the last N.
`TestCollectBuggyCorrupts` is the proof of the hazard: with a small scanner buffer
and enough lines to force a refill, the retain-without-copy path returns corrupted
data, while the clone path does not. The mistake this module exists to prevent is
`append(lines, sc.Bytes())` — it passes on small inputs and corrupts (and pins the
buffer) in production, where lines stream past the buffer size. Clone the line, or
use `Scanner.Text()`. Run `go test -race` to confirm the collector is safe under
concurrent scans of independent readers.

## Resources

- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Bytes` (valid only until the next `Scan`), `Text`, and `Buffer`.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the copy that severs the alias and the pin.
- [`bufio.Scanner.Buffer`](https://pkg.go.dev/bufio#Scanner.Buffer) — bounding the scanner's line size.

---

Back to [04-clip-token-out-of-request-buffer.md](04-clip-token-out-of-request-buffer.md) | Next: [06-pool-buffer-reset-cap-guard.md](06-pool-buffer-reset-cap-guard.md)
