# 24. Consistent Prefix Reads

Consistent prefix reads sit between eventual consistency and strong consistency: a reader always sees writes in the order they were applied — if write B causally depends on write A, no reader ever sees B without first seeing A. The hard part is not the concept but the enforcement: you need per-operation version vectors, per-session state, and a replica-selection policy that routes reads only to replicas that are caught up enough. This lesson builds the full mechanism from scratch in Go.

```text
cpread/
  go.mod
  cpread.go
  cpread_test.go
  cmd/demo/main.go
```

## Concepts

### Why Eventual Consistency Breaks Reads

In an eventually-consistent system every write is replicated asynchronously. Replica A may apply `w1` then `w2`; replica B may apply `w2` before `w1` if the messages arrive out of order. A client that reads from replica B first sees a reply without the question — the effect before the cause.

Consistent prefix reads eliminate that class of anomaly: the system guarantees that if a reader observes write `wN`, they have already observed every write in the prefix `w1 … wN-1`. The replicas still diverge in how far ahead they are, but they never apply writes out of causal order.

### Version Vectors as Causal Timestamps

A version vector (also called a vector clock in this context) is a map from replica ID to the highest sequence number that replica has processed. Each write carries the vector of the session that produced it, capturing "what state did the writer see before writing?". A replica that has processed at least that vector can safely serve that write's value without creating a causal anomaly.

Formally, vector `V1` dominates `V2` (written `V2 <= V1`) when every component of `V1` is greater than or equal to the corresponding component of `V2`. Dominance is the core check for every session guarantee in this lesson.

### The Four Session Guarantees (Terry et al. 1994)

Terry, Theimer, Petersen, Demers, Spreitzer and Hauser defined four session guarantees for weakly consistent replicated data:

1. Read Your Writes: after a client writes, all subsequent reads in the same session return that write or a later version.
2. Monotonic Reads: if a session reads a value at version V, all subsequent reads return a version >= V; time never goes backward.
3. Monotonic Writes: a session's writes are applied in order across all replicas; write `w2` from a session is never applied without `w1` having been applied first.
4. Writes Follow Reads: if a session reads at version V and then writes, that write's causal timestamp includes V, so any replica that applies the write has already applied the reads that preceded it.

### Replica Selection

The practical mechanism for enforcing all four guarantees is the same: each replica publishes its current version vector. When a session makes a read, the coordinator selects only replicas whose version vector dominates the session's current read vector. If no replica qualifies, the coordinator either blocks or falls back to the write-ahead log.

This is the approach used in production causal-consistency systems such as COPS and Eiger. The session vector is the key invariant: it is updated on every read and write to track the frontier of what the session has observed.

### Consistent Prefix vs. Causal Consistency

Consistent prefix is weaker than full causal consistency across sessions. Consistent prefix enforces order within a single causal chain: if `A` caused `B`, readers see `A` before `B`. Causal consistency extends this across sessions: even if a different session wrote `B` after reading `A`, that dependency is tracked and enforced. The lesson implements per-session enforcement; cross-session causality requires propagating dependency vectors from read to write in the write payload.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/24-consistent-prefix-reads/24-consistent-prefix-reads/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/24-consistent-prefix-reads/24-consistent-prefix-reads
```

This is a library with a demo and tests. There is no ad-hoc main — verification is `go test`.

### Exercise 1: Version Vector and the Replica

Create `cpread.go`:

```go
package cpread

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNoReplica is returned when no replica has caught up to the required
// version vector for a session read.
var ErrNoReplica = errors.New("cpread: no replica caught up to session vector")

// VersionVector is a causal timestamp: replicaID -> highest sequence number
// the holder has processed from that replica.
type VersionVector map[string]uint64

// Dominates reports whether v dominates other, meaning v is at least as
// advanced as other on every component. A replica whose vector dominates the
// session vector is safe to read from.
func (v VersionVector) Dominates(other VersionVector) bool {
	for id, seq := range other {
		if v[id] < seq {
			return false
		}
	}
	return true
}

// Merge advances v to the component-wise maximum of v and other. This is used
// to update the session vector after a read or write.
func (v VersionVector) Merge(other VersionVector) {
	for id, seq := range other {
		if seq > v[id] {
			v[id] = seq
		}
	}
}

// Clone returns an independent copy of v.
func (v VersionVector) Clone() VersionVector {
	c := make(VersionVector, len(v))
	for id, seq := range v {
		c[id] = seq
	}
	return c
}

// Entry is a single written value with its causal timestamp.
type Entry struct {
	Key     string
	Value   string
	Written VersionVector // the session vector at the moment of the write
}

// Replica holds an ordered log of applied entries. Entries are appended in
// causal order: an entry is applied only after every entry it causally depends
// on has already been applied.
type Replica struct {
	mu      sync.RWMutex
	id      string
	applied []Entry
	version VersionVector
}

// NewReplica returns a replica with the given id.
func NewReplica(id string) *Replica {
	return &Replica{
		id:      id,
		version: make(VersionVector),
	}
}

// ID returns the replica's identifier.
func (r *Replica) ID() string { return r.id }

// Apply adds an entry to the replica's log. In a real system, Apply would
// block until causal dependencies are satisfied. Here the caller is responsible
// for applying entries in causal order.
func (r *Replica) Apply(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, e)
	// Advance the replica's version to include the entry's causal vector.
	r.version.Merge(e.Written)
	// Increment this replica's own sequence number for the new write.
	r.version[r.id]++
}

// Version returns a snapshot of the replica's current version vector.
func (r *Replica) Version() VersionVector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version.Clone()
}

// Read returns the latest value for key visible at this replica, along with the
// replica's current version vector. Returns ("", nil, version) if the key has
// no entries.
func (r *Replica) Read(key string) (string, VersionVector) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	val := ""
	for i := len(r.applied) - 1; i >= 0; i-- {
		if r.applied[i].Key == key {
			val = r.applied[i].Value
			break
		}
	}
	return val, r.version.Clone()
}
```

`Dominates` is the predicate that replica selection and monotonic-read enforcement both use.

### Exercise 2: Session with All Four Guarantees

Append to `cpread.go`:

```go
// Session tracks per-client consistency state. It enforces read-your-writes,
// monotonic reads, monotonic writes, and writes-follow-reads.
type Session struct {
	mu       sync.Mutex
	id       string
	readVec  VersionVector // monotonic read frontier
	writeVec VersionVector // causal vector for next write
}

// NewSession returns a new client session.
func NewSession(id string) *Session {
	return &Session{
		id:       id,
		readVec:  make(VersionVector),
		writeVec: make(VersionVector),
	}
}

// ReadVector returns a snapshot of the session's current read frontier.
func (s *Session) ReadVector() VersionVector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readVec.Clone()
}

// WriteVector returns a snapshot of the session's causal write vector.
func (s *Session) WriteVector() VersionVector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeVec.Clone()
}

// Read selects a replica whose version dominates the session's read vector
// (monotonic reads + read-your-writes). It returns the value for key and
// advances the session's read frontier.
func (s *Session) Read(key string, replicas []*Replica) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range replicas {
		rv := r.Version()
		if rv.Dominates(s.readVec) {
			val, repVec := r.Read(key)
			// Monotonic reads: advance read frontier to the max of current
			// frontier and the replica's version after this read.
			s.readVec.Merge(repVec)
			// Writes follow reads: the next write must include what we just read.
			s.writeVec.Merge(repVec)
			return val, nil
		}
	}
	return "", fmt.Errorf("%w (need %v)", ErrNoReplica, s.readVec)
}

// Write returns an Entry stamped with the session's causal vector.  The caller
// applies the entry to replicas; after Apply returns the caller must call
// RecordWrite to advance the session's write frontier.
func (s *Session) Write(key, value string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Entry{
		Key:     key,
		Value:   value,
		Written: s.writeVec.Clone(),
	}
}

// RecordWrite advances the session's write and read frontiers to include the
// replica version returned after applying the entry. This enforces read-your-
// writes and monotonic writes for subsequent operations.
func (s *Session) RecordWrite(repVec VersionVector) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readVec.Merge(repVec)
	s.writeVec.Merge(repVec)
}
```

### Exercise 3: Test the Contract

Create `cpread_test.go`:

```go
package cpread

import (
	"errors"
	"fmt"
	"testing"
)

// helpers ----------------------------------------------------------------

func applyToAll(entry Entry, replicas ...*Replica) {
	for _, r := range replicas {
		r.Apply(entry)
	}
}

// VersionVector tests -----------------------------------------------------

func TestDominatesSymmetry(t *testing.T) {
	t.Parallel()

	a := VersionVector{"r1": 3, "r2": 1}
	b := VersionVector{"r1": 2, "r2": 2}
	if a.Dominates(b) {
		t.Error("a should not dominate b: a[r2]=1 < b[r2]=2")
	}
	if b.Dominates(a) {
		t.Error("b should not dominate a: b[r1]=2 < a[r1]=3")
	}
}

func TestDominatesSelf(t *testing.T) {
	t.Parallel()

	v := VersionVector{"r1": 5, "r2": 3}
	if !v.Dominates(v) {
		t.Error("a vector always dominates itself")
	}
}

func TestDominatesEmpty(t *testing.T) {
	t.Parallel()

	v := VersionVector{"r1": 2}
	empty := VersionVector{}
	if !v.Dominates(empty) {
		t.Error("non-empty should dominate empty")
	}
	if !empty.Dominates(empty) {
		t.Error("empty dominates empty")
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()

	a := VersionVector{"r1": 3, "r2": 1}
	b := VersionVector{"r1": 2, "r2": 5, "r3": 1}
	a.Merge(b)
	if a["r1"] != 3 || a["r2"] != 5 || a["r3"] != 1 {
		t.Fatalf("after merge: %v", a)
	}
}

// Replica tests -----------------------------------------------------------

func TestReplicaApplyAndRead(t *testing.T) {
	t.Parallel()

	r := NewReplica("r1")
	e := Entry{Key: "x", Value: "hello", Written: VersionVector{}}
	r.Apply(e)

	val, _ := r.Read("x")
	if val != "hello" {
		t.Fatalf("Read(x) = %q, want hello", val)
	}
}

func TestReplicaVersionAdvances(t *testing.T) {
	t.Parallel()

	r := NewReplica("r1")
	before := r.Version()["r1"]
	r.Apply(Entry{Key: "k", Value: "v", Written: VersionVector{}})
	after := r.Version()["r1"]
	if after <= before {
		t.Fatalf("replica version did not advance: before=%d after=%d", before, after)
	}
}

func TestReplicaReadMissingKey(t *testing.T) {
	t.Parallel()

	r := NewReplica("r1")
	val, _ := r.Read("nonexistent")
	if val != "" {
		t.Fatalf("Read missing key = %q, want empty string", val)
	}
}

// Session guarantee tests -------------------------------------------------

func TestReadYourWrites(t *testing.T) {
	t.Parallel()

	r := NewReplica("r1")
	sess := NewSession("s1")

	// Write "v1" to "key" via the session, apply to the replica.
	entry := sess.Write("key", "v1")
	r.Apply(entry)
	sess.RecordWrite(r.Version())

	// Read must return "v1" — the session's own write.
	val, err := sess.Read("key", []*Replica{r})
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if val != "v1" {
		t.Fatalf("Read = %q, want v1 (read-your-writes violated)", val)
	}
}

func TestMonotonicReads(t *testing.T) {
	t.Parallel()

	r1 := NewReplica("r1")
	r2 := NewReplica("r2")

	// r1 has two writes; r2 only has the first.
	e1 := Entry{Key: "k", Value: "v1", Written: VersionVector{}}
	r1.Apply(e1)
	r2.Apply(e1)

	e2 := Entry{Key: "k", Value: "v2", Written: r1.Version().Clone()}
	r1.Apply(e2)
	// r2 does NOT get e2 yet.

	sess := NewSession("s1")

	// First read from r1, which has both writes; session sees v2.
	val1, err := sess.Read("k", []*Replica{r1, r2})
	if err != nil || val1 != "v2" {
		t.Fatalf("first Read = %q, %v; want v2, nil", val1, err)
	}

	// Now r1 is "down"; only r2 is available. r2 is behind the session
	// frontier, so the session must reject r2 to avoid going backward.
	_, err = sess.Read("k", []*Replica{r2})
	if !errors.Is(err, ErrNoReplica) {
		t.Fatalf("expected ErrNoReplica (monotonic read protection), got: %v", err)
	}
}

func TestMonotonicWritesOrdering(t *testing.T) {
	t.Parallel()

	r := NewReplica("r1")
	sess := NewSession("s1")

	e1 := sess.Write("k", "w1")
	r.Apply(e1)
	sess.RecordWrite(r.Version())

	e2 := sess.Write("k", "w2")
	// e2.Written must include r1's version after e1 was applied.
	if !e2.Written.Dominates(e1.Written) {
		t.Error("monotonic writes: second write's causal vector must dominate first write's")
	}
}

func TestWritesFollowReads(t *testing.T) {
	t.Parallel()

	r := NewReplica("r1")
	sess := NewSession("s1")

	// Seed a value.
	r.Apply(Entry{Key: "x", Value: "base", Written: VersionVector{}})

	// Session reads x; this advances the session's write vector.
	_, err := sess.Read("x", []*Replica{r})
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}

	// The subsequent write must carry the causal dependency on what was read.
	entry := sess.Write("y", "derived")
	rv := r.Version()
	if !entry.Written.Dominates(rv) {
		t.Error("writes-follow-reads: write's causal vector must dominate the prior read's version")
	}
}

func TestConsistentPrefixViolationWithoutReplica(t *testing.T) {
	t.Parallel()

	// Demonstrate the anomaly: a session whose frontier is ahead of all available
	// replicas must get ErrNoReplica, not stale data.
	sess := NewSession("s1")
	// Artificially advance the session's read vector.
	sess.readVec["r1"] = 99

	staleReplica := NewReplica("r1") // version r1=1 after one apply
	staleReplica.Apply(Entry{Key: "k", Value: "v", Written: VersionVector{}})

	_, err := sess.Read("k", []*Replica{staleReplica})
	if !errors.Is(err, ErrNoReplica) {
		t.Fatalf("expected ErrNoReplica when replica is behind session frontier, got: %v", err)
	}
}

func ExampleSession_Read() {
	r := NewReplica("r1")
	sess := NewSession("s1")

	entry := sess.Write("greeting", "hello")
	r.Apply(entry)
	sess.RecordWrite(r.Version())

	val, err := sess.Read("greeting", []*Replica{r})
	if err != nil {
		panic(err)
	}
	fmt.Println(val)
	// Output: hello
}
```

Your turn: add `TestMultipleReplicas` that creates three replicas, applies writes to all of them, and asserts that the session can read the latest value from any of them.

### Exercise 4: Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/cpread"
)

func main() {
	// Two replicas simulate an eventually-consistent cluster.
	r1 := cpread.NewReplica("r1")
	r2 := cpread.NewReplica("r2")

	sess := cpread.NewSession("client-1")

	// Write "question" to r1 and r2 (both replicas receive the write).
	e1 := sess.Write("msg:1", "What is the answer?")
	r1.Apply(e1)
	r2.Apply(e1)
	sess.RecordWrite(r1.Version())

	// Write the answer — causally depends on the question.
	e2 := sess.Write("msg:2", "The answer is 42.")
	r1.Apply(e2)
	r2.Apply(e2)
	sess.RecordWrite(r1.Version())

	// Read both messages from any replica in the session.
	replicas := []*cpread.Replica{r1, r2}
	for _, key := range []string{"msg:1", "msg:2"} {
		val, err := sess.Read(key, replicas)
		if err != nil {
			log.Fatalf("read %s: %v", key, err)
		}
		fmt.Printf("%-8s => %s\n", key, val)
	}

	fmt.Println()
	fmt.Printf("r1 version: %v\n", r1.Version())
	fmt.Printf("r2 version: %v\n", r2.Version())
	fmt.Printf("session read vector: %v\n", sess.ReadVector())
}
```

Run it:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Comparing Version Vectors With ==

Wrong: `if sessionVec == replicaVec { ... }`. Map comparison with `==` does not compile in Go; two maps are never equal by identity unless they are the same underlying map.

Fix: use `Dominates` for the correct partial-order check. The predicate checks each component independently.

### Forgetting to Call RecordWrite After Apply

Wrong: calling `sess.Write` and `r.Apply`, then doing a subsequent `sess.Read` — the session's read vector is still at zero, so a stale replica can serve the read.

Fix: always call `sess.RecordWrite(r.Version())` after every `Apply`. The session needs to know what version the write landed at before it can enforce read-your-writes.

### Using a Stale Version Snapshot

Wrong: capturing `ver := r.Version()` before the apply and passing that stale snapshot to `RecordWrite`. The replica's version after the apply is higher.

Fix: capture the replica version after `Apply` returns. The `Apply` method increments the replica's own sequence number as part of committing the entry.

### Ignoring ErrNoReplica

Wrong: `val, _ := sess.Read(key, replicas)` — if all replicas are behind the session's frontier, the read silently returns the empty string, which looks like "key not found" rather than "no qualified replica".

Fix: always check the error. `ErrNoReplica` means the system is temporarily unable to satisfy the monotonic-read guarantee and the caller must retry or degrade gracefully.

### Session Vector Not Propagated Across Calls

Wrong: creating a new `Session` for each request. Session guarantees are per-session: a fresh session has no history and cannot enforce monotonic reads or read-your-writes across calls.

Fix: reuse the same `Session` object for the lifetime of a logical client session.

## Verification

From `~/go-exercises/cpread`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass with no output from `gofmt -l`. The race detector catches concurrent map mutations in `VersionVector` if locking is missing in `Session` or `Replica`.

## Summary

- Consistent prefix reads guarantee that if write B causally depends on write A, no reader ever sees B without first seeing A.
- Version vectors (map from replica ID to sequence number) are the causal timestamp; `Dominates` encodes the partial order.
- Session guarantees — read-your-writes, monotonic reads, monotonic writes, writes-follow-reads — are all enforced by tracking a per-session version frontier and selecting only replicas that dominate that frontier.
- `Merge` advances the session frontier after every read or write; failing to call it breaks all four guarantees.
- `ErrNoReplica` signals that no available replica satisfies the session's consistency requirements; the caller must retry or escalate.

## What's Next

Next: [Linux Namespaces: UTS and PID](../../38-capstone-container-runtime/01-linux-namespaces-uts-pid/01-linux-namespaces-uts-pid.md).

## Resources

- [Session Guarantees for Weakly Consistent Replicated Data (Terry et al., 1994)](https://dl.acm.org/doi/10.1145/190163.190169) — the foundational paper defining the four session guarantees
- [sync package](https://pkg.go.dev/sync) — RWMutex semantics used in Replica and Session
- [Causal Consistency (Jepsen)](https://jepsen.io/consistency/models/causal) — formal model and annotated violation examples
- [COPS: Causal+ Consistency (Lloyd et al., SOSP 2011)](https://www.cs.cmu.edu/~dga/papers/cops-sosp2011.pdf) — production causal consistency with version vectors at datacenter scale
- [errors package](https://pkg.go.dev/errors) — errors.Is and sentinel error wrapping used in ErrNoReplica
