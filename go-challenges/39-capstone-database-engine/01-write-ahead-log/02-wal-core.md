# Exercise 2: The WAL Core

A record format is inert until something assigns each record a place in a total order, writes it to a file, forces that file to disk, and resumes the numbering correctly after a restart. That machine is the WAL core. This exercise builds it: `Open`, `Append`, segment rotation, monotonic LSN assignment, an fsync on every commit, an optional group-commit timer, an empty checkpoint marker, and a `Close` that leaves the log durable. The design problem it solves is concurrency and durability at once — many goroutines may append simultaneously, each record must get a strictly larger LSN than the last, and no caller may be told "committed" until the bytes are actually on the platter.

This module is fully self-contained. It defines its own minimal record type so it stands alone, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
record.go            LogRecord, LSN, RecordType, Encode, Decode, plus readSegment
wal.go               Options, WAL, Open, Append, segment rotation, fsync,
                     group-commit timer, Checkpoint, Truncate, Close
cmd/
  demo/
    main.go          append a transaction, checkpoint, reopen and watch the LSN resume
wal_test.go          monotonic + restart-resumed LSNs, concurrent appends, rotation,
                     the group-commit path, and checkpoint + segment GC
```

- Files: `record.go`, `wal.go`, `cmd/demo/main.go`, `wal_test.go`.
- Implement: `Options`, `WAL`, `Open(Options) (*WAL, error)`, `(*WAL).Append`, `(*WAL).Checkpoint`, `(*WAL).Truncate`, and `(*WAL).Close`.
- Test: `wal_test.go` asserts monotonic and restart-resumed LSNs, that concurrent appends never collide, that the log rotates segments, that the group-commit timer path assigns unique LSNs, and that a checkpoint plus `Truncate` frees old segments.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/01-write-ahead-log/02-wal-core/cmd/demo && cd go-solutions/39-capstone-database-engine/01-write-ahead-log/02-wal-core
```

### The four invariants the core upholds

Most real-world WAL bugs are a violation of one of four invariants, and every later exercise assumes all four hold, so it is worth naming them before the code.

First, LSN assignment is the only work `Append` does under the lock before releasing it. It reads `nextLSN`, stamps the record, increments, and unlocks; the expensive encoding and the I/O happen outside that critical section. Two goroutines can never receive the same LSN because the read-and-increment is serialized, but they do not serialize on the slow disk write. The on-disk byte order still matches LSN order because a second lock around the actual write preserves the order in which LSNs were handed out.

Second, `Open` resumes the counter by scanning. After a restart the in-memory `nextLSN` is gone, so `Open` finds the highest-numbered segment, scans it for the last record's LSN, and sets `nextLSN = last + 1`. Without this a restart would reset LSNs to zero and recovery could not order records across the crash. Note the deliberate simplification: `Open` resumes the counter but does not physically repair a torn tail — that is the recovery path's job (the next exercise), and keeping `Open` cheap is the reason it is split out.

Third, rotation syncs before it closes. `rotate` calls `Sync()` on the full segment, then `Close()`, then opens the next one. The order matters: a crash between the sync and the close leaves the just-filled segment fully durable and cleanly readable, so the segment boundary is itself a durable fact and never a source of lost records. Every segment is opened with `os.O_APPEND`, which makes the kernel move the write cursor to end-of-file atomically before each write — the property that protects previously written records when a file is reopened after a crash, where a plain `os.O_RDWR` would put the cursor at offset 0 and overwrite them.

Fourth, `Truncate` reads the active segment sequence under the lock and never deletes at or above it. The active segment is the file the WAL is currently appending to; deleting it would leave the open handle writing into an unlinked file whose records vanish at the next rotation. Reading `activeSeq` under the mutex and breaking the loop at `seq >= activeSeq` is the guard.

Create `record.go`:

```go
package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
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
// The CRC32 covers bytes [8:end]; the payloadLen and CRC fields at [0:8] are the
// framing envelope and are not checksummed.
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
```

`readSegment` lives in this file because two methods need it: `Open` scans the last segment to resume the LSN counter, and `Truncate` scans a candidate segment to find its highest LSN. It reads the 4-byte length first, then the rest, exactly as the frame is laid out, and treats both `io.EOF` (a clean boundary) and `io.ErrUnexpectedEOF` (a record cut off mid-write) as non-errors that end the scan, returning the records read so far. That tolerance is what lets the same scanner run over a cleanly closed log and a crashed one.

Now the writer itself. Read `Append` as a two-phase operation: a tiny locked phase that assigns the LSN, then an unlocked encode, then a second locked phase (inside `appendDirect` or `flushPending`) that writes and fsyncs. The group-commit timer, when `GroupCommitInterval` is positive, replaces the per-record fsync with a per-tick one: appenders park on a per-write channel and a flusher goroutine drains the queue and syncs once per tick.

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
	"time"
)

const (
	defaultMaxSegmentSize = 64 << 20 // 64 MB
	segmentPattern        = "wal-%06d.log"
)

// Options configures a WAL.
type Options struct {
	// Dir is the directory for WAL segment files. Required.
	Dir string
	// MaxSegmentSize is the maximum segment file size in bytes before rotation.
	// Default: 64 MB.
	MaxSegmentSize int64
	// GroupCommitInterval, if positive, enables the group-commit timer: appended
	// records are accumulated and fsynced on this interval rather than per record.
	GroupCommitInterval time.Duration
}

// WAL is a write-ahead log. It is safe for concurrent use.
type WAL struct {
	opts    Options
	mu      sync.Mutex
	file    *os.File
	segSeq  uint64
	nextLSN LSN
	size    int64

	pending []pendingWrite
	stopCh  chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

type pendingWrite struct {
	data []byte
	lsn  LSN
	done chan error
}

// Open opens (or creates) the WAL at opts.Dir. Existing segments are scanned to
// resume from the correct segment sequence and LSN so sequence numbers are never
// reused across restarts.
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

	if opts.GroupCommitInterval > 0 {
		w.stopCh = make(chan struct{})
		w.wg.Add(1)
		go w.flusher()
	}
	return w, nil
}

// Append serializes r, assigns r.LSN, and persists the record. It is safe for
// concurrent callers. With group commit disabled (the default), the file is
// fsynced before Append returns. With group commit enabled, Append blocks until
// the batch containing this record has been fsynced.
func (w *WAL) Append(r *LogRecord) (LSN, error) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return 0, fmt.Errorf("wal: closed")
	}
	lsn := w.nextLSN
	r.LSN = lsn
	w.nextLSN++
	w.mu.Unlock()

	data, err := r.Encode()
	if err != nil {
		return 0, fmt.Errorf("wal: encode: %w", err)
	}

	if w.opts.GroupCommitInterval > 0 {
		return w.appendGrouped(data, lsn)
	}
	return w.appendDirect(data, lsn)
}

func (w *WAL) appendDirect(data []byte, lsn LSN) (LSN, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.write(data); err != nil {
		return 0, err
	}
	if err := w.file.Sync(); err != nil {
		return 0, fmt.Errorf("wal: fsync: %w", err)
	}
	return lsn, nil
}

func (w *WAL) appendGrouped(data []byte, lsn LSN) (LSN, error) {
	done := make(chan error, 1)
	w.mu.Lock()
	w.pending = append(w.pending, pendingWrite{data: data, lsn: lsn, done: done})
	w.mu.Unlock()
	if err := <-done; err != nil {
		return 0, err
	}
	return lsn, nil
}

// flusher batches accumulated writes and calls fsync once per tick.
func (w *WAL) flusher() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.opts.GroupCommitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.flushPending()
		case <-w.stopCh:
			w.flushPending() // drain any writes queued before Close
			return
		}
	}
}

func (w *WAL) flushPending() {
	w.mu.Lock()
	batch := w.pending
	w.pending = nil
	w.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	var flushErr error
	w.mu.Lock()
	for _, pw := range batch {
		if err := w.write(pw.data); err != nil {
			flushErr = err
			break
		}
	}
	if flushErr == nil {
		if err := w.file.Sync(); err != nil {
			flushErr = fmt.Errorf("wal: fsync: %w", err)
		}
	}
	w.mu.Unlock()

	for _, pw := range batch {
		pw.done <- flushErr
	}
}

// write appends data to the active segment, rotating when the configured maximum
// size would be exceeded. The caller must hold w.mu.
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

// Checkpoint writes a TypeCheckpoint record and returns its LSN. The baseline
// marker carries no payload; the checkpoint-payloads exercise gives it a redo
// point and an active-transaction list.
func (w *WAL) Checkpoint() (LSN, error) {
	return w.Append(&LogRecord{Type: TypeCheckpoint})
}

// Truncate deletes all WAL segment files in which every record has an LSN
// strictly less than upToLSN. The currently active segment is never deleted.
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

// Close syncs and closes the WAL. After Close returns, concurrent Appends will
// receive an error. Close is idempotent.
func (w *WAL) Close() error {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()

	if w.stopCh != nil {
		close(w.stopCh)
		w.wg.Wait()
		w.stopCh = nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()
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

// listSegmentSeqs returns the sequence numbers of all WAL segment files in dir,
// sorted ascending.
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

// scanLastLSN returns the LSN of the last valid record in the segment at path,
// or 0 for an empty or unreadable segment.
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

Two details in `Close` are worth a second look. It sets `stopped` first so any concurrent `Append` bails out with an error rather than racing into a closing file, then it stops the flusher (draining whatever was queued) before the final `Sync()`/`Close()`. Setting `stopCh` back to nil after the wait keeps `Close` idempotent: a second call sees a nil channel and a nil file and returns cleanly. The group-commit path here is the timer design from the concepts chapter — simple, with latency bounded by one tick. The leader/follower exercise builds the alternative, timer-free design as a standalone component.

### The runnable demo

This demo writes a small committed transaction, takes a checkpoint, closes the log, then reopens it and appends once more so you can watch the LSN counter resume across the restart instead of starting over at zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/wal-core"
)

func main() {
	dir, err := os.MkdirTemp("", "wal-core-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		log.Fatal(err)
	}
	a, _ := w.Append(&wal.LogRecord{TxID: 1, Type: wal.TypeInsert, Payload: []byte("alice")})
	b, _ := w.Append(&wal.LogRecord{TxID: 1, Type: wal.TypeInsert, Payload: []byte("bob")})
	c, _ := w.Append(&wal.LogRecord{TxID: 1, Type: wal.TypeCommit})
	cp, _ := w.Checkpoint()
	fmt.Printf("appended LSNs: %d %d %d, checkpoint %d\n", a, b, c, cp)
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	// Reopen: the LSN counter resumes from where it left off.
	w2, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		log.Fatal(err)
	}
	d, _ := w2.Append(&wal.LogRecord{TxID: 2, Type: wal.TypeInsert, Payload: []byte("carol")})
	fmt.Printf("after reopen, next LSN: %d\n", d)
	if err := w2.Close(); err != nil {
		log.Fatal(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
appended LSNs: 0 1 2, checkpoint 3
after reopen, next LSN: 4
```

### Tests

These tests exercise each invariant directly: monotonic LSNs over a single goroutine, an LSN that resumes after a close and reopen, fifty goroutines appending without a single duplicate LSN, a small segment size that forces rotation (counted back via `readSegment`), the timer group-commit path producing unique LSNs under concurrency, and a checkpoint plus `Truncate` that actually deletes segments. The `mustOpen`/`mustClose` helpers are reused throughout.

Create `wal_test.go`:

```go
package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLSNIsMonotonicallyIncreasing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})
	defer mustClose(t, w)

	var prev LSN
	for i := 0; i < 100; i++ {
		lsn, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("x")})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if i > 0 && lsn <= prev {
			t.Fatalf("LSN not monotonic at record %d: %d <= %d", i, lsn, prev)
		}
		prev = lsn
	}
}

func TestLSNResumedAfterReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})
	last, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, w)

	w2 := mustOpen(t, Options{Dir: dir})
	defer mustClose(t, w2)
	next, err := w2.Append(&LogRecord{Type: TypeInsert, Payload: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	if next <= last {
		t.Fatalf("LSN after reopen: %d <= %d (last before close)", next, last)
	}
}

func TestConcurrentAppendNoDuplicateLSN(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir})
	defer mustClose(t, w)

	const goroutines = 50
	const perG = 20
	lsns := make(chan LSN, goroutines*perG)
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				lsn, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("concurrent")})
				if err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				lsns <- lsn
			}
		}()
	}
	wg.Wait()
	close(lsns)

	seen := make(map[LSN]bool, goroutines*perG)
	for lsn := range lsns {
		if seen[lsn] {
			t.Errorf("duplicate LSN: %d", lsn)
		}
		seen[lsn] = true
	}
	if len(seen) != goroutines*perG {
		t.Errorf("expected %d unique LSNs, got %d", goroutines*perG, len(seen))
	}
}

func TestSegmentRotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Each record: headerSize(25) + 4 bytes payload = 29 bytes; limit 100 -> ~3/seg.
	w := mustOpen(t, Options{Dir: dir, MaxSegmentSize: 100})

	const total = 10
	for i := 0; i < total; i++ {
		if _, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("data")}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	mustClose(t, w)

	seqs, err := listSegmentSeqs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(seqs))
	}

	got := 0
	for _, seq := range seqs {
		recs, _, err := readSegment(filepath.Join(dir, fmt.Sprintf(segmentPattern, seq)))
		if err != nil {
			t.Fatalf("readSegment seq %d: %v", seq, err)
		}
		got += len(recs)
	}
	if got != total {
		t.Fatalf("scanned %d records across segments, want %d", got, total)
	}
}

func TestGroupCommitNoDuplicateLSN(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir, GroupCommitInterval: 10 * time.Millisecond})
	defer mustClose(t, w)

	const goroutines = 20
	const perG = 10
	lsns := make(chan LSN, goroutines*perG)
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				lsn, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("gc")})
				if err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				lsns <- lsn
			}
		}()
	}
	wg.Wait()
	close(lsns)

	seen := make(map[LSN]bool, goroutines*perG)
	for lsn := range lsns {
		if seen[lsn] {
			t.Errorf("duplicate LSN in group commit: %d", lsn)
		}
		seen[lsn] = true
	}
}

func TestCheckpointAndTruncateFreesSegments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := mustOpen(t, Options{Dir: dir, MaxSegmentSize: 100})

	var midLSN LSN
	for i := 0; i < 12; i++ {
		lsn, err := w.Append(&LogRecord{Type: TypeInsert, Payload: []byte("row")})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if i == 5 {
			midLSN = lsn
		}
	}
	if _, err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	mustClose(t, w)

	before, _ := listSegmentSeqs(dir)
	if len(before) < 2 {
		t.Fatalf("need at least 2 segments for this test, got %d", len(before))
	}

	w2 := mustOpen(t, Options{Dir: dir, MaxSegmentSize: 100})
	if err := w2.Truncate(midLSN); err != nil {
		t.Fatal(err)
	}
	mustClose(t, w2)

	after, _ := listSegmentSeqs(dir)
	if len(after) >= len(before) {
		t.Errorf("Truncate removed no segments: before=%d, after=%d", len(before), len(after))
	}
}

func ExampleOpen() {
	dir, err := os.MkdirTemp("", "wal-example-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)

	w, err := Open(Options{Dir: dir})
	if err != nil {
		return
	}
	lsn1, _ := w.Append(&LogRecord{TxID: 1, Type: TypeInsert, Payload: []byte(`{"id":1}`)})
	lsn2, _ := w.Append(&LogRecord{TxID: 1, Type: TypeCommit})
	w.Close()

	fmt.Printf("insert LSN=%d commit LSN=%d\n", lsn1, lsn2)
	// Output:
	// insert LSN=0 commit LSN=1
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

The core is correct when its four invariants hold at once. LSN assignment must be the only work `Append` does under the lock in its first phase, with encoding and I/O outside it, which the concurrent-append test confirms by passing under `-race` with no duplicate LSN. `Open` must resume `nextLSN` by scanning the last segment, which the close-reopen-append test verifies by observing a next LSN strictly greater than the last before close. `rotate` must sync the full segment before closing it and every segment must be opened with `os.O_APPEND`, which the rotation test checks by recounting every record across segments with none lost on a boundary. And `Truncate` must read `activeSeq` under the lock and break at `seq >= activeSeq` so it never deletes the active file, which the checkpoint test confirms through a real drop in segment count.

Common mistakes for this feature. Assigning the LSN outside the lock lets two appenders collide, which the duplicate-LSN test catches immediately. Opening a segment without `os.O_APPEND` puts the cursor at offset 0 on reopen and silently overwrites earlier records — invisible in a single clean run, which is exactly why the restart-resume test matters. Closing a segment before syncing it can lose the tail of the just-filled file on a boundary crash, so the sync-then-close order in `rotate` is load-bearing, not incidental. And deleting the active segment in `Truncate` succeeds while the handle stays open, then loses those records at the next rotation; the `seq >= activeSeq` break is the guard against it.

## Resources

- [PostgreSQL: WAL Internals](https://www.postgresql.org/docs/current/wal-internals.html) — segment files, LSNs, and the durability rules a real WAL core upholds.
- [SQLite: Write-Ahead Logging](https://www.sqlite.org/wal.html) — a second production WAL design, including how appends and log growth are managed.
- [`os` package](https://pkg.go.dev/os) — `O_APPEND` for atomic end-of-file writes and `File.Sync` for forcing bytes to stable storage.

---

Back to [01-record-encoding.md](01-record-encoding.md) | Next: [03-crash-recovery.md](03-crash-recovery.md)
