# Exercise 8: Zero-Alloc Wire Encoder with the Append Pattern

A metrics agent emits thousands of statsd lines per second. Building each line with
`fmt.Sprintf` reflects over its arguments and allocates a fresh string every call —
pure garbage on a hot path. This exercise builds a statsd line encoder on the Append
pattern: format each record into a caller-owned, reused `[]byte` with
`strconv.AppendInt`, `strconv.AppendFloat`, and friends, so a warm encoder emits at
zero allocations per record.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
statsd/                     independent module: example.com/statsd
  go.mod                    go 1.25
  statsd.go                 Metric/Kind; AppendMetric, Encoder.Encode (reused buffer), ParseLine
  cmd/
    demo/
      main.go               encodes a batch of metrics, prints the wire bytes
  statsd_test.go            golden output, round-trip, warm zero-alloc, Sprintf benchmark
```

- Files: `statsd.go`, `cmd/demo/main.go`, `statsd_test.go`.
- Implement: `AppendMetric([]byte, Metric) []byte` formatting `name:value|type[|#tags]` with the Append family; an `Encoder` that reuses one buffer across a batch; and a `ParseLine` for the round-trip.
- Test: golden byte output for counter/gauge/timing and a quoted tag; a round-trip through `ParseLine`; an `AllocsPerRun` assertion that a warm encoder hits 0 allocs; a benchmark against a `fmt.Sprintf` baseline.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/02-byte-slices-vs-strings/08-append-encoder-hot-path/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/02-byte-slices-vs-strings/08-append-encoder-hot-path
go mod edit -go=1.25
```

### Format into a reused buffer, never into a fresh string

`fmt.Sprintf("%s:%d|c", name, v)` does three costly things per call: it reflects
over the argument types, it walks a format string, and it allocates a new string for
the result. On a loop that emits every few microseconds, that allocation is the
profile. The Append pattern removes all three. Each `AppendX` function takes a
destination `[]byte`, formats one value onto its end, and returns the extended
slice — no reflection, no format string, no intermediate string. `AppendMetric`
builds a full line by appending the name (`append(dst, m.Name...)`), a colon, the
value (`strconv.AppendInt` for a counter or timing, `strconv.AppendFloat` for a
gauge), a pipe, and the type suffix.

The `Encoder` is where the allocation actually goes to zero. It owns one `[]byte`.
`Encode` resets its *length* with `e.buf = e.buf[:0]` — keeping the capacity — then
appends every metric into it. After the first batch grows the buffer to the needed
size, every later batch of similar size reuses that same backing array and allocates
nothing. The separator between lines is a configurable `rune` appended with
`utf8.AppendRune`, which correctly encodes any rune (not just ASCII) into the buffer;
for the default `'\n'` it writes one byte, but the API generalizes to a multi-byte
record separator without special-casing.

Tags show the last member of the family. A tag value that contains a delimiter (a
comma, a pipe, a space) must be quoted so a parser does not mis-split it;
`appendTag` uses `strconv.AppendQuote` for those and a plain `append` otherwise.
`AppendQuote` writes an escaped, double-quoted form directly into the buffer — the
Append-pattern equivalent of `strconv.Quote`, with no intermediate string.

`ParseLine` reads a line back with the `bytes` package (`bytes.Cut` on `:` and `|`)
so the round-trip test can prove the encoder's output parses to the metrics it was
given.

Create `statsd.go`:

```go
package statsd

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is the statsd metric type.
type Kind int

const (
	Counter Kind = iota // "c"
	Gauge               // "g"
	Timing              // "ms"
)

func (k Kind) suffix() string {
	switch k {
	case Gauge:
		return "g"
	case Timing:
		return "ms"
	default:
		return "c"
	}
}

// Metric is one statsd sample. I carries the value for Counter/Timing, F for Gauge.
type Metric struct {
	Name string
	Kind Kind
	I    int64
	F    float64
	Tags []string // optional "key:value" tags
}

// AppendMetric formats m as name:value|type[|#tag,tag] onto dst and returns the
// extended slice. It never allocates an intermediate string.
func AppendMetric(dst []byte, m Metric) []byte {
	dst = append(dst, m.Name...)
	dst = append(dst, ':')
	if m.Kind == Gauge {
		dst = strconv.AppendFloat(dst, m.F, 'g', -1, 64)
	} else {
		dst = strconv.AppendInt(dst, m.I, 10)
	}
	dst = append(dst, '|')
	dst = append(dst, m.Kind.suffix()...)
	if len(m.Tags) > 0 {
		dst = append(dst, "|#"...)
		for i, tag := range m.Tags {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendTag(dst, tag)
		}
	}
	return dst
}

// appendTag appends tag, quoting it (via the Append-pattern strconv.AppendQuote)
// when it contains a delimiter that would otherwise break parsing.
func appendTag(dst []byte, tag string) []byte {
	if strings.ContainsAny(tag, ", |\n") {
		return strconv.AppendQuote(dst, tag)
	}
	return append(dst, tag...)
}

// Encoder formats batches of metrics into one reused buffer. A warm Encoder emits
// at zero allocations per batch.
type Encoder struct {
	buf []byte
	sep rune
}

// NewEncoder returns an Encoder separating lines with '\n'.
func NewEncoder() *Encoder { return &Encoder{sep: '\n'} }

// Encode formats every metric into the reused buffer and returns it. The returned
// slice aliases the Encoder's storage and is valid only until the next Encode.
func (e *Encoder) Encode(ms []Metric) []byte {
	e.buf = e.buf[:0]
	for _, m := range ms {
		e.buf = AppendMetric(e.buf, m)
		e.buf = utf8.AppendRune(e.buf, e.sep)
	}
	return e.buf
}

// ParseLine parses one encoded line (without the trailing separator) back into a
// Metric. Tags are ignored; it exists for the round-trip test.
func ParseLine(line []byte) (Metric, error) {
	name, rest, ok := bytes.Cut(line, []byte(":"))
	if !ok {
		return Metric{}, fmt.Errorf("no ':' in %q", line)
	}
	valB, typB, ok := bytes.Cut(rest, []byte("|"))
	if !ok {
		return Metric{}, fmt.Errorf("no '|' in %q", line)
	}
	// Drop any trailing |#tags on the type field.
	typB, _, _ = bytes.Cut(typB, []byte("|"))
	m := Metric{Name: string(name)}
	switch string(typB) {
	case "c", "ms":
		if string(typB) == "ms" {
			m.Kind = Timing
		} else {
			m.Kind = Counter
		}
		v, err := strconv.ParseInt(string(valB), 10, 64)
		if err != nil {
			return Metric{}, err
		}
		m.I = v
	case "g":
		m.Kind = Gauge
		v, err := strconv.ParseFloat(string(valB), 64)
		if err != nil {
			return Metric{}, err
		}
		m.F = v
	default:
		return Metric{}, fmt.Errorf("unknown type %q", typB)
	}
	return m, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statsd"
)

func main() {
	enc := statsd.NewEncoder()
	batch := []statsd.Metric{
		{Name: "api.requests", Kind: statsd.Counter, I: 3},
		{Name: "api.latency", Kind: statsd.Timing, I: 42},
		{Name: "queue.depth", Kind: statsd.Gauge, F: 12.5},
		{Name: "api.requests", Kind: statsd.Counter, I: 1, Tags: []string{"env:prod", "region:us east"}},
	}
	fmt.Print(string(enc.Encode(batch)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api.requests:3|c
api.latency:42|ms
queue.depth:12.5|g
api.requests:1|c|#env:prod,"region:us east"
```

### Tests

`TestGolden` asserts the exact bytes for a batch covering all three kinds and a
quoted tag. `TestRoundTrip` encodes a batch, splits it on the separator, parses each
line with `ParseLine`, and checks the decoded metrics equal the originals.
`TestWarmZeroAlloc` pins the operational claim: after the buffer is warm, `Encode`
allocates nothing. `BenchmarkAppendVsSprintf` contrasts the encoder with a
`fmt.Sprintf` baseline under `-benchmem`.

Create `statsd_test.go`:

```go
package statsd

import (
	"bytes"
	"fmt"
	"testing"
)

func TestGolden(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	batch := []Metric{
		{Name: "api.requests", Kind: Counter, I: 3},
		{Name: "api.latency", Kind: Timing, I: 42},
		{Name: "queue.depth", Kind: Gauge, F: 12.5},
		{Name: "api.requests", Kind: Counter, I: 1, Tags: []string{"env:prod", "region:us east"}},
	}
	want := "api.requests:3|c\n" +
		"api.latency:42|ms\n" +
		"queue.depth:12.5|g\n" +
		"api.requests:1|c|#env:prod,\"region:us east\"\n"
	if got := string(enc.Encode(batch)); got != want {
		t.Fatalf("Encode:\n got %q\nwant %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	in := []Metric{
		{Name: "a.count", Kind: Counter, I: 7},
		{Name: "a.time", Kind: Timing, I: 130},
		{Name: "a.gauge", Kind: Gauge, F: 0.25},
		{Name: "neg", Kind: Counter, I: -4},
	}
	enc := NewEncoder()
	out := enc.Encode(in)
	lines := bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n"))
	if len(lines) != len(in) {
		t.Fatalf("got %d lines, want %d", len(lines), len(in))
	}
	for i, line := range lines {
		m, err := ParseLine(line)
		if err != nil {
			t.Fatalf("ParseLine(%q): %v", line, err)
		}
		if m.Name != in[i].Name || m.Kind != in[i].Kind || m.I != in[i].I || m.F != in[i].F {
			t.Fatalf("line %d = %+v, want %+v", i, m, in[i])
		}
	}
}

func TestWarmZeroAlloc(t *testing.T) {
	// No t.Parallel(): AllocsPerRun must not run under a parallel test.
	enc := NewEncoder()
	batch := []Metric{
		{Name: "api.requests", Kind: Counter, I: 3},
		{Name: "api.latency", Kind: Timing, I: 42},
		{Name: "queue.depth", Kind: Gauge, F: 12.5},
	}
	allocs := testing.AllocsPerRun(1000, func() {
		_ = enc.Encode(batch)
	})
	if allocs != 0 {
		t.Fatalf("warm Encode allocated %.2f/op; want 0", allocs)
	}
}

func BenchmarkAppendVsSprintf(b *testing.B) {
	batch := []Metric{
		{Name: "api.requests", Kind: Counter, I: 3},
		{Name: "api.latency", Kind: Timing, I: 42},
		{Name: "queue.depth", Kind: Gauge, F: 12.5},
	}
	b.Run("append_reused_buffer", func(b *testing.B) {
		enc := NewEncoder()
		b.ReportAllocs()
		for range b.N {
			_ = enc.Encode(batch)
		}
	})
	b.Run("sprintf_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			var s string
			for _, m := range batch {
				switch m.Kind {
				case Gauge:
					s += fmt.Sprintf("%s:%g|g\n", m.Name, m.F)
				case Timing:
					s += fmt.Sprintf("%s:%d|ms\n", m.Name, m.I)
				default:
					s += fmt.Sprintf("%s:%d|c\n", m.Name, m.I)
				}
			}
			_ = s
		}
	})
}
```

## Review

The encoder is correct when its bytes are exactly the statsd wire form — pinned by
`TestGolden` down to the quoted tag — and when those bytes parse back to the metrics
they came from, pinned by `TestRoundTrip`. The float format matters: `AppendFloat`
with `'g'` and precision `-1` produces the shortest representation that round-trips,
so `12.5` stays `12.5` and does not drift.

The operational point is `TestWarmZeroAlloc`: once the reused buffer is warm,
`Encode` allocates nothing, while the `fmt.Sprintf` baseline allocates on every line.
The pattern is `buf = buf[:0]` to reset length while keeping capacity, then append.
The one caveat that comes with reuse: `Encode` returns a slice aliasing the
Encoder's storage, valid only until the next `Encode` — write it to the socket
before you encode the next batch, or copy it if you must hold it.

## Resources

- [`strconv` Append functions](https://pkg.go.dev/strconv#AppendInt) — `AppendInt`, `AppendFloat`, `AppendQuote`.
- [`utf8.AppendRune`](https://pkg.go.dev/unicode/utf8#AppendRune) — appending any rune into a byte buffer.
- [StatsD metric types](https://github.com/statsd/statsd/blob/master/docs/metric_types.md) — the wire format this encoder emits.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-inplace-log-redaction.md](09-inplace-log-redaction.md)
