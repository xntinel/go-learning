# Exercise 6: Measure Lock Contention: Mutex vs Atomic vs Sharded Counter

A sequential benchmark of a metrics counter measures it with no competition, which is
never how it runs in production. Under real concurrent traffic the synchronization
choice dominates: a `sync.Mutex` that looks fine alone collapses under contention,
while an atomic or a sharded counter stays flat. This module benchmarks three
concurrent-counter implementations with `b.RunParallel` and `b.SetParallelism` so the
contention cost is visible.

## What you'll build

```text
counter/                   independent module: example.com/counter
  go.mod                   go 1.24
  counter.go               MutexCounter, AtomicCounter, ShardedCounter (all Inc()/Value())
  cmd/
    demo/
      main.go              runnable demo: concurrent increments, print totals
  counter_test.go          TestCounter (correct total under concurrency, -race);
                           RunParallel benchmarks for all three; SetParallelism variant; Example
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: three counters exposing `Inc()` and `Value() int64` — mutex, atomic, sharded.
- Test: each reaches the correct total under concurrent increments (`-race`), plus parallel benchmarks.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
go mod edit -go=1.24
```

### The three implementations and where they diverge

All three counters have the same contract — `Inc()` adds one, `Value()` reads the
total — and all three are correct: a `TestCounter` that fires N goroutines each doing
M increments must see `N*M` from every one. Where they diverge is *cost under
contention*, which only a parallel benchmark exposes.

`MutexCounter` guards an `int64` with a `sync.Mutex`. Every `Inc` acquires the lock,
so under many goroutines they serialize on one lock and one cache line — the classic
contention bottleneck. `AtomicCounter` uses `atomic.Int64.Add`, a single lock-free
instruction; it still contends on one cache line (every core fighting to own it) but
avoids the lock's park/wake machinery, so it is dramatically cheaper under load.
`ShardedCounter` removes even the cache-line contention: it holds an array of
padded per-shard `atomic.Int64`s, and `Inc` picks a shard using the scalable per-P
random source in `math/rand/v2`, so different goroutines usually touch different cache
lines. `Value` sums the shards. The padding (`_ [56]byte` after an 8-byte counter)
pushes each shard onto its own 64-byte cache line to prevent false sharing — two
shards on one line would contend as if they were the same variable.

`b.RunParallel` runs each `Inc` across `GOMAXPROCS` goroutines. Read the three
side by side and the ordering is the lesson: mutex slowest, atomic faster, sharded
fastest, with the gap widening as parallelism rises. `b.SetParallelism(p)` scales the
goroutine count to `p * GOMAXPROCS` to model an oversubscribed server, where the mutex
degrades further while the sharded counter barely moves.

Create `counter.go`:

```go
package counter

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

// MutexCounter guards an int64 with a mutex. Every Inc serializes on the lock.
type MutexCounter struct {
	mu sync.Mutex
	n  int64
}

func (c *MutexCounter) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *MutexCounter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// AtomicCounter uses a single lock-free add. Contends on one cache line but avoids
// the lock's park/wake cost.
type AtomicCounter struct {
	n atomic.Int64
}

func (c *AtomicCounter) Inc() { c.n.Add(1) }

func (c *AtomicCounter) Value() int64 { return c.n.Load() }

// shard is a counter padded to its own 64-byte cache line to avoid false sharing.
type shard struct {
	n atomic.Int64
	_ [56]byte
}

// ShardedCounter spreads increments across many cache lines. Each Inc adds to a
// pseudo-randomly chosen shard; Value sums them. The random choice uses the scalable
// per-P source in math/rand/v2, so it does not reintroduce a shared hot spot.
type ShardedCounter struct {
	shards []shard
}

// NewSharded returns a sharded counter with n shards (n >= 1).
func NewSharded(n int) *ShardedCounter {
	if n < 1 {
		n = 1
	}
	return &ShardedCounter{shards: make([]shard, n)}
}

func (c *ShardedCounter) Inc() {
	c.shards[rand.IntN(len(c.shards))].n.Add(1)
}

func (c *ShardedCounter) Value() int64 {
	var total int64
	for i := range c.shards {
		total += c.shards[i].n.Load()
	}
	return total
}
```

### The runnable demo

The demo increments each counter from many goroutines and prints the totals, which are
deterministic (each reaches `workers*perWorker`) even though the interleaving is not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/counter"
)

const (
	workers   = 50
	perWorker = 1000
)

func run(inc func()) {
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				inc()
			}
		}()
	}
	wg.Wait()
}

func main() {
	var m counter.MutexCounter
	var a counter.AtomicCounter
	s := counter.NewSharded(16)

	run(m.Inc)
	run(a.Inc)
	run(s.Inc)

	fmt.Printf("mutex   = %d\n", m.Value())
	fmt.Printf("atomic  = %d\n", a.Value())
	fmt.Printf("sharded = %d\n", s.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mutex   = 50000
atomic  = 50000
sharded = 50000
```

### Tests

`TestCounter` runs each implementation through the same concurrent-increment harness
and asserts the exact total; under `-race` it also proves each is data-race free. The
benchmarks use `b.RunParallel`, and `BenchmarkShardedOversubscribed` adds
`b.SetParallelism(4)` to model four goroutines per core.

Create `counter_test.go`:

```go
package counter

import (
	"fmt"
	"sync"
	"testing"
)

// counter is the shared contract the three implementations satisfy.
type counter interface {
	Inc()
	Value() int64
}

func hammer(c counter, workers, per int) {
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				c.Inc()
			}
		}()
	}
	wg.Wait()
}

func TestCounter(t *testing.T) {
	t.Parallel()
	const workers, per = 50, 200
	want := int64(workers * per)

	cases := []struct {
		name string
		make func() counter
	}{
		{"mutex", func() counter { return &MutexCounter{} }},
		{"atomic", func() counter { return &AtomicCounter{} }},
		{"sharded", func() counter { return NewSharded(16) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := tc.make()
			hammer(c, workers, per)
			if got := c.Value(); got != want {
				t.Fatalf("%s total = %d, want %d", tc.name, got, want)
			}
		})
	}
}

func BenchmarkMutex(b *testing.B) {
	var c MutexCounter
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func BenchmarkAtomic(b *testing.B) {
	var c AtomicCounter
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func BenchmarkSharded(b *testing.B) {
	c := NewSharded(64)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func BenchmarkShardedOversubscribed(b *testing.B) {
	c := NewSharded(64)
	b.SetParallelism(4) // 4 * GOMAXPROCS goroutines
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func ExampleAtomicCounter() {
	var c AtomicCounter
	c.Inc()
	c.Inc()
	fmt.Println(c.Value())
	// Output: 2
}
```

Run the parallel benchmarks; the ordering is the point (illustrative numbers):

```bash
go test -bench=. -benchmem
```

```text
BenchmarkMutex-8                    18402331      64.1 ns/op    0 B/op   0 allocs/op
BenchmarkAtomic-8                   96110254      12.2 ns/op    0 B/op   0 allocs/op
BenchmarkSharded-8                 271043118       4.4 ns/op    0 B/op   0 allocs/op
BenchmarkShardedOversubscribed-8   248817360       4.8 ns/op    0 B/op   0 allocs/op
PASS
```

## Review

`TestCounter` establishes all three are correct under concurrency and race-clean —
the non-negotiable precondition before any performance comparison, because a faster
counter that loses increments is not faster, it is broken. The benchmark lesson is
that `RunParallel` reverses the intuition a sequential benchmark would give: sequentially
the mutex and atomic look nearly identical, but under contention the mutex's serialize
-and-park cost separates it from the atomic by roughly 5x here, and the sharded counter
by more, precisely because it spreads writes across cache lines that padding keeps
apart. `SetParallelism(4)` shows the sharded design barely degrades when oversubscribed
while a mutex would degrade further. The takeaway a senior engineer carries: benchmark
anything touched by multiple goroutines with `RunParallel`, and treat a sequential-only
number for a concurrent structure as untrustworthy.

## Resources

- [`testing.B.RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — run the body across GOMAXPROCS goroutines to expose contention.
- [`testing.B.SetParallelism`](https://pkg.go.dev/testing#B.SetParallelism) — scale the goroutine count to model oversubscription.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — the lock-free `Int64` used by the atomic and sharded counters.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-throughput-with-setbytes.md](05-throughput-with-setbytes.md) | Next: [07-custom-metrics-reportmetric.md](07-custom-metrics-reportmetric.md)
