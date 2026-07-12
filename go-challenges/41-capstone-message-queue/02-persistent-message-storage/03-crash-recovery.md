# Exercise 3: Crash Recovery and Replay

Writing a crash-resumable log is only half a durability story; the other half is the moment after the lights come back on. The process may have died in the middle of an append, leaving a half-written record at the tail of the file. Reopening the store must turn that file back into a clean, ordered sequence — repairing the torn tail in place and rebuilding the in-memory state (the offset index and the next-offset counter) by replaying every record that survived. This exercise builds that recovery path and the one positional rule that makes it correct: a bad record at the very end of the log is an expected crash artifact, while a bad record anywhere earlier is real corruption.

This module is fully self-contained. It bundles its own copy of the message encoding, defines a single-file `Store` whose `Open` performs recovery, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
store.go             Message, Header, Encode, Decode, error sentinels (from Exercise 1)
recovery.go          Store: Open (recover + replay), Append, Read, Count, HighestOffset, Close
cmd/
  demo/
    main.go          append, close, reopen-and-replay, then survive a torn tail
recovery_test.go     append+recover round-trip, torn-tail truncation, corrupt-tail drop
```

- Files: `store.go`, `recovery.go`, `cmd/demo/main.go`, `recovery_test.go`.
- Implement: `Store` with `Open(path)`, a `recover` pass that rebuilds the index and truncates the tail, plus `Append`, `Read`, `Count`, and `HighestOffset`.
- Test: `recovery_test.go` asserts every appended message comes back after a reopen, that a garbage tail is truncated, and that a record with a flipped CRC at the end is dropped.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/02-persistent-message-storage/03-crash-recovery/cmd/demo && cd go-solutions/41-capstone-message-queue/02-persistent-message-storage/03-crash-recovery
```

### Recovery is a single forward pass that rebuilds state

When `Open` is handed an existing file, it knows nothing about it: not how many records it holds, not where the last good one ends, not whether the process that wrote it died mid-append. The only way to learn is to read the file from byte zero, record by record, and the same pass answers every question at once. For each record it reads the 4-byte length prefix, then the body, validates the CRC32 through `Decode`, and on success records the message's offset and the byte position where its length prefix began. That `offset to position` map is the in-memory state that lets a later `Read` jump straight to a record instead of rescanning; the running `nextOffset` is the counter the next `Append` continues from.

The pass stops at the first thing it cannot trust, and there are three such things, all expected at the tail of a crashed log. A short read on the length prefix or the body means the process wrote part of a record and died — `io.ReadFull` returns an error and the scan ends. A length prefix that is absurd — smaller than the minimum record, or large enough to run past the end of the file — is a torn or garbage length, and is rejected *before* allocating a buffer for it, so a corrupt four bytes can never trigger a multi-gigabyte allocation. A body that reads fully but fails its CRC32 is a record whose bytes were only partially flushed. In every case the position where the scan stopped is the end of the last clean record, and everything after it is garbage that `Truncate` removes, so the next append lands exactly where the durable log ended.

### Why the tail is special and the middle is not

The positional rule is the heart of recovery. A torn or corrupt record at the *tail* is a crash artifact: the process was appending there when it died, and those bytes were never acknowledged as durable, so discarding them loses nothing a caller was promised. The same damage in the *middle* of the log is a different and graver event — it means a record that was already written, and whose successors were durably written after it, is now unreadable, which is genuine data loss that recovery cannot paper over.

This single-file store enforces the rule by construction: it only ever stops at, and truncates from, the point where decoding first fails while scanning forward from the start. It can never silently skip a bad record and keep the good ones after it, because it does not know where the next record would begin once framing is lost — the length prefix it would need is exactly the thing that was corrupted. Stopping at the first failure is therefore both the safe policy and the only one the framing supports. (A multi-segment log applies the same rule across files: only the highest-numbered, active segment may end in a torn tail; a decode failure inside an already-sealed segment is reported as an error, not truncated.)

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

Create `recovery.go`:

```go
package msgstore

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

// Store is a single-file, append-only message store that recovers a torn tail
// on Open and serves reads from an in-memory offset index.
type Store struct {
	mu   sync.Mutex
	f    *os.File
	path string

	index      map[int64]int64 // offset -> byte position of its length prefix
	order      []int64         // offsets in append order
	nextOffset int64
	size       int64 // valid bytes; the position of the next append
}

// Open opens or creates the store at path and recovers it: it replays every
// durable record to rebuild the index, then truncates any torn tail in place.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("msgstore: open: %w", err)
	}
	s := &Store{f: f, path: path, index: make(map[int64]int64)}
	if err := s.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// recover scans the file from byte zero, rebuilding the offset index and the
// next-offset counter, and truncates whatever follows the last clean record.
func (s *Store) recover() error {
	info, err := s.f.Stat()
	if err != nil {
		return err
	}
	fileSize := info.Size()
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(s.f)

	var pos int64
	lenBuf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			break // clean EOF or a torn length prefix
		}
		recLen := int64(binary.BigEndian.Uint32(lenBuf))
		// Reject a garbage length before allocating for it.
		if recLen < minRecord || pos+4+recLen > fileSize {
			break
		}
		recBuf := make([]byte, recLen)
		if _, err := io.ReadFull(r, recBuf); err != nil {
			break // truncated body
		}
		msg, err := Decode(recBuf)
		if err != nil {
			break // corrupt record at the tail
		}
		s.index[msg.Offset] = pos
		s.order = append(s.order, msg.Offset)
		s.nextOffset = msg.Offset + 1
		pos += 4 + recLen
	}

	if fileSize > pos {
		if err := s.f.Truncate(pos); err != nil {
			return err
		}
	}
	s.size = pos
	return nil
}

// Append writes msg, fsyncs, and returns its assigned offset.
func (s *Store) Append(msg *Message) (int64, error) {
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
	if _, err := s.f.Write(lenBuf); err != nil {
		return 0, err
	}
	if _, err := s.f.Write(data); err != nil {
		return 0, err
	}
	if err := s.f.Sync(); err != nil {
		return 0, err
	}

	s.index[offset] = pos
	s.order = append(s.order, offset)
	s.size += int64(4 + len(data))
	s.nextOffset++
	return offset, nil
}

// Read returns the message at offset using the in-memory index.
func (s *Store) Read(offset int64) (*Message, error) {
	s.mu.Lock()
	pos, ok := s.index[offset]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrOffsetNotFound, offset)
	}

	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, err
	}
	recBuf := make([]byte, int(binary.BigEndian.Uint32(lenBuf)))
	if _, err := io.ReadFull(r, recBuf); err != nil {
		return nil, err
	}
	return Decode(recBuf)
}

// Count returns the number of recovered or appended messages.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

// HighestOffset returns the last offset, or -1 if the store is empty.
func (s *Store) HighestOffset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextOffset - 1
}

// Close flushes and closes the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.f.Sync(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}
```

### The runnable demo

The demo writes three messages and closes the store, then reopens it to show recovery rebuilds the count and highest offset from disk alone. It then simulates a crash mid-append by writing a stray length prefix and a few garbage bytes to the end of the file, reopens once more, and shows recovery truncates the torn tail and leaves the three good messages intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/crash-recovery"
)

func main() {
	path := filepath.Join(os.TempDir(), "crash-recovery-demo.log")
	_ = os.Remove(path)
	defer os.Remove(path)

	s, err := msgstore.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := s.Append(&msgstore.Message{Key: []byte(k), Value: []byte("v")}); err != nil {
			log.Fatal(err)
		}
	}
	s.Close()

	// Reopen: recovery replays the durable records.
	s2, err := msgstore.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after reopen: count=%d highest=%d\n", s2.Count(), s2.HighestOffset())
	s2.Close()

	// Simulate a crash mid-append: a length prefix claiming 200 bytes plus a
	// short, incomplete body.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	var torn [4]byte
	binary.BigEndian.PutUint32(torn[:], 200)
	f.Write(torn[:])
	f.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF})
	f.Close()

	// Reopen: recovery truncates the torn tail.
	s3, err := msgstore.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer s3.Close()
	fmt.Printf("after torn tail: count=%d highest=%d\n", s3.Count(), s3.HighestOffset())
	msg, err := s3.Read(2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read offset=%d key=%s value=%s\n", msg.Offset, msg.Key, msg.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after reopen: count=3 highest=2
after torn tail: count=3 highest=2
read offset=2 key=c value=v
```

### Tests

`TestAppendAndRecover` appends a batch, closes, reopens, and asserts the count, highest offset, and every value survive a reopen — recovery rebuilt the index from disk. `TestRecoverTruncatesTornTail` appends, closes, writes a stray length prefix with a too-short body to the tail, reopens, and asserts the good records remain and a fresh append continues at the right offset. `TestRecoverDropsCorruptTail` flips a byte inside the last record on disk, reopens, and asserts that record is dropped while its predecessors survive.

Create `recovery_test.go`:

```go
package msgstore

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

func appendN(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Append(&Message{Key: []byte(fmt.Sprintf("k%d", i)), Value: []byte(fmt.Sprintf("v%d", i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

func TestAppendAndRecover(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/store.log"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, s, 50)
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.Count() != 50 {
		t.Fatalf("count after recover: got %d, want 50", s2.Count())
	}
	if s2.HighestOffset() != 49 {
		t.Fatalf("highest after recover: got %d, want 49", s2.HighestOffset())
	}
	for i := 0; i < 50; i++ {
		got, err := s2.Read(int64(i))
		if err != nil {
			t.Fatalf("Read(%d): %v", i, err)
		}
		if want := fmt.Sprintf("v%d", i); string(got.Value) != want {
			t.Errorf("offset %d: got %q, want %q", i, got.Value, want)
		}
	}
}

func TestRecoverTruncatesTornTail(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/store.log"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, s, 10)
	s.Close()

	// Append a stray length prefix claiming 500 bytes plus a 5-byte body.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	var torn [4]byte
	binary.BigEndian.PutUint32(torn[:], 500)
	f.Write(torn[:])
	f.Write([]byte("short"))
	f.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.Count() != 10 {
		t.Fatalf("count after torn-tail recover: got %d, want 10", s2.Count())
	}
	// The next append must continue at offset 10 and be readable.
	off, err := s2.Append(&Message{Value: []byte("after")})
	if err != nil {
		t.Fatal(err)
	}
	if off != 10 {
		t.Fatalf("append after recover: got offset %d, want 10", off)
	}
	got, err := s2.Read(10)
	if err != nil {
		t.Fatalf("Read(10): %v", err)
	}
	if string(got.Value) != "after" {
		t.Fatalf("Read(10): got %q, want %q", got.Value, "after")
	}
}

func TestRecoverDropsCorruptTail(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/store.log"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, s, 5)
	// Position of the last record (offset 4) before closing.
	lastPos := s.index[4]
	s.Close()

	// Flip a byte inside the last record's body (past its 4-byte length prefix).
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(lastPos+8, 0); err != nil {
		t.Fatal(err)
	}
	one := make([]byte, 1)
	f.Read(one)
	one[0] ^= 0xFF
	f.Seek(lastPos+8, 0)
	f.Write(one)
	f.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.Count() != 4 {
		t.Fatalf("count after corrupt-tail recover: got %d, want 4", s2.Count())
	}
	if _, err := s2.Read(4); err == nil {
		t.Fatal("expected offset 4 to be dropped, but Read succeeded")
	}
	if got, err := s2.Read(3); err != nil || string(got.Value) != "v3" {
		t.Fatalf("Read(3): got %v, %v", got, err)
	}
}
```

## Review

Recovery is correct when a reopen reconstructs exactly the durable prefix of the log and nothing more. Confirm that the forward scan stops at the first decode failure and truncates from there, so the file after `Open` ends on a clean record boundary and the next append continues at `nextOffset` without a gap. The torn-tail test proves a garbage length plus a short body is discarded; the corrupt-tail test proves a flipped byte in the last record drops that record and keeps its predecessors. Both leave the store immediately usable, which is the real goal — recovery that returns an error on every post-crash open is no recovery at all.

The mistakes to avoid are concentrated in the scan. Allocating `make([]byte, recLen)` before checking `recLen` against the remaining file size lets a corrupt four-byte length trigger a huge allocation or a scan past the data — bound the length first. Treating a tail CRC failure as fatal makes every crashed log unopenable, which defeats the purpose; the failure is expected and the response is truncation. And forgetting to truncate after the scan leaves the garbage bytes in the file, so the next append writes *after* them and the log can never be cleanly read again — the `Truncate(pos)` is what makes the repair durable.

## Resources

- [`os.File.Truncate`](https://pkg.go.dev/os#File.Truncate) — the in-place repair that removes the torn tail down to the last clean record.
- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — distinguishes a clean EOF on a record boundary from a body cut short mid-write.
- [SQLite: Atomic Commit](https://www.sqlite.org/atomiccommit.html) — how a database treats a partially written tail after a crash as something to discard, not to trust.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-segmented-log.md](02-segmented-log.md) | Next: [04-mmap-reads.md](04-mmap-reads.md)
