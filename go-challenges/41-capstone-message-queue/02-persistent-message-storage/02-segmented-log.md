# Exercise 2: Segmented Log with a Sparse Offset Index

A single record is a fact in isolation; a queue needs a sequence of them on disk that survives restarts, rolls over into bounded files, and can locate the message at any offset without scanning from the start. This exercise builds that engine: a `Log` over a directory of append-only segment files, each carrying an in-memory sparse offset index, so that appending is a sequential write and reading offset O is a binary search followed by a short forward scan.

This module is fully self-contained. It bundles its own copy of the message encoding from Exercise 1, defines the segment and the log, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
store.go             Message, Header, Encode, Decode, error sentinels (from Exercise 1)
segment.go           segment: append, sparse index, read by offset (binary search + scan)
log.go               Log: Open, Append, Read, rotation, TruncateBefore, offsets, Close
cmd/
  demo/
    main.go          append messages across rotated segments, then read each back
log_test.go          append/read round-trip, rotation, sparse-index seek, truncation
```

- Files: `store.go`, `segment.go`, `log.go`, `cmd/demo/main.go`, `log_test.go`.
- Implement: `segment` with `append`/`read` and a sparse index; `Log` with `Open`, `Append`, `Read`, `RollSegment`, `TruncateBefore`, `ListSegments`, `LowestOffset`, `HighestOffset`, `Close`.
- Test: `log_test.go` round-trips many messages, forces rotation, exercises the sparse-index seek with a small interval, and checks `TruncateBefore` drops sealed segments.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/02-persistent-message-storage/02-segmented-log/cmd/demo && cd go-solutions/41-capstone-message-queue/02-persistent-message-storage/02-segmented-log
```

### How the segment lays records on disk

Each segment owns a single `.log` file opened with `os.O_APPEND`, so the kernel moves the write cursor to the end before every write — even after the file is reopened on restart — which is what guarantees a new append never overwrites existing data. A record is written as a 4-byte big-endian length prefix followed by the encoded message. The length prefix is the framing that lets a reader (and recovery) advance record by record with two `io.ReadFull` calls and never decode a field just to find the next boundary.

The segment tracks three numbers under its own `sync.RWMutex`: `baseOffset`, the absolute offset of its first message and the value in its file name; `nextOffset`, the offset the next append will receive; and `size`, the current byte length of the log file, which doubles as the file position of the next record. The absolute offset of a message is therefore `baseOffset` plus its position in the segment, and offsets increase monotonically across the whole log without ever repeating.

### The sparse index and the O(log n) seek

A dense index — one entry per message — would be as large as the log. The segment keeps a *sparse* one instead: an in-memory slice of `(relativeOffset, filePosition)` entries, one recorded for the segment's first message and then one every `indexInterval` messages. The relative offset is the message's position within the segment, so the slice is sorted by construction and a binary search applies.

To read absolute offset O, the segment computes `rel = O - baseOffset`, then uses `sort.Search` to find the first entry whose relative offset is strictly greater than `rel`. The entry it wants is the one before that — the largest entry at or below the target — so it takes index `i-1`. Because the segment always records an entry for relative offset 0, `i` is never zero for an in-range offset, so `i-1` is always valid. It then opens a fresh read handle, seeks to that entry's file position, and scans forward decoding records until `msg.Offset == O`. The binary search is O(log n) over the sparse entries; the forward scan reads at most `indexInterval` records. A smaller interval shrinks the scan and grows the index; a larger one does the reverse — the classic space-for-time knob.

The index lives only in memory and is rebuilt by scanning the log once when the segment is opened, in `recover`. Rebuilding on open keeps the index and the log from ever disagreeing after a crash, at the cost of one sequential pass over the segment; `recover` reuses that same pass to set `size` and `nextOffset` and to truncate any torn record left at the tail.

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
	"time"
)

// Sentinel errors that callers can match with errors.Is.
var (
	ErrChecksumMismatch = errors.New("msgstore: crc32 checksum mismatch")
	ErrShortRecord      = errors.New("msgstore: record is too short to decode")
	ErrOffsetNotFound   = errors.New("msgstore: offset not found")
	ErrInvalidOffset    = errors.New("msgstore: offset is negative")
)

// Header is a single key/value pair attached to a Message.
type Header struct {
	Key   []byte
	Value []byte
}

// Message is the unit of storage. Offset and Timestamp are assigned by the Log
// on Append; callers populate Key, Value, and Headers.
type Message struct {
	Offset    int64
	Timestamp int64 // Unix nanoseconds
	Key       []byte
	Value     []byte
	Headers   []Header
}

// minRecord is the encoded size of a message with empty key, value, and headers.
const minRecord = 8 + 8 + 4 + 4 + 2 + 4

// nowNano returns the current Unix nanosecond timestamp, split out so callers
// can reason about it; Append sets Timestamp from it when the field is zero.
var nowNano = func() int64 { return time.Now().UnixNano() }

// Encode serializes msg into a self-describing record with a trailing CRC32.
func Encode(msg *Message) ([]byte, error) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, msg.Offset)
	_ = binary.Write(&buf, binary.BigEndian, msg.Timestamp)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(msg.Key)))
	buf.Write(msg.Key)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(msg.Value)))
	buf.Write(msg.Value)
	_ = binary.Write(&buf, binary.BigEndian, uint16(len(msg.Headers)))
	for _, h := range msg.Headers {
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(h.Key)))
		buf.Write(h.Key)
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(h.Value)))
		buf.Write(h.Value)
	}
	crc := crc32.ChecksumIEEE(buf.Bytes())
	_ = binary.Write(&buf, binary.BigEndian, crc)
	return buf.Bytes(), nil
}

// Decode validates the CRC32 then deserializes a Message.
func Decode(data []byte) (*Message, error) {
	if len(data) < minRecord {
		return nil, ErrShortRecord
	}
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
```

Create `segment.go`:

```go
package msgstore

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// SyncPolicy controls when data is flushed to durable storage.
type SyncPolicy int

const (
	// SyncEveryMessage calls fsync after every append: safest, slowest.
	SyncEveryMessage SyncPolicy = iota
	// SyncEveryN calls fsync once every N appends.
	SyncEveryN
	// SyncOSDefault never calls fsync; the OS flushes at its own pace.
	SyncOSDefault
)

// indexEntry maps a relative offset to a byte position in the .log file.
type indexEntry struct {
	relativeOffset uint64
	filePosition   uint64
}

// segment is a single append-only .log file plus an in-memory sparse index.
type segment struct {
	mu         sync.RWMutex
	baseOffset int64
	nextOffset int64
	size       int64 // current .log size in bytes; also the next record's position

	logFile *os.File
	path    string

	entries       []indexEntry // sparse index, sorted by relativeOffset
	indexInterval int64        // one index entry per this many messages

	policy    SyncPolicy
	syncEvery int
	sinceSync int
}

func openSegment(dir string, baseOffset int64, interval int64, policy SyncPolicy, syncEvery int) (*segment, error) {
	if interval < 1 {
		interval = 1
	}
	path := filepath.Join(dir, fmt.Sprintf("%020d.log", baseOffset))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("msgstore: open segment: %w", err)
	}
	s := &segment{
		baseOffset:    baseOffset,
		nextOffset:    baseOffset,
		logFile:       f,
		path:          path,
		indexInterval: interval,
		policy:        policy,
		syncEvery:     syncEvery,
	}
	if err := s.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// recover scans the log from byte zero, rebuilds the sparse index, sets size
// and nextOffset, and truncates any torn record left at the tail.
func (s *segment) recover() error {
	info, err := s.logFile.Stat()
	if err != nil {
		return err
	}
	fileSize := info.Size()
	if _, err := s.logFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(s.logFile)

	var pos int64
	offset := s.baseOffset
	lenBuf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			break // clean EOF or a torn length prefix
		}
		recLen := int64(binary.BigEndian.Uint32(lenBuf))
		if recLen < minRecord || pos+4+recLen > fileSize {
			break // garbage or truncated length at the tail
		}
		recBuf := make([]byte, recLen)
		if _, err := io.ReadFull(r, recBuf); err != nil {
			break // truncated body
		}
		if _, err := Decode(recBuf); err != nil {
			break // corrupt record at the tail
		}
		rel := offset - s.baseOffset
		if rel%s.indexInterval == 0 {
			s.entries = append(s.entries, indexEntry{relativeOffset: uint64(rel), filePosition: uint64(pos)})
		}
		pos += 4 + recLen
		offset++
	}

	if fileSize > pos {
		if err := s.logFile.Truncate(pos); err != nil {
			return err
		}
	}
	s.size = pos
	s.nextOffset = offset
	return nil
}

// append writes msg to the segment and returns its absolute offset.
func (s *segment) append(msg *Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	offset := s.nextOffset
	msg.Offset = offset
	if msg.Timestamp == 0 {
		msg.Timestamp = nowNano()
	}

	data, err := Encode(msg)
	if err != nil {
		return 0, err
	}

	pos := s.size
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := s.logFile.Write(lenBuf); err != nil {
		return 0, fmt.Errorf("msgstore: write length: %w", err)
	}
	if _, err := s.logFile.Write(data); err != nil {
		return 0, fmt.Errorf("msgstore: write record: %w", err)
	}

	rel := offset - s.baseOffset
	if rel%s.indexInterval == 0 {
		s.entries = append(s.entries, indexEntry{relativeOffset: uint64(rel), filePosition: uint64(pos)})
	}

	s.size += int64(4 + len(data))
	s.nextOffset++

	switch s.policy {
	case SyncEveryMessage:
		if err := s.logFile.Sync(); err != nil {
			return 0, err
		}
	case SyncEveryN:
		s.sinceSync++
		if s.sinceSync >= s.syncEvery {
			s.sinceSync = 0
			if err := s.logFile.Sync(); err != nil {
				return 0, err
			}
		}
	}
	return offset, nil
}

// read returns the message at the given absolute offset.
func (s *segment) read(offset int64) (*Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if offset < s.baseOffset || offset >= s.nextOffset {
		return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, offset)
	}
	rel := uint64(offset - s.baseOffset)

	// Largest index entry with relativeOffset <= rel.
	i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].relativeOffset > rel })
	if i == 0 {
		return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, offset)
	}
	return s.scanFrom(offset, int64(s.entries[i-1].filePosition))
}

// scanFrom opens an independent read handle, seeks to startPos, and scans
// forward decoding records until the target offset is found.
func (s *segment) scanFrom(target, startPos int64) (*Message, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(startPos, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	lenBuf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, target)
		}
		recLen := int(binary.BigEndian.Uint32(lenBuf))
		recBuf := make([]byte, recLen)
		if _, err := io.ReadFull(r, recBuf); err != nil {
			return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, target)
		}
		msg, err := Decode(recBuf)
		if err != nil {
			return nil, err
		}
		if msg.Offset == target {
			return msg, nil
		}
		if msg.Offset > target {
			return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, target)
		}
	}
}

// endOffset returns the offset the next append will receive.
func (s *segment) endOffset() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextOffset
}

// sizeBytes returns the current .log size.
func (s *segment) sizeBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.size
}

// sync flushes the segment to durable storage.
func (s *segment) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logFile.Sync()
}

// close flushes and closes the segment file.
func (s *segment) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.logFile.Sync(); err != nil {
		s.logFile.Close()
		return err
	}
	return s.logFile.Close()
}

// SegmentInfo is the metadata returned by Log.ListSegments.
type SegmentInfo struct {
	BaseOffset   int64
	NextOffset   int64
	SizeBytes    int64
	MessageCount int64
}
```

Create `log.go`:

```go
package msgstore

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
	defaultMaxSegmentSize = 1 << 30 // 1 GiB
	defaultIndexInterval  = 4096
	defaultSyncEveryN     = 1000
)

// Config controls Log behaviour. Zero fields take documented defaults.
type Config struct {
	MaxSegmentSize int64      // rotate when the active segment exceeds this many bytes
	IndexInterval  int64      // sparse index: one entry per N messages
	Policy         SyncPolicy // fsync policy
	SyncEveryN     int        // used when Policy == SyncEveryN
}

// Log is a durable, segmented, append-only log. All methods are safe for
// concurrent use.
type Log struct {
	mu       sync.RWMutex
	dir      string
	cfg      Config
	segments []*segment // sorted by base offset; the last element is active
}

// Open opens or creates a Log rooted at dir.
func Open(dir string, cfg Config) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("msgstore: mkdir: %w", err)
	}
	if cfg.MaxSegmentSize == 0 {
		cfg.MaxSegmentSize = defaultMaxSegmentSize
	}
	if cfg.IndexInterval == 0 {
		cfg.IndexInterval = defaultIndexInterval
	}
	if cfg.SyncEveryN == 0 {
		cfg.SyncEveryN = defaultSyncEveryN
	}

	l := &Log{dir: dir, cfg: cfg}
	if err := l.loadSegments(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) loadSegments() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	var bases []int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".log"), 10, 64)
		if err != nil {
			continue
		}
		bases = append(bases, n)
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })

	if len(bases) == 0 {
		seg, err := openSegment(l.dir, 0, l.cfg.IndexInterval, l.cfg.Policy, l.cfg.SyncEveryN)
		if err != nil {
			return err
		}
		l.segments = append(l.segments, seg)
		return nil
	}
	for _, base := range bases {
		seg, err := openSegment(l.dir, base, l.cfg.IndexInterval, l.cfg.Policy, l.cfg.SyncEveryN)
		if err != nil {
			return err
		}
		l.segments = append(l.segments, seg)
	}
	return nil
}

// Append writes msg to the active segment and returns its assigned offset.
func (l *Log) Append(msg *Message) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	active := l.segments[len(l.segments)-1]
	offset, err := active.append(msg)
	if err != nil {
		return 0, err
	}
	if active.sizeBytes() >= l.cfg.MaxSegmentSize {
		if err := l.rollLocked(); err != nil {
			return offset, err
		}
	}
	return offset, nil
}

// Read returns the message at the given offset.
func (l *Log) Read(offset int64) (*Message, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidOffset, offset)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	seg := l.findSegment(offset)
	if seg == nil {
		return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, offset)
	}
	return seg.read(offset)
}

// findSegment returns the segment whose range contains offset, or nil.
// The caller must hold at least l.mu.RLock.
func (l *Log) findSegment(offset int64) *segment {
	i := sort.Search(len(l.segments), func(i int) bool { return l.segments[i].baseOffset > offset })
	if i == 0 {
		return nil
	}
	return l.segments[i-1]
}

// RollSegment seals the active segment and opens a new one.
func (l *Log) RollSegment() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rollLocked()
}

func (l *Log) rollLocked() error {
	active := l.segments[len(l.segments)-1]
	if err := active.sync(); err != nil {
		return err
	}
	seg, err := openSegment(l.dir, active.endOffset(), l.cfg.IndexInterval, l.cfg.Policy, l.cfg.SyncEveryN)
	if err != nil {
		return err
	}
	l.segments = append(l.segments, seg)
	return nil
}

// TruncateBefore deletes every sealed segment whose messages all lie before
// offset. The active segment is never deleted.
func (l *Log) TruncateBefore(offset int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	keep := l.segments[:0:0]
	for i, seg := range l.segments {
		active := i == len(l.segments)-1
		if !active && seg.endOffset() <= offset {
			seg.close()
			os.Remove(filepath.Join(l.dir, fmt.Sprintf("%020d.log", seg.baseOffset)))
			continue
		}
		keep = append(keep, seg)
	}
	l.segments = keep
	return nil
}

// ListSegments returns metadata for all segments, oldest first.
func (l *Log) ListSegments() []SegmentInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()

	infos := make([]SegmentInfo, len(l.segments))
	for i, seg := range l.segments {
		seg.mu.RLock()
		infos[i] = SegmentInfo{
			BaseOffset:   seg.baseOffset,
			NextOffset:   seg.nextOffset,
			SizeBytes:    seg.size,
			MessageCount: seg.nextOffset - seg.baseOffset,
		}
		seg.mu.RUnlock()
	}
	return infos
}

// LowestOffset returns the first available offset in the log.
func (l *Log) LowestOffset() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments[0].baseOffset
}

// HighestOffset returns the last written offset, or -1 if the log is empty.
func (l *Log) HighestOffset() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments[len(l.segments)-1].endOffset() - 1
}

// Sync flushes the active segment to durable storage.
func (l *Log) Sync() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments[len(l.segments)-1].sync()
}

// Close flushes and closes all segments.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for _, seg := range l.segments {
		if err := seg.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
```

### The runnable demo

The demo opens a log with a deliberately tiny `MaxSegmentSize` so a handful of messages force a rotation, appends four events, reports how many segments the rotation produced, and reads every offset back to prove the sparse-index seek lands on the right record across segment boundaries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/segmented-log"
)

func main() {
	dir := filepath.Join(os.TempDir(), "segmented-log-demo")
	_ = os.RemoveAll(dir)
	defer os.RemoveAll(dir)

	l, err := msgstore.Open(dir, msgstore.Config{
		MaxSegmentSize: 80, // tiny: rotate after roughly two messages
		IndexInterval:  2,
		Policy:         msgstore.SyncOSDefault,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()

	keys := []string{"order.created", "order.paid", "order.shipped", "order.delivered"}
	for _, k := range keys {
		off, err := l.Append(&msgstore.Message{Key: []byte(k), Value: []byte("event")})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("appended offset=%d key=%s\n", off, k)
	}

	fmt.Printf("segments=%d lowest=%d highest=%d\n", len(l.ListSegments()), l.LowestOffset(), l.HighestOffset())

	for off := l.LowestOffset(); off <= l.HighestOffset(); off++ {
		msg, err := l.Read(off)
		if err != nil {
			log.Fatalf("read offset %d: %v", off, err)
		}
		fmt.Printf("read offset=%d key=%s\n", msg.Offset, msg.Key)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
appended offset=0 key=order.created
appended offset=1 key=order.paid
appended offset=2 key=order.shipped
appended offset=3 key=order.delivered
segments=3 lowest=0 highest=3
read offset=0 key=order.created
read offset=1 key=order.paid
read offset=2 key=order.shipped
read offset=3 key=order.delivered
```

### Tests

`TestAppendAndRead` writes 200 messages and reads each back, checking the offset is the loop index and the value round-trips. `TestSegmentRotation` uses a small `MaxSegmentSize` to force several segments and asserts more than one appears. `TestSparseIndexSeek` sets `IndexInterval` to a small value so most reads must scan forward from a sparse entry, exercising the binary-search-then-scan path on every offset. `TestTruncateBefore` rotates several segments, truncates before the highest offset, and asserts the early sealed segments are gone while the message at the highest offset remains readable. `TestReadInvalidOffset` checks the negative-offset guard.

Create `log_test.go`:

```go
package msgstore

import (
	"errors"
	"fmt"
	"testing"
)

func TestAppendAndRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l, err := Open(dir, Config{Policy: SyncOSDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const n = 200
	for i := 0; i < n; i++ {
		off, err := l.Append(&Message{Key: []byte(fmt.Sprintf("key-%d", i)), Value: []byte(fmt.Sprintf("value-%d", i))})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if off != int64(i) {
			t.Fatalf("offset %d: got %d", i, off)
		}
	}
	for i := 0; i < n; i++ {
		got, err := l.Read(int64(i))
		if err != nil {
			t.Fatalf("Read(%d): %v", i, err)
		}
		if want := fmt.Sprintf("value-%d", i); string(got.Value) != want {
			t.Errorf("offset %d: got %q, want %q", i, got.Value, want)
		}
	}
}

func TestSegmentRotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l, err := Open(dir, Config{MaxSegmentSize: 256, Policy: SyncOSDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 0; i < 40; i++ {
		if _, err := l.Append(&Message{Value: make([]byte, 64)}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if got := len(l.ListSegments()); got < 2 {
		t.Fatalf("expected multiple segments, got %d", got)
	}
	// Reads must still find every offset across the rotated segments.
	for i := int64(0); i <= l.HighestOffset(); i++ {
		if _, err := l.Read(i); err != nil {
			t.Fatalf("Read(%d) after rotation: %v", i, err)
		}
	}
}

func TestSparseIndexSeek(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// IndexInterval 8 means most offsets are not at an index entry and must be
	// reached by scanning forward from the nearest one.
	l, err := Open(dir, Config{IndexInterval: 8, Policy: SyncOSDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const n = 100
	for i := 0; i < n; i++ {
		if _, err := l.Append(&Message{Value: []byte(fmt.Sprintf("v%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		got, err := l.Read(int64(i))
		if err != nil {
			t.Fatalf("Read(%d): %v", i, err)
		}
		if want := fmt.Sprintf("v%d", i); string(got.Value) != want {
			t.Errorf("offset %d: got %q, want %q", i, got.Value, want)
		}
	}
	if _, err := l.Read(int64(n)); !errors.Is(err, ErrOffsetNotFound) {
		t.Errorf("Read past end: expected ErrOffsetNotFound, got %v", err)
	}
}

func TestTruncateBefore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l, err := Open(dir, Config{MaxSegmentSize: 96, Policy: SyncOSDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 0; i < 30; i++ {
		if _, err := l.Append(&Message{Value: make([]byte, 32)}); err != nil {
			t.Fatal(err)
		}
	}
	highest := l.HighestOffset()
	if err := l.TruncateBefore(highest); err != nil {
		t.Fatal(err)
	}
	if got := l.LowestOffset(); got > highest {
		t.Fatalf("lowest offset %d exceeds highest %d", got, highest)
	}
	for _, info := range l.ListSegments() {
		if info.NextOffset <= highest {
			t.Errorf("segment base=%d next=%d should have been truncated", info.BaseOffset, info.NextOffset)
		}
	}
	if _, err := l.Read(highest); err != nil {
		t.Fatalf("Read(highest=%d) after truncation: %v", highest, err)
	}
}

func TestReadInvalidOffset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l, err := Open(dir, Config{Policy: SyncOSDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if _, err := l.Read(-1); !errors.Is(err, ErrInvalidOffset) {
		t.Fatalf("expected ErrInvalidOffset, got %v", err)
	}
}
```

## Review

The engine is correct when three things hold. First, offsets are dense and monotonic: each append returns the previous `nextOffset`, and `nextOffset` advances by one, across rotations, so a consumer can store a single integer and resume. Second, the sparse-index seek lands on the right record: `sort.Search` returns the first entry above the target, so the read must use `i-1`, and because every segment records an entry at relative offset 0 that index is always valid. Third, `TruncateBefore` never deletes the active segment — the guard on the last element is what keeps `Append` from writing into a freed segment.

The mistakes that break each of these are worth naming. Using the `sort.Search` result directly instead of `i-1` starts the forward scan past the target and reports a spurious not-found. Reading offset `O` from a second `os.Open` handle relies on the appended bytes being visible through the OS page cache, which they are even without an fsync — fsync governs power-loss durability, not intra-process visibility — so `SyncOSDefault` reads still succeed. And rotating without first flushing the sealed segment, or deleting the active segment in `TruncateBefore`, both surface as lost records on the next open.

## Resources

- [`sort.Search`](https://pkg.go.dev/sort#Search) — the binary-search primitive behind the sparse-index lookup, and the source of the off-by-one that `i-1` corrects.
- [`os.OpenFile`](https://pkg.go.dev/os#OpenFile) — the `O_APPEND|O_CREATE|O_RDWR` flags that make every write land at the end, even after reopening.
- [Apache Kafka: Log Internals](https://kafka.apache.org/documentation/#log) — the canonical reference for segmented logs, base-offset file naming, and sparse `.index` files.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-message-encoding.md](01-message-encoding.md) | Next: [03-crash-recovery.md](03-crash-recovery.md)
