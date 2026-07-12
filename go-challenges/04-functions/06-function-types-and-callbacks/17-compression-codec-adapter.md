# Exercise 17: Compression Codec Adapter Pattern for Algorithm Selection

**Nivel: Intermedio** — validacion rapida (un test corto).

A storage or transport layer often supports several compression algorithms
and picks one at runtime — by config, by negotiation, by what the payload
looks like. Each algorithm has a different library API; the adapter pattern
wraps every one of them behind the same pair of function types so the
storage layer never branches on "which algorithm."

## What you'll build

```text
codec/                      independent module: example.com/compression-codec-adapter
  go.mod                     go 1.24
  codec.go                    type Compressor, type Decompressor, type Codec, GzipCodec, RLECodec, type Registry
  cmd/
    demo/
      main.go                 runnable demo: round-trip both codecs, then a failed lookup
  codec_test.go                table test: round trip, empty input, long run, corrupt stream, registry lookup
```

Files: `codec.go`, `cmd/demo/main.go`, `codec_test.go`.
Implement: `type Compressor func(data []byte) ([]byte, error)`, `type Decompressor func(data []byte) ([]byte, error)`, a `Codec` struct pairing a `Name` with both, `GzipCodec()` adapting `compress/gzip`, `RLECodec()` adapting a hand-rolled run-length encoder, and a `Registry` (`map[string]Codec`) with `NewRegistry` and `Select`.
Test: gzip round trip, RLE round trip, RLE on empty input, RLE on a run longer than 255 (forces a second count/value pair), RLE rejecting a corrupt (odd-length) stream, and registry lookup for both known names plus an unknown one.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why an adapter pair of function types, and why RLE stands in for brotli/zstd

`compress/gzip` wants an `io.Writer` you write into and `Close`, then an
`io.Reader` you wrap and read from — an entirely different shape than "take
bytes, return bytes." The adapter's job is to hide that shape behind
`Compressor`/`Decompressor`, so a caller holding a `Codec` never needs to know
whether the underlying library streams, buffers, or does something else
entirely. Real brotli and zstd implementations aren't in the standard
library — they require a third-party module or cgo — which would break the
"stdlib-only" constraint of this module. `RLECodec` fills that role instead:
a small hand-rolled run-length encoder adapted into the exact same `Codec`
shape as `GzipCodec`, so the exercise demonstrates the adapter pattern (and
lets `Registry.Select` pick between two real, different algorithms) without
reaching outside the standard library.

Create `codec.go`:

```go
// Package codec adapts several compression algorithms behind one function
// type pair so callers can select an algorithm by name at runtime.
package codec

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// Compressor compresses a payload. It is the adapter surface every
// algorithm plugs into.
type Compressor func(data []byte) ([]byte, error)

// Decompressor reverses a Compressor.
type Decompressor func(data []byte) ([]byte, error)

// Codec pairs a name with its Compressor/Decompressor adapter functions.
type Codec struct {
	Name       string
	Compress   Compressor
	Decompress Decompressor
}

// GzipCodec adapts the standard library's real DEFLATE-based gzip
// implementation into the Codec shape.
func GzipCodec() Codec {
	return Codec{
		Name: "gzip",
		Compress: func(data []byte) ([]byte, error) {
			var buf bytes.Buffer
			w := gzip.NewWriter(&buf)
			if _, err := w.Write(data); err != nil {
				return nil, fmt.Errorf("gzip compress: %w", err)
			}
			if err := w.Close(); err != nil {
				return nil, fmt.Errorf("gzip compress: %w", err)
			}
			return buf.Bytes(), nil
		},
		Decompress: func(data []byte) ([]byte, error) {
			r, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("gzip decompress: %w", err)
			}
			defer r.Close()
			out, err := io.ReadAll(r)
			if err != nil {
				return nil, fmt.Errorf("gzip decompress: %w", err)
			}
			return out, nil
		},
	}
}

// RLECodec adapts a hand-rolled run-length encoder into the Codec shape. It
// stands in for algorithms like brotli or zstd, which need a third-party or
// cgo-backed module and are out of scope for a stdlib-only exercise; the
// point here is the adapter pattern, not RLE's compression ratio.
func RLECodec() Codec {
	return Codec{
		Name:       "rle",
		Compress:   rleCompress,
		Decompress: rleDecompress,
	}
}

// rleCompress encodes data as a sequence of (count byte, value byte) pairs.
// Runs longer than 255 are split across multiple pairs.
func rleCompress(data []byte) ([]byte, error) {
	var out bytes.Buffer
	for i := 0; i < len(data); {
		b := data[i]
		run := 1
		for i+run < len(data) && data[i+run] == b && run < 255 {
			run++
		}
		out.WriteByte(byte(run))
		out.WriteByte(b)
		i += run
	}
	return out.Bytes(), nil
}

func rleDecompress(data []byte) ([]byte, error) {
	if len(data)%2 != 0 {
		return nil, fmt.Errorf("rle decompress: corrupt stream (odd length %d)", len(data))
	}
	var out bytes.Buffer
	for i := 0; i < len(data); i += 2 {
		count, value := data[i], data[i+1]
		for j := byte(0); j < count; j++ {
			out.WriteByte(value)
		}
	}
	return out.Bytes(), nil
}

// Registry selects a Codec by name, the same shape a Content-Encoding
// negotiator or a config-driven pipeline would use.
type Registry map[string]Codec

// NewRegistry builds a Registry from a list of codecs, keyed by their Name.
func NewRegistry(codecs ...Codec) Registry {
	r := make(Registry, len(codecs))
	for _, c := range codecs {
		r[c.Name] = c
	}
	return r
}

// Select looks up a Codec by name.
func (r Registry) Select(name string) (Codec, error) {
	c, ok := r[name]
	if !ok {
		return Codec{}, fmt.Errorf("codec: unknown algorithm %q", name)
	}
	return c, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/compression-codec-adapter"
)

func main() {
	registry := codec.NewRegistry(codec.GzipCodec(), codec.RLECodec())
	payload := []byte("aaaaaaaaaabbbbbbbbbbccccccccccdddddddddd")

	for _, name := range []string{"gzip", "rle"} {
		c, err := registry.Select(name)
		if err != nil {
			fmt.Println("select error:", err)
			continue
		}
		compressed, err := c.Compress(payload)
		if err != nil {
			fmt.Println("compress error:", err)
			continue
		}
		restored, err := c.Decompress(compressed)
		if err != nil {
			fmt.Println("decompress error:", err)
			continue
		}
		fmt.Printf("%s: original=%d compressed=%d roundtrip=%v\n",
			c.Name, len(payload), len(compressed), string(restored) == string(payload))
	}

	if _, err := registry.Select("brotli"); err != nil {
		fmt.Println("select brotli:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
gzip: original=40 compressed=34 roundtrip=true
rle: original=40 compressed=8 roundtrip=true
select brotli: codec: unknown algorithm "brotli"
```

### Tests

Create `codec_test.go`:

```go
package codec

import (
	"bytes"
	"testing"
)

func TestGzipRoundTrip(t *testing.T) {
	t.Parallel()
	c := GzipCodec()
	original := []byte("the quick brown fox jumps over the lazy dog, repeatedly, repeatedly")

	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	restored, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", restored, original)
	}
}

func TestRLERoundTrip(t *testing.T) {
	t.Parallel()
	c := RLECodec()
	original := []byte("aaaabbbccccccccd")

	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	restored, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", restored, original)
	}
}

func TestRLEEmptyInput(t *testing.T) {
	t.Parallel()
	c := RLECodec()
	compressed, err := c.Compress(nil)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(compressed) != 0 {
		t.Fatalf("compressed empty input should be empty, got %d bytes", len(compressed))
	}
	restored, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("restored empty input should be empty, got %d bytes", len(restored))
	}
}

func TestRLELongRunSplitsAcrossPairs(t *testing.T) {
	t.Parallel()
	c := RLECodec()
	original := bytes.Repeat([]byte{'x'}, 300) // exceeds the 255 max run length

	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(compressed) != 4 { // two (count, value) pairs: 255 + 45
		t.Fatalf("expected 4 bytes (two pairs), got %d", len(compressed))
	}
	restored, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("roundtrip mismatch for long run")
	}
}

func TestRLEDecompressRejectsCorruptStream(t *testing.T) {
	t.Parallel()
	c := RLECodec()
	if _, err := c.Decompress([]byte{5}); err == nil {
		t.Fatal("expected error decompressing an odd-length stream")
	}
}

func TestRegistrySelect(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(GzipCodec(), RLECodec())

	if c, err := reg.Select("gzip"); err != nil || c.Name != "gzip" {
		t.Fatalf("Select(gzip) = %+v, %v", c, err)
	}
	if c, err := reg.Select("rle"); err != nil || c.Name != "rle" {
		t.Fatalf("Select(rle) = %+v, %v", c, err)
	}
	if _, err := reg.Select("brotli"); err == nil {
		t.Fatal("expected error selecting an unregistered codec")
	}
}
```

## Review

Every codec satisfies the same two-function shape, so `Registry.Select`
returns a value the caller can use identically regardless of which algorithm
backs it — that is the entire point of the adapter pattern. `GzipCodec`
proves the pattern works for a real, streaming-API library by wrapping
`gzip.Writer`/`gzip.Reader` into plain `[]byte`-in, `[]byte`-out functions.
`RLECodec` proves it works for a hand-written algorithm with no library at
all, and its two edge tests — the 300-byte run and the corrupt odd-length
stream — pin the two mistakes a naive RLE implementation makes: forgetting
that a single `byte` count caps a run at 255, and forgetting to validate the
stream shape before decoding it.

## Resources

- [compress/gzip](https://pkg.go.dev/compress/gzip)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [Adapter pattern (refactoring.guru)](https://refactoring.guru/design-patterns/adapter)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-batch-process-error-handler-callback.md](16-batch-process-error-handler-callback.md) | Next: [18-dns-resolver-strategy-callback.md](18-dns-resolver-strategy-callback.md)
