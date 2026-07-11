# Exercise 4: End-to-End Replay with Durable Offsets

Recovering the message log is only half of durability. If committed offsets live only in memory, a restart resets every consumer group to the beginning and the whole log is reprocessed. This exercise closes that gap: a broker that persists committed offsets to an append-only offset log, driven through its public API across a full produce → persist → consume → commit → restart → replay cycle.

This module is fully self-contained: its own `go mod init`, an inline partition log and durable offset store, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
log.go            PartitionLog: append-only segment, recovery, ReadFrom
offsets.go        offsetStore: append-only durable committed offsets
broker.go         Broker: Open, CreateTopic, Produce, Fetch, Commit/FetchOffset
cmd/
  demo/
    main.go       full cycle: produce, consume, commit, restart, resume
replay_test.go    committed offset survives restart; uncommitted tail is replayed
```

- Files: `log.go`, `offsets.go`, `broker.go`, `cmd/demo/main.go`, `replay_test.go`.
- Implement: `Broker` with `Open`, `CreateTopic`, `Produce`, `Fetch`, `CommitOffset`, `FetchOffset`, and `Close`; offsets are durable across a reopen.
- Test: a committed offset survives a broker restart and the consumer resumes past it; an uncommitted tail is replayed from the last committed offset.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p replay/cmd/demo && cd replay
go mod init example.com/replay
```

### Why durable offsets, and how the offset log mirrors the message log

A consumer's contract is the two-phase loop: fetch the committed offset, consume from there, commit the offset of the last record it processed. The contract only holds across a restart if the committed offset is itself durable. Kafka stores committed offsets in an internal `__consumer_offsets` topic for exactly this reason; here the same idea is an append-only `offsets.log` next to the message segments.

The offset store reuses the discipline of the message log. Each `CommitOffset` appends one length-framed record — `groupLen, group, topicLen, topic, partition, offset` — and `Sync`s it, so an acknowledged commit is on disk. On `Open`, the store replays the whole file, last-write-wins per `(group, topic, partition)` key, rebuilding the in-memory map. A commit interrupted by a crash leaves a partial trailing record, which replay detects by a short read and ignores — the same tail-truncation rule the message segment uses. The result is that two independent durable structures, the message log and the offset log, together make the full cycle correct: messages survive in the segments, the committed offset survives in the offset store, and a replacement consumer resumes exactly where its predecessor stopped.

The replay semantics fall out of one rule: resume from `committed + 1`, and read `committed` as `-1` when nothing was ever committed. A consumer that processed records but crashed before committing leaves the committed offset behind its actual progress, so the next consumer re-reads and reprocesses the tail — at-least-once delivery. A consumer that committed leaves the offset at its true progress, so the next consumer skips what is done — no redundant work. The same code path delivers both behaviors; the only difference is whether the commit landed.

Create `log.go`:

```go
package broker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

const headerSize = 24

// Message is one record in a partition log.
type Message struct {
	Offset    int64
	Timestamp time.Time
	Key       []byte
	Value     []byte
}

type indexEntry struct {
	offset  int64
	filePos int64
}

// PartitionLog is an append-only binary log for a single partition.
type PartitionLog struct {
	mu      sync.Mutex
	file    *os.File
	index   []indexEntry
	nextOff int64
	size    int64
}

func newPartitionLog(dir string) (*PartitionLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("partition: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(dir+"/segment.log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("partition: open: %w", err)
	}
	pl := &PartitionLog{file: f}
	if err := pl.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return pl, nil
}

func (pl *PartitionLog) append(key, value []byte) (int64, error) {
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
	return off, nil
}

// readFrom returns up to maxMsgs messages at startOffset (non-blocking).
func (pl *PartitionLog) readFrom(startOffset int64, maxMsgs int) ([]*Message, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
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

func (pl *PartitionLog) close() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.file.Close()
}
```

Create `offsets.go`:

```go
package broker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
)

type offsetKey struct {
	group     string
	topic     string
	partition int
}

// offsetStore is an append-only durable record of committed offsets. Each commit
// appends one length-framed record and is fsynced; Open replays the file.
type offsetStore struct {
	mu     sync.Mutex
	file   *os.File
	values map[offsetKey]int64
}

func openOffsetStore(path string) (*offsetStore, error) {
	s := &offsetStore{values: make(map[offsetKey]int64)}
	if data, err := os.ReadFile(path); err == nil {
		if perr := s.parse(data); perr != nil {
			return nil, perr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("offsets: read %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("offsets: open %s: %w", path, err)
	}
	s.file = f
	return s, nil
}

// parse replays committed records, ignoring a partial trailing record.
func (s *offsetStore) parse(data []byte) error {
	pos := 0
	for pos < len(data) {
		if pos+2 > len(data) {
			break
		}
		gl := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+gl+2 > len(data) {
			break
		}
		group := string(data[pos : pos+gl])
		pos += gl
		tl := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+tl+12 > len(data) {
			break
		}
		topic := string(data[pos : pos+tl])
		pos += tl
		partition := int(int32(binary.BigEndian.Uint32(data[pos : pos+4])))
		pos += 4
		offset := int64(binary.BigEndian.Uint64(data[pos : pos+8]))
		pos += 8
		s.values[offsetKey{group, topic, partition}] = offset
	}
	return nil
}

func (s *offsetStore) commit(group, topic string, partition int, offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf := make([]byte, 0, 2+len(group)+2+len(topic)+12)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], uint16(len(group)))
	buf = append(buf, u16[:]...)
	buf = append(buf, group...)
	binary.BigEndian.PutUint16(u16[:], uint16(len(topic)))
	buf = append(buf, u16[:]...)
	buf = append(buf, topic...)
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(partition))
	buf = append(buf, u32[:]...)
	var u64 [8]byte
	binary.BigEndian.PutUint64(u64[:], uint64(offset))
	buf = append(buf, u64[:]...)

	if _, err := s.file.Write(buf); err != nil {
		return fmt.Errorf("offsets: write: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("offsets: sync: %w", err)
	}
	s.values[offsetKey{group, topic, partition}] = offset
	return nil
}

func (s *offsetStore) fetch(group, topic string, partition int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	off, ok := s.values[offsetKey{group, topic, partition}]
	if !ok {
		return -1
	}
	return off
}

func (s *offsetStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}
```

Create `broker.go`:

```go
package broker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrTopicNotFound    = errors.New("broker: topic not found")
	ErrPartitionInvalid = errors.New("broker: invalid partition")
)

const offsetsFile = "offsets.log"

// Broker composes durable partition logs and a durable offset store.
type Broker struct {
	mu      sync.RWMutex
	dir     string
	topics  map[string][]*PartitionLog
	offsets *offsetStore
}

// Open opens (or creates) a broker rooted at dir, recovering message logs and
// committed offsets from disk.
func Open(dir string) (*Broker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("broker: mkdir %s: %w", dir, err)
	}
	b := &Broker{dir: dir, topics: make(map[string][]*PartitionLog)}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("broker: readdir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		topicDir := filepath.Join(dir, e.Name())
		parts, err := os.ReadDir(topicDir)
		if err != nil {
			return nil, fmt.Errorf("broker: readdir topic %s: %w", e.Name(), err)
		}
		var partitions []*PartitionLog
		for _, pe := range parts {
			if !pe.IsDir() {
				continue
			}
			pl, err := newPartitionLog(filepath.Join(topicDir, pe.Name()))
			if err != nil {
				return nil, err
			}
			partitions = append(partitions, pl)
		}
		if len(partitions) > 0 {
			b.topics[e.Name()] = partitions
		}
	}

	store, err := openOffsetStore(filepath.Join(dir, offsetsFile))
	if err != nil {
		return nil, err
	}
	b.offsets = store
	return b, nil
}

// CreateTopic creates a topic with numPartitions partitions. Idempotent.
func (b *Broker) CreateTopic(name string, numPartitions int) error {
	if numPartitions < 1 {
		return fmt.Errorf("broker: %w: partitions must be >= 1", ErrPartitionInvalid)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.topics[name]; ok {
		return nil
	}
	topicDir := filepath.Join(b.dir, name)
	partitions := make([]*PartitionLog, numPartitions)
	for i := range partitions {
		pl, err := newPartitionLog(filepath.Join(topicDir, fmt.Sprintf("partition-%d", i)))
		if err != nil {
			return err
		}
		partitions[i] = pl
	}
	b.topics[name] = partitions
	return nil
}

func (b *Broker) partition(topic string, p int) (*PartitionLog, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	parts, ok := b.topics[topic]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicNotFound, topic)
	}
	if p < 0 || p >= len(parts) {
		return nil, fmt.Errorf("%w: %s/%d", ErrPartitionInvalid, topic, p)
	}
	return parts[p], nil
}

// Produce appends a message and returns its offset.
func (b *Broker) Produce(topic string, partition int, key, value []byte) (int64, error) {
	pl, err := b.partition(topic, partition)
	if err != nil {
		return 0, err
	}
	return pl.append(key, value)
}

// Fetch returns up to maxMsgs messages from offset (non-blocking).
func (b *Broker) Fetch(topic string, partition int, offset int64, maxMsgs int) ([]*Message, error) {
	pl, err := b.partition(topic, partition)
	if err != nil {
		return nil, err
	}
	return pl.readFrom(offset, maxMsgs)
}

// CommitOffset durably records a group's committed offset for a partition.
func (b *Broker) CommitOffset(group, topic string, partition int, offset int64) error {
	return b.offsets.commit(group, topic, partition, offset)
}

// FetchOffset returns a group's committed offset, or -1 if none is committed.
func (b *Broker) FetchOffset(group, topic string, partition int) int64 {
	return b.offsets.fetch(group, topic, partition)
}

// Close closes all partition logs and the offset store.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var first error
	for _, parts := range b.topics {
		for _, pl := range parts {
			if err := pl.close(); err != nil && first == nil {
				first = err
			}
		}
	}
	if err := b.offsets.close(); err != nil && first == nil {
		first = err
	}
	return first
}
```

The integration point is `FetchOffset` reading a value that `CommitOffset` wrote to disk in a previous process lifetime. After `Open` replays `offsets.log`, a group that committed offset 3 before a restart sees `FetchOffset` return 3, and the consumer resumes from 4. Nothing in `Fetch` knows about groups; the consumer drives the loop, asking the offset store where to start and telling it where it finished.

### The runnable demo

The demo runs the full cycle in two broker lifetimes against one directory. The first consumer processes part of the log and commits; the broker is closed mid-stream; a second broker opens, recovers messages and the committed offset, and a replacement consumer resumes past the commit. Output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/replay"
)

func consume(b *broker.Broker, group, topic string, partition int, max int) []int64 {
	start := b.FetchOffset(group, topic, partition) + 1
	msgs, err := b.Fetch(topic, partition, start, max)
	if err != nil {
		log.Fatal(err)
	}
	var processed []int64
	for _, m := range msgs {
		processed = append(processed, m.Offset)
	}
	return processed
}

func main() {
	dir, err := os.MkdirTemp("", "replay-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Phase 1: produce, partially consume, commit, then "crash".
	b1, err := broker.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := b1.CreateTopic("orders", 1); err != nil {
		log.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := b1.Produce("orders", 0, nil, []byte(fmt.Sprintf("order-%d", i))); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("phase 1: produced 6 messages to orders/0")

	processed := consume(b1, "billing", "orders", 0, 4)
	last := processed[len(processed)-1]
	if err := b1.CommitOffset("billing", "orders", 0, last); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("phase 1: billing-1 processed offsets %v, committed offset %d\n", processed, last)
	b1.Close()
	fmt.Println("phase 1: crash (broker closed) before processing the tail")

	// Phase 2: reopen, recover messages and committed offset, resume.
	b2, err := broker.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer b2.Close()
	all, _ := b2.Fetch("orders", 0, 0, 100)
	fmt.Printf("phase 2: recovered %d messages from disk\n", len(all))
	fmt.Printf("phase 2: committed offset survived restart: %d\n", b2.FetchOffset("billing", "orders", 0))

	resume := b2.FetchOffset("billing", "orders", 0) + 1
	fmt.Printf("phase 2: billing-2 resumes from offset %d\n", resume)
	tail := consume(b2, "billing", "orders", 0, 100)
	last2 := tail[len(tail)-1]
	if err := b2.CommitOffset("billing", "orders", 0, last2); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("phase 2: processed offsets %v, committed offset %d\n", tail, last2)
	fmt.Println("end-to-end: every message processed exactly once across the restart")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
phase 1: produced 6 messages to orders/0
phase 1: billing-1 processed offsets [0 1 2 3], committed offset 3
phase 1: crash (broker closed) before processing the tail
phase 2: recovered 6 messages from disk
phase 2: committed offset survived restart: 3
phase 2: billing-2 resumes from offset 4
phase 2: processed offsets [4 5], committed offset 5
end-to-end: every message processed exactly once across the restart
```

### Tests

The first test is the headline integration: drive the full cycle through the public API and assert that across a restart every message is processed exactly once because the committed offset was durable. The second test isolates the at-least-once behavior: when a consumer processes but does not commit, a restart replays the uncommitted records.

Create `replay_test.go`:

```go
package broker

import (
	"fmt"
	"testing"
)

func consumeOffsets(t *testing.T, b *Broker, group, topic string, partition, max int) []int64 {
	t.Helper()
	start := b.FetchOffset(group, topic, partition) + 1
	msgs, err := b.Fetch(topic, partition, start, max)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, m := range msgs {
		got = append(got, m.Offset)
	}
	return got
}

func TestEndToEndReplayAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	b1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.CreateTopic("orders", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := b1.Produce("orders", 0, nil, []byte(fmt.Sprintf("order-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	first := consumeOffsets(t, b1, "billing", "orders", 0, 4)
	if len(first) != 4 || first[0] != 0 || first[3] != 3 {
		t.Fatalf("phase 1 processed %v, want [0 1 2 3]", first)
	}
	if err := b1.CommitOffset("billing", "orders", 0, first[len(first)-1]); err != nil {
		t.Fatal(err)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	b2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()

	if got := b2.FetchOffset("billing", "orders", 0); got != 3 {
		t.Fatalf("committed offset after restart = %d, want 3", got)
	}
	all, err := b2.Fetch("orders", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("recovered %d messages, want 6", len(all))
	}

	tail := consumeOffsets(t, b2, "billing", "orders", 0, 100)
	if len(tail) != 2 || tail[0] != 4 || tail[1] != 5 {
		t.Fatalf("phase 2 processed %v, want [4 5]", tail)
	}

	// Together, the two phases must have processed every offset exactly once.
	seen := make(map[int64]int)
	for _, off := range append(first, tail...) {
		seen[off]++
	}
	for off := int64(0); off < 6; off++ {
		if seen[off] != 1 {
			t.Fatalf("offset %d processed %d times, want exactly 1", off, seen[off])
		}
	}
}

func TestUncommittedTailIsReplayed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	b1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.CreateTopic("t", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := b1.Produce("t", 0, nil, []byte(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Consumer processes all three but crashes before committing anything.
	processed := consumeOffsets(t, b1, "g", "t", 0, 100)
	if len(processed) != 3 {
		t.Fatalf("processed %v, want 3 messages", processed)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	b2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()

	if got := b2.FetchOffset("g", "t", 0); got != -1 {
		t.Fatalf("uncommitted group offset = %d, want -1", got)
	}
	replayed := consumeOffsets(t, b2, "g", "t", 0, 100)
	if len(replayed) != 3 || replayed[0] != 0 {
		t.Fatalf("replayed %v, want all three offsets from 0 (at-least-once)", replayed)
	}
}

func TestCommitOffsetIsDurable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	b1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.CommitOffset("g", "t", 2, 99); err != nil {
		t.Fatal(err)
	}
	if err := b1.CommitOffset("g", "t", 2, 123); err != nil { // last write wins
		t.Fatal(err)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	b2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if got := b2.FetchOffset("g", "t", 2); got != 123 {
		t.Fatalf("durable offset = %d, want 123 (last write wins)", got)
	}
}
```

## Review

The cycle is correct when two durable structures agree across a restart. `TestEndToEndReplayAcrossRestart` is the integration proof: it produces six records, processes and commits the first four, closes the broker, reopens it, and asserts the committed offset comes back as 3, all six messages are recovered, the replacement consumer resumes at 4, and across both phases every offset is processed exactly once. That last check is the whole point of durable offsets — without them the second consumer would restart at 0 and reprocess everything. `TestUncommittedTailIsReplayed` proves the complementary guarantee: a consumer that processes without committing leaves the offset at `-1`, so the replacement replays from the start — at-least-once, never at-most-once. `TestCommitOffsetIsDurable` pins last-write-wins replay of the append-only offset log. The two failure modes to keep in mind: forgetting to `Sync` the offset record (a crash then loses an acknowledged commit, silently turning exactly-once-by-offset back into full replay), and parsing the offset log without tolerating a short trailing record (a crash mid-commit then makes `Open` fail instead of ignoring the partial write).

## Resources

- [Apache Kafka: offset management and `__consumer_offsets`](https://kafka.apache.org/documentation/#impl_offsettracking) — the durable-committed-offset design this exercise mirrors.
- [`os.File.Sync`](https://pkg.go.dev/os#File.Sync) — the durability call that makes an acknowledged commit survive a crash (issues `F_FULLFSYNC` on macOS).
- [Kafka delivery semantics](https://kafka.apache.org/documentation/#semantics) — at-least-once vs exactly-once, and why commit placement decides which you get.

---

Back to [03-broker-orchestration.md](03-broker-orchestration.md) | Next: [05-throughput-benchmark.md](05-throughput-benchmark.md)
