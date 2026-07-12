# Exercise 10: Benchmark: RWMutex Snapshot Reads vs atomic.Pointer Under Contention

The concepts file claims `RLock`/`RUnlock` bounce a cache line between reader
cores while an atomic pointer load scales flat. A senior engineer does not
repeat that claim — they measure it. This exercise builds two implementations
of the same `ConfigSource` interface, proves them behaviorally identical with
one shared correctness suite, and benchmarks them under `b.RunParallel`
contention, including a realistic 99-percent-read/1-percent-write mix.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgbench/                  independent module: example.com/cfgbench
  go.mod
  source.go                ConfigSource interface; MutexSource (RWMutex + copy);
                           AtomicSource (atomic.Pointer load)
  cmd/
    demo/
      main.go              runnable demo: both sources behave identically
  source_test.go           one shared torn-read suite run against both; parallel benchmarks
```

- Files: `source.go`, `cmd/demo/main.go`, `source_test.go`.
- Implement: `ConfigSource` with `Snapshot() Config` and `Update(Config)`; `MutexSource` copying the struct under `RLock`; `AtomicSource` dereferencing an `atomic.Pointer[Config]` load.
- Test: a shared consistency suite (writer stores `{v,v,v}`, readers assert the three fields always agree — a torn read breaks the invariant) run table-driven against both implementations under `-race`; then `BenchmarkSnapshot` and `BenchmarkMixed99Read1Write` with `b.RunParallel` and `b.ReportAllocs`.
- Verify: `go test -count=1 -race ./...` then `go test -bench . -benchmem`

### Measuring the claim instead of asserting it

Both implementations expose value semantics — `Snapshot() Config` returns a
copy — so callers cannot tell them apart and the benchmark compares exactly
the synchronization strategy, nothing else.

`MutexSource` is the textbook approach: an `RWMutex` guarding a struct,
readers copying it out under `RLock`. Correct, simple — and every `RLock`
and `RUnlock` is an atomic read-modify-write on the lock word. On a
multi-core box, that word lives in one cache line that every reader core
must acquire in exclusive mode to update the reader count, so the line
ping-pongs between cores and readers serialize on cache-coherence traffic
even though they never logically block each other.

`AtomicSource` holds an `atomic.Pointer[Config]`; `Snapshot` is one atomic
load (a plain load with acquire ordering on mainstream hardware — no store,
no exclusive cache-line acquisition) plus a struct copy. Readers share the
cache line in shared mode and scale flat. The cost moves to the writer:
`Update` heap-allocates a fresh `Config` per call, where the mutex version
overwrites in place. That is the trade the benchmark should make visible:
read-side scaling bought with write-side allocation.

Two benchmark-design details matter more than the numbers. First,
`b.RunParallel` distributes `pb.Next` iterations across `GOMAXPROCS`
goroutines — contention is the point, so a single-goroutine benchmark of a
lock is close to meaningless (an uncontended `RLock` is just a couple of
atomic ops and looks cheap). Second, the pure-read benchmark flatters the
atomic version; the honest comparison is the mixed workload, here 1 write
per 99 reads per goroutine, which also charges the atomic version its
allocation bill (`b.ReportAllocs` makes it visible in the `allocs/op`
column).

The correctness suite runs *first* and runs against both implementations
through the interface — benchmarking two things that do not behave
identically is comparing apples to a crashed process. The torn-read detector
is worth stealing for other code: the writer only ever stores configs with
all three fields equal to the same `v`, so any snapshot where the fields
disagree proves a reader observed half an update. (Replace either
implementation with an unsynchronized struct and this suite fails under
`-race` immediately.)

Create `source.go`:

```go
// Package cfgbench compares two snapshot-read strategies for hot config —
// RWMutex-guarded copy vs atomic pointer load — behind one interface, so
// the same correctness suite and benchmarks drive both.
package cfgbench

import (
	"sync"
	"sync/atomic"
)

// Config is one configuration snapshot. Scalars only, so a struct copy is
// cheap and the benchmark isolates synchronization cost.
type Config struct {
	MaxConnections int
	TimeoutMillis  int
	Version        int
}

// ConfigSource serves consistent config snapshots to concurrent readers.
type ConfigSource interface {
	// Snapshot returns a consistent copy of the current config.
	Snapshot() Config
	// Update replaces the current config.
	Update(Config)
}

// MutexSource guards the config with an RWMutex; every Snapshot copies
// the struct under RLock. Correct everywhere, but every reader performs
// two atomic read-modify-writes on the shared lock word.
type MutexSource struct {
	mu  sync.RWMutex
	cfg Config
}

// NewMutexSource returns a MutexSource serving initial.
func NewMutexSource(initial Config) *MutexSource {
	return &MutexSource{cfg: initial}
}

// Snapshot copies the current config under a read lock.
func (s *MutexSource) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update overwrites the config in place under the write lock.
func (s *MutexSource) Update(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// AtomicSource publishes immutable snapshots behind an atomic pointer;
// Snapshot is one atomic load plus a struct copy, with no shared write.
// Update pays instead: one heap allocation per call.
type AtomicSource struct {
	ptr atomic.Pointer[Config]
}

// NewAtomicSource returns an AtomicSource serving initial.
func NewAtomicSource(initial Config) *AtomicSource {
	s := &AtomicSource{}
	s.ptr.Store(&initial)
	return s
}

// Snapshot dereferences the current immutable snapshot.
func (s *AtomicSource) Snapshot() Config {
	return *s.ptr.Load()
}

// Update publishes a fresh immutable snapshot.
func (s *AtomicSource) Update(cfg Config) {
	s.ptr.Store(&cfg)
}
```

### The runnable demo

The demo proves the two sources are drop-in replacements for each other:
same interface, same observable behavior. The performance difference is the
benchmark's job, not the demo's — timing output is machine-dependent and
belongs in `go test -bench`, never in an expected-output block.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfgbench"
)

func main() {
	initial := cfgbench.Config{MaxConnections: 100, TimeoutMillis: 5000, Version: 1}
	next := cfgbench.Config{MaxConnections: 200, TimeoutMillis: 3000, Version: 2}

	sources := []struct {
		name string
		src  cfgbench.ConfigSource
	}{
		{"rwmutex", cfgbench.NewMutexSource(initial)},
		{"atomic ", cfgbench.NewAtomicSource(initial)},
	}

	for _, s := range sources {
		before := s.src.Snapshot()
		s.src.Update(next)
		after := s.src.Snapshot()
		fmt.Printf("%s: v%d max=%d -> v%d max=%d\n",
			s.name, before.Version, before.MaxConnections, after.Version, after.MaxConnections)
	}
	fmt.Println("behaviorally identical; compare cost: go test -bench . -benchmem")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rwmutex: v1 max=100 -> v2 max=200
atomic : v1 max=100 -> v2 max=200
behaviorally identical; compare cost: go test -bench . -benchmem
```

### Tests and benchmarks

The suite is table-driven over both implementations. The consistency test
launches one writer cycling `{v,v,v}` snapshots and eight readers checking
the all-fields-equal invariant on every read; `-race` supervises the whole
thing. The benchmarks then reuse the same constructors, so what you measure
is exactly what you tested.

Create `source_test.go`:

```go
package cfgbench

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func implementations(initial Config) []struct {
	name string
	src  ConfigSource
} {
	return []struct {
		name string
		src  ConfigSource
	}{
		{"rwmutex", NewMutexSource(initial)},
		{"atomic", NewAtomicSource(initial)},
	}
}

func TestUpdateVisible(t *testing.T) {
	t.Parallel()

	for _, tc := range implementations(Config{MaxConnections: 1, TimeoutMillis: 1, Version: 1}) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.src.Update(Config{MaxConnections: 2, TimeoutMillis: 2, Version: 2})
			if got := tc.src.Snapshot(); got.Version != 2 || got.MaxConnections != 2 {
				t.Fatalf("Snapshot after Update = %+v", got)
			}
		})
	}
}

func TestNoTornReadsUnderContention(t *testing.T) {
	t.Parallel()

	for _, tc := range implementations(Config{MaxConnections: 1, TimeoutMillis: 1, Version: 1}) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The writer only ever publishes {v, v, v}. A snapshot whose
			// fields disagree is a torn read: half of one update, half of
			// another.
			var torn atomic.Int64
			stop := make(chan struct{})
			var readers sync.WaitGroup
			for range 8 {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						c := tc.src.Snapshot()
						if c.MaxConnections != c.Version || c.TimeoutMillis != c.Version {
							torn.Add(1)
							return
						}
					}
				}()
			}

			for v := 2; v <= 2000; v++ {
				tc.src.Update(Config{MaxConnections: v, TimeoutMillis: v, Version: v})
			}
			close(stop)
			readers.Wait()

			if n := torn.Load(); n != 0 {
				t.Fatalf("%d torn snapshots observed", n)
			}
			if got := tc.src.Snapshot(); got.Version != 2000 {
				t.Fatalf("final Version = %d, want 2000", got.Version)
			}
		})
	}
}

func BenchmarkSnapshot(b *testing.B) {
	for _, tc := range implementations(Config{MaxConnections: 100, TimeoutMillis: 5000, Version: 1}) {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					c := tc.src.Snapshot()
					if c.Version == 0 {
						b.Fatal("invalid snapshot")
					}
				}
			})
		})
	}
}

func BenchmarkMixed99Read1Write(b *testing.B) {
	for _, tc := range implementations(Config{MaxConnections: 100, TimeoutMillis: 5000, Version: 1}) {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					i++
					if i%100 == 0 {
						tc.src.Update(Config{MaxConnections: i, TimeoutMillis: i, Version: i})
					} else {
						c := tc.src.Snapshot()
						if c.Version == 0 {
							b.Fatal("invalid snapshot")
						}
					}
				}
			})
		})
	}
}

func ExampleConfigSource() {
	var src ConfigSource = NewAtomicSource(Config{MaxConnections: 100, Version: 1})
	src.Update(Config{MaxConnections: 200, Version: 2})
	c := src.Snapshot()
	fmt.Println(c.MaxConnections, c.Version)
	// Output: 200 2
}
```

Run the correctness gate, then the benchmarks:

```bash
go test -count=1 -race ./...
go test -bench . -benchmem
```

Sample benchmark output from an Apple M-series laptop (your absolute numbers
will differ; the *shape* is what to look for):

```
BenchmarkSnapshot/rwmutex-10              15093603    79.95 ns/op     0 B/op    0 allocs/op
BenchmarkSnapshot/atomic-10             1000000000    0.2104 ns/op    0 B/op    0 allocs/op
BenchmarkMixed99Read1Write/rwmutex-10     47728740    26.09 ns/op     0 B/op    0 allocs/op
BenchmarkMixed99Read1Write/atomic-10    1000000000    0.9276 ns/op    0 B/op    0 allocs/op
```

## Review

Reading the numbers is the skill this module teaches. In the pure-read
benchmark, the gap between the two `ns/op` figures is almost entirely
cache-coherence traffic on the RWMutex's lock word — the logical work
(copy three ints) is identical. Watch how the gap *widens* as parallelism
grows: re-run with `GOMAXPROCS=1 go test -bench .` and the mutex looks far
more respectable, which is exactly why single-threaded benchmarks of
synchronization primitives mislead — an uncontended lock is cheap; it is
contention that separates the designs. The mixed benchmark is the honest
one: it charges the atomic version for its per-write allocation — though at
1 write per 100 iterations the average rounds down to `0 allocs/op`; raise
the write fraction (say `i%10`) and the allocation column separates the two
designs — and still shows the read side dominating at 99/1, which is the
actual shape of config workloads.

Do not overgeneralize the result. The measurement says: for read-mostly,
copy-cheap snapshots under multi-core contention, the atomic pointer wins
decisively. Shift any of those qualifiers — write-heavy traffic, a huge
struct you would have to copy, readers that must observe a mutation the
instant it commits alongside other state — and the answer can flip; that is
why the correctness suite runs both implementations behind one interface,
so swapping strategies later is a one-line change backed by the same tests.
Verify with `go test -count=1 -race ./...` before trusting any benchmark.

## Resources

- [testing: B.RunParallel](https://pkg.go.dev/testing#B.RunParallel) — parallel benchmarks and testing.PB.
- [sync: RWMutex](https://pkg.go.dev/sync#RWMutex) — semantics of the reader-writer lock being measured.
- [Russ Cox: Hardware Memory Models](https://research.swtch.com/hwmm) — why a shared writable cache line is the scaling bottleneck.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-config-observability-endpoint.md](09-config-observability-endpoint.md) | Next: [../09-lock-ordering-deadlock-prevention/00-concepts.md](../09-lock-ordering-deadlock-prevention/00-concepts.md)
