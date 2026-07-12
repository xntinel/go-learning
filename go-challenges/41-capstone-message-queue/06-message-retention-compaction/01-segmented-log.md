# Exercise 1: The Segmented Log

Every retention and compaction policy in this lesson is a question about a *segment*, never about a single message. Before you can answer those questions you need the object they ask about: an immutable, sealed slice of messages that knows its own size, its time bounds, and its base offset, sitting inside a log that lets producers append while a background pass swaps the whole segment list out from under them atomically. This exercise builds that foundation.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
log.go                 Message, Segment, Log; Append, Segments, TotalSizeBytes, ReplaceSegments
cmd/
  demo/
    main.go            append three segments, snapshot, then atomically replace the list
log_test.go            sealing math, tombstone detection, concurrent Append/Segments under -race
```

- Files: `log.go`, `cmd/demo/main.go`, `log_test.go`.
- Implement: `Message` with `IsTombstone()`, `Segment` with its accessors and `NewSegment`, and `Log` with `Append`, `Segments`, `TotalSizeBytes`, and `ReplaceSegments`.
- Test: seal a segment and check the size/timestamp/offset math, detect a tombstone, and drive `Append` and `Segments` from many goroutines under the race detector.
- Verify: `go test -race ./...`

### Why a sealed segment is the unit of everything

A segment is sealed once and never touched again. That single property is what lets retention and compaction run concurrently with producers: a sealed segment is immutable, so a reader scanning it and a producer appending to a *different* (newer) segment cannot interfere. The cost of that immutability is that you can never edit a segment in place — you delete it whole or you rewrite it whole — which is exactly the segment-granularity constraint that shapes every policy that follows.

`NewSegment` does the sealing. It walks the messages once to sum `len(key) + len(value)` into `sizeBytes`, captures the first and last timestamps as the segment's time bounds, and records the first message's offset as the `baseOffset`. Because the input is required to be in ascending offset order, the first message is the oldest and the last is the youngest; `LastTimestamp()` is therefore the deciding value for time retention (if even the youngest message is expired, the whole segment is), and `BaseOffset()` orders segments against each other for size retention. An empty slice seals to a zero-value segment so the constructor never indexes out of bounds.

The accessors are deliberately read-only. `Messages()` hands back the underlying slice with a documented "do not mutate" contract rather than copying it, because the hot path (compaction, retention scans) reads it repeatedly and a defensive copy on every call would dominate the cost. The fields stay unexported so the only way to build a segment is through `NewSegment`, which guarantees the derived size and bounds always match the contents.

### Why the log swaps lists instead of editing one

`Log` is an ordered slice of sealed segments behind a `sync.RWMutex`. The lock choice matters. `Segments` and `TotalSizeBytes` take the read lock, so any number of readers proceed in parallel; `Append` and `ReplaceSegments` take the write lock, so mutations serialize. The reason this works without copying segments around is that segments are immutable: handing a reader a snapshot of the *slice of pointers* is safe because the segments those pointers reference never change.

`ReplaceSegments` is the handoff point that makes retention and compaction safe. A pass reads the current segments, spends however long it needs building a new list off to the side *without holding the lock*, then calls `ReplaceSegments` to install the result under the write lock in a single assignment. The Go memory model guarantees that the write performed under the lock happens-before any subsequent read under the lock, so a reader either sees the entire old list or the entire new one, never a splice of the two. `Segments` returns a copy of the slice header's contents so that a caller iterating its snapshot is unaffected by a concurrent `ReplaceSegments` — the caller's slice keeps pointing at the old (still-immutable) segments.

Create `log.go`:

```go
package retention

import (
	"sync"
	"time"
)

// Message is a single record in the log. A nil Value with a non-empty Key is a
// tombstone: it marks the key as deleted.
type Message struct {
	Key       []byte
	Value     []byte // nil means tombstone
	Offset    int64
	Timestamp time.Time
}

// IsTombstone reports whether the message marks a key as deleted.
func (m Message) IsTombstone() bool {
	return len(m.Key) > 0 && m.Value == nil
}

// Segment is an immutable slice of messages sealed at a point in time.
// Retention and compaction operate at segment granularity, not message
// granularity, because rewriting individual messages inside a segment is too
// expensive.
type Segment struct {
	messages       []Message
	sizeBytes      int64
	firstTimestamp time.Time
	lastTimestamp  time.Time
	baseOffset     int64
}

// NewSegment seals an ordered slice of messages into a Segment, computing the
// aggregate size and the timestamp bounds. The messages must be in ascending
// offset order. An empty slice seals to a zero-value Segment.
func NewSegment(msgs []Message) *Segment {
	if len(msgs) == 0 {
		return &Segment{}
	}
	var size int64
	for _, m := range msgs {
		size += int64(len(m.Key) + len(m.Value))
	}
	return &Segment{
		messages:       msgs,
		sizeBytes:      size,
		firstTimestamp: msgs[0].Timestamp,
		lastTimestamp:  msgs[len(msgs)-1].Timestamp,
		baseOffset:     msgs[0].Offset,
	}
}

// Messages returns the messages in the segment. Do not mutate the slice.
func (s *Segment) Messages() []Message { return s.messages }

// SizeBytes returns the sum of key and value byte lengths across all messages.
func (s *Segment) SizeBytes() int64 { return s.sizeBytes }

// FirstTimestamp is the timestamp of the first (oldest) message.
func (s *Segment) FirstTimestamp() time.Time { return s.firstTimestamp }

// LastTimestamp is the timestamp of the last (youngest) message.
func (s *Segment) LastTimestamp() time.Time { return s.lastTimestamp }

// BaseOffset is the log offset of the first message; it orders segments.
func (s *Segment) BaseOffset() int64 { return s.baseOffset }

// Log is a sequence of sealed segments. Append and ReplaceSegments are safe for
// concurrent use alongside the read-only accessors.
type Log struct {
	mu       sync.RWMutex
	segments []*Segment
	nextOff  int64
}

// NewLog creates an empty Log.
func NewLog() *Log { return &Log{} }

// Append seals a new segment from the given key/value pairs, assigning
// sequential offsets starting at the current head offset, and returns it.
func (l *Log) Append(keys, values [][]byte, now time.Time) *Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	msgs := make([]Message, len(keys))
	for i := range keys {
		msgs[i] = Message{
			Key:       keys[i],
			Value:     values[i],
			Offset:    l.nextOff,
			Timestamp: now,
		}
		l.nextOff++
	}
	seg := NewSegment(msgs)
	l.segments = append(l.segments, seg)
	return seg
}

// Segments returns a point-in-time snapshot of the segment list. The snapshot
// is a fresh slice, so a concurrent ReplaceSegments does not change it.
func (l *Log) Segments() []*Segment {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Segment, len(l.segments))
	copy(out, l.segments)
	return out
}

// TotalSizeBytes sums the sizes of all segments.
func (l *Log) TotalSizeBytes() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var total int64
	for _, s := range l.segments {
		total += s.sizeBytes
	}
	return total
}

// ReplaceSegments atomically installs a new segment list. Call it after a
// retention or compaction pass to commit the result. The write lock ensures no
// reader observes a partially replaced list.
func (l *Log) ReplaceSegments(segs []*Segment) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.segments = segs
}
```

### The runnable demo

The demo appends three segments, prints the snapshot, then replaces the list with just the newest segment — the exact shape every retention pass takes — and prints again to show the swap took effect.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/segmented-log"
)

func main() {
	log := retention.NewLog()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	log.Append([][]byte{[]byte("a")}, [][]byte{[]byte("v1")}, base)
	log.Append([][]byte{[]byte("b")}, [][]byte{[]byte("v2")}, base.Add(time.Hour))
	newest := log.Append([][]byte{[]byte("c")}, [][]byte{[]byte("v3")}, base.Add(2*time.Hour))

	segs := log.Segments()
	fmt.Printf("before: %d segments, %d bytes\n", len(segs), log.TotalSizeBytes())
	fmt.Printf("newest base offset: %d\n", newest.BaseOffset())

	// Keep only the newest segment, the way a retention pass commits its result.
	log.ReplaceSegments([]*retention.Segment{newest})
	fmt.Printf("after replace: %d segments, %d bytes\n", len(log.Segments()), log.TotalSizeBytes())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before: 3 segments, 9 bytes
newest base offset: 2
after replace: 1 segments, 3 bytes
```

Each segment holds one 1-byte key and one 2-byte value, so 3 bytes each, 9 total before the replace. After installing a list of just the newest segment, `TotalSizeBytes` sums only that one segment and reports 3 — exactly what a retention pass that trimmed the two older segments would leave.

### Tests

`TestNewSegmentSeals` pins the derived metadata: size is the byte sum, the bounds are the first and last timestamps, and the base offset is the first message's offset. `TestIsTombstone` covers the nil-value-with-key case and its neighbors. `TestLogConcurrentAppendAndRead` is the one that matters under `-race`: it runs many appenders against many readers and asserts the log never tears, which is the property the `RWMutex` exists to provide.

Create `log_test.go`:

```go
package retention

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestNewSegmentSeals(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Key: []byte("aa"), Value: []byte("xxxx"), Offset: 5, Timestamp: epoch},
		{Key: []byte("bb"), Value: []byte("yy"), Offset: 6, Timestamp: epoch.Add(time.Hour)},
	}
	seg := NewSegment(msgs)
	if got := seg.SizeBytes(); got != int64(2+4+2+2) {
		t.Errorf("SizeBytes = %d, want %d", got, 10)
	}
	if !seg.FirstTimestamp().Equal(epoch) {
		t.Errorf("FirstTimestamp = %v, want %v", seg.FirstTimestamp(), epoch)
	}
	if !seg.LastTimestamp().Equal(epoch.Add(time.Hour)) {
		t.Errorf("LastTimestamp = %v, want %v", seg.LastTimestamp(), epoch.Add(time.Hour))
	}
	if seg.BaseOffset() != 5 {
		t.Errorf("BaseOffset = %d, want 5", seg.BaseOffset())
	}
}

func TestNewSegmentEmpty(t *testing.T) {
	t.Parallel()
	seg := NewSegment(nil)
	if seg.SizeBytes() != 0 || len(seg.Messages()) != 0 {
		t.Errorf("empty segment should be zero-valued, got size=%d len=%d",
			seg.SizeBytes(), len(seg.Messages()))
	}
}

func TestIsTombstone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		m    Message
		want bool
	}{
		{"tombstone", Message{Key: []byte("k"), Value: nil}, true},
		{"live value", Message{Key: []byte("k"), Value: []byte("v")}, false},
		{"empty value not nil", Message{Key: []byte("k"), Value: []byte{}}, false},
		{"no key", Message{Key: nil, Value: nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.m.IsTombstone(); got != tc.want {
				t.Errorf("IsTombstone = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogConcurrentAppendAndRead(t *testing.T) {
	t.Parallel()
	log := NewLog()
	var wg sync.WaitGroup

	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			k := []byte(fmt.Sprintf("k%d", n))
			for range 50 {
				log.Append([][]byte{k}, [][]byte{[]byte("v")}, epoch)
			}
		}(i)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = log.Segments()
				_ = log.TotalSizeBytes()
			}
		}()
	}
	wg.Wait()

	if got := len(log.Segments()); got != 8*50 {
		t.Errorf("segment count = %d, want %d", got, 8*50)
	}
}
```

## Review

The segment is correct when its derived metadata can never disagree with its contents: the fields are unexported, the only constructor is `NewSegment`, and the constructor computes size and bounds from the very slice it stores. Confirm `NewSegment(nil)` returns a usable zero segment rather than panicking, and that `LastTimestamp` (not `FirstTimestamp`) is what callers reach for when asking "is the whole segment expired", since the last message is the youngest in an ascending-offset segment.

The log is correct when no reader ever sees a torn list. The two mutators take the write lock, the two accessors take the read lock, and `Segments` returns a copy so a caller's snapshot survives a concurrent `ReplaceSegments`. The common mistake is to have a retention pass sort or truncate the live slice in place; do the work on a copy and install it with one `ReplaceSegments` call. `TestLogConcurrentAppendAndRead` under `-race` is what proves the locking is right — a plain run would pass even with the locks removed, so the race detector is the real test here.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock semantics that let the accessors run in parallel while mutators serialize.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantee that makes `ReplaceSegments` under the write lock visible, whole, to every later reader.
- [Kafka: Log Design](https://kafka.apache.org/documentation/#design_log) — the segment-based, append-only structure and the offset model this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retention-policies.md](02-retention-policies.md)
