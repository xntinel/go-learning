# Exercise 1: Record Encoding

Every durability guarantee a write-ahead log makes rests on one humble object: the on-disk record. Before you can recover after a crash you must be able to read a log back, and before you can read it back you must be able to tell, using only the bytes in front of you, where one record ends, where the next begins, and whether the bytes you just read are the bytes that were written. This exercise builds that object: a length-prefixed, CRC32-checksummed binary frame with `Encode` and `Decode`, the self-delimiting, corruption-detecting unit that every later exercise reads and writes.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
record.go            LogRecord, LSN, RecordType, Encode, Decode (CRC32 framing)
cmd/
  demo/
    main.go          encode a record, decode it, then prove a flipped byte is caught
record_test.go       round-trip table, exhaustive bit-flip detection, short-buffer rejection
```

- Files: `record.go`, `cmd/demo/main.go`, `record_test.go`.
- Implement: `LogRecord` with `Encode() ([]byte, error)` and the package function `Decode(src []byte) (*LogRecord, error)`, plus the `LSN` and `RecordType` types.
- Test: `record_test.go` round-trips several records, flips every payload byte and asserts each flip is caught, and rejects buffers shorter than a header.
- Verify: `go test -run 'TestRecord|TestDecode' -race ./...`

### Why framing and a checksum, and why this exact layout

A log file is a flat stream of bytes with no index. The reader is handed a file that a crashed process may have stopped writing in the middle of any record, and it must still parse the good prefix without seeking, guessing, or reading past a record into the next one. Two properties make that possible, and the frame is designed to provide exactly them.

The first is self-delimitation. The frame starts with a 4-byte little-endian length prefix that gives the payload size before the reader commits to reading the body, so the reader does exactly one `io.ReadFull` for the length and one more for the remaining fixed header plus that many payload bytes. There are no sentinel bytes to escape and no terminator to scan for; the length is the only thing the reader needs to advance.

The second is corruption detection. A 4-byte IEEE CRC32 lets the reader reject a frame whose contents were only partially written or silently flipped on disk. The subtle design decision is the range the checksum covers, and it is deliberately asymmetric. The layout is:

```text
Offset  Size  Field
     0     4  payloadLen (uint32 LE): byte count of the Payload field
     4     4  crc32 (uint32 LE): IEEE CRC32 of bytes [8:end]
     8     8  lsn (uint64 LE): log sequence number
    16     8  txid (uint64 LE): transaction identifier
    24     1  type (byte): RecordType (INSERT=1, UPDATE=2, ...)
    25  plen  payload: variable-length operation data
```

The CRC covers bytes `[8:end]` — the LSN, TxID, Type, and payload — and never the length prefix or the CRC slot itself. `Encode` writes the checksum last; `Decode` reads it first and computes over the same range. Because neither side ever has to zero the CRC slot before checksumming, neither side can forget to, which is the single most common framing bug. The framing fields at `[0:8]` are intentionally left outside the checksum's protection: a corrupted length prefix does not silently masquerade as a different valid record, it makes the frame unreadable, and an unreadable frame is the safe failure mode for a log. `Decode` enforces its two preconditions — at least 25 header bytes, and a declared payload length that does not run past the end of the buffer — before it trusts any field, so it can be handed arbitrary bytes from a crashed file and never index out of bounds.

Create `record.go`:

```go
package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// LSN is a monotonically increasing log sequence number assigned to each record.
type LSN uint64

// RecordType classifies the operation recorded in a LogRecord.
type RecordType byte

const (
	TypeInsert     RecordType = 1
	TypeUpdate     RecordType = 2
	TypeDelete     RecordType = 3
	TypeCommit     RecordType = 4
	TypeAbort      RecordType = 5
	TypeCheckpoint RecordType = 6
)

// headerSize is the byte length of the fixed record header:
//
//	[4B payloadLen] [4B CRC32] [8B LSN] [8B TxID] [1B RecordType] = 25 bytes
const headerSize = 25

// LogRecord is the unit written to and read from the WAL.
type LogRecord struct {
	LSN     LSN
	TxID    uint64
	Type    RecordType
	Payload []byte
}

// Encode serializes r into a length-prefixed, CRC32-checksummed binary record.
//
// Layout:
//
//	[payloadLen uint32 LE] [crc32 uint32 LE] [lsn uint64 LE] [txid uint64 LE] [type byte] [payload]
//
// The CRC32 covers bytes [8:end], that is, the LSN, TxID, Type, and Payload.
// The payloadLen and CRC fields at [0:8] are the framing envelope and are not
// covered by the checksum.
func (r *LogRecord) Encode() ([]byte, error) {
	plen := uint32(len(r.Payload))
	buf := make([]byte, headerSize+int(plen))

	binary.LittleEndian.PutUint32(buf[0:4], plen)
	// buf[4:8] is the CRC32 field; filled after computing the checksum below.
	binary.LittleEndian.PutUint64(buf[8:16], uint64(r.LSN))
	binary.LittleEndian.PutUint64(buf[16:24], r.TxID)
	buf[24] = byte(r.Type)
	copy(buf[headerSize:], r.Payload)

	crc := crc32.ChecksumIEEE(buf[8:])
	binary.LittleEndian.PutUint32(buf[4:8], crc)
	return buf, nil
}

// Decode parses a LogRecord from src. It returns an error if the buffer is too
// short or the embedded CRC32 does not match the computed value.
func Decode(src []byte) (*LogRecord, error) {
	if len(src) < headerSize {
		return nil, fmt.Errorf("wal: record too short: have %d bytes, need at least %d", len(src), headerSize)
	}
	plen := int(binary.LittleEndian.Uint32(src[0:4]))
	storedCRC := binary.LittleEndian.Uint32(src[4:8])

	if len(src) < headerSize+plen {
		return nil, fmt.Errorf("wal: buffer too small: have %d, need %d", len(src), headerSize+plen)
	}

	computedCRC := crc32.ChecksumIEEE(src[8 : headerSize+plen])
	if computedCRC != storedCRC {
		return nil, fmt.Errorf("wal: CRC mismatch: stored %08x, computed %08x", storedCRC, computedCRC)
	}

	rec := &LogRecord{
		LSN:  LSN(binary.LittleEndian.Uint64(src[8:16])),
		TxID: binary.LittleEndian.Uint64(src[16:24]),
		Type: RecordType(src[24]),
	}
	if plen > 0 {
		rec.Payload = make([]byte, plen)
		copy(rec.Payload, src[headerSize:headerSize+plen])
	}
	return rec, nil
}
```

Read `Encode` and `Decode` as mirror images. `Encode` lays out the fixed header, copies the payload after it, then computes `crc32.ChecksumIEEE(buf[8:])` and writes the result into `buf[4:8]` afterward, so the CRC slot is the last thing populated and is never itself part of the checksum. `Decode` does the reverse order: it reads the length and stored CRC from the framing envelope first, checks that the buffer actually holds `headerSize + plen` bytes, then recomputes `crc32.ChecksumIEEE(src[8 : headerSize+plen])` over exactly the range `Encode` covered and compares. The payload is copied into a fresh slice rather than aliased into `src`, so a decoded record owns its bytes and the caller can reuse or discard the source buffer freely.

### The runnable demo

A test proves a property in the abstract; a demo makes the object concrete. This one encodes a single record, reports the byte sizes so the 25-byte header is visible, decodes it back to confirm the round-trip, then flips the final byte and shows that `Decode` rejects it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/record-encoding"
)

func main() {
	rec := &wal.LogRecord{LSN: 42, TxID: 7, Type: wal.TypeInsert, Payload: []byte("hello world")}

	enc, err := rec.Encode()
	if err != nil {
		fmt.Println("encode error:", err)
		return
	}
	fmt.Printf("encoded %d bytes (header=%d payload=%d)\n", len(enc), len(enc)-len(rec.Payload), len(rec.Payload))

	dec, err := wal.Decode(enc)
	if err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Printf("decoded LSN=%d TxID=%d Type=%d payload=%q\n", dec.LSN, dec.TxID, dec.Type, dec.Payload)

	// Flip the last payload byte and prove the CRC catches it.
	enc[len(enc)-1] ^= 0xFF
	_, err = wal.Decode(enc)
	fmt.Printf("corruption detected: %v\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
encoded 36 bytes (header=25 payload=11)
decoded LSN=42 TxID=7 Type=1 payload="hello world"
corruption detected: true
```

### Tests

The tests pin the three properties the frame must have. `TestRecordEncodeDecode` round-trips a table of records — with payloads, without payloads, large payloads, and a zero LSN — and checks every field survives. `TestDecodeDetectsBitFlip` flips every byte position past the header and asserts each single-bit corruption is caught, which is the property the CRC exists to provide. `TestDecodeRejectsTooShort` hands `Decode` buffers shorter than a header and asserts it errors instead of indexing out of bounds.

Create `record_test.go`:

```go
package wal

import (
	"testing"
)

func TestRecordEncodeDecode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		r    LogRecord
	}{
		{name: "insert with payload", r: LogRecord{LSN: 42, TxID: 7, Type: TypeInsert, Payload: []byte("hello world")}},
		{name: "checkpoint no payload", r: LogRecord{LSN: 1, TxID: 0, Type: TypeCheckpoint}},
		{name: "delete large payload", r: LogRecord{LSN: 999, TxID: 5, Type: TypeDelete, Payload: make([]byte, 4096)}},
		{name: "zero lsn", r: LogRecord{LSN: 0, TxID: 0, Type: TypeCommit}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			enc, err := tc.r.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(enc)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.LSN != tc.r.LSN || got.TxID != tc.r.TxID || got.Type != tc.r.Type {
				t.Fatalf("header mismatch: got %+v, want %+v", got, tc.r)
			}
			if string(got.Payload) != string(tc.r.Payload) {
				t.Fatalf("payload mismatch: got %q, want %q", got.Payload, tc.r.Payload)
			}
		})
	}
}

func TestDecodeDetectsBitFlip(t *testing.T) {
	t.Parallel()

	base := LogRecord{LSN: 1, TxID: 1, Type: TypeInsert, Payload: []byte("sensitive data")}
	enc, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}

	for i := headerSize; i < len(enc); i++ {
		corrupt := make([]byte, len(enc))
		copy(corrupt, enc)
		corrupt[i] ^= 0xFF

		if _, err := Decode(corrupt); err == nil {
			t.Errorf("byte %d: expected CRC error, got nil", i)
		}
	}
}

func TestDecodeRejectsTooShort(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 12, headerSize - 1} {
		if _, err := Decode(make([]byte, n)); err == nil {
			t.Errorf("len=%d: expected error, got nil", n)
		}
	}
}
```

## Review

The frame is sound when encode and decode are exact mirrors: `Encode` writes the CRC last and checksums only `buf[8:]`, and `Decode` reads the CRC first and recomputes over the same `[8:headerSize+plen]` range, so neither side ever has to zero the CRC slot. Confirm `Decode` rejects a buffer shorter than `headerSize` and one whose declared `payloadLen` runs past the end before it indexes any field, and that a decoded record owns its payload — it is copied out of `src` — so reusing or discarding the source buffer cannot corrupt a record already returned. The round-trip table, the exhaustive bit-flip sweep, and the short-buffer cases all passing under `go test -race ./...` together establish those properties.

Common mistakes for this feature. The first is checksumming the wrong range: the tempting symmetric design computes the CRC over the whole buffer including its own slot, which forces both encoder and decoder to zero that slot before computing, and the day one side forgets, every record reports a false mismatch. Checksumming only `buf[8:]` — CRC written last, read first — removes the zeroing step entirely. The second is trusting a length prefix without bounds-checking it: a crashed file can hand you a `payloadLen` that claims megabytes, so `Decode` must compare `len(src)` against `headerSize+plen` before slicing or it will panic on hostile input. The third is aliasing the payload into `src` instead of copying it, which turns a reused read buffer into silent record corruption.

## Resources

- [`hash/crc32`](https://pkg.go.dev/hash/crc32) — the standard-library CRC32 package and `ChecksumIEEE`, the exact checksum this frame uses.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — fixed-width little-endian encoding of the length prefix and the header fields.
- [PostgreSQL: WAL Internals](https://www.postgresql.org/docs/current/wal-internals.html) — how a production WAL lays out records and log sequence numbers in an append-only stream.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-wal-core.md](02-wal-core.md)
