# Exercise 14: Cache-Line-Padded Shard Counters to Avoid False Sharing

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A single `atomic.Uint64` hot counter, incremented from every core in a
multi-core server, becomes a bottleneck long before the CPU itself is
saturated: every core's write invalidates every other core's cached copy of
that same 8-byte word, so the cores end up serialized on cache-coherence
traffic instead of doing useful work. The standard fix is sharding — an
array of independent counters, one per worker or per core, summed only on
read — but a naive array of counters just moves the problem: eight 8-byte
`atomic.Uint64` values pack into a single 64-byte cache line, so eight
*logically* independent shards still *physically* share cache lines and
still contend. This module builds both versions, measures the size
difference with `unsafe.Sizeof`, and proves the padded version's totals stay
correct under concurrent, lock-free increments — this is the same trick the
Go runtime itself uses for per-P `mcache` counters.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
shardcounters/                module example.com/shardcounters
  go.mod                      go 1.24
  shards.go                   PaddedShard/UnpaddedShard; Shards{shards [16]PaddedShard}; Add/Get/Total; ErrShardOutOfRange
  shards_test.go               add/get table, out-of-range, concurrent totals under -race,
                                sizeof check, two benchmarks, ExampleShards_Add
```

- Files: `shards.go`, `shards_test.go`.
- Implement: `PaddedShard{value atomic.Uint64; _ [64-unsafe.Sizeof(atomic.Uint64{})]byte}` and its unpadded twin `UnpaddedShard`; `Shards` wrapping `[NumShards]PaddedShard` with `Add(idx int, delta uint64) error`, `Get(idx int) (uint64, error)`, and `Total() uint64`.
- Test: a table of `Add`/`Get` cases including negative and past-end indices; a concurrency test with many goroutines incrementing every shard that must pass under `-race`; `unsafe.Sizeof(PaddedShard{}) == 64` and `unsafe.Sizeof(UnpaddedShard{}) < 64`; two benchmarks (`BenchmarkPaddedShards`, `BenchmarkUnpaddedShards`) exercising the same workload over each shard type; and `ExampleShards_Add` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/14-cache-line-padded-shard-counters
cd go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/14-cache-line-padded-shard-counters
go mod edit -go=1.24
```

### Why padding, not just sharding, fixes the contention

Sharding alone already helps: instead of every core racing to increment one
`atomic.Uint64`, each core (or each worker goroutine) increments its own
shard, and `Total` sums them all on read. But CPU caches do not track
individual bytes — they track 64-byte cache lines as a unit. If eight
`atomic.Uint64` shards (8 bytes each) sit packed back-to-back in an array,
all eight land inside the same one or two cache lines. A core writing to
shard 3 invalidates the cache line that also holds shards 0-7 on every other
core, so those cores still stall on cache-coherence traffic even though they
are touching "different" counters. This is false sharing: the data is
logically independent but physically adjacent, and the hardware's coherence
protocol cannot tell the difference.

`PaddedShard` fixes this by making every shard exactly one cache line wide:

```go
type PaddedShard struct {
	value atomic.Uint64
	_     [cacheLineSize - unsafe.Sizeof(atomic.Uint64{})]byte
}
```

`atomic.Uint64` is 8 bytes on both amd64 and arm64, so the blank padding
field is `64 - 8 = 56` bytes, bringing the whole struct to exactly 64
bytes — one cache line. An array of `PaddedShard` places every shard on its
own cache line, so a write to shard *i* only ever invalidates shard *i*'s
line; shard *i+1*, sitting on the next line entirely, is untouched. This is
not a micro-optimization confined to benchmarks — it is the same technique
the Go runtime uses for per-P scheduler state and `mcache` structures, and
it shows up in any per-CPU metrics library (Prometheus client libraries,
statsd shard pools) that needs a hot counter under real multi-core
contention.

The array itself still needs a bounds guard: `Add` and `Get` both reject
`idx` outside `[0, NumShards)` with `ErrShardOutOfRange` before ever
touching `shards[idx]`, for the same reason the status-class counter in the
previous exercise guards its index — an unguarded out-of-range index panics
immediately.

Create `shards.go`:

```go
// Package shardcounters implements cache-line-padded, per-shard atomic
// counters -- the fix for false sharing when a hot counter is incremented
// from every core of a multi-core server.
package shardcounters

import (
	"errors"
	"sync/atomic"
	"unsafe"
)

// NumShards is the number of independent counters, sized like a typical
// per-CPU shard count for a metrics or connection-pool counter.
const NumShards = 16

// cacheLineSize is the assumed CPU cache line size in bytes. Both x86-64
// and arm64 server hardware use 64-byte lines; this is the same constant
// the Go runtime itself hardcodes for mcache and other per-P structures.
const cacheLineSize = 64

// ErrShardOutOfRange means the requested shard index is not in [0, NumShards).
var ErrShardOutOfRange = errors.New("shardcounters: shard index out of range")

// PaddedShard is one atomic counter padded out to a full cache line. When
// an array of these sits side by side in memory, incrementing shard i from
// one CPU core and shard i+1 from another core never touches the same
// cache line, so the two cores' writes do not invalidate each other's
// cache. This is the standard fix for false sharing: pad, don't just pack.
type PaddedShard struct {
	value atomic.Uint64
	_     [cacheLineSize - unsafe.Sizeof(atomic.Uint64{})]byte
}

// UnpaddedShard is the naive version: just the counter, no padding. An
// array of these packs eight 8-byte counters into a single 64-byte cache
// line, so eight logically-independent shards share one physical cache
// line and contend on every write from a different core. Kept here only
// so its unsafe.Sizeof can be compared against PaddedShard's.
type UnpaddedShard struct {
	value atomic.Uint64
}

// Shards is a fixed array of padded, independently-incrementable atomic
// counters -- the same shape as the Go runtime's per-P mcache counters or a
// per-CPU metrics vector. Sharding a hot counter across NumShards slots and
// summing on read trades one bottleneck (every core fighting over one
// counter's cache line) for NumShards cheap, cache-local writes.
//
// Shards is safe for concurrent use by multiple goroutines: every access
// goes through atomic.Uint64, and each shard lives on its own cache line.
type Shards struct {
	shards [NumShards]PaddedShard
}

// Add atomically adds delta to the shard at idx. It returns
// ErrShardOutOfRange, and touches nothing, if idx is not in [0, NumShards).
func (s *Shards) Add(idx int, delta uint64) error {
	if idx < 0 || idx >= NumShards {
		return ErrShardOutOfRange
	}
	s.shards[idx].value.Add(delta)
	return nil
}

// Get atomically reads the shard at idx. It returns ErrShardOutOfRange if
// idx is not in [0, NumShards).
func (s *Shards) Get(idx int) (uint64, error) {
	if idx < 0 || idx >= NumShards {
		return 0, ErrShardOutOfRange
	}
	return s.shards[idx].value.Load(), nil
}

// Total atomically reads and sums every shard. Because each shard is read
// with its own atomic Load, Total never observes a torn write, though two
// calls to Total made while Add is still running concurrently can
// legitimately disagree -- this is a snapshot sum, not a lock-held total.
func (s *Shards) Total() uint64 {
	var total uint64
	for i := range s.shards {
		total += s.shards[i].value.Load()
	}
	return total
}
```

### Using it

`Shards` needs no constructor: its zero value is `NumShards` zeroed, already
cache-line-padded counters, ready for `Add` from any goroutine. A typical
caller assigns each worker goroutine (or each CPU, via `runtime.NumCPU()`
and a round-robin) a fixed shard index and always increments through that
same index, then calls `Total` wherever the aggregate is needed — a
`/metrics` handler, a periodic flush, a shutdown summary.

The module has no `main.go`, because a shard-counter library is a package,
not a tool. Its executable demonstration is `ExampleShards_Add`: `go test`
runs it and compares its standard output against the `// Output:` comment,
so the usage shown below cannot drift away from the code. It launches
`NumShards * 20` goroutines, twenty assigned to each shard, each
incrementing its shard one hundred times, then prints the first and last
shard's final count, the grand total, and the `unsafe.Sizeof` of the padded
and unpadded shard types side by side. Every shard lands on exactly
`goroutinesPerShard * incrementsPerGoroutine = 2000`, deterministically,
because every goroutine only ever touches its own assigned shard and
`wg.Wait()` blocks until all of them finish before anything is printed — no
scheduling nondeterminism can change the final counts.

### Tests

`TestAddAndGet` is a table over the ordinary and boundary shard indices,
including the two illegal ones (`-1` and `NumShards`) that must return
`ErrShardOutOfRange`. `TestGetOutOfRange` checks the same guard from the
read side. `TestConcurrentIncrementsSumCorrectly` is the concurrency case
this exercise exists for: many goroutines hammering every shard at once,
verified under `-race`, checking both the grand `Total` and each individual
shard's exact count — every increment must be accounted for with no lock
beyond the `atomic.Uint64` inside each shard.
`TestPaddedShardFillsCacheLine` pins the `unsafe.Sizeof` claim from the
prose. The two `Benchmark` functions are not part of `go test`'s default
run; they exist to be run with `go test -bench=.` and demonstrate the padded
version's lower per-op cost under real parallel contention, but their
timings are not deterministic and are not asserted on in any test.

Create `shards_test.go`:

```go
package shardcounters

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestAddAndGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		idx   int
		delta uint64
		want  uint64
		err   error
	}{
		{name: "first shard", idx: 0, delta: 5, want: 5},
		{name: "last shard", idx: NumShards - 1, delta: 7, want: 7},
		{name: "middle shard", idx: NumShards / 2, delta: 3, want: 3},
		{name: "negative index", idx: -1, delta: 1, err: ErrShardOutOfRange},
		{name: "index at NumShards", idx: NumShards, delta: 1, err: ErrShardOutOfRange},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var s Shards
			err := s.Add(tc.idx, tc.delta)
			if tc.err != nil {
				if err != tc.err {
					t.Fatalf("Add(%d) err = %v, want %v", tc.idx, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Add(%d) = %v, want nil", tc.idx, err)
			}
			got, err := s.Get(tc.idx)
			if err != nil {
				t.Fatalf("Get(%d) = %v, want nil", tc.idx, err)
			}
			if got != tc.want {
				t.Fatalf("Get(%d) = %d, want %d", tc.idx, got, tc.want)
			}
		})
	}
}

func TestGetOutOfRange(t *testing.T) {
	t.Parallel()

	var s Shards
	if _, err := s.Get(-1); err != ErrShardOutOfRange {
		t.Fatalf("Get(-1) err = %v, want ErrShardOutOfRange", err)
	}
	if _, err := s.Get(NumShards); err != ErrShardOutOfRange {
		t.Fatalf("Get(NumShards) err = %v, want ErrShardOutOfRange", err)
	}
}

// TestConcurrentIncrementsSumCorrectly launches many goroutines that all
// increment shards concurrently and checks the grand total. Run with
// -race: every increment goes through PaddedShard's atomic.Uint64, so the
// race detector must find nothing to report even though NumShards*goroutines
// writers are running at once with no lock.
func TestConcurrentIncrementsSumCorrectly(t *testing.T) {
	t.Parallel()

	const goroutinesPerShard = 50
	const incrementsPerGoroutine = 200

	var s Shards
	var wg sync.WaitGroup

	for shard := 0; shard < NumShards; shard++ {
		for g := 0; g < goroutinesPerShard; g++ {
			wg.Add(1)
			go func(shard int) {
				defer wg.Done()
				for i := 0; i < incrementsPerGoroutine; i++ {
					if err := s.Add(shard, 1); err != nil {
						t.Errorf("Add(%d): %v", shard, err)
					}
				}
			}(shard)
		}
	}
	wg.Wait()

	want := uint64(NumShards * goroutinesPerShard * incrementsPerGoroutine)
	if got := s.Total(); got != want {
		t.Fatalf("Total() = %d, want %d", got, want)
	}

	for shard := 0; shard < NumShards; shard++ {
		got, err := s.Get(shard)
		if err != nil {
			t.Fatalf("Get(%d): %v", shard, err)
		}
		wantShard := uint64(goroutinesPerShard * incrementsPerGoroutine)
		if got != wantShard {
			t.Fatalf("Get(%d) = %d, want %d", shard, got, wantShard)
		}
	}
}

func TestPaddedShardFillsCacheLine(t *testing.T) {
	t.Parallel()

	if got := unsafe.Sizeof(PaddedShard{}); got != cacheLineSize {
		t.Fatalf("unsafe.Sizeof(PaddedShard{}) = %d, want %d", got, cacheLineSize)
	}
	if got := unsafe.Sizeof(UnpaddedShard{}); got >= cacheLineSize {
		t.Fatalf("unsafe.Sizeof(UnpaddedShard{}) = %d, want < %d", got, cacheLineSize)
	}
}

// BenchmarkPaddedShards increments a private, per-goroutine shard on a
// Shards array. Because each shard is cache-line padded, parallel
// goroutines incrementing distinct shards do not invalidate each other's
// cache line.
func BenchmarkPaddedShards(b *testing.B) {
	var s Shards
	var next atomic.Int64

	b.RunParallel(func(pb *testing.PB) {
		shard := int(next.Add(1)-1) % NumShards
		for pb.Next() {
			s.Add(shard, 1)
		}
	})
}

// BenchmarkUnpaddedShards is the false-sharing control: the same workload
// over an array of UnpaddedShard, where several logical shards pack into
// one physical cache line and contend on every concurrent write from a
// different core.
func BenchmarkUnpaddedShards(b *testing.B) {
	var shards [NumShards]UnpaddedShard
	var next atomic.Int64

	b.RunParallel(func(pb *testing.PB) {
		shard := int(next.Add(1)-1) % NumShards
		for pb.Next() {
			shards[shard].value.Add(1)
		}
	})
}

// ExampleShards_Add is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below. It
// increments every shard from many goroutines, waits for all of them to
// finish, then prints the exact resulting counts -- deterministic because
// wg.Wait blocks until every goroutine has touched only its own assigned
// shard.
func ExampleShards_Add() {
	const goroutinesPerShard = 20
	const incrementsPerGoroutine = 100

	var s Shards
	var wg sync.WaitGroup

	for shard := 0; shard < NumShards; shard++ {
		for g := 0; g < goroutinesPerShard; g++ {
			wg.Add(1)
			go func(shard int) {
				defer wg.Done()
				for i := 0; i < incrementsPerGoroutine; i++ {
					_ = s.Add(shard, 1)
				}
			}(shard)
		}
	}
	wg.Wait()

	first, _ := s.Get(0)
	last, _ := s.Get(NumShards - 1)
	fmt.Printf("shard 0: %d\n", first)
	fmt.Printf("shard %d: %d\n", NumShards-1, last)

	want := uint64(NumShards * goroutinesPerShard * incrementsPerGoroutine)
	fmt.Printf("total: %d (want %d, match=%v)\n", s.Total(), want, s.Total() == want)

	fmt.Printf("sizeof(PaddedShard)   = %d bytes\n", unsafe.Sizeof(PaddedShard{}))
	fmt.Printf("sizeof(UnpaddedShard) = %d bytes\n", unsafe.Sizeof(UnpaddedShard{}))

	// Output:
	// shard 0: 2000
	// shard 15: 2000
	// total: 32000 (want 32000, match=true)
	// sizeof(PaddedShard)   = 64 bytes
	// sizeof(UnpaddedShard) = 8 bytes
}
```

`Shards` deliberately does not expose `PaddedShard` or `UnpaddedShard` as
something a caller constructs directly; both are internal building blocks,
and `UnpaddedShard` exists purely as the size-comparison control referenced
in `TestPaddedShardFillsCacheLine`. A caller only ever sees `Shards` itself
and its three methods, which is what keeps the padding detail an
implementation choice rather than something every call site has to get
right on its own.

## Review

The design is correct on two independent axes, and the test suite checks
both. Correctness of the counting itself:
`TestConcurrentIncrementsSumCorrectly` runs `-race`-clean with `NumShards *
50` goroutines hammering every shard at once and both the grand `Total` and
every individual shard land exactly where arithmetic says they should — no
lost updates, because every write goes through `atomic.Uint64.Add`.
Correctness of the *layout*: `TestPaddedShardFillsCacheLine` pins
`unsafe.Sizeof(PaddedShard{}) == 64`, which is what guarantees no two shards
in the `[NumShards]PaddedShard` array can ever share a cache line. The
mistake this design avoids is stopping at "I sharded the counter" and
assuming that alone removes contention — an array of bare `atomic.Uint64`
values is still correct (no lost updates, same as here) but still slow under
real multi-core write pressure, because false sharing operates below the
level any correctness test can see; only a benchmark or a cache-miss
profiler exposes it, which is why `BenchmarkPaddedShards` and
`BenchmarkUnpaddedShards` exist side by side. Run `go test -count=1 -race
./...` to confirm correctness, and `go test -bench=. -benchtime=1x ./...` to
see the two benchmarks execute (their absolute numbers vary by machine and
are not asserted on).

## Resources

- [sync/atomic package](https://pkg.go.dev/sync/atomic) — `atomic.Uint64` and its `Add`/`Load` methods used by every shard.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — the compile-time constant this module uses both to size the padding and to verify it in a test.
- [Go blog: Profiling Go programs](https://go.dev/blog/pprof) — how you would find false sharing in a real service via CPU profiling.
- [Wikipedia: False sharing](https://en.wikipedia.org/wiki/False_sharing) — the cache-coherence mechanics this module's padding works around.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-status-class-counter-array.md](13-status-class-counter-array.md) | Next: [15-latency-reservoir-fixed-array.md](15-latency-reservoir-fixed-array.md)
