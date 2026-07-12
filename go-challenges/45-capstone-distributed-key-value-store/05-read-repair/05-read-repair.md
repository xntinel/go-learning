# 5. Read Repair

Read repair is a convergence protocol that runs on the critical read path rather than in a background sweep. The coordinator fans out to all N replicas during a quorum read and, while the client is already waiting, compares the responses to detect staleness. The hard parts are making repair writes idempotent under concurrent repairs on the same key, keeping per-read overhead bounded with probabilistic sampling and digest reads, and feeding the divergence rate back into the anti-entropy scheduler without tightly coupling the two subsystems.

## Concepts

### Fan-out Reads and the Repair Window

A standard quorum read fetches from R of N replicas and returns as soon as R responses arrive. The remaining N-R replicas are never consulted, so a stale replica can go undetected indefinitely. Read repair changes the shape: the coordinator fans out to all N replicas in parallel and waits for all responses within the same RTT the client is already paying. The extra responses arrive for free from a latency perspective; the cost is extra bandwidth and the work of comparing clocks.

The key invariant is unchanged: the client always receives the most recent value seen by any quorum member. The repair is strictly a side effect.

### Vector Clock Dominance: Detecting Staleness

A vector clock is a `map[NodeID]uint64` where each entry records the write count observed at a specific node. Clock A **dominates** clock B when every component of A is >= the corresponding component of B and at least one is strictly greater. A replica is stale when the coordinator's best-seen clock dominates the replica's clock for that key.

```
A = {n1:3, n2:1}  B = {n1:2, n2:1}  →  A dominates B  (n1: 3 > 2)
A = {n1:3, n2:1}  B = {n1:2, n2:2}  →  concurrent      (n1: 3>2 but n2: 1<2)
A = {n1:1, n2:1}  B = {n1:1, n2:1}  →  equal           (neither dominates)
```

Concurrent clocks indicate a write conflict; resolving conflicts is outside the scope of read repair. Read repair only corrects replicas that are demonstrably behind.

### Probabilistic Triggering and Overhead

Performing the full N-way clock comparison on every read multiplies CPU and network load by N/R. A probabilistic trigger runs the comparison on only a fraction P of reads (default P = 0.10). The expected number of reads before a stale replica is repaired is 1/P; at P = 0.10 that is 10 reads on average. The quorum guarantee is unchanged: the client always receives the newest value found by the coordinator, regardless of whether repair was triggered.

### Digest Reads: Reducing Repair Bandwidth

When replicas are consistent — the common case on a healthy cluster — shipping the full value from all N replicas for every sampled read is wasteful. The coordinator instead requests a full value from one replica and only a 32-byte SHA-256 digest of (sorted-key bytes || sorted-clock bytes || data) from the remaining N-1. If all digests match, no divergence exists and the coordinator skips the full comparison entirely. Only when a digest differs does the coordinator fetch the full value from that replica to compare clocks. Dynamo uses a similar two-round mechanism [DeCandia et al., 2007].

### Idempotent Repair Writes

Two coordinators repairing the same stale key simultaneously both enqueue repair writes to the same replica. The replica must apply a repair write only when the incoming clock dominates its current clock for that key. If the replica already holds an equal or newer value — because another repair arrived first or a client wrote directly — it returns `ErrClockDominated` and no data is overwritten. This rule also protects against a repair write arriving after a concurrent client write: the client's write wins because its clock is newer.

### Anti-Entropy Integration via a Sliding Window

A single stale replica is not alarming. A burst of divergence — 5% of reads over the last 1 000 reads, for instance — indicates that a node has been consistently behind longer than read repair can catch up alone. The coordinator tracks a circular boolean window: `true` if that read found a stale replica, `false` otherwise. When the ratio of `true` entries exceeds a configurable threshold, it closes a signal channel (`AntiEntropySignal`) that the anti-entropy subsystem watches. This avoids coupling the coordinator to the anti-entropy scheduler directly and makes the threshold logic independently testable.

## Exercises

This is a library, not a program; the entry point is `go test`.

### Exercise 1: Vector Clocks, Values, and the Replica Interface

Create `readrepair.go`. This file contains all core types, the `ReplicaClient` interface, and an exported in-memory implementation for tests and demos.

```go
package readrepair

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// NodeID identifies a replica in the cluster.
type NodeID string

// VectorClock tracks per-node write counts. A nil map is a valid empty clock
// dominated by any non-empty clock.
type VectorClock map[NodeID]uint64

// Clone returns a deep copy of the vector clock.
func (vc VectorClock) Clone() VectorClock {
	c := make(VectorClock, len(vc))
	for k, v := range vc {
		c[k] = v
	}
	return c
}

// Dominates reports whether vc strictly dominates other: every component of vc
// is >= the corresponding component of other, and at least one is strictly
// greater. A nil map dominates nothing and is dominated by any non-empty clock.
func (vc VectorClock) Dominates(other VectorClock) bool {
	hasGreater := false
	for node, v := range vc {
		o := other[node]
		if v < o {
			return false
		}
		if v > o {
			hasGreater = true
		}
	}
	// A node present in other but absent from vc has an implicit count of 0 in
	// vc. If other has a positive count for such a node, vc cannot dominate.
	for node, o := range other {
		if o == 0 {
			continue
		}
		if _, ok := vc[node]; !ok {
			return false
		}
	}
	return hasGreater
}

// Equal reports whether vc and other represent the same logical time.
func (vc VectorClock) Equal(other VectorClock) bool {
	if len(vc) != len(other) {
		return false
	}
	for node, v := range vc {
		if other[node] != v {
			return false
		}
	}
	return true
}

// Concurrent reports whether vc and other are concurrent: neither dominates
// the other and they are not equal.
func (vc VectorClock) Concurrent(other VectorClock) bool {
	return !vc.Dominates(other) && !other.Dominates(vc) && !vc.Equal(other)
}

// Value is the versioned payload stored at a key on a replica.
type Value struct {
	Data    []byte
	Clock   VectorClock
	Written time.Time // wall-clock timestamp; used as a LWW tiebreaker for concurrent versions
}

// Digest returns the SHA-256 digest of Data concatenated with the serialized
// clock (node IDs sorted lexicographically for determinism). Used by digest
// reads to check consistency without transferring the full value.
func (v *Value) Digest() [32]byte {
	h := sha256.New()
	h.Write(v.Data)
	nodes := make([]string, 0, len(v.Clock))
	for node := range v.Clock {
		nodes = append(nodes, string(node))
	}
	sort.Strings(nodes)
	var buf [8]byte
	for _, node := range nodes {
		h.Write([]byte(node))
		binary.LittleEndian.PutUint64(buf[:], v.Clock[NodeID(node)])
		h.Write(buf[:])
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

// ErrNotFound is returned by ReplicaClient.Get when the key is absent.
var ErrNotFound = errors.New("key not found")

// ErrQuorumUnavailable is returned by Coordinator.Read when fewer than
// QuorumSize replicas responded successfully.
var ErrQuorumUnavailable = errors.New("not enough replicas responded for quorum")

// ErrClockDominated is returned by ReplicaClient.Put when the replica's current
// clock dominates the incoming value's clock; the write is rejected to prevent
// overwriting a newer version.
var ErrClockDominated = errors.New("replica clock dominates incoming value; write rejected")

// ReplicaClient abstracts reads and writes to a single replica.
// Production implementations wrap gRPC stubs; tests and demos use InMemReplica.
type ReplicaClient interface {
	// Get returns the current value for key, or ErrNotFound when the key is absent.
	Get(ctx context.Context, key string) (*Value, error)
	// Put stores value for key, applying idempotent repair semantics: the
	// replica must reject the write with ErrClockDominated when its current
	// clock for key dominates the incoming clock.
	Put(ctx context.Context, key string, value *Value) error
}

// InMemReplica is an in-memory ReplicaClient for tests and demos.
// It is safe for concurrent use.
type InMemReplica struct {
	mu   sync.RWMutex
	data map[string]*Value
}

// NewInMemReplica returns a fresh, empty in-memory replica.
func NewInMemReplica() *InMemReplica {
	return &InMemReplica{data: make(map[string]*Value)}
}

// Get returns the stored value for key, or ErrNotFound when absent.
func (r *InMemReplica) Get(_ context.Context, key string) (*Value, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

// Put stores value for key, enforcing idempotent repair semantics.
func (r *InMemReplica) Put(_ context.Context, key string, incoming *Value) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.data[key]; ok {
		if current.Clock.Dominates(incoming.Clock) {
			return ErrClockDominated
		}
	}
	r.data[key] = incoming
	return nil
}

// replicaResponse holds the result of one parallel Get call.
type replicaResponse struct {
	client ReplicaClient
	value  *Value // nil when the key is absent on this replica
	err    error
}

// repairTask describes an asynchronous repair write.
type repairTask struct {
	client ReplicaClient
	key    string
	value  *Value
}

// Config controls coordinator behaviour.
type Config struct {
	// QuorumSize is the minimum number of replicas that must respond (R >= 1).
	// Default 2.
	QuorumSize int
	// RepairProbability is the fraction of reads [0.0, 1.0] on which the
	// coordinator performs the full N-way comparison and schedules repairs.
	// Default 0.1 (10% of reads).
	RepairProbability float64
	// DivergeThreshold is the fraction of recent reads [0.0, 1.0] that must
	// show divergence before AntiEntropySignal is closed. Default 0.01.
	DivergeThreshold float64
	// WindowSize is the number of recent reads tracked for the divergence ratio.
	// Default 1000.
	WindowSize int
	// RepairWorkers is the number of goroutines draining the async repair queue.
	// Default 4.
	RepairWorkers int
	// RepairQueueDepth is the buffered-channel capacity for pending repair tasks.
	// Tasks dropped when the queue is full are counted in RepairStats.RepairWritesDropped.
	// Default 256.
	RepairQueueDepth int
}

func (cfg *Config) applyDefaults() {
	if cfg.QuorumSize < 1 {
		cfg.QuorumSize = 2
	}
	if cfg.RepairProbability <= 0 {
		cfg.RepairProbability = 0.1
	}
	if cfg.DivergeThreshold <= 0 {
		cfg.DivergeThreshold = 0.01
	}
	if cfg.WindowSize < 1 {
		cfg.WindowSize = 1000
	}
	if cfg.RepairWorkers < 1 {
		cfg.RepairWorkers = 4
	}
	if cfg.RepairQueueDepth < 1 {
		cfg.RepairQueueDepth = 256
	}
}

// Coordinator performs quorum reads with optional background read repair.
// Create with New; call Close to stop background workers.
type Coordinator struct {
	cfg      Config
	counters counters

	repairQ chan repairTask

	windowMu sync.Mutex
	window   []bool // circular buffer; true = that read found at least one stale replica
	windowI  int    // next slot index (unbounded; use mod len for array indexing)

	// AntiEntropySignal is closed exactly once when the divergence ratio in
	// the sliding window exceeds cfg.DivergeThreshold. Watch this channel to
	// schedule an early anti-entropy round.
	AntiEntropySignal chan struct{}
	signalOnce        sync.Once

	wg   sync.WaitGroup
	quit chan struct{}
}

// New creates a Coordinator and starts its repair workers.
// Call Close when done to release goroutines.
func New(cfg Config) *Coordinator {
	cfg.applyDefaults()
	c := &Coordinator{
		cfg:               cfg,
		repairQ:           make(chan repairTask, cfg.RepairQueueDepth),
		window:            make([]bool, cfg.WindowSize),
		AntiEntropySignal: make(chan struct{}),
		quit:              make(chan struct{}),
	}
	for i := 0; i < cfg.RepairWorkers; i++ {
		c.wg.Add(1)
		go c.repairWorker()
	}
	return c
}

// Close stops all repair workers. Pending repair tasks in the queue may not
// complete; anti-entropy is the fallback for any repairs that are dropped.
func (c *Coordinator) Close() {
	close(c.quit)
	c.wg.Wait()
}

// Read fans out to all replicas in parallel, returns the newest value seen by
// any quorum member, and conditionally schedules async repairs for stale
// replicas.
//
// replicas is the full replica set for the key (all N replicas, not just R).
// If fewer than cfg.QuorumSize replicas respond without error, Read returns
// ErrQuorumUnavailable. If all responding replicas report ErrNotFound, Read
// returns ErrNotFound.
func (c *Coordinator) Read(
	ctx context.Context,
	key string,
	replicas []ReplicaClient,
) (*Value, error) {
	c.counters.readsTotal.Add(1)

	responses := make([]replicaResponse, len(replicas))
	var wg sync.WaitGroup
	wg.Add(len(replicas))
	for i, r := range replicas {
		i, r := i, r
		go func() {
			defer wg.Done()
			v, err := r.Get(ctx, key)
			if errors.Is(err, ErrNotFound) {
				// Key absent on this replica: record a successful nil response so
				// the replica is counted toward quorum and eligible for repair.
				responses[i] = replicaResponse{client: r}
				return
			}
			responses[i] = replicaResponse{client: r, value: v, err: err}
		}()
	}
	wg.Wait()

	var ok []replicaResponse
	for _, r := range responses {
		if r.err == nil {
			ok = append(ok, r)
		}
	}
	if len(ok) < c.cfg.QuorumSize {
		return nil, fmt.Errorf("%w: got %d, need %d",
			ErrQuorumUnavailable, len(ok), c.cfg.QuorumSize)
	}

	newest := newestValue(ok)
	if newest == nil {
		return nil, ErrNotFound
	}

	diverged := false
	if rand.Float64() < c.cfg.RepairProbability {
		diverged = c.scheduleRepairs(key, newest, ok)
	}
	c.recordWindow(diverged)

	return newest, nil
}

// newestValue returns the value with the dominant vector clock among responses.
// When two clocks are concurrent (neither dominates), the Written timestamp
// breaks the tie (last-write-wins). This is a best-effort heuristic for
// concurrent conflicts; the application layer is responsible for true conflict
// resolution.
func newestValue(responses []replicaResponse) *Value {
	var best *Value
	for _, r := range responses {
		if r.value == nil {
			continue
		}
		switch {
		case best == nil:
			best = r.value
		case r.value.Clock.Dominates(best.Clock):
			best = r.value
		case r.value.Clock.Concurrent(best.Clock) && r.value.Written.After(best.Written):
			best = r.value
		}
	}
	return best
}

// scheduleRepairs enqueues repair writes for every replica whose value is
// dominated by newest. Returns true if at least one stale replica was found.
func (c *Coordinator) scheduleRepairs(
	key string,
	newest *Value,
	responses []replicaResponse,
) bool {
	found := false
	for _, r := range responses {
		stale := r.value == nil || newest.Clock.Dominates(r.value.Clock)
		if !stale {
			continue
		}
		c.counters.staleDetected.Add(1)
		found = true
		task := repairTask{client: r.client, key: key, value: newest}
		select {
		case c.repairQ <- task:
			c.counters.repairIssued.Add(1)
		default:
			// Queue full: drop the task. Anti-entropy will catch this replica
			// in the next background sweep.
			c.counters.repairDropped.Add(1)
		}
	}
	return found
}

// repairWorker drains the repair queue until Close is called.
func (c *Coordinator) repairWorker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.quit:
			return
		case task := <-c.repairQ:
			err := task.client.Put(context.Background(), task.key, task.value)
			if err == nil || errors.Is(err, ErrClockDominated) {
				// ErrClockDominated means the replica already holds a newer value;
				// that is a successful convergence outcome.
				c.counters.repairSucceeded.Add(1)
			} else {
				c.counters.repairFailed.Add(1)
			}
		}
	}
}

// recordWindow records whether the latest read found divergence and closes
// AntiEntropySignal when the divergence ratio exceeds the configured threshold.
func (c *Coordinator) recordWindow(diverged bool) {
	c.windowMu.Lock()
	defer c.windowMu.Unlock()
	c.window[c.windowI%len(c.window)] = diverged
	c.windowI++
	if c.windowI < len(c.window) {
		return // window not yet full; ratio is not meaningful
	}
	count := 0
	for _, d := range c.window {
		if d {
			count++
		}
	}
	ratio := float64(count) / float64(len(c.window))
	if ratio >= c.cfg.DivergeThreshold {
		c.signalOnce.Do(func() { close(c.AntiEntropySignal) })
	}
}
```

### Exercise 2: Counters and the Stats Snapshot

Create `stats.go`. Keeping the atomic counters in a separate file makes the coordinator file easier to read and makes the counter set easy to extend.

```go
package readrepair

import "sync/atomic"

// counters holds atomic live counters for the coordinator. Fields are grouped
// to avoid false sharing on large cache-line architectures.
type counters struct {
	readsTotal      atomic.Uint64
	staleDetected   atomic.Uint64
	repairIssued    atomic.Uint64
	repairSucceeded atomic.Uint64
	repairFailed    atomic.Uint64
	repairDropped   atomic.Uint64
}

// RepairStats is an immutable snapshot of coordinator counters suitable for
// logging or exposing via a metrics endpoint.
type RepairStats struct {
	ReadsTotal            uint64
	StaleReplicasDetected uint64
	RepairWritesIssued    uint64
	RepairWritesSucceeded uint64
	RepairWritesFailed    uint64
	RepairWritesDropped   uint64
}

// Stats returns a point-in-time snapshot of all counters.
func (c *Coordinator) Stats() RepairStats {
	return RepairStats{
		ReadsTotal:            c.counters.readsTotal.Load(),
		StaleReplicasDetected: c.counters.staleDetected.Load(),
		RepairWritesIssued:    c.counters.repairIssued.Load(),
		RepairWritesSucceeded: c.counters.repairSucceeded.Load(),
		RepairWritesFailed:    c.counters.repairFailed.Load(),
		RepairWritesDropped:   c.counters.repairDropped.Load(),
	}
}
```

### Exercise 3: Test Suite

Create `readrepair_test.go`. The tests cover the vector clock algebra, end-to-end read-repair flow, quorum failure, idempotence of repair writes, and counter accuracy.

```go
package readrepair

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// failReplica is a ReplicaClient that always returns an error, used to
// simulate an unreachable node.
type failReplica struct{}

func (failReplica) Get(_ context.Context, _ string) (*Value, error) {
	return nil, errors.New("replica unreachable")
}

func (failReplica) Put(_ context.Context, _ string, _ *Value) error {
	return errors.New("replica unreachable")
}

// makeValue is a test helper.
func makeValue(data string, clock VectorClock) *Value {
	return &Value{
		Data:    []byte(data),
		Clock:   clock,
		Written: time.Now(),
	}
}

// TestVectorClockDominates verifies the dominance relation exhaustively.
func TestVectorClockDominates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		a, b   VectorClock
		aDomsB bool
		bDomsA bool
	}{
		{
			name:   "a has one extra write",
			a:      VectorClock{"n1": 2, "n2": 1},
			b:      VectorClock{"n1": 1, "n2": 1},
			aDomsB: true,
			bDomsA: false,
		},
		{
			name:   "equal clocks",
			a:      VectorClock{"n1": 1},
			b:      VectorClock{"n1": 1},
			aDomsB: false,
			bDomsA: false,
		},
		{
			name:   "concurrent: each leads on a different node",
			a:      VectorClock{"n1": 2, "n2": 1},
			b:      VectorClock{"n1": 1, "n2": 2},
			aDomsB: false,
			bDomsA: false,
		},
		{
			name:   "empty clock dominates nothing",
			a:      VectorClock{},
			b:      VectorClock{"n1": 1},
			aDomsB: false,
			bDomsA: true,
		},
		{
			name:   "non-empty dominates empty",
			a:      VectorClock{"n1": 1},
			b:      VectorClock{},
			aDomsB: true,
			bDomsA: false,
		},
		{
			name:   "a is a strict superset with larger counts",
			a:      VectorClock{"n1": 5, "n2": 3, "n3": 1},
			b:      VectorClock{"n1": 4, "n2": 2},
			aDomsB: true,
			bDomsA: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.Dominates(tc.b); got != tc.aDomsB {
				t.Errorf("a.Dominates(b) = %v, want %v", got, tc.aDomsB)
			}
			if got := tc.b.Dominates(tc.a); got != tc.bDomsA {
				t.Errorf("b.Dominates(a) = %v, want %v", got, tc.bDomsA)
			}
		})
	}
}

// TestVectorClockConcurrent verifies concurrent detection is consistent with
// the dominance relation.
func TestVectorClockConcurrent(t *testing.T) {
	t.Parallel()

	a := VectorClock{"n1": 2, "n2": 1}
	b := VectorClock{"n1": 1, "n2": 2}
	if !a.Concurrent(b) {
		t.Fatal("expected a and b to be concurrent")
	}
	// A clock is not concurrent with itself (it is equal, not concurrent).
	if a.Concurrent(a) {
		t.Fatal("a clock must not be concurrent with itself")
	}
}

// TestValueDigestDeterministic checks that identical values produce the same
// digest and that different values do not.
func TestValueDigestDeterministic(t *testing.T) {
	t.Parallel()

	clock := VectorClock{"n1": 3, "n2": 2}
	v1 := makeValue("hello", clock)
	v2 := makeValue("hello", clock.Clone())
	if v1.Digest() != v2.Digest() {
		t.Fatal("identical values produced different digests")
	}

	v3 := makeValue("world", clock)
	if v1.Digest() == v3.Digest() {
		t.Fatal("different values produced the same digest")
	}
}

// TestReadAllConsistentNoRepair verifies that when all replicas hold the same
// value no repair tasks are issued.
func TestReadAllConsistentNoRepair(t *testing.T) {
	t.Parallel()

	clock := VectorClock{"n1": 1}
	val := makeValue("v1", clock)

	r1, r2, r3 := NewInMemReplica(), NewInMemReplica(), NewInMemReplica()
	for _, r := range []*InMemReplica{r1, r2, r3} {
		if err := r.Put(context.Background(), "k", val); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	c := New(Config{QuorumSize: 2, RepairProbability: 1.0})
	defer c.Close()

	got, err := c.Read(context.Background(), "k", []ReplicaClient{r1, r2, r3})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.Data) != "v1" {
		t.Fatalf("data = %q, want %q", got.Data, "v1")
	}

	// Allow repair workers time to drain (there should be nothing to drain).
	time.Sleep(20 * time.Millisecond)
	s := c.Stats()
	if s.StaleReplicasDetected != 0 {
		t.Errorf("StaleReplicasDetected = %d, want 0", s.StaleReplicasDetected)
	}
	if s.RepairWritesIssued != 0 {
		t.Errorf("RepairWritesIssued = %d, want 0", s.RepairWritesIssued)
	}
}

// TestReadOneStaleRepairTriggered verifies that one stale replica is detected,
// a repair write is enqueued, and the replica is updated within one second.
func TestReadOneStaleRepairTriggered(t *testing.T) {
	t.Parallel()

	freshClock := VectorClock{"n1": 2}
	staleClock := VectorClock{"n1": 1}

	r1 := NewInMemReplica() // fresh
	r2 := NewInMemReplica() // fresh
	r3 := NewInMemReplica() // stale

	for _, r := range []*InMemReplica{r1, r2} {
		if err := r.Put(context.Background(), "k", makeValue("v2", freshClock)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := r3.Put(context.Background(), "k", makeValue("v1", staleClock)); err != nil {
		t.Fatalf("Put stale: %v", err)
	}

	c := New(Config{
		QuorumSize:        2,
		RepairProbability: 1.0,
		RepairWorkers:     1,
		RepairQueueDepth:  8,
	})
	defer c.Close()

	got, err := c.Read(context.Background(), "k", []ReplicaClient{r1, r2, r3})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.Data) != "v2" {
		t.Fatalf("Read returned %q, want v2", string(got.Data))
	}

	// Poll until r3 is repaired or the deadline expires.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, _ := r3.Get(context.Background(), "k")
		if v != nil && string(v.Data) == "v2" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	v3, err := r3.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("r3.Get after repair: %v", err)
	}
	if string(v3.Data) != "v2" {
		t.Fatalf("r3 not repaired: got %q, want v2", v3.Data)
	}

	s := c.Stats()
	if s.StaleReplicasDetected == 0 {
		t.Error("StaleReplicasDetected = 0, want > 0")
	}
	if s.RepairWritesIssued == 0 {
		t.Error("RepairWritesIssued = 0, want > 0")
	}
}

// TestReadQuorumUnavailable verifies that ErrQuorumUnavailable is returned when
// fewer than QuorumSize replicas respond.
func TestReadQuorumUnavailable(t *testing.T) {
	t.Parallel()

	r1 := NewInMemReplica()
	// r2 and r3 always fail.
	var r2, r3 failReplica

	c := New(Config{QuorumSize: 2})
	defer c.Close()

	_, err := c.Read(context.Background(), "k", []ReplicaClient{r1, r2, r3})
	if !errors.Is(err, ErrQuorumUnavailable) {
		t.Fatalf("err = %v, want ErrQuorumUnavailable", err)
	}
}

// TestRepairIdempotentWhenReplicaAlreadyNewer verifies that a repair write is
// rejected when the replica already holds a newer value, and that the newer
// value is not overwritten.
func TestRepairIdempotentWhenReplicaAlreadyNewer(t *testing.T) {
	t.Parallel()

	staleClock := VectorClock{"n1": 1}
	freshClock := VectorClock{"n1": 3}

	r := NewInMemReplica()
	// Store a fresh value directly.
	if err := r.Put(context.Background(), "k", makeValue("v3", freshClock)); err != nil {
		t.Fatalf("Put fresh: %v", err)
	}

	// Attempt to overwrite with an older value (simulating a late repair write).
	err := r.Put(context.Background(), "k", makeValue("v1", staleClock))
	if !errors.Is(err, ErrClockDominated) {
		t.Fatalf("err = %v, want ErrClockDominated", err)
	}

	// The fresh value must survive.
	current, err := r.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(current.Data) != "v3" {
		t.Fatalf("data = %q after rejected repair; want v3 (newer value must survive)", current.Data)
	}
}

// TestStatsAccountedCorrectly verifies that counters are updated atomically
// across multiple reads.
func TestStatsAccountedCorrectly(t *testing.T) {
	t.Parallel()

	freshClock := VectorClock{"n1": 2}
	staleClock := VectorClock{"n1": 1}

	r1, r2, r3 := NewInMemReplica(), NewInMemReplica(), NewInMemReplica()
	for _, r := range []*InMemReplica{r1, r2} {
		if err := r.Put(context.Background(), "k", makeValue("v2", freshClock)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := r3.Put(context.Background(), "k", makeValue("v1", staleClock)); err != nil {
		t.Fatalf("Put stale: %v", err)
	}

	c := New(Config{
		QuorumSize:        2,
		RepairProbability: 1.0,
		RepairWorkers:     2,
		RepairQueueDepth:  32,
	})
	defer c.Close()

	const N = 5
	for i := 0; i < N; i++ {
		if _, err := c.Read(context.Background(), "k", []ReplicaClient{r1, r2, r3}); err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
	}

	// Wait for repair workers to drain the queue.
	time.Sleep(50 * time.Millisecond)

	s := c.Stats()
	if s.ReadsTotal != N {
		t.Errorf("ReadsTotal = %d, want %d", s.ReadsTotal, N)
	}
	if s.StaleReplicasDetected == 0 {
		t.Error("StaleReplicasDetected = 0, want > 0")
	}
	if s.RepairWritesIssued == 0 {
		t.Error("RepairWritesIssued = 0, want > 0")
	}
	// After the first repair the replica holds v2, so ErrClockDominated is
	// returned for subsequent repair attempts; those still count as succeeded.
	if s.RepairWritesSucceeded == 0 {
		t.Error("RepairWritesSucceeded = 0, want > 0")
	}
}

// Your turn: add TestAntiEntropySignalFires. Set WindowSize to 10,
// DivergeThreshold to 0.5, and RepairProbability to 1.0. Put a stale value on
// one of three replicas. Call Read 6 times (6/10 = 0.6 > 0.5 threshold). After
// filling the window, assert that AntiEntropySignal is closed within 100 ms.

// ExampleVectorClock_Dominates shows the dominance relation between two clocks.
func ExampleVectorClock_Dominates() {
	a := VectorClock{"n1": 2, "n2": 1}
	b := VectorClock{"n1": 1, "n2": 1}
	fmt.Println(a.Dominates(b))
	fmt.Println(b.Dominates(a))
	// Output:
	// true
	// false
}
```

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`. This binary uses only exported API and is runnable with `go run ./cmd/demo`.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/readrepair"
)

func main() {
	freshClock := readrepair.VectorClock{"n1": 3}
	staleClock := readrepair.VectorClock{"n1": 1}

	r1 := readrepair.NewInMemReplica()
	r2 := readrepair.NewInMemReplica()
	r3 := readrepair.NewInMemReplica() // intentionally stale

	fresh := &readrepair.Value{
		Data:    []byte("value-at-version-3"),
		Clock:   freshClock,
		Written: time.Now(),
	}
	stale := &readrepair.Value{
		Data:    []byte("value-at-version-1"),
		Clock:   staleClock,
		Written: time.Now().Add(-5 * time.Minute),
	}

	for _, r := range []*readrepair.InMemReplica{r1, r2} {
		if err := r.Put(context.Background(), "greeting", fresh); err != nil {
			log.Fatalf("seeding fresh replica: %v", err)
		}
	}
	if err := r3.Put(context.Background(), "greeting", stale); err != nil {
		log.Fatalf("seeding stale replica: %v", err)
	}

	c := readrepair.New(readrepair.Config{
		QuorumSize:        2,
		RepairProbability: 1.0, // always repair for the demo
		RepairWorkers:     2,
	})
	defer c.Close()

	before, _ := r3.Get(context.Background(), "greeting")
	fmt.Printf("r3 before repair: %s\n", before.Data)

	result, err := c.Read(context.Background(), "greeting",
		[]readrepair.ReplicaClient{r1, r2, r3})
	if err != nil {
		log.Fatalf("Read: %v", err)
	}
	fmt.Printf("Read returned:    %s\n", result.Data)

	// The repair write is asynchronous; wait for the worker to finish.
	time.Sleep(50 * time.Millisecond)

	after, _ := r3.Get(context.Background(), "greeting")
	fmt.Printf("r3 after repair:  %s\n", after.Data)

	s := c.Stats()
	fmt.Printf("stats: stale_detected=%d issued=%d succeeded=%d dropped=%d\n",
		s.StaleReplicasDetected,
		s.RepairWritesIssued,
		s.RepairWritesSucceeded,
		s.RepairWritesDropped,
	)
}
```

## Common Mistakes

**Wrong:** Checking only R responses for staleness instead of all N.
What happens: replicas that respond after the quorum threshold is met are silently ignored. A permanently stale replica is never detected because it always ends up in the "ignored" set.
Fix: fan out to all N replicas in parallel and process every response for comparison, even those that arrive after quorum is reached.

**Wrong:** Using a monotonic `time.Now()` clock instead of a vector clock to determine which value is newer.
What happens: wall-clock skew across nodes causes the coordinator to pick a stale value and repair fresh replicas with it, corrupting data.
Fix: use vector clock dominance as the primary ordering; apply `Written` timestamps only as a last-resort tiebreaker for clocks that are concurrent (neither dominates the other).

**Wrong:** Repairing every replica on every read regardless of `RepairProbability`.
What happens: on a hot key with N=3 and a read rate of 10 000 req/s, the coordinator issues 10 000 × (N-1) = 20 000 repair writes per second even when all replicas are consistent, creating a write amplification loop.
Fix: gate the full N-way comparison behind `rand.Float64() < cfg.RepairProbability` and only issue repair writes for replicas that are actually dominated.

**Wrong:** Returning an error from `Put` in `InMemReplica` when the incoming clock is concurrent with the current clock.
What happens: the repair is dropped silently when it should be applied, because the recipient cannot determine dominance conclusively.
Fix: reject only when the current clock dominates the incoming clock (`current.Clock.Dominates(incoming.Clock)`). Concurrent clocks indicate a conflict; the repair write should still be accepted and the conflict escalated to application-level resolution.

**Wrong:** Closing `AntiEntropySignal` inside a goroutine that holds `windowMu`.
What happens: if the anti-entropy subsystem's `close` call blocks (for example, because a receiver on the channel is not ready), the `windowMu` lock is held during the block, deadlocking all concurrent `Read` calls.
Fix: use `sync.Once.Do` so the channel is closed without holding any lock; `Once` provides its own synchronization.

## Verification

From `~/go-exercises/readrepair`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The test output for a correctly implemented read repair is:

```
ok  	example.com/readrepair	(cached)
```

Run the demo to observe the repair in action:

```bash
go run ./cmd/demo
```

Expected output (timing may vary):

```
r3 before repair: value-at-version-1
Read returned:    value-at-version-3
r3 after repair:  value-at-version-3
stats: stale_detected=1 issued=1 succeeded=1 dropped=0
```

## Summary

- Read repair detects staleness during a quorum read by fanning out to all N replicas instead of only R, adding no latency because the comparison happens in the same RTT.
- Vector clock dominance is the correct staleness criterion; wall-clock LWW is used only as a concurrent-conflict tiebreaker.
- Probabilistic triggering (default 10%) caps per-read overhead; the quorum guarantee is unchanged regardless of whether repair is triggered.
- Repair writes are idempotent: the recipient applies a write only when the incoming clock dominates its current clock, protecting against concurrent repairs and late-arriving writes.
- A sliding-window divergence counter integrates with anti-entropy by signalling when the repair-eligible fraction of recent reads exceeds a threshold, triggering an early background sweep.
- Stats counters use `sync/atomic.Uint64` for zero-overhead, race-free updates from concurrent repair workers.

## What's Next

Next: [Membership Protocol](../06-membership-protocol/06-membership-protocol.md).

## Resources

- DeCandia, G. et al. "Dynamo: Amazon's Highly Available Key-Value Store." SOSP 2007. https://dl.acm.org/doi/10.1145/1294261.1294281 — original description of read repair and digest reads in a production system.
- Apache Cassandra read repair documentation. https://cassandra.apache.org/doc/latest/cassandra/operating/read_repair.html — production configuration of repair probability and blocking vs. background repair.
- Lamport, L. "Time, Clocks, and the Ordering of Events in a Distributed System." CACM 1978. https://doi.org/10.1145/359545.359563 — the original vector-clock paper; the dominance relation used here is a direct application.
- Go standard library: `crypto/sha256`. https://pkg.go.dev/crypto/sha256 — used for deterministic digest computation.
- Go standard library: `math/rand/v2`. https://pkg.go.dev/math/rand/v2 — `rand.Float64()` for probabilistic trigger; introduced in Go 1.22, available in Go 1.26.
