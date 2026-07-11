# Exercise 5: Fix Race with Atomic

For simple numeric state -- counters, flags, gauges -- `sync/atomic` offers a
lock-free alternative to a mutex: the operation is a single CPU instruction that
completes without interruption from other cores, so there is no lock to acquire,
no goroutine to block, and no lock to deadlock on. The trade-off is scope. Atomics
work only for scalar types, so the rule of thumb is a single counter or flag gets
an atomic, a complex struct or multi-field update gets a mutex, and communication
between goroutines gets a channel. This exercise takes the racy `count++` and
replaces it the right way, then builds it up into a real server stats tracker
where every metric is its own atomic.

## What you'll build

```text
05-fix-race-with-atomic/
  main.go        an atomic hit counter, a multi-metric server stats tracker,
                 and a mutex-vs-channel-vs-atomic benchmark
```

- Build: three programs -- a race-free hit counter, a `ServerStats` tracker of four independent metrics, and a head-to-head comparison of the three synchronization approaches.
- Implement: `atomic.Int64` with `.Add()`, `.Load()`, and `.Add(-1)`; a `ServerStats` struct exposing `HandleRequest`/`ConnectionOpened`/`ConnectionClosed`/`Report`; and `MutexBench`/`ChannelBench`/`AtomicBench` runners.
- Verify: `go run -race main.go` (Steps 1 and 2 must be race-clean)

### Why atomics fit server metrics

In a real server, you track simple metrics: total requests served, bytes sent,
active connections, errors. Each is a single integer that gets incremented or
decremented. These are the perfect use case for atomics: independent scalars,
updated under contention, with no invariant tying them together, so each can
have its own `atomic.Int64` and never contend with the others for a lock.

## Step 1 -- Fix the Hit Counter with Atomic

Replace the racy `hitCount++` with an atomic operation:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func atomicHitCounter() int64 {
	var hitCount atomic.Int64
	var wg sync.WaitGroup

	for handler := 0; handler < 100; handler++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := 0; req < 100; req++ {
				hitCount.Add(1)
			}
		}()
	}

	wg.Wait()
	return hitCount.Load()
}

func main() {
	result := atomicHitCounter()
	fmt.Printf("Hit count: %d (expected 10000)\n", result)
}
```

Key details:
- `atomic.Int64` is the modern Go type-safe wrapper (Go 1.19+)
- `.Add(1)` atomically increments the counter: the entire read-modify-write is a single CPU instruction
- `.Load()` atomically reads the final value
- No lock, no channel, no blocking: the fastest option for a simple counter

### Verification
```bash
go run -race main.go
```
Expected: 10000 with zero race warnings.

## Step 2 -- Build a Server Stats Tracker

Build a real server stats tracker using multiple atomic values. This is what production Go servers use to expose metrics at `/debug/vars` or to Prometheus:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ServerStats struct {
	TotalRequests     atomic.Int64
	TotalBytesSent    atomic.Int64
	ActiveConnections atomic.Int64
	TotalErrors       atomic.Int64
}

func (s *ServerStats) HandleRequest(bytesWritten int64, isError bool) {
	s.TotalRequests.Add(1)
	s.TotalBytesSent.Add(bytesWritten)
	if isError {
		s.TotalErrors.Add(1)
	}
}

func (s *ServerStats) ConnectionOpened() {
	s.ActiveConnections.Add(1)
}

func (s *ServerStats) ConnectionClosed() {
	s.ActiveConnections.Add(-1)
}

func (s *ServerStats) Report() string {
	return fmt.Sprintf(
		"requests=%d bytes_sent=%d active_conns=%d errors=%d",
		s.TotalRequests.Load(),
		s.TotalBytesSent.Load(),
		s.ActiveConnections.Load(),
		s.TotalErrors.Load(),
	)
}

func main() {
	stats := &ServerStats{}
	var wg sync.WaitGroup

	// Simulate 50 concurrent connections.
	for conn := 0; conn < 50; conn++ {
		wg.Add(1)
		go func(connID int) {
			defer wg.Done()
			stats.ConnectionOpened()
			defer stats.ConnectionClosed()

			// Each connection processes 100 requests.
			for req := 0; req < 100; req++ {
				isError := req%20 == 0 // 5% error rate
				bytesWritten := int64(256 + req%512)
				stats.HandleRequest(bytesWritten, isError)
			}
		}(conn)
	}

	// Print stats while requests are in flight.
	fmt.Println("=== Server Stats (live) ===")
	for i := 0; i < 3; i++ {
		time.Sleep(1 * time.Millisecond)
		fmt.Printf("  [snapshot] %s\n", stats.Report())
	}

	wg.Wait()

	fmt.Println()
	fmt.Println("=== Server Stats (final) ===")
	fmt.Printf("  %s\n", stats.Report())
	fmt.Println()
	fmt.Printf("  Expected: requests=5000, active_conns=0, errors=250\n")
	fmt.Printf("  (bytes_sent varies by request size)\n")
}
```

Key design points:
- Each metric is an independent `atomic.Int64`: no lock contention between unrelated counters
- `ActiveConnections` uses `.Add(-1)` for decrement: atomics handle both directions
- `Report()` reads all values atomically, though the snapshot is not transactional (each Load is independent)
- Live snapshots work safely because every read is atomic

### Verification
```bash
go run -race main.go
```
Expected: 5000 total requests, 0 active connections at the end, 250 errors, zero race warnings. The live snapshots show counters being updated in real time.

## Step 3 -- Grand Comparison of All Three Approaches

Compare mutex, channel, and atomic side by side on the same counter problem:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	comparisonWorkers    = 100
	comparisonIncrements = 10000
	channelBufferSize    = 256
)

// ComparisonResult holds the outcome of a single synchronization benchmark.
type ComparisonResult struct {
	Label   string
	Value   int64
	Elapsed time.Duration
}

// MutexBench benchmarks mutex-based counter increments.
type MutexBench struct{}

func (MutexBench) Run(workers, increments int) ComparisonResult {
	counter := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				mu.Lock()
				counter++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return ComparisonResult{"Mutex", int64(counter), time.Since(start)}
}

// ChannelBench benchmarks channel-based counter increments.
type ChannelBench struct{}

func (ChannelBench) Run(workers, increments int) ComparisonResult {
	inc := make(chan struct{}, channelBufferSize)
	done := make(chan int)

	go func() {
		counter := 0
		for range inc {
			counter++
		}
		done <- counter
	}()

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				inc <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(inc)
	result := <-done
	return ComparisonResult{"Channel", int64(result), time.Since(start)}
}

// AtomicBench benchmarks atomic counter increments.
type AtomicBench struct{}

func (AtomicBench) Run(workers, increments int) ComparisonResult {
	var counter atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				counter.Add(1)
			}
		}()
	}
	wg.Wait()
	return ComparisonResult{"Atomic", counter.Load(), time.Since(start)}
}

func printResults(results []ComparisonResult) {
	for _, r := range results {
		fmt.Printf("  %-10s %d in %v\n", r.Label+":", r.Value, r.Elapsed)
	}
	fmt.Println()
	fmt.Println("Decision Guide:")
	fmt.Println("  atomic  -> single counter, flag, or gauge")
	fmt.Println("  mutex   -> map, struct, multi-field update")
	fmt.Println("  channel -> ownership transfer, pipeline, command pattern")
}

func main() {
	fmt.Println("=== Grand Comparison: 100 goroutines x 10000 increments ===")
	fmt.Println()

	results := []ComparisonResult{
		MutexBench{}.Run(comparisonWorkers, comparisonIncrements),
		ChannelBench{}.Run(comparisonWorkers, comparisonIncrements),
		AtomicBench{}.Run(comparisonWorkers, comparisonIncrements),
	}

	printResults(results)
}
```

### Verification
```bash
go run main.go
```

Typical ordering: **atomic < mutex < channel** for simple counter operations.

| Approach | Speed | Complexity | Best For |
|----------|-------|------------|----------|
| `atomic` | Fastest | Simple types only | Counters, flags, single values |
| `mutex` | Medium | Any type | Complex structs, multi-field updates |
| `channel` | Slowest | Communication | Ownership transfer, pipelines |

## Common Mistakes

### Using Regular Reads with Atomic Writes
```go
var counter atomic.Int64
counter.Add(1)           // atomic write
fmt.Println(counter)     // BUG: prints the struct, not the value
```
**Fix:** Always use `.Load()` to read: `fmt.Println(counter.Load())`.

### Using Atomic for Complex State
```go
var total atomic.Int64
var count atomic.Int64
total.Add(amount)
count.Add(1)
// BUG: another goroutine can read total and count between these two operations
// The average (total/count) may be calculated with mismatched values.
```
**Fix:** Use a mutex to protect multi-variable updates when the values must be consistent with each other.

### Thinking Atomic Operations Compose
Each atomic operation is individually atomic, but a **sequence** of atomic operations is NOT atomic as a whole:

```go
var counter atomic.Int64
val := counter.Load()    // step 1: atomic read
val++                    // step 2: local compute
counter.Store(val)       // step 3: atomic write
// RACE: another goroutine can modify counter between steps 1 and 3!

// USE THIS INSTEAD:
counter.Add(1) // single atomic operation
```

### Overusing Atomics for Readability
Code with many atomic operations scattered across a large struct can be harder to reason about than a single mutex. If you have more than 4-5 related atomic fields, consider whether a mutex with clear locking scope would be clearer.

## Review

`sync/atomic` gives lock-free operations for scalar types, and the modern
type-safe API (`atomic.Int64`, Go 1.19+) exposes `.Add()`, `.Load()`, and
`.Store()` on a value that cannot accidentally be read or written non-atomically
-- which is why it is preferred over the older free-function `atomic.AddInt64`
style that leaves the raw `int64` exposed. For a single counter, flag, or gauge
it is the fastest tool available, and a server stats tracker with one
`atomic.Int64` per metric gets zero-contention updates because unrelated counters
never share a lock. The one law you cannot violate: atomic operations do not
compose. Each is individually atomic, but a sequence is not, so
`counter.Store(counter.Load() + 1)` has a gap between the load and the store
where another goroutine can slip in and lose an update -- `counter.Add(1)` is a
single indivisible step and the only correct increment. That composition limit is
also the boundary of the whole technique: the moment two values must stay
consistent with each other, atomics are the wrong tool and a mutex is right.

Confirm you can answer the exercise's own checkpoints without the code: that the
atomic version is clean under `go run -race main.go`; why `atomic.Int64` beats
the `atomic.AddInt64(&counter, 1)` style; when you would pick `sync/atomic` over
`sync.Mutex`; and why `counter.Store(counter.Load() + 1)` is not equivalent to
`counter.Add(1)`. The decision rule to carry away is short -- atomic for single
counters and flags, mutex for complex or multi-field state, channels for
communication.

## Resources

- [sync/atomic Package](https://pkg.go.dev/sync/atomic) -- the full set of atomic types and operations, including the `Int64` methods used here.
- [Go Memory Model: Synchronization](https://go.dev/ref/mem#synchronization) -- the happens-before guarantees atomic operations provide across goroutines.
- [Go 1.19 Release Notes: atomic types](https://go.dev/doc/go1.19#atomic_types) -- the release that introduced the type-safe `atomic.Int64` and friends.

---

Back to [Concurrency](../../concurrency.md) | Next: [06-subtle-race-map-access](../06-subtle-race-map-access/06-subtle-race-map-access.md)
