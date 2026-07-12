# 1. Race Condition Reproduction

Go's race detector surfaces data races that pass every functional test yet corrupt state in production. This lesson focuses on four concrete race patterns — read-write, map, slice-append, and struct-field — and on the precise question: what output does the detector produce and why? The hard part is not enabling `-race`; it is reading the two-stack report accurately enough to identify the conflicting accesses and choose the right fix.

```text
racedemo/
  go.mod
  counter/
    counter.go
    counter_test.go
  cmd/demo/
    main.go
```

## Concepts

### The Go Memory Model and Happens-Before

The Go memory model (go.dev/ref/mem) defines when one goroutine's write is guaranteed to be visible to another goroutine's read. If no synchronization event establishes a happens-before edge between a write and a read of the same variable, the two accesses form a data race. The program's behavior is then undefined: it may produce wrong values, corrupt internal data structures, or crash. The model is precise: "If package p imports package q, the completion of q's init functions happens before the start of any of p's init functions" — but two goroutines modifying the same variable without synchronization have no such guarantee.

### ThreadSanitizer and the Race Detector

`go build -race` and `go test -race` instrument every memory load and store with calls into ThreadSanitizer (TSan), which maintains a shadow memory that records the last access per memory word. When a read and a write (or two writes) from different goroutines arrive without an intervening synchronization event, TSan reports a DATA RACE with two full stack traces — one for the current access and one for the earlier conflicting access — plus the goroutine creation sites.

The cost is approximately 2-20x slower and 5-10x more memory. Run `-race` in CI but not in production unless you need live detection.

### Four Race Patterns

**Read-write race.** One goroutine reads a variable while another writes it. The most common pattern; often invisible because the racy value is just stale rather than corrupt.

**Map concurrent access.** The Go runtime's built-in map is not concurrency-safe. Concurrent writes (or a concurrent read and write) trigger a `fatal error: concurrent map writes` or `concurrent map read and map write` at runtime — this is a runtime panic, not just a race detector warning, and it fires even without `-race`.

**Slice-append race.** `append` modifies the slice header (pointer, length, capacity). Two goroutines appending to the same slice variable race on the header. Even if the backing array is large enough that no reallocation occurs, the length update is unsynchronized.

**Struct-field race.** Races on distinct fields of a struct are independent at the hardware level but still races from Go's memory-model perspective. The race detector reports them; the fix is the same (synchronize all accesses to the struct).

### Choosing the Fix

| Pattern | Preferred fix |
| --- | --- |
| Simple counter | `sync/atomic` (`atomic.Int64`) |
| Shared struct/slice | `sync.Mutex` protecting all reads and writes |
| Producer/consumer | Channel (data ownership transfer) |
| Read-heavy map | `sync.Map` or `sync.RWMutex`-protected `map` |
| Slice results collection | Buffered channel or mutex-protected slice |

Do not reach for `sync/atomic` for compound operations (check-then-act, read-modify-write across multiple variables). `atomic` is correct only for a single-word load/store/add that stands alone.

### Reading the Race Report

```
WARNING: DATA RACE
Write at 0x00c0000b4010 by goroutine 7:
  racedemo/counter.(*Counter).Inc()
      counter/counter.go:18 +0x38

Previous read at 0x00c0000b4010 by goroutine 8:
  racedemo/counter.(*Counter).Value()
      counter/counter.go:22 +0x28

Goroutine 7 (running) created at:
  racedemo/counter.TestRace()
      counter/counter_test.go:14 +0xa4
```

The first block names the goroutine and line that made the current access. The second block names the previous conflicting access. The third block shows where each goroutine was created. File and line numbers are exact; there is no ambiguity about which variables are racing.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/32-concurrency-debugging-and-testing/01-race-condition-reproduction/01-race-condition-reproduction/counter
mkdir -p go-solutions/32-concurrency-debugging-and-testing/01-race-condition-reproduction/01-race-condition-reproduction/cmd/demo
cd go-solutions/32-concurrency-debugging-and-testing/01-race-condition-reproduction/01-race-condition-reproduction
```

### Exercise 1: A Counter With a Race

Create `counter/counter.go`:

```go
package counter

import "sync"

// Counter is a concurrency-safe integer counter.
type Counter struct {
	mu  sync.Mutex
	val int64
}

// Inc increments the counter by one.
func (c *Counter) Inc() {
	c.mu.Lock()
	c.val++
	c.mu.Unlock()
}

// Value returns the current count.
func (c *Counter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// UnsafeInc increments without any synchronization. It races.
func (c *Counter) UnsafeInc() {
	c.val++ // DATA RACE: no lock held
}

// UnsafeValue reads without any synchronization. It races.
func (c *Counter) UnsafeValue() int64 {
	return c.val // DATA RACE: no lock held
}
```

The safe methods hold the mutex for the full read-modify-write cycle. `UnsafeInc` and `UnsafeValue` are intentionally racy; they exist so the tests below can demonstrate detector output without needing a separate file.

### Exercise 2: Tests That Pin Both Racy and Safe Behavior

Create `counter/counter_test.go`:

```go
package counter

import (
	"sync"
	"testing"
)

// TestSafeCounterRace runs 100 goroutines incrementing via the safe path.
// The race detector must not report any warnings.
func TestSafeCounterRace(t *testing.T) {
	t.Parallel()

	var c Counter
	var wg sync.WaitGroup
	const n = 100

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	if got := c.Value(); got != n {
		t.Fatalf("Value() = %d, want %d", got, n)
	}
}

// TestUnsafeCounterDetected verifies the unsafe path is racy.
// Run this test with -race to observe the detector output;
// under normal go test it may or may not produce a wrong result.
// The race detector will exit non-zero when it fires, so this test
// is kept separate so CI can run -race on the safe tests only.
func TestUnsafeIncDoesNotPanic(t *testing.T) {
	t.Parallel()

	// Running UnsafeInc concurrently is a race. This test does NOT
	// run it concurrently — it exercises the method on a single goroutine
	// to verify the method compiles and is reachable. The race is
	// demonstrated in the Example below (sequential for reproducibility).
	var c Counter
	c.UnsafeInc()
	if c.UnsafeValue() != 1 {
		t.Fatalf("UnsafeValue = %d, want 1 (sequential, no race)", c.UnsafeValue())
	}
}

// TestCounterZeroValue verifies a zero-value Counter starts at zero.
func TestCounterZeroValue(t *testing.T) {
	t.Parallel()

	var c Counter
	if v := c.Value(); v != 0 {
		t.Fatalf("Value() = %d, want 0 for zero Counter", v)
	}
}

// TestCounterTableDriven verifies Inc accumulates correctly.
func TestCounterTableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		incs int
		want int64
	}{
		{"zero increments", 0, 0},
		{"one increment", 1, 1},
		{"ten increments", 10, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c Counter
			for i := 0; i < tt.incs; i++ {
				c.Inc()
			}
			if got := c.Value(); got != tt.want {
				t.Fatalf("Value() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ExampleCounter_Inc shows the basic counter usage.
func ExampleCounter_Inc() {
	var c Counter
	c.Inc()
	c.Inc()
	c.Inc()
	_ = c.Value() // 3
	// Output:
}
```

Your turn: add `TestConcurrentCounterFinalValue` that launches `N` goroutines each calling `Inc` once, waits for all of them, then asserts `Value() == N`. Use `t.Parallel()` and pick `N = 500`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/racedemo/counter"
)

func main() {
	var c counter.Counter
	var wg sync.WaitGroup

	const workers = 50
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	fmt.Printf("safe counter after %d goroutines: %d\n", workers, c.Value())

	// Demonstrate map concurrent access detection (without -race, this may panic).
	// Uncomment to see the runtime fatal error from concurrent map writes:
	//
	//   m := make(map[int]int)
	//   for i := 0; i < 100; i++ {
	//       go func(n int) { m[n] = n }(i)
	//   }
}
```

## Common Mistakes

### Not Using `-race` in CI

Wrong: running `go test ./...` without `-race` and assuming the tests prove race-freedom.

What happens: a racy program can pass thousands of tests because the race depends on goroutine scheduling timing that the test environment happens not to trigger.

Fix: always run `go test -race ./...` in CI. The detector adds latency but catches races that functional tests miss.

### Using `atomic` for Compound Operations

Wrong:

```go
if atomic.LoadInt64(&c.val) == 0 {
	atomic.StoreInt64(&c.val, 1) // race: another goroutine may have stored between Load and Store
}
```

What happens: the check-then-act is not atomic as a unit. Another goroutine can interleave between the Load and the Store.

Fix: use `sync.Mutex` for any compound read-modify-write. `atomic` is correct only for standalone single-word operations.

### Closing a Channel From Multiple Goroutines

Wrong:

```go
close(done) // if two goroutines call this, panic: close of closed channel
```

What happens: a channel may only be closed once. Closing it twice panics, and two goroutines closing it is also a data race on the channel's internal state.

Fix: designate exactly one goroutine (typically the producer) as responsible for closing the channel.

### Assuming Different Struct Fields Do Not Race

Wrong: believing that `goroutine A writes c.x` and `goroutine B writes c.y` is safe because they are different fields.

What happens: the Go memory model does not guarantee independence for struct fields accessed concurrently without synchronization. The race detector will report it.

Fix: use one mutex per struct (or per shard if the struct is wide and contention matters).

## Verification

From `~/go-exercises/racedemo`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must produce no errors. `go test -race` is the primary check: it must exit 0 with no `WARNING: DATA RACE` lines. The demo prints the final counter value and exits cleanly.

## Summary

- The Go memory model defines correctness; two unsynchronized accesses where at least one is a write form a data race with undefined behavior.
- `-race` instruments every memory access and reports conflicting accesses with full goroutine stacks, including creation sites.
- Concurrent map writes cause a runtime panic even without `-race`; this is distinct from a data race.
- Slice-append races occur on the slice header (pointer + length), not just the backing array.
- Fix patterns: `atomic.Int64` for standalone counters; `sync.Mutex` for compound operations and structs; channels for producer-consumer transfer of data ownership.
- Always run `go test -race` in CI.

## What's Next

Next: [Goroutine Leak Detection](../02-goroutine-leak-detection-goleak/02-goroutine-leak-detection-goleak.md).

## Resources

- [The Go Memory Model](https://go.dev/ref/mem)
- [Data Race Detector](https://go.dev/doc/articles/race_detector)
- [sync/atomic package](https://pkg.go.dev/sync/atomic)
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector)
