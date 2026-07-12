# Exercise 3: Crash Recovery

A WAL that can write a crash-resumable log is only half a durability story. The other half is the moment after the lights come back on: turning a directory of segment files — the last of which the process may have died in the middle of writing — back into a clean, ordered list of records, repairing the torn tail in place so the next open can append safely. This exercise builds `Recover`, the multi-segment driver that replays every durable record in order and applies the one positional rule that makes crash recovery correct: a bad record at the very end of the last segment is an expected crash artifact, while corruption anywhere earlier is a different and more serious matter.

This module is fully self-contained. It ships its own record frame, a minimal writer, the segment scanner, and the recovery driver, plus a demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
record.go            LogRecord, LSN, RecordType, Encode, Decode
wal.go               minimal writer: Open, Append (fsync), rotation, Checkpoint, Truncate, Close
recovery.go          readSegment (single-file scanner) + Recover (multi-segment driver)
cmd/
  demo/
    main.go          write a transaction, checkpoint, close, recover, then truncate
recover_test.go      append+recover round-trip, partial-tail repair, corrupt-CRC repair
```

- Files: `record.go`, `wal.go`, `recovery.go`, `cmd/demo/main.go`, `recover_test.go`.
- Implement: `readSegment(path string) ([]*LogRecord, int, error)` and `Recover(dir string) ([]*LogRecord, error)`, over the writer's `listSegmentSeqs` and `segmentPattern`.
- Test: `recover_test.go` asserts all appended records come back, that a sub-header garbage tail is truncated, and that a full-length record with a flipped CRC is dropped.
- Verify: `go test -run 'TestAppendAndRecover|TestCrashRecovery' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/01-write-ahead-log/03-crash-recovery/cmd/demo && cd go-solutions/39-capstone-database-engine/01-write-ahead-log/03-crash-recovery
```

### The scanner and the driver are two different jobs

Recovery splits cleanly into a byte-level job and a policy-level job, and keeping them in separate functions is what makes each one simple. The byte-level job is `readSegment`: scan one file record by record and report two things — the records it could decode, and the offset where it stopped. It stops on one of three conditions: a clean `io.EOF` exactly on a record boundary, an `io.ErrUnexpectedEOF` partway through a length prefix or body (a record cut off mid-write), or a frame that arrived complete but fails its CRC. Crucially, `readSegment` returns all three as `(records, stopOffset, nil)` — not as errors — because at the tail of a crashed log they are all expected, and it deliberately does not decide whether a given stop is acceptable.

That decision is the policy-level job, and it depends on information `readSegment` does not have: which segment in the directory this file is. A WAL directory is an ordered sequence `wal-000000.log`, `wal-000001.log`, and so on, and only the highest-numbered file can legitimately end in a torn write, because it is the one the process was appending to when it died. Every earlier segment was sealed and fsynced by `rotate` before the next was opened, so a short or corrupt record inside one of them is not a crash artifact — it is data that was acknowledged as durable and is now unreadable. `Recover` is the layer that knows the segment order and therefore can apply that policy: truncate the torn tail of the final segment, trust everything before it.

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

// headerSize is the fixed record header:
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
// The CRC32 covers bytes [8:end]; the [0:8] framing envelope is not checksummed.
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

The writer below is a focused version of the WAL core: it fsyncs every append, rotates segments at the size limit, syncs before it closes a full segment, and resumes its LSN counter on open. It exists here so the demo and tests have something to crash and recover. Its `Open` uses `scanLastLSN` (which calls `readSegment`) to resume.

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

// Open opens (or creates) the WAL at opts.Dir, resuming the LSN counter from the
// last record of the highest-numbered existing segment.
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

// Checkpoint writes an empty TypeCheckpoint marker and returns its LSN.
func (w *WAL) Checkpoint() (LSN, error) {
	return w.Append(&LogRecord{Type: TypeCheckpoint})
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

Now the two recovery functions. Read `readSegment` first: it is the byte-level scanner, and the distinction between `io.EOF` and `io.ErrUnexpectedEOF` is load-bearing. `io.EOF` means zero bytes were available at a clean boundary — a normal end of file. `io.ErrUnexpectedEOF` means some but not all of the requested bytes were there — a record cut off mid-write, the exact crash signature the WAL must tolerate. Both end the scan by returning the records read so far and the start offset of the incomplete record, with a nil error. A `Decode` failure (a complete frame whose CRC fails) is handled identically, because at the tail it is just a torn write of a whole record.

Then read `Recover`: it enumerates segments in ascending order, accumulates their records, and truncates only the final segment, to the exact offset where `readSegment` stopped. When the final segment ended cleanly, that offset equals the file size and `os.Truncate` is a no-op, so the common case costs nothing; when it ended in half a header, the garbage is removed and the file is left appendable.

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

// readSegment reads all valid records from a single segment file. It returns the
// decoded records and the byte offset at which the first corrupt or partial
// record begins (equal to the clean end-of-file offset when the segment ends on
// a record boundary). A non-nil error is returned only for unexpected I/O
// failures; a torn or corrupt tail is reported through the offset, not the error.
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
			// io.EOF = clean boundary; io.ErrUnexpectedEOF = truncated length field.
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return records, startOffset, nil
			}
			return nil, -1, fmt.Errorf("wal: read length at offset %d: %w", startOffset, err)
		}

		plen := int(binary.LittleEndian.Uint32(lenBuf))
		bodyLen := headerSize - 4 + plen // remaining header bytes + payload
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(f, body); err != nil {
			if err == io.ErrUnexpectedEOF {
				return records, startOffset, nil // truncated record body
			}
			return nil, -1, fmt.Errorf("wal: read record body at offset %d: %w", startOffset, err)
		}

		full := make([]byte, 4+bodyLen)
		copy(full[:4], lenBuf)
		copy(full[4:], body)

		rec, err := Decode(full)
		if err != nil {
			// CRC failure: treat as a corrupt partial write at this offset.
			return records, startOffset, nil
		}
		records = append(records, rec)
		offset = startOffset + 4 + bodyLen
	}
}

// Recover reads all WAL segments from dir in order and returns every valid
// LogRecord. A partial or corrupt record at the tail of the final segment is
// truncated and the segment is repaired in-place. Because rotate seals and
// fsyncs a segment before opening the next, a clean log never has a bad record
// before the final segment. Recover is idempotent: a second run returns the same
// records and its truncate is a no-op.
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
		// Only the last segment may have a partial tail; truncate it in-place.
		if i == len(seqs)-1 && tailOffset >= 0 {
			if err := os.Truncate(path, int64(tailOffset)); err != nil {
				return nil, fmt.Errorf("wal: truncate tail of %s: %w", path, err)
			}
		}
	}
	return all, nil
}
```

Two design honesties are worth stating plainly. First, `Recover` truncates only the last segment; if an earlier segment somehow stops early, the records after the stop are silently dropped from the returned slice but the file is left untouched. The writer treats that as a should-never-happen because rotation makes sealed segments durable, and a stricter engine would promote a non-final stop to a hard error — the torn-write exercise builds the pure, total version of that stricter check. Second, the `err` that `Recover` checks for is only a genuine I/O failure (an unreadable file), because `readSegment` reports torn and corrupt tails through the offset, not through `err`. That separation is why the same scanner serves both the LSN-resume path and the recovery path.

### The runnable demo

A test proves a property; a demo shows the lifecycle. This one writes a small committed transaction, checkpoints, closes, recovers (printing what came back), then reopens and truncates below the checkpoint — the full write -> recover -> truncate arc in one readable run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/crash-recovery"
)

func main() {
	dir, err := os.MkdirTemp("", "wal-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		log.Fatal(err)
	}

	lsn1, _ := w.Append(&wal.LogRecord{TxID: 1, Type: wal.TypeInsert, Payload: []byte(`{"id":1,"name":"Alice"}`)})
	lsn2, _ := w.Append(&wal.LogRecord{TxID: 1, Type: wal.TypeInsert, Payload: []byte(`{"id":2,"name":"Bob"}`)})
	lsn3, _ := w.Append(&wal.LogRecord{TxID: 1, Type: wal.TypeCommit})
	fmt.Printf("wrote: insert LSN=%d, insert LSN=%d, commit LSN=%d\n", lsn1, lsn2, lsn3)

	cpLSN, err := w.Checkpoint()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("checkpoint LSN=%d\n", cpLSN)

	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	// Simulate crash recovery.
	records, err := wal.Recover(dir)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recovered %d records:\n", len(records))
	for _, r := range records {
		fmt.Printf("  LSN=%-3d TxID=%d Type=%d payload=%q\n", r.LSN, r.TxID, r.Type, r.Payload)
	}

	// Reopen and truncate segments whose records are all below the checkpoint.
	w2, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		log.Fatal(err)
	}
	if err := w2.Truncate(cpLSN); err != nil {
		log.Fatal(err)
	}
	w2.Close()
	fmt.Println("truncation complete")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote: insert LSN=0, insert LSN=1, commit LSN=2
checkpoint LSN=3
recovered 4 records:
  LSN=0   TxID=1 Type=1 payload="{\"id\":1,\"name\":\"Alice\"}"
  LSN=1   TxID=1 Type=1 payload="{\"id\":2,\"name\":\"Bob\"}"
  LSN=2   TxID=1 Type=4 payload=""
  LSN=3   TxID=0 Type=6 payload=""
truncation complete
```

### Tests

The three tests pin the three outcomes that matter: a clean round-trip, a sub-header garbage tail (fewer than 25 bytes, so the length read itself is short), and a full-length record whose CRC byte is flipped. The last two append their corruption directly to the segment file with `os.O_APPEND` to simulate exactly what a crash leaves behind, then assert that `Recover` returns only the good records.

Create `recover_test.go`:

```go
package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndRecover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})

	payloads := []string{"hello", "world", "foo"}
	for _, p := range payloads {
		if _, err := w.Append(&LogRecord{TxID: 1, Type: TypeInsert, Payload: []byte(p)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	mustClose(t, w)

	records, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(records) != len(payloads) {
		t.Fatalf("len(records) = %d, want %d", len(records), len(payloads))
	}
	for i, r := range records {
		if string(r.Payload) != payloads[i] {
			t.Errorf("record[%d].Payload = %q, want %q", i, r.Payload, payloads[i])
		}
	}
}

func TestCrashRecoveryTruncatesPartialTail(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})

	for i := 0; i < 2; i++ {
		if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("ok")}); err != nil {
			t.Fatal(err)
		}
	}
	mustClose(t, w)

	// Simulate a crash: append 10 bytes of garbage (less than the 25-byte header).
	seg := filepath.Join(dir, fmt.Sprintf(segmentPattern, 0))
	f, err := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 10)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	records, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records after recovery, got %d", len(records))
	}
}

func TestCrashRecoveryTruncatesCorruptCRC(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})

	if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("good")}); err != nil {
		t.Fatal(err)
	}
	mustClose(t, w)

	// Corrupt: append a full-length record with a flipped CRC byte.
	bad := LogRecord{LSN: 99, TxID: 0, Type: TypeInsert, Payload: []byte("bad")}
	enc, _ := bad.Encode()
	enc[5] ^= 0xFF // flip a byte in the CRC field

	seg := filepath.Join(dir, fmt.Sprintf(segmentPattern, 0))
	f, err := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(enc); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	records, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if string(records[0].Payload) != "good" {
		t.Fatalf("wrong payload: %q", records[0].Payload)
	}
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

Recovery is correct when the scanner and the driver keep their jobs separate. `readSegment` should return torn and corrupt tails as `(records, offset, nil)` and reserve a non-nil error for a genuine I/O failure such as an unreadable file, while `Recover` should truncate only the final segment, to the exact stop offset, so a clean segment yields a no-op truncate. Confirm the append-and-recover round-trip returns every record in order, that the partial-tail and corrupt-CRC tests return only the good records, and that running `Recover` twice on the same directory is stable — the second run returns the same records and changes nothing on disk.

Common mistakes for this feature. Treating a CRC failure on the last record as fatal makes every crash unrecoverable, because a mid-write crash always leaves a torn or bad-CRC tail; the fix is to distinguish position, truncating a tail stop while treating a stop before the final segment as data loss. Truncating to the wrong offset — for instance the file size instead of the record-start offset — either leaves the garbage in place or eats a good record, so the truncation must use exactly the offset `readSegment` reports. And forgetting idempotence (for example by recording recovery state that makes a second run diverge) breaks the property that a recovery process can itself crash and be retried.

## Resources

- [ARIES: A Transaction Recovery Method (Mohan et al., 1992)](https://cs.stanford.edu/people/chrismre/cs345/rl/aries.pdf) — the foundational paper on write-ahead logging and crash recovery: redo/undo, the analysis pass, and the torn tail.
- [PostgreSQL: WAL Internals](https://www.postgresql.org/docs/current/wal-internals.html) — how a real server finds the checkpoint and replays (REDO) forward after a crash.
- [CMU 15-445: Intro to Database Systems](https://15445.courses.cs.cmu.edu/) — its logging-and-recovery lectures cover checkpoints and ARIES-style recovery in depth.

---

Back to [02-wal-core.md](02-wal-core.md) | Next: [04-tailing-reader.md](04-tailing-reader.md)
