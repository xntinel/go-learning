# Exercise 4: Memory-Mapped Reads for Sealed Segments

A segment that is no longer being written to is the most common thing a message queue reads: consumers replaying history, a follower catching up, a compaction job scanning old data. Each of those reads, served through `read(2)`, copies bytes from the kernel page cache into a user buffer. Memory mapping removes that copy — once a segment is sealed and its size is fixed, its bytes can be mapped directly into the process address space and read as an ordinary slice. This exercise builds a segment that serves reads through the file API while it is active and switches to a zero-copy memory map the moment it is sealed.

This module is fully self-contained. It bundles its own copy of the message encoding, defines a `Segment` with both read paths, and ships its own demo and tests. It uses `syscall.Mmap`, a POSIX interface, so it runs on Linux and macOS. Nothing here imports any other exercise.

## What you'll build

```text
store.go             Message, Header, Encode, Decode, error sentinels (from Exercise 1)
segment.go           Segment: Append, Seal (mmap), Read (file or map), Close (munmap)
cmd/
  demo/
    main.go          append, read active (file), seal, read sealed (mmap)
segment_test.go      read-equivalence before and after seal, append-after-seal, empty seal
```

- Files: `store.go`, `segment.go`, `cmd/demo/main.go`, `segment_test.go`.
- Implement: `Segment` with `Append`, `Seal` (which memory-maps the file), `Read` (file path while active, map path once sealed), and `Close` (which unmaps).
- Test: `segment_test.go` reads every offset before and after sealing and asserts identical results, rejects an append after seal, and seals an empty segment without error.
- Verify: `go test -race ./...`

### Why only a sealed segment can be mapped

`syscall.Mmap(fd, 0, length, PROT_READ, MAP_SHARED)` asks the kernel to make `length` bytes of the file appear at an address in the process. The returned `[]byte` aliases the page cache: reading from it faults the relevant pages in on demand and never copies through a user buffer, which is what makes a mapped read cheaper than a `read` syscall and lets the kernel share one cached copy across every reader.

The constraint that drives the whole design is that `length` is fixed at the moment of the call. The active segment is still growing, so a record appended after the mapping lies beyond the mapped region; reading it through a stale map would return zeroes or fault past the end. That is why a segment is mapped only after it is *sealed* — declared full and immutable. Sealing fsyncs the file so every appended byte is durable and the on-disk size is final, then maps exactly that size once and caches the mapping for the life of the segment. While the segment is active, `Read` goes through an ordinary file handle; once it is sealed, `Read` decodes straight out of the mapped slice. The two paths must return identical messages — that read-equivalence is the property the tests pin.

A dense position index keeps the read O(1) in this exercise so the focus stays on the two read paths rather than on the search: `positions[i]` is the byte offset of the record at relative offset `i`. Locating a record is a slice index; decoding it is either a file seek-and-read or a slice into the map. The mapped slice is handed to `Decode`, which copies the fields it needs into a fresh `Message`, so the decoded message stays valid even after the map is later unmapped.

The mapping is a kernel resource, not Go-managed memory, so it must be released explicitly: `Close` calls `syscall.Munmap` before closing the file. Unmapping after the file is closed, or not at all, leaks the mapping; reading the slice after unmapping faults. `Close` does them in the right order.

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
	ErrSealed           = errors.New("msgstore: segment is sealed")
)

// Header is a single key/value pair attached to a Message.
type Header struct {
	Key   []byte
	Value []byte
}

// Message is the unit of storage. Offset and Timestamp are assigned on Append.
type Message struct {
	Offset    int64
	Timestamp int64 // Unix nanoseconds
	Key       []byte
	Value     []byte
	Headers   []Header
}

// minRecord is the encoded size of a message with empty key, value, and headers.
const minRecord = 8 + 8 + 4 + 4 + 2 + 4

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

// Decode validates the CRC32 then deserializes a Message. It copies every field
// out of data, so the returned Message stays valid after data is freed.
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
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// Segment is a single append-only file that can be sealed and memory-mapped.
// While active it reads through the file API; once sealed it reads from the map.
type Segment struct {
	mu         sync.RWMutex
	f          *os.File
	path       string
	baseOffset int64
	nextOffset int64
	size       int64

	positions []int64 // positions[rel] = byte offset of the record at relative offset rel
	sealed    bool
	mmap      []byte // non-nil once sealed with a non-empty file
}

// NewSegment creates a fresh, empty, active segment at path with the given base
// offset.
func NewSegment(path string, baseOffset int64) (*Segment, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("msgstore: create segment: %w", err)
	}
	return &Segment{f: f, path: path, baseOffset: baseOffset, nextOffset: baseOffset}, nil
}

// Append writes msg to the active segment and returns its absolute offset.
func (s *Segment) Append(msg *Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return 0, ErrSealed
	}
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
	if _, err := s.f.Write(lenBuf); err != nil {
		return 0, err
	}
	if _, err := s.f.Write(data); err != nil {
		return 0, err
	}

	s.positions = append(s.positions, pos)
	s.size += int64(4 + len(data))
	s.nextOffset++
	return offset, nil
}

// Seal flushes the segment, marks it immutable, and memory-maps its bytes.
// After Seal, Append fails and Read serves from the map.
func (s *Segment) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	info, err := s.f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		data, err := syscall.Mmap(int(s.f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("msgstore: mmap: %w", err)
		}
		s.mmap = data
	}
	s.sealed = true
	return nil
}

// Read returns the message at the given absolute offset.
func (s *Segment) Read(offset int64) (*Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rel := offset - s.baseOffset
	if rel < 0 || rel >= int64(len(s.positions)) {
		return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, offset)
	}
	pos := s.positions[rel]
	if s.mmap != nil {
		return decodeAt(s.mmap, pos)
	}
	return s.readFileAt(pos)
}

// decodeAt decodes the record whose length prefix starts at pos in the mapped
// slice. No bytes are copied until Decode copies the fields it keeps.
func decodeAt(data []byte, pos int64) (*Message, error) {
	if pos+4 > int64(len(data)) {
		return nil, ErrShortRecord
	}
	recLen := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
	start := pos + 4
	if start+recLen > int64(len(data)) {
		return nil, ErrShortRecord
	}
	return Decode(data[start : start+recLen])
}

// readFileAt decodes the record at pos through an independent file handle.
func (s *Segment) readFileAt(pos int64) (*Message, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		return nil, err
	}
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(f, lenBuf); err != nil {
		return nil, err
	}
	recBuf := make([]byte, int(binary.BigEndian.Uint32(lenBuf)))
	if _, err := io.ReadFull(f, recBuf); err != nil {
		return nil, err
	}
	return Decode(recBuf)
}

// Sealed reports whether the segment has been sealed and mapped.
func (s *Segment) Sealed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sealed
}

// Close unmaps the segment (if mapped) and closes the file, in that order.
func (s *Segment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mmap != nil {
		if err := syscall.Munmap(s.mmap); err != nil {
			s.f.Close()
			return err
		}
		s.mmap = nil
	}
	return s.f.Close()
}
```

### The runnable demo

The demo appends four messages, reads one back while the segment is still active (the file path), seals the segment, then reads every message back from the memory map, printing whether the read came from a mapped segment and proving the values are identical across the two paths.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/mmap-reads"
)

func main() {
	path := filepath.Join(os.TempDir(), "mmap-reads-demo.log")
	_ = os.Remove(path)
	defer os.Remove(path)

	seg, err := msgstore.NewSegment(path, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer seg.Close()

	keys := []string{"alpha", "bravo", "charlie", "delta"}
	for _, k := range keys {
		if _, err := seg.Append(&msgstore.Message{Key: []byte(k), Value: []byte("payload")}); err != nil {
			log.Fatal(err)
		}
	}

	// Read while active: this goes through the file API.
	msg, _ := seg.Read(1)
	fmt.Printf("active read  sealed=%v offset=%d key=%s\n", seg.Sealed(), msg.Offset, msg.Key)

	if err := seg.Seal(); err != nil {
		log.Fatal(err)
	}

	// Reads now come from the memory map.
	for off := int64(0); off < int64(len(keys)); off++ {
		msg, err := seg.Read(off)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("mapped read  sealed=%v offset=%d key=%s\n", seg.Sealed(), msg.Offset, msg.Key)
	}

	// A sealed segment refuses further appends.
	_, err = seg.Append(&msgstore.Message{Key: []byte("echo")})
	fmt.Printf("append after seal rejected: %v\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
active read  sealed=false offset=1 key=bravo
mapped read  sealed=true offset=0 key=alpha
mapped read  sealed=true offset=1 key=bravo
mapped read  sealed=true offset=2 key=charlie
mapped read  sealed=true offset=3 key=delta
append after seal rejected: true
```

### Tests

`TestReadEquivalence` appends a batch, reads every offset while active, seals, reads every offset again from the map, and asserts the two sets of messages are byte-for-byte identical — the core property of the mmap path. `TestAppendAfterSealFails` asserts an append on a sealed segment returns `ErrSealed`. `TestSealEmptySegment` seals a segment with no records and asserts it succeeds with no mapping and that reads still report not-found rather than crashing.

Create `segment_test.go`:

```go
package msgstore

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestReadEquivalence(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/seg.log"
	seg, err := NewSegment(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()

	const n = 64
	for i := 0; i < n; i++ {
		if _, err := seg.Append(&Message{Key: []byte(fmt.Sprintf("k%d", i)), Value: []byte(fmt.Sprintf("v%d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	active := make([]*Message, n)
	for i := 0; i < n; i++ {
		m, err := seg.Read(int64(i))
		if err != nil {
			t.Fatalf("active Read(%d): %v", i, err)
		}
		active[i] = m
	}

	if err := seg.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !seg.Sealed() {
		t.Fatal("segment should report sealed")
	}

	for i := 0; i < n; i++ {
		m, err := seg.Read(int64(i))
		if err != nil {
			t.Fatalf("mapped Read(%d): %v", i, err)
		}
		if m.Offset != active[i].Offset || !bytes.Equal(m.Key, active[i].Key) || !bytes.Equal(m.Value, active[i].Value) {
			t.Fatalf("offset %d: mapped read %+v != active read %+v", i, m, active[i])
		}
	}
}

func TestAppendAfterSealFails(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/seg.log"
	seg, err := NewSegment(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()

	if _, err := seg.Append(&Message{Value: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := seg.Seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := seg.Append(&Message{Value: []byte("y")}); !errors.Is(err, ErrSealed) {
		t.Fatalf("expected ErrSealed, got %v", err)
	}
}

func TestSealEmptySegment(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/seg.log"
	seg, err := NewSegment(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()

	if err := seg.Seal(); err != nil {
		t.Fatalf("Seal empty: %v", err)
	}
	if _, err := seg.Read(0); !errors.Is(err, ErrOffsetNotFound) {
		t.Fatalf("expected ErrOffsetNotFound, got %v", err)
	}
}
```

## Review

The mmap path is correct when a sealed segment returns exactly what the active segment did. The read-equivalence test is the proof: every offset decoded from the file before sealing must equal the same offset decoded from the map afterward. Confirm that `Seal` fsyncs before mapping so the on-disk size is final, that it maps only when the file is non-empty, and that `Read` chooses the map path solely on `s.mmap != nil` so an empty sealed segment falls back to the (also valid) file path. Confirm too that `Decode` copies fields out of the mapped slice, so a returned `Message` outlives the mapping and `Close`'s `Munmap` cannot invalidate data already handed to a caller.

The mistakes here are specific to the kernel resource. Mapping the active segment captures a stale size and silently truncates every later record — map only after `Seal`. Closing the file before unmapping, or never unmapping, leaks the mapping, while reading the slice after `Munmap` faults — `Close` unmaps first, then closes. And mapping a zero-length file is an error on many systems, so the empty-segment case must skip the `Mmap` call and serve through the file path instead.

## Resources

- [`syscall.Mmap`](https://pkg.go.dev/syscall#Mmap) and [`syscall.Munmap`](https://pkg.go.dev/syscall#Munmap) — the POSIX mapping primitives this segment uses for zero-copy reads.
- [mmap(2) (Linux man page)](https://man7.org/linux/man-pages/man2/mmap.2.html) — the semantics of `PROT_READ` / `MAP_SHARED` and why a mapping is fixed to the length at the call.
- [Apache Kafka: Log Internals](https://kafka.apache.org/documentation/#log) — Kafka maps its index and log files for sealed segments for exactly this reason.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-crash-recovery.md](03-crash-recovery.md) | Next: [../03-consumer-groups-offset-tracking/00-concepts.md](../03-consumer-groups-offset-tracking/00-concepts.md)
