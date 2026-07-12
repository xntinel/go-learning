# Exercise 5: Padding a sharded counter to kill false sharing

A sharded counter spreads writes across per-shard cells so concurrent writers do
not contend on one atomic. But if two shards share a 64-byte cache line, the
cores writing them fight over that line anyway — *false sharing* — and the
sharding buys nothing. This module builds a sharded counter whose cells are each
padded to a full cache line, and contrasts it with an unpadded one.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test, and a benchmark that documents the win.

## What you'll build

```text
shardcounter/              independent module: example.com/shardcounter
  go.mod                   go 1.26
  counter.go               paddedShard (64B), PaddedCounter, UnpaddedCounter
  cmd/
    demo/
      main.go              runs writers across shards, prints the total
  counter_test.go          Sizeof(paddedShard) % 64 == 0; -race correctness; benchmarks
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `paddedShard` sized to exactly one cache line, a `PaddedCounter` and an `UnpaddedCounter` each exposing `Add(shard int, delta int64)`, `Sum() int64`, and `Shards() int`.
- Test: assert `unsafe.Sizeof(paddedShard{})` is a multiple of 64; a `-race` test where N goroutines add on their own shard and `Sum` equals the expected total; benchmarks `BenchmarkPadded`/`BenchmarkUnpadded` that document (not assert) the throughput difference.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/05-cache-line-false-sharing/cmd/demo
cd go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/05-cache-line-false-sharing
```

### Why padding to a cache line matters

Cache coherency operates on cache lines, typically 64 bytes. When core A writes a
variable, the whole line holding it is marked modified and any copy in core B's
cache is invalidated. If two logically independent counters sit in the same line,
A's write to shard 0 invalidates B's cached copy of the line it is using for
shard 1, so B must re-fetch before its next write. The two cores ping-pong the
line between their caches and effectively serialize — even though there is no
lock and no logical dependency. That is false sharing, and it can cost a large
fraction of throughput on a hot counter while being completely invisible to a
correctness test.

The fix is to make each shard occupy its own cache line, so no two shards ever
share one. `paddedShard` holds an `atomic.Int64` (8 bytes) plus a padding array
sized to fill the rest of a 64-byte line: `[cacheLine - unsafe.Sizeof(atomic
.Int64{})]byte`. Because `unsafe.Sizeof` is a compile-time constant, that array
length is legal, and the struct comes out to exactly 64 bytes. A `[]paddedShard`
then places each counter on its own line. `UnpaddedCounter` uses a bare
`atomic.Int64` per shard (8 bytes), so eight shards pack into one line and writers
to those eight shards contend on it — the shape you must avoid.

Correctness is identical for both: each shard is an independent atomic, `Add`
routes to a shard, and `Sum` reads them all. Only throughput differs, which is
why the difference is documented by a benchmark rather than asserted by a test —
timing is not a hard contract, and CI machines vary.

Create `counter.go`:

```go
// Package shardcounter is a sharded atomic counter whose shards are padded to a
// full cache line to eliminate false sharing between concurrent writers.
package shardcounter

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// cacheLine is the common x86-64 / arm64 cache-line size in bytes.
const cacheLine = 64

// paddedShard occupies exactly one cache line, so no two shards share a line and
// writers on different cores never invalidate each other's cache line.
type paddedShard struct {
	n atomic.Int64
	_ [cacheLine - unsafe.Sizeof(atomic.Int64{})]byte
}

// unpaddedShard is a bare atomic; several pack into one cache line, so writers to
// adjacent shards suffer false sharing. Kept for the benchmark contrast.
type unpaddedShard struct {
	n atomic.Int64
}

// PaddedCounter spreads increments across cache-line-padded shards.
type PaddedCounter struct {
	shards []paddedShard
}

// UnpaddedCounter spreads increments across unpadded shards (false-sharing prone).
type UnpaddedCounter struct {
	shards []unpaddedShard
}

// NewPadded returns a PaddedCounter with one shard per CPU (at least one).
func NewPadded() *PaddedCounter {
	return &PaddedCounter{shards: make([]paddedShard, max(1, runtime.NumCPU()))}
}

// NewUnpadded returns an UnpaddedCounter with one shard per CPU (at least one).
func NewUnpadded() *UnpaddedCounter {
	return &UnpaddedCounter{shards: make([]unpaddedShard, max(1, runtime.NumCPU()))}
}

// Add applies delta to the given shard (indexed modulo the shard count).
func (c *PaddedCounter) Add(shard int, delta int64) {
	c.shards[shard%len(c.shards)].n.Add(delta)
}

// Sum totals all shards. Not atomic across shards; call when writers are quiesced.
func (c *PaddedCounter) Sum() int64 {
	var total int64
	for i := range c.shards {
		total += c.shards[i].n.Load()
	}
	return total
}

// Shards reports the number of shards.
func (c *PaddedCounter) Shards() int { return len(c.shards) }

// Add applies delta to the given shard (indexed modulo the shard count).
func (c *UnpaddedCounter) Add(shard int, delta int64) {
	c.shards[shard%len(c.shards)].n.Add(delta)
}

// Sum totals all shards.
func (c *UnpaddedCounter) Sum() int64 {
	var total int64
	for i := range c.shards {
		total += c.shards[i].n.Load()
	}
	return total
}

// Shards reports the number of shards.
func (c *UnpaddedCounter) Shards() int { return len(c.shards) }

// ShardSize reports the byte size of one padded shard (one cache line).
func ShardSize() uintptr { return unsafe.Sizeof(paddedShard{}) }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/shardcounter"
)

func main() {
	c := shardcounter.NewPadded()
	const totalWork = 800_000
	shards := c.Shards()

	var wg sync.WaitGroup
	for s := range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distribute a fixed amount of work across however many shards
			// this CPU has; shard 0 absorbs the remainder so the grand total
			// is exactly totalWork regardless of the core count.
			n := totalWork / shards
			if s == 0 {
				n += totalWork % shards
			}
			for range n {
				c.Add(s, 1)
			}
		}()
	}
	wg.Wait()

	fmt.Printf("padded shard size: %d bytes\n", shardcounter.ShardSize())
	fmt.Printf("total: %d\n", c.Sum())
}
```

Run it (the total is deterministic; the shard size is 64 on amd64/arm64):

```bash
go run ./cmd/demo
```

Expected output (the shard count varies with your CPU, but the total is fixed at 800000):

```
padded shard size: 64 bytes
total: 800000
```

### Tests

The layout test pins the padded shard to a whole cache line. The correctness test
runs one writer goroutine per shard under `-race` and checks the total. The
benchmarks compare padded and unpadded throughput but assert nothing — they exist
to be run with `go test -bench`.

Create `counter_test.go`:

```go
package shardcounter

import (
	"sync"
	"testing"
	"unsafe"
)

func TestPaddedShardIsWholeCacheLine(t *testing.T) {
	t.Parallel()

	sz := unsafe.Sizeof(paddedShard{})
	if sz%cacheLine != 0 {
		t.Errorf("paddedShard size = %d, want a multiple of the %d-byte cache line", sz, cacheLine)
	}
	if unsafe.Sizeof(unpaddedShard{}) >= cacheLine {
		t.Errorf("unpaddedShard should be smaller than a cache line, got %d", unsafe.Sizeof(unpaddedShard{}))
	}
}

func TestConcurrentAddSumsCorrectly(t *testing.T) {
	t.Parallel()

	c := NewPadded()
	const perShard = 10_000
	var wg sync.WaitGroup
	for s := range c.Shards() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perShard {
				c.Add(s, 1)
			}
		}()
	}
	wg.Wait()

	if want := int64(c.Shards()) * perShard; c.Sum() != want {
		t.Errorf("Sum = %d, want %d", c.Sum(), want)
	}
}

func TestAddRoutesModuloShards(t *testing.T) {
	t.Parallel()

	c := NewPadded()
	// A shard index beyond the count wraps; totals still add up.
	c.Add(c.Shards()+1, 5)
	c.Add(0, 3)
	if c.Sum() != 8 {
		t.Errorf("Sum = %d, want 8", c.Sum())
	}
}

func benchmarkCounter(b *testing.B, add func(shard int, delta int64), shards int) {
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			add(i%shards, 1)
			i++
		}
	})
}

func BenchmarkPadded(b *testing.B) {
	c := NewPadded()
	benchmarkCounter(b, c.Add, c.Shards())
}

func BenchmarkUnpadded(b *testing.B) {
	c := NewUnpadded()
	benchmarkCounter(b, c.Add, c.Shards())
}
```

## Review

The counter is correct for both layouts — each shard is an independent atomic, so
`Sum` after a quiesce equals the number of increments regardless of padding. What
padding changes is throughput: run `go test -bench . -cpu $(nproc)` and, on a
multi-core machine under contention, `BenchmarkPadded` typically beats
`BenchmarkUnpadded` because the padded shards never share a cache line. The
mistake to avoid is packing hot per-goroutine counters adjacently and then
blaming the scheduler or a lock for the lost throughput — there is no lock; the
cost is coherency traffic on a shared line, and the cure is a cache line of
padding per hot cell.

## Resources

- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64.Add` / `Load` used per shard.
- [runtime.NumCPU](https://pkg.go.dev/runtime#NumCPU) — sizing the shard array to the core count.
- [False sharing (Wikipedia)](https://en.wikipedia.org/wiki/False_sharing) — the coherency-traffic mechanism the padding defeats.

---

Back to [04-size-regression-gate.md](04-size-regression-gate.md) | Next: [06-atomic-alignment-32bit.md](06-atomic-alignment-32bit.md)
