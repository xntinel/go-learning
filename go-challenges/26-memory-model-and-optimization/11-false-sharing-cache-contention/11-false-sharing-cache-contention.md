# 11. False Sharing and Cache Contention

False sharing is a performance bug in otherwise correct concurrent code. Two goroutines can update different variables with no data race and still slow each other down because those variables occupy the same hardware cache line. This lesson builds a sharded counter library that separates correctness from layout, then verifies its padding and validation behavior with tests.

```text
cachepad/
  go.mod
  counter.go
  counter_test.go
  cmd/demo/main.go
```

## Concepts

### False Sharing Is Contention On A Cache Line

Modern CPUs move memory through cache lines, commonly 64 bytes on mainstream server and desktop hardware. If two cores repeatedly write different words that live on the same cache line, the cache coherence protocol must move ownership of that line between cores. The program is logically race-free, but the hardware serializes the writes at the cache-line level.

### Atomics Fix Races, Not Layout

`sync/atomic` provides low-level atomic operations with sequentially consistent behavior in Go's memory model. That makes concurrent increments correct, but it does not guarantee that adjacent atomic counters live on separate cache lines. Correctness and scalability are different properties.

### Padding Is A Local Layout Choice

Padding adds unused bytes so each frequently written counter occupies its own cache-line-sized slot. This can improve multi-core write throughput, but it increases memory use and is architecture-sensitive. Padding should be limited to hot fields that measurements show are contended.

### Benchmarks Detect The Scaling Shape

A benchmark for false sharing should compare contiguous slots against padded slots while varying goroutine count. The exact numbers depend on CPU, OS, and load. The important signal is scaling shape: contiguous counters often plateau or degrade under parallel writes, while padded counters usually scale better until another bottleneck dominates.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/cachepad/cmd/demo
cd ~/go-exercises/cachepad
go mod init cachepad
```

This is a library package. The demo program imports the library and uses only exported identifiers.

### Exercise 1: Build Sharded Counters

Create `counter.go`:

```go
package cachepad

import (
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

const CacheLineSize = 64

var (
	ErrInvalidShardCount = errors.New("shard count must be positive")
	ErrInvalidShardIndex = errors.New("shard index out of range")
)

type ContiguousCounter struct {
	shards []atomic.Uint64
}

type PaddedCounter struct {
	shards []paddedSlot
}

type paddedSlot struct {
	value atomic.Uint64
	pad   [CacheLineSize - 8]byte
}

func NewContiguous(shards int) (*ContiguousCounter, error) {
	if shards <= 0 {
		return nil, fmt.Errorf("cachepad: %w: got %d", ErrInvalidShardCount, shards)
	}
	return &ContiguousCounter{shards: make([]atomic.Uint64, shards)}, nil
}

func NewPadded(shards int) (*PaddedCounter, error) {
	if shards <= 0 {
		return nil, fmt.Errorf("cachepad: %w: got %d", ErrInvalidShardCount, shards)
	}
	return &PaddedCounter{shards: make([]paddedSlot, shards)}, nil
}

func (c *ContiguousCounter) Add(shard int, delta uint64) error {
	if c == nil || shard < 0 || shard >= len(c.shards) {
		return fmt.Errorf("cachepad: %w: got %d", ErrInvalidShardIndex, shard)
	}
	c.shards[shard].Add(delta)
	return nil
}

func (c *ContiguousCounter) Value() uint64 {
	if c == nil {
		return 0
	}
	var total uint64
	for i := range c.shards {
		total += c.shards[i].Load()
	}
	return total
}

func (c *PaddedCounter) Add(shard int, delta uint64) error {
	if c == nil || shard < 0 || shard >= len(c.shards) {
		return fmt.Errorf("cachepad: %w: got %d", ErrInvalidShardIndex, shard)
	}
	c.shards[shard].value.Add(delta)
	return nil
}

func (c *PaddedCounter) Value() uint64 {
	if c == nil {
		return 0
	}
	var total uint64
	for i := range c.shards {
		total += c.shards[i].value.Load()
	}
	return total
}

func PaddedSlotSize() uintptr {
	return unsafe.Sizeof(paddedSlot{})
}
```

Both counter types are correct. The difference is memory layout: `ContiguousCounter` keeps atomic values next to each other, while `PaddedCounter` gives each hot value a 64-byte slot.

### Exercise 2: Test Correctness, Layout, And Validation

Create `counter_test.go`:

```go
package cachepad

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

func TestCountersAddAndSum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		new  func(int) (interface {
			Add(int, uint64) error
			Value() uint64
		}, error)
	}{
		{name: "contiguous", new: func(n int) (interface {
			Add(int, uint64) error
			Value() uint64
		}, error) {
			return NewContiguous(n)
		}},
		{name: "padded", new: func(n int) (interface {
			Add(int, uint64) error
			Value() uint64
		}, error) {
			return NewPadded(n)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			counter, err := tt.new(4)
			if err != nil {
				t.Fatal(err)
			}
			for shard := 0; shard < 4; shard++ {
				if err := counter.Add(shard, uint64(shard+1)); err != nil {
					t.Fatal(err)
				}
			}
			if got := counter.Value(); got != 10 {
				t.Fatalf("Value = %d, want 10", got)
			}
		})
	}
}

func TestCountersRejectInvalidShardCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		new  func(int) error
	}{
		{name: "contiguous", new: func(n int) error { _, err := NewContiguous(n); return err }},
		{name: "padded", new: func(n int) error { _, err := NewPadded(n); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.new(0); !errors.Is(err, ErrInvalidShardCount) {
				t.Fatalf("err = %v, want ErrInvalidShardCount", err)
			}
		})
	}
}

func TestCountersRejectInvalidShardIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		new  func() (interface{ Add(int, uint64) error }, error)
	}{
		{name: "contiguous", new: func() (interface{ Add(int, uint64) error }, error) { return NewContiguous(2) }},
		{name: "padded", new: func() (interface{ Add(int, uint64) error }, error) { return NewPadded(2) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			counter, err := tt.new()
			if err != nil {
				t.Fatal(err)
			}
			for _, shard := range []int{-1, 2} {
				if err := counter.Add(shard, 1); !errors.Is(err, ErrInvalidShardIndex) {
					t.Fatalf("Add(%d) err = %v, want ErrInvalidShardIndex", shard, err)
				}
			}
		})
	}
}

func TestPaddedSlotIsCacheLineSized(t *testing.T) {
	t.Parallel()

	if got := PaddedSlotSize(); got != CacheLineSize {
		t.Fatalf("PaddedSlotSize = %d, want %d", got, CacheLineSize)
	}
	if got := unsafe.Alignof(paddedSlot{}); got < unsafe.Alignof(atomicUint64ForTest{}) {
		t.Fatalf("paddedSlot alignment = %d, want at least atomic alignment", got)
	}
}

func TestPaddedCounterConcurrentAdds(t *testing.T) {
	t.Parallel()

	const shards = 4
	const perShard = 1000
	counter, err := NewPadded(shards)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for shard := 0; shard < shards; shard++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			for i := 0; i < perShard; i++ {
				if err := counter.Add(shard, 1); err != nil {
					t.Errorf("Add(%d): %v", shard, err)
				}
			}
		}(shard)
	}
	wg.Wait()

	if got := counter.Value(); got != shards*perShard {
		t.Fatalf("Value = %d, want %d", got, shards*perShard)
	}
}

func ExampleNewPadded() {
	counter, _ := NewPadded(2)
	_ = counter.Add(0, 3)
	_ = counter.Add(1, 4)
	fmt.Println(counter.Value())
	// Output: 7
}

type atomicUint64ForTest struct {
	value uint64
}
```

The tests are table-driven where the contract is shared by both implementations. `errors.Is` proves the validation errors remain stable even though they are wrapped with context.

### Exercise 3: Add A Demo That Uses The Padded Counter

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"cachepad"
)

func main() {
	counter, err := cachepad.NewPadded(4)
	if err != nil {
		log.Fatal(err)
	}
	for shard := 0; shard < 4; shard++ {
		if err := counter.Add(shard, 10); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("value=%d slot-size=%d\n", counter.Value(), cachepad.PaddedSlotSize())
}
```

For a benchmark extension, add `BenchmarkContiguousCounter` and `BenchmarkPaddedCounter` with sub-benchmarks for different goroutine counts. Keep benchmark conclusions machine-specific unless you have repeated measurements.

## Common Mistakes

### Expecting The Race Detector To Find False Sharing

Wrong: assuming `go test -race` will report cache-line contention.

Fix: use the race detector for data races and benchmarks or hardware counters for contention. False sharing is a performance issue in race-free code.

### Padding Every Struct Field

Wrong: adding 56 bytes after every `uint64` in a codebase.

Fix: pad only hot, independently written fields where measurement shows contention. Padding increases memory use and can reduce cache locality for read-heavy data.

### Copying Atomic Values After First Use

Wrong: copying a counter struct that contains `atomic.Uint64` values after goroutines have started using it.

Fix: keep counters behind pointers and avoid copying them. The constructors in this lesson return pointers to make that ownership clearer.

## Verification

Run this from `~/go-exercises/cachepad`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that creates a `NewContiguous(3)` counter, calls `Add(2, 9)`, and verifies `Value() == 9`.

## Summary

- False sharing happens when independent writes contend on the same cache line.
- Atomic operations make increments race-free, but they do not guarantee scalable layout.
- Padding can separate hot counters into cache-line-sized slots at the cost of memory.
- Tests verify correctness and layout; benchmarks verify whether padding helps on a specific machine.

## What's Next

Next: [Zero-Allocation Patterns](../12-zero-allocation-patterns/12-zero-allocation-patterns.md).

## Resources

- [sync/atomic package](https://pkg.go.dev/sync/atomic)
- [The Go Memory Model](https://go.dev/ref/mem)
- [Go Specification: Size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees)
- [Mechanical Sympathy: False Sharing](https://mechanical-sympathy.blogspot.com/2011/07/false-sharing.html)
