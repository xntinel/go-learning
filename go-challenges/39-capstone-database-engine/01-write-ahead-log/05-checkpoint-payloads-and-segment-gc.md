# Exercise 5: Checkpoint Payloads and Segment GC

An empty checkpoint marker records *that* a checkpoint happened but not *where the redo point is*, so on restart recovery would have no choice but to replay from the beginning of the oldest surviving segment. This exercise gives the marker a body: a `CheckpointPayload` carrying the redo LSN recovery must replay from and the transactions still in flight when it was taken, the `LastCheckpoint` scanner that finds the most recent one in a recovered stream, and the end-to-end wiring that turns that redo LSN into the safe frontier for segment garbage collection.

This module is fully self-contained. It ships its own record frame, a minimal writer with `Truncate`, the recovery scanner, the checkpoint payload codec, a demo, and tests. Nothing here imports any other exercise.

## What you'll build

```text
record.go            LogRecord, LSN, RecordType, Encode, Decode
wal.go               minimal writer: Open, Append, Truncate (segment GC), Close
recovery.go          readSegment + Recover (to recover the checkpoint stream)
checkpoint.go        CheckpointPayload, Marshal, UnmarshalCheckpoint, LastCheckpoint
cmd/
  demo/
    main.go          write segments, checkpoint with a redo LSN, recover, GC below it
checkpoint_test.go   marshal round-trip, length-mismatch errors, last-checkpoint scan, GC end-to-end
```

- Files: `record.go`, `wal.go`, `recovery.go`, `checkpoint.go`, `cmd/demo/main.go`, `checkpoint_test.go`.
- Implement: `CheckpointPayload` with `Marshal`/`UnmarshalCheckpoint`, the sentinels `ErrShortCheckpoint`/`ErrCheckpointLength`/`ErrNoCheckpoint`, and `LastCheckpoint([]*LogRecord) (LSN, CheckpointPayload, error)`.
- Test: `checkpoint_test.go` asserts the wire round-trip preserves `RedoLSN` and `ActiveTxns`, that short and length-mismatched buffers return the right sentinels, that `LastCheckpoint` selects the highest-LSN checkpoint, and that `Truncate(redoLSN)` frees segments end to end.
- Verify: `go test -run 'TestUnmarshalCheckpoint|TestCheckpointRoundTrip|TestLastCheckpoint|TestCheckpointGC' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/01-write-ahead-log/05-checkpoint-payloads-and-segment-gc/cmd/demo && cd go-solutions/39-capstone-database-engine/01-write-ahead-log/05-checkpoint-payloads-and-segment-gc
```

### Why an empty marker is not enough

A checkpoint exists to bound recovery work: it names a redo point, a single LSN such that replaying only from there forward is guaranteed correct because everything earlier is already reflected in the data files. To skip the replay of everything before it, the marker has to carry the redo LSN itself. It also has to carry the set of transactions that were still active when the checkpoint was taken, because those are exactly the ones recovery may need to undo — a fuzzy checkpoint runs concurrently with live traffic, so transactions straddle it, and recovery must know which were unfinished. That is two pieces of state, and packing them into the record's payload is the job of `CheckpointPayload`.

The wire format is deliberately fixed-then-variable: an 8-byte redo LSN, a 4-byte count, then that many 8-byte transaction IDs. The count is what makes the variable tail self-describing, and validating it on decode is what turns a truncated or garbage payload into a clean error instead of an out-of-bounds panic. Because the payload rides inside the existing record frame, nothing about the writer changes: a rich checkpoint is just `Append(&LogRecord{Type: TypeCheckpoint, Payload: cp.Marshal()})`.

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

Create `wal.go`:

```go
package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultMaxSegmentSize = 64 << 20 // 64 MB
	segmentPattern        = "wal-%06d.log"
)

// Options configures a WAL.
type Options struct {
	Dir            string
	MaxSegmentSize int64
}

// WAL is a minimal write-ahead log writer, safe for concurrent use.
type WAL struct {
	opts    Options
	mu      sync.Mutex
	file    *os.File
	segSeq  uint64
	nextLSN LSN
	size    int64
	stopped bool
}

// Open opens (or creates) the WAL at opts.Dir, resuming the LSN counter.
func Open(opts Options) (*WAL, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("wal: Options.Dir is required")
	}
	if opts.MaxSegmentSize <= 0 {
		opts.MaxSegmentSize = defaultMaxSegmentSize
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: create dir: %w", err)
	}

	w := &WAL{opts: opts}
	seqs, err := listSegmentSeqs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if len(seqs) > 0 {
		w.segSeq = seqs[len(seqs)-1]
		last := filepath.Join(opts.Dir, fmt.Sprintf(segmentPattern, w.segSeq))
		lsn, err := scanLastLSN(last)
		if err != nil {
			return nil, err
		}
		w.nextLSN = lsn + 1
	}
	if err := w.openSegment(); err != nil {
		return nil, err
	}
	return w, nil
}

// Append serializes r, assigns r.LSN, writes it, and fsyncs before returning.
func (w *WAL) Append(r *LogRecord) (LSN, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return 0, fmt.Errorf("wal: closed")
	}
	lsn := w.nextLSN
	r.LSN = lsn

	data, err := r.Encode()
	if err != nil {
		return 0, fmt.Errorf("wal: encode: %w", err)
	}
	if err := w.write(data); err != nil {
		return 0, err
	}
	if err := w.file.Sync(); err != nil {
		return 0, fmt.Errorf("wal: fsync: %w", err)
	}
	w.nextLSN++
	return lsn, nil
}

func (w *WAL) write(data []byte) error {
	if w.size+int64(len(data)) > w.opts.MaxSegmentSize {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	n, err := w.file.Write(data)
	if err != nil {
		return fmt.Errorf("wal: write: %w", err)
	}
	w.size += int64(n)
	return nil
}

func (w *WAL) rotate() error {
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("wal: sync before rotate: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("wal: close before rotate: %w", err)
	}
	w.segSeq++
	return w.openSegment()
}

func (w *WAL) openSegment() error {
	path := filepath.Join(w.opts.Dir, fmt.Sprintf(segmentPattern, w.segSeq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("wal: open segment: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: stat segment: %w", err)
	}
	w.file = f
	w.size = fi.Size()
	return nil
}

// Truncate deletes every segment whose records all have an LSN strictly less
// than upToLSN. The active segment is never deleted.
func (w *WAL) Truncate(upToLSN LSN) error {
	w.mu.Lock()
	activeSeq := w.segSeq
	w.mu.Unlock()

	seqs, err := listSegmentSeqs(w.opts.Dir)
	if err != nil {
		return err
	}
	for _, seq := range seqs {
		if seq >= activeSeq {
			break
		}
		path := filepath.Join(w.opts.Dir, fmt.Sprintf(segmentPattern, seq))
		maxLSN, err := scanLastLSN(path)
		if err != nil {
			return err
		}
		if maxLSN < upToLSN {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("wal: remove segment %s: %w", path, err)
			}
		}
	}
	return nil
}

// Close syncs and closes the WAL. It is idempotent.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.file == nil {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		w.file = nil
		return fmt.Errorf("wal: final sync: %w", err)
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func listSegmentSeqs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal: read dir: %w", err)
	}
	var seqs []uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "wal-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		mid := name[4 : len(name)-4]
		n, err := strconv.ParseUint(mid, 10, 64)
		if err != nil {
			continue
		}
		seqs = append(seqs, n)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs, nil
}

func scanLastLSN(path string) (LSN, error) {
	records, _, err := readSegment(path)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	return records[len(records)-1].LSN, nil
}
```

Create `recovery.go`:

```go
package wal

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// readSegment reads all valid records from a single segment file, returning the
// records and the offset where the first partial or corrupt record begins.
func readSegment(path string) ([]*LogRecord, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, -1, fmt.Errorf("wal: open segment: %w", err)
	}
	defer f.Close()

	var records []*LogRecord
	offset := 0
	lenBuf := make([]byte, 4)

	for {
		startOffset := offset

		if _, err := io.ReadFull(f, lenBuf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return records, startOffset, nil
			}
			return nil, -1, fmt.Errorf("wal: read length at offset %d: %w", startOffset, err)
		}

		plen := int(binary.LittleEndian.Uint32(lenBuf))
		bodyLen := headerSize - 4 + plen
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(f, body); err != nil {
			if err == io.ErrUnexpectedEOF {
				return records, startOffset, nil
			}
			return nil, -1, fmt.Errorf("wal: read record body at offset %d: %w", startOffset, err)
		}

		full := make([]byte, 4+bodyLen)
		copy(full[:4], lenBuf)
		copy(full[4:], body)

		rec, err := Decode(full)
		if err != nil {
			return records, startOffset, nil
		}
		records = append(records, rec)
		offset = startOffset + 4 + bodyLen
	}
}

// Recover reads all WAL segments from dir in order and returns every valid
// record, repairing a partial or corrupt tail of the final segment in-place.
func Recover(dir string) ([]*LogRecord, error) {
	seqs, err := listSegmentSeqs(dir)
	if err != nil {
		return nil, err
	}
	var all []*LogRecord
	for i, seq := range seqs {
		path := filepath.Join(dir, fmt.Sprintf(segmentPattern, seq))
		recs, tailOffset, err := readSegment(path)
		if err != nil {
			return nil, fmt.Errorf("wal: segment %d: %w", seq, err)
		}
		all = append(all, recs...)
		if i == len(seqs)-1 && tailOffset >= 0 {
			if err := os.Truncate(path, int64(tailOffset)); err != nil {
				return nil, fmt.Errorf("wal: truncate tail of %s: %w", path, err)
			}
		}
	}
	return all, nil
}
```

Now the payload codec. `UnmarshalCheckpoint` is where the care lives, and the exact-equality check is the subtle part: it tests `len(src) != checkpointMinLen+8*n`, not `>=`. Trailing bytes are as wrong as missing ones, because a payload that decodes "successfully" while ignoring extra bytes would mask a framing bug and let two different byte strings claim to be the same checkpoint. The two sentinels separate the two failure modes a caller cares about — too short to even read the header (`ErrShortCheckpoint`) versus a header whose declared count disagrees with the body (`ErrCheckpointLength`) — and both are wrapped with `%w` so `errors.Is` works through the descriptive message. `LastCheckpoint` scans from the end because a recovered stream is in ascending LSN order and the relevant checkpoint is the most recent one; the first match walking backward is the highest LSN.

Create `checkpoint.go`:

```go
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Sentinel errors returned when decoding a checkpoint payload.
var (
	// ErrShortCheckpoint is returned when a payload is smaller than the fixed
	// 12-byte header.
	ErrShortCheckpoint = errors.New("wal: checkpoint payload too short")
	// ErrCheckpointLength is returned when the declared active-transaction count
	// does not match the bytes that follow it.
	ErrCheckpointLength = errors.New("wal: checkpoint payload length mismatch")
	// ErrNoCheckpoint is returned when a record stream contains no checkpoint.
	ErrNoCheckpoint = errors.New("wal: no checkpoint record found")
)

// checkpointMinLen is the fixed header: 8-byte RedoLSN + 4-byte txn count.
const checkpointMinLen = 12

// CheckpointPayload is the body carried by a TypeCheckpoint record. RedoLSN is
// the oldest LSN whose effect may not yet be on a data page: recovery replays
// from RedoLSN forward. ActiveTxns lists transactions still in flight when the
// checkpoint was taken, so recovery knows which to undo.
type CheckpointPayload struct {
	RedoLSN    LSN
	ActiveTxns []uint64
}

// Marshal encodes p into the wire form stored in a checkpoint record payload:
//
//	[RedoLSN uint64 LE] [count uint32 LE] [count * (txid uint64 LE)]
func (p CheckpointPayload) Marshal() []byte {
	buf := make([]byte, checkpointMinLen+8*len(p.ActiveTxns))
	binary.LittleEndian.PutUint64(buf[0:8], uint64(p.RedoLSN))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(p.ActiveTxns)))
	off := checkpointMinLen
	for _, tx := range p.ActiveTxns {
		binary.LittleEndian.PutUint64(buf[off:off+8], tx)
		off += 8
	}
	return buf
}

// UnmarshalCheckpoint decodes a checkpoint payload. It returns ErrShortCheckpoint
// when src is shorter than the fixed header and ErrCheckpointLength when the
// declared transaction count does not match the remaining bytes.
func UnmarshalCheckpoint(src []byte) (CheckpointPayload, error) {
	if len(src) < checkpointMinLen {
		return CheckpointPayload{}, fmt.Errorf("wal: decode checkpoint: %w", ErrShortCheckpoint)
	}
	p := CheckpointPayload{RedoLSN: LSN(binary.LittleEndian.Uint64(src[0:8]))}
	n := int(binary.LittleEndian.Uint32(src[8:12]))
	if len(src) != checkpointMinLen+8*n {
		return CheckpointPayload{}, fmt.Errorf("wal: decode checkpoint: want %d bytes for %d txns, have %d: %w", checkpointMinLen+8*n, n, len(src), ErrCheckpointLength)
	}
	if n > 0 {
		p.ActiveTxns = make([]uint64, n)
		off := checkpointMinLen
		for i := 0; i < n; i++ {
			p.ActiveTxns[i] = binary.LittleEndian.Uint64(src[off : off+8])
			off += 8
		}
	}
	return p, nil
}

// LastCheckpoint scans recs from the end and returns the LSN and decoded payload
// of the last TypeCheckpoint record (the highest-LSN checkpoint when recs is in
// LSN order, as a recovered stream is). It returns ErrNoCheckpoint when no
// checkpoint record is present.
func LastCheckpoint(recs []*LogRecord) (LSN, CheckpointPayload, error) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Type == TypeCheckpoint {
			p, err := UnmarshalCheckpoint(recs[i].Payload)
			if err != nil {
				return 0, CheckpointPayload{}, err
			}
			return recs[i].LSN, p, nil
		}
	}
	return 0, CheckpointPayload{}, ErrNoCheckpoint
}
```

`LastCheckpoint` is the bridge between recovery and GC: after `Recover` returns the records, it extracts the redo point, and the writer's `Truncate(redoLSN)` then deletes every segment whose records all lie strictly below it. That is the full chain the demo walks.

### The runnable demo

This demo writes enough records over a small segment limit to span several segments, appends a checkpoint whose `RedoLSN` points into a later segment, closes, recovers the stream, reads the redo point back with `LastCheckpoint`, and truncates below it — printing the segment count before and after so the GC is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/checkpoint-gc"
)

func segments(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	return len(files)
}

func main() {
	dir, err := os.MkdirTemp("", "wal-cp-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	w, err := wal.Open(wal.Options{Dir: dir, MaxSegmentSize: 90})
	if err != nil {
		log.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := w.Append(&wal.LogRecord{TxID: uint64(i/3) + 1, Type: wal.TypeInsert, Payload: []byte("row")}); err != nil {
			log.Fatal(err)
		}
	}
	// A rich checkpoint: replay from LSN 7, transaction 99 still active.
	cp := wal.CheckpointPayload{RedoLSN: 7, ActiveTxns: []uint64{99}}
	cpLSN, err := w.Append(&wal.LogRecord{Type: wal.TypeCheckpoint, Payload: cp.Marshal()})
	if err != nil {
		log.Fatal(err)
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("checkpoint written at LSN=%d; segments before GC: %d\n", cpLSN, segments(dir))

	records, err := wal.Recover(dir)
	if err != nil {
		log.Fatal(err)
	}
	lsn, payload, err := wal.LastCheckpoint(records)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recovered %d records; last checkpoint LSN=%d redo=%d active=%v\n", len(records), lsn, payload.RedoLSN, payload.ActiveTxns)

	w2, err := wal.Open(wal.Options{Dir: dir, MaxSegmentSize: 90})
	if err != nil {
		log.Fatal(err)
	}
	if err := w2.Truncate(payload.RedoLSN); err != nil {
		log.Fatal(err)
	}
	w2.Close()
	fmt.Printf("segments after GC below redo LSN %d: %d\n", payload.RedoLSN, segments(dir))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
checkpoint written at LSN=12; segments before GC: 5
recovered 13 records; last checkpoint LSN=12 redo=7 active=[99]
segments after GC below redo LSN 7: 3
```

### Tests

The tests cover the behaviours independently: decode errors (short and length-mismatch, asserted via `errors.Is` against the sentinels), a full marshal/unmarshal round-trip preserving both fields, the last-checkpoint scan over a mixed stream that must select the higher-LSN checkpoint, and an end-to-end GC test that writes a checkpoint, recovers it, and asserts `Truncate(redoLSN)` actually drops segments. `ExampleUnmarshalCheckpoint` is auto-verified against its `// Output:` line.

Create `checkpoint_test.go`:

```go
package wal

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestUnmarshalCheckpoint(t *testing.T) {
	t.Parallel()

	mismatch := CheckpointPayload{RedoLSN: 5, ActiveTxns: []uint64{1, 2}}.Marshal()
	mismatch = mismatch[:len(mismatch)-1] // drop a trailing byte

	cases := []struct {
		name    string
		in      []byte
		wantErr error
	}{
		{name: "too short", in: []byte{1, 2, 3}, wantErr: ErrShortCheckpoint},
		{name: "length mismatch", in: mismatch, wantErr: ErrCheckpointLength},
		{name: "ok no txns", in: CheckpointPayload{RedoLSN: 7}.Marshal(), wantErr: nil},
		{name: "ok with txns", in: CheckpointPayload{RedoLSN: 9, ActiveTxns: []uint64{3, 4, 5}}.Marshal(), wantErr: nil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := UnmarshalCheckpoint(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	t.Parallel()

	want := CheckpointPayload{RedoLSN: 128, ActiveTxns: []uint64{10, 20, 30}}
	got, err := UnmarshalCheckpoint(want.Marshal())
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.RedoLSN != want.RedoLSN {
		t.Fatalf("RedoLSN = %d, want %d", got.RedoLSN, want.RedoLSN)
	}
	if len(got.ActiveTxns) != len(want.ActiveTxns) {
		t.Fatalf("len(ActiveTxns) = %d, want %d", len(got.ActiveTxns), len(want.ActiveTxns))
	}
	for i := range want.ActiveTxns {
		if got.ActiveTxns[i] != want.ActiveTxns[i] {
			t.Fatalf("ActiveTxns[%d] = %d, want %d", i, got.ActiveTxns[i], want.ActiveTxns[i])
		}
	}
}

func TestLastCheckpointSelectsLast(t *testing.T) {
	t.Parallel()

	recs := []*LogRecord{
		{LSN: 0, Type: TypeInsert},
		{LSN: 1, Type: TypeCheckpoint, Payload: CheckpointPayload{RedoLSN: 0}.Marshal()},
		{LSN: 2, Type: TypeInsert},
		{LSN: 3, Type: TypeCheckpoint, Payload: CheckpointPayload{RedoLSN: 2, ActiveTxns: []uint64{99}}.Marshal()},
		{LSN: 4, Type: TypeInsert},
	}
	lsn, cp, err := LastCheckpoint(recs)
	if err != nil {
		t.Fatalf("LastCheckpoint: %v", err)
	}
	if lsn != 3 {
		t.Fatalf("checkpoint LSN = %d, want 3", lsn)
	}
	if cp.RedoLSN != 2 {
		t.Fatalf("RedoLSN = %d, want 2", cp.RedoLSN)
	}
	if len(cp.ActiveTxns) != 1 || cp.ActiveTxns[0] != 99 {
		t.Fatalf("ActiveTxns = %v, want [99]", cp.ActiveTxns)
	}
}

func TestLastCheckpointNone(t *testing.T) {
	t.Parallel()

	_, _, err := LastCheckpoint([]*LogRecord{{LSN: 0, Type: TypeInsert}})
	if !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("err = %v, want ErrNoCheckpoint", err)
	}
}

func TestCheckpointGCEndToEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir, MaxSegmentSize: 90})
	for i := 0; i < 12; i++ {
		if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("row")}); err != nil {
			t.Fatal(err)
		}
	}
	cp := CheckpointPayload{RedoLSN: 7, ActiveTxns: []uint64{99}}
	if _, err := w.Append(&LogRecord{Type: TypeCheckpoint, Payload: cp.Marshal()}); err != nil {
		t.Fatal(err)
	}
	mustClose(t, w)

	before, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	if len(before) < 2 {
		t.Fatalf("need multiple segments, got %d", len(before))
	}

	records, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := LastCheckpoint(records)
	if err != nil {
		t.Fatal(err)
	}

	w2 := mustOpen(t, Options{Dir: dir, MaxSegmentSize: 90})
	if err := w2.Truncate(payload.RedoLSN); err != nil {
		t.Fatal(err)
	}
	mustClose(t, w2)

	after, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	if len(after) >= len(before) {
		t.Fatalf("Truncate freed no segments: before=%d after=%d", len(before), len(after))
	}
}

func ExampleUnmarshalCheckpoint() {
	p := CheckpointPayload{RedoLSN: 42, ActiveTxns: []uint64{7, 8}}
	got, _ := UnmarshalCheckpoint(p.Marshal())
	fmt.Printf("redo=%d active=%v\n", got.RedoLSN, got.ActiveTxns)
	// Output:
	// redo=42 active=[7 8]
}

func mustOpen(t *testing.T, opts Options) *WAL {
	t.Helper()
	w, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

func mustClose(t *testing.T, w *WAL) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
```

## Review

The payload codec and the GC frontier are correct when they compose. `Marshal` followed by `UnmarshalCheckpoint` must round-trip `RedoLSN` and every entry of `ActiveTxns`; a buffer shorter than twelve bytes must return `ErrShortCheckpoint` and one whose declared count disagrees with its length must return `ErrCheckpointLength`, both satisfying `errors.Is`. `LastCheckpoint` must return the highest-LSN checkpoint in a recovered stream and `ErrNoCheckpoint` when there is none, and the end-to-end test should show `Truncate(redoLSN)` actually reducing the segment count while never touching the active segment.

Common mistakes for this feature. Using a `>=` length check instead of `!=` lets a payload with trailing garbage decode as valid, which silently masks a framing bug; the exact-equality test is deliberate. Forgetting to wrap the sentinels with `%w` breaks `errors.Is` for callers that switch on the failure mode. Scanning for the *first* checkpoint instead of the last hands recovery a stale redo point and replays more than necessary. And truncating to the checkpoint's own LSN rather than its `RedoLSN` would delete segments that recovery still needs, because the redo point is deliberately earlier than the checkpoint in a fuzzy scheme.

## Resources

- [PostgreSQL: WAL Configuration](https://www.postgresql.org/docs/current/wal-configuration.html) — checkpoints, the redo point, and how checkpoint frequency trades recovery time against I/O.
- [SQLite: Write-Ahead Logging](https://www.sqlite.org/wal.html) — checkpointing as the process that lets old log data be reclaimed (section 2.1).
- [ARIES: A Transaction Recovery Method (Mohan et al., 1992)](https://cs.stanford.edu/people/chrismre/cs345/rl/aries.pdf) — fuzzy checkpoints and the active-transaction list a checkpoint must carry.

---

Back to [04-tailing-reader.md](04-tailing-reader.md) | Next: [06-leader-follower-group-commit.md](06-leader-follower-group-commit.md)
