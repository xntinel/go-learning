# Exercise 9: The evidence — Mutex vs RWMutex vs COW under controlled read/write ratios

"RWMutex is faster for reads" is folklore until you measure it on your workload.
This exercise builds the harness a senior uses to close that argument: one
`Store` interface, three lock strategies behind it, and a parameterized
`b.RunParallel` benchmark that sweeps read:write ratios so the crossover — where
`RWMutex` stops paying and where copy-on-write starts — is a number in your
terminal, not an opinion in a review thread.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
lockbench/                   independent module: example.com/lockbench
  go.mod                     module example.com/lockbench
  lockbench.go               type Store interface; mutexStore, rwStore, cowStore + constructors
  cmd/
    demo/
      main.go                runnable demo: same feature-flag lookup through all three
  lockbench_test.go          shared conformance test over all three; Benchmark<Impl>/<ratio>
```

Files: `lockbench.go`, `cmd/demo/main.go`, `lockbench_test.go`.
Implement: `Store` (`Get`/`Set` over a small map) three ways — `mutexStore` (`sync.Mutex`), `rwStore` (`sync.RWMutex`), `cowStore` (`atomic.Pointer` to an immutable map snapshot, CAS loop on write).
Test: one conformance test run against all three implementations via a constructor table (concurrent readers and writers under `-race`, exact final state); a COW-specific snapshot-immutability test; benchmarks named `Benchmark<Impl>/<ratio>` at 100:0, 99:1, 90:10 and 50:50 read:write.
Verify: `go test -count=1 -race ./...` then `go test -bench . -benchmem -cpu 8`

### One interface so the benchmark is a fair fight

The three stores implement an identical `Get`/`Set` contract over the same map
shape, so the only variable in the benchmark is the synchronization strategy.
`mutexStore` is the baseline: one `sync.Mutex`, every access exclusive.
`rwStore` swaps it for `sync.RWMutex` — shared `RLock` on `Get`, exclusive
`Lock` on `Set`. `cowStore` abandons read-side locking entirely: readers `Load`
an `atomic.Pointer` to an immutable map snapshot; a writer clones the current
snapshot, applies its change, and publishes with a `CompareAndSwap` loop so
concurrent writers compose instead of clobbering. Each strategy has a cost the
others do not: the `Mutex` serializes readers; the `RWMutex` pays reader-count
atomics on every `RLock`/`RUnlock`; COW pays a full map clone per write plus CAS
retries under write contention.

### The ratio knob, without coordination overhead

The benchmark must mix reads and writes at a controlled ratio *without* the
mixing mechanism itself becoming the bottleneck. A shared `atomic` counter to
decide "is this op a read?" would serialize the very goroutines the benchmark
is trying to run in parallel. Instead, each `RunParallel` worker keeps a plain
local counter: `i++; if i%100 < reads { Get } else { Set }`. It costs nothing,
is perfectly deterministic per goroutine, and produces the exact requested mix
in aggregate. `b.RunParallel` distributes `b.N` across `GOMAXPROCS` goroutines
(scale with `-cpu 1,4,8` to see contention grow with parallelism), and
`testing.PB.Next` hands out iterations from a local cache, so the harness
overhead per op is a few nanoseconds — small enough that lock effects dominate.

Keys cycle through a fixed 64-entry set via `i&63`, keeping map size constant
so COW's per-write clone cost is stable and comparable across ratios.

Create `lockbench.go`:

```go
// Package lockbench compares three synchronization strategies for a small,
// read-mostly map (a feature-flag table, a routing table) behind one Store
// interface, so a parameterized benchmark can expose the crossover points.
package lockbench

import (
	"maps"
	"sync"
	"sync/atomic"
)

// Store is a concurrency-safe string-to-int map: the shape of a feature-flag
// or routing table hot enough to argue about.
type Store interface {
	Get(key string) (int, bool)
	Set(key string, value int)
}

// mutexStore serializes every access, reads included, behind one Mutex.
type mutexStore struct {
	mu sync.Mutex
	m  map[string]int
}

// NewMutexStore returns the sync.Mutex baseline.
func NewMutexStore() Store { return &mutexStore{m: make(map[string]int)} }

func (s *mutexStore) Get(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok
}

func (s *mutexStore) Set(key string, value int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
}

// rwStore lets readers share the lock; writers are exclusive.
type rwStore struct {
	mu sync.RWMutex
	m  map[string]int
}

// NewRWStore returns the sync.RWMutex variant.
func NewRWStore() Store { return &rwStore{m: make(map[string]int)} }

func (s *rwStore) Get(key string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}

func (s *rwStore) Set(key string, value int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
}

// cowStore keeps an immutable map snapshot behind an atomic pointer: reads are
// lock-free; each write clones the snapshot and publishes it with a CAS loop.
type cowStore struct {
	ptr atomic.Pointer[map[string]int]
}

// NewCOWStore returns the copy-on-write variant.
func NewCOWStore() Store {
	s := &cowStore{}
	m := make(map[string]int)
	s.ptr.Store(&m)
	return s
}

func (s *cowStore) Get(key string) (int, bool) {
	m := *s.ptr.Load()
	v, ok := m[key]
	return v, ok
}

func (s *cowStore) Set(key string, value int) {
	for {
		old := s.ptr.Load()
		next := maps.Clone(*old)
		next[key] = value
		if s.ptr.CompareAndSwap(old, &next) {
			return
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lockbench"
)

func main() {
	impls := []struct {
		name  string
		store lockbench.Store
	}{
		{"mutex", lockbench.NewMutexStore()},
		{"rwmutex", lockbench.NewRWStore()},
		{"cow", lockbench.NewCOWStore()},
	}
	for _, impl := range impls {
		impl.store.Set("feature.checkout.v2", 1)
		v, ok := impl.store.Get("feature.checkout.v2")
		fmt.Printf("%-8s feature.checkout.v2=%d found=%v\n", impl.name, v, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mutex    feature.checkout.v2=1 found=true
rwmutex  feature.checkout.v2=1 found=true
cow      feature.checkout.v2=1 found=true
```

### The conformance test: all three must be equally correct

A benchmark comparing a correct implementation against a subtly racy one is
worthless, so one table-driven test gates all three identically: four writers
hammer disjoint key ranges for fifty rounds while readers poll, and afterwards
every key must hold its final-round value exactly. The COW-specific test pins
the property the strategy depends on: a snapshot pointer loaded *before* a
`Set` must be completely unaffected by it — if that fails, a "writer" mutated a
published snapshot in place instead of cloning.

Create `lockbench_test.go`:

```go
package lockbench

import (
	"fmt"
	"sync"
	"testing"
)

func TestStoreConformance(t *testing.T) {
	t.Parallel()
	impls := []struct {
		name string
		new  func() Store
	}{
		{"mutex", NewMutexStore},
		{"rwmutex", NewRWStore},
		{"cow", NewCOWStore},
	}
	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()
			s := impl.new()
			const writers, keysPerWriter, rounds = 4, 8, 50

			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for r := range rounds {
						for k := range keysPerWriter {
							s.Set(fmt.Sprintf("w%d-k%d", w, k), r)
						}
					}
				}()
			}
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 500 {
						if v, ok := s.Get("w0-k0"); ok && (v < 0 || v >= rounds) {
							t.Errorf("impossible value observed: %d", v)
							return
						}
					}
				}()
			}
			wg.Wait()

			for w := range writers {
				for k := range keysPerWriter {
					key := fmt.Sprintf("w%d-k%d", w, k)
					if v, ok := s.Get(key); !ok || v != rounds-1 {
						t.Fatalf("%s = %d,%v after all writers finished, want %d,true", key, v, ok, rounds-1)
					}
				}
			}
		})
	}
}

func TestCOWSnapshotImmutable(t *testing.T) {
	t.Parallel()
	s := NewCOWStore().(*cowStore)
	s.Set("flag", 1)

	snapshot := s.ptr.Load() // a reader's view, taken before the write

	s.Set("flag", 2)
	s.Set("other", 9)

	if v := (*snapshot)["flag"]; v != 1 {
		t.Fatalf("published snapshot mutated: flag = %d, want 1", v)
	}
	if _, ok := (*snapshot)["other"]; ok {
		t.Fatal("published snapshot grew a key written after it was loaded")
	}
	if v, _ := s.Get("flag"); v != 2 {
		t.Fatalf("live store = %d, want 2", v)
	}
}

func ExampleNewRWStore() {
	s := NewRWStore()
	s.Set("feature.dark-mode", 1)
	v, ok := s.Get("feature.dark-mode")
	fmt.Println(v, ok)
	// Output: 1 true
}

var ratios = []struct {
	name  string
	reads int // reads per 100 operations
}{
	{"reads100", 100},
	{"reads99", 99},
	{"reads90", 90},
	{"reads50", 50},
}

func benchStore(b *testing.B, newStore func() Store) {
	keys := make([]string, 64)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%02d", i)
	}
	for _, ratio := range ratios {
		b.Run(ratio.name, func(b *testing.B) {
			s := newStore()
			for i, k := range keys {
				s.Set(k, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0 // per-goroutine counter: the ratio knob costs nothing shared
				for pb.Next() {
					i++
					k := keys[i&63]
					if i%100 < ratio.reads {
						s.Get(k)
					} else {
						s.Set(k, i)
					}
				}
			})
		})
	}
}

func BenchmarkMutex(b *testing.B)   { benchStore(b, NewMutexStore) }
func BenchmarkRWMutex(b *testing.B) { benchStore(b, NewRWStore) }
func BenchmarkCOW(b *testing.B)     { benchStore(b, NewCOWStore) }
```

Run the sweep:

```bash
go test -bench . -benchmem -cpu 8
```

### Reading the table

A real run of this harness on an Apple M4 (Go 1.26, `go test -bench . -benchmem
-cpu 1,8`) produced this — your absolute numbers will differ, and part of the
lesson is that even the *shape* can surprise you:

```
BenchmarkMutex/reads100          8.429 ns/op       0 B/op
BenchmarkMutex/reads100-8       79.04 ns/op        0 B/op
BenchmarkMutex/reads99           8.247 ns/op       0 B/op
BenchmarkMutex/reads99-8        75.13 ns/op        0 B/op
BenchmarkMutex/reads90           9.189 ns/op       0 B/op
BenchmarkMutex/reads90-8        80.70 ns/op        0 B/op
BenchmarkMutex/reads50          10.24 ns/op        0 B/op
BenchmarkMutex/reads50-8        85.65 ns/op        0 B/op
BenchmarkRWMutex/reads100        8.445 ns/op       0 B/op
BenchmarkRWMutex/reads100-8     86.01 ns/op        0 B/op
BenchmarkRWMutex/reads99         8.894 ns/op       0 B/op
BenchmarkRWMutex/reads99-8      40.07 ns/op        0 B/op
BenchmarkRWMutex/reads90         8.918 ns/op       0 B/op
BenchmarkRWMutex/reads90-8      39.59 ns/op        0 B/op
BenchmarkRWMutex/reads50        11.14 ns/op        0 B/op
BenchmarkRWMutex/reads50-8      67.51 ns/op        0 B/op
BenchmarkCOW/reads100            6.475 ns/op       0 B/op
BenchmarkCOW/reads100-8          2.048 ns/op       0 B/op
BenchmarkCOW/reads99            11.25 ns/op       35 B/op
BenchmarkCOW/reads99-8          12.29 ns/op       88 B/op
BenchmarkCOW/reads90            51.94 ns/op      355 B/op
BenchmarkCOW/reads90-8          94.50 ns/op      806 B/op
BenchmarkCOW/reads50           236.7 ns/op     1775 B/op
BenchmarkCOW/reads50-8         471.0 ns/op     3981 B/op
```

Four conclusions fall out, and two of them contradict folklore. First, at
`-cpu 1` — no contention — `RWMutex` never beats the plain `Mutex`: the
sections are single map lookups and the reader-count bookkeeping is pure
overhead, the "not a free upgrade" warning from the concepts file in numbers.
Second, under 8-way contention `RWMutex` earns its keep at the read-heavy
ratios (99:1 and 90:10 run about twice as fast as the `Mutex`, 40 vs 75-80
ns/op), and here it even holds a diminished edge at 50:50 — a machine- and
version-dependent result you would not have predicted from the rule of thumb;
on other hardware the 50:50 row flips. Third, look at `reads100-8`: with *zero*
writers, `RWMutex` does no better than the `Mutex` (86 vs 79 ns/op), because
eight cores hammering `RLock`/`RUnlock` are all fighting over the same
reader-count cache line — reader "sharing" is not free parallelism when the
section is this short. Fourth, COW is in a different universe on reads (2 ns at
`-cpu 8`, a bare atomic load that scales *up* with cores instead of down) and
collapses under writes: at 50:50 every second op clones a 64-entry map and
fights a CAS, landing 5-6x slower than the boring `Mutex`, with the allocation
column telling the garbage-collection story. The deliverable of this exercise
is not the table — it is the one-line conclusion you write after running it at
your own workload's ratio, parallelism, and section length, e.g. "at our 99:1
flag-lookup ratio, COW reads are 40x cheaper and the write cost is irrelevant;
use COW" or "our sections are single lookups on a two-core box; keep the plain
Mutex".

## Review

The harness is only as honest as its construction, and the mistakes that
invalidate it are all quiet. Deciding read-vs-write through a shared atomic
counter serializes the workers and flattens every difference — the
per-goroutine counter exists precisely to keep the ratio knob off the shared
path. Forgetting `b.ResetTimer()` after seeding charges setup to the first
implementation measured. Benchmarking only at `-cpu 8` hides that contention
effects grow with parallelism — sweep `-cpu 1,4,8` before believing a number.
And benchmarking an incorrect store proves nothing, which is why the
conformance test gates all three implementations identically and the
COW-immutability test pins the property the lock-free read depends on. Confirm
correctness with `go test -count=1 -race ./...`, then produce your table with
`go test -bench . -benchmem -cpu 1,4,8` and write the one-line conclusion for
your service's real ratio — that line, with the table behind it, is what ends
the code-review argument.

## Resources

- [`testing.B.RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — parallel benchmark loops distributed across GOMAXPROCS goroutines.
- [`testing.PB`](https://pkg.go.dev/testing#PB) — the per-goroutine iterator whose `Next` keeps harness overhead off the measurement.
- [`sync/atomic` `Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — the lock-free snapshot publication COW builds on.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the contract and costs this harness puts on trial.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-sync-once/00-concepts.md](../03-sync-once/00-concepts.md)
