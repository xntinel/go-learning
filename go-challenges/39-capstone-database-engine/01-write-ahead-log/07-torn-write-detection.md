# Exercise 7: Torn-Write Detection on a Segment Image

Streaming recovery folds two physically different crash signatures into one "bad tail" outcome. This exercise builds the pure, buffer-oriented companion: a total function over a complete in-memory segment image that separates a torn record (the frame itself is incomplete) from a corrupt record (the frame is complete but its CRC fails), returns the exact good-prefix length to truncate to, and leaves the policy decision of whether mid-stream corruption is fatal to the caller.

This module is fully self-contained. It ships its own record frame, the scanner, a demo, and tests. Nothing here imports any other exercise.

## What you'll build

```text
record.go        LogRecord, LSN, RecordType, Encode, Decode
torn.go          ScanSegment, ScanResult, ErrTornTail, ErrCorruptRecord
cmd/
  demo/
    main.go      scan a buffer of good records plus a torn tail and classify the stop
torn_test.go     clean boundary + torn header + torn body + corrupt CRC, all byte-exact
```

- Files: `record.go`, `torn.go`, `cmd/demo/main.go`, `torn_test.go`.
- Implement: `ScanResult`, the sentinels `ErrTornTail` and `ErrCorruptRecord`, and `ScanSegment(buf []byte) (ScanResult, error)` over `Decode` and `headerSize`.
- Test: `torn_test.go` constructs a buffer of good records plus each bad-tail shape and asserts the record count, the good-prefix length, and which sentinel is returned.
- Verify: `go test -run 'TestScanSegment|ExampleScanSegment' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/01-write-ahead-log/07-torn-write-detection/cmd/demo && cd go-solutions/39-capstone-database-engine/01-write-ahead-log/07-torn-write-detection
```

### Why a pure scanner separates torn from corrupt

A streaming file scanner reports torn tails and CRC failures the same way — records-so-far plus a stop offset, no error — which is the right call for the file path, where both are just "the writer died here." But there are two physically different things happening, and some callers need to tell them apart. A torn record means the frame is incomplete — the writer got partway through the length prefix or the body and stopped, so there literally are not enough bytes to form a full record. A corrupt record means the frame is fully present, all its declared bytes are there, but the CRC over them does not match — a torn write of a *complete* record, or bit rot on disk. The distinction matters because the appropriate response can differ: a torn tail is always benign (truncate and move on), while a CRC failure in the middle of a sealed segment is acknowledged data gone bad, which a strict engine treats as fatal.

Making the scanner operate over a complete in-memory buffer rather than a file stream buys two properties. It is total: every input maps to a defined output, so every torn and corrupt shape can be constructed and asserted byte-for-byte in a test, with no timing or I/O. And it is pure: it never truncates anything itself; it returns `Good`, the length of the valid prefix, and lets the caller decide what to do with the bad tail. Keeping the policy in the caller — is a non-final-segment stop fatal? — rather than baking it into the scanner is the design point of the exercise.

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

// headerSize is the fixed record header (payloadLen+CRC+LSN+TxID+Type) = 25 bytes.
const headerSize = 25

// LogRecord is the unit written to and read from the WAL.
type LogRecord struct {
	LSN     LSN
	TxID    uint64
	Type    RecordType
	Payload []byte
}

// Encode serializes r into a length-prefixed, CRC32-checksummed binary record.
func (r *LogRecord) Encode() ([]byte, error) {
	plen := uint32(len(r.Payload))
	buf := make([]byte, headerSize+int(plen))

	binary.LittleEndian.PutUint32(buf[0:4], plen)
	binary.LittleEndian.PutUint64(buf[8:16], uint64(r.LSN))
	binary.LittleEndian.PutUint64(buf[16:24], r.TxID)
	buf[24] = byte(r.Type)
	copy(buf[headerSize:], r.Payload)

	crc := crc32.ChecksumIEEE(buf[8:])
	binary.LittleEndian.PutUint32(buf[4:8], crc)
	return buf, nil
}

// Decode parses a LogRecord from src, validating the length and CRC32.
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

Now the scanner. The loop classifies each position with two cheap checks before it trusts the frame. First, are there at least `headerSize` bytes left? If not, the length prefix or fixed header was only partially written — torn. Second, does the declared total `off + headerSize + plen` fit inside the buffer? The length prefix is readable but it claims a body that runs past the end — also torn, just discovered one step later. Only when both pass does `ScanSegment` hand the exact frame slice `buf[off:end]` to `Decode`, and a failure there is unambiguous: the bytes are all present but the CRC rejects them, so it is corrupt, not torn. In every stopping case `Good` is set to `off`, the start of the bad record, which is precisely the length to truncate to.

Create `torn.go`:

```go
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Sentinel errors describing why a segment scan stopped before the end of the
// buffer. Both are recoverable at the tail of the last segment.
var (
	// ErrTornTail indicates the buffer ends inside a record: the length prefix
	// or the body was only partially written. Recovery truncates to the good
	// prefix and continues.
	ErrTornTail = errors.New("wal: torn record at tail")
	// ErrCorruptRecord indicates a fully present frame whose CRC32 does not
	// match: a torn write of a complete record, or bit rot.
	ErrCorruptRecord = errors.New("wal: corrupt record (CRC mismatch)")
)

// ScanResult reports the outcome of scanning an in-memory segment image.
type ScanResult struct {
	// Records are the valid records decoded from the good prefix.
	Records []*LogRecord
	// Good is the byte length of the valid prefix. Truncating the segment to
	// Good removes the torn or corrupt tail.
	Good int
}

// ScanSegment scans a complete in-memory segment image and returns every valid
// record plus the length of the good prefix. It distinguishes the two ways a
// crash leaves a bad tail: a torn record (not enough bytes for the declared
// frame) wraps ErrTornTail, and a fully present frame that fails its CRC wraps
// ErrCorruptRecord. Both stop the scan and set Good to the truncation point. A
// buffer that ends exactly on a record boundary returns a nil error.
func ScanSegment(buf []byte) (ScanResult, error) {
	var recs []*LogRecord
	off := 0
	for {
		if off == len(buf) {
			return ScanResult{Records: recs, Good: off}, nil
		}
		if len(buf)-off < headerSize {
			// Not enough bytes for even a header: the length or fixed header
			// was only partially written.
			return ScanResult{Records: recs, Good: off}, fmt.Errorf("wal: offset %d: %w", off, ErrTornTail)
		}
		plen := int(binary.LittleEndian.Uint32(buf[off : off+4]))
		end := off + headerSize + plen
		if end > len(buf) {
			return ScanResult{Records: recs, Good: off}, fmt.Errorf("wal: offset %d: %w", off, ErrTornTail)
		}
		rec, err := Decode(buf[off:end])
		if err != nil {
			// A complete frame that fails to decode (CRC mismatch): corrupt, not
			// torn. At the tail this is still a crash artifact; truncate here.
			return ScanResult{Records: recs, Good: off}, fmt.Errorf("wal: offset %d: %w", off, ErrCorruptRecord)
		}
		recs = append(recs, rec)
		off = end
	}
}
```

Notice what `ScanSegment` does *not* do: it does not say "this is fine" or "this is fatal." It reports the first bad offset wherever it occurs and which kind it is, and stops. A caller recovering the final segment treats either sentinel as benign and truncates to `Good`; a caller validating a sealed, non-final segment treats a stop before `len(buf)` as data loss and fails. That separation of mechanism (find the bad offset) from policy (decide if it is acceptable) is why the same function serves both callers. The three return points — clean boundary, torn, corrupt — are mutually exclusive and total, which is what lets the test assert the outcome of every shape exactly.

### The runnable demo

This demo builds three good records, appends a two-byte torn tail, scans the buffer, and prints the record count, the good-prefix length, and which sentinel classifies the stop — the whole point of the function visible in one run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/torn-write"
)

func main() {
	var buf []byte
	for i := 0; i < 3; i++ {
		enc, err := (&wal.LogRecord{LSN: wal.LSN(i), Type: wal.TypeInsert, Payload: []byte("row")}).Encode()
		if err != nil {
			fmt.Println("encode error:", err)
			return
		}
		buf = append(buf, enc...)
	}
	goodLen := len(buf)

	// Append a 2-byte torn tail: not enough bytes for a header.
	buf = append(buf, 0xDE, 0xAD)

	res, err := wal.ScanSegment(buf)
	fmt.Printf("scanned %d records, good prefix=%d bytes (of %d)\n", len(res.Records), res.Good, len(buf))
	fmt.Printf("good prefix matches the 3 clean records: %v\n", res.Good == goodLen)
	fmt.Printf("torn tail: %v, corrupt: %v\n", errors.Is(err, wal.ErrTornTail), errors.Is(err, wal.ErrCorruptRecord))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scanned 3 records, good prefix=84 bytes (of 86)
good prefix matches the 3 clean records: true
torn tail: true, corrupt: false
```

### Tests

The table builds three known-good records, captures their combined length as `goodLen`, and then appends each bad shape: nothing (clean boundary), two stray bytes (torn header), a body cut three bytes short (torn body), and a full record with a flipped payload byte (corrupt CRC). Every case must report three records and `Good == goodLen`; only the sentinel differs. The example shows the single-record-then-torn case end to end.

Create `torn_test.go`:

```go
package wal

import (
	"errors"
	"fmt"
	"testing"
)

func TestScanSegment(t *testing.T) {
	t.Parallel()

	var good []byte
	for i := 0; i < 3; i++ {
		enc, err := (&LogRecord{LSN: LSN(i), Type: TypeInsert, Payload: []byte("row")}).Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		good = append(good, enc...)
	}
	goodLen := len(good)

	corrupt, err := (&LogRecord{LSN: 9, Type: TypeInsert, Payload: []byte("bad")}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	corrupt[len(corrupt)-1] ^= 0xFF // flip a payload byte -> CRC mismatch

	partial, err := (&LogRecord{LSN: 3, Type: TypeInsert, Payload: []byte("partial")}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	clone := func(extra []byte) []byte {
		out := make([]byte, 0, goodLen+len(extra))
		out = append(out, good...)
		return append(out, extra...)
	}

	cases := []struct {
		name     string
		buf      []byte
		wantRecs int
		wantErr  error
	}{
		{name: "clean boundary", buf: clone(nil), wantRecs: 3, wantErr: nil},
		{name: "torn header", buf: clone([]byte{0x01, 0x02}), wantRecs: 3, wantErr: ErrTornTail},
		{name: "torn body", buf: clone(partial[:len(partial)-3]), wantRecs: 3, wantErr: ErrTornTail},
		{name: "corrupt crc", buf: clone(corrupt), wantRecs: 3, wantErr: ErrCorruptRecord},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := ScanSegment(tc.buf)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if len(res.Records) != tc.wantRecs {
				t.Fatalf("records = %d, want %d", len(res.Records), tc.wantRecs)
			}
			if res.Good != goodLen {
				t.Fatalf("good = %d, want %d", res.Good, goodLen)
			}
		})
	}
}

func ExampleScanSegment() {
	enc, _ := (&LogRecord{LSN: 0, Type: TypeCommit}).Encode()
	buf := append(enc, 0xDE, 0xAD) // one clean record then a torn 2-byte tail
	res, err := ScanSegment(buf)
	fmt.Printf("records=%d good=%d torn=%v\n", len(res.Records), res.Good, errors.Is(err, ErrTornTail))
	// Output:
	// records=1 good=25 torn=true
}
```

## Review

The scanner is correct when it is total and classifies every shape. A buffer ending on a record boundary must return a nil error with `Good == len(buf)`; a buffer with fewer than `headerSize` trailing bytes, or a declared length that overruns the buffer, must return `ErrTornTail` with `Good` at the start of the bad frame; and a fully present frame whose CRC fails must return `ErrCorruptRecord`, again with `Good` at its start. In every stopping case `Good` is the exact good-prefix length, which the byte-exact table asserts for all four shapes under `go test -race ./...`.

Common mistakes for this feature. Conflating torn and corrupt — returning one sentinel for both — throws away the distinction a strict caller needs to decide whether a mid-segment stop is fatal. Setting `Good` to `end` instead of `off` on a bad frame would keep the bad bytes in the truncated prefix. Reading the length prefix before confirming `headerSize` bytes are present risks indexing past the buffer on a frame torn inside its own header. And baking the fatal-or-benign policy into the scanner (treating a non-final stop as an error inside `ScanSegment`) couples it to one caller and defeats the reason it is pure.

## Resources

- [PostgreSQL: WAL parameters (full_page_writes)](https://www.postgresql.org/docs/current/runtime-config-wal.html) — full-page writes, Postgres's defense against partial (torn) page writes after a crash.
- [SQLite: Write-Ahead Logging](https://www.sqlite.org/wal.html) — per-frame checksums and how a partial or mismatched frame ends a valid read of the log.
- [`hash/crc32`](https://pkg.go.dev/hash/crc32) — the checksum that separates a corrupt but complete frame from a merely torn one.

---

Back to [06-leader-follower-group-commit.md](06-leader-follower-group-commit.md) | Next: [../02-btree-index/00-concepts.md](../02-btree-index/00-concepts.md)
