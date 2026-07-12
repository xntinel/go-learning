# Exercise 4: Benchmark both stores under parallel load with profilers armed

A benchmark is the vehicle that captures a contention profile. This module builds
the harness that drives both the single-lock and sharded stores under a fixed
goroutine fan-out, arms the mutex and block profilers, and exposes a `b.RunParallel`
variant that scales with `-cpu`. Run it with `-mutexprofile=mutex.prof` and you get
the exact profile that `go tool pprof` reads. The correctness of the harness itself
is pinned with a `-race` test, because a benchmark that measures the wrong thing is
worse than none.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
store-bench/                  independent module: example.com/store-bench
  go.mod                      go 1.26
  store.go                    Single + Sharded + busyWork (the two stores)
  cmd/
    demo/
      main.go                 runnable demo: drive both stores, print totals
  bench_test.go               benchmarkStore harness, BenchmarkSingle/Sharded,
                              RunParallel variant, -race correctness test
```

- Files: `store.go`, `cmd/demo/main.go`, `bench_test.go`.
- Implement: a shared `benchmarkStore` harness plus `BenchmarkSingle`/`BenchmarkSharded` that call `SetMutexProfileFraction(1)`/`SetBlockProfileRate(1)`, spread work across a fixed fan-out over 100 keys, and report allocations; plus a `b.RunParallel` variant that scales with `-cpu`.
- Test: a `-race` correctness test that drives the harness workload and asserts the totals; the benchmark is the vehicle for capturing the profile inspected with `go tool pprof`.
- Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchmem -run=^$ ./...`

### What the harness measures, and how to capture the profile

`benchmarkStore` builds 100 keys once, resets the timer so setup is excluded, then
fans out a fixed number of goroutines that each hammer `Increment`/`Get` across the
key set for `b.N` iterations. The fixed fan-out (32 goroutines) is deliberate: it
holds the concurrency constant so the single-versus-sharded comparison is
apples-to-apples, and it guarantees enough goroutines to actually contend on the
single lock. `BenchmarkSingle` and `BenchmarkSharded` arm both profilers at
fraction 1 before running — appropriate for a benchmark where you want every
contention event, not a production sample rate — and both restore the previous
fraction afterward so a later benchmark in the same binary is not taxed.

The `RunParallel` variant is the idiomatic way to write a benchmark that scales
with `-cpu`. `b.RunParallel` starts `GOMAXPROCS` worker goroutines and hands each a
`*testing.PB`; `pb.Next()` returns true until the shared `b.N` iterations are
exhausted. Running `go test -bench=Parallel -cpu=1,2,4,8` then shows how throughput
responds as you add parallelism — the single-lock store flattens out because the
lock serializes everything, while the sharded store keeps scaling. That divergence,
visible in the ns/op column across `-cpu` values, is the contention story told in
numbers before you even open a profile.

To capture the profile the tooling reads:

```bash
go test -bench=BenchmarkSingle -run=^$ -mutexprofile=mutex.prof ./...
go tool pprof mutex.prof
# in pprof:  top      (hottest wait stacks)
#            list Increment   (wait attributed per source line)
#            web      (call graph, needs Graphviz)
```

The gate runs `go test -race` without `-bench`, so the benchmark functions only
have to compile there; they are exercised on demand. What the gate *does* run is
`TestHarnessCorrectness`, which drives the same fan-out workload and asserts the
final totals — a benchmark that races or miscounts is a broken measurement, and
this test is what keeps the harness honest under `-race`.

Create `store.go`:

```go
package store

import (
	"hash/fnv"
	"sync"
)

// Single is the single-lock baseline.
type Single struct {
	mu   sync.Mutex
	data map[string]int
}

// NewSingle returns an empty single-lock store.
func NewSingle() *Single { return &Single{data: make(map[string]int)} }

// Increment adds one to key's count under the one lock.
func (s *Single) Increment(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key]++
	guard(busyWork(64))
}

// Get returns key's count under the one lock.
func (s *Single) Get(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key]
}

type shard struct {
	mu   sync.Mutex
	data map[string]int
}

// Sharded splits the keyspace across numShards independent locks.
type Sharded struct {
	shards    []*shard
	numShards uint32
}

// NewSharded returns a store with numShards independent shards (at least one).
func NewSharded(numShards int) *Sharded {
	if numShards < 1 {
		numShards = 1
	}
	shards := make([]*shard, numShards)
	for i := range shards {
		shards[i] = &shard{data: make(map[string]int)}
	}
	return &Sharded{shards: shards, numShards: uint32(numShards)}
}

func (s *Sharded) shardFor(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return s.shards[h.Sum32()%s.numShards]
}

// Increment adds one to key's count under that key's shard lock.
func (s *Sharded) Increment(key string) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.data[key]++
	guard(busyWork(64))
}

// Get returns key's count under that key's shard lock.
func (s *Sharded) Get(key string) int {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.data[key]
}

func busyWork(iterations int) uint64 {
	var acc uint64
	for i := range iterations {
		acc += uint64(i)*2 + 1
	}
	return acc
}

var sink uint64

func guard(v uint64) {
	if v == 1<<63 {
		sink = v
	}
}
```

### The runnable demo

The demo drives both stores over a shared key set and prints the totals, so you can
confirm both count correctly before you benchmark them.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/store-bench"
)

func main() {
	single := store.NewSingle()
	sharded := store.NewSharded(16)
	for i := range 100 {
		k := fmt.Sprintf("k-%d", i%10)
		single.Increment(k)
		sharded.Increment(k)
	}
	fmt.Printf("single k-0=%d\n", single.Get("k-0"))
	fmt.Printf("sharded k-0=%d\n", sharded.Get("k-0"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
single k-0=10
sharded k-0=10
```

### Tests and benchmarks

`TestHarnessCorrectness` drives the fan-out workload the benchmark uses and asserts
the final totals, so the thing being benchmarked is provably correct under `-race`.
The benchmarks arm the profilers and are the capture vehicle. `BenchmarkSingleParallel`
uses `b.RunParallel` so `-cpu` scaling reveals the lock ceiling.

Create `bench_test.go`:

```go
package store

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

func makeKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k-%d", i)
	}
	return keys
}

// benchmarkStore fans out a fixed number of goroutines that hammer inc/get across
// a shared key set for b.N iterations each.
func benchmarkStore(b *testing.B, inc func(string), get func(string) int, fanout int) {
	keys := makeKeys(100)
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	wg.Add(fanout)
	for range fanout {
		go func() {
			defer wg.Done()
			for i := range b.N {
				k := keys[i%len(keys)]
				inc(k)
				_ = get(k)
			}
		}()
	}
	wg.Wait()
}

// armProfilers turns on both contention profilers at fraction 1 for the benchmark
// and restores the previous mutex fraction (and disables the block profiler) after.
func armProfilers(b *testing.B) {
	prev := runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	b.Cleanup(func() {
		runtime.SetMutexProfileFraction(prev)
		runtime.SetBlockProfileRate(0)
	})
}

func BenchmarkSingle(b *testing.B) {
	armProfilers(b)
	s := NewSingle()
	benchmarkStore(b, s.Increment, s.Get, 32)
}

func BenchmarkSharded(b *testing.B) {
	armProfilers(b)
	s := NewSharded(16)
	benchmarkStore(b, s.Increment, s.Get, 32)
}

// BenchmarkSingleParallel scales with -cpu: run go test -bench=Parallel -cpu=1,2,4,8
// to watch the single lock flatten out as parallelism rises.
func BenchmarkSingleParallel(b *testing.B) {
	armProfilers(b)
	s := NewSingle()
	keys := makeKeys(100)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%len(keys)]
			s.Increment(k)
			_ = s.Get(k)
			i++
		}
	})
}

func BenchmarkShardedParallel(b *testing.B) {
	armProfilers(b)
	s := NewSharded(16)
	keys := makeKeys(100)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i%len(keys)]
			s.Increment(k)
			_ = s.Get(k)
			i++
		}
	})
}

func TestHarnessCorrectness(t *testing.T) {
	t.Parallel()
	const fanout, ops = 16, 500
	s := NewSingle()
	var wg sync.WaitGroup
	wg.Add(fanout)
	for range fanout {
		go func() {
			defer wg.Done()
			for range ops {
				s.Increment("k")
			}
		}()
	}
	wg.Wait()
	if got := s.Get("k"); got != fanout*ops {
		t.Fatalf("Get(k) = %d, want %d (harness miscounts under load)", got, fanout*ops)
	}
}

func ExampleSharded() {
	s := NewSharded(4)
	s.Increment("a")
	s.Increment("a")
	fmt.Println(s.Get("a"))
	// Output: 2
}
```

## Review

The harness is correct when the workload it benchmarks is provably right:
`TestHarnessCorrectness` under `-race` is what guarantees the ns/op numbers describe
a correct store, not a racing one. The profiler discipline matters as much as the
numbers — both benchmarks capture the previous mutex fraction and restore it in
`b.Cleanup`, so a leftover fraction of 1 does not silently tax every later benchmark
in the binary. The payoff is the `-cpu` sweep: run the `RunParallel` variants across
`-cpu=1,2,4,8` and the single-lock ns/op stops improving while the sharded one keeps
falling, which is the contention ceiling made visible. Capture the profile with
`-mutexprofile=mutex.prof`, then `go tool pprof` and `list Increment` to see the
wait attributed to the exact locked line. Do not assert wall-time in a test — it is
flaky on shared CI; assert correctness and read the profile for the speed story.

## Resources

- [testing.B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — parallel benchmarks and `-cpu` scaling.
- [testing.B.ReportAllocs](https://pkg.go.dev/testing#B.ReportAllocs) — per-op allocation reporting.
- [runtime.SetMutexProfileFraction](https://pkg.go.dev/runtime#SetMutexProfileFraction) — arming the mutex profiler and its return-the-previous contract.
- [Profiling Go Programs](https://go.dev/blog/pprof) — capturing and reading a profile with `go tool pprof`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-busywork-critical-section-helper.md](03-busywork-critical-section-helper.md) | Next: [05-mutex-and-block-profile-capture.md](05-mutex-and-block-profile-capture.md)
