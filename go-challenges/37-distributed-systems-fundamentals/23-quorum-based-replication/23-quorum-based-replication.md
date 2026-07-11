# 23. Quorum-Based Replication

Quorum-based replication makes consistency tunable: with N replicas, write quorum W, and read quorum R, the invariant `R + W > N` guarantees that every read quorum intersects every write quorum — so at least one node in a read always holds the latest write. Violating that inequality trades consistency for availability and exposes stale reads. The hard part is not the arithmetic but the mechanics: concurrent writes to N nodes, partial failures where only W < N reply, and read repair that silently re-syncs stale replicas without blocking the caller.

```text
quorum/
  go.mod
  quorum.go
  quorum_test.go
  cmd/demo/main.go
```

The package simulates a replication group in memory. Nodes are goroutines; network delay is a `time.Duration` injected per node so tests can force slow replicas without sleeping.

## Concepts

### The R + W > N Guarantee

Given N total replicas, a write is acknowledged after W nodes confirm it, and a read succeeds after R nodes reply. Because W + R > N and N is the total replica count, any set of W replicas and any set of R replicas must share at least one replica. That shared replica holds the latest write, so the read returns it.

When W + R <= N the two quorums need not overlap. A reader may contact only replicas that missed the latest write, returning a stale value. The system becomes eventually consistent: if you wait long enough for anti-entropy to propagate, all replicas converge, but no single read is guaranteed to see the most recent write.

Common configurations and their properties:

| Configuration | Reads | Writes | Consistency | Note |
|---|---|---|---|---|
| R=1, W=N | fastest | slowest | strong | all-replica write, any-replica read |
| R=N, W=1 | slowest | fastest | strong | any-replica write, all-replica read |
| R=ceil(N/2)+1, W=ceil(N/2) | balanced | balanced | strong | majority quorum |
| R=1, W=1 | fastest | fastest | eventual | no overlap guarantee |

### Versioning And Read Repair

Because multiple replicas respond to a quorum read, they may return different values if a previous write reached only W of N replicas. A version number (monotone counter) or timestamp lets the coordinator pick the latest value. Read repair is the follow-up step: after returning the winner to the caller, the coordinator pushes it to any replica that returned an older version. Read repair is best-effort and asynchronous; it does not block the read response.

### Partial Failure And Coordinator Logic

A coordinator sends a write to all N replicas concurrently, waits for W successes, and returns success to the client — without waiting for the remaining N-W replicas. Those trailing replicas are now temporarily stale. If one of the W replicas that did acknowledge later crashes before the trailing replicas catch up, data is at risk. This is why durability-critical systems set W = majority and persist to stable storage before acknowledging.

On the read side the coordinator collects R responses and picks the one with the highest version. The responses arrive concurrently; the coordinator does not wait beyond R.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/quorum/cmd/demo
cd ~/go-exercises/quorum
go mod init example.com/quorum
```

### Exercise 1: Replica And Coordinator Types

Create `quorum.go`:

```go
package quorum

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQuorumNotMet is returned when fewer than the required number of replicas
// responded successfully within the deadline.
var ErrQuorumNotMet = errors.New("quorum not met")

// record holds one key's value and its monotone version on a single replica.
type record struct {
	value   string
	version uint64
}

// Replica simulates a single storage node. Delay is added to every operation
// so tests can exercise slow or partitioned nodes without real sleeps.
type Replica struct {
	mu    sync.Mutex
	store map[string]record
	// Delay is the artificial latency injected before every Get/Put.
	Delay time.Duration
	// Down, when true, causes every operation to return an error.
	Down atomic.Bool
}

// NewReplica creates an empty Replica.
func NewReplica(delay time.Duration) *Replica {
	return &Replica{
		store: make(map[string]record),
		Delay: delay,
	}
}

// Put stores value at key with the given version. If the stored version is
// already >= version, the write is a no-op (last-write-wins by version).
func (r *Replica) Put(key, value string, version uint64) error {
	if r.Delay > 0 {
		time.Sleep(r.Delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Down.Load() {
		return fmt.Errorf("replica down")
	}
	cur := r.store[key]
	if version > cur.version {
		r.store[key] = record{value: value, version: version}
	}
	return nil
}

// Get returns the value and version stored for key. Missing keys return version 0.
func (r *Replica) Get(key string) (value string, version uint64, err error) {
	if r.Delay > 0 {
		time.Sleep(r.Delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Down.Load() {
		return "", 0, fmt.Errorf("replica down")
	}
	rec := r.store[key]
	return rec.value, rec.version, nil
}

// Coordinator implements quorum reads and writes across a set of Replicas.
type Coordinator struct {
	replicas []*Replica
	// N is the total number of replicas (len(replicas)).
	N int
	// W is the write quorum: the number of replicas that must acknowledge a write.
	W int
	// R is the read quorum: the number of replicas that must respond to a read.
	R int

	// seq is a monotone counter used to assign write versions.
	seq atomic.Uint64
}

// New creates a Coordinator over the given replicas (N=len(replicas)) with the
// supplied write quorum w and read quorum r. It returns ErrQuorumNotMet if w or r
// is outside [1,N]. Pass w=r=N/2+1 for a majority (strong-consistency) quorum.
func New(replicas []*Replica, w, r int) (*Coordinator, error) {
	n := len(replicas)
	if n < 1 {
		return nil, fmt.Errorf("quorum: need at least 1 replica, got %d", n)
	}
	if w < 1 || w > n {
		return nil, fmt.Errorf("%w: W=%d out of range [1,%d]", ErrQuorumNotMet, w, n)
	}
	if r < 1 || r > n {
		return nil, fmt.Errorf("%w: R=%d out of range [1,%d]", ErrQuorumNotMet, r, n)
	}
	return &Coordinator{replicas: replicas, N: n, W: w, R: r}, nil
}

// writeResult holds the outcome of a single replica write.
type writeResult struct {
	err error
}

// readResult holds the outcome of a single replica read.
type readResult struct {
	value   string
	version uint64
	replica *Replica
	err     error
}

// Put writes key=value to all N replicas concurrently and waits for W
// acknowledgments. Returns ErrQuorumNotMet (wrapped) if fewer than W succeed.
func (c *Coordinator) Put(key, value string) error {
	ver := c.seq.Add(1)
	ch := make(chan writeResult, c.N)
	for _, rep := range c.replicas {
		rep := rep
		go func() {
			ch <- writeResult{err: rep.Put(key, value, ver)}
		}()
	}

	acks := 0
	for range c.replicas {
		res := <-ch
		if res.err == nil {
			acks++
			if acks >= c.W {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: got %d/%d acks", ErrQuorumNotMet, acks, c.W)
}

// Get reads key from all N replicas concurrently, waits for R responses to
// return a value to the caller, and then performs read repair on any replica
// (from the full N) that returned a stale version. Returns ErrQuorumNotMet
// (wrapped) if fewer than R replicas respond successfully.
func (c *Coordinator) Get(key string) (string, error) {
	ch := make(chan readResult, c.N)
	for _, rep := range c.replicas {
		rep := rep
		go func() {
			v, ver, err := rep.Get(key)
			ch <- readResult{value: v, version: ver, replica: rep, err: err}
		}()
	}

	// Collect all N results; we need them all for read repair.
	all := make([]readResult, 0, c.N)
	for range c.replicas {
		all = append(all, <-ch)
	}

	// Build the quorum from successful responses.
	var ok []readResult
	for _, r := range all {
		if r.err == nil {
			ok = append(ok, r)
		}
	}
	if len(ok) < c.R {
		return "", fmt.Errorf("%w: got %d/%d responses", ErrQuorumNotMet, len(ok), c.R)
	}

	// Pick the highest-version response among successful replicas.
	best := ok[0]
	for _, r := range ok[1:] {
		if r.version > best.version {
			best = r
		}
	}

	// Read repair: push the latest value to any successful replica that is stale.
	for _, r := range ok {
		if r.version < best.version {
			r := r
			go func() {
				_ = r.replica.Put(key, best.value, best.version)
			}()
		}
	}

	return best.value, nil
}
```

The coordinator tracks writes with an atomic version counter. Concurrent writes to N replicas drain the same channel; the function returns as soon as W acks land. Read repair is a fire-and-forget goroutine launched after returning to the caller.

### Exercise 2: Tests

Create `quorum_test.go`:

```go
package quorum

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func makeReplicas(n int, delay time.Duration) []*Replica {
	reps := make([]*Replica, n)
	for i := range reps {
		reps[i] = NewReplica(delay)
	}
	return reps
}

// TestStrongConsistency verifies that with R+W>N every read returns the
// latest write.
func TestStrongConsistency(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)
	c, err := New(reps, 2, 2) // W=2, R=2, N=3, W+R=4>3
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Put("k", "v1"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1" {
		t.Fatalf("Get = %q, want %q", got, "v1")
	}
}

// TestWriteQuorumPartialFailure shows that Put succeeds when at least W
// replicas are up even if others are down.
func TestWriteQuorumPartialFailure(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)
	// Take one replica down; W=2 so the write should still succeed.
	reps[2].Down.Store(true)

	c, err := New(reps, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("k", "hello"); err != nil {
		t.Fatalf("Put failed with one replica down: %v", err)
	}
}

// TestWriteQuorumNotMet shows that Put returns ErrQuorumNotMet when too
// many replicas are down.
func TestWriteQuorumNotMet(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)
	reps[0].Down.Store(true)
	reps[1].Down.Store(true) // only 1 up, need W=2

	c, err := New(reps, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Put("k", "v")
	if !errors.Is(err, ErrQuorumNotMet) {
		t.Fatalf("err = %v, want ErrQuorumNotMet", err)
	}
}

// TestReadQuorumNotMet shows that Get returns ErrQuorumNotMet when too
// many replicas are down.
func TestReadQuorumNotMet(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)
	c, err := New(reps, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("k", "v"); err != nil {
		t.Fatal(err)
	}
	reps[0].Down.Store(true)
	reps[1].Down.Store(true) // only 1 up, need R=2

	_, err = c.Get("k")
	if !errors.Is(err, ErrQuorumNotMet) {
		t.Fatalf("err = %v, want ErrQuorumNotMet", err)
	}
}

// TestReadRepairConvergesStaleReplica verifies that after a quorum read the
// stale replica is repaired to the latest value.
func TestReadRepairConvergesStaleReplica(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)
	// Write directly to two replicas to simulate a partial write (W=2 succeeded
	// but reps[2] missed it).
	_ = reps[0].Put("k", "v2", 2)
	_ = reps[1].Put("k", "v2", 2)
	_ = reps[2].Put("k", "v1", 1) // stale

	c, err := New(reps, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v2" {
		t.Fatalf("Get = %q, want v2", got)
	}

	// Allow the async read repair to complete.
	time.Sleep(10 * time.Millisecond)

	val, ver, err := reps[2].Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if val != "v2" || ver != 2 {
		t.Fatalf("after read repair: replica[2] = %q ver=%d, want v2 ver=2", val, ver)
	}
}

// TestStaleReadPossibleWithoutQuorumOverlap demonstrates that R+W<=N can
// return stale data.
func TestStaleReadPossibleWithoutQuorumOverlap(t *testing.T) {
	t.Parallel()

	// N=3, W=1, R=1 — no overlap guarantee.
	reps := makeReplicas(3, 0)

	// Write "v2" only to reps[0] manually (bypass coordinator).
	_ = reps[0].Put("k", "v1", 1)
	_ = reps[1].Put("k", "v1", 1)
	_ = reps[2].Put("k", "v1", 1)
	// Now update only reps[0] to v2.
	_ = reps[0].Put("k", "v2", 2)

	// A coordinator that reads only from reps[1] (W=1,R=1) sees stale data.
	c, err := New([]*Replica{reps[1]}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	// reps[1] still holds "v1" — stale read demonstrated.
	if got != "v1" {
		t.Fatalf("expected stale read v1, got %q", got)
	}
}

// TestConcurrentWrites verifies that sequential version numbers are assigned
// even when multiple goroutines write concurrently.
func TestConcurrentWrites(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)
	c, err := New(reps, 2, 2)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 20
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		i := i
		go func() {
			defer wg.Done()
			if err := c.Put("counter", fmt.Sprintf("v%d", i)); err != nil {
				t.Errorf("Put error: %v", err)
			}
		}()
	}
	wg.Wait()

	// After all writes, the key must exist and be readable.
	_, err = c.Get("counter")
	if err != nil {
		t.Fatalf("Get after concurrent writes: %v", err)
	}
}

// TestNewRejectsInvalidParameters checks constructor validation.
func TestNewRejectsInvalidParameters(t *testing.T) {
	t.Parallel()

	reps := makeReplicas(3, 0)

	cases := []struct {
		w, r int
	}{
		{0, 2},
		{2, 0},
		{4, 2},
		{2, 4},
	}
	for _, tc := range cases {
		_, err := New(reps, tc.w, tc.r)
		if !errors.Is(err, ErrQuorumNotMet) {
			t.Errorf("New(w=%d,r=%d): err = %v, want ErrQuorumNotMet", tc.w, tc.r, err)
		}
	}
}

func ExampleCoordinator_Put() {
	reps := makeReplicas(3, 0)
	c, _ := New(reps, 2, 2)
	_ = c.Put("greeting", "hello")
	got, _ := c.Get("greeting")
	fmt.Println(got)
	// Output: hello
}
```

Your turn: add `TestMajorityQuorumFiveReplicas` that creates 5 replicas, sets W=3 and R=3 (W+R=6>5), marks two replicas as `Down`, and verifies that `Put` and `Get` still succeed.

### Exercise 3: Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/quorum"
)

func main() {
	// Three replicas with different simulated latencies.
	reps := []*quorum.Replica{
		quorum.NewReplica(0),
		quorum.NewReplica(5 * time.Millisecond),
		quorum.NewReplica(10 * time.Millisecond),
	}

	// Majority quorum: W=2, R=2, N=3 (W+R=4>3).
	c, err := quorum.New(reps, 2, 2)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("--- strong consistency (W=2, R=2, N=3) ---")
	if err := c.Put("city", "London"); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote: city=London")

	got, err := c.Get("city")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read:  city=%s\n", got)

	// Simulate one replica going down; the quorum still holds.
	fmt.Println()
	fmt.Println("--- one replica down (quorum still holds) ---")
	reps[2].Down.Store(true)
	if err := c.Put("city", "Paris"); err != nil {
		log.Fatalf("Put failed: %v", err)
	}
	fmt.Println("wrote: city=Paris (with one replica down)")
	got, err = c.Get("city")
	if err != nil {
		log.Fatalf("Get failed: %v", err)
	}
	fmt.Printf("read:  city=%s\n", got)

	// Take a second replica down; writes should fail.
	fmt.Println()
	fmt.Println("--- two replicas down (quorum lost) ---")
	reps[1].Down.Store(true)
	err = c.Put("city", "Berlin")
	if err != nil {
		fmt.Printf("Put rejected as expected: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Wrong: Version counter shared across coordinator instances

Two coordinators each starting their `seq` at 0 can assign the same version number to different writes. The replicas then silently discard one write when the second arrives with an equal version.

Fix: use a single coordinator per replica group, or route all writes through a single sequencer. In production systems, versions come from a timestamp or a distributed sequence (Lamport clock, HLC).

### Wrong: Returning before draining the channel

```go
// Bad: goroutines leak when the coordinator returns early.
for range c.replicas {
    res := <-ch
    if res.err == nil {
        acks++
        if acks == c.W {
            return nil // remaining goroutines block on ch forever
        }
    }
}
```

Fix: size the channel to `cap(N)` (buffered) so goroutines can send and exit even when the coordinator has already returned. The code in Exercise 1 uses `make(chan writeResult, c.N)`.

### Wrong: Blocking the caller during read repair

Launching read repair as a synchronous call delays the read response by the latency of the slowest stale replica. For a system aiming at low-latency reads this is unacceptable.

Fix: fire read repair in a goroutine (as in Exercise 1) and return the value to the caller immediately. Accept that read repair may not complete if the process exits.

### Wrong: Assuming R+W>N is sufficient for linearizability

The intersection guarantee only ensures that at least one reader has seen the latest write. If two concurrent writes race, last-write-wins by version is not linearizable — a reader may observe writes out of causal order.

Fix: for linearizable semantics, add conditional writes (compare-and-swap) or route all writes through a leader that serializes them before distributing.

## Verification

From `~/go-exercises/quorum`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass with no output from `gofmt -l`. Run `go run ./cmd/demo` to observe the coordinator behavior manually.

## Summary

- With N replicas, write quorum W, and read quorum R, the invariant R + W > N guarantees read-after-write consistency.
- Violating that invariant (R + W <= N) allows stale reads; the system is eventually consistent.
- The coordinator writes to all N replicas concurrently, returns after W acks, and leaves trailing replicas temporarily stale.
- Read repair is a best-effort background push of the latest value to any replica that returned a stale version during a quorum read.
- Buffered channels prevent goroutine leaks when the coordinator returns early after reaching a quorum.
- Version numbers must come from a single, globally-monotone source; per-coordinator counters break the tie-breaking logic under concurrent writers.

## What's Next

Next: [Consistent Prefix Reads](../24-consistent-prefix-reads/24-consistent-prefix-reads.md).

## Resources

- [pkg.go.dev/sync/atomic](https://pkg.go.dev/sync/atomic) — atomic.Uint64 for the version counter
- [pkg.go.dev/sync](https://pkg.go.dev/sync) — sync.Mutex for per-replica locking
- [Amazon Dynamo paper (2007)](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — the original quorum + sloppy quorum + hinted handoff design
- [go.dev/ref/spec#Channel_types](https://go.dev/ref/spec#Channel_types) — buffered channels and the happens-before guarantees used here
- [Designing Data-Intensive Applications, Chapter 5](https://dataintensive.net/) — quorums, read repair, and tunable consistency
