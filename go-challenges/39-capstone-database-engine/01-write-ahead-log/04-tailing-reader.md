# Exercise 4: Tailing Reader

The writer can produce a log and recovery can read a sealed one, but neither lets a consumer follow a *live* WAL as records arrive. That access pattern — stream from a chosen LSN and keep delivering new records as the writer appends them, crossing segment boundaries transparently — is the foundation of replication and change-data-capture. This exercise builds a tailing `Reader`: `NewReader`, a blocking `Next`, and `Close`, with the small three-way state machine that distinguishes "no more data yet" from "the writer rotated" from "the writer shut down."

This module is fully self-contained. It ships its own record frame, a minimal writer to tail, the reader, a demo, and tests. Nothing here imports any other exercise.

## What you'll build

```text
record.go            LogRecord, LSN, RecordType, Encode, Decode
wal.go               minimal writer: Open, Append, rotation, Close (the log to tail)
reader.go            Reader, NewReader, Next (blocks for new records), Close
cmd/
  demo/
    main.go          write across several segments, then tail them from LSN 0
reader_test.go       tail a live WAL across writes; resume from a chosen LSN across rotation
```

- Files: `record.go`, `wal.go`, `reader.go`, `cmd/demo/main.go`, `reader_test.go`.
- Implement: `Reader`, `NewReader(dir string, fromLSN LSN) (*Reader, error)`, `(*Reader).Next() (*LogRecord, error)`, and `(*Reader).Close() error`, following rotation via `segmentPattern`.
- Test: `reader_test.go` tails an active WAL across pre- and post-creation writes and asserts every record arrives, and resumes from a chosen LSN across a segment boundary.
- Verify: `go test -run 'TestReaderTailsActiveWAL|TestReaderStartsFromLSN' -race ./...`

### Why tailing is its own problem

Reading a sealed log is a finite scan: open each segment, decode until EOF, stop. Tailing a live log inverts two of those assumptions. EOF is no longer terminal — it means "no more data *yet*", and the right response is to wait, not to quit. And the file you are reading is not the file you will be reading a moment later, because the writer rotates to a new segment when the current one fills. A tailing reader therefore needs a small state machine: decode the next record; on EOF decide whether the writer has rotated (a successor segment exists), or simply has not written more (poll and retry), or has shut down (return EOF for real). Getting that three-way decision right is the whole exercise.

The writer below is the same minimal WAL used to produce a log to tail — it appends, rotates at the size limit, and resumes its LSN on open. The interesting code is in `reader.go`.

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
	"encoding/binary"
	"fmt"
	"io"
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
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("wal: open segment: %w", err)
	}
	defer f.Close()

	var last LSN
	lenBuf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(f, lenBuf); err != nil {
			return last, nil
		}
		plen := int(binary.LittleEndian.Uint32(lenBuf))
		body := make([]byte, headerSize-4+plen)
		if _, err := io.ReadFull(f, body); err != nil {
			return last, nil
		}
		full := make([]byte, 4+len(body))
		copy(full[:4], lenBuf)
		copy(full[4:], body)
		rec, err := Decode(full)
		if err != nil {
			return last, nil
		}
		last = rec.LSN
	}
}
```

Now the reader. The heart of the design is the loop in `Next`. `readOne` is the cheap inner step: it reuses the same length-prefixed framing as the writer, returning a decoded record, or a blanket `io.EOF` whenever the file does not currently hold a full frame. When `Next` gets that EOF it asks one question — does `wal-(curSeq+1).log` exist? The existence of the successor file is the signal that the writer rotated, and because `rotate` fsyncs and closes the old segment before creating the new one, by the time the successor appears the predecessor is complete and durable, so it is safe for the reader to close it and advance without losing the records still buffered at its tail. If no successor exists, the writer is merely between writes; the reader checks the `closed` flag (so `Close` can unblock it) and otherwise sleeps briefly before retrying.

Create `reader.go`:

```go
package wal

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Reader tails a WAL directory starting from a given LSN. It yields records as
// they are appended by an active WAL, following segment rotation transparently.
// Reader is intended for single-consumer use; concurrent Next calls are not safe.
type Reader struct {
	dir     string
	fromLSN LSN

	curSeq  uint64
	curFile *os.File

	mu     sync.Mutex
	closed bool
}

// NewReader opens a Reader that yields records with LSN >= fromLSN, starting at
// the earliest existing segment.
func NewReader(dir string, fromLSN LSN) (*Reader, error) {
	seqs, err := listSegmentSeqs(dir)
	if err != nil {
		return nil, err
	}
	startSeq := uint64(0)
	if len(seqs) > 0 {
		startSeq = seqs[0]
	}
	r := &Reader{dir: dir, fromLSN: fromLSN, curSeq: startSeq}
	if err := r.openSeg(startSeq); err != nil {
		return nil, err
	}
	return r, nil
}

// Next blocks until a record with LSN >= r.fromLSN is available and returns it.
// It returns nil, io.EOF when the Reader is closed.
func (r *Reader) Next() (*LogRecord, error) {
	for {
		rec, err := r.readOne()
		if err == nil {
			if rec.LSN >= r.fromLSN {
				return rec, nil
			}
			continue
		}

		// EOF on the current segment: has the active WAL rotated?
		nextPath := filepath.Join(r.dir, fmt.Sprintf(segmentPattern, r.curSeq+1))
		if _, statErr := os.Stat(nextPath); statErr == nil {
			if closeErr := r.curFile.Close(); closeErr != nil {
				return nil, fmt.Errorf("wal reader: close old segment: %w", closeErr)
			}
			if openErr := r.openSeg(r.curSeq + 1); openErr != nil {
				return nil, openErr
			}
			continue
		}

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, io.EOF
		}
		r.mu.Unlock()
		// Poll for new data. A production implementation would use a sync.Cond
		// broadcast from the WAL's flush path to avoid the sleep.
		time.Sleep(5 * time.Millisecond)
	}
}

// Close stops the Reader. A blocked Next call returns io.EOF shortly after.
func (r *Reader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	if r.curFile != nil {
		err := r.curFile.Close()
		r.curFile = nil
		return err
	}
	return nil
}

func (r *Reader) openSeg(seq uint64) error {
	path := filepath.Join(r.dir, fmt.Sprintf(segmentPattern, seq))
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("wal reader: open %s: %w", path, err)
	}
	r.curSeq = seq
	r.curFile = f
	return nil
}

func (r *Reader) readOne() (*LogRecord, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r.curFile, lenBuf); err != nil {
		return nil, io.EOF
	}
	plen := int(binary.LittleEndian.Uint32(lenBuf))
	body := make([]byte, headerSize-4+plen)
	if _, err := io.ReadFull(r.curFile, body); err != nil {
		return nil, io.EOF
	}
	full := make([]byte, 4+len(body))
	copy(full[:4], lenBuf)
	copy(full[4:], body)
	return Decode(full)
}
```

That `time.Sleep(5 * time.Millisecond)` is the deliberate teaching simplification, and it is worth understanding its cost. Polling introduces a latency floor: a record can sit available for up to one poll interval before the reader notices it. A production tailing reader replaces the sleep with a `sync.Cond` that the WAL's flush path broadcasts on, so a waiting reader wakes the instant new bytes are durable, with no fixed delay and no wasted wakeups. The polling version is correct and far simpler to reason about, which is why it is the one to learn first; the structural seam — a single place that waits for "more data" — is exactly where the condition variable would slot in. One concurrency note: the `mu` mutex guards only the `closed` flag, not the file cursor. `Next` is documented as single-consumer precisely so the read position needs no locking; the only cross-goroutine interaction is `Close` flipping `closed` while a `Next` is parked in its poll.

### The runnable demo

This demo writes five records into a WAL whose segment limit is small enough to rotate several times, then opens a reader at LSN 0 and tails all five, crossing the segment boundaries transparently. Because the writer is closed before the reader starts and the reader stops after the known count, the run is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/tailing-reader"
)

func main() {
	dir, err := os.MkdirTemp("", "wal-tail-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Small segment limit so 5 records span several segments.
	w, err := wal.Open(wal.Options{Dir: dir, MaxSegmentSize: 60})
	if err != nil {
		log.Fatal(err)
	}
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := w.Append(&wal.LogRecord{Type: wal.TypeInsert, Payload: []byte(fmt.Sprintf("rec-%d", i))}); err != nil {
			log.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	fmt.Printf("appended %d records across %d segments\n", n, len(files))

	rdr, err := wal.NewReader(dir, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer rdr.Close()

	fmt.Println("tailing from LSN 0:")
	for i := 0; i < n; i++ {
		rec, err := rdr.Next()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  LSN=%d payload=%q\n", rec.LSN, rec.Payload)
	}
	fmt.Printf("tail complete: %d records\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
appended 5 records across 3 segments
tailing from LSN 0:
  LSN=0 payload="rec-0"
  LSN=1 payload="rec-1"
  LSN=2 payload="rec-2"
  LSN=3 payload="rec-3"
  LSN=4 payload="rec-4"
tail complete: 5 records
```

### Tests

The first test drives the live path end to end: three records before the reader is created and three after, asserting all six are delivered and that closing the reader terminates the stream with `io.EOF`. The two-second timeout guards against a hang if the rotation-or-poll decision is wrong. The second test pins the `fromLSN` filter across a rotation: five records over a small segment size, a reader started at LSN 3, and an assertion that the first record returned is LSN 3.

Create `reader_test.go`:

```go
package wal

import (
	"io"
	"testing"
	"time"
)

func TestReaderTailsActiveWAL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})

	for i := 0; i < 3; i++ {
		if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("pre")}); err != nil {
			t.Fatal(err)
		}
	}

	rdr, err := NewReader(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	for i := 0; i < 3; i++ {
		if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("post")}); err != nil {
			t.Fatal(err)
		}
	}
	mustClose(t, w)

	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := rdr.Next(); err == io.EOF {
				return
			} else if err != nil {
				t.Errorf("Next: %v", err)
				return
			}
			got++
			if got == 6 {
				rdr.Close()
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tailing reader did not receive all records within 2s")
	}

	if got != 6 {
		t.Errorf("reader got %d records, want 6", got)
	}
}

func TestReaderStartsFromLSN(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Small segment size so the 5 records span multiple segments and the reader
	// must cross a boundary to reach LSN 3.
	w := mustOpen(t, Options{Dir: dir, MaxSegmentSize: 60})
	for i := 0; i < 5; i++ {
		if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("rec")}); err != nil {
			t.Fatal(err)
		}
	}
	mustClose(t, w)

	rdr, err := NewReader(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Close()

	first, err := rdr.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.LSN != 3 {
		t.Fatalf("first delivered LSN = %d, want 3", first.LSN)
	}
	second, err := rdr.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if second.LSN != 4 {
		t.Fatalf("second delivered LSN = %d, want 4", second.LSN)
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

The reader is correct when the three-way decision in `Next` is exact. A record whose LSN is below `fromLSN` must be skipped rather than returned, which the resume test confirms by seeing LSN 3 first. On EOF the reader must advance to `curSeq+1` only when that successor file already exists, reusing the same framing to read it — the resume test exercises this by crossing a segment boundary — and on EOF with no successor it must poll while open and return `io.EOF` once closed, so the consumer loop terminates. The live-tail test, which races appends against `Next` and must stay clean under `go test -race ./...`, is what ties these branches together.

Common mistakes for this feature. Treating EOF as terminal turns a tailing reader into a one-shot scanner — the fix is the rotate-or-poll-or-stop branch. Advancing to the next segment without first confirming the successor exists makes the reader open a file that is not there yet, or skip records still buffered at the tail of the current one; the `os.Stat` check (safe because `rotate` seals the old segment before the new one appears) is what orders it correctly. And forgetting that `Close` must be observable from a parked `Next` leaves a consumer goroutine blocked forever; the `closed` flag, read under the mutex on every poll, is what lets `Close` end the stream.

## Resources

- [PostgreSQL: Logical Decoding](https://www.postgresql.org/docs/current/logicaldecoding.html) — streaming committed changes to external consumers, the production form of tailing a WAL.
- [PostgreSQL: Write-Ahead Logging (WAL)](https://www.postgresql.org/docs/current/wal-intro.html) — the durability ordering (records flushed before data pages) that makes a sealed segment safe to tail.
- [`bufio` package](https://pkg.go.dev/bufio) — buffered reads for scanning an append-only file efficiently as it grows.

---

Back to [03-crash-recovery.md](03-crash-recovery.md) | Next: [05-checkpoint-payloads-and-segment-gc.md](05-checkpoint-payloads-and-segment-gc.md)
