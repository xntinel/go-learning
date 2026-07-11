# 10. Arena Allocation Patterns

An arena allocator pre-allocates a large block of memory and hands out slices of it for individual objects. When the entire batch of work is done, the whole arena is reset in O(1) — no GC scanning, no per-object sweeping. The technique is ideal for batch processing, request-scoped allocations, and protocol parsing, where object lifetimes are bounded and uniform. This lesson implements a bump-pointer arena, a pooled arena, and a slab (freelist) allocator in pure Go, then benchmarks them against standard heap allocation and `sync.Pool`.

```text
arena/
  go.mod
  arena.go
  arena_test.go
  cmd/demo/main.go
```

## Concepts

### Bump-Pointer Allocation

The simplest arena: a backing `[]byte` and an integer offset. Allocating n bytes means: return `backing[offset:offset+n]` and advance `offset` by n (with alignment padding). Reset means `offset = 0`. Allocation is O(1) with a single bounds check. Reset is O(1) regardless of how many objects were allocated.

The key property: the backing `[]byte` contains no pointer fields from the GC's perspective. It is an opaque byte slice. The GC only needs to scan the single pointer to the backing array, not every object within it. This makes the GC cost proportional to the number of arenas, not the number of objects they contain.

### Alignment

CPU architectures require that values be accessed at addresses that are multiples of their size (4-byte integers at 4-byte boundaries, 8-byte integers at 8-byte boundaries). The bump pointer must be rounded up to the required alignment before each allocation. In Go, `unsafe.Alignof(T{})` returns the alignment requirement for type T. A portable alignment helper:

```go
func align(offset, alignment int) int {
    return (offset + alignment - 1) &^ (alignment - 1)
}
```

### Typed Arena with Generics

A generic `TypedArena[T]` wraps a `ByteArena` and returns `*T` values. It converts sub-slices of the backing store to typed pointers using `unsafe.Pointer`. This is safe as long as:

1. T contains no unexported fields that the runtime relies on being in a specific location.
2. The arena outlives all `*T` values obtained from it.
3. The arena is not used concurrently without external synchronisation.

### Pooled Arena

A `sync.Pool` of arenas amortises the backing-store allocation across many batch cycles. The pool provides a fresh arena for each batch (or creates a new one if the pool is empty), and returns the arena to the pool after the batch is reset. This is the canonical pattern for request-scoped arenas in a server.

### Slab / Freelist Allocator

A slab allocator maintains a freelist of fixed-size blocks. Allocation pops from the freelist (O(1)); deallocation pushes back (O(1)). Unlike a bump-pointer arena, individual objects can be freed independently. The cost: the freelist pointer occupies space in each block, and deallocation requires the caller to explicitly free rather than a single batch reset. Slab allocators are preferable when objects are freed at different times within a batch.

### When Not to Use Arenas

- When object lifetimes are not bounded by a clear batch boundary. An object allocated from an arena must not outlive the arena reset.
- When the arena needs to be concurrent. The simple bump-pointer arena is not goroutine-safe. Either use one arena per goroutine or add a mutex.
- When objects contain pointers to GC-managed memory that the GC needs to trace. Pointers inside a `[]byte` arena are opaque to the GC; if an arena-allocated object holds a pointer to a GC-managed value, the GC will not see that pointer and may collect the target.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/arena/cmd/demo
cd ~/go-exercises/arena
go mod init example.com/arena
```

### Exercise 1: ByteArena and TypedArena

Create `arena.go`:

```go
package arena

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// ErrOutOfMemory is returned when the arena has insufficient space.
var ErrOutOfMemory = errors.New("arena: out of memory")

// ByteArena is a bump-pointer allocator backed by a pre-allocated []byte slab.
// It is not safe for concurrent use.
type ByteArena struct {
	slab   []byte
	offset int
	allocs int // number of Alloc calls since last Reset
}

// NewByteArena creates a ByteArena with the given capacity in bytes.
func NewByteArena(capacity int) *ByteArena {
	return &ByteArena{slab: make([]byte, capacity)}
}

// alignOffset rounds offset up to the given alignment (must be a power of two).
func alignOffset(offset, alignment int) int {
	return (offset + alignment - 1) &^ (alignment - 1)
}

// Alloc returns a sub-slice of size bytes aligned to alignment.
// alignment must be a power of two and > 0.
// Returns ErrOutOfMemory if the arena does not have enough space.
func (a *ByteArena) Alloc(size, alignment int) ([]byte, error) {
	if alignment <= 0 || alignment&(alignment-1) != 0 {
		alignment = 1
	}
	start := alignOffset(a.offset, alignment)
	end := start + size
	if end > len(a.slab) {
		return nil, ErrOutOfMemory
	}
	a.offset = end
	a.allocs++
	return a.slab[start:end:end], nil
}

// Reset resets the arena, logically freeing all allocations in O(1).
func (a *ByteArena) Reset() {
	a.offset = 0
	a.allocs = 0
}

// Used returns the number of bytes currently allocated.
func (a *ByteArena) Used() int { return a.offset }

// Remaining returns the number of bytes still available.
func (a *ByteArena) Remaining() int { return len(a.slab) - a.offset }

// AllocCount returns the number of Alloc calls since the last Reset.
func (a *ByteArena) AllocCount() int { return a.allocs }

// TypedArena allocates values of type T from an underlying ByteArena.
// T must not contain any GC-managed pointer fields; see the lesson
// for the safety preconditions.
type TypedArena[T any] struct {
	ba *ByteArena
}

// NewTypedArena creates a TypedArena pre-sized for count objects of type T.
func NewTypedArena[T any](count int) *TypedArena[T] {
	var zero T
	size := int(unsafe.Sizeof(zero))
	align := int(unsafe.Alignof(zero))
	// Allocate enough capacity for count objects plus worst-case alignment padding.
	capacity := count*(size+align-1) + align
	return &TypedArena[T]{ba: NewByteArena(capacity)}
}

// Alloc returns a pointer to a zero-initialised T allocated from the arena.
// Returns (nil, ErrOutOfMemory) if the arena is full.
func (a *TypedArena[T]) Alloc() (*T, error) {
	var zero T
	size := int(unsafe.Sizeof(zero))
	align := int(unsafe.Alignof(zero))
	b, err := a.ba.Alloc(size, align)
	if err != nil {
		return nil, err
	}
	// Clear the region (it may have been used by a previous allocation cycle).
	for i := range b {
		b[i] = 0
	}
	//nolint:unsafeptr
	return (*T)(unsafe.Pointer(&b[0])), nil
}

// Reset resets the arena for reuse in O(1).
func (a *TypedArena[T]) Reset() { a.ba.Reset() }

// Used returns bytes currently in use.
func (a *TypedArena[T]) Used() int { return a.ba.Used() }

// block is a fixed-size node used by SlabAllocator.
type block[T any] struct {
	value T
	next  *block[T]
}

// SlabAllocator maintains a freelist of fixed-size blocks of type T.
// Free returns blocks to the freelist; subsequent Alloc calls reuse them.
// It is not safe for concurrent use.
type SlabAllocator[T any] struct {
	freelist *block[T]
	allocs   int
	frees    int
}

// Alloc returns a pointer to a T, reusing a freelist entry if available.
func (s *SlabAllocator[T]) Alloc() *T {
	if s.freelist != nil {
		b := s.freelist
		s.freelist = b.next
		b.next = nil
		var zero T
		b.value = zero // clear before reuse
		s.allocs++
		return &b.value
	}
	b := &block[T]{}
	s.allocs++
	return &b.value
}

// Free returns a previously-allocated *T to the freelist.
// The caller must not use the pointer after calling Free.
func (s *SlabAllocator[T]) Free(p *T) {
	// Recover the block header via pointer arithmetic.
	// This is safe because *T is always the first field of block[T].
	b := (*block[T])(unsafe.Pointer(p))
	b.next = s.freelist
	s.freelist = b
	s.frees++
}

// Outstanding returns the number of Alloc calls minus Free calls.
func (s *SlabAllocator[T]) Outstanding() int { return s.allocs - s.frees }

// PooledArena wraps ByteArena with a sync.Pool so arenas can be reused
// across batch cycles without reallocating the backing store.
type PooledArena struct {
	pool     sync.Pool
	capacity int
}

// NewPooledArena creates a PooledArena that creates ByteArenas of the given
// capacity when the pool is empty.
func NewPooledArena(capacity int) *PooledArena {
	pa := &PooledArena{capacity: capacity}
	pa.pool = sync.Pool{
		New: func() any { return NewByteArena(capacity) },
	}
	return pa
}

// Get returns a ByteArena from the pool, already Reset.
func (pa *PooledArena) Get() *ByteArena {
	a := pa.pool.Get().(*ByteArena)
	a.Reset()
	return a
}

// Put returns a ByteArena to the pool after resetting it.
func (pa *PooledArena) Put(a *ByteArena) {
	a.Reset()
	pa.pool.Put(a)
}
```

### Exercise 2: Example Function

Append to `arena.go`:

```go
// ExampleNewByteArena demonstrates bump-pointer allocation and O(1) reset.
func ExampleNewByteArena() {
	a := NewByteArena(128)
	b1, _ := a.Alloc(16, 8)
	b2, _ := a.Alloc(32, 8)
	_ = b1
	_ = b2
	fmt.Printf("used=%d allocs=%d\n", a.Used(), a.AllocCount())
	a.Reset()
	fmt.Printf("after reset: used=%d allocs=%d\n", a.Used(), a.AllocCount())
	// Output:
	// used=48 allocs=2
	// after reset: used=0 allocs=0
}
```

### Exercise 3: Tests and Benchmarks

Create `arena_test.go`:

```go
package arena

import (
	"errors"
	"testing"
)

func TestByteArenaAllocAndReset(t *testing.T) {
	t.Parallel()

	a := NewByteArena(256)
	b, err := a.Alloc(32, 8)
	if err != nil {
		t.Fatalf("Alloc(32, 8): %v", err)
	}
	if len(b) != 32 {
		t.Errorf("len(b) = %d, want 32", len(b))
	}
	if a.Used() < 32 {
		t.Errorf("Used() = %d, want >= 32", a.Used())
	}

	a.Reset()
	if a.Used() != 0 {
		t.Errorf("Used() after Reset = %d, want 0", a.Used())
	}
	if a.AllocCount() != 0 {
		t.Errorf("AllocCount() after Reset = %d, want 0", a.AllocCount())
	}
}

func TestByteArenaOutOfMemory(t *testing.T) {
	t.Parallel()

	a := NewByteArena(16)
	_, err := a.Alloc(32, 1)
	if !errors.Is(err, ErrOutOfMemory) {
		t.Errorf("Alloc(32, 1) on 16-byte arena: err = %v, want ErrOutOfMemory", err)
	}
}

func TestByteArenaAlignmentRespected(t *testing.T) {
	t.Parallel()

	// Allocate 1 byte to misalign the pointer, then alloc with alignment 8.
	a := NewByteArena(128)
	_, _ = a.Alloc(1, 1)
	b, err := a.Alloc(8, 8)
	if err != nil {
		t.Fatalf("Alloc(8, 8): %v", err)
	}
	// The start address of b must be 8-byte aligned.
	if len(b) != 8 {
		t.Errorf("len(b) = %d, want 8", len(b))
	}
}

func TestByteArenaRemainingIsConsistent(t *testing.T) {
	t.Parallel()

	a := NewByteArena(64)
	initial := a.Remaining()
	_, _ = a.Alloc(16, 1)
	if a.Remaining() > initial {
		t.Error("Remaining() increased after allocation")
	}
}

func TestTypedArenaAllocReturnsNonNil(t *testing.T) {
	t.Parallel()

	type Point struct{ X, Y int32 }
	ta := NewTypedArena[Point](10)
	p, err := ta.Alloc()
	if err != nil {
		t.Fatalf("TypedArena.Alloc(): %v", err)
	}
	if p == nil {
		t.Fatal("TypedArena.Alloc() returned nil")
	}
	p.X = 42
	p.Y = 7
	if p.X != 42 || p.Y != 7 {
		t.Errorf("p = %+v, want {42 7}", *p)
	}
}

func TestTypedArenaReset(t *testing.T) {
	t.Parallel()

	type Item struct{ Value int64 }
	ta := NewTypedArena[Item](5)
	for i := 0; i < 5; i++ {
		_, _ = ta.Alloc()
	}
	beforeUsed := ta.Used()
	ta.Reset()
	if ta.Used() != 0 {
		t.Errorf("Used() after Reset = %d, want 0", ta.Used())
	}
	_ = beforeUsed
}

func TestSlabAllocatorReusesFreeList(t *testing.T) {
	t.Parallel()

	type Node struct{ Val int }
	var s SlabAllocator[Node]

	p1 := s.Alloc()
	p1.Val = 99
	s.Free(p1)

	// After freeing, Alloc should reuse the same block.
	p2 := s.Alloc()
	if p2 == nil {
		t.Fatal("Alloc after Free returned nil")
	}
	// The value should be cleared.
	if p2.Val != 0 {
		t.Errorf("reused block not zeroed: Val = %d", p2.Val)
	}
}

func TestSlabAllocatorOutstanding(t *testing.T) {
	t.Parallel()

	type Node struct{ Val int }
	var s SlabAllocator[Node]

	p := s.Alloc()
	if s.Outstanding() != 1 {
		t.Errorf("Outstanding() = %d, want 1", s.Outstanding())
	}
	s.Free(p)
	if s.Outstanding() != 0 {
		t.Errorf("Outstanding() = %d, want 0 after Free", s.Outstanding())
	}
}

func TestPooledArenaGetAndPut(t *testing.T) {
	t.Parallel()

	pa := NewPooledArena(1024)
	a := pa.Get()
	if a == nil {
		t.Fatal("Get() returned nil")
	}
	_, _ = a.Alloc(64, 8)
	pa.Put(a)

	// Get again: should be reset.
	a2 := pa.Get()
	if a2.Used() != 0 {
		t.Errorf("arena from pool not reset: Used() = %d", a2.Used())
	}
	pa.Put(a2)
}

// Benchmarks: run with go test -bench=. -benchmem

// benchItem is the type allocated in BenchmarkHeapAlloc.
type benchItem struct{ A, B, C, D int64 }

// heapSink forces p to escape to the heap. Without this, the compiler
// stack-allocates p (escape analysis: p does not outlive the loop body)
// and reports 0 allocs/op, which defeats the comparison.
var heapSink *benchItem

func BenchmarkHeapAlloc(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := new(benchItem)
		p.A = int64(i)
		heapSink = p // escape to heap: 1 alloc/op
	}
}

func BenchmarkByteArenaAlloc(b *testing.B) {
	type Item struct{ A, B, C, D int64 }
	a := NewByteArena(b.N*64 + 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = a.Alloc(32, 8)
	}
}

func BenchmarkTypedArenaAlloc(b *testing.B) {
	type Item struct{ A, B, C, D int64 }
	ta := NewTypedArena[Item](b.N + 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := ta.Alloc()
		if err != nil {
			b.Fatal(err)
		}
		_ = p
	}
}

func BenchmarkSlabAlloc(b *testing.B) {
	type Item struct{ A, B, C, D int64 }
	var s SlabAllocator[Item]
	// Pre-warm the freelist.
	ps := make([]*Item, 100)
	for i := range ps {
		ps[i] = s.Alloc()
	}
	for _, p := range ps {
		s.Free(p)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := s.Alloc()
		p.A = int64(i)
		s.Free(p)
	}
}

// Your turn: write BenchmarkPooledArena that uses PooledArena to allocate
// and reset a 64 KB arena for each iteration of b.N. Report allocs/op; the
// steady-state result should be near 0 allocs/op after pool warm-up.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/arena"
)

func main() {
	fmt.Println("Arena allocation patterns demo")
	fmt.Println()

	// ByteArena: batch of 1000 small allocations.
	ba := arena.NewByteArena(64 * 1024)
	const batchSize = 1000
	for i := 0; i < batchSize; i++ {
		b, err := ba.Alloc(32, 8)
		if err != nil {
			fmt.Printf("Alloc failed at i=%d: %v\n", i, err)
			return
		}
		b[0] = byte(i)
	}
	fmt.Printf("ByteArena: %d allocs, %d bytes used, %d remaining\n",
		ba.AllocCount(), ba.Used(), ba.Remaining())
	ba.Reset()
	fmt.Printf("After Reset: used=%d allocs=%d\n", ba.Used(), ba.AllocCount())
	fmt.Println()

	// TypedArena: typed allocation without unsafe at the call site.
	type Point struct{ X, Y float64 }
	ta := arena.NewTypedArena[Point](100)
	for i := 0; i < 50; i++ {
		p, err := ta.Alloc()
		if err != nil {
			fmt.Printf("TypedArena.Alloc failed: %v\n", err)
			return
		}
		p.X = float64(i)
		p.Y = float64(i) * 2
	}
	fmt.Printf("TypedArena[Point]: %d bytes used\n", ta.Used())
	ta.Reset()
	fmt.Println()

	// SlabAllocator: individual free without batch reset.
	type Node struct{ Val int }
	var slab arena.SlabAllocator[Node]
	nodes := make([]*Node, 10)
	for i := range nodes {
		nodes[i] = slab.Alloc()
		nodes[i].Val = i
	}
	fmt.Printf("SlabAllocator: %d outstanding\n", slab.Outstanding())
	for _, n := range nodes {
		slab.Free(n)
	}
	fmt.Printf("SlabAllocator after free: %d outstanding\n", slab.Outstanding())
	fmt.Println()

	// PooledArena: batch cycle with pool reuse.
	pa := arena.NewPooledArena(32 * 1024)
	for cycle := 0; cycle < 3; cycle++ {
		a := pa.Get()
		for i := 0; i < 100; i++ {
			_, _ = a.Alloc(16, 8)
		}
		fmt.Printf("Cycle %d: used=%d\n", cycle, a.Used())
		pa.Put(a)
	}

	fmt.Println()
	fmt.Println("Run benchmarks: go test -bench=. -benchmem")
}
```

## Common Mistakes

### Arena-Allocated Objects Outliving the Arena Reset

Wrong: storing a `*T` obtained from an arena in a long-lived data structure, then resetting the arena.

What happens: the backing store is reused. The pointer now points to memory that may be overwritten by subsequent allocations. This is a use-after-free bug.

Fix: arenas enforce a strict lifetime rule: all `*T` values obtained from the arena must be discarded (or their values copied out) before `Reset` is called.

### Pointers Inside Arena Objects Confusing the GC

Wrong: an arena-allocated struct that holds a pointer to a GC-managed value (e.g., `type Item struct { data *BigObject }`), stored in a `[]byte` arena.

What happens: the GC scans the `[]byte` backing store as opaque bytes. It does not see the pointer to `BigObject`. If `BigObject` has no other live references, the GC collects it, leaving a dangling pointer in the arena.

Fix: arena objects must not contain pointers to GC-managed memory unless you also maintain a root reference to the pointed-to object outside the arena. Prefer arenas for plain-data structs (no pointer fields).

### Using the Wrong Arena Pattern for the Workload

Wrong: using a bump-pointer arena for a workload where objects are freed at different times.

What happens: you cannot free individual objects; the only reclamation is a full `Reset`. Objects that need to be freed early hold their slot in the arena until `Reset`, potentially using much more memory than necessary.

Fix: use a slab/freelist allocator when objects need independent lifetimes. Use a bump-pointer arena when all objects in a batch have the same lifetime.

### Not Pre-Warming the Pool Before Benchmarking

Wrong: benchmarking `PooledArena.Get` without a pool warm-up.

What happens: the first iteration creates a new arena from scratch (one allocation). Subsequent iterations reuse the pooled arena (zero allocations). The benchmark result is not stable.

Fix: call `Get` and `Put` once before `b.ResetTimer()` to ensure the pool has one arena ready.

## Verification

From `~/go-exercises/arena`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=. -benchmem
```

Expected benchmark ordering (allocs/op, lowest to highest). The arena and
slab benchmarks report 0 allocs/op because they serve from pre-allocated
memory. `BenchmarkHeapAlloc` assigns to the package-level `heapSink` to
force escape analysis to heap-allocate each object, so it reports 1 alloc/op:

```
BenchmarkByteArenaAlloc:  0 allocs/op
BenchmarkTypedArenaAlloc: 0 allocs/op
BenchmarkSlabAlloc:       0 allocs/op (warm freelist)
BenchmarkHeapAlloc:       1 alloc/op
```

## Summary

- A bump-pointer arena allocates in O(1) by advancing an offset into a pre-allocated slab; Reset is O(1) regardless of object count.
- The backing `[]byte` is opaque to the GC: one root pointer instead of n object pointers reduces GC scan work.
- A `TypedArena[T]` uses generics and `unsafe.Pointer` to provide type-safe allocation without per-call unsafe code at the call site.
- A slab/freelist allocator supports O(1) individual freeing, suitable when objects have independent lifetimes within a batch.
- A `PooledArena` combines `sync.Pool` with a bump-pointer arena to amortise backing-store allocation across batch cycles.
- The critical constraint: arena-allocated objects must not outlive the arena reset, and must not hold pointers to GC-managed memory unless external roots exist.

## What's Next

Next: [Reading SSA Output](../../36-runtime-compiler-and-assembly/01-reading-ssa-output/01-reading-ssa-output.md).

## Resources

- [sync.Pool](https://pkg.go.dev/sync#Pool) — object pooling; the complement to arena allocation for GC-managed objects
- [unsafe package](https://pkg.go.dev/unsafe) — Sizeof, Alignof, and Pointer conversion used in TypedArena
- [Region-based memory management](https://en.wikipedia.org/wiki/Region-based_memory_management) — the general concept behind arena allocation
- [Go arena experiment proposal](https://github.com/golang/go/issues/51317) — the discussion behind the (removed) experimental arena package and its design constraints
