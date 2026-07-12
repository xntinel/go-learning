# 1. Partitioned Storage Engine

A distributed key-value store rests on a single hard guarantee: every key must map deterministically to exactly one partition, reads and writes on different partitions must not block each other, and the engine must survive a crash without losing committed writes. This lesson builds that foundation — a consistent-hash ring that routes keys to partitions, a per-partition write-ahead log that provides crash durability, and a cross-partition scan that returns keys in sorted order. Every subsequent lesson in this capstone extends what you build here.

```text
kvstore/
  go.mod
  ring.go
  engine.go
  engine_test.go
  cmd/demo/main.go
```

## Concepts

### Consistent Hashing and Virtual Nodes

A naive modulo-hash assignment (`key % N`) breaks badly when N changes: adding one node reassigns nearly every key. Consistent hashing fixes this by mapping both nodes and keys onto a circular token space. A key is owned by the first node whose token is >= the key's hash (wrapping around the ring). When a node is added or removed, only the keys between that node's predecessors and its new position move.

The catch is that with few physical nodes the positions on the ring cluster unevenly. The solution — virtual nodes — places each physical node at K positions (typically 100 – 300) spread uniformly by hashing `"node#i"`. Ownership is still the nearest clockwise token, but the density of tokens per region makes the key distribution much more even. At K=256, the standard deviation of key counts across nodes is typically under 5 % for millions of keys.

Implementation: a sorted `[]token` slice, one token per virtual node position, looked up with `sort.Search` in O(log K·N).

### Write-Ahead Logging

A memtable (in-memory map) answers reads and accumulates writes at memory speed. The problem is that an in-memory structure vanishes on crash. The write-ahead log (WAL) fixes this by recording every mutation to disk **before** applying it to the memtable. On restart the engine replays the WAL to rebuild the memtable. The log is append-only; each record includes a CRC32 checksum so that a partial write at the tail (the most common crash artifact) is detected and discarded at replay time.

The tradeoff: every write pays one `fsync`-free sequential disk write plus an in-memory update. Sequential writes to a single append-only file are fast; the bottleneck is rarely the WAL.

### Last-Write-Wins with Timestamps

Clients attach a logical timestamp (nanoseconds, Lamport clock, or HLC) to every `Put` and `Delete`. The rule is simple: the entry with the highest timestamp is the winner. On recovery the map already holds the winning entry for each key; replaying a lower-timestamp record does nothing. Deletes are tombstones — an entry with `Tombstone: true` — so that a delete at ts=10 beats a put at ts=8 even after compaction discards old values.

### Per-Partition Concurrency

Each `Partition` holds its own `sync.RWMutex`. Multiple goroutines can `Get` from different partitions simultaneously. A `Put` or `Delete` acquires the partition mutex, appends to the WAL (under the lock, so that WAL order is the same as memtable update order), then updates the memtable. The WAL mutex and the memtable mutex are the same lock; there is no risk of deadlock, and no global lock serializes unrelated partitions.

### Cross-Partition Scan: the K-Way Merge

Each partition owns a non-overlapping key range (by the hash ring), so a global scan must collect results from every partition and merge them in key order. The simple approach — collect all, sort — works when result sets are small. The production approach is a k-way merge with `container/heap`: maintain one iterator per partition in a min-heap keyed by the current entry's key. Each `Next()` call pops the minimum, advances that partition's iterator, and pushes the new minimum back. This streams results in O(N log P) where N is the total number of entries and P is the number of partitions, without loading all results into memory at once.

This lesson uses the collect-and-sort approach to keep the implementation readable; the exercises point to the heap variant.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/45-capstone-distributed-key-value-store/01-partitioned-storage/01-partitioned-storage/cmd/demo
cd go-solutions/45-capstone-distributed-key-value-store/01-partitioned-storage/01-partitioned-storage
```

### Exercise 1: The Hash Ring

Create `ring.go`. The ring is a sorted slice of `token` values; `sort.Search` provides O(log n) owner lookup.

```go
package kvstore

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

// token is one virtual-node position on the 64-bit hash ring.
type token struct {
	hash uint64
	node string
}

// Ring is a consistent-hash ring. It is safe for concurrent use.
type Ring struct {
	mu     sync.RWMutex
	tokens []token // sorted ascending by hash
	vnodes int     // virtual nodes per physical node
}

// NewRing returns a Ring that places vnodes virtual positions per physical
// node. If vnodes <= 0 it defaults to 256.
func NewRing(vnodes int) *Ring {
	if vnodes <= 0 {
		vnodes = 256
	}
	return &Ring{vnodes: vnodes}
}

// AddNode places vnodes virtual positions for node on the ring.
func (r *Ring) AddNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < r.vnodes; i++ {
		r.tokens = append(r.tokens, token{
			hash: hashKey(fmt.Sprintf("%s#%d", node, i)),
			node: node,
		})
	}
	sort.Slice(r.tokens, func(i, j int) bool {
		return r.tokens[i].hash < r.tokens[j].hash
	})
}

// RemoveNode removes all virtual positions for node.
func (r *Ring) RemoveNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.tokens[:0]
	for _, t := range r.tokens {
		if t.node != node {
			kept = append(kept, t)
		}
	}
	r.tokens = kept
}

// Owner returns the physical node responsible for key. It returns "" if
// the ring is empty.
func (r *Ring) Owner(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.tokens) == 0 {
		return ""
	}
	h := hashKey(key)
	idx := sort.Search(len(r.tokens), func(i int) bool {
		return r.tokens[i].hash >= h
	})
	// Wrap around to the first token if h is larger than all tokens.
	return r.tokens[idx%len(r.tokens)].node
}

// Nodes returns the distinct physical nodes on the ring, sorted.
func (r *Ring) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, t := range r.tokens {
		seen[t.node] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// hashKey returns a uint64 derived from the first 8 bytes of SHA-256(s).
func hashKey(s string) uint64 {
	sum := sha256.Sum256([]byte(s))
	return binary.BigEndian.Uint64(sum[:8])
}
```

`AddNode` keeps `tokens` sorted after every insertion so that `Owner` can call `sort.Search`. The `%` wrap-around in `Owner` handles the case where the key's hash is larger than every token — it wraps to the first token, closing the ring.

### Exercise 2: The Partition — WAL and Memtable

Create `engine.go`. The partition holds a map-based memtable (key → winning entry) and an append-only WAL file. Every mutation is written to disk before the memtable is updated; replay on startup rebuilds the memtable from the WAL.

WAL record layout (big-endian binary):

```text
[4 bytes] CRC32 checksum of the remaining bytes
[1 byte]  flags (bit 0 = tombstone)
[8 bytes] timestamp (int64)
[4 bytes] key length
[N bytes] key
[4 bytes] value length
[M bytes] value
```

```go
package kvstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Sentinel errors.
var (
	ErrKeyEmpty   = errors.New("key must not be empty")
	ErrNotFound   = errors.New("key not found")
	ErrCorruptWAL = errors.New("WAL record checksum mismatch")
)

// Entry is a key-value pair with a logical timestamp and a tombstone flag.
// A tombstone entry represents a deletion.
type Entry struct {
	Key       string
	Value     string
	Timestamp int64
	Tombstone bool
}

// Partition is one storage shard. All exported methods are safe for
// concurrent use from multiple goroutines.
type Partition struct {
	mu   sync.RWMutex
	data map[string]Entry // key -> highest-timestamp entry (LWW)
	wal  *os.File
}

// openPartition opens (or creates) a Partition rooted at dir and replays
// its WAL to reconstruct the memtable.
func openPartition(dir string) (*Partition, error) {
	walPath := filepath.Join(dir, "wal")
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	p := &Partition{data: make(map[string]Entry), wal: f}
	if err := p.replayWAL(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("WAL replay: %w", err)
	}
	return p, nil
}

func (p *Partition) put(e Entry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.appendWAL(e); err != nil {
		return err
	}
	p.upsertLocked(e)
	return nil
}

func (p *Partition) get(key string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.data[key]
	if !ok || e.Tombstone {
		return "", ErrNotFound
	}
	return e.Value, nil
}

// scan returns all live entries in [start, end), sorted by key.
// An empty start or end means unbounded on that side.
func (p *Partition) scan(start, end string) []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	keys := make([]string, 0, len(p.data))
	for k := range p.data {
		afterStart := start == "" || k >= start
		beforeEnd := end == "" || k < end
		if afterStart && beforeEnd {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	out := make([]Entry, 0, len(keys))
	for _, k := range keys {
		e := p.data[k]
		if !e.Tombstone {
			out = append(out, e)
		}
	}
	return out
}

func (p *Partition) close() error {
	if p.wal != nil {
		return p.wal.Close()
	}
	return nil
}

// upsertLocked applies last-write-wins: the entry with the higher timestamp
// wins. Must be called with p.mu held for writing.
func (p *Partition) upsertLocked(e Entry) {
	existing, ok := p.data[e.Key]
	if !ok || e.Timestamp > existing.Timestamp {
		p.data[e.Key] = e
	}
}

func (p *Partition) appendWAL(e Entry) error {
	_, err := p.wal.Write(encodeWALRecord(e))
	return err
}

func (p *Partition) replayWAL() error {
	if _, err := p.wal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for {
		e, err := decodeWALRecord(p.wal)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Normal end-of-WAL or truncated tail from a crash; stop here.
			break
		}
		if err != nil {
			return err
		}
		p.upsertLocked(e)
	}
	// Return to the end for future appends.
	_, err := p.wal.Seek(0, io.SeekEnd)
	return err
}

func encodeWALRecord(e Entry) []byte {
	keyB := []byte(e.Key)
	valB := []byte(e.Value)
	// Build the payload (everything after the CRC).
	payload := make([]byte, 1+8+4+len(keyB)+4+len(valB))
	off := 0
	var flags byte
	if e.Tombstone {
		flags |= 1
	}
	payload[off] = flags
	off++
	binary.BigEndian.PutUint64(payload[off:], uint64(e.Timestamp))
	off += 8
	binary.BigEndian.PutUint32(payload[off:], uint32(len(keyB)))
	off += 4
	copy(payload[off:], keyB)
	off += len(keyB)
	binary.BigEndian.PutUint32(payload[off:], uint32(len(valB)))
	off += 4
	copy(payload[off:], valB)

	checksum := crc32.ChecksumIEEE(payload)
	rec := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(rec, checksum)
	copy(rec[4:], payload)
	return rec
}

func decodeWALRecord(r io.Reader) (Entry, error) {
	var crcBuf [4]byte
	if _, err := io.ReadFull(r, crcBuf[:]); err != nil {
		return Entry{}, err
	}
	storedCRC := binary.BigEndian.Uint32(crcBuf[:])

	// Fixed-size header: flags(1) + timestamp(8) + keyLen(4) = 13 bytes.
	var hdr [13]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Entry{}, err
	}
	flags := hdr[0]
	ts := int64(binary.BigEndian.Uint64(hdr[1:9]))
	keyLen := int(binary.BigEndian.Uint32(hdr[9:13]))

	keyBuf := make([]byte, keyLen)
	if _, err := io.ReadFull(r, keyBuf); err != nil {
		return Entry{}, err
	}
	var valLenBuf [4]byte
	if _, err := io.ReadFull(r, valLenBuf[:]); err != nil {
		return Entry{}, err
	}
	valLen := int(binary.BigEndian.Uint32(valLenBuf[:]))
	valBuf := make([]byte, valLen)
	if _, err := io.ReadFull(r, valBuf); err != nil {
		return Entry{}, err
	}

	// Reconstruct the payload and verify the checksum.
	payload := make([]byte, 1+8+4+keyLen+4+valLen)
	payload[0] = flags
	binary.BigEndian.PutUint64(payload[1:], uint64(ts))
	binary.BigEndian.PutUint32(payload[9:], uint32(keyLen))
	copy(payload[13:], keyBuf)
	binary.BigEndian.PutUint32(payload[13+keyLen:], uint32(valLen))
	copy(payload[13+keyLen+4:], valBuf)

	if got := crc32.ChecksumIEEE(payload); got != storedCRC {
		return Entry{}, fmt.Errorf("%w: stored %08x computed %08x",
			ErrCorruptWAL, storedCRC, got)
	}
	return Entry{
		Key:       string(keyBuf),
		Value:     string(valBuf),
		Timestamp: ts,
		Tombstone: flags&1 != 0,
	}, nil
}
```

### Exercise 3: The Engine and Cross-Partition Scan

Add the `Engine` type and `mergeScan` to `engine.go`. The engine owns the ring and a map from node name to partition:

```go
// Engine routes key operations to partitions via a consistent-hash ring.
// It is safe for concurrent use.
type Engine struct {
	ring       *Ring
	mu         sync.RWMutex
	partitions map[string]*Partition // node name -> partition
}

// NewEngine creates an Engine that stores each node's partition under
// baseDir/<node>/. If the directory already contains a WAL it is replayed.
func NewEngine(baseDir string, vnodes int, nodes ...string) (*Engine, error) {
	ring := NewRing(vnodes)
	partitions := make(map[string]*Partition, len(nodes))
	for _, n := range nodes {
		ring.AddNode(n)
		dir := filepath.Join(baseDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("engine: mkdir %s: %w", dir, err)
		}
		p, err := openPartition(dir)
		if err != nil {
			return nil, fmt.Errorf("engine: partition %s: %w", n, err)
		}
		partitions[n] = p
	}
	return &Engine{ring: ring, partitions: partitions}, nil
}

// Put writes key=value at the given logical timestamp.
func (e *Engine) Put(key, value string, ts int64) error {
	if key == "" {
		return ErrKeyEmpty
	}
	p, err := e.partitionFor(key)
	if err != nil {
		return err
	}
	return p.put(Entry{Key: key, Value: value, Timestamp: ts})
}

// Delete records a tombstone for key at the given timestamp.
func (e *Engine) Delete(key string, ts int64) error {
	if key == "" {
		return ErrKeyEmpty
	}
	p, err := e.partitionFor(key)
	if err != nil {
		return err
	}
	return p.put(Entry{Key: key, Timestamp: ts, Tombstone: true})
}

// Get returns the current value of key. It returns ErrNotFound if the key
// does not exist or has been deleted.
func (e *Engine) Get(key string) (string, error) {
	if key == "" {
		return "", ErrKeyEmpty
	}
	p, err := e.partitionFor(key)
	if err != nil {
		return "", err
	}
	return p.get(key)
}

// Scan returns all live entries with key in [start, end), sorted by key.
// Empty start or end means unbounded on that side.
func (e *Engine) Scan(start, end string) ([]Entry, error) {
	e.mu.RLock()
	parts := make([]*Partition, 0, len(e.partitions))
	for _, p := range e.partitions {
		parts = append(parts, p)
	}
	e.mu.RUnlock()
	return mergeScan(parts, start, end), nil
}

// Close closes all partitions and their WAL files.
func (e *Engine) Close() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var first error
	for _, p := range e.partitions {
		if err := p.close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (e *Engine) partitionFor(key string) (*Partition, error) {
	node := e.ring.Owner(key)
	e.mu.RLock()
	p, ok := e.partitions[node]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine: no partition for node %q", node)
	}
	return p, nil
}

// mergeScan collects scan results from all partitions and merges them in
// key order. Because the ring routes each key to exactly one partition,
// there is no key overlap between partitions and no deduplication is needed.
//
// Production systems use a k-way merge with container/heap to stream results
// without loading everything into memory first; see Resources for details.
func mergeScan(parts []*Partition, start, end string) []Entry {
	var all []Entry
	for _, p := range parts {
		all = append(all, p.scan(start, end)...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Key < all[j].Key
	})
	return all
}
```

### Exercise 4: Test Suite and Demo

Create `engine_test.go`. The tests use `t.TempDir()` so there is no cleanup overhead, `t.Parallel()` for concurrency safety, and `errors.Is` for sentinel error assertions.

```go
package kvstore

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func newTestEngine(t *testing.T, nodes ...string) *Engine {
	t.Helper()
	if len(nodes) == 0 {
		nodes = []string{"node-a", "node-b", "node-c"}
	}
	eng, err := NewEngine(t.TempDir(), 64, nodes...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func TestRingOwnerIsStable(t *testing.T) {
	t.Parallel()

	r := NewRing(64)
	r.AddNode("alpha")
	r.AddNode("beta")
	r.AddNode("gamma")

	// The owner of a key must not change between calls.
	want := r.Owner("stable-key")
	for i := 0; i < 100; i++ {
		if got := r.Owner("stable-key"); got != want {
			t.Fatalf("owner changed: %q -> %q", want, got)
		}
	}
}

func TestRingDistribution(t *testing.T) {
	t.Parallel()

	r := NewRing(256)
	nodes := []string{"n1", "n2", "n3"}
	for _, n := range nodes {
		r.AddNode(n)
	}

	counts := make(map[string]int)
	const total = 10_000
	for i := 0; i < total; i++ {
		counts[r.Owner(fmt.Sprintf("key-%d", i))]++
	}
	// Each node should own between 20 % and 47 % of keys.
	for _, n := range nodes {
		ratio := float64(counts[n]) / float64(total)
		if ratio < 0.20 || ratio > 0.47 {
			t.Errorf("node %s owns %.1f%% of keys (want 20–47%%)", n, ratio*100)
		}
	}
}

func TestEnginePutGet(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "node-a")
	if err := eng.Put("hello", "world", 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, err := eng.Get("hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "world" {
		t.Fatalf("Get = %q, want %q", v, "world")
	}
}

func TestEngineGetNotFound(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "node-a")
	_, err := eng.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestEngineKeyEmpty(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "node-a")
	if err := eng.Put("", "v", 1); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Put(\"\") err = %v, want ErrKeyEmpty", err)
	}
	if _, err := eng.Get(""); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Get(\"\") err = %v, want ErrKeyEmpty", err)
	}
}

func TestEngineDeleteTombstone(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "node-a")
	if err := eng.Put("x", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if err := eng.Delete("x", 2); err != nil {
		t.Fatal(err)
	}
	_, err := eng.Get("x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestEngineLWW(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "node-a")
	// Write with a higher timestamp first, then overwrite with a lower one.
	// The first (higher-timestamp) value must win.
	if err := eng.Put("k", "new", 100); err != nil {
		t.Fatal(err)
	}
	if err := eng.Put("k", "old", 1); err != nil {
		t.Fatal(err)
	}
	v, err := eng.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "new" {
		t.Fatalf("LWW: Get = %q, want %q", v, "new")
	}
}

func TestEngineScanRange(t *testing.T) {
	t.Parallel()

	// Use a single node so all keys land in the same partition; the
	// cross-partition merge path is exercised by TestEngineScanCrossPartition.
	eng := newTestEngine(t, "node-a")

	keys := []string{"apple", "banana", "cherry", "date", "elderberry"}
	for i, k := range keys {
		if err := eng.Put(k, fmt.Sprintf("v%d", i), int64(i+1)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}

	got, err := eng.Scan("banana", "date")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"banana", "cherry"}
	if len(got) != len(want) {
		t.Fatalf("Scan returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.Key != want[i] {
			t.Errorf("entry[%d].Key = %q, want %q", i, e.Key, want[i])
		}
	}
}

func TestEngineScanCrossPartition(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "n1", "n2", "n3")

	// Insert 30 keys; they will be distributed across the three partitions.
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("key-%02d", i)
		if err := eng.Put(key, "v", int64(i+1)); err != nil {
			t.Fatalf("Put(%s): %v", key, err)
		}
	}

	entries, err := eng.Scan("", "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 30 {
		t.Fatalf("Scan returned %d entries, want 30", len(entries))
	}
	// Verify sorted order.
	for i := 1; i < len(entries); i++ {
		if entries[i].Key <= entries[i-1].Key {
			t.Errorf("entries not sorted at [%d]: %q <= %q",
				i, entries[i].Key, entries[i-1].Key)
		}
	}
}

func TestEngineWALReplay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write three entries to one engine, close it.
	eng1, err := NewEngine(dir, 64, "node-a")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := eng1.Put(key, fmt.Sprintf("v%d", i), int64(i+1)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := eng1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Open a new engine pointing at the same directory — it must replay the WAL.
	eng2, err := NewEngine(dir, 64, "node-a")
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer func() { _ = eng2.Close() }()

	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("k%d", i)
		want := fmt.Sprintf("v%d", i)
		got, err := eng2.Get(key)
		if err != nil {
			t.Fatalf("Get(%s) after replay: %v", key, err)
		}
		if got != want {
			t.Fatalf("Get(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestEngineRace(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t, "n1", "n2", "n3")
	const goroutines = 32
	const opsEach = 50

	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer func() { done <- struct{}{} }()
			for op := 0; op < opsEach; op++ {
				key := fmt.Sprintf("g%d-k%d", g, op)
				_ = eng.Put(key, "v", int64(op))
				_, _ = eng.Get(key)
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
}

// Your turn: add TestEngineUpdateBeatsOlderTimestamp that puts a key at ts=5,
// then puts the same key at ts=10, and asserts Get returns the ts=10 value.

func ExampleRing_Owner() {
	r := NewRing(4)
	r.AddNode("alpha")
	r.AddNode("beta")
	// Owner always returns one of the registered nodes for any key.
	owner := r.Owner("hello")
	fmt.Println(owner == "alpha" || owner == "beta")
	// Output: true
}

func ExampleEngine_Get() {
	dir, err := os.MkdirTemp("", "kvstore-eg")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	eng, err := NewEngine(dir, 8, "node-a")
	if err != nil {
		panic(err)
	}
	defer func() { _ = eng.Close() }()

	if err := eng.Put("greeting", "hello", 1); err != nil {
		panic(err)
	}
	v, err := eng.Get("greeting")
	if err != nil {
		panic(err)
	}
	fmt.Println(v)
	// Output: hello
}
```

Create `cmd/demo/main.go`. This file uses only the exported API; run it with `go run ./cmd/demo`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/kvstore"
)

func main() {
	dir, err := os.MkdirTemp("", "kvstore-demo")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	eng, err := kvstore.NewEngine(dir, 64, "node-a", "node-b", "node-c")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := eng.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()

	// Write 10 keys across all three nodes.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("item-%02d", i)
		val := fmt.Sprintf("value-%d", i)
		if err := eng.Put(key, val, int64(i+1)); err != nil {
			log.Fatalf("Put(%s): %v", key, err)
		}
	}

	// Delete one key.
	if err := eng.Delete("item-05", 100); err != nil {
		log.Fatalf("Delete: %v", err)
	}

	// Scan all remaining keys.
	entries, err := eng.Scan("", "")
	if err != nil {
		log.Fatalf("Scan: %v", err)
	}
	fmt.Printf("Scan returned %d live entries:\n", len(entries))
	for _, e := range entries {
		fmt.Printf("  %s = %s\n", e.Key, e.Value)
	}

	// Targeted get.
	v, err := eng.Get("item-03")
	if err != nil {
		log.Fatalf("Get: %v", err)
	}
	fmt.Printf("Get(item-03) = %s\n", v)
}
```

## Common Mistakes

### Putting the WAL Write Outside the Mutex

Wrong: append to the WAL before acquiring the partition lock, then acquire the lock and update the memtable. Two concurrent writers interleave their WAL appends, producing a WAL whose record order disagrees with the memtable update order. Replay reconstructs a different state than the original.

Fix: hold the partition mutex for both the WAL append and the memtable update, as `put` does above. Sequential WAL writes are fast; the latency cost is negligible compared to the disk write itself.

### Using `>=` for the LWW Timestamp Comparison

Wrong: `if e.Timestamp >= existing.Timestamp { p.data[e.Key] = e }`. During WAL replay the same entry is replayed once; using `>=` is fine. But under concurrent `Put` calls with the same timestamp from two goroutines, the last one always overwrites the first — the outcome is non-deterministic.

Fix: use strict `>` so the first write at a given timestamp wins (`upsertLocked` above). For equal timestamps the tiebreak is arrival order at the lock, which is deterministic within a single process.

### Forgetting the Ring Wrap-Around

Wrong: `return r.tokens[idx].node` where `idx` is the result of `sort.Search`. When the key's hash is larger than every token on the ring, `sort.Search` returns `len(r.tokens)`, which is an out-of-bounds index.

Fix: wrap with `idx % len(r.tokens)`. This correctly maps the hash to the first token when it exceeds all tokens, closing the ring.

### Treating `io.ErrUnexpectedEOF` as a Corruption Error

Wrong: returning an error from `replayWAL` when `decodeWALRecord` returns `io.ErrUnexpectedEOF`. A crash during a WAL write leaves a truncated partial record at the tail. That is expected and not corruption.

Fix: treat both `io.EOF` (empty WAL) and `io.ErrUnexpectedEOF` (truncated tail) as normal end-of-replay signals. Only return an error when `ErrCorruptWAL` is detected mid-stream (CRC mismatch on a fully-written record).

### Using `sync.Map` for the Memtable

Wrong: `var memtable sync.Map` with `Store`/`Load`. `sync.Map` does not support ordered iteration, so a `scan` that needs to return keys in sorted order must collect all keys into a slice and sort — at which point the `sync.Map`'s concurrent-write optimization is wasted. Worse, range over `sync.Map` under a load-heavy workload can miss recently stored keys.

Fix: use a plain `map[string]Entry` protected by a `sync.RWMutex`, as above. You control the locking scope, iteration is a plain range, and the ordering is explicit.

## Verification

From `~/go-exercises/kvstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. `go test -race` catches the data races that `TestEngineRace` is designed to trigger if the locking is wrong.

## Summary

- Consistent hashing with virtual nodes maps keys to partitions with O(log K·N) lookup and minimal key movement when the node set changes.
- Write-ahead logging ensures committed writes survive a crash: every mutation is appended to disk before the memtable is updated; replay on startup rebuilds the in-memory state.
- Last-write-wins with strict `>` timestamp comparison gives deterministic conflict resolution without coordination between writers.
- Per-partition `sync.RWMutex` lets reads on different partitions run in parallel; there is no global lock.
- Cross-partition scan is a sorted merge: collect results from each partition (each already sorted), merge into one sorted slice. Production systems stream this with `container/heap` to avoid materializing the full result set.

## What's Next

Next: [Replication and Configurable Consistency](../02-replication-consistency/02-replication-consistency.md).

## Resources

- [Go blog: Maps in action](https://go.dev/blog/maps) — map internals and iteration order
- [pkg.go.dev/sort](https://pkg.go.dev/sort) — `sort.Search` binary search contract and examples
- [pkg.go.dev/hash/crc32](https://pkg.go.dev/hash/crc32) — `ChecksumIEEE` and the IEEE polynomial used for WAL checksums
- [pkg.go.dev/container/heap](https://pkg.go.dev/container/heap) — k-way merge heap interface for production cross-partition scans
- Karger et al., "Consistent Hashing and Random Trees" (1997) — original consistent hashing paper; virtual nodes described in the Dynamo paper (DeCandia et al., 2007)
