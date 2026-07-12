# Exercise 3: Batch Compression

Compression is the second reason to batch. A batch of similar records shares structure, so compressing the whole batch beats compressing each tiny record alone by a wide margin. This exercise builds the batch payload: a reversible record serialization, an optional gzip codec, and the decode path a broker would use to read it back.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
compression.go       CompressionCodec, RecordBatch, Payload, DecodePayload
  RecordBatch.Payload   length-prefix the records, then run the codec
  DecodePayload         reverse the codec, then split records back out
cmd/
  demo/
    main.go          compress 50 near-identical records, show the win, round-trip
compression_test.go  gzip and none round-trips, the compression win, edge cases
```

- Files: `compression.go`, `cmd/demo/main.go`, `compression_test.go`.
- Implement: `RecordBatch.Payload`, the package function `DecodePayload`, and the `CompressionCodec` type.
- Test: gzip and none both round-trip, a repetitive batch compresses below half its raw size, an empty batch round-trips, and an unknown codec errors.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/04-producer-api-batching/03-batch-compression/cmd/demo && cd go-solutions/41-capstone-message-queue/04-producer-api-batching/03-batch-compression
```

### A reversible frame, then a codec on top

Compression only earns its place if the broker can read the batch back, so the serialization has to be reversible before any codec is applied. Each record is framed length-prefixed: a 4-byte little-endian key length, the key bytes, a 4-byte little-endian value length, the value bytes. Concatenating those frames gives a self-delimiting raw payload that `DecodePayload` walks by reading a length, slicing that many bytes, and advancing, with no separators to escape and no ambiguity about where one record ends.

The codec wraps that raw payload as an opaque blob. `Payload` builds the raw frames, then either returns them as-is (`CodecNone`) or pipes them through a `gzip.Writer` (`CodecGzip`). `DecodePayload` does the mirror: for `CodecGzip` it gunzips first, for `CodecNone` it takes the bytes directly, then it parses the frames. The batch carries the codec tag so the reader knows which branch to take; in a real system the tag travels in the batch header on the wire.

### The gzip trailer is not optional

The single mistake that turns gzip into a debugging session is forgetting to `Close` the writer. A `gzip.Writer` buffers and, crucially, writes the gzip trailer, the CRC32 and the uncompressed length, only when you `Close` it. If you call `Flush` (or nothing) and read the buffer, the stream is missing its trailer and every reader, including `gzip.NewReader`, rejects it as corrupt. `Close` is therefore part of producing the bytes, not cleanup you can defer and forget; `Payload` calls it and checks its error before returning the buffer. On the read side, `gzip.NewReader` validates that trailer, so a truncated or corrupted compressed payload is caught rather than silently mis-decoded.

### Why per-batch beats per-record

Run the demo and the reason batching and compression belong together becomes concrete. Fifty near-identical JSON records share almost every byte; gzip's dictionary captures that shared structure once and refers back to it, so the batch compresses to a small fraction of its raw size. Compress those same fifty records individually and each one restarts gzip's dictionary and pays the roughly 18-byte gzip header and trailer per record, so the total is often larger than the raw input. Compression ratio is a property of the batch, which is exactly why production producers compress at the batch boundary and never per message.

Create `compression.go`:

```go
package compression

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// CompressionCodec selects the batch-level compression algorithm.
type CompressionCodec uint8

const (
	CodecNone CompressionCodec = iota // store the frames uncompressed
	CodecGzip                         // compress/gzip
)

func (c CompressionCodec) String() string {
	switch c {
	case CodecNone:
		return "none"
	case CodecGzip:
		return "gzip"
	default:
		return fmt.Sprintf("codec(%d)", uint8(c))
	}
}

// ErrMalformed reports a payload whose length prefixes do not line up.
var ErrMalformed = errors.New("compression: malformed payload")

// Record is one key/value message inside a batch.
type Record struct {
	Key   []byte
	Value []byte
}

// RecordBatch is a group of records that share one codec.
type RecordBatch struct {
	Codec   CompressionCodec
	Records []Record
}

// Payload serializes the records into length-prefixed frames and applies the
// configured codec. The returned bytes are what the broker receives.
func (b *RecordBatch) Payload() ([]byte, error) {
	raw := encodeRecords(b.Records)
	switch b.Codec {
	case CodecNone:
		return raw, nil
	case CodecGzip:
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			return nil, fmt.Errorf("compression: gzip write: %w", err)
		}
		// Close writes the gzip trailer; without it the stream is unreadable.
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("compression: gzip close: %w", err)
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("compression: unknown codec %s", b.Codec)
	}
}

// DecodePayload reverses Payload: it removes the codec, then splits the frames
// back into records.
func DecodePayload(codec CompressionCodec, data []byte) ([]Record, error) {
	var raw []byte
	switch codec {
	case CodecNone:
		raw = data
	case CodecGzip:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("compression: gzip reader: %w", err)
		}
		defer r.Close()
		raw, err = io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("compression: gzip read: %w", err)
		}
	default:
		return nil, fmt.Errorf("compression: unknown codec %s", codec)
	}
	return decodeRecords(raw)
}

func encodeRecords(records []Record) []byte {
	var buf bytes.Buffer
	var lp [4]byte
	for _, r := range records {
		binary.LittleEndian.PutUint32(lp[:], uint32(len(r.Key)))
		buf.Write(lp[:])
		buf.Write(r.Key)
		binary.LittleEndian.PutUint32(lp[:], uint32(len(r.Value)))
		buf.Write(lp[:])
		buf.Write(r.Value)
	}
	return buf.Bytes()
}

func decodeRecords(raw []byte) ([]Record, error) {
	var out []Record
	for off := 0; off < len(raw); {
		k, n, err := readField(raw, off)
		if err != nil {
			return nil, err
		}
		off = n
		v, n, err := readField(raw, off)
		if err != nil {
			return nil, err
		}
		off = n
		out = append(out, Record{Key: k, Value: v})
	}
	return out, nil
}

// readField reads a length-prefixed field starting at off and returns the field
// (copied), the offset just past it, and any framing error.
func readField(raw []byte, off int) ([]byte, int, error) {
	if off+4 > len(raw) {
		return nil, 0, ErrMalformed
	}
	n := int(binary.LittleEndian.Uint32(raw[off:]))
	off += 4
	if off+n > len(raw) {
		return nil, 0, ErrMalformed
	}
	field := append([]byte(nil), raw[off:off+n]...)
	return field, off + n, nil
}
```

### The runnable demo

The demo builds fifty near-identical JSON records, reports the raw serialized size and confirms the gzip payload is under half that, then decodes the gzip payload and confirms every record survives the round-trip. The raw size is fixed by the record bytes; the win and the round-trip are reported as booleans so the output is stable across machines regardless of the exact compressed byte count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/batch-compression"
)

func main() {
	records := make([]compression.Record, 50)
	for i := range records {
		records[i] = compression.Record{Value: []byte(`{"event":"order_created","status":"ok"}`)}
	}

	rawBatch := &compression.RecordBatch{Codec: compression.CodecNone, Records: records}
	raw, err := rawBatch.Payload()
	if err != nil {
		log.Fatal(err)
	}

	gzBatch := &compression.RecordBatch{Codec: compression.CodecGzip, Records: records}
	gz, err := gzBatch.Payload()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("records: %d\n", len(records))
	fmt.Printf("raw bytes: %d\n", len(raw))
	fmt.Printf("gzip smaller than raw: %v\n", len(gz) < len(raw))
	fmt.Printf("gzip under half of raw: %v\n", len(gz)*2 < len(raw))

	decoded, err := compression.DecodePayload(compression.CodecGzip, gz)
	if err != nil {
		log.Fatal(err)
	}
	ok := len(decoded) == len(records)
	for i := range decoded {
		if !bytes.Equal(decoded[i].Value, records[i].Value) {
			ok = false
		}
	}
	fmt.Printf("round-trip ok: %v\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
records: 50
raw bytes: 2350
gzip smaller than raw: true
gzip under half of raw: true
round-trip ok: true
```

### Tests

The tests pin both directions and the win. `TestRoundTrip` runs a table through both codecs and asserts every key and value survives. `TestGzipCompressesRepetitive` asserts a repetitive batch's gzip payload is strictly smaller than its raw payload. `TestEmptyBatch` asserts a zero-record batch produces a payload that decodes back to no records under both codecs. `TestUnknownCodecErrors` and `TestDecodeRejectsTruncated` pin the error paths.

Create `compression_test.go`:

```go
package compression

import (
	"bytes"
	"errors"
	"testing"
)

func sampleRecords() []Record {
	return []Record{
		{Key: []byte("k1"), Value: []byte("hello world")},
		{Key: nil, Value: []byte("")},
		{Key: []byte("k3"), Value: bytes.Repeat([]byte("payload"), 100)},
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	for _, codec := range []CompressionCodec{CodecNone, CodecGzip} {
		codec := codec
		t.Run(codec.String(), func(t *testing.T) {
			t.Parallel()

			in := sampleRecords()
			b := &RecordBatch{Codec: codec, Records: in}
			payload, err := b.Payload()
			if err != nil {
				t.Fatalf("Payload: %v", err)
			}
			out, err := DecodePayload(codec, payload)
			if err != nil {
				t.Fatalf("DecodePayload: %v", err)
			}
			if len(out) != len(in) {
				t.Fatalf("got %d records, want %d", len(out), len(in))
			}
			for i := range in {
				if !bytes.Equal(out[i].Key, in[i].Key) || !bytes.Equal(out[i].Value, in[i].Value) {
					t.Errorf("record %d mismatch: got %+v, want %+v", i, out[i], in[i])
				}
			}
		})
	}
}

func TestGzipCompressesRepetitive(t *testing.T) {
	t.Parallel()

	records := make([]Record, 50)
	for i := range records {
		records[i] = Record{Value: []byte(`{"event":"order_created","status":"ok"}`)}
	}
	raw, err := (&RecordBatch{Codec: CodecNone, Records: records}).Payload()
	if err != nil {
		t.Fatal(err)
	}
	gz, err := (&RecordBatch{Codec: CodecGzip, Records: records}).Payload()
	if err != nil {
		t.Fatal(err)
	}
	if len(gz) >= len(raw) {
		t.Errorf("gzip %d bytes >= raw %d bytes; expected compression", len(gz), len(raw))
	}
}

func TestEmptyBatch(t *testing.T) {
	t.Parallel()

	for _, codec := range []CompressionCodec{CodecNone, CodecGzip} {
		b := &RecordBatch{Codec: codec, Records: nil}
		payload, err := b.Payload()
		if err != nil {
			t.Fatalf("%s Payload: %v", codec, err)
		}
		out, err := DecodePayload(codec, payload)
		if err != nil {
			t.Fatalf("%s DecodePayload: %v", codec, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: got %d records, want 0", codec, len(out))
		}
	}
}

func TestUnknownCodecErrors(t *testing.T) {
	t.Parallel()

	b := &RecordBatch{Codec: CompressionCodec(99), Records: sampleRecords()}
	if _, err := b.Payload(); err == nil {
		t.Error("Payload with unknown codec: want error, got nil")
	}
	if _, err := DecodePayload(CompressionCodec(99), []byte("x")); err == nil {
		t.Error("DecodePayload with unknown codec: want error, got nil")
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	t.Parallel()

	// A 4-byte length prefix claiming 1000 bytes with no body.
	bad := []byte{0xE8, 0x03, 0x00, 0x00}
	if _, err := DecodePayload(CodecNone, bad); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}
```

## Review

The payload is sound when `Payload` and `DecodePayload` are exact mirrors: the same length-prefix framing on both sides and the same codec applied and then removed. Confirm a repetitive batch compresses well below its raw size, which is the entire point of compressing at the batch boundary, and that both codecs round-trip every byte of every key and value including empty ones. Confirm `DecodePayload` rejects a truncated frame instead of slicing out of bounds, and that an unknown codec is an error on both the encode and decode sides.

The mistakes to avoid. Reading a gzip buffer that was flushed but never closed hands the reader a stream with no trailer, which it rejects; `Payload` must `Close` the writer and check the error before returning. Trusting a length prefix without bounds-checking it lets a corrupt payload drive an out-of-range slice, so `readField` compares against `len(raw)` before slicing. And compressing per record instead of per batch throws away the shared-dictionary win and usually inflates the data, which is why the codec lives on the batch.

## Resources

- [`compress/gzip`](https://pkg.go.dev/compress/gzip) — `Writer`, `Reader`, and the requirement to `Close` the writer so the trailer is written.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — the little-endian length prefixes that make the record frames self-delimiting.
- [Kafka message compression](https://kafka.apache.org/documentation/#design_compression) — how a production broker compresses at the batch (record-set) level and tags the codec.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-idempotent-producer.md](04-idempotent-producer.md)
