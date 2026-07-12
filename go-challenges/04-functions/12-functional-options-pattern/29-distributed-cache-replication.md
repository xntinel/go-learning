# Exercise 29: Redis Cache With Replication Strategy and Failover Options

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A replicated cache that requires acknowledgment from a quorum of replicas
before a write commits has a hard constraint hiding in its configuration: a
consistency level higher than the replication factor asks for more
acknowledgments than replicas exist to give them, and quorum could never be
reached no matter what happens on the wire. This module builds that cache
with functional options, replicating writes to simulated replicas
concurrently and rejecting a commit that reaches quorum too slowly to beat
its own failover timeout.

## What you'll build

```text
cache/                            independent module: example.com/distributed-cache-replication
  go.mod                          go 1.24
  cache.go                        Cache, PutResult, Option, New, WithReplicationFactor,
                                   WithConsistencyLevel, WithFailoverTimeout,
                                   MarkReplicaDown, SetReplicaLatency, Put, Get
  cmd/
    demo/
      main.go                     a fast quorum commit, then a replica outage forcing failover
  cache_test.go                    option-validation table, quorum, timeout, and -race tests
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a `Cache` built by `New(opts ...Option) (*Cache, error)` whose `Put` replicates concurrently to every replica and commits only if enough replicas acknowledge within the failover timeout, validating that the consistency level never exceeds the replication factor.
- Test: every option-validation case including the exact boundary where consistency level equals replication factor, a commit that reaches quorum in time, a commit that never reaches quorum because a replica is down, a commit that reaches quorum too slowly, and a `-race` concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/29-distributed-cache-replication/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/29-distributed-cache-replication
go mod edit -go=1.24
```

### Why consistency level can never exceed replication factor

`WithReplicationFactor` and `WithConsistencyLevel` are independent options
— either can be set without the other, in any order — but together they
describe a physical impossibility if consistency level is the larger of the
two: a write replicated to 3 places can never collect 5 acknowledgments.
Neither option's closure knows the other's value while it runs, so `New`
checks `consistencyLevel > replicationFactor` once, after every option has
applied — the same constructor-boundary pattern used for every cross-field
invariant in this chapter.

### Replicating concurrently, deterministically

`Put` fans a write out to every replica in its own goroutine and waits for
all of them with a `sync.WaitGroup` before deciding anything — that
concurrency is real, exercised under `-race`. What is not real is time:
`SetReplicaLatency` records a fixed `time.Duration` per replica as plain
data, never a real sleep, so `Put` can compute "the time to reach quorum" —
the consistency-level-th fastest acknowledgment, once latencies are sorted
— as pure arithmetic. That is what makes `TestPutFailsWhenQuorumTooSlow`
deterministic: a replica whose configured latency exceeds the failover
timeout reliably triggers the same failure every run, with no flakiness a
real clock or a real sleep could introduce.

### The check-then-act discipline

`MarkReplicaDown`, `SetReplicaLatency`, and `replicaSnapshot` all take
`c.mu` for their entire read-or-write of the shared replica slices — never
just part of it — so a concurrent `Put` reading replica state and a
concurrent `MarkReplicaDown` call can never interleave into a torn read.
`Put` commits the write to `c.data` under the same mutex, and only after
every quorum and timeout check has already passed.

Create `cache.go`:

```go
package cache

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Cache replicates writes across a fixed number of simulated replicas,
// requiring acknowledgment from at least consistencyLevel of them before a
// write is considered committed, and rejecting a commit whose time to reach
// quorum would have exceeded the failover timeout.
type Cache struct {
	replicationFactor int
	consistencyLevel  int
	failoverTimeout   time.Duration

	mu             sync.Mutex
	replicaDown    []bool
	replicaLatency []time.Duration
	data           map[string]string
}

// Option configures a Cache and may reject invalid input.
type Option func(*Cache) error

// PutResult reports how a committed write was acknowledged.
type PutResult struct {
	Acked        int
	TimeToQuorum time.Duration
}

// New builds a Cache, seeding a replication factor of 3, a consistency
// level of 2, and a one-second failover timeout, then applies opts. It is
// the single validation boundary: the consistency level must never exceed
// the replication factor, or quorum could never be reached no matter how
// many replicas acknowledge a write.
func New(opts ...Option) (*Cache, error) {
	c := &Cache{
		replicationFactor: 3,
		consistencyLevel:  2,
		failoverTimeout:   time.Second,
		data:              make(map[string]string),
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if c.consistencyLevel > c.replicationFactor {
		return nil, fmt.Errorf("consistency level %d exceeds replication factor %d", c.consistencyLevel, c.replicationFactor)
	}

	c.replicaDown = make([]bool, c.replicationFactor)
	c.replicaLatency = make([]time.Duration, c.replicationFactor)
	return c, nil
}

// WithReplicationFactor sets how many replicas each write is sent to
// (>= 1).
func WithReplicationFactor(n int) Option {
	return func(c *Cache) error {
		if n < 1 {
			return fmt.Errorf("replication factor must be >= 1, got %d", n)
		}
		c.replicationFactor = n
		return nil
	}
}

// WithConsistencyLevel sets how many replicas must acknowledge a write
// before it is considered committed (>= 1).
func WithConsistencyLevel(n int) Option {
	return func(c *Cache) error {
		if n < 1 {
			return fmt.Errorf("consistency level must be >= 1, got %d", n)
		}
		c.consistencyLevel = n
		return nil
	}
}

// WithFailoverTimeout sets the maximum time quorum may take to reach before
// a write is treated as failed over (> 0).
func WithFailoverTimeout(d time.Duration) Option {
	return func(c *Cache) error {
		if d <= 0 {
			return fmt.Errorf("failover timeout must be positive, got %s", d)
		}
		c.failoverTimeout = d
		return nil
	}
}

// MarkReplicaDown marks replica i as down (unable to acknowledge writes) or
// back up.
func (c *Cache) MarkReplicaDown(i int, down bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.replicaDown) {
		return fmt.Errorf("replica index %d out of range [0,%d)", i, len(c.replicaDown))
	}
	c.replicaDown[i] = down
	return nil
}

// SetReplicaLatency sets the simulated acknowledgment latency for replica i
// (>= 0). No real time passes; the value is only used as deterministic data
// when computing time to quorum.
func (c *Cache) SetReplicaLatency(i int, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.replicaLatency) {
		return fmt.Errorf("replica index %d out of range [0,%d)", i, len(c.replicaLatency))
	}
	if d < 0 {
		return fmt.Errorf("replica latency must not be negative, got %s", d)
	}
	c.replicaLatency[i] = d
	return nil
}

func (c *Cache) replicaSnapshot(i int) (down bool, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replicaDown[i], c.replicaLatency[i]
}

// Put replicates key/value to every replica concurrently, then checks
// whether enough replicas acknowledged quickly enough to reach quorum
// within the failover timeout. It commits the write and returns a
// PutResult only if at least consistencyLevel replicas acknowledged and the
// time to reach that quorum (the consistencyLevel-th fastest acknowledgment)
// did not exceed the failover timeout.
func (c *Cache) Put(key, value string) (PutResult, error) {
	type ack struct {
		latency time.Duration
	}

	var wg sync.WaitGroup
	acksCh := make(chan ack, c.replicationFactor)

	for i := 0; i < c.replicationFactor; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			down, latency := c.replicaSnapshot(i)
			if down {
				return
			}
			acksCh <- ack{latency: latency}
		}(i)
	}
	wg.Wait()
	close(acksCh)

	var latencies []time.Duration
	for a := range acksCh {
		latencies = append(latencies, a.latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	if len(latencies) < c.consistencyLevel {
		return PutResult{}, fmt.Errorf("quorum not reached: %d of %d required replicas acknowledged", len(latencies), c.consistencyLevel)
	}

	timeToQuorum := latencies[c.consistencyLevel-1]
	if timeToQuorum > c.failoverTimeout {
		return PutResult{}, fmt.Errorf("failover timeout exceeded: quorum would take %s, limit is %s", timeToQuorum, c.failoverTimeout)
	}

	c.mu.Lock()
	c.data[key] = value
	c.mu.Unlock()

	return PutResult{Acked: len(latencies), TimeToQuorum: timeToQuorum}, nil
}

// Get returns the committed value for key, if any.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}
```

### The runnable demo

The demo sets three replica latencies (10ms, 30ms, 200ms) with a 100ms
failover timeout and a consistency level of 2: the write commits, needing
only the 2nd-fastest ack (30ms). Then replica 0 is marked down, leaving only
the 30ms and 200ms replicas — quorum of 2 is still reachable, but the
2nd-fastest ack among the survivors is now 200ms, over the limit, so the
second write is rejected and never committed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/distributed-cache-replication"
)

func main() {
	c, err := cache.New(
		cache.WithReplicationFactor(3),
		cache.WithConsistencyLevel(2),
		cache.WithFailoverTimeout(100*time.Millisecond),
	)
	if err != nil {
		panic(err)
	}

	_ = c.SetReplicaLatency(0, 10*time.Millisecond)
	_ = c.SetReplicaLatency(1, 30*time.Millisecond)
	_ = c.SetReplicaLatency(2, 200*time.Millisecond)

	result, err := c.Put("key1", "v1")
	fmt.Printf("put key1: acked=%d, timeToQuorum=%s, err=%v\n", result.Acked, result.TimeToQuorum, err)

	_ = c.MarkReplicaDown(0, true)

	_, err = c.Put("key2", "v2")
	fmt.Printf("put key2: err=%v\n", err)

	v1, ok1 := c.Get("key1")
	fmt.Printf("get key1: value=%q, found=%t\n", v1, ok1)

	v2, ok2 := c.Get("key2")
	fmt.Printf("get key2: value=%q, found=%t\n", v2, ok2)

	_, err = cache.New(
		cache.WithReplicationFactor(3),
		cache.WithConsistencyLevel(5),
	)
	fmt.Printf("consistency level exceeding replication factor rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
put key1: acked=3, timeToQuorum=30ms, err=<nil>
put key2: err=failover timeout exceeded: quorum would take 200ms, limit is 100ms
get key1: value="v1", found=true
get key2: value="", found=false
consistency level exceeding replication factor rejected: true
```

### Tests

`TestNewValidation` tables the construction failures, including the exact
boundary where consistency level equals replication factor (allowed) versus
exceeds it (rejected). `TestPutCommitsWhenQuorumReachedInTime` proves the
happy path and its exact `TimeToQuorum` arithmetic.
`TestPutFailsWhenReplicaDownDropsBelowQuorum` proves a write is never
committed when too few replicas are up to ever reach quorum.
`TestPutFailsWhenQuorumTooSlow` proves a write that *can* reach quorum but
only too slowly is still rejected and never committed.
`TestReplicaAccessorsRejectOutOfRangeIndex` guards the replica-index bounds
checks. `TestConcurrentPutAndReplicaUpdates` runs `-race` over concurrent
`Put` calls and concurrent replica-latency updates.

Create `cache_test.go`:

```go
package cache

import (
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "invalid replication factor", opts: []Option{WithReplicationFactor(0)}, wantErr: true},
		{name: "invalid consistency level", opts: []Option{WithConsistencyLevel(0)}, wantErr: true},
		{name: "invalid failover timeout", opts: []Option{WithFailoverTimeout(0)}, wantErr: true},
		{
			name:    "consistency level exceeds replication factor",
			opts:    []Option{WithReplicationFactor(3), WithConsistencyLevel(5)},
			wantErr: true,
		},
		{
			name: "consistency level equal to replication factor is allowed",
			opts: []Option{WithReplicationFactor(3), WithConsistencyLevel(3)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPutCommitsWhenQuorumReachedInTime(t *testing.T) {
	t.Parallel()

	c, err := New(
		WithReplicationFactor(3),
		WithConsistencyLevel(2),
		WithFailoverTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetReplicaLatency(0, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReplicaLatency(1, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReplicaLatency(2, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	result, err := c.Put("key1", "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Acked != 3 {
		t.Fatalf("Acked = %d, want 3 (no replica is down)", result.Acked)
	}
	if result.TimeToQuorum != 30*time.Millisecond {
		t.Fatalf("TimeToQuorum = %s, want 30ms (the 2nd-fastest ack)", result.TimeToQuorum)
	}

	v, ok := c.Get("key1")
	if !ok || v != "v1" {
		t.Fatalf("Get(key1) = (%q, %t), want (v1, true)", v, ok)
	}
}

func TestPutFailsWhenReplicaDownDropsBelowQuorum(t *testing.T) {
	t.Parallel()

	c, err := New(WithReplicationFactor(3), WithConsistencyLevel(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReplicaDown(0, true); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Put("key1", "v1"); err == nil {
		t.Fatal("expected an error: only 2 of 3 required replicas can acknowledge")
	}
	if _, ok := c.Get("key1"); ok {
		t.Fatal("a write that never reached quorum must not be committed")
	}
}

func TestPutFailsWhenQuorumTooSlow(t *testing.T) {
	t.Parallel()

	c, err := New(
		WithReplicationFactor(3),
		WithConsistencyLevel(2),
		WithFailoverTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReplicaDown(0, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReplicaLatency(1, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReplicaLatency(2, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Put("key2", "v2"); err == nil {
		t.Fatal("expected a failover-timeout error: quorum needs the 200ms replica, over the 100ms limit")
	}
	if _, ok := c.Get("key2"); ok {
		t.Fatal("a write that exceeded the failover timeout must not be committed")
	}
}

func TestReplicaAccessorsRejectOutOfRangeIndex(t *testing.T) {
	t.Parallel()

	c, err := New(WithReplicationFactor(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReplicaDown(5, true); err == nil {
		t.Fatal("expected an error for an out-of-range replica index")
	}
	if err := c.SetReplicaLatency(-1, time.Millisecond); err == nil {
		t.Fatal("expected an error for a negative replica index")
	}
}

func TestConcurrentPutAndReplicaUpdates(t *testing.T) {
	c, err := New(WithReplicationFactor(4), WithConsistencyLevel(2))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _ = c.Put("k", "v")
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = c.SetReplicaLatency(i%4, time.Duration(i)*time.Microsecond)
		}(i)
	}
	wg.Wait()

	if _, ok := c.Get("k"); !ok {
		t.Fatal("expected at least one Put to have committed under concurrent load")
	}
}
```

## Review

The cache is correct when quorum is never asked to reach a number of
replicas that could not physically exist, and when reaching quorum
"eventually" is not the same as reaching it "in time" — a write must commit
or fail atomically based on both conditions. The `-race`-checked concurrency
here is real (every replica is contacted through an actual goroutine), while
the timing is deliberately fake (a `time.Duration` value, never a sleep),
which is what lets `TestPutFailsWhenQuorumTooSlow` fail for exactly the same
reason every single run. The consistency-level check follows the same
constructor-boundary shape as every other cross-field invariant in this
chapter: seed defaults, apply every option, validate the combination once.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [Redis replication](https://redis.io/docs/latest/operate/oss_and_stack/management/replication/)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-schema-validator-rule-engine.md](28-schema-validator-rule-engine.md) | Next: [30-full-text-search-analyzer.md](30-full-text-search-analyzer.md)
