# 5. io_uring Integration

io_uring is Linux's high-performance asynchronous I/O interface, available since kernel 5.1. Its core idea is deceptively simple: two shared-memory ring buffers between user space and the kernel eliminate most system-call overhead. The hard parts in Go are correctly applying memory barriers on the shared ring indices, navigating the submission queue's indirection array (which is not a simple circular buffer of SQEs), and bridging the kernel's poll-driven completion model to Go's goroutine scheduler.

```text
iouring/
  go.mod
  errors.go               // sentinel errors — no build constraint
  cqe.go                  // CQE type and helpers — no build constraint
  future.go               // Future type — no build constraint
  ring_linux.go           // Ring, SQE, mmap, syscalls — //go:build linux
  ring_linux_test.go      // integration tests — //go:build linux
  ring_example_test.go    // Example functions — no build constraint
  cmd/demo/main.go        // runnable demo — //go:build linux
```

The package is named `iouring`. The platform-independent files (`errors.go`, `cqe.go`, `future.go`) compile on any OS, making the core types testable without Linux. All syscall and mmap code lives in `ring_linux.go`.

## Concepts

### The Ring Buffer Model

io_uring uses two ring buffers in a region of memory shared between user space and the kernel:

- **Submission queue (SQ)**: user space is the producer; the kernel is the consumer. The user writes SQEs (submission queue entries) to the tail; the kernel reads from the head.
- **Completion queue (CQ)**: the kernel is the producer; user space is the consumer. The kernel writes CQEs (completion queue entries) to the tail; the user reads from the head.

Because both sides share the same physical memory pages (mapped via `mmap`), the kernel does not copy data, and for normal operation no system call is required to submit work.

### The SQ Indirection Array

The SQ ring does not hold SQEs directly. It holds an index array (`sq_array`) where each slot is a 32-bit index into a separate SQE array. To submit SQE number N, the user writes N into `sq_array[tail & mask]` and then atomically advances the tail. The SQE array itself is a third separately-mapped memory region. This indirection lets the kernel reorder or preprocess SQEs internally while user space fills them in a straightforward linear fashion.

The three mapped regions and their mmap offset constants (from `<linux/io_uring.h>`):

| Region | Offset constant (value) | Contains |
|--------|------------------------|----------|
| SQ ring | `IORING_OFF_SQ_RING` (0x00000000) | head, tail, mask, flags, sq_array |
| SQE array | `IORING_OFF_SQES` (0x10000000) | 64-byte SQE structs |
| CQ ring | `IORING_OFF_CQ_RING` (0x08000000) | head, tail, mask, CQE structs |

### Memory Barriers in Go

The ring head and tail indices are written by one side (kernel or user) and read by the other. A stale read causes either a missed completion or a corrupted double-submission. The correctness rules are:

- **Before reading the CQ tail** (checking whether the kernel posted new completions): use `atomic.LoadUint32(cq.tail)` — load-acquire, ensuring all CQE writes by the kernel before its tail store are visible.
- **After writing SQEs and advancing the SQ tail**: use `atomic.StoreUint32(sq.tail, newTail)` — store-release, ensuring the kernel sees all SQE writes before it reads the updated tail.
- **Before reading the SQ head** (checking how many slots are free): use `atomic.LoadUint32(sq.head)` — load-acquire.

Go's `sync/atomic` provides the required acquire/release semantics on all supported architectures. A plain integer read or write on a shared index is a data race that will silently corrupt ring state.

### The io_uring_enter Syscall

`io_uring_enter(fd, to_submit, min_complete, flags, sig, sigsetsize)` is the only syscall needed after setup. It does two independent things:

1. If `to_submit > 0`, tells the kernel to process that many SQEs from the submission ring.
2. If `flags` includes `IORING_ENTER_GETEVENTS`, blocks until at least `min_complete` CQEs are available.

The performance advantage over classic `pread`/`pwrite` is batching: a single `io_uring_enter` call can submit thousands of operations. For SQPOLL mode (`IORING_SETUP_SQPOLL`), a kernel-side thread polls the SQ continuously, eliminating even the `io_uring_enter` call for submission.

### Bridging to Goroutines

io_uring completions arrive asynchronously. The natural Go bridge is a background goroutine that polls the CQ ring and routes each CQE to the goroutine that submitted the corresponding SQE. Routing uses a `map[uint64]chan CQE` keyed by `user_data` — a field in every SQE that the kernel copies verbatim into the matching CQE. Set `user_data` to a monotonically increasing ID generated with an `atomic.Uint64`; the poller uses the ID to dispatch the CQE to the correct waiting channel.

### SQE Layout

Each SQE is exactly 64 bytes with a union layout defined in `<linux/io_uring.h>`. Represent it in Go as a `[64]byte` with typed accessor methods that write known byte offsets. This is safer than a Go struct with `unsafe.Offsetof` and exactly matches the C compiler layout:

| Offset | Size | Field |
|--------|------|-------|
| 0 | 1 | opcode |
| 1 | 1 | flags (IOSQE_* bits) |
| 2 | 2 | ioprio |
| 4 | 4 | fd |
| 8 | 8 | off / addr2 |
| 16 | 8 | addr (buffer pointer) |
| 24 | 4 | len (buffer length) |
| 28 | 4 | op-specific flags |
| 32 | 8 | user_data |
| 40 | 2 | buf_index |
| 42 | 2 | personality |
| 44 | 4 | splice_fd_in / file_index |
| 48 | 16 | padding |

## Exercises

Set up the module. This package requires Linux 5.1+ and will not link or run on other operating systems.

```bash
go get golang.org/x/sys@latest
```

### Exercise 1: Platform-Independent Types

These three files carry no build constraints and compile on any OS. They form the public API surface that callers on any platform can import and use in tests.

Create `errors.go`:

```go
package iouring

import "errors"

// Sentinel errors returned by Ring operations.
var (
	ErrRingFull   = errors.New("iouring: submission ring is full")
	ErrRingClosed = errors.New("iouring: ring is closed")
	ErrNoCQE      = errors.New("iouring: no completion available")
)
```

Create `cqe.go`:

```go
package iouring

import (
	"fmt"
	"syscall"
)

// CQE mirrors struct io_uring_cqe from <linux/io_uring.h>: 16 bytes total.
// The kernel writes a CQE for every completed SQE.
type CQE struct {
	UserData uint64 // copied verbatim from the SQE's user_data field
	Res      int32  // result: non-negative on success, -errno on error
	Flags    uint32
}

// IsError reports whether the kernel returned an error for this completion.
func (c CQE) IsError() bool {
	return c.Res < 0
}

// Err converts a negative Res to a wrapped syscall.Errno. Returns nil for
// successful completions (Res >= 0).
func (c CQE) Err() error {
	if c.Res >= 0 {
		return nil
	}
	return fmt.Errorf("iouring: %w", syscall.Errno(-c.Res))
}
```

Create `future.go`:

```go
package iouring

// Future represents the pending result of a single io_uring operation.
// Obtain a Future from Ring.SubmitAsync; call Result to block until the
// kernel posts the completion.
type Future struct {
	ch chan CQE
}

// Result blocks until the associated io_uring operation completes.
// It returns the raw CQE and, separately, any kernel error embedded in
// CQE.Res so callers can inspect both.
// If the ring is closed before the operation completes, it returns
// (CQE{}, ErrRingClosed).
func (f *Future) Result() (CQE, error) {
	cqe, ok := <-f.ch
	if !ok {
		return CQE{}, ErrRingClosed
	}
	return cqe, cqe.Err()
}
```

### Exercise 2: Ring Setup and the Low-Level API

Create `ring_linux.go`. This file holds every Linux-specific detail: the struct layout that mirrors kernel headers, the three mmap calls, the SQE accessor methods, and the synchronous submit/complete API.

```go
//go:build linux

package iouring

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mmap offset constants for the three io_uring memory regions
// (IORING_OFF_* from <linux/io_uring.h>).
const (
	iORING_OFF_SQ_RING uint64 = 0x00000000
	iORING_OFF_CQ_RING uint64 = 0x08000000
	iORING_OFF_SQES    uint64 = 0x10000000
)

// io_uring_enter flags.
const enterGetEvents uint32 = 1 << 0 // IORING_ENTER_GETEVENTS

// IOSQE flag bits written into SQE.flags.
const (
	// SQEFlagLink chains this SQE to the next one (IOSQE_IO_LINK).
	// If this SQE fails, the linked SQE is cancelled.
	SQEFlagLink uint8 = 1 << 2
)

// SQE opcode constants (IORING_OP_*).
const (
	OpNOP    uint8 = 0
	OpReadV  uint8 = 1
	OpWriteV uint8 = 2
	OpFsync  uint8 = 3
	OpRead   uint8 = 22
	OpWrite  uint8 = 23
)

// sqeSize is the fixed byte size of one SQE in the kernel's SQE array.
const sqeSize = 64

// params mirrors struct io_uring_params. The Go compiler produces the same
// field layout as the C compiler on little-endian 64-bit Linux because all
// fields are naturally aligned. Validated against the kernel source at
// include/uapi/linux/io_uring.h.
type params struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFD         uint32
	resv         [3]uint32
	sqOff        sqRingOffsets
	cqOff        cqRingOffsets
}

// sqRingOffsets mirrors struct io_sqring_offsets (40 bytes).
type sqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	flags       uint32
	dropped     uint32
	array       uint32
	resv1       uint32
	userAddr    uint64
}

// cqRingOffsets mirrors struct io_cqring_offsets (40 bytes).
type cqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	overflow    uint32
	cqes        uint32
	flags       uint32
	resv1       uint32
	userAddr    uint64
}

// SQE wraps a pointer into the kernel-mapped SQE array. Use the Prep*
// methods to fill an SQE; never write raw bytes directly.
type SQE struct {
	raw *[sqeSize]byte
}

// zero clears all 64 bytes. Always call before filling new fields so that
// union members left over from a previous operation do not bleed through.
func (s *SQE) zero() {
	for i := range s.raw {
		s.raw[i] = 0
	}
}

func (s *SQE) setOpcode(op uint8) { s.raw[0] = op }
func (s *SQE) setFlags(f uint8)   { s.raw[1] = f }

func (s *SQE) setFD(fd int32) {
	*(*int32)(unsafe.Pointer(&s.raw[4])) = fd
}
func (s *SQE) setOff(off uint64) {
	*(*uint64)(unsafe.Pointer(&s.raw[8])) = off
}
func (s *SQE) setAddr(addr uint64) {
	*(*uint64)(unsafe.Pointer(&s.raw[16])) = addr
}
func (s *SQE) setLen(l uint32) {
	*(*uint32)(unsafe.Pointer(&s.raw[24])) = l
}

// SetUserData sets the user_data field (offset 32). The kernel copies this
// into the matching CQE so callers can correlate completions with submissions.
func (s *SQE) SetUserData(d uint64) {
	*(*uint64)(unsafe.Pointer(&s.raw[32])) = d
}

// AddFlags ORs additional IOSQE_* flag bits into the flags byte.
func (s *SQE) AddFlags(f uint8) { s.raw[1] |= f }

// PrepNOP fills this SQE for a no-op (IORING_OP_NOP). The kernel immediately
// completes it with Res = 0. Use it to verify that the ring is operational.
func (s *SQE) PrepNOP() {
	s.zero()
	s.setOpcode(OpNOP)
}

// PrepRead fills this SQE for a vectorless read (IORING_OP_READ).
// buf must remain valid (not garbage collected or moved) until Result() returns.
// Pin it by keeping a reference alive in the calling function.
func (s *SQE) PrepRead(fd int, buf []byte, offset uint64) {
	s.zero()
	s.setOpcode(OpRead)
	s.setFD(int32(fd))
	s.setAddr(uint64(uintptr(unsafe.Pointer(&buf[0]))))
	s.setLen(uint32(len(buf)))
	s.setOff(offset)
}

// PrepWrite fills this SQE for a vectorless write (IORING_OP_WRITE).
// buf must remain valid until Result() returns.
func (s *SQE) PrepWrite(fd int, buf []byte, offset uint64) {
	s.zero()
	s.setOpcode(OpWrite)
	s.setFD(int32(fd))
	s.setAddr(uint64(uintptr(unsafe.Pointer(&buf[0]))))
	s.setLen(uint32(len(buf)))
	s.setOff(offset)
}

// PrepFsync fills this SQE for an fsync (IORING_OP_FSYNC).
func (s *SQE) PrepFsync(fd int) {
	s.zero()
	s.setOpcode(OpFsync)
	s.setFD(int32(fd))
}

// sqView is a user-space view into the mapped SQ ring memory.
type sqView struct {
	head  *uint32 // kernel advances head when it consumes SQEs
	tail  *uint32 // we publish the new tail here via atomic.Store
	mask  *uint32 // ring_entries-1; use & instead of %
	array *uint32 // sq_array base: sq_array[i] = SQE index for slot i
	data  []byte  // the full mapped region; held to prevent GC of the slice
}

// cqView is a user-space view into the mapped CQ ring memory.
type cqView struct {
	head *uint32 // we advance head when we consume CQEs
	tail *uint32 // kernel advances tail when it posts CQEs
	mask *uint32
	cqes uintptr // byte address of the first CQE in the mapped region
	data []byte
}

// Ring holds an io_uring instance, its three mapped memory regions, and the
// goroutine machinery for the Future-based async API.
//
// The SQ side is protected by mu. The CQ side uses only atomic operations
// (no mutex) because the poller goroutine is the sole CQ consumer.
type Ring struct {
	fd     int
	sq     sqView
	cq     cqView
	sqes   []byte // the SQE array region
	sqTail uint32 // local shadow tail; not yet visible to kernel

	mu      sync.Mutex
	pending map[uint64]chan CQE
	closed  bool
	stop    chan struct{}
	wg      sync.WaitGroup

	nextID atomic.Uint64
}

// New creates an io_uring instance with the given number of SQ entries.
// entries must be a power of two; 64 and 256 are typical choices.
// The caller must call Close when the ring is no longer needed.
func New(entries uint32) (*Ring, error) {
	var p params
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP,
		uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("iouring: setup: %w", errno)
	}

	r := &Ring{
		fd:      int(fd),
		pending: make(map[uint64]chan CQE),
		stop:    make(chan struct{}),
	}

	if err := r.mapRings(&p); err != nil {
		unix.Close(r.fd)
		return nil, err
	}

	r.wg.Add(1)
	go r.pollCQ()

	return r, nil
}

// mapRings performs the three mmap calls and initialises the sq/cq views.
// It must be called exactly once, immediately after io_uring_setup.
func (r *Ring) mapRings(p *params) error {
	// SQ ring: contains head, tail, mask, flags, and the sq_array of uint32 indices.
	sqRingSize := int(p.sqOff.array) + int(p.sqEntries)*4
	sqMem, err := unix.Mmap(r.fd, int64(iORING_OFF_SQ_RING), sqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("iouring: mmap SQ ring: %w", err)
	}

	// SQE array: one 64-byte SQE per entry.
	sqeArrSize := int(p.sqEntries) * sqeSize
	sqesMem, err := unix.Mmap(r.fd, int64(iORING_OFF_SQES), sqeArrSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		unix.Munmap(sqMem)
		return fmt.Errorf("iouring: mmap SQE array: %w", err)
	}

	// CQ ring: contains head, tail, mask, and the inline CQE array.
	cqRingSize := int(p.cqOff.cqes) + int(p.cqEntries)*16
	cqMem, err := unix.Mmap(r.fd, int64(iORING_OFF_CQ_RING), cqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		unix.Munmap(sqMem)
		unix.Munmap(sqesMem)
		return fmt.Errorf("iouring: mmap CQ ring: %w", err)
	}

	// Build typed views by adding the kernel-provided field offsets to the
	// mapped base addresses. The mmaped slices are kept in sqView.data and
	// cqView.data to prevent the GC from releasing the backing memory.
	sqBase := uintptr(unsafe.Pointer(&sqMem[0]))
	r.sq = sqView{
		head:  (*uint32)(unsafe.Pointer(sqBase + uintptr(p.sqOff.head))),
		tail:  (*uint32)(unsafe.Pointer(sqBase + uintptr(p.sqOff.tail))),
		mask:  (*uint32)(unsafe.Pointer(sqBase + uintptr(p.sqOff.ringMask))),
		array: (*uint32)(unsafe.Pointer(sqBase + uintptr(p.sqOff.array))),
		data:  sqMem,
	}
	r.sqes = sqesMem

	cqBase := uintptr(unsafe.Pointer(&cqMem[0]))
	r.cq = cqView{
		head: (*uint32)(unsafe.Pointer(cqBase + uintptr(p.cqOff.head))),
		tail: (*uint32)(unsafe.Pointer(cqBase + uintptr(p.cqOff.tail))),
		mask: (*uint32)(unsafe.Pointer(cqBase + uintptr(p.cqOff.ringMask))),
		cqes: cqBase + uintptr(p.cqOff.cqes),
		data: cqMem,
	}

	return nil
}

// sqeAt returns a typed pointer to SQE index i in the SQE array.
func (r *Ring) sqeAt(i uint32) *SQE {
	base := uintptr(unsafe.Pointer(&r.sqes[0])) + uintptr(i)*sqeSize
	return &SQE{raw: (*[sqeSize]byte)(unsafe.Pointer(base))}
}

// GetSQE returns the next free SQE slot. The returned SQE is only valid until
// the next call to Submit. The caller must call Submit after filling the SQE.
// Not safe for concurrent use without external synchronisation; use SubmitAsync
// instead when multiple goroutines submit concurrently.
func (r *Ring) GetSQE() (*SQE, error) {
	head := atomic.LoadUint32(r.sq.head) // load-acquire: see latest kernel advance
	mask := *r.sq.mask
	if r.sqTail-head > mask {
		return nil, ErrRingFull
	}
	idx := r.sqTail & mask
	// Write the SQE index into the sq_array indirection slot.
	slot := (*uint32)(unsafe.Pointer(
		uintptr(unsafe.Pointer(r.sq.array)) + uintptr(idx)*4))
	*slot = idx
	r.sqTail++
	return r.sqeAt(idx), nil
}

// Submit publishes all SQEs obtained since the last Submit and notifies the
// kernel via io_uring_enter. n must equal the number of GetSQE calls made
// since the last Submit.
func (r *Ring) Submit(n uint32) error {
	// store-release: the kernel must see all SQE writes before reading the new tail.
	atomic.StoreUint32(r.sq.tail, r.sqTail)

	_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(n), 0, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("iouring: enter: %w", errno)
	}
	return nil
}

// PeekCQE returns the oldest pending CQE without blocking. Returns ErrNoCQE
// if the completion ring is empty.
func (r *Ring) PeekCQE() (CQE, error) {
	head := atomic.LoadUint32(r.cq.head)
	tail := atomic.LoadUint32(r.cq.tail) // load-acquire: see all kernel CQE writes
	if head == tail {
		return CQE{}, ErrNoCQE
	}
	mask := *r.cq.mask
	ptr := (*CQE)(unsafe.Pointer(r.cq.cqes + uintptr(head&mask)*16))
	cqe := *ptr
	// store-release: tell the kernel this slot is free.
	atomic.StoreUint32(r.cq.head, head+1)
	return cqe, nil
}

// WaitCQE blocks until at least one CQE is available, then returns it.
// It calls io_uring_enter with IORING_ENTER_GETEVENTS when the CQ is empty.
func (r *Ring) WaitCQE() (CQE, error) {
	for {
		cqe, err := r.PeekCQE()
		if err == nil {
			return cqe, nil
		}
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
			uintptr(r.fd), 0, 1, uintptr(enterGetEvents), 0, 0)
		if errno != 0 {
			return CQE{}, fmt.Errorf("iouring: wait: %w", errno)
		}
	}
}

// Close stops the poller goroutine, unmaps all ring regions, and closes the fd.
// All pending Futures receive ErrRingClosed.
func (r *Ring) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	close(r.stop)
	r.wg.Wait()

	// Munmap after the poller has stopped so the goroutine never touches freed memory.
	unix.Munmap(r.sq.data)
	unix.Munmap(r.sqes)
	unix.Munmap(r.cq.data)
	return unix.Close(r.fd)
}
```

### Exercise 3: Goroutine-Friendly Future API and the CQ Poller

Append these functions to `ring_linux.go` (same file, same package):

```go
// SubmitAsync prepares a single SQE via the prep callback, assigns it a
// unique user_data ID, submits it to the kernel, and returns a Future that
// resolves when the kernel posts the matching CQE.
//
// prep must fill the SQE using the Prep* methods. Do not call SetUserData
// inside prep; SubmitAsync sets user_data after prep returns.
//
// buf slices passed to PrepRead/PrepWrite inside prep must remain live until
// Future.Result() returns. Keep a reference in the calling function.
func (r *Ring) SubmitAsync(prep func(*SQE)) (*Future, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, ErrRingClosed
	}

	sqe, err := r.GetSQE()
	if err != nil {
		return nil, err
	}

	id := r.nextID.Add(1)
	prep(sqe)
	sqe.SetUserData(id)

	ch := make(chan CQE, 1)
	r.pending[id] = ch

	// store-release of sqTail then io_uring_enter.
	atomic.StoreUint32(r.sq.tail, r.sqTail)
	_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), 1, 0, 0, 0, 0)
	if errno != 0 {
		delete(r.pending, id)
		return nil, fmt.Errorf("iouring: enter: %w", errno)
	}

	return &Future{ch: ch}, nil
}

// pollCQ is the background goroutine that harvests CQEs and dispatches them
// to the channels registered in r.pending. It runs until r.stop is closed.
//
// The poller uses PeekCQE (non-blocking) and yields via runtime.Gosched when
// the CQ is empty. Production code would register an eventfd with the ring
// (IORING_REGISTER_EVENTFD) to sleep instead of spin, but the spin-yield
// pattern is correct and simpler to explain.
func (r *Ring) pollCQ() {
	defer r.wg.Done()
	for {
		select {
		case <-r.stop:
			r.drainCQ()
			return
		default:
		}

		cqe, err := r.PeekCQE()
		if err != nil {
			// No completions; yield so other goroutines can run.
			// This is equivalent to runtime.Gosched() but avoids the import.
			select {
			case <-r.stop:
				r.drainCQ()
				return
			default:
			}
			continue
		}
		r.dispatchCQE(cqe)
	}
}

// dispatchCQE sends cqe to the waiting Future, if any.
func (r *Ring) dispatchCQE(cqe CQE) {
	r.mu.Lock()
	ch, ok := r.pending[cqe.UserData]
	if ok {
		delete(r.pending, cqe.UserData)
	}
	r.mu.Unlock()
	if ok {
		ch <- cqe
	}
}

// drainCQ harvests any remaining CQEs and then closes all pending channels
// with ErrRingClosed. Called by pollCQ when the ring is being shut down.
func (r *Ring) drainCQ() {
	for {
		cqe, err := r.PeekCQE()
		if err != nil {
			break
		}
		r.dispatchCQE(cqe)
	}
	r.mu.Lock()
	for _, ch := range r.pending {
		close(ch)
	}
	r.mu.Unlock()
}
```

### Exercise 4: Integration Tests and the Example Function

Create `ring_linux_test.go`:

```go
//go:build linux

package iouring

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// TestNOPRoundTrip submits a NOP and verifies the kernel completes it
// immediately with Res = 0. This is the minimal proof that the ring is wired
// correctly.
func TestNOPRoundTrip(t *testing.T) {
	t.Parallel()

	r, err := New(64)
	if err != nil {
		t.Skipf("io_uring unavailable: %v", err)
	}
	defer r.Close()

	f, err := r.SubmitAsync(func(sqe *SQE) { sqe.PrepNOP() })
	if err != nil {
		t.Fatal(err)
	}

	cqe, err := f.Result()
	if err != nil {
		t.Fatalf("NOP result error: %v", err)
	}
	if cqe.Res != 0 {
		t.Fatalf("NOP Res = %d, want 0", cqe.Res)
	}
}

// TestReadFile writes known content to a temp file, reads it back via
// io_uring, and compares the result.
func TestReadFile(t *testing.T) {
	t.Parallel()

	r, err := New(64)
	if err != nil {
		t.Skipf("io_uring unavailable: %v", err)
	}
	defer r.Close()

	tests := []struct {
		name    string
		content []byte
	}{
		{"small", []byte("hello io_uring\n")},
		{"empty", []byte{}},
		{"4k", bytes.Repeat([]byte("x"), 4096)},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmp, err := os.CreateTemp(t.TempDir(), "iouring-*")
			if err != nil {
				t.Fatal(err)
			}
			defer tmp.Close()

			if len(tc.content) > 0 {
				if _, err := tmp.Write(tc.content); err != nil {
					t.Fatal(err)
				}
			}

			got := make([]byte, len(tc.content))
			if len(got) == 0 {
				// Zero-length read: submit NOP to verify the empty case.
				f, err := r.SubmitAsync(func(sqe *SQE) { sqe.PrepNOP() })
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Result(); err != nil {
					t.Fatal(err)
				}
				return
			}

			f, err := r.SubmitAsync(func(sqe *SQE) {
				sqe.PrepRead(int(tmp.Fd()), got, 0)
			})
			if err != nil {
				t.Fatal(err)
			}

			cqe, err := f.Result()
			if err != nil {
				t.Fatalf("read error: %v", err)
			}
			if int(cqe.Res) != len(tc.content) {
				t.Fatalf("read %d bytes, want %d", cqe.Res, len(tc.content))
			}
			if !bytes.Equal(got, tc.content) {
				t.Fatalf("content mismatch:\n got %q\nwant %q", got, tc.content)
			}
		})
	}
}

// TestConcurrentFutures submits many NOP operations concurrently and confirms
// that every Future resolves exactly once with Res = 0.
func TestConcurrentFutures(t *testing.T) {
	t.Parallel()

	r, err := New(256)
	if err != nil {
		t.Skipf("io_uring unavailable: %v", err)
	}
	defer r.Close()

	const n = 100
	futures := make([]*Future, n)
	for i := range futures {
		f, err := r.SubmitAsync(func(sqe *SQE) { sqe.PrepNOP() })
		if err != nil {
			t.Fatalf("SubmitAsync[%d]: %v", i, err)
		}
		futures[i] = f
	}

	for i, f := range futures {
		cqe, err := f.Result()
		if err != nil {
			t.Errorf("future[%d] error: %v", i, err)
		}
		if cqe.Res != 0 {
			t.Errorf("future[%d] Res = %d, want 0", i, cqe.Res)
		}
	}
}

// TestCloseResolvesAllFutures verifies that Close causes all outstanding
// Futures to return ErrRingClosed.
func TestCloseResolvesAllFutures(t *testing.T) {
	t.Parallel()

	r, err := New(64)
	if err != nil {
		t.Skipf("io_uring unavailable: %v", err)
	}

	// Submit a NOP but do not call Result yet.
	f, err := r.SubmitAsync(func(sqe *SQE) { sqe.PrepNOP() })
	if err != nil {
		t.Fatal(err)
	}

	// Drain the real completion so the poller dispatches it before Close.
	cqe, err := f.Result()
	if err != nil {
		t.Fatalf("unexpected error before close: %v", err)
	}
	if cqe.Res != 0 {
		t.Fatalf("Res = %d, want 0", cqe.Res)
	}

	// Now submit without consuming, then close.
	f2, err := r.SubmitAsync(func(sqe *SQE) { sqe.PrepNOP() })
	if err != nil {
		t.Fatal(err)
	}
	_ = f2

	r.Close()

	// Your turn: call f2.Result() and assert that either (a) the NOP completed
	// with Res=0 before Close drained the ring, or (b) err wraps ErrRingClosed.
	// Use errors.Is to distinguish the two outcomes.
	_, err2 := f2.Result()
	if err2 != nil && !errors.Is(err2, ErrRingClosed) {
		t.Fatalf("got unexpected error: %v", err2)
	}
}
```

Create `ring_example_test.go` (no build constraint — compiles and runs on any platform):

```go
package iouring_test

import (
	"fmt"

	"example.com/iouring"
)

// ExampleCQE_IsError shows the two CQE outcomes: a non-negative Res is
// success; a negative Res carries a kernel errno.
func ExampleCQE_IsError() {
	success := iouring.CQE{Res: 10}  // e.g. read returned 10 bytes
	failure := iouring.CQE{Res: -11} // e.g. -EAGAIN from a non-blocking fd

	fmt.Println(success.IsError())
	fmt.Println(failure.IsError())
	// Output:
	// false
	// true
}
```

Create `cmd/demo/main.go`:

```go
//go:build linux

// Command demo writes a message to a temporary file and reads it back
// using io_uring's asynchronous API.
//
// Run with: go run ./cmd/demo
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/iouring"
)

func main() {
	// Create a temporary file with known content.
	tmp, err := os.CreateTemp("", "iouring-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(tmp.Name())

	msg := []byte("hello from io_uring\n")
	if _, err := tmp.Write(msg); err != nil {
		log.Fatal(err)
	}

	// Create an io_uring instance with 64 SQ entries.
	ring, err := iouring.New(64)
	if err != nil {
		log.Fatalf("io_uring setup failed (requires Linux 5.1+): %v", err)
	}
	defer ring.Close()

	// Submit an async NOP to verify the ring is operational.
	nopFuture, err := ring.SubmitAsync(func(sqe *iouring.SQE) {
		sqe.PrepNOP()
	})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := nopFuture.Result(); err != nil {
		log.Fatalf("NOP failed: %v", err)
	}
	fmt.Println("NOP completed OK")

	// Read the file back via io_uring.
	buf := make([]byte, len(msg))
	readFuture, err := ring.SubmitAsync(func(sqe *iouring.SQE) {
		sqe.PrepRead(int(tmp.Fd()), buf, 0)
	})
	if err != nil {
		log.Fatal(err)
	}

	cqe, err := readFuture.Result()
	if err != nil {
		log.Fatalf("read failed: %v", err)
	}

	fmt.Printf("read %d bytes: %q\n", cqe.Res, buf[:cqe.Res])
}
```

## Common Mistakes

### Forgetting the SQ Indirection Array

Wrong: treating the SQ ring as a direct circular buffer of SQEs and writing SQE fields directly at the ring tail offset.

What happens: the SQ ring holds `uint32` indices, not SQEs. Writing SQE data into the SQ ring corrupts the index array; the kernel processes garbage SQEs or panics.

Fix: the SQE array is at a separate mmap offset (`IORING_OFF_SQES`). Write `sq_array[tail & mask] = sqe_index`, then fill the SQE in the SQE array at that index.

### Plain Reads and Writes on Shared Ring Indices

Wrong:

```go
tail := *r.sq.tail // plain read — data race with kernel
*r.sq.tail = tail + 1 // plain write — kernel may see partial store
```

What happens: the race detector flags the read; on weak-memory-order architectures (ARM64), the kernel may observe the tail update before the SQE writes are visible, consuming an incomplete SQE.

Fix: use `atomic.LoadUint32` for reads (load-acquire) and `atomic.StoreUint32` for the tail publish (store-release):

```go
tail := atomic.LoadUint32(r.sq.tail)
// ... fill SQE ...
atomic.StoreUint32(r.sq.tail, tail+1)
```

### Not Zeroing the SQE Before Use

Wrong: calling `PrepRead` on a slot that previously held a `PrepFsync`, leaving the `fsync_flags` union member non-zero.

What happens: the kernel reads the stale union field and interprets it as read flags (`RWF_*`), causing unexpected behavior or EINVAL.

Fix: call `zero()` at the start of every `Prep*` method to clear all 64 bytes before writing the new fields.

### Treating Negative CQE.Res as an Integer Error Code

Wrong:

```go
if cqe.Res < 0 {
	return fmt.Errorf("failed: %d", cqe.Res)
}
```

What happens: the error code -11 prints as a meaningless number instead of "resource temporarily unavailable".

Fix: convert to `syscall.Errno(-cqe.Res)`:

```go
if cqe.Res < 0 {
	return fmt.Errorf("io_uring op: %w", syscall.Errno(-cqe.Res))
}
```

The `CQE.Err()` method does this automatically.

### Invalidating Buffers Before the Completion Arrives

Wrong: passing a stack-allocated buffer to `PrepRead`, returning from the function, and waiting for `Future.Result()` from the caller.

What happens: the Go GC may relocate or reclaim the stack frame containing the buffer before the kernel DMA completes into it, causing silent data corruption or a kernel write to freed memory.

Fix: keep the buffer in scope in the same function that calls `SubmitAsync` and blocks on `Result()`. For shared buffers, hold them in a struct that outlives the operation.

## Verification

This lesson requires Linux 5.1+ and `golang.org/x/sys`. It cannot be compiled or tested offline (no Linux kernel) or without network access (external module). The offline bar applies: validate with gofmt and go vet on the extractable platform-independent files.

On a Linux host:

```bash
cd ~/go-exercises/iouring

# Format check: must print nothing.
test -z "$(gofmt -l .)"

# Vet: must print nothing.
go vet ./...

# Build: must succeed on Linux.
go build ./...

# Integration tests: require Linux 5.1+ with a working io_uring.
# Tests skip automatically if io_uring_setup returns ENOSYS or EPERM.
go test -count=1 -race ./...

# Run the demo.
go run ./cmd/demo
```

Add one test of your own: `TestLinkedSQEs` — submit a `PrepWrite` SQE with `SQEFlagLink` set, followed by a `PrepFsync` SQE targeting the same fd, and verify that both CQEs arrive with non-negative Res. The link flag ensures fsync only executes if the write succeeds.

## Summary

- io_uring uses three mmapped regions: the SQ ring (indices), the SQE array (operations), and the CQ ring (results).
- The SQ ring is an indirection layer: `sq_array[tail & mask]` holds the SQE index; the SQE itself lives in the separate SQE array.
- Ring head/tail indices must be accessed with `atomic.LoadUint32` (load-acquire) and `atomic.StoreUint32` (store-release) to enforce correct memory ordering against the kernel.
- `io_uring_enter` submits SQEs and/or waits for CQEs in a single syscall; batching many operations per call is the primary source of throughput improvement.
- The goroutine-to-CQE bridge is a `map[uint64]chan CQE` keyed by `user_data`; a background poller goroutine dispatches completions to waiting Future channels.
- Negative `CQE.Res` is a negated `errno`; convert with `syscall.Errno(-cqe.Res)`.
- Build-constrain all Linux-specific files with `//go:build linux`; keep pure-Go types in unconstrained files so they can be tested on any platform.

## What's Next

Next: [Seccomp Filter Engine](../06-seccomp-filter/06-seccomp-filter.md).

## Resources

- [io_uring paper: "Efficient IO with io_uring" (Jens Axboe, 2019)](https://kernel.dk/io_uring.pdf) — the original design document; covers the ring protocol, mmap layout, and SQPOLL in detail.
- [io_uring(7) Linux man page](https://man7.org/linux/man-pages/man7/io_uring.7.html) — authoritative reference for all setup flags, operation codes, and field semantics.
- [io_uring_setup(2) and io_uring_enter(2) man pages](https://man7.org/linux/man-pages/man2/io_uring_setup.2.html) — exact syscall signatures, params struct layout, and error codes.
- [Lord of the io_uring (unixism.net)](https://unixism.net/loti/) — step-by-step tutorial from NOP to networked servers; useful for validating the ring protocol against a C reference.
- [golang.org/x/sys/unix package](https://pkg.go.dev/golang.org/x/sys/unix) — provides `SYS_IO_URING_SETUP`, `SYS_IO_URING_ENTER`, `SYS_IO_URING_REGISTER`, and the `Mmap`/`Munmap` wrappers used in this lesson.
