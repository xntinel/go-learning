# 13. Implementing a Custom Memory Allocator

Go's runtime allocator (a descendant of TCMalloc) uses per-P caches, size-class spans, and a page heap. Understanding it at that depth requires building a simpler version from scratch. This lesson constructs a free-list allocator on top of a contiguous `[]byte` backing store, using `unsafe` for inline block headers and pointer arithmetic. The hard parts are: keeping the header layout consistent with Go's alignment rules, coalescing adjacent free blocks on `Free`, and writing tests that reason about pointer identity across allocations.

```text
allocator/
  go.mod
  allocator.go
  allocator_test.go
  cmd/demo/main.go
```

## Concepts

### What a Free-List Allocator Does

A free-list allocator owns one large region of memory (the "backing store") and subdivides it on demand. Each subdivision is called a block. A block consists of a fixed-size header followed by the payload the caller receives. The free list is a singly-linked list that threads through the headers of all currently-free blocks. On `Alloc`, the allocator walks the list looking for a block large enough to satisfy the request; on `Free`, it inserts the block back into the list and merges it with any adjacent free blocks.

```
backing store ([]byte, 64 KB)
+-----------+----------+-----------+----------+------ ...
| hdr(used) | payload  | hdr(free) | payload  |
|  size=32  |  32 B    |  size=96  |  96 B    |
+-----------+----------+-----------+----------+------ ...
                            ^
                    free list head
```

The returned pointer points to the payload, not to the header. The allocator recovers the header by subtracting the header size.

### Inline Block Headers and `unsafe.Pointer`

Go's `unsafe` package lets you treat an arbitrary memory address as a pointer to any type. The header is a small struct:

```go
type header struct {
	size uint32 // payload size in bytes (not including the header itself)
	free uint32 // 1 if block is free, 0 if in use; uint32 for alignment
	next *header
}
```

The header is placed at the start of each block. The payload begins immediately after. `unsafe.Add` (available since Go 1.17) is the idiomatic, vet-safe way to compute pointer offsets:

```go
// payload pointer from a header pointer:
payload := unsafe.Add(unsafe.Pointer(h), unsafe.Sizeof(header{}))

// header pointer from a payload pointer:
h := (*header)(unsafe.Add(ptr, -int(unsafe.Sizeof(header{}))))
```

The rule from the `unsafe` package documentation: converting a `unsafe.Pointer` to `uintptr`, storing the `uintptr` in a variable, then converting it back is unsafe (the GC may have moved the object between those two steps). `unsafe.Add` is a single-expression conversion that the compiler treats as a first-class safe-point-compatible operation.

### Alignment

Hardware requires that a value of type T be stored at an address that is a multiple of `unsafe.Alignof(T)`. A `header` contains a pointer field (`*header`), so its alignment is 8 on 64-bit systems. Every block — header plus payload — must start at an 8-byte boundary. `Alloc` rounds the requested size up to the next multiple of 8 before carving out a block.

The backing store itself must start at an 8-byte-aligned address. `make([]byte, n)` does not guarantee any specific alignment; the allocator finds the first 8-byte-aligned offset inside the slice and stores that as a typed `*header`, keeping the `[]byte` alive via the struct field.

### First-Fit Search and Block Splitting

The simplest search strategy is first-fit: walk the free list and use the first block that is large enough. If the block is significantly larger than the request, split it: carve out just enough for the header plus the (rounded) payload, initialize a new header for the remainder, and insert the remainder back into the free list. Splitting avoids wasting space when a large free block satisfies a small request.

The minimum useful split size is `headerSize + 8` (16 + 8 = 24 bytes on 64-bit), because a smaller remainder could not hold a header plus an 8-byte aligned payload.

### Coalescing on Free

Fragmentation accumulates when the free list fills with many small blocks that cannot satisfy a large request even though their total free space is sufficient. Coalescing (also called merging) prevents this by joining adjacent free blocks into a single larger block when a block is freed.

To check adjacency, use `unsafe.Add` to compute the expected address of the next block and compare it with `h.next`:

```go
// expected start of the block that immediately follows h:
expected := unsafe.Add(unsafe.Pointer(h), headerSize+uintptr(h.size))
if h.next != nil && unsafe.Pointer(h.next) == expected && h.next.free == 1 {
    // h and h.next are physically adjacent free blocks — merge them
    h.size += uint32(headerSize) + h.next.size
    h.next = h.next.next
}
```

This forward-coalescing pass runs at `Free` time, restoring large free blocks before the next allocation.

### Stats and Diagnostics

A production allocator exposes diagnostics. This lesson tracks:

- `TotalBytes` — total backing store capacity (minus alignment waste)
- `UsedBytes` — sum of header+payload for all in-use blocks
- `FreeBytes` — sum of header+payload for all free blocks
- `LargestFree` — largest contiguous free payload size (fragmentation indicator)
- `AllocCount`, `FreeCount` — running counters

Fragmentation ratio: `1 - float64(LargestFree) / float64(totalFreePayload)`. Zero means all free space is contiguous; close to 1 means it is scattered in tiny fragments.

## Exercises

This is a library verified by `go test`, not by `go run`.

### Exercise 1: The Header, the Backing Store, Constructor, Alloc, Free, and Coalescing

Create `allocator.go`:

```go
package allocator

import (
	"errors"
	"unsafe"
)

// headerSize is the payload offset from the start of a block.
// unsafe.Sizeof is evaluated at compile time; on 64-bit it is 16
// (4+4+8 for size, free, and next).
const headerSize = unsafe.Sizeof(header{})

// header is the inline metadata stored at the very start of every block.
// uint32 fields keep the struct at 16 bytes on 64-bit (4+4+8), which is a
// multiple of 8 — so every header is naturally 8-byte-aligned.
type header struct {
	size uint32 // payload bytes, not counting the header itself
	free uint32 // 1 = free, 0 = in use
	next *header
}

// Stats summarises the current state of the allocator.
type Stats struct {
	TotalBytes  int
	UsedBytes   int
	FreeBytes   int
	LargestFree int // largest contiguous free payload (fragmentation indicator)
	AllocCount  int
	FreeCount   int
}

// ErrNoMemory is returned by Alloc when no free block can satisfy the request.
var ErrNoMemory = errors.New("allocator: out of memory")

// ErrInvalidPtr is returned by Free when the pointer was not issued by this
// allocator or its header is corrupt.
var ErrInvalidPtr = errors.New("allocator: invalid pointer")

// ErrZeroSize is returned by Alloc when size is zero or negative.
var ErrZeroSize = errors.New("allocator: size must be positive")

// FreeListAllocator manages a contiguous backing store with a first-fit
// free list. It is not safe for concurrent use without external locking.
type FreeListAllocator struct {
	buf        []byte  // raw backing store — kept alive so GC does not collect it
	base       *header // first aligned block header in buf
	end        uintptr // one-past-end address of the usable region
	head       *header // head of the free list (address-ordered)
	allocCount int
	freeCount  int
}

// align8 rounds n up to the nearest multiple of 8.
func align8(n int) int {
	return (n + 7) &^ 7
}

// headerToPayload returns the payload pointer for a given block header.
// unsafe.Add is the vet-approved single-expression pointer arithmetic form.
func headerToPayload(h *header) unsafe.Pointer {
	return unsafe.Add(unsafe.Pointer(h), headerSize)
}

// nextBlock returns the header of the block that physically follows h.
func nextBlock(h *header) *header {
	return (*header)(unsafe.Add(unsafe.Pointer(h), headerSize+uintptr(h.size)))
}

// New creates a FreeListAllocator backed by a fresh byte slice of at least
// size bytes. The usable capacity may be a few bytes less due to alignment.
func New(size int) (*FreeListAllocator, error) {
	minSize := int(headerSize) + 8
	if size <= minSize {
		return nil, errors.New("allocator: size too small")
	}
	// Over-allocate by 7 so we can find an 8-byte-aligned start inside buf.
	buf := make([]byte, size+7)
	p0 := unsafe.Pointer(&buf[0])
	// Advance to the next 8-byte boundary if not already aligned.
	off := uintptr(p0) & 7
	if off != 0 {
		p0 = unsafe.Add(p0, 8-off)
	}
	base := (*header)(p0)
	// Compute the one-past-end address from buf's last valid byte.
	end := uintptr(unsafe.Pointer(&buf[len(buf)-1])) + 1
	// Capacity = usable bytes starting at p0.
	capacity := int(end-uintptr(p0)) - int(headerSize)
	if capacity < 8 {
		return nil, errors.New("allocator: capacity too small after alignment")
	}

	// Initialise the single free block that spans the whole region.
	base.size = uint32(capacity)
	base.free = 1
	base.next = nil

	return &FreeListAllocator{
		buf:  buf,
		base: base,
		end:  end,
		head: base,
	}, nil
}

// Alloc allocates at least size bytes and returns a pointer to the payload.
// The returned pointer is valid until the FreeListAllocator is collected.
// Returns ErrZeroSize if size <= 0 and ErrNoMemory if no block is large enough.
func (a *FreeListAllocator) Alloc(size int) (unsafe.Pointer, error) {
	if size <= 0 {
		return nil, ErrZeroSize
	}
	need := uint32(align8(size))

	var prev *header
	cur := a.head
	for cur != nil {
		if cur.free == 1 && cur.size >= need {
			// Split if the remainder is large enough to hold a header + 8 bytes.
			minSplit := uint32(headerSize) + 8
			if cur.size >= need+minSplit {
				// newH sits immediately after cur's payload.
				newH := (*header)(unsafe.Add(unsafe.Pointer(cur), headerSize+uintptr(need)))
				newH.size = cur.size - need - uint32(headerSize)
				newH.free = 1
				newH.next = cur.next
				cur.size = need
				cur.next = newH
			}
			// Unlink cur from the free list.
			if prev == nil {
				a.head = cur.next
			} else {
				prev.next = cur.next
			}
			cur.free = 0
			cur.next = nil
			a.allocCount++
			return headerToPayload(cur), nil
		}
		prev = cur
		cur = cur.next
	}
	return nil, ErrNoMemory
}

// Free returns a block to the free list and coalesces adjacent free blocks.
// It returns ErrInvalidPtr if ptr is nil, out of range, or already freed.
func (a *FreeListAllocator) Free(ptr unsafe.Pointer) error {
	if ptr == nil {
		return ErrInvalidPtr
	}
	// Bounds check using uintptr integer arithmetic only — no unsafe.Add on a
	// foreign pointer, which would trigger the checkptr runtime check.
	pAddr := uintptr(ptr)
	baseAddr := uintptr(unsafe.Pointer(a.base))
	// A valid payload address must be at least baseAddr+headerSize (the first
	// possible payload) and at most a.end-1.
	if pAddr < baseAddr+headerSize || pAddr >= a.end {
		return ErrInvalidPtr
	}
	// Recover the header using an offset from a.base so checkptr stays happy:
	// we always compute the result relative to a pointer we own.
	offset := pAddr - headerSize - baseAddr
	h := (*header)(unsafe.Add(unsafe.Pointer(a.base), offset))
	if h.free == 1 {
		return ErrInvalidPtr // double-free
	}
	h.free = 1
	a.freeCount++

	// Insert h into the free list in ascending address order so that
	// coalesce can always find adjacent blocks as immediate list neighbors.
	hAddr := uintptr(unsafe.Pointer(h))
	if a.head == nil || uintptr(unsafe.Pointer(a.head)) > hAddr {
		h.next = a.head
		a.head = h
	} else {
		cur := a.head
		for cur.next != nil && uintptr(unsafe.Pointer(cur.next)) < hAddr {
			cur = cur.next
		}
		h.next = cur.next
		cur.next = h
	}

	// Forward coalesce: h + h.next.
	a.coalesce(h)
	// Backward coalesce: predecessor + h (now possibly enlarged).
	if a.head != h {
		cur := a.head
		for cur.next != nil && cur.next != h {
			cur = cur.next
		}
		if cur.next == h {
			a.coalesce(cur)
		}
	}
	return nil
}

// coalesce merges h with h.next when they are physically adjacent and h.next is free.
func (a *FreeListAllocator) coalesce(h *header) {
	if h.next == nil || h.next.free != 1 {
		return
	}
	// nextBlock computes the expected address using unsafe.Add (vet-safe).
	if unsafe.Pointer(nextBlock(h)) != unsafe.Pointer(h.next) {
		return // not adjacent in memory
	}
	h.size += uint32(headerSize) + h.next.size
	h.next = h.next.next
}

// Stats returns a snapshot of the allocator state by walking all blocks.
func (a *FreeListAllocator) Stats() Stats {
	var used, free, largest, total int
	baseAddr := uintptr(unsafe.Pointer(a.base))
	cur := a.base
	for {
		sz := int(cur.size)
		total += int(headerSize) + sz
		if cur.free == 1 {
			free += int(headerSize) + sz
			if sz > largest {
				largest = sz
			}
		} else {
			used += int(headerSize) + sz
		}
		// Compute the next block's address as a uintptr before converting;
		// this avoids computing an out-of-bounds unsafe.Add when cur is the last block.
		nextAddr := uintptr(unsafe.Pointer(cur)) + headerSize + uintptr(cur.size)
		if nextAddr >= a.end {
			break
		}
		// nextAddr is within buf, so it is safe to derive a pointer from a.base.
		cur = (*header)(unsafe.Add(unsafe.Pointer(a.base), nextAddr-baseAddr))
	}
	return Stats{
		TotalBytes:  total,
		UsedBytes:   used,
		FreeBytes:   free,
		LargestFree: largest,
		AllocCount:  a.allocCount,
		FreeCount:   a.freeCount,
	}
}

// FragmentationRatio returns a value in [0, 1]: 0 = all free space is
// contiguous; approaching 1 = heavily fragmented.
func (a *FreeListAllocator) FragmentationRatio() float64 {
	s := a.Stats()
	if s.FreeBytes == 0 {
		return 0
	}
	freePayload := s.FreeBytes - countFreeBlocks(a)*int(headerSize)
	if freePayload <= 0 {
		return 0
	}
	return 1 - float64(s.LargestFree)/float64(freePayload)
}

func countFreeBlocks(a *FreeListAllocator) int {
	n := 0
	cur := a.head
	for cur != nil {
		n++
		cur = cur.next
	}
	return n
}
```

The header is placed directly in the backing store; no heap allocation is needed for the free list. All pointer arithmetic uses `unsafe.Add`, which is the vet-approved form for single-expression pointer arithmetic.

### Exercise 2: Alloc and Free in Action

`Alloc` walks the free list with first-fit search, splits oversized blocks so the remainder stays in the list, then unlinks the chosen block and returns a pointer to its payload. `Free` inserts the block back in address order, then calls `coalesce` twice: once to merge the freed block with its successor, and once to merge its predecessor with it.

### Exercise 3: Tests

Create `allocator_test.go`:

```go
// allocator_test.go
package allocator

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewRejectsTooSmall(t *testing.T) {
	t.Parallel()
	_, err := New(4)
	if err == nil {
		t.Fatal("expected error for size=4, got nil")
	}
}

func TestAllocAndFreeBasic(t *testing.T) {
	t.Parallel()
	a, err := New(4096)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Alloc(64)
	if err != nil {
		t.Fatalf("Alloc(64) error = %v", err)
	}
	if p == nil {
		t.Fatal("Alloc returned nil pointer")
	}
	if err := a.Free(p); err != nil {
		t.Fatalf("Free error = %v", err)
	}
}

func TestAllocZeroSizeReturnsError(t *testing.T) {
	t.Parallel()
	a, _ := New(4096)
	_, err := a.Alloc(0)
	if !errors.Is(err, ErrZeroSize) {
		t.Fatalf("Alloc(0) err = %v, want ErrZeroSize", err)
	}
}

func TestFreeNilReturnsError(t *testing.T) {
	t.Parallel()
	a, _ := New(4096)
	if err := a.Free(nil); !errors.Is(err, ErrInvalidPtr) {
		t.Fatalf("Free(nil) err = %v, want ErrInvalidPtr", err)
	}
}

func TestFreeOutsideRegionReturnsError(t *testing.T) {
	t.Parallel()
	a, _ := New(4096)
	// Allocate from a second allocator so the pointer is a valid heap address
	// but is outside a's region.  Using a stack-variable pointer would trigger
	// the runtime checkptr check.
	b, _ := New(4096)
	p, _ := b.Alloc(32)
	if err := a.Free(p); !errors.Is(err, ErrInvalidPtr) {
		t.Fatalf("Free(other allocator ptr) err = %v, want ErrInvalidPtr", err)
	}
}

func TestDoubleFreeReturnsError(t *testing.T) {
	t.Parallel()
	a, _ := New(4096)
	p, _ := a.Alloc(32)
	if err := a.Free(p); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(p); !errors.Is(err, ErrInvalidPtr) {
		t.Fatalf("double Free err = %v, want ErrInvalidPtr", err)
	}
}

func TestAllocExhausts(t *testing.T) {
	t.Parallel()
	// 512 bytes with a 32-byte payload request: expect exhaustion eventually.
	a, err := New(512)
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 100; i++ {
		_, last = a.Alloc(32)
		if errors.Is(last, ErrNoMemory) {
			return
		}
	}
	t.Fatal("expected ErrNoMemory but did not get it")
}

func TestCoalescingRestoredCapacity(t *testing.T) {
	t.Parallel()
	a, err := New(4096)
	if err != nil {
		t.Fatal(err)
	}
	// Allocate three blocks, free two of them, confirm the allocator can
	// satisfy a large request after coalescing restores capacity.
	p1, _ := a.Alloc(128)
	p2, _ := a.Alloc(128)
	p3, _ := a.Alloc(128)

	_ = a.Free(p1)
	_ = a.Free(p2)
	_ = a.Free(p3)

	// After freeing all three contiguous blocks they should coalesce.
	// A fresh 300-byte alloc should succeed.
	p, err := a.Alloc(300)
	if err != nil {
		t.Fatalf("Alloc(300) after coalesce: %v", err)
	}
	if err := a.Free(p); err != nil {
		t.Fatal(err)
	}
}

func TestStatsConsistency(t *testing.T) {
	t.Parallel()
	a, _ := New(4096)
	p1, _ := a.Alloc(64)
	p2, _ := a.Alloc(64)
	s := a.Stats()
	if s.AllocCount != 2 {
		t.Fatalf("AllocCount = %d, want 2", s.AllocCount)
	}
	if s.UsedBytes == 0 {
		t.Fatal("UsedBytes should be > 0 after allocations")
	}
	_ = a.Free(p1)
	_ = a.Free(p2)
	s2 := a.Stats()
	if s2.FreeCount != 2 {
		t.Fatalf("FreeCount = %d, want 2", s2.FreeCount)
	}
}

func TestTableAllocSizes(t *testing.T) {
	t.Parallel()
	sizes := []int{1, 7, 8, 9, 16, 64, 100, 200}
	for _, sz := range sizes {
		sz := sz
		t.Run(fmt.Sprintf("size=%d", sz), func(t *testing.T) {
			t.Parallel()
			a, err := New(4096)
			if err != nil {
				t.Fatal(err)
			}
			p, err := a.Alloc(sz)
			if err != nil {
				t.Fatalf("Alloc(%d) error = %v", sz, err)
			}
			if err := a.Free(p); err != nil {
				t.Fatalf("Free after Alloc(%d) error = %v", sz, err)
			}
		})
	}
}

func TestWriteAndReadPayload(t *testing.T) {
	t.Parallel()
	a, _ := New(4096)
	p, err := a.Alloc(8)
	if err != nil {
		t.Fatal(err)
	}
	// Write a uint64 into the payload and read it back.
	*(*uint64)(p) = 0xDEADBEEFCAFEBABE
	got := *(*uint64)(p)
	if got != 0xDEADBEEFCAFEBABE {
		t.Fatalf("payload read = %x, want DEADBEEFCAFEBABE", got)
	}
	_ = a.Free(p)
}

// ExampleNew demonstrates basic allocation and Stats reporting.
func ExampleNew() {
	a, _ := New(4096)
	p, _ := a.Alloc(64)
	s := a.Stats()
	fmt.Printf("allocs=%d used=%v\n", s.AllocCount, s.UsedBytes > 0)
	_ = a.Free(p)
	s2 := a.Stats()
	fmt.Printf("frees=%d\n", s2.FreeCount)
	// Output:
	// allocs=1 used=true
	// frees=1
}

// Your turn: add TestFragmentationAfterFreeingEveryOther that allocates 10
// blocks of 32 bytes, frees every even-indexed block, and asserts that
// a.FragmentationRatio() > 0 (space is fragmented). Then free the odd blocks
// and assert FragmentationRatio() == 0 or very close to zero.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"
	"unsafe"

	"example.com/allocator"
)

func main() {
	a, err := allocator.New(64 * 1024) // 64 KB backing store
	if err != nil {
		log.Fatal(err)
	}

	// Allocate several blocks of different sizes.
	sizes := []int{16, 32, 64, 128, 256}
	ptrs := make([]unsafe.Pointer, len(sizes))
	for i, sz := range sizes {
		p, err := a.Alloc(sz)
		if err != nil {
			log.Fatalf("Alloc(%d): %v", sz, err)
		}
		ptrs[i] = p
	}

	s := a.Stats()
	fmt.Printf("After %d allocations:\n", s.AllocCount)
	fmt.Printf("  total=%d B  used=%d B  free=%d B\n", s.TotalBytes, s.UsedBytes, s.FreeBytes)
	fmt.Printf("  fragmentation ratio: %.3f\n", a.FragmentationRatio())

	// Free every other pointer to create fragmentation.
	for i := 0; i < len(ptrs); i += 2 {
		if err := a.Free(ptrs[i]); err != nil {
			log.Fatalf("Free[%d]: %v", i, err)
		}
		ptrs[i] = nil
	}

	s2 := a.Stats()
	fmt.Printf("\nAfter freeing every other block:\n")
	fmt.Printf("  frees=%d  free=%d B  largest_free=%d B\n", s2.FreeCount, s2.FreeBytes, s2.LargestFree)
	fmt.Printf("  fragmentation ratio: %.3f\n", a.FragmentationRatio())

	// Free the remaining blocks.
	for _, p := range ptrs {
		if p != nil {
			_ = a.Free(p)
		}
	}

	s3 := a.Stats()
	fmt.Printf("\nAfter freeing all blocks:\n")
	fmt.Printf("  free=%d B  largest_free=%d B\n", s3.FreeBytes, s3.LargestFree)
	fmt.Printf("  fragmentation ratio: %.3f\n", a.FragmentationRatio())
}
```

## Common Mistakes

### Forgetting That the Returned Pointer Is the Payload, Not the Header

Wrong: treating the pointer returned by `Alloc` as the block start and computing the next block as `ptr + size`. This skips the header and lands in the middle of a block.

Fix: the payload pointer is `header_address + headerSize`. Recovering the header is `payload_pointer - headerSize`. The allocator does this consistently in both `Alloc` and `Free`.

### Using Spaces for Go Indentation

Wrong: pasting code with space-indented bodies into the lesson. `gofmt -l` flags every such file and the gate fails.

Fix: Go source is always tab-indented. Run `gofmt -w .` in the module directory before testing.

### Inserting into the Free List Without Maintaining Address Order

Wrong: inserting freed blocks at the head of the free list regardless of address. Coalescing then fails because the list is not sorted by address; two physically adjacent free blocks may have non-adjacent list entries.

Fix: insert in address order (traverse the list until `cur.next > hAddr`, insert between `cur` and `cur.next`). Then coalescing only needs to check the immediate neighbors in the list.

### Missing the Alignment Requirement on the Backing Store

Wrong: using `&buf[0]` as the base without aligning it. On platforms where `buf[0]` is not 8-byte-aligned, casting to `*header` violates the Go memory model and causes undefined behavior.

Fix: compute `base = (uintptr(&buf[0]) + 7) &^ 7` and work from `base`, using a slightly over-allocated `buf`.

### Using `uintptr` Across a GC Safepoint

Wrong: storing a `uintptr` derived from `unsafe.Pointer` in a variable and then calling a function that may trigger GC. The GC may move the object, leaving the `uintptr` pointing at freed memory.

Fix: keep the conversion and dereference in a single expression, or use `unsafe.Pointer` for storage. The allocator stores `*header` pointers (which are valid GC roots) not bare `uintptr` values, so the backing store is kept alive through the `buf` slice field.

## Verification

From `~/go-exercises/allocator`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed. The race detector is important here because `unsafe` pointer arithmetic bypasses the normal escape analysis; a race condition would be silent without `-race`.

Add the "your turn" test described in Exercise 3 before calling the lesson complete.

## Summary

- A free-list allocator subdivides a contiguous backing store into blocks with inline headers containing size, free flag, and a next pointer.
- `Alloc` walks the free list with first-fit search and splits oversized blocks to reduce waste.
- `Free` inserts the block back in address order and coalesces adjacent free blocks to counteract fragmentation.
- `unsafe.Pointer` arithmetic is used to move between the header and the payload; the header is stored in the backing store, not on the heap.
- Alignment matters: headers and payloads must start at 8-byte boundaries on 64-bit systems; the constructor pads the backing store start accordingly.
- The `uintptr` rule: never store a derived `uintptr` across a GC safepoint; keep the full `unsafe.Pointer` chain live via a struct field or a single-expression conversion.

## What's Next

Next: [Writing a Goroutine-Aware Profiler](../14-writing-a-goroutine-aware-profiler/14-writing-a-goroutine-aware-profiler.md).

## Resources

- [unsafe package documentation](https://pkg.go.dev/unsafe) — `Sizeof`, `Alignof`, `Pointer`, `Add`; the rules for valid `unsafe.Pointer` conversions
- [Go runtime memory allocator source (malloc.go)](https://github.com/golang/go/blob/master/src/runtime/malloc.go) — the actual TCMalloc-derived Go allocator
- [TCMalloc design document](https://google.github.io/tcmalloc/design.html) — the allocator architecture Go's runtime descends from
- [Go specification: Package unsafe](https://go.dev/ref/spec#Package_unsafe) — the formal rules governing `unsafe.Pointer` conversions
- [The Slab Allocator: An Object-Caching Kernel Memory Allocator (Bonwick, USENIX 1994)](https://www.usenix.org/legacy/publications/library/proceedings/bos94/bonwick.html) — the original slab allocator paper; context for the size-class approach used by TCMalloc and Go's runtime
