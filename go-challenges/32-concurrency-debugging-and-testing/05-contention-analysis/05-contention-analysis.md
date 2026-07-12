# 5. Contention Analysis

A program can be correct — no data races, no deadlocks — and still perform poorly because goroutines spend most of their time waiting for a lock rather than doing work. Mutex contention is the silent performance killer in concurrent Go programs. This lesson builds a contended key-value store, profiles it with Go's mutex and block profilers, then applies two successive optimizations — `sync.RWMutex` and lock sharding — and measures the improvement with benchmarks. The goal is to connect the profiler output to the code that causes it.

```text
kvstore/
  go.mod
  store.go
  store_test.go
  cmd/demo/
    main.go
```

## Concepts

### Why Profiling Matters

Without measurement, optimization is guessing. A single `sync.Mutex` protecting all reads and writes serializes every goroutine: under read-heavy load, readers block each other even though reads are safe to overlap. The question is not "is there contention?" (there always is under load) but "which lock site is the bottleneck and by how much?"

### Block Profiler and Mutex Profiler

Go provides two profilers for synchronization:

**Block profiler** (`runtime.SetBlockProfileRate(n)`) records goroutine blocking events: channel operations and `sync.Mutex`/`sync.RWMutex` waits. It captures the duration and stack trace of each blocking event. Rate 1 captures every event; higher values sample one event per n nanoseconds.

**Mutex profiler** (`runtime.SetMutexProfileFraction(n)`) records mutex contention specifically: how long goroutines waited before acquiring a mutex. Fraction 1 captures every contention event.

Both produce profiles in the `pprof` format, readable with `go tool pprof`. For tests, pass `-mutexprofile=mutex.prof` or `-blockprofile=block.prof` to `go test`.

### `sync.RWMutex` for Read-Heavy Workloads

`sync.Mutex` serializes all operations including reads. `sync.RWMutex` allows multiple concurrent readers but only one writer. Under an 80/20 read/write split, switching from `Mutex` to `RWMutex` can improve throughput significantly because reads no longer block each other.

The trade-off: `RLock/RUnlock` has higher overhead than `Lock/Unlock` per call (it must coordinate the reader count). For write-heavy workloads the improvement disappears or inverts.

### Lock Sharding

A single `RWMutex` protecting a large map is still a single serialization point. Lock sharding divides the map into N independent shards, each with its own lock. Two goroutines operating on keys in different shards never contend. The number of shards trades contention against memory and complexity: 16–256 shards is a common range.

The shard for a key is determined by hashing the key modulo N. The `hash/fnv` package provides a fast non-cryptographic hash suitable for this purpose.

## Exercises

### Exercise 1: Three Store Implementations

Create `store.go`:

```go
package kvstore

import (
	"hash/fnv"
	"sync"
)

// MutexStore protects all operations with a single sync.Mutex.
// Under concurrent read load all goroutines serialize, even though
// reads do not modify data.
type MutexStore struct {
	mu   sync.Mutex
	data map[string]string
}

// NewMutexStore creates an empty MutexStore.
func NewMutexStore() *MutexStore {
	return &MutexStore{data: make(map[string]string)}
}

// Set stores val for key.
func (s *MutexStore) Set(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = val
}

// Get returns the value for key and whether it was found.
func (s *MutexStore) Get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// RWStore protects reads with an RLock and writes with a full Lock.
// Concurrent readers do not block each other.
type RWStore struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewRWStore creates an empty RWStore.
func NewRWStore() *RWStore {
	return &RWStore{data: make(map[string]string)}
}

// Set stores val for key.
func (s *RWStore) Set(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = val
}

// Get returns the value for key and whether it was found.
func (s *RWStore) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// ShardedStore partitions keys across numShards independent shards,
// each with its own RWMutex. Goroutines on distinct key ranges never contend.
type ShardedStore struct {
	shards []*rwShard
	n      int
}

type rwShard struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewShardedStore creates a store with numShards independent shards.
func NewShardedStore(numShards int) *ShardedStore {
	if numShards < 1 {
		numShards = 1
	}
	shards := make([]*rwShard, numShards)
	for i := range shards {
		shards[i] = &rwShard{data: make(map[string]string)}
	}
	return &ShardedStore{shards: shards, n: numShards}
}

func (s *ShardedStore) shard(key string) *rwShard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return s.shards[h.Sum32()%uint32(s.n)]
}

// Set stores val for key.
func (s *ShardedStore) Set(key, val string) {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.data[key] = val
}

// Get returns the value for key and whether it was found.
func (s *ShardedStore) Get(key string) (string, bool) {
	sh := s.shard(key)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.data[key]
	return v, ok
}

// NumShards returns the number of shards (used by the demo to inspect config).
func (s *ShardedStore) NumShards() int { return s.n }
```

### Exercise 2: Tests and Benchmarks

Create `store_test.go`:

```go
package kvstore

import (
	"fmt"
	"sync"
	"testing"
)

// store is the common interface exercised by all three implementations.
type store interface {
	Set(key, val string)
	Get(key string) (string, bool)
}

// TestRoundTrip verifies all three implementations store and retrieve correctly.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	impls := []struct {
		name string
		s    store
	}{
		{"MutexStore", NewMutexStore()},
		{"RWStore", NewRWStore()},
		{"ShardedStore-16", NewShardedStore(16)},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()
			impl.s.Set("hello", "world")
			val, ok := impl.s.Get("hello")
			if !ok {
				t.Fatal("Get: key not found")
			}
			if val != "world" {
				t.Fatalf("Get = %q, want %q", val, "world")
			}
			if _, ok := impl.s.Get("missing"); ok {
				t.Fatal("Get: expected false for missing key")
			}
		})
	}
}

// TestConcurrentAccess runs readers and writers concurrently to verify correctness.
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	impls := []struct {
		name string
		s    store
	}{
		{"MutexStore", NewMutexStore()},
		{"RWStore", NewRWStore()},
		{"ShardedStore-32", NewShardedStore(32)},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()

			const (
				keys    = 20
				readers = 50
				writers = 10
			)
			var wg sync.WaitGroup

			// Seed initial values.
			for i := 0; i < keys; i++ {
				impl.s.Set(fmt.Sprintf("key-%d", i), "init")
			}

			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < keys; i++ {
						impl.s.Set(fmt.Sprintf("key-%d", i), fmt.Sprintf("val-%d-%d", w, i))
					}
				}(w)
			}
			for r := 0; r < readers; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < keys; i++ {
						impl.s.Get(fmt.Sprintf("key-%d", i))
					}
				}()
			}
			wg.Wait()
		})
	}
}

// runBench is the shared benchmark body: 80% reads, 20% writes.
func runBench(b *testing.B, s store) {
	b.Helper()
	const keys = 64
	for i := 0; i < keys; i++ {
		s.Set(fmt.Sprintf("key-%d", i), "seed")
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i%keys)
			if i%5 == 0 {
				s.Set(key, "updated")
			} else {
				s.Get(key)
			}
			i++
		}
	})
}

// BenchmarkMutexStore measures throughput with a single Mutex.
func BenchmarkMutexStore(b *testing.B) {
	runBench(b, NewMutexStore())
}

// BenchmarkRWStore measures throughput with RWMutex (readers do not block each other).
func BenchmarkRWStore(b *testing.B) {
	runBench(b, NewRWStore())
}

// BenchmarkShardedStore16 measures throughput with 16 shards.
func BenchmarkShardedStore16(b *testing.B) {
	runBench(b, NewShardedStore(16))
}

// BenchmarkShardedStore64 measures throughput with 64 shards.
func BenchmarkShardedStore64(b *testing.B) {
	runBench(b, NewShardedStore(64))
}

// ExampleMutexStore_Set shows basic MutexStore usage.
func ExampleMutexStore_Set() {
	s := NewMutexStore()
	s.Set("key", "value")
	val, ok := s.Get("key")
	_ = ok
	_ = val
	// Output:
}
```

Your turn: add `BenchmarkShardedStore256` following the same pattern. Run `go test -bench=. -benchmem ./...` and observe how ns/op changes as shards increase. At what point does the improvement flatten?

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/kvstore"
)

type storeIface interface {
	Set(key, val string)
	Get(key string) (string, bool)
}

func benchmark(name string, s storeIface, readers, writers, keys int, dur time.Duration) {
	var (
		ops  int64
		mu   sync.Mutex
		wg   sync.WaitGroup
		stop = make(chan struct{})
	)
	for i := 0; i < keys; i++ {
		s.Set(fmt.Sprintf("k%d", i), "seed")
	}
	worker := func(read bool) {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("k%d", i%keys)
			if read {
				s.Get(key)
			} else {
				s.Set(key, "v")
			}
			mu.Lock()
			ops++
			mu.Unlock()
			i++
		}
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go worker(true)
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go worker(false)
	}
	time.Sleep(dur)
	close(stop)
	wg.Wait()
	fmt.Printf("%-22s readers=%d writers=%d ops=%d ops/ms=%.0f\n",
		name, readers, writers, ops, float64(ops)/float64(dur.Milliseconds()))
}

func main() {
	const (
		readers = 8
		writers = 2
		keys    = 64
		dur     = 200 * time.Millisecond
	)
	benchmark("MutexStore", kvstore.NewMutexStore(), readers, writers, keys, dur)
	benchmark("RWStore", kvstore.NewRWStore(), readers, writers, keys, dur)
	benchmark("ShardedStore-16", kvstore.NewShardedStore(16), readers, writers, keys, dur)
	benchmark("ShardedStore-64", kvstore.NewShardedStore(64), readers, writers, keys, dur)
}
```

## Common Mistakes

### Using `sync.Mutex` for Read-Heavy Workloads

Wrong: protecting a read-heavy cache with `sync.Mutex`.

What happens: readers serialize even though multiple reads are safe in parallel. Throughput is limited to one goroutine at a time regardless of CPU count.

Fix: use `sync.RWMutex`. Callers must use `RLock`/`RUnlock` for reads and `Lock`/`Unlock` for writes. Mixing `Lock` on read paths negates the benefit.

### Making All Calls Use `Lock` on an `RWMutex`

Wrong:

```go
func (s *RWStore) Get(key string) (string, bool) {
	s.mu.Lock()   // should be RLock
	defer s.mu.Unlock()
	return s.data[key], true
}
```

What happens: readers block each other identically to `sync.Mutex`; the `RWMutex` provides no benefit.

Fix: use `s.mu.RLock()` / `s.mu.RUnlock()` on all read paths.

### Too Few or Too Many Shards

Wrong: 1 shard (equivalent to no sharding) or 10000 shards (map allocation overhead exceeds benefit).

What happens: too few shards leave cross-key contention; too many shards waste memory and hurt cache locality.

Fix: start with 16 or 32 shards, benchmark, and increase only if the profile shows contention persisting. Shard count should be a power of two for efficient masking if you use bitwise operations instead of modulo.

### Profiling Without Resetting the Rate Afterward

Wrong: calling `runtime.SetMutexProfileFraction(1)` in a test binary's `init()` without resetting it.

What happens: the profiler adds overhead to every mutex acquisition in the process, including those in imported packages, for the lifetime of the binary.

Fix: set the rate in a `TestMain` or specific test, capture the profile, and reset with `runtime.SetMutexProfileFraction(0)` after the profiling window closes.

## Verification

From `~/go-exercises/kvstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=. -benchmem -count=3 ./...
go run ./cmd/demo
```

The benchmark output should show `BenchmarkRWStore` with higher ns/op improvement over `BenchmarkMutexStore` under the 80/20 read/write split, and `BenchmarkShardedStore64` higher still. The demo prints ops/ms for each implementation.

## Summary

- Correct concurrent code can still perform poorly due to mutex contention; profile before optimizing.
- `runtime.SetBlockProfileRate(1)` and `runtime.SetMutexProfileFraction(1)` capture all blocking and contention events respectively; use `go tool pprof` to analyze.
- `sync.RWMutex` allows concurrent readers; use `RLock`/`RUnlock` on all read paths.
- Lock sharding divides a large map into independent shards, each with its own lock, eliminating cross-key contention.
- Benchmark with `b.RunParallel` and `-benchmem` to measure real throughput and allocation cost.

## What's Next

Next: [Goroutine Dump Analysis](../06-goroutine-dump-analysis/06-goroutine-dump-analysis.md).

## Resources

- [runtime.SetBlockProfileRate](https://pkg.go.dev/runtime#SetBlockProfileRate)
- [runtime.SetMutexProfileFraction](https://pkg.go.dev/runtime#SetMutexProfileFraction)
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [Go Blog: Profiling Go Programs](https://go.dev/blog/pprof)
- [hash/fnv](https://pkg.go.dev/hash/fnv)
