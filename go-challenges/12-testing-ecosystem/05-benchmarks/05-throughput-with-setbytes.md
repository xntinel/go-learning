# Exercise 5: Report Throughput (MB/s) with b.SetBytes

For code whose cost is bound by data volume — parsers, encoders, compressors, I/O —
`ns/op` is the wrong unit. What you care about is throughput: megabytes per second.
`b.SetBytes(n)` tells the framework each operation processed `n` bytes and it reports
an `MB/s` column. This module benchmarks two data-volume paths from a log pipeline: a
line parser built on `bufio.Scanner`, and a gzip compressor, each reporting MB/s.

## What you'll build

```text
logpipe/                   independent module: example.com/logpipe
  go.mod                   go 1.24
  logpipe.go               CountLevels([]byte) map[string]int (bufio.Scanner);
                           Compress([]byte) ([]byte, error) (gzip)
  cmd/
    demo/
      main.go              runnable demo: parse a small log, compress a payload
  logpipe_test.go          parse correctness, gzip round-trip; BenchmarkParse and
                           BenchmarkCompress with SetBytes; SetBytes-value assertion; Example
```

- Files: `logpipe.go`, `cmd/demo/main.go`, `logpipe_test.go`.
- Implement: `CountLevels` (scan lines, tally the level token) and `Compress` (gzip).
- Test: parse correctness, gzip round-trip, and two throughput benchmarks using `SetBytes`.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
mkdir -p ~/go-exercises/logpipe/cmd/demo
cd ~/go-exercises/logpipe
go mod init example.com/logpipe
go mod edit -go=1.24
```

### Why throughput is the right unit here

`CountLevels` scans a byte slice line by line with a `bufio.Scanner`, reads the second
whitespace-separated token as the log level (`INFO`, `WARN`, `ERROR`), and tallies
each. `Compress` gzips a payload. Both do work proportional to the number of *bytes*
they consume, so the natural question is not "how many nanoseconds per call" — a call
over a 10 KB payload and a call over a 10 MB payload are not comparable in `ns/op` —
but "how many megabytes per second can it push". `b.SetBytes(int64(len(payload)))`
converts the measurement: the framework divides bytes-per-op by seconds-per-op and
prints `MB/s`. Now the number is size-independent and directly comparable to a disk's
sequential read speed or a network link's bandwidth, which is how you reason about
whether a parser can keep up with an ingest rate.

The rule for `SetBytes`: pass the number of bytes *one operation* processes. In the
`b.Loop` form each iteration processes the whole payload, so `SetBytes(len(payload))`
is correct and is called once before the loop. Getting that value wrong (passing the
compressed size, or the number of lines) silently scales the MB/s by the wrong factor,
which is why the test asserts the payload length explicitly as documentation of intent.

`Compress` returns `(bytes, error)` and the benchmark checks the error inside the loop
with `b.Fatal` on failure — a compressor that errors mid-benchmark must fail loudly,
not silently skew the number.

Create `logpipe.go`:

```go
package logpipe

import (
	"bufio"
	"bytes"
	"compress/gzip"
)

// CountLevels scans data line by line and tallies the log level, which it reads as
// the second whitespace-separated token of each non-empty line. Lines with fewer
// than two tokens are ignored.
func CountLevels(data []byte) map[string]int {
	counts := make(map[string]int)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := bytes.Fields(sc.Bytes())
		if len(fields) < 2 {
			continue
		}
		counts[string(fields[1])]++
	}
	return counts
}

// Compress returns the gzip-compressed form of data.
func Compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logpipe"
)

func main() {
	log := []byte("2026-07-02 INFO started\n2026-07-02 WARN slow query\n2026-07-02 INFO handled\n")
	counts := logpipe.CountLevels(log)
	fmt.Printf("INFO=%d WARN=%d ERROR=%d\n", counts["INFO"], counts["WARN"], counts["ERROR"])

	comp, err := logpipe.Compress(log)
	if err != nil {
		panic(err)
	}
	fmt.Printf("compressed %d bytes -> %d bytes\n", len(log), len(comp))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the gzip size is deterministic for this input and Go's default level):

```
INFO=2 WARN=1 ERROR=0
compressed 75 bytes -> 76 bytes
```

Compressing a tiny input barely helps or slightly grows it — gzip has fixed header and
trailer overhead, so 75 bytes becomes 76. Compression only pays off at scale, which is
exactly why the benchmark uses a large payload.

### Tests

`TestCountLevels` checks the tally on a fixed log. `TestCompressRoundTrip` gzips a
payload and decompresses it, asserting the bytes survive — a round-trip is the honest
correctness check for a compressor, not an exact output-length assertion (which is
brittle across Go versions). The benchmarks build a large payload, call
`SetBytes(len(payload))`, and loop; `TestSetBytesValue` documents that the value passed
is the uncompressed payload length.

Create `logpipe_test.go`:

```go
package logpipe

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCountLevels(t *testing.T) {
	t.Parallel()
	log := []byte("t INFO a\nt WARN b\nt INFO c\n\nt ERROR d\n")
	got := CountLevels(log)
	want := map[string]int{"INFO": 2, "WARN": 1, "ERROR": 1}
	for level, n := range want {
		if got[level] != n {
			t.Errorf("count[%s] = %d, want %d", level, got[level], n)
		}
	}
}

func TestCompressRoundTrip(t *testing.T) {
	t.Parallel()
	src := bytes.Repeat([]byte("the quick brown fox\n"), 500)
	comp, err := Compress(src)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	r, err := gzip.NewReader(bytes.NewReader(comp))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Fatal("round trip changed the payload")
	}
}

// payload is a realistic multi-line log used by both throughput benchmarks.
func payload() []byte {
	var b strings.Builder
	for i := range 2000 {
		level := "INFO"
		if i%5 == 0 {
			level = "WARN"
		}
		fmt.Fprintf(&b, "2026-07-02T10:00:00Z %s request %d handled in %dms\n", level, i, i%40)
	}
	return []byte(b.String())
}

func TestSetBytesValue(t *testing.T) {
	t.Parallel()
	p := payload()
	// The value handed to SetBytes must be the uncompressed bytes processed per op.
	if int64(len(p)) <= 0 {
		t.Fatal("payload is empty; SetBytes would report 0 MB/s")
	}
}

func BenchmarkParse(b *testing.B) {
	p := payload()
	b.ReportAllocs()
	b.SetBytes(int64(len(p))) // report MB/s over the uncompressed input
	for b.Loop() {
		_ = CountLevels(p)
	}
}

func BenchmarkCompress(b *testing.B) {
	p := payload()
	b.ReportAllocs()
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		if _, err := Compress(p); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleCountLevels() {
	counts := CountLevels([]byte("t INFO a\nt ERROR b\n"))
	fmt.Println(counts["INFO"], counts["ERROR"])
	// Output: 1 1
}
```

Run the benchmarks; the output now carries an MB/s column:

```bash
go test -bench=. -benchmem
```

```text
BenchmarkParse-8         254      206200 ns/op    525.7 MB/s   364445 B/op   4004 allocs/op
BenchmarkCompress-8      216      282073 ns/op    384.3 MB/s   830102 B/op     25 allocs/op
PASS
```

The parser's high `allocs/op` is real: `string(fields[1])` allocates a fresh string per
line to use as a map key. That is itself a benchmark-surfaced optimization target — a
`map[string]int` keyed on a byte-slice-derived string reallocates each line, and a
senior engineer reading this would consider interning levels or counting into a fixed
set of `INFO`/`WARN`/`ERROR` counters to drive it toward zero allocations.

## Review

Correctness is the level tally and the gzip round-trip; both are exact and robust,
and the round-trip deliberately avoids asserting a compressed size, which drifts with
the compression library. The benchmark lesson is the unit: with `SetBytes` the parser
and the compressor report `MB/s`, and that number — unlike `ns/op` — you can compare
directly against an ingest requirement ("we receive 200 MB/s of logs; does the parser
keep up?"). The demo's counterintuitive "75 bytes -> 76 bytes" is the honest face of
compression overhead at small sizes and reinforces why throughput benchmarks must use
a realistically large payload. The common `SetBytes` bug is passing the wrong byte
count (compressed size, line count); the number it produces looks plausible but is off
by a constant factor, so make the value obviously the uncompressed input length, as
here.

## Resources

- [`testing.B.SetBytes`](https://pkg.go.dev/testing#B.SetBytes) — report throughput in MB/s for data-volume-bound benchmarks.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line scanner the parser is built on.
- [`compress/gzip`](https://pkg.go.dev/compress/gzip) — `NewWriter`/`NewReader` for the compress round-trip.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-exclude-setup-with-timers.md](04-exclude-setup-with-timers.md) | Next: [06-contention-with-runparallel.md](06-contention-with-runparallel.md)
