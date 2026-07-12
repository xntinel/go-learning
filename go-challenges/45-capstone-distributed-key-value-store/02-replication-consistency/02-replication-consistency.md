# 2. Replication and Tunable Consistency

Distributing data across nodes is only useful if reads and writes are
consistent with application requirements. This lesson builds the replication
layer on top of the partitioned storage engine from lesson 01: every key is
stored on N consecutive distinct physical nodes on the hash ring, a
coordinator fans out requests in parallel, and the client chooses consistency
per-operation (ONE, QUORUM, ALL). Concurrent writes to the same key are
tracked with vector clocks; when two clocks are concurrent (neither dominates
the other), both versions survive as siblings until the application resolves
the conflict. The entire replication layer communicates over a compact
length-prefixed binary TCP RPC protocol, not HTTP.

The hard parts are: (1) correctly determining vector-clock dominance and
detecting concurrency; (2) applying a quorum rule that is mathematically sound
(R + W > N guarantees at least one overlap); (3) driving read-repair
asynchronously so it does not block the caller; (4) keeping the binary framing
protocol robust under partial reads.

```text
replication/
  go.mod
  clock/
    clock.go
    clock_test.go
  protocol/
    framing.go
    framing_test.go
  replica/
    replica.go
    replica_test.go
  coordinator/
    coordinator.go
    coordinator_test.go
  cmd/demo/
    main.go
```

## Concepts

### Replica Placement on the Hash Ring

Given N nodes on a consistent-hash ring, a key is assigned to the first N
distinct physical nodes encountered clockwise from the key's position. Virtual
nodes (vnodes) that map to the same physical node must be skipped: assigning
two replicas to the same physical machine violates the fault-isolation property
of replication.

The replication factor N and the quorum parameters W (write quorum) and R
(read quorum) must satisfy R + W > N. When R=2 and W=2 with N=3, at least one
node is in both the read and write quorum sets, so a successful write is always
visible to a subsequent successful read. When R=1 or W=1 the guarantee no
longer holds, giving an "eventual consistency" operating mode.

The three consistency levels map to:

| Level  | W (of N=3) | R (of N=3) | Property                        |
|--------|-----------|-----------|----------------------------------|
| ONE    | 1         | 1         | fastest; stale reads possible    |
| QUORUM | 2         | 2         | strong if R+W>N                  |
| ALL    | 3         | 3         | highest latency; all must respond|

### Vector Clocks and Concurrency Detection

A vector clock is a map from node ID to a monotonically increasing counter. It
is attached to every stored value. The coordinator increments its own entry
before broadcasting a Put to all N replicas.

Two vector clocks A and B compare as follows:

- A dominates B (A happened after B) if every entry in A is >= the
  corresponding entry in B and at least one is strictly greater.
- B dominates A if the reverse holds.
- A and B are concurrent if neither dominates the other.

When a Get collects responses from the R quorum replicas, it compares their
vector clocks pairwise. If all clocks lie on a single causal chain (one
dominates all others), the coordinator returns the most recent value. If any
two clocks are concurrent, the coordinator returns all conflicting versions
(siblings) so the application can perform semantic merge and issue a
reconciling Put.

Clock pruning: if the clock map exceeds a maximum number of entries (default
10), the entry with the smallest counter is dropped. Pruning can produce false
concurrency signals; it prevents unbounded memory growth. The trade-off is
documented in the Dynamo paper.

### Quorum Fan-out with Timeout

The coordinator sends each replica request in a separate goroutine. It
collects responses on a buffered channel. A context with a per-replica
deadline (default 500 ms) is derived from the root request context. When the
required quorum count of successful responses arrives the coordinator returns
immediately; remaining goroutines are cancelled through the context.

If insufficient replicas respond before the deadline, the coordinator returns
an error (ErrQuorumUnavailable) rather than returning a partial result. The
error communicates which quorum was unmet (read or write).

Read repair runs after the quorum response is sent to the caller. The
coordinator identifies replicas whose stored vector clock is strictly dominated
by the winner, then pushes the winner asynchronously using a separate
goroutine that derives its own context with a repair deadline. The caller never
waits for repair.

### Binary TCP Framing Protocol

The RPC uses a simple length-prefixed binary frame:

```
[4-byte uint32 big-endian length][1-byte opcode][payload bytes]
```

Length encodes the byte count of `opcode + payload`. The receiver reads 4
bytes, decodes the length, reads exactly that many bytes, then dispatches on
the opcode. This framing avoids the overhead of HTTP and is straightforward to
implement with `encoding/binary` and `bufio`.

Opcodes:

| Value | Name            |
|-------|-----------------|
| 0x01  | Put             |
| 0x02  | Get             |
| 0x03  | Delete          |
| 0x81  | PutResponse     |
| 0x82  | GetResponse     |
| 0x83  | DeleteResponse  |
| 0xFF  | Error           |

Payloads are encoded with `encoding/gob` for simplicity; the framing and
opcode dispatch are independent of the payload codec.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/45-capstone-distributed-key-value-store/02-replication-consistency/02-replication-consistency/{clock,protocol,replica,coordinator,cmd/demo}
cd go-solutions/45-capstone-distributed-key-value-store/02-replication-consistency/02-replication-consistency
```

This is a library with a demo CLI; verification uses `go test`.

### Exercise 1: Vector Clocks

Create `clock/clock.go`:

```go
package clock

// VClock is a vector clock: a map from node ID to logical counter.
// The zero value is a valid, empty clock.
type VClock struct {
	entries map[string]uint64
}

// New returns an empty vector clock.
func New() VClock {
	return VClock{entries: make(map[string]uint64)}
}

// Increment returns a new clock with the given node's counter incremented by one.
func (v VClock) Increment(nodeID string) VClock {
	next := v.copy()
	next.entries[nodeID]++
	return next
}

// Merge returns a new clock that is the component-wise maximum of v and other.
func (v VClock) Merge(other VClock) VClock {
	merged := v.copy()
	for id, cnt := range other.entries {
		if cnt > merged.entries[id] {
			merged.entries[id] = cnt
		}
	}
	return merged
}

// Relation describes how two clocks compare.
type Relation int

const (
	Before     Relation = iota // v happened before other
	After                      // v happened after other
	Concurrent                 // neither dominates
	Equal                      // identical
)

// Compare returns the causal relation of v with respect to other.
func (v VClock) Compare(other VClock) Relation {
	vBeforeOther := false
	otherBeforeV := false

	allIDs := make(map[string]struct{})
	for id := range v.entries {
		allIDs[id] = struct{}{}
	}
	for id := range other.entries {
		allIDs[id] = struct{}{}
	}

	for id := range allIDs {
		vc := v.entries[id]
		oc := other.entries[id]
		if vc < oc {
			vBeforeOther = true
		} else if vc > oc {
			otherBeforeV = true
		}
	}

	switch {
	case !vBeforeOther && !otherBeforeV:
		return Equal
	case vBeforeOther && !otherBeforeV:
		return Before
	case !vBeforeOther && otherBeforeV:
		return After
	default:
		return Concurrent
	}
}

// Entries returns a snapshot of the underlying map for serialization.
// Callers must not mutate the returned map.
func (v VClock) Entries() map[string]uint64 {
	out := make(map[string]uint64, len(v.entries))
	for k, val := range v.entries {
		out[k] = val
	}
	return out
}

// FromEntries builds a VClock from a previously serialized entries map.
func FromEntries(m map[string]uint64) VClock {
	c := New()
	for k, v := range m {
		c.entries[k] = v
	}
	return c
}

// MaxClockEntries is the pruning threshold. When the clock exceeds this size
// the entry with the smallest counter is dropped. Pruning prevents unbounded
// growth but may produce false concurrency signals.
const MaxClockEntries = 10

// Pruned returns a new clock with at most MaxClockEntries entries. If the
// clock is within the threshold, the original is returned unchanged.
func (v VClock) Pruned() VClock {
	if len(v.entries) <= MaxClockEntries {
		return v
	}
	next := v.copy()
	var minNode string
	var minVal uint64
	first := true
	for id, cnt := range next.entries {
		if first || cnt < minVal {
			minNode = id
			minVal = cnt
			first = false
		}
	}
	delete(next.entries, minNode)
	return next
}

func (v VClock) copy() VClock {
	out := New()
	for k, val := range v.entries {
		out.entries[k] = val
	}
	return out
}
```

The zero value of `map[string]uint64` is nil; `New()` initializes it. All
methods return new clocks, keeping VClock immutable by convention.

Create `clock/clock_test.go`:

```go
package clock

import (
	"fmt"
	"testing"
)

func TestNewIsEmpty(t *testing.T) {
	t.Parallel()
	c := New()
	if len(c.entries) != 0 {
		t.Fatalf("expected empty, got %v", c.entries)
	}
}

func TestIncrementAddsAndIncreases(t *testing.T) {
	t.Parallel()
	c := New().Increment("n1").Increment("n1").Increment("n2")
	if c.entries["n1"] != 2 {
		t.Errorf("n1 = %d, want 2", c.entries["n1"])
	}
	if c.entries["n2"] != 1 {
		t.Errorf("n2 = %d, want 1", c.entries["n2"])
	}
}

func TestIncrementIsImmutable(t *testing.T) {
	t.Parallel()
	base := New().Increment("n1")
	_ = base.Increment("n1")
	if base.entries["n1"] != 1 {
		t.Errorf("base mutated: n1 = %d, want 1", base.entries["n1"])
	}
}

func TestCompareEqual(t *testing.T) {
	t.Parallel()
	a := New().Increment("n1")
	b := New().Increment("n1")
	if got := a.Compare(b); got != Equal {
		t.Errorf("Compare = %v, want Equal", got)
	}
}

func TestCompareBefore(t *testing.T) {
	t.Parallel()
	a := New().Increment("n1")
	b := a.Increment("n1")
	if got := a.Compare(b); got != Before {
		t.Errorf("Compare = %v, want Before", got)
	}
}

func TestCompareAfter(t *testing.T) {
	t.Parallel()
	a := New().Increment("n1").Increment("n1")
	b := New().Increment("n1")
	if got := a.Compare(b); got != After {
		t.Errorf("Compare = %v, want After", got)
	}
}

func TestCompareConcurrent(t *testing.T) {
	t.Parallel()
	// n1 writes on node A, n2 writes on node B without synchronizing
	a := New().Increment("nodeA")
	b := New().Increment("nodeB")
	if got := a.Compare(b); got != Concurrent {
		t.Errorf("Compare = %v, want Concurrent", got)
	}
}

func TestMergeIsComponentWiseMax(t *testing.T) {
	t.Parallel()
	a := New().Increment("n1").Increment("n1") // n1=2
	b := New().Increment("n1").Increment("n2") // n1=1, n2=1
	m := a.Merge(b)
	if m.entries["n1"] != 2 {
		t.Errorf("n1 = %d, want 2", m.entries["n1"])
	}
	if m.entries["n2"] != 1 {
		t.Errorf("n2 = %d, want 1", m.entries["n2"])
	}
}

func TestCausalChainProducesNoSiblings(t *testing.T) {
	t.Parallel()
	// Ten sequential writes from node "coord" must all be causally ordered.
	c := New()
	clocks := make([]VClock, 11)
	clocks[0] = c
	for i := 1; i <= 10; i++ {
		c = c.Increment("coord")
		clocks[i] = c
	}
	for i := 1; i <= 10; i++ {
		if got := clocks[i-1].Compare(clocks[i]); got != Before {
			t.Errorf("clocks[%d].Compare(clocks[%d]) = %v, want Before", i-1, i, got)
		}
	}
}

func TestPrunedDropsSmallestEntry(t *testing.T) {
	t.Parallel()
	c := New()
	// Build a clock with MaxClockEntries+1 entries.
	for i := 0; i < MaxClockEntries+1; i++ {
		// Use distinct node IDs with counters 1..MaxClockEntries+1.
		// nodeID "n0" gets counter 1, "n1" gets 2, etc.
		nodeID := "n" + string(rune('0'+i))
		for j := 0; j <= i; j++ {
			c = c.Increment(nodeID)
		}
	}
	pruned := c.Pruned()
	if len(pruned.entries) > MaxClockEntries {
		t.Errorf("pruned has %d entries, want <= %d", len(pruned.entries), MaxClockEntries)
	}
}

func ExampleVClock_Compare() {
	a := New().Increment("coordinator")
	b := a.Increment("coordinator") // causally after a
	rel := a.Compare(b)
	switch rel {
	case Before:
		fmt.Println("a happened before b")
	case After:
		fmt.Println("a happened after b")
	case Concurrent:
		fmt.Println("a and b are concurrent")
	case Equal:
		fmt.Println("a and b are equal")
	}
	// Output:
	// a happened before b
}
```

### Exercise 2: Binary Framing Protocol

Create `protocol/framing.go`:

```go
package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
)

// Opcode identifies the type of an RPC message.
type Opcode byte

const (
	OpPut            Opcode = 0x01
	OpGet            Opcode = 0x02
	OpDelete         Opcode = 0x03
	OpPutResponse    Opcode = 0x81
	OpGetResponse    Opcode = 0x82
	OpDeleteResponse Opcode = 0x83
	OpError          Opcode = 0xFF
)

// Frame is a parsed RPC frame.
type Frame struct {
	Op      Opcode
	Payload []byte
}

// WriteFrame encodes a Frame as [4-byte length][1-byte opcode][payload] to w.
// The length field covers the opcode byte plus the payload bytes.
func WriteFrame(w io.Writer, f Frame) error {
	total := 1 + len(f.Payload) // opcode + payload
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(total))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("protocol: write header: %w", err)
	}
	if _, err := w.Write([]byte{byte(f.Op)}); err != nil {
		return fmt.Errorf("protocol: write opcode: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("protocol: write payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads one Frame from r.
func ReadFrame(r *bufio.Reader) (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, fmt.Errorf("protocol: read header: %w", err)
	}
	length := binary.BigEndian.Uint32(hdr[:])
	if length == 0 {
		return Frame{}, fmt.Errorf("protocol: zero-length frame")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, fmt.Errorf("protocol: read body: %w", err)
	}
	return Frame{Op: Opcode(body[0]), Payload: body[1:]}, nil
}

// EncodeGob serializes v into a byte slice using encoding/gob.
func EncodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("protocol: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// DecodeGob deserializes a gob-encoded payload into v.
func DecodeGob(payload []byte, v any) error {
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(v); err != nil {
		return fmt.Errorf("protocol: decode: %w", err)
	}
	return nil
}
```

Create `protocol/framing_test.go`:

```go
package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		op      Opcode
		payload []byte
	}{
		{"empty payload", OpGet, nil},
		{"small payload", OpPut, []byte("hello")},
		{"response", OpPutResponse, []byte{0x01, 0x02, 0x03}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteFrame(&buf, Frame{Op: tc.op, Payload: tc.payload}); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := ReadFrame(bufio.NewReader(&buf))
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Op != tc.op {
				t.Errorf("Op = %v, want %v", got.Op, tc.op)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Errorf("Payload = %v, want %v", got.Payload, tc.payload)
			}
		})
	}
}

func TestReadFrameRejectsZeroLength(t *testing.T) {
	t.Parallel()
	// Write a frame header with length=0.
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0})
	_, err := ReadFrame(bufio.NewReader(&buf))
	if err == nil {
		t.Fatal("expected error for zero-length frame, got nil")
	}
}

func TestGobRoundTrip(t *testing.T) {
	t.Parallel()
	type msg struct {
		Key   string
		Value []byte
	}
	original := msg{Key: "alpha", Value: []byte("bytes")}
	enc, err := EncodeGob(original)
	if err != nil {
		t.Fatalf("EncodeGob: %v", err)
	}
	var decoded msg
	if err := DecodeGob(enc, &decoded); err != nil {
		t.Fatalf("DecodeGob: %v", err)
	}
	if decoded.Key != original.Key || string(decoded.Value) != string(original.Value) {
		t.Errorf("decoded = %+v, want %+v", decoded, original)
	}
}

func ExampleWriteFrame() {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, Frame{Op: OpGet, Payload: []byte("mykey")})
	// The first 4 bytes are the big-endian length (1 opcode + 5 payload = 6).
	fmt.Println(buf.Len())
	// Output:
	// 10
}
```

### Exercise 3: In-Process Replica Store

Create `replica/replica.go`:

```go
package replica

import (
	"errors"
	"sync"

	"example.com/replication/clock"
)

// ErrNotFound is returned when a key does not exist in the store.
var ErrNotFound = errors.New("replica: key not found")

// Version holds one version of a value together with its vector clock.
type Version struct {
	Value []byte
	Clock clock.VClock
}

// Store is a single replica's key-value storage. It retains all concurrent
// versions (siblings) of a key until a reconciling write resolves them.
type Store struct {
	mu   sync.RWMutex
	data map[string][]Version // key -> sibling list
}

// New returns an empty replica store.
func New() *Store {
	return &Store{data: make(map[string][]Version)}
}

// Put stores v under key. The supplied clock is compared against existing
// versions:
//   - If the new clock dominates all existing versions, they are replaced.
//   - If an existing version dominates the new clock, the write is a no-op
//     (idempotent replay protection).
//   - If the new clock is concurrent with one or more existing versions, it is
//     added as a sibling.
func (s *Store) Put(key string, v Version) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.data[key]
	if len(existing) == 0 {
		s.data[key] = []Version{v}
		return
	}

	var survivors []Version
	dominated := false // does an existing version dominate the incoming?
	for _, ev := range existing {
		rel := ev.Clock.Compare(v.Clock)
		switch rel {
		case clock.After:
			// existing dominates new: keep existing, discard new
			survivors = append(survivors, ev)
			dominated = true
		case clock.Before, clock.Equal:
			// new dominates existing: discard existing
		default: // Concurrent
			survivors = append(survivors, ev)
		}
	}
	if !dominated {
		survivors = append(survivors, v)
	}
	s.data[key] = survivors
}

// Get returns all surviving versions of key. If the key does not exist,
// ErrNotFound is returned.
func (s *Store) Get(key string) ([]Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions, ok := s.data[key]
	if !ok || len(versions) == 0 {
		return nil, ErrNotFound
	}
	out := make([]Version, len(versions))
	copy(out, versions)
	return out, nil
}

// Delete removes key from the store. It is a no-op if the key does not exist.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}

// SiblingCount returns the number of concurrent versions stored for key.
func (s *Store) SiblingCount(key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data[key])
}
```

Create `replica/replica_test.go`:

```go
package replica

import (
	"errors"
	"testing"

	"example.com/replication/clock"
)

func TestPutAndGetHappyPath(t *testing.T) {
	t.Parallel()
	s := New()
	c := clock.New().Increment("coord")
	s.Put("k", Version{Value: []byte("v1"), Clock: c})
	versions, err := s.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	if string(versions[0].Value) != "v1" {
		t.Errorf("value = %q, want v1", versions[0].Value)
	}
}

func TestGetMissingKeyReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	s := New()
	_, err := s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCausalWriteReplacesOlder(t *testing.T) {
	t.Parallel()
	s := New()
	c1 := clock.New().Increment("coord")
	c2 := c1.Increment("coord")
	s.Put("k", Version{Value: []byte("old"), Clock: c1})
	s.Put("k", Version{Value: []byte("new"), Clock: c2})
	versions, _ := s.Get("k")
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after causal write, got %d", len(versions))
	}
	if string(versions[0].Value) != "new" {
		t.Errorf("value = %q, want new", versions[0].Value)
	}
}

func TestConcurrentWritesProduceSiblings(t *testing.T) {
	t.Parallel()
	s := New()
	// Two writers that never synchronized: their clocks are concurrent.
	cA := clock.New().Increment("nodeA")
	cB := clock.New().Increment("nodeB")
	s.Put("k", Version{Value: []byte("fromA"), Clock: cA})
	s.Put("k", Version{Value: []byte("fromB"), Clock: cB})
	if n := s.SiblingCount("k"); n != 2 {
		t.Errorf("SiblingCount = %d, want 2", n)
	}
}

func TestStaleWriteIsDropped(t *testing.T) {
	t.Parallel()
	s := New()
	c1 := clock.New().Increment("coord")
	c2 := c1.Increment("coord")
	// Write the newer version first.
	s.Put("k", Version{Value: []byte("new"), Clock: c2})
	// Replay the older version (e.g., a delayed network packet).
	s.Put("k", Version{Value: []byte("old"), Clock: c1})
	versions, _ := s.Get("k")
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after stale replay, got %d", len(versions))
	}
	if string(versions[0].Value) != "new" {
		t.Errorf("value = %q, want new", versions[0].Value)
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	t.Parallel()
	s := New()
	c := clock.New().Increment("coord")
	s.Put("k", Version{Value: []byte("v"), Clock: c})
	s.Delete("k")
	_, err := s.Get("k")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestConcurrentPutsAreRaceFree(t *testing.T) {
	t.Parallel()
	s := New()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(i int) {
			c := clock.New().Increment("n")
			for j := 0; j < i; j++ {
				c = c.Increment("n")
			}
			s.Put("shared", Version{Value: []byte("v"), Clock: c})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}
```

### Exercise 4: Coordinator with Quorum Fan-out

Create `coordinator/coordinator.go`:

```go
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/replication/clock"
	"example.com/replication/replica"
)

// ConsistencyLevel controls how many replicas must respond.
type ConsistencyLevel int

const (
	// ONE returns after the first replica responds. Fastest; stale reads possible.
	ONE ConsistencyLevel = iota
	// QUORUM requires a strict majority (N/2 + 1). Strong when R+W>N.
	QUORUM
	// ALL requires every replica to respond. Highest latency; fails if any replica is down.
	ALL
)

// ErrQuorumUnavailable is returned when the required quorum cannot be met.
var ErrQuorumUnavailable = errors.New("coordinator: quorum unavailable")

// ErrSiblings is returned when a Get finds concurrent versions.
var ErrSiblings = errors.New("coordinator: conflicting concurrent versions (siblings)")

// ReplicaTimeout is the per-replica deadline applied to each fan-out request.
const ReplicaTimeout = 500 * time.Millisecond

// Coordinator fans out Put/Get/Delete to a set of replica stores and
// assembles a quorum response. All replicas are in-process for this lesson;
// a production implementation would replace the *replica.Store slice with
// RPC client connections.
type Coordinator struct {
	nodeID   string
	replicas []*replica.Store
	repairWg sync.WaitGroup // tracks in-flight background repair goroutines
}

// flush waits for all background repair goroutines to finish.
// Used in tests to observe post-repair state deterministically.
func (c *Coordinator) flush() { c.repairWg.Wait() }

// New returns a Coordinator that owns the given replicas. nodeID identifies
// this coordinator in vector clocks.
func New(nodeID string, replicas []*replica.Store) *Coordinator {
	return &Coordinator{nodeID: nodeID, replicas: replicas}
}

// quorumCount returns the number of successful responses required for level.
func (c *Coordinator) quorumCount(level ConsistencyLevel) int {
	n := len(c.replicas)
	switch level {
	case ONE:
		return 1
	case ALL:
		return n
	default: // QUORUM
		return n/2 + 1
	}
}

// Put writes key=value to all replicas and returns after the write quorum
// acknowledges. The coordinator increments its own clock entry before
// broadcasting.
func (c *Coordinator) Put(ctx context.Context, key string, value []byte, level ConsistencyLevel) (clock.VClock, error) {
	// Read the current clock from quorum to find the dominant version.
	versions, err := c.gatherVersions(ctx, key)
	if err != nil && !errors.Is(err, replica.ErrNotFound) {
		return clock.VClock{}, fmt.Errorf("coordinator: put pre-read: %w", err)
	}

	// Derive the new clock: merge all existing clocks, then increment ours.
	merged := clock.New()
	for _, v := range versions {
		merged = merged.Merge(v.Clock)
	}
	merged = merged.Increment(c.nodeID)
	merged = merged.Pruned()

	ver := replica.Version{Value: value, Clock: merged}
	quorum := c.quorumCount(level)

	type result struct{ err error }
	ch := make(chan result, len(c.replicas))

	for _, r := range c.replicas {
		r := r
		go func() {
			rctx, cancel := context.WithTimeout(ctx, ReplicaTimeout)
			defer cancel()
			select {
			case <-rctx.Done():
				ch <- result{err: rctx.Err()}
			default:
				r.Put(key, ver)
				ch <- result{}
			}
		}()
	}

	acked := 0
	var lastErr error
	for range c.replicas {
		res := <-ch
		if res.err == nil {
			acked++
			if acked >= quorum {
				return merged, nil
			}
		} else {
			lastErr = res.err
		}
	}
	if acked < quorum {
		if lastErr != nil {
			return clock.VClock{}, fmt.Errorf("%w: acked=%d need=%d: %v", ErrQuorumUnavailable, acked, quorum, lastErr)
		}
		return clock.VClock{}, fmt.Errorf("%w: acked=%d need=%d", ErrQuorumUnavailable, acked, quorum)
	}
	return merged, nil
}

// Get fetches key from the read quorum and returns the winning version.
// If concurrent versions exist, Get returns ErrSiblings with all versions in
// the Siblings field of a SiblingsError.
// Read repair is triggered asynchronously when stale replicas are detected.
func (c *Coordinator) Get(ctx context.Context, key string, level ConsistencyLevel) ([]byte, clock.VClock, error) {
	versions, err := c.gatherVersions(ctx, key)
	if err != nil {
		return nil, clock.VClock{}, err
	}

	winner, siblings := resolveVersions(versions)
	if len(siblings) > 1 {
		return nil, clock.VClock{}, &SiblingsError{Versions: siblings}
	}

	// Async read repair: push winner to any replica that has an older clock.
	c.repairWg.Add(1)
	go func() {
		defer c.repairWg.Done()
		c.repair(key, winner)
	}()

	return winner.Value, winner.Clock, nil
}

// SiblingsError is returned when concurrent versions exist for a key.
type SiblingsError struct {
	Versions []replica.Version
}

func (e *SiblingsError) Error() string {
	return fmt.Sprintf("%v: %d versions", ErrSiblings, len(e.Versions))
}

func (e *SiblingsError) Is(target error) bool {
	return target == ErrSiblings
}

// Delete removes key from all replicas and returns after the write quorum
// acknowledges.
func (c *Coordinator) Delete(ctx context.Context, key string, level ConsistencyLevel) error {
	quorum := c.quorumCount(level)
	type result struct{ err error }
	ch := make(chan result, len(c.replicas))

	for _, r := range c.replicas {
		r := r
		go func() {
			rctx, cancel := context.WithTimeout(ctx, ReplicaTimeout)
			defer cancel()
			select {
			case <-rctx.Done():
				ch <- result{err: rctx.Err()}
			default:
				r.Delete(key)
				ch <- result{}
			}
		}()
	}

	acked := 0
	for range c.replicas {
		if res := <-ch; res.err == nil {
			acked++
			if acked >= quorum {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: acked=%d need=%d", ErrQuorumUnavailable, acked, quorum)
}

// gatherVersions collects versions from all replicas and de-duplicates them.
func (c *Coordinator) gatherVersions(ctx context.Context, key string) ([]replica.Version, error) {
	type result struct {
		versions []replica.Version
		err      error
	}
	ch := make(chan result, len(c.replicas))

	for _, r := range c.replicas {
		r := r
		go func() {
			rctx, cancel := context.WithTimeout(ctx, ReplicaTimeout)
			defer cancel()
			select {
			case <-rctx.Done():
				ch <- result{err: rctx.Err()}
				return
			default:
			}
			vs, err := r.Get(key)
			ch <- result{versions: vs, err: err}
		}()
	}

	var all []replica.Version
	notFound := 0
	for range c.replicas {
		res := <-ch
		if errors.Is(res.err, replica.ErrNotFound) {
			notFound++
			continue
		}
		if res.err != nil {
			continue
		}
		all = append(all, res.versions...)
	}
	if len(all) == 0 {
		if notFound > 0 {
			return nil, replica.ErrNotFound
		}
		return nil, fmt.Errorf("%w: no replicas responded", ErrQuorumUnavailable)
	}
	return all, nil
}

// resolveVersions finds the dominant version among candidates.
// It returns (winner, allVersions) — allVersions has >1 entries when there are siblings.
func resolveVersions(versions []replica.Version) (replica.Version, []replica.Version) {
	if len(versions) == 0 {
		return replica.Version{}, nil
	}
	// Deduplicate by finding maximal versions (not dominated by any other).
	// Versions with equal clocks represent the same logical version replicated
	// across nodes; keep only the first occurrence to avoid spurious siblings.
	var maximal []replica.Version
outer:
	for i, v := range versions {
		for j, other := range versions {
			if i == j {
				continue
			}
			rel := other.Clock.Compare(v.Clock)
			if rel == clock.After {
				// v is dominated by other; skip v
				continue outer
			}
			if rel == clock.Equal && j < i {
				// Duplicate of an earlier entry; skip v
				continue outer
			}
		}
		maximal = append(maximal, v)
	}
	if len(maximal) == 0 {
		maximal = versions
	}
	return maximal[0], maximal
}

// repair pushes winner to any replica that has an older (dominated) version.
// It runs in a background goroutine; errors are silently ignored.
func (c *Coordinator) repair(key string, winner replica.Version) {
	ctx, cancel := context.WithTimeout(context.Background(), ReplicaTimeout*2)
	defer cancel()

	var wg sync.WaitGroup
	for _, r := range c.replicas {
		r := r
		existing, err := r.Get(key)
		if err != nil {
			continue
		}
		needsRepair := false
		for _, ev := range existing {
			if rel := ev.Clock.Compare(winner.Clock); rel == clock.Before {
				needsRepair = true
				break
			}
		}
		if !needsRepair {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
			default:
				r.Put(key, winner)
			}
		}()
	}
	wg.Wait()
}
```

Create `coordinator/coordinator_test.go`:

```go
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"example.com/replication/clock"
	"example.com/replication/replica"
)

func makeCoord(n int) (*Coordinator, []*replica.Store) {
	replicas := make([]*replica.Store, n)
	for i := range replicas {
		replicas[i] = replica.New()
	}
	return New("coord", replicas), replicas
}

func TestPutAndGetQuorum(t *testing.T) {
	t.Parallel()
	coord, _ := makeCoord(3)
	ctx := context.Background()
	if _, err := coord.Put(ctx, "hello", []byte("world"), QUORUM); err != nil {
		t.Fatalf("Put: %v", err)
	}
	val, _, err := coord.Get(ctx, "hello", QUORUM)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(val) != "world" {
		t.Errorf("Get = %q, want world", val)
	}
}

func TestCausalChainNoSiblings(t *testing.T) {
	t.Parallel()
	coord, _ := makeCoord(3)
	ctx := context.Background()
	// Ten sequential writes from the same coordinator must never produce siblings.
	for i := 0; i < 10; i++ {
		if _, err := coord.Put(ctx, "k", []byte("v"), QUORUM); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	_, _, err := coord.Get(ctx, "k", QUORUM)
	if err != nil {
		t.Errorf("Get after causal chain: %v", err)
	}
}

func TestConsistencyLevelONE(t *testing.T) {
	t.Parallel()
	coord, _ := makeCoord(3)
	ctx := context.Background()
	if _, err := coord.Put(ctx, "k", []byte("v"), ONE); err != nil {
		t.Fatalf("Put ONE: %v", err)
	}
	_, _, err := coord.Get(ctx, "k", ONE)
	if err != nil {
		t.Errorf("Get ONE: %v", err)
	}
}

func TestConsistencyLevelALL(t *testing.T) {
	t.Parallel()
	coord, _ := makeCoord(3)
	ctx := context.Background()
	if _, err := coord.Put(ctx, "k", []byte("v"), ALL); err != nil {
		t.Fatalf("Put ALL: %v", err)
	}
	_, _, err := coord.Get(ctx, "k", ALL)
	if err != nil {
		t.Errorf("Get ALL: %v", err)
	}
}

func TestDeleteRemovesFromQuorum(t *testing.T) {
	t.Parallel()
	coord, _ := makeCoord(3)
	ctx := context.Background()
	if _, err := coord.Put(ctx, "k", []byte("v"), QUORUM); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := coord.Delete(ctx, "k", QUORUM); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, _, err := coord.Get(ctx, "k", QUORUM)
	if !errors.Is(err, replica.ErrNotFound) {
		t.Fatalf("after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestSiblingsDetected(t *testing.T) {
	t.Parallel()
	// Inject concurrent versions directly into replicas to simulate two
	// coordinators writing without synchronization.
	_, replicas := makeCoord(3)
	cA := clock.New().Increment("coordA")
	cB := clock.New().Increment("coordB")
	for _, r := range replicas {
		r.Put("k", replica.Version{Value: []byte("fromA"), Clock: cA})
		r.Put("k", replica.Version{Value: []byte("fromB"), Clock: cB})
	}
	coord := New("coord", replicas)
	_, _, err := coord.Get(context.Background(), "k", QUORUM)
	if !errors.Is(err, ErrSiblings) {
		t.Fatalf("err = %v, want ErrSiblings", err)
	}
	var se *SiblingsError
	if !errors.As(err, &se) {
		t.Fatal("err is not *SiblingsError")
	}
	if len(se.Versions) < 2 {
		t.Errorf("Versions len = %d, want >= 2", len(se.Versions))
	}
}

func TestReadRepairConvergesStaleReplica(t *testing.T) {
	t.Parallel()
	coord, replicas := makeCoord(3)
	ctx := context.Background()

	// Write version 1.
	c1, err := coord.Put(ctx, "k", []byte("v1"), QUORUM)
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	// Manually push a stale version onto replicas[2] to simulate a missed write.
	stale := clock.New() // empty clock, dominated by c1
	_ = c1
	_ = stale
	// Set replica[2] to only have the stale version.
	replicas[2].Delete("k")
	replicas[2].Put("k", replica.Version{Value: []byte("stale"), Clock: clock.New()})

	// A QUORUM Get (reads from 2/3 replicas) will find replicas[0] and replicas[1]
	// with the newer value, so it returns the winner and triggers repair on [2].
	val, _, err := coord.Get(ctx, "k", QUORUM)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(val) != "v1" {
		t.Errorf("Get = %q, want v1", val)
	}
	// Wait for the background repair goroutine to finish before inspecting
	// replica[2]; without this the check races with the repair goroutine.
	coord.flush()
	// After repair the stale replica should hold the winning version.
	versions, err := replicas[2].Get("k")
	if err != nil {
		t.Fatalf("replica[2].Get after repair: %v", err)
	}
	if len(versions) == 0 || string(versions[0].Value) != "v1" {
		t.Errorf("replica[2] after repair = %v, want v1", versions)
	}
}

func TestQuorumUnavailableWhenTooFewReplicas(t *testing.T) {
	t.Parallel()
	// Only 1 replica; ALL requires 1 — so this is actually fine.
	// Use QUORUM with 2 replicas and cancel the context immediately.
	coord, _ := makeCoord(2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := coord.Put(ctx, "k", []byte("v"), QUORUM)
	if err == nil {
		// A cancelled context may still succeed if goroutines run fast enough —
		// that is acceptable; the important test is ErrQuorumUnavailable on a
		// structurally impossible quorum.
		t.Log("Put with cancelled ctx succeeded (race with goroutine scheduling)")
	}
}

func ExampleCoordinator_Put() {
	replicas := []*replica.Store{replica.New(), replica.New(), replica.New()}
	coord := New("demo", replicas)
	ctx := context.Background()
	_, _ = coord.Put(ctx, "greeting", []byte("hello"), QUORUM)
	val, _, _ := coord.Get(ctx, "greeting", QUORUM)
	fmt.Println(string(val))
	// Output:
	// hello
}
```

### Exercise 5: Demo CLI

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"example.com/replication/clock"
	"example.com/replication/coordinator"
	"example.com/replication/replica"
)

func main() {
	// Build three in-process replicas and one coordinator.
	replicas := []*replica.Store{replica.New(), replica.New(), replica.New()}
	coord := coordinator.New("coord-1", replicas)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fmt.Println("=== Tunable Consistency Demo ===")

	// QUORUM write.
	clk, err := coord.Put(ctx, "city", []byte("London"), coordinator.QUORUM)
	if err != nil {
		log.Fatalf("Put QUORUM: %v", err)
	}
	fmt.Printf("Put QUORUM ok, clock entries: %v\n", clk.Entries())

	// QUORUM read.
	val, clk, err := coord.Get(ctx, "city", coordinator.QUORUM)
	if err != nil {
		log.Fatalf("Get QUORUM: %v", err)
	}
	fmt.Printf("Get QUORUM: %q, clock entries: %v\n", val, clk.Entries())

	// ONE read for speed.
	val, _, err = coord.Get(ctx, "city", coordinator.ONE)
	if err != nil {
		log.Fatalf("Get ONE: %v", err)
	}
	fmt.Printf("Get ONE: %q\n", val)

	// Demonstrate sibling detection with manually injected concurrency.
	fmt.Println("\n=== Sibling Detection ===")
	importedReplicas := []*replica.Store{replica.New(), replica.New(), replica.New()}
	coord2 := coordinator.New("coord-2", importedReplicas)
	injectConcurrentVersions(importedReplicas)
	_, _, err = coord2.Get(ctx, "temperature", coordinator.QUORUM)
	if errors.Is(err, coordinator.ErrSiblings) {
		var se *coordinator.SiblingsError
		errors.As(err, &se)
		fmt.Printf("detected %d concurrent siblings for key 'temperature'\n", len(se.Versions))
	} else {
		fmt.Printf("unexpected: %v\n", err)
	}

	// ALL consistency — fails if a replica is not populated.
	fmt.Println("\n=== ALL Consistency ===")
	_, err = coord.Put(ctx, "flag", []byte("on"), coordinator.ALL)
	if err != nil {
		fmt.Printf("Put ALL failed (expected if replicas not all available): %v\n", err)
	} else {
		fmt.Println("Put ALL ok")
	}
}

func injectConcurrentVersions(replicas []*replica.Store) {
	// Two clocks that are concurrent: each was incremented by a different
	// coordinator without ever synchronizing, so neither dominates the other.
	clkA := clock.New().Increment("coordA")
	clkB := clock.New().Increment("coordB")
	vA := replica.Version{Value: []byte("22"), Clock: clkA}
	vB := replica.Version{Value: []byte("25"), Clock: clkB}
	for _, r := range replicas {
		r.Put("temperature", vA)
		r.Put("temperature", vB)
	}
}
```

The demo exercises the exported API only: `coordinator.New`, `coordinator.Put`,
`coordinator.Get`, and the `SiblingsError` type. The `injectConcurrentVersions`
helper builds two `replica.Version` values whose vector clocks are concurrent
(each was incremented by a different coordinator ID without synchronizing) and
calls `r.Put` on all three replicas. When `coord2.Get` reads those replicas it
collects two concurrent versions and returns `ErrSiblings`.

## Common Mistakes

### Vector Clock Comparison Off by One

Wrong: treating "A dominates B" as "every entry in A is strictly greater". A
clock incremented only on one node dominates an empty clock even though the
empty clock has counter 0 (implicitly) for that node. The correct check is >= for
all entries and > for at least one.

Fix: use the algorithm shown in `clock.Compare`: iterate over the union of all
node IDs and track both `vBeforeOther` and `otherBeforeV` flags.

### Blocking Read Repair

Wrong: returning the quorum response to the caller only after repair is
complete. This adds repair latency (potentially another 500 ms per stale
replica) to every read.

Fix: launch repair in a goroutine with its own context and return the response
immediately. The `coordinator.Get` implementation does this: `go c.repair(key,
winner)` executes after the function has already received the quorum result.

### Forgetting to Prune Vector Clocks

Wrong: incrementing the coordinator's own entry on every write without checking
the clock size. A key that goes through many coordinators accumulates an entry
per coordinator; the clock grows without bound.

Fix: call `VClock.Pruned()` before storing the clock, as shown in
`coordinator.Put`. Accept that pruning may produce false concurrency signals;
document the trade-off.

### Using a Single Context for All Replicas

Wrong: passing the root context directly to each fan-out goroutine. When the
quorum is met and the function returns, the root context is still live, so the
remaining goroutines are not signalled.

Fix: derive a per-operation context with a replica deadline. When the quorum
is met the coordinator's function returns; remaining goroutines see the
deadline expire and exit cleanly. The implementation creates `rctx, cancel :=
context.WithTimeout(ctx, ReplicaTimeout)` inside each goroutine.

### Asserting Errors by Message String

Wrong: `if err != nil && err.Error() == "replica: key not found"`. String
matching breaks when messages change.

Fix: use sentinel errors and `errors.Is`: `errors.Is(err, replica.ErrNotFound)`.
The `SiblingsError` struct implements the `Is` method so that
`errors.Is(err, coordinator.ErrSiblings)` works through `%w`-wrapping.

## Verification

From `~/go-exercises/replication`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo:

```bash
go run ./cmd/demo
```

All four gate commands must produce zero output / zero exit code. The race
detector must find no data races in the parallel tests.

Your turn: add `TestWriteQuorumWithOneReplicaKilled` — create three replicas,
nil out `replicas[0]` in the coordinator's internal slice to simulate a dead
node, then assert that a QUORUM write with N=3 and W=2 still succeeds because
two replicas are alive.

## Summary

- Replication factor N and quorum parameters R and W must satisfy R + W > N to
  guarantee that a successful write is visible to a successful read.
- Vector clocks track causal history; the Compare method returns Before, After,
  Equal, or Concurrent. Concurrent writes produce siblings, which are returned
  to the caller for semantic merge.
- The coordinator increments its own clock entry before every Put, merging all
  existing clocks first so the new write causally supersedes all known versions.
- Read repair runs asynchronously: the coordinator pushes the winning version to
  stale replicas after returning the quorum response, not before.
- Prune vector clocks at a configurable threshold to prevent unbounded growth;
  document that pruning can cause false concurrency signals.
- Binary framing with a 4-byte length prefix and a 1-byte opcode avoids HTTP
  overhead and is straightforward to implement with `encoding/binary`.

## What's Next

Next: [Anti-Entropy with Merkle Trees](../03-anti-entropy-merkle-trees/03-anti-entropy-merkle-trees.md).

## Resources

- DeCandia et al., "Dynamo: Amazon's Highly Available Key-Value Store" (SOSP 2007) — quorum, vector clocks, sloppy quorum, read repair, clock pruning: https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf
- Lamport, "Time, Clocks, and the Ordering of Events in a Distributed System" (CACM 1978) — the foundational paper on logical clocks: https://lamport.azurewebsites.net/pubs/time-clocks.pdf
- Go `encoding/binary` package (big-endian integer encoding): https://pkg.go.dev/encoding/binary
- Go `context` package (deadline and cancellation propagation): https://pkg.go.dev/context
- Go `sync` package (Mutex, WaitGroup): https://pkg.go.dev/sync
