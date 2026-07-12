# Exercise 1: Message Encoding

Every durability guarantee the storage engine makes rests on one humble object: the on-disk record. Before a crashed log can be recovered it must be readable, and before it can be read it must be possible to tell, from the bytes alone, where a record ends, where the next begins, and whether the bytes are the bytes that were written. This exercise builds that object — a self-describing, CRC32-checksummed binary encoding of a `Message`, with `Encode` and `Decode` as exact mirror images — which every later exercise reads and writes.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
store.go             Message, Header, Encode, Decode, RecordSize, error sentinels
cmd/
  demo/
    main.go          encode a message, decode it, then prove a flipped byte is caught
store_test.go        round-trip table, exhaustive bit-flip detection, short-buffer rejection
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Message` and `Header` types, `Encode(*Message) ([]byte, error)`, `Decode([]byte) (*Message, error)`, and `RecordSize(*Message) int`.
- Test: `store_test.go` round-trips several messages, flips every body byte and asserts each flip is caught, and rejects buffers shorter than a header.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/02-persistent-message-storage/01-message-encoding/cmd/demo && cd go-solutions/41-capstone-message-queue/02-persistent-message-storage/01-message-encoding
```

### Why this exact layout

A message has a fixed part — its offset and timestamp — and three variable parts — the key, the value, and a list of headers. To write all of that into a flat byte stream so a reader can pull it back out with no external schema, every variable field is preceded by its length. The reader thus always knows how many bytes to consume next; it never has to guess or scan for a terminator. This is length-prefixed framing, and the layout is:

```text
offset       uint64  8 bytes
timestamp    int64   8 bytes
key_len      uint32  4 bytes
key          []byte  key_len bytes
value_len    uint32  4 bytes
value        []byte  value_len bytes
header_count uint16  2 bytes
  (per header: key_len uint16, key bytes, val_len uint16, val bytes)
crc32        uint32  4 bytes   IEEE checksum of all bytes above
```

All fixed fields are big-endian, which sorts byte-wise in the same order as numerically and is the convention for on-disk and on-wire formats. `encoding/binary` never pads, so the layout is exactly the byte count above with no surprises.

The trailing CRC32 is the integrity guarantee. It is computed with `hash/crc32.ChecksumIEEE` over every byte that precedes it, and it is the *last* thing written. On decode it is the *first* thing checked: `Decode` recomputes the checksum over the same prefix and compares before it interprets a single field. That ordering — written last, checked first, covering only the bytes ahead of it — is what keeps the checksum out of its own coverage, so neither side ever has to zero the slot, and a corrupt record can never produce a half-built `Message`.

Create `store.go`:

```go
package msgstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Sentinel errors that callers can match with errors.Is.
var (
	ErrChecksumMismatch = errors.New("msgstore: crc32 checksum mismatch")
	ErrShortRecord      = errors.New("msgstore: record is too short to decode")
)

// Header is a single key/value pair attached to a Message.
type Header struct {
	Key   []byte
	Value []byte
}

// Message is the unit of storage. Offset and Timestamp are assigned by the log
// on append; callers populate Key, Value, and Headers.
type Message struct {
	Offset    int64
	Timestamp int64 // Unix nanoseconds
	Key       []byte
	Value     []byte
	Headers   []Header
}

// minRecord is the encoded size of a message with empty key, value, and headers:
// offset(8) + timestamp(8) + key_len(4) + value_len(4) + header_count(2) + crc(4).
const minRecord = 8 + 8 + 4 + 4 + 2 + 4

// Encode serializes msg into a self-describing byte slice with a trailing
// CRC32-IEEE checksum over all preceding bytes.
func Encode(msg *Message) ([]byte, error) {
	var buf bytes.Buffer

	// Fixed-width header and length-prefixed key/value. binary.Write to a
	// bytes.Buffer never returns an error, so the results are discarded.
	_ = binary.Write(&buf, binary.BigEndian, msg.Offset)
	_ = binary.Write(&buf, binary.BigEndian, msg.Timestamp)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(msg.Key)))
	buf.Write(msg.Key)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(msg.Value)))
	buf.Write(msg.Value)

	// Variable-length headers.
	_ = binary.Write(&buf, binary.BigEndian, uint16(len(msg.Headers)))
	for _, h := range msg.Headers {
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(h.Key)))
		buf.Write(h.Key)
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(h.Value)))
		buf.Write(h.Value)
	}

	// Checksum over every byte written so far, appended last.
	crc := crc32.ChecksumIEEE(buf.Bytes())
	_ = binary.Write(&buf, binary.BigEndian, crc)

	return buf.Bytes(), nil
}

// Decode deserializes data produced by Encode. It validates the CRC32 before
// reading any field, returning ErrChecksumMismatch on corruption and
// ErrShortRecord on a truncated buffer.
func Decode(data []byte) (*Message, error) {
	if len(data) < minRecord {
		return nil, ErrShortRecord
	}

	// Validate the checksum before trusting any field.
	body := data[:len(data)-4]
	stored := binary.BigEndian.Uint32(data[len(data)-4:])
	if got := crc32.ChecksumIEEE(body); got != stored {
		return nil, fmt.Errorf("%w: got %08x want %08x", ErrChecksumMismatch, got, stored)
	}

	r := bytes.NewReader(body)
	msg := &Message{}

	var u32 uint32
	var u16 uint16

	if err := binary.Read(r, binary.BigEndian, &msg.Offset); err != nil {
		return nil, ErrShortRecord
	}
	if err := binary.Read(r, binary.BigEndian, &msg.Timestamp); err != nil {
		return nil, ErrShortRecord
	}

	if err := binary.Read(r, binary.BigEndian, &u32); err != nil {
		return nil, ErrShortRecord
	}
	msg.Key = make([]byte, u32)
	if _, err := io.ReadFull(r, msg.Key); err != nil {
		return nil, ErrShortRecord
	}

	if err := binary.Read(r, binary.BigEndian, &u32); err != nil {
		return nil, ErrShortRecord
	}
	msg.Value = make([]byte, u32)
	if _, err := io.ReadFull(r, msg.Value); err != nil {
		return nil, ErrShortRecord
	}

	if err := binary.Read(r, binary.BigEndian, &u16); err != nil {
		return nil, ErrShortRecord
	}
	msg.Headers = make([]Header, u16)
	for i := range msg.Headers {
		if err := binary.Read(r, binary.BigEndian, &u16); err != nil {
			return nil, ErrShortRecord
		}
		msg.Headers[i].Key = make([]byte, u16)
		if _, err := io.ReadFull(r, msg.Headers[i].Key); err != nil {
			return nil, ErrShortRecord
		}
		if err := binary.Read(r, binary.BigEndian, &u16); err != nil {
			return nil, ErrShortRecord
		}
		msg.Headers[i].Value = make([]byte, u16)
		if _, err := io.ReadFull(r, msg.Headers[i].Value); err != nil {
			return nil, ErrShortRecord
		}
	}

	return msg, nil
}

// RecordSize returns the encoded size of msg without allocating.
func RecordSize(msg *Message) int {
	size := minRecord + len(msg.Key) + len(msg.Value)
	for _, h := range msg.Headers {
		size += 2 + len(h.Key) + 2 + len(h.Value)
	}
	return size
}
```

Read `Encode` and `Decode` as mirror images. `Encode` lays the fixed header down, writes each variable field after its length, then computes `crc32.ChecksumIEEE(buf.Bytes())` and appends it — so the checksum is the last byte group and is never part of its own coverage. `Decode` reverses the order exactly: it slices off the trailing 4-byte checksum, recomputes over the same `body` range, and only if that matches does it walk the fields. Every variable field is read with `io.ReadFull`, which fails loudly on a buffer too short to satisfy a declared length rather than returning a short slice, so a malformed record turns into `ErrShortRecord` instead of a silently truncated message. Each decoded slice is freshly allocated, so a decoded message owns its bytes and the caller may reuse or discard the source buffer freely.

### The runnable demo

A test proves a property in the abstract; a demo makes the object concrete. This one encodes a message with a header, reports the byte size, decodes it to confirm the round-trip, then flips a byte in the middle of the record and shows that `Decode` rejects it with `ErrChecksumMismatch`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/message-encoding"
)

func main() {
	msg := &msgstore.Message{
		Offset: 42,
		Key:    []byte("order-id"),
		Value:  []byte("shipped"),
		Headers: []msgstore.Header{
			{Key: []byte("source"), Value: []byte("demo")},
		},
	}

	data, err := msgstore.Encode(msg)
	if err != nil {
		fmt.Println("encode error:", err)
		return
	}
	fmt.Printf("encoded %d bytes (RecordSize=%d)\n", len(data), msgstore.RecordSize(msg))

	got, err := msgstore.Decode(data)
	if err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Printf("decoded offset=%d key=%s value=%s headers=%d\n", got.Offset, got.Key, got.Value, len(got.Headers))

	// Flip a byte in the body and prove the CRC catches it.
	data[len(data)/2] ^= 0xFF
	_, err = msgstore.Decode(data)
	fmt.Printf("corruption detected: %v\n", errors.Is(err, msgstore.ErrChecksumMismatch))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
encoded 59 bytes (RecordSize=59)
decoded offset=42 key=order-id value=shipped headers=1
corruption detected: true
```

### Tests

The tests pin the three properties the encoding must have. `TestEncodeDecode` round-trips a table of messages — with and without a key and value, with headers, and an all-zero message — and checks every field survives. `TestDecodeDetectsCorruption` flips every byte position in the body and asserts each single-byte corruption is caught, which is the property the CRC exists to provide. `TestDecodeShortData` hands `Decode` buffers shorter than a header and asserts it errors instead of indexing out of bounds.

Create `store_test.go`:

```go
package msgstore

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  *Message
	}{
		{
			name: "simple key/value",
			msg:  &Message{Offset: 42, Timestamp: 1_000_000, Key: []byte("order-id"), Value: []byte(`{"amount":99}`)},
		},
		{
			name: "with headers",
			msg: &Message{
				Offset: 7,
				Key:    []byte("k"),
				Value:  []byte("v"),
				Headers: []Header{
					{Key: []byte("content-type"), Value: []byte("application/json")},
					{Key: []byte("trace-id"), Value: []byte("abc123")},
				},
			},
		},
		{name: "empty key and value", msg: &Message{Offset: 0}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := Encode(tc.msg)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if len(data) != RecordSize(tc.msg) {
				t.Fatalf("len(data)=%d, RecordSize=%d", len(data), RecordSize(tc.msg))
			}
			got, err := Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Offset != tc.msg.Offset || got.Timestamp != tc.msg.Timestamp {
				t.Errorf("header: got off=%d ts=%d, want off=%d ts=%d", got.Offset, got.Timestamp, tc.msg.Offset, tc.msg.Timestamp)
			}
			if !bytes.Equal(got.Key, tc.msg.Key) {
				t.Errorf("Key: got %q, want %q", got.Key, tc.msg.Key)
			}
			if !bytes.Equal(got.Value, tc.msg.Value) {
				t.Errorf("Value: got %q, want %q", got.Value, tc.msg.Value)
			}
			if len(got.Headers) != len(tc.msg.Headers) {
				t.Fatalf("Headers: got %d, want %d", len(got.Headers), len(tc.msg.Headers))
			}
			for i := range got.Headers {
				if !bytes.Equal(got.Headers[i].Key, tc.msg.Headers[i].Key) || !bytes.Equal(got.Headers[i].Value, tc.msg.Headers[i].Value) {
					t.Errorf("header %d mismatch", i)
				}
			}
		})
	}
}

func TestDecodeDetectsCorruption(t *testing.T) {
	t.Parallel()

	msg := &Message{Offset: 1, Key: []byte("key"), Value: []byte("value")}
	data, err := Encode(msg)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < len(data); i++ {
		corrupt := make([]byte, len(data))
		copy(corrupt, data)
		corrupt[i] ^= 0xFF
		if _, err := Decode(corrupt); err == nil {
			t.Errorf("byte %d: expected error, got nil", i)
		}
	}
}

func TestDecodeShortData(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 12, minRecord - 1} {
		if _, err := Decode(make([]byte, n)); !errors.Is(err, ErrShortRecord) {
			t.Errorf("len=%d: expected ErrShortRecord, got %v", n, err)
		}
	}
}
```

## Review

The encoding is sound when `Encode` and `Decode` are exact mirrors: the checksum is written last over `buf.Bytes()` and read first over the same prefix, so neither side ever zeroes the CRC slot and a single flipped bit anywhere in the record is caught. Confirm that `Decode` rejects a buffer shorter than `minRecord` before it touches a field, that every variable field is read with `io.ReadFull` so a declared length longer than the buffer becomes `ErrShortRecord` rather than a short slice, and that each decoded field is copied into its own allocation so the source buffer can be reused. The round-trip table, the exhaustive bit-flip sweep, and the short-buffer cases passing under `go test -race ./...` establish those properties together.

The common mistakes here all come from the checksum. Checksumming the whole buffer including the CRC field forces both sides to zero the slot first, and the moment one forgets, every record reports a false mismatch — covering only the bytes ahead of the checksum removes the step entirely. Reading a variable field with a plain `Read` instead of `io.ReadFull` can return fewer bytes than requested without an error, leaving a message that looks decoded but is truncated. And reusing one scratch buffer across `Decode` calls, by aliasing slices into the source instead of copying, turns a later overwrite of that buffer into silent corruption of a record already handed back.

## Resources

- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — fixed-width big-endian encoding of the header fields and length prefixes.
- [`hash/crc32`](https://pkg.go.dev/hash/crc32) — `ChecksumIEEE`, the exact checksum this record uses, the same polynomial Kafka uses.
- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the read that fails loudly on a short buffer instead of returning a partial result.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-segmented-log.md](02-segmented-log.md)
