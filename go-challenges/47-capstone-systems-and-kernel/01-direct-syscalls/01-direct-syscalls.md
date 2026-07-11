# 1. Direct System Calls

Go's `os`, `io`, and `net` packages are thin wrappers over a small set of Linux kernel interfaces. Every file open, every socket read, every process fork is ultimately a single machine instruction — `SYSCALL` on x86-64, `SVC` on arm64 — that switches privilege level and transfers control to the kernel. Stripping the abstraction away reveals the exact contract between a Go process and the kernel: how arguments travel in registers, how errors come back as errno values, and why the Go scheduler must be told when a goroutine is about to block.

This lesson builds `package rawsys`, a Linux-only library that wraps `openat`, `read`, `write`, `close`, `lseek`, `mmap`, `munmap`, `getpid`, and `kill` using `syscall.Syscall` directly, without touching `os`, `io`, or `net` in the implementation. All source files carry `//go:build linux`.

## Concepts

### The SYSCALL Instruction

On x86-64 Linux, userspace places the syscall number in RAX and up to six arguments in RDI, RSI, RDX, R10, R8, R9, then executes the `SYSCALL` instruction. The CPU switches to kernel mode, the kernel handler runs, places the result in RAX, and executes `SYSRET` to return to userspace. A return value in the range `[-4096, -1]` encodes a negated errno.

Go's `syscall.Syscall(trap, a1, a2, a3)` wraps this exactly: `trap` goes in RAX, arguments go in the correct registers, and the function returns `(r1, r2 uintptr, errno Errno)`. `syscall.Syscall6` covers syscalls that take more than three arguments.

### Syscall vs RawSyscall

`syscall.Syscall` and `syscall.RawSyscall` differ in exactly one way: `Syscall` notifies the Go runtime scheduler before and after the kernel boundary so the scheduler can park this goroutine and wake another OS thread if this one blocks. `RawSyscall` skips that notification entirely.

`RawSyscall` is only safe for syscalls that are guaranteed not to block — `getpid` is the canonical example. Using `RawSyscall` for a potentially blocking call (read, write, accept) starves the scheduler: no other goroutine can run on that OS thread while it waits inside the kernel.

### The syscall Package vs golang.org/x/sys/unix

The `syscall` package is part of the Go standard library but is frozen: no new constants, types, or functions are added because any addition risks breaking existing code. Modern Linux syscalls — `io_uring`, `landlock`, `pidfd_open`, `clone3` — live exclusively in `golang.org/x/sys/unix`, which is actively maintained and provides better type safety for sockaddrs and `ioctl` arguments.

New production code that requires low-level syscall access should import `golang.org/x/sys/unix`. This lesson uses `syscall` for a stdlib-only demonstration that can be built without network access.

### EINTR: Interrupted System Calls

When a signal is delivered to a thread blocked inside a syscall, the kernel returns `EINTR`. The call made no progress and must be retried. The Go runtime handles EINTR internally for calls it makes on behalf of goroutines; raw wrappers written by the application must handle it via an explicit retry loop.

There is one critical exception: **do not retry `close` on EINTR on Linux.** The kernel releases the file descriptor before `close(2)` returns EINTR. Retrying `close` races against any other goroutine that opens a file concurrently and receives the same fd number. The conservative, correct approach on Linux is to call `close` once and treat EINTR as a non-fatal condition.

### errno Is Already an Error

`syscall.Errno` implements the `error` interface. Constants such as `syscall.ENOENT`, `syscall.EBADF`, and `syscall.EINTR` are typed values of type `syscall.Errno`. They satisfy `errors.Is` directly:

```go
if errors.Is(err, syscall.ENOENT) { ... }
```

Raw syscall wrappers should return `errno` values as-is so callers can use `errors.Is`. Add context with `fmt.Errorf("open %s: %w", path, err)` at the call site, not inside the generic wrapper.

### Memory Mapping and the GC

`mmap(2)` returns a region of memory managed entirely by the kernel. The Go garbage collector has no knowledge of it: the GC will not trace pointers into the region, will not move it, and will not call `munmap` when the slice goes out of scope. You are responsible for tracking the length and calling `Munmap` explicitly.

The raw `SYS_MMAP` call returns a `uintptr`. Converting that to a `[]byte` requires `unsafe.Slice((*byte)(unsafe.Pointer(addr)), length)`. This conversion is safe for kernel-returned addresses (the GC cannot move them) but `go vet` flags it as "possible misuse of unsafe.Pointer" because vet cannot distinguish kernel addresses from Go heap addresses. The `syscall.Mmap` stdlib wrapper performs this conversion through an internal path that vet exempts. For user-code packages, delegating to `syscall.Mmap` is the clean approach; the Concepts section shows what it does internally.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/rawsys/cmd/demo
cd ~/go-exercises/rawsys
go mod init example.com/rawsys
```

### Exercise 1: Build the rawsys Package

Create `rawsys.go`. This file contains the entire implementation: file I/O, process introspection, and memory mapping.

```go
//go:build linux

package rawsys

import (
	"syscall"
	"unsafe"
)

// atFDCWD is AT_FDCWD (-100) expressed as a uintptr constant.
// In two's complement, ^uintptr(0)-99 is 0xFF...FF9C, which the Linux kernel
// reads as the signed int value -100 when interpreting the openat dirfd argument.
// The syscall package exposes this as _AT_FDCWD (unexported); we define it here.
const atFDCWD = ^uintptr(0) - 99

// Open opens or creates the file at path with the given flags and mode.
// It calls openat(2) with AT_FDCWD rather than open(2) because SYS_OPEN is
// absent on arm64; SYS_OPENAT is present on both amd64 and arm64.
func Open(path string, flags int, mode uint32) (int, error) {
	name, err := syscall.BytePtrFromString(path)
	if err != nil {
		return -1, err
	}
	r1, _, errno := syscall.Syscall6(
		syscall.SYS_OPENAT,
		atFDCWD,
		uintptr(unsafe.Pointer(name)),
		uintptr(flags),
		uintptr(mode),
		0,
		0,
	)
	if errno != 0 {
		return -1, errno
	}
	return int(r1), nil
}

// Read reads up to len(buf) bytes from fd into buf, retrying on EINTR.
// A return of (0, nil) signals EOF.
func Read(fd int, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	for {
		r1, _, errno := syscall.Syscall(
			syscall.SYS_READ,
			uintptr(fd),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
		)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return 0, errno
		}
		return int(r1), nil
	}
}

// Write writes buf to fd, retrying on EINTR.
// It may write fewer bytes than len(buf) (short write) on sockets and pipes;
// callers that need all bytes written should use WriteAll.
func Write(fd int, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	for {
		r1, _, errno := syscall.Syscall(
			syscall.SYS_WRITE,
			uintptr(fd),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
		)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return 0, errno
		}
		return int(r1), nil
	}
}

// WriteAll calls Write in a loop until all bytes in buf have been written
// or an error occurs.
func WriteAll(fd int, buf []byte) error {
	for len(buf) > 0 {
		n, err := Write(fd, buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	return nil
}

// Close closes fd.
// It does NOT retry on EINTR: on Linux the kernel releases the file descriptor
// before returning EINTR, so a retry would close a concurrently opened fd.
func Close(fd int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_CLOSE, uintptr(fd), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// Lseek repositions the file offset of fd.
// Use io.SeekStart (0), io.SeekCurrent (1), or io.SeekEnd (2) for whence.
func Lseek(fd int, offset int64, whence int) (int64, error) {
	r1, _, errno := syscall.Syscall(
		syscall.SYS_LSEEK,
		uintptr(fd),
		uintptr(offset),
		uintptr(whence),
	)
	if errno != 0 {
		return 0, errno
	}
	return int64(r1), nil
}

// Getpid returns the calling process's PID via a raw syscall.
// RawSyscall is correct here because getpid(2) never blocks.
func Getpid() int {
	r1, _, _ := syscall.RawSyscall(syscall.SYS_GETPID, 0, 0, 0)
	return int(r1)
}

// Kill sends signal sig to the process identified by pid.
// Pass sig=0 to probe whether the process exists without delivering a signal.
func Kill(pid int, sig syscall.Signal) error {
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_KILL,
		uintptr(pid),
		uintptr(sig),
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// Mmap maps length bytes of fd starting at offset into memory with prot and flags.
// Pass fd=-1 and flags=syscall.MAP_ANONYMOUS|syscall.MAP_PRIVATE for an anonymous
// mapping backed by zeroed kernel pages.
// The returned slice is backed by kernel-managed memory; the GC does not track it.
// The caller must call Munmap when done.
//
// Implementation note: the raw mmap(2) syscall returns a uintptr (kernel address).
// Converting that uintptr to unsafe.Pointer requires special compiler treatment
// (unsafe.Pointer rule 4) that go vet flags as "possible misuse" when done in
// user packages. syscall.Mmap handles this conversion internally via an exempted
// pattern (see syscall/syscall_unix.go mmapper.Mmap). We delegate here rather
// than reproduce the flagged pattern.
func Mmap(fd int, offset int64, length int, prot int, flags int) ([]byte, error) {
	if length <= 0 {
		return nil, syscall.EINVAL
	}
	return syscall.Mmap(fd, offset, length, prot, flags)
}

// Munmap unmaps a region previously returned by Mmap.
func Munmap(data []byte) error {
	return syscall.Munmap(data)
}
```

The `Write` / `WriteAll` split mirrors the POSIX `write(2)` contract: on pipes and sockets a single call may write fewer bytes than requested. On a local regular file it typically does not, but the wrapper must not assume it.

`Mmap` and `Munmap` delegate to `syscall.Mmap` and `syscall.Munmap`, which are themselves direct wrappers over `SYS_MMAP` and `SYS_MUNMAP`. The delegation avoids reproducing the `uintptr`-to-`unsafe.Pointer` conversion that `go vet` correctly flags in user packages; see the Concepts section on Memory Mapping for what the stdlib does internally.

### Exercise 2: Test the Contract

Create `rawsys_test.go`. The tests are the verification — there is no main program to eyeball:

```go
//go:build linux

package rawsys_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"

	"example.com/rawsys"
)

func TestWriteAllReadRoundTrip(t *testing.T) {
	t.Parallel()

	tmp, err := os.CreateTemp("", "rawsys-rw-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	name := tmp.Name()
	tmp.Close()

	fd, err := rawsys.Open(name, syscall.O_RDWR, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = rawsys.Close(fd) })

	want := []byte("kernel contact established\n")
	if err := rawsys.WriteAll(fd, want); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	if _, err = rawsys.Lseek(fd, 0, io.SeekStart); err != nil {
		t.Fatalf("Lseek: %v", err)
	}

	got := make([]byte, len(want))
	n, err := rawsys.Read(fd, got)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got[:n]) != string(want) {
		t.Fatalf("round-trip: got %q, want %q", got[:n], want)
	}
}

func TestOpenNonexistentReturnsENOENT(t *testing.T) {
	t.Parallel()

	_, err := rawsys.Open("/no/such/rawsys/path", syscall.O_RDONLY, 0)
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("err = %v, want ENOENT", err)
	}
}

func TestCloseInvalidFdReturnsEBADF(t *testing.T) {
	t.Parallel()

	if err := rawsys.Close(-1); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("Close(-1): err = %v, want EBADF", err)
	}
}

func TestReadEmptyBufReturnsZero(t *testing.T) {
	t.Parallel()

	// An empty buf must not call the kernel (avoids a nil &buf[0] panic).
	// We pass fd=0 (stdin): even if stdin is readable, len(buf)==0 returns early.
	n, err := rawsys.Read(0, nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(0, nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestGetpidMatchesRuntime(t *testing.T) {
	t.Parallel()

	if got, want := rawsys.Getpid(), os.Getpid(); got != want {
		t.Fatalf("Getpid() = %d, os.Getpid() = %d", got, want)
	}
}

func TestKillZeroSignalSelf(t *testing.T) {
	t.Parallel()

	// Signal 0 does not deliver a signal; it checks that the target process
	// exists and that the caller has permission to signal it.
	if err := rawsys.Kill(rawsys.Getpid(), 0); err != nil {
		t.Fatalf("Kill(self, 0): %v", err)
	}
}

func TestMmapFileContent(t *testing.T) {
	t.Parallel()

	content := []byte("mmap round-trip content")
	tmp, err := os.CreateTemp("", "rawsys-mmap-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	fd, err := rawsys.Open(tmp.Name(), syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = rawsys.Close(fd) })

	data, err := rawsys.Mmap(fd, 0, len(content), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("Mmap: %v", err)
	}
	t.Cleanup(func() {
		if err := rawsys.Munmap(data); err != nil {
			t.Errorf("Munmap: %v", err)
		}
	})

	if string(data) != string(content) {
		t.Fatalf("mmap read = %q, want %q", data, content)
	}
}

func TestMmapAnonymousZeroInitialized(t *testing.T) {
	t.Parallel()

	const size = 4096
	data, err := rawsys.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANONYMOUS|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("Mmap anonymous: %v", err)
	}
	t.Cleanup(func() {
		if err := rawsys.Munmap(data); err != nil {
			t.Errorf("Munmap: %v", err)
		}
	})

	// Anonymous mappings are zero-initialized by the kernel.
	for i, b := range data {
		if b != 0 {
			t.Fatalf("data[%d] = %d, want 0 (anonymous mapping must be zero-initialized)", i, b)
		}
	}

	copy(data, []byte("hello from mmap"))
	if string(data[:15]) != "hello from mmap" {
		t.Fatalf("mmap write/read: got %q", data[:15])
	}
}

// ExampleGetpid confirms that Getpid returns a positive integer.
func ExampleGetpid() {
	pid := rawsys.Getpid()
	if pid > 0 {
		fmt.Println("pid is positive")
	}
	// Output:
	// pid is positive
}

// ExampleWrite demonstrates writing a small buffer through a raw write(2) call.
func ExampleWrite() {
	tmp, err := os.CreateTemp("", "rawsys-ex-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	name := tmp.Name()
	tmp.Close()

	fd, err := rawsys.Open(name, syscall.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer rawsys.Close(fd) //nolint:errcheck

	n, err := rawsys.Write(fd, []byte("hello"))
	if err != nil {
		return
	}
	fmt.Printf("wrote %d bytes\n", n)
	// Output:
	// wrote 5 bytes
}
```

Your turn: add `TestWriteEmptyBufDoesNothing` — call `rawsys.WriteAll(fd, []byte{})` on a valid fd (use a temp file opened with `O_WRONLY`) and assert that the result is `nil` and the file remains empty. This pins the `len(buf) == 0` early-return guard in `Write`.

### Exercise 3: Command-Line Demo

Create `cmd/demo/main.go`. This file exercises only the exported API and can be run directly:

```go
//go:build linux

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"syscall"

	"example.com/rawsys"
)

func main() {
	// Process identity: compare raw syscall result against the runtime.
	rawPID := rawsys.Getpid()
	runtimePID := os.Getpid()
	fmt.Printf("getpid via syscall.RawSyscall: %d\n", rawPID)
	fmt.Printf("os.Getpid():                   %d\n", runtimePID)
	if rawPID != runtimePID {
		log.Fatalf("pid mismatch: syscall=%d runtime=%d", rawPID, runtimePID)
	}

	// File I/O using only rawsys (no os.File).
	tmp, err := os.CreateTemp("", "rawsys-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	name := tmp.Name()
	tmp.Close()

	fd, err := rawsys.Open(name, syscall.O_RDWR, 0)
	if err != nil {
		log.Fatalf("open %s: %v", name, err)
	}
	defer rawsys.Close(fd) //nolint:errcheck

	msg := []byte("written via raw write(2)\n")
	if err := rawsys.WriteAll(fd, msg); err != nil {
		log.Fatalf("WriteAll: %v", err)
	}
	fmt.Printf("wrote:  %q\n", msg)

	if _, err := rawsys.Lseek(fd, 0, io.SeekStart); err != nil {
		log.Fatalf("Lseek: %v", err)
	}

	buf := make([]byte, len(msg))
	n, err := rawsys.Read(fd, buf)
	if err != nil {
		log.Fatalf("Read: %v", err)
	}
	fmt.Printf("read:   %q\n", buf[:n])

	// Anonymous memory mapping: kernel-allocated, GC-invisible.
	const pageSize = 4096
	mem, err := rawsys.Mmap(-1, 0, pageSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANONYMOUS|syscall.MAP_PRIVATE)
	if err != nil {
		log.Fatalf("Mmap anonymous: %v", err)
	}
	copy(mem, []byte("anonymous mmap page"))
	fmt.Printf("mmap:   %q\n", mem[:19])
	if err := rawsys.Munmap(mem); err != nil {
		log.Fatalf("Munmap: %v", err)
	}
}
```

Run on Linux with `go run ./cmd/demo`. The PIDs in the output differ per run; everything else is deterministic.

## Common Mistakes

### Using RawSyscall for a Blocking Call

Wrong: `syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), ...)` on a socket or pipe.

What happens: the goroutine parks in the kernel while the Go scheduler cannot run other goroutines on that OS thread. Under load the program appears to hang.

Fix: use `syscall.Syscall` for anything that might block. `RawSyscall` is only correct for calls that are guaranteed not to block, like `getpid`. Every `Read`, `Write`, and `Open` in this lesson uses `Syscall`, not `RawSyscall`.

### Retrying close on EINTR

Wrong:

```go
for {
	_, _, errno := syscall.Syscall(syscall.SYS_CLOSE, uintptr(fd), 0, 0)
	if errno == syscall.EINTR {
		continue // wrong on Linux
	}
	// ...
}
```

What happens: on Linux the kernel releases the file descriptor before `close(2)` returns EINTR. The fd number is now free. A concurrent goroutine may open a file and receive the same fd number. The retry then closes that goroutine's file descriptor.

Fix: call `close` once. Treat EINTR as non-fatal. This is exactly what `rawsys.Close` above does.

### Taking &buf[0] on an Empty Slice

Wrong: the first version of `Read` without the length guard:

```go
r1, _, errno := syscall.Syscall(syscall.SYS_READ, uintptr(fd),
	uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
```

What happens: `&buf[0]` panics with "index out of range" when `len(buf) == 0`. The same applies to `Write`.

Fix: add `if len(buf) == 0 { return 0, nil }` before taking the address. Both `Read` and `Write` above include this guard.

### Converting a Go Pointer to uintptr Before the Syscall Call

Wrong:

```go
ptr := uintptr(unsafe.Pointer(&buf[0])) // GC may move buf here
syscall.Syscall(syscall.SYS_READ, uintptr(fd), ptr, uintptr(len(buf)))
```

What happens: the GC may run and move `buf` in memory in the gap between the `uintptr` conversion and the `Syscall` call. `ptr` now holds a stale address; the kernel reads or writes the wrong memory.

Fix: perform the conversion inline inside the `Syscall` argument list. The Go compiler guarantees a `uintptr(unsafe.Pointer(...))` expression inside a syscall call is not split across a GC safepoint:

```go
syscall.Syscall(syscall.SYS_READ, uintptr(fd),
	uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
```

This is the pattern used in every function above.

## Verification

From `~/go-exercises/rawsys`, format and vet checks can be run on any host with cross-compilation:

```bash
test -z "$(gofmt -l .)"
GOOS=linux GOARCH=amd64 go vet ./...
```

On a Linux host, run the full suite:

```bash
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All four must be clean. Add the test suggested at the end of Exercise 2 before running.

## Summary

- `syscall.Syscall` notifies the Go scheduler before and after the kernel boundary; `syscall.RawSyscall` does not. Use `RawSyscall` only for syscalls that never block.
- The `syscall` package is frozen; `golang.org/x/sys/unix` is the maintained replacement with all modern Linux syscall constants and type-safe wrappers.
- EINTR means the call was interrupted by a signal and made no progress — retry it. Exception on Linux: do not retry `close`, because the fd is already released before EINTR is returned.
- `syscall.Errno` implements `error` and satisfies `errors.Is` without wrapping; use it directly as a sentinel.
- Mmap memory is invisible to the GC: track the length, call `Munmap` explicitly, and convert the kernel address to `[]byte` with `unsafe.Slice`.
- Guard every `&buf[0]` with a `len(buf) == 0` check. Perform all `unsafe.Pointer` conversions inline inside the `Syscall` argument list to prevent GC interference.

## What's Next

Next: [eBPF Tracing Tool](../02-ebpf-tracing/02-ebpf-tracing.md).

## Resources

- [Go syscall package](https://pkg.go.dev/syscall) — constants, types, and low-level function signatures used throughout this lesson
- [Go unsafe package](https://pkg.go.dev/unsafe#Pointer) — the six valid conversions between `unsafe.Pointer` and `uintptr`; rules (1) and (4) apply here
- [golang.org/x/sys/unix](https://pkg.go.dev/golang.org/x/sys/unix) — the actively maintained replacement for `syscall`; includes all modern Linux syscall constants
- [Linux man pages: openat(2), read(2), write(2), close(2), mmap(2)](https://man7.org/linux/man-pages/) — authoritative reference for each syscall's behavior, short-write semantics, and when EINTR is returned
- [The Go Blog: The Laws of Reflection — unsafe.Pointer rules](https://go.dev/blog/laws-of-reflection) — background on safe use of `unsafe.Pointer` in syscall wrappers
