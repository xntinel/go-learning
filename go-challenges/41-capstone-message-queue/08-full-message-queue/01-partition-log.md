# Exercise 1: Partition Log

The partition log is the unit of durability in the whole broker: an append-only binary file plus an in-memory offset index. Every produce lands here, every fetch reads from here, and crash recovery is just replaying it. This exercise builds it as a standalone module — binary framing, concurrent reads via `ReadAt`, a `sync.Cond` long-poll, and recovery with tail truncation.

This module is fully self-contained: its own `go mod init`, every type it needs defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
partition.go         PartitionLog, Message, Append, Fetch (long-poll), recovery
cmd/
  demo/
    main.go          append a few records, fetch them back, reopen to recover
partition_test.go    round-trip with keys, recovery, tail truncation, long-poll
```

- Files: `partition.go`, `cmd/demo/main.go`, `partition_test.go`.
- Implement: `PartitionLog` with `Append(key, value []byte) (int64, error)`, `Fetch(ctx, startOffset, maxMsgs) ([]*Message, error)`, `NextOffset()`, and `Close()`; opening a log recovers any existing segment.
- Test: keyed round-trip, recovery after reopen, tail truncation of a partial record, long-poll unblock on append, and context-deadline timeout.
- Verify: `go test -race ./...`

### Why a fixed header, ReadAt, and a condition variable

A partition is a flat append-only file. Three design choices make it both correct and concurrent.

First, the record frame keeps *both* length fields in a fixed 24-byte header — offset, timestamp, key length, value length — before either variable-length field. The interleaved alternative (key length, key, value length, value) puts a length after a variable field, so a decoder that reads a fixed header disagrees with the encoder the instant a key is non-empty. With both lengths up front, the decoder reads exactly 24 bytes, learns both sizes, and reads two sized slices with no further parsing. The layout:

```text
Offset  Size  Field
     0     8  offset    (int64, big-endian)
     8     8  timestamp (int64 nanoseconds, big-endian)
    16     4  keyLen    (uint32, big-endian)
    20     4  valueLen  (uint32, big-endian)
    24  keyLen   key bytes
     ?  valueLen value bytes
```

Second, reads use `os.File.ReadAt`, never `Seek`+`Read`. `ReadAt` takes an absolute byte position and does not move the file's shared cursor, so a fetch reading old records cannot disturb an append writing at the end. The only shared mutable state a reader must coordinate on is the in-memory index, which it consults under the lock; the byte reads themselves need no further coordination.

Third, an empty fetch does not return — it long-polls on a `sync.Cond` tied to the log's mutex. A fetcher checks for messages, and if there are none, calls `cond.Wait()`, which atomically releases the lock and parks the goroutine. An append calls `cond.Broadcast()` after writing, waking every parked fetcher to re-check. Context cancellation is wired in by a watcher goroutine that locks the mutex and broadcasts when `ctx.Done()` fires; locking before the broadcast is what prevents a missed wakeup in the gap between a fetcher's `ctx.Err()` check and its `Wait()`.

Recovery is the inverse of append: on open, scan from byte zero, decode record by record, rebuild the index, and set `nextOff` to one past the highest offset. A partial trailing record — a write the kernel never finished before a crash — surfaces as a short `ReadAt` returning `io.EOF` partway through, at which point the file is truncated back to the last complete record.

Create `partition.go`:

```go
package partition

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// headerSize is the fixed per-record header:
//
//	[8] offset [8] timestamp-nanos [4] keyLen [4] valueLen
const headerSize = 24

// Message is one record in a partition log.
type Message struct {
	Offset    int64
	Timestamp time.Time
	Key       []byte
	Value     []byte
}

// indexEntry maps a logical offset to a byte position in the segment file.
type indexEntry struct {
	offset  int64
	filePos int64
}

// PartitionLog is an append-only binary log for a single partition. Writes are
// serialized under mu; reads use ReadAt so they never move a shared cursor.
type PartitionLog struct {
	mu      sync.Mutex
	cond    *sync.Cond
	file    *os.File
	index   []indexEntry
	nextOff int64
	size    int64
}

// NewPartitionLog opens or creates dir/segment.log and recovers its contents.
func NewPartitionLog(dir string) (*PartitionLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("partition: mkdir %s: %w", dir, err)
	}
	path := dir + "/segment.log"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("partition: open %s: %w", path, err)
	}
	pl := &PartitionLog{file: f}
	pl.cond = sync.NewCond(&pl.mu)
	if err := pl.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return pl, nil
}

// Append encodes (key, value) and appends it. Returns the assigned offset.
func (pl *PartitionLog) Append(key, value []byte) (int64, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	off := pl.nextOff
	kl := len(key)
	vl := len(value)

	buf := make([]byte, headerSize+kl+vl)
	binary.BigEndian.PutUint64(buf[0:8], uint64(off))
	binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(buf[16:20], uint32(kl))
	binary.BigEndian.PutUint32(buf[20:24], uint32(vl))
	copy(buf[24:24+kl], key)
	copy(buf[24+kl:], value)

	filePos := pl.size
	n, err := pl.file.Write(buf)
	if err != nil {
		return 0, fmt.Errorf("partition: write offset %d: %w", off, err)
	}
	pl.index = append(pl.index, indexEntry{offset: off, filePos: filePos})
	pl.nextOff++
	pl.size += int64(n)
	pl.cond.Broadcast()
	return off, nil
}

// Fetch returns up to maxMsgs messages starting at startOffset. If none are
// available it long-polls until a message arrives or ctx is cancelled.
func (pl *PartitionLog) Fetch(ctx context.Context, startOffset int64, maxMsgs int) ([]*Message, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		pl.mu.Lock()
		pl.cond.Broadcast()
		pl.mu.Unlock()
	}()

	pl.mu.Lock()
	defer pl.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msgs, err := pl.readFromLocked(startOffset, maxMsgs)
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 {
			return msgs, nil
		}
		pl.cond.Wait()
	}
}

// readFromLocked returns up to maxMsgs messages at startOffset. Caller holds mu.
func (pl *PartitionLog) readFromLocked(startOffset int64, maxMsgs int) ([]*Message, error) {
	if len(pl.index) == 0 || startOffset >= pl.nextOff {
		return nil, nil
	}
	i := sort.Search(len(pl.index), func(k int) bool {
		return pl.index[k].offset >= startOffset
	})
	if i >= len(pl.index) {
		return nil, nil
	}
	pos := pl.index[i].filePos
	var msgs []*Message
	for len(msgs) < maxMsgs {
		msg, next, err := pl.decodeAt(pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("partition: decode at %d: %w", pos, err)
		}
		msgs = append(msgs, msg)
		pos = next
	}
	return msgs, nil
}

// decodeAt decodes one record at byte position pos, returning the next position.
func (pl *PartitionLog) decodeAt(pos int64) (*Message, int64, error) {
	var hdr [headerSize]byte
	if _, err := pl.file.ReadAt(hdr[:], pos); err != nil {
		return nil, 0, err
	}
	off := int64(binary.BigEndian.Uint64(hdr[0:8]))
	tsNano := int64(binary.BigEndian.Uint64(hdr[8:16]))
	keyLen := int(binary.BigEndian.Uint32(hdr[16:20]))
	valLen := int(binary.BigEndian.Uint32(hdr[20:24]))
	pos += headerSize

	var key []byte
	if keyLen > 0 {
		key = make([]byte, keyLen)
		if _, err := pl.file.ReadAt(key, pos); err != nil {
			return nil, 0, err
		}
		pos += int64(keyLen)
	}
	var val []byte
	if valLen > 0 {
		val = make([]byte, valLen)
		if _, err := pl.file.ReadAt(val, pos); err != nil {
			return nil, 0, err
		}
		pos += int64(valLen)
	}
	return &Message{Offset: off, Timestamp: time.Unix(0, tsNano).UTC(), Key: key, Value: val}, pos, nil
}

// recover rebuilds the index by scanning from the start, truncating a partial
// trailing record. Called at construction, before any other goroutine exists.
func (pl *PartitionLog) recover() error {
	info, err := pl.file.Stat()
	if err != nil {
		return fmt.Errorf("partition: stat: %w", err)
	}
	pl.size = info.Size()
	pl.index = pl.index[:0]
	pl.nextOff = 0

	pos := int64(0)
	for pos < pl.size {
		msg, next, err := pl.decodeAt(pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if terr := pl.file.Truncate(pos); terr != nil {
					return fmt.Errorf("partition: truncate partial tail: %w", terr)
				}
				pl.size = pos
				break
			}
			return fmt.Errorf("partition: recover at %d: %w", pos, err)
		}
		pl.index = append(pl.index, indexEntry{offset: msg.Offset, filePos: pos})
		if msg.Offset >= pl.nextOff {
			pl.nextOff = msg.Offset + 1
		}
		pos = next
	}
	return nil
}

// NextOffset returns the offset that the next Append will assign.
func (pl *PartitionLog) NextOffset() int64 {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.nextOff
}

// Close wakes any parked fetchers and closes the segment file.
func (pl *PartitionLog) Close() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.cond.Broadcast()
	return pl.file.Close()
}
```

Read `Append` and `decodeAt` as mirror images. `Append` writes the 24-byte header — offset, timestamp, key length, value length — then the key and value, and records the pre-write file size as this record's byte position in the index. `decodeAt` reads the 24-byte header, learns both lengths, then reads exactly that many key and value bytes with `ReadAt`. Because both lengths sit in the header, a record with a real key decodes the same way as one without. `recover` walks the file with `decodeAt`, and the moment a read runs short — a partial tail — it truncates and stops, so reopening a crashed log yields only its complete records.

### The runnable demo

The demo appends three keyed records, fetches them back, prints offsets and decoded fields, then reopens the same directory to show recovery rebuilds the index. Output is deterministic — no timestamps or timings are printed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"example.com/partition-log"
)

func main() {
	dir, err := os.MkdirTemp("", "partition-demo-*")
	if err != nil {
		fmt.Println("mkdirtemp:", err)
		return
	}
	defer os.RemoveAll(dir)

	pl, err := partition.NewPartitionLog(dir)
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	for i := 0; i < 3; i++ {
		key := []byte(fmt.Sprintf("k%d", i))
		val := []byte(fmt.Sprintf("event-%d", i))
		off, err := pl.Append(key, val)
		if err != nil {
			fmt.Println("append:", err)
			return
		}
		fmt.Printf("appended offset=%d key=%s value=%s\n", off, key, val)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msgs, err := pl.Fetch(ctx, 0, 10)
	if err != nil {
		fmt.Println("fetch:", err)
		return
	}
	fmt.Printf("fetched %d messages\n", len(msgs))
	for _, m := range msgs {
		fmt.Printf("  offset=%d key=%s value=%s\n", m.Offset, m.Key, m.Value)
	}
	pl.Close()

	// Reopen the same directory: recovery rebuilds the index from disk.
	pl2, err := partition.NewPartitionLog(dir)
	if err != nil {
		fmt.Println("reopen:", err)
		return
	}
	defer pl2.Close()
	fmt.Printf("after recovery: nextOffset=%d\n", pl2.NextOffset())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
appended offset=0 key=k0 value=event-0
appended offset=1 key=k1 value=event-1
appended offset=2 key=k2 value=event-2
fetched 3 messages
  offset=0 key=k0 value=event-0
  offset=1 key=k1 value=event-1
  offset=2 key=k2 value=event-2
after recovery: nextOffset=3
```

### Tests

The tests pin the properties that matter: keyed records round-trip (the regression guard against an interleaved length field), recovery rebuilds the index across a reopen, a partial tail is truncated rather than treated as data, a blocked fetch unblocks on a later append, and an empty fetch honors a context deadline.

Create `partition_test.go`:

```go
package partition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestAppendFetchWithKeys(t *testing.T) {
	t.Parallel()
	pl, err := NewPartitionLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()

	for i := 0; i < 4; i++ {
		key := []byte(fmt.Sprintf("key-%d", i))
		val := []byte(fmt.Sprintf("val-%d", i))
		off, err := pl.Append(key, val)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if off != int64(i) {
			t.Fatalf("offset %d = %d, want %d", i, off, i)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, err := pl.Fetch(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("fetched %d, want 4", len(msgs))
	}
	for i, m := range msgs {
		wantKey := fmt.Sprintf("key-%d", i)
		wantVal := fmt.Sprintf("val-%d", i)
		if string(m.Key) != wantKey || string(m.Value) != wantVal {
			t.Fatalf("msg %d = (%q,%q), want (%q,%q)", i, m.Key, m.Value, wantKey, wantVal)
		}
	}
}

func TestRecoverAfterReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	pl, err := NewPartitionLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := pl.Append([]byte("k"), []byte(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	pl.Close()

	pl2, err := NewPartitionLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer pl2.Close()
	if got := pl2.NextOffset(); got != 5 {
		t.Fatalf("nextOffset after recovery = %d, want 5", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, err := pl2.Fetch(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("recovered %d messages, want 5", len(msgs))
	}
}

func TestRecoverTruncatesPartialTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	pl, err := NewPartitionLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pl.Append([]byte("k"), []byte("good")); err != nil {
		t.Fatal(err)
	}
	if _, err := pl.Append([]byte("k"), []byte("alsogood")); err != nil {
		t.Fatal(err)
	}
	pl.Close()

	// Simulate a crash mid-write by appending a partial header to the segment.
	f, err := os.OpenFile(dir+"/segment.log", os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0, 0, 0, 0, 9, 9}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	pl2, err := NewPartitionLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer pl2.Close()
	if got := pl2.NextOffset(); got != 2 {
		t.Fatalf("nextOffset after truncation = %d, want 2", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, err := pl2.Fetch(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || string(msgs[1].Value) != "alsogood" {
		t.Fatalf("recovered %v, want 2 complete records", msgs)
	}
}

func TestFetchUnblocksOnAppend(t *testing.T) {
	t.Parallel()
	pl, err := NewPartitionLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()

	type result struct {
		msgs []*Message
		err  error
	}
	ch := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		msgs, err := pl.Fetch(ctx, 0, 1)
		ch <- result{msgs, err}
	}()

	time.Sleep(20 * time.Millisecond) // let the fetcher park
	if _, err := pl.Append(nil, []byte("late")); err != nil {
		t.Fatal(err)
	}

	res := <-ch
	if res.err != nil {
		t.Fatalf("fetch error: %v", res.err)
	}
	if len(res.msgs) != 1 || string(res.msgs[0].Value) != "late" {
		t.Fatalf("got %v, want one 'late' message", res.msgs)
	}
}

func TestFetchTimesOut(t *testing.T) {
	t.Parallel()
	pl, err := NewPartitionLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := pl.Fetch(ctx, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}
```

## Review

The frame is sound when append and decode are exact mirrors and both length fields live in the header. The regression that this design exists to prevent is the interleaved layout — length, variable field, length — which passes every `nil`-key test and then mis-decodes the first record with a real key; `TestAppendFetchWithKeys` is the guard. Recovery is sound when it sets `nextOff` to one past the highest recovered offset and truncates a short tail rather than erroring: `TestRecoverTruncatesPartialTail` writes a deliberate partial header and asserts the two complete records survive and the garbage is gone. The long-poll is sound when a parked fetcher wakes on a later append and when an empty fetch returns `context.DeadlineExceeded` instead of blocking forever; the context watcher must lock the mutex before it broadcasts, or a cancellation that races the fetcher's `Wait()` is lost. All of this holding under `go test -race` is the real bar: the race detector is what proves the `ReadAt` reads and the appending write genuinely interleave without a data race.

## Resources

- [`os.File.ReadAt`](https://pkg.go.dev/os#File.ReadAt) — positional reads that do not move the shared file cursor, the basis for concurrent read/append.
- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — the condition variable used for the long-poll wait/broadcast pattern.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — fixed-width big-endian encoding of the record header.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-consumer-group-coordinator.md](02-consumer-group-coordinator.md)
