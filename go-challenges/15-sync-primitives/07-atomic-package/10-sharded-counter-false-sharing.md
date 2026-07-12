# Exercise 10: Sharded Hot Counter: Beating Contention and False Sharing

Exercise 1's metrics counter is lock-free, yet on a many-core box the profiler
shows its single `atomic.Int64` burning CPU: every core's `Add` drags the same
64-byte cache line into exclusive mode, invalidating everyone else's copy. This
module builds the fix — a sharded counter with cache-line-padded shards — and
measures the trade instead of asserting it.

This module is fully self-contained.

## What you'll build

```text
shardcount/                independent module: example.com/shardcount
  go.mod
  counter.go               type shard (padded to 64 bytes, compile-time asserted); type ShardedCounter; New, Inc, IncHint, Value, Reset
  cmd/
    demo/
      main.go              8 workers x 100000 increments, exact total at quiescence, Reset
  counter_test.go          exactness-at-quiescence table, sizeof assertion, Reset test, Example, BenchmarkSingleAtomic vs BenchmarkSharded vs BenchmarkShardedHint
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `shard` struct padded to exactly 64 bytes with a compile-time size assertion; `ShardedCounter` with `New` (shard count rounded up to a power of two, defaulting to `runtime.NumCPU()`), `Inc` (random shard via `math/rand/v2`), `IncHint` (caller-supplied shard hint, masked), `Value` (sum of per-shard loads, exact only at quiescence), `Reset`.
- Test: G goroutines x K increments then quiesce, `Value` equals G*K for 1, 4, and default shard counts; `unsafe.Sizeof(shard{}) == 64`; Reset-then-count; benchmarks comparing one hot atomic against the sharded counter under `b.RunParallel`.
- Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchmem`

### Why a lock-free counter still hits a wall

No goroutine ever blocks on an `atomic.Int64`, but the hardware still serializes
the writes. Cores cache memory in 64-byte lines, and the coherence protocol (MESI
and friends) lets only one core hold a line writable at a time. Sixteen cores
calling `Add` on one counter means the line ping-pongs: each increment first pulls
the line into that core's cache in exclusive mode, costing a cross-core round trip
of tens of nanoseconds, then loses it to the next core. Throughput flatlines or
drops as cores are added. This is the difference between lock-free and
contention-free, and no faster atomic exists to buy you out of it — the fix is to
stop sharing the cache line.

Sharding does exactly that: one counter becomes a small array of counters, each
writer picks a shard cheaply and increments it, and a read sums the shards.
Writers spread across many lines and stop invalidating each other. The shard
count is `runtime.NumCPU()` rounded up to a power of two, because a power-of-two
length turns "pick a shard from a hint" into a single AND with `len-1` — no
modulo, and negative hints mask to a valid index for free since the mask's sign
bit is zero.

The subtlety that silently erases the whole win is false sharing. A bare
`atomic.Int64` is 8 bytes, so eight adjacent shards pack into ONE cache line —
physically distinct variables, same line, same ping-pong you started with. Each
shard therefore carries a `[56]byte` filler so `unsafe.Sizeof(shard{}) == 64` and
every shard owns a full line. Because a well-meaning refactor (adding a field,
shrinking the pad) reintroduces the problem with zero compiler feedback, the size
is asserted at compile time: two blank array declarations whose lengths are
`64 - Sizeof` and `Sizeof - 64` — if the size drifts either way, one length goes
negative and the build fails. (The stdlib does the same padding trick inside
`sync.Pool`'s per-P structures.)

What you pay is read-side semantics. `Value` loops over the shards doing atomic
`Load`s and sums them in ordinary code. Each load is atomic, but the sum is not a
global snapshot: while writers run, an increment can land on a shard already
summed (missed) or not yet reached (counted early). The total is exact once
writers quiesce and approximate while they move — the same consistency class as a
Prometheus counter scrape. That trade is correct for metrics and telemetry and
wrong for money, quotas, or anything audited; those need a single atomic (and its
contention) or a mutex.

Two increment paths exist because picking the shard must be cheaper than the
contention it avoids. `Inc` draws `rand.IntN` — a few nanoseconds, fine for most
callers. `IncHint` skips even that: a worker pool that already knows its worker
index passes it and lands on a stable shard, which is both faster and friendlier
to the cache (each worker keeps hitting its own line). The benchmarks measure all
three arrangements so the decision is data, not folklore.

Create `counter.go`:

```go
package shardcount

import (
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// shard holds one slice of the total. The filler pads the struct to exactly
// 64 bytes -- one cache line on x86-64 and most arm64 server cores -- so two
// shards never share a line and one shard's writes never invalidate another's.
type shard struct {
	v atomic.Int64
	_ [56]byte
}

// Compile-time proof that shard fills exactly one 64-byte cache line: if the
// size drifts either way, one of these array lengths goes negative and the
// package stops compiling.
var (
	_ [64 - unsafe.Sizeof(shard{})]byte
	_ [unsafe.Sizeof(shard{}) - 64]byte
)

// ShardedCounter spreads increments over padded per-shard atomics so the cache
// line of one hot counter stops ping-ponging between cores. Value is exact only
// once writers quiesce; see its doc comment.
type ShardedCounter struct {
	shards []shard
}

// New returns a counter with n shards rounded up to the next power of two.
// n <= 0 defaults to runtime.NumCPU(). A power-of-two count makes shard
// selection a single mask instead of a modulo.
func New(n int) *ShardedCounter {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	return &ShardedCounter{shards: make([]shard, nextPow2(n))}
}

// Inc adds one to a randomly chosen shard.
func (c *ShardedCounter) Inc() {
	c.IncHint(rand.IntN(len(c.shards)))
}

// IncHint adds one to the shard selected by hint. Any int works -- the value is
// masked into range (negative hints included, since the mask clears the sign
// bit). Callers that hold a cheap stable index (worker number, P-local id)
// should pass it: it skips the random draw and keeps each caller on one line.
func (c *ShardedCounter) IncHint(hint int) {
	c.shards[hint&(len(c.shards)-1)].v.Add(1)
}

// Value returns the sum of all shards. Each per-shard Load is atomic, but the
// sum is NOT a global snapshot: while writers run it can miss increments that
// land on already-summed shards. Once writers quiesce it is exact. That is the
// consistency of a metrics scrape -- fine for telemetry, wrong for money.
func (c *ShardedCounter) Value() int64 {
	var total int64
	for i := range c.shards {
		total += c.shards[i].v.Load()
	}
	return total
}

// Reset zeroes every shard. Like Value, it is a clean cut only at quiescence;
// concurrent increments may land before or after any given shard's Store.
func (c *ShardedCounter) Reset() {
	for i := range c.shards {
		c.shards[i].v.Store(0)
	}
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
```

### The runnable demo

The demo pins the correctness story — exact total at quiescence, clean Reset —
and points at the benchmarks for the performance story, because raw throughput
numbers are machine-dependent and belong in `go test -bench`, not in an expected
output block.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/shardcount"
)

func main() {
	c := shardcount.New(4)

	const workers = 8
	const perWorker = 100000

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				c.IncHint(w) // each worker sticks to its own shard
			}
		}()
	}
	wg.Wait()

	fmt.Println("total after quiesce:", c.Value())
	c.Reset()
	fmt.Println("total after reset:", c.Value())
	fmt.Println("benchmark the contention story with:")
	fmt.Println("  go test -bench=. -benchmem")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total after quiesce: 800000
total after reset: 0
benchmark the contention story with:
  go test -bench=. -benchmem
```

### Tests and benchmarks

`TestExactAtQuiescence` is the load-bearing test: G goroutines each perform K
increments through a mix of `Inc` and `IncHint`, then fully quiesce via
`wg.Wait()`, and `Value` must equal exactly G*K — sharding may spread the counts
anywhere, but it must never lose one. The sizeof test guards the padding against
refactors (redundantly with the compile-time assertion, but a test failure
message beats a cryptic build error). The benchmarks compare one hot atomic
against random-shard and hint-shard increments under `b.RunParallel`; they are
informational, not asserted, because the crossover point depends on core count.

Create `counter_test.go`:

```go
package shardcount

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestExactAtQuiescence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shards     int
		goroutines int
		perG       int
	}{
		{"single shard degenerate", 1, 8, 2000},
		{"four shards", 4, 16, 2000},
		{"default shards", 0, 32, 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := New(tc.shards)
			var wg sync.WaitGroup
			for g := range tc.goroutines {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range tc.perG {
						if i%2 == 0 {
							c.Inc()
						} else {
							c.IncHint(g)
						}
					}
				}()
			}
			wg.Wait()

			want := int64(tc.goroutines * tc.perG)
			if got := c.Value(); got != want {
				t.Fatalf("Value() = %d after quiesce, want %d", got, want)
			}
		})
	}
}

func TestShardFillsOneCacheLine(t *testing.T) {
	t.Parallel()

	if got := unsafe.Sizeof(shard{}); got != 64 {
		t.Fatalf("unsafe.Sizeof(shard{}) = %d, want 64: padding drifted, false sharing is back", got)
	}
}

func TestShardCountRounding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int
		want int
	}{
		{1, 1},
		{3, 4},
		{4, 4},
		{5, 8},
		{9, 16},
	}
	for _, tc := range tests {
		if got := len(New(tc.n).shards); got != tc.want {
			t.Errorf("New(%d) has %d shards, want %d", tc.n, got, tc.want)
		}
	}
}

func TestResetThenCount(t *testing.T) {
	t.Parallel()

	c := New(4)
	for i := range 100 {
		c.IncHint(i)
	}
	c.IncHint(-3) // negative hints mask into range
	if got := c.Value(); got != 101 {
		t.Fatalf("Value() = %d before reset, want 101", got)
	}

	c.Reset()
	if got := c.Value(); got != 0 {
		t.Fatalf("Value() = %d after Reset, want 0", got)
	}

	c.Inc()
	if got := c.Value(); got != 1 {
		t.Fatalf("Value() = %d after post-reset Inc, want 1", got)
	}
}

func ExampleShardedCounter() {
	c := New(4)
	for range 10 {
		c.Inc()
	}
	fmt.Println(c.Value())
	// Output: 10
}

func BenchmarkSingleAtomic(b *testing.B) {
	var n atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n.Add(1)
		}
	})
}

func BenchmarkShardedRandom(b *testing.B) {
	c := New(0)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func BenchmarkShardedHint(b *testing.B) {
	c := New(0)
	var worker atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		hint := int(worker.Add(1))
		for pb.Next() {
			c.IncHint(hint)
		}
	})
}
```

## Review

The counter is correct when no increment is ever lost — `TestExactAtQuiescence`
under `-race` proves it across shard counts — and it is *effective* only when the
padding holds, which both the compile-time array assertion and the sizeof test
pin. Run the benchmarks with several `-cpu` values (`go test -bench=. -cpu=1,4,8`)
and read the story: at one core the single atomic wins (no contention to avoid,
and `Inc` pays for the random draw), and as cores climb the sharded variants pull
ahead while `BenchmarkShardedHint` beats the random pick by skipping it. The
mistakes to avoid: dropping the padding (false sharing quietly restores the
ping-pong, and only a benchmark regression tells you); treating `Value` under
motion as a consistent global total — it is exact only at quiescence, fine for a
metrics scrape, wrong for a balance; and sharding preemptively — one atomic is
simpler and faster until the profiler shows the line bouncing, so measure first.

## Resources

- [`sync/atomic` package documentation](https://pkg.go.dev/sync/atomic) — `Int64.Add`, `Int64.Load`, `Int64.Store`.
- [False sharing (cache-line contention)](https://en.wikipedia.org/wiki/False_sharing) — the mechanism the 64-byte padding defeats.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `IntN` and the concurrency-safe top-level generator.
- [`testing.B.RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — how the parallel benchmarks drive contention.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-atomic-bitmask-feature-flags.md](09-atomic-bitmask-feature-flags.md) | Next: [../08-atomic-value-config-hot-reload/00-concepts.md](../08-atomic-value-config-hot-reload/00-concepts.md)
