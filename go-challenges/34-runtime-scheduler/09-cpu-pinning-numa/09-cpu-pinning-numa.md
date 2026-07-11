# 9. CPU Pinning and NUMA

CPU affinity pins an OS thread to a specific CPU core, preventing the kernel from migrating it. In NUMA systems, this also keeps threads close to the memory they access, eliminating inter-node memory latency. Go's scheduler normally prevents you from controlling which physical core a goroutine runs on — but `runtime.LockOSThread()` pins a goroutine to its current OS thread, and `sched_setaffinity(2)` (Linux only) pins that thread to a specific core. Understanding when and why to combine these tools, and what the portability trade-offs are, is the focus of this lesson.

## Concepts

### runtime.LockOSThread

`runtime.LockOSThread()` wires the calling goroutine to its current OS thread (M). Normally the scheduler is free to move a goroutine from one M to another between scheduling points. After `LockOSThread()`:
- The goroutine stays on the same M until `runtime.UnlockOSThread()` is called.
- Other goroutines cannot run on that M while the lock is held.
- The thread is unavailable for the work-stealing pool.

This is the cross-platform part of CPU pinning and is sufficient for CGO callbacks, OS-thread-local state, and ensuring that a subsequent affinity syscall applies to the correct thread.

### sched_setaffinity (Linux only)

`sched_setaffinity(2)` is a Linux syscall that sets the CPU affinity mask for a thread. Combined with `LockOSThread`, it pins a goroutine to a specific physical core:

```
LockOSThread() -> sched_setaffinity(core N) -> goroutine runs only on core N
```

This has no portable equivalent on macOS or Windows. On macOS, `thread_policy_set` with `THREAD_AFFINITY_POLICY` offers soft hints only. This lesson keeps Linux-specific code in a `//go:build linux` file and provides a no-op stub for other platforms.

### NUMA Topology

In NUMA (Non-Uniform Memory Access) systems, memory is divided into nodes, each attached to a group of CPU cores. Accessing memory on a remote node costs 2-4x more latency than accessing local node memory. Affinity pinning can keep a goroutine's thread on the same NUMA node as its data, reducing memory latency. Go does not expose NUMA topology in the standard library; reading `/sys/devices/system/cpu/cpu0/topology/` or using platform tools is required for production NUMA-aware code.

### Trade-offs

CPU pinning benefits narrow, latency-sensitive workloads (network packet processing, real-time audio, HFT). For general-purpose servers, pinning is usually harmful: it prevents the OS and Go scheduler from distributing load, and a pinned goroutine blocks its M from running other goroutines. The Go scheduler's work-stealing mechanism is the right tool for most programs.

### Portability

`unix.SchedSetaffinity` (from `golang.org/x/sys/unix`) and the raw `syscall.SYS_SCHED_SETAFFINITY` syscall are Linux-only. Code using them will not compile on macOS or Windows. Always isolate platform-specific code with `//go:build linux` and provide a cross-platform stub.

## Exercises

Module path: `example.com/pinning`. Set up the module:

```go
// go.mod
module example.com/pinning

go 1.26
```

### Exercise 1: Implement the pinning package

Create `pinning.go`:

```go
// pinning.go
package pinning

import (
	"runtime"
	"sync"
)

// WorkerConfig holds configuration for a PinnedWorker.
type WorkerConfig struct {
	ID         int
	Iterations int
}

// SumWork is a reference work function: it returns the sum of [0, cfg.Iterations).
func SumWork(cfg WorkerConfig) int {
	sum := 0
	for i := 0; i < cfg.Iterations; i++ {
		sum += i
	}
	return sum
}

// PinnedWorker calls runtime.LockOSThread, runs work(cfg), calls
// runtime.UnlockOSThread, and returns the result.
func PinnedWorker(cfg WorkerConfig, work func(cfg WorkerConfig) int) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return work(cfg)
}

// RunPool runs n PinnedWorkers concurrently. Each worker computes
// SumWork with the given iters. Returns results in ID order.
func RunPool(n int, iters int) []int {
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := WorkerConfig{ID: i, Iterations: iters}
			results[i] = PinnedWorker(cfg, SumWork)
		}()
	}
	wg.Wait()
	return results
}
```

Create `pinning_linux.go`:

```go
//go:build linux

// pinning_linux.go
package pinning

import (
	"syscall"
	"unsafe"
)

// cpuSet mirrors the kernel cpu_set_t (1024-bit bitfield, 16 uint64 words).
type cpuSet [1024 / 64]uintptr

// SetAffinity pins the calling goroutine's OS thread to the given CPU core.
// Must be called after runtime.LockOSThread() so the goroutine remains on
// the pinned thread.
// Returns syscall.EINVAL if core is out of [0, 1023], or a syscall error.
func SetAffinity(core int) error {
	if core < 0 || core >= 1024 {
		return syscall.EINVAL
	}
	var mask cpuSet
	mask[core/64] |= 1 << uint(core%64)
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_SETAFFINITY,
		0,
		unsafe.Sizeof(mask),
		uintptr(unsafe.Pointer(&mask[0])),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
```

Create `pinning_other.go`:

```go
//go:build !linux

// pinning_other.go
package pinning

// SetAffinity is a no-op on non-Linux platforms.
// CPU affinity requires platform-specific syscalls not available here.
func SetAffinity(_ int) error {
	return nil
}
```

### Exercise 2: Write the test file

Create `pinning_test.go`:

```go
// pinning_test.go
package pinning

import (
	"testing"
)

func TestSumWork(t *testing.T) {
	t.Parallel()
	cases := []struct {
		iters int
		want  int
	}{
		{0, 0},
		{1, 0},
		{5, 10},
		{10, 45},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			cfg := WorkerConfig{ID: 0, Iterations: tc.iters}
			got := SumWork(cfg)
			if got != tc.want {
				t.Errorf("SumWork(%d) = %d, want %d", tc.iters, got, tc.want)
			}
		})
	}
}

func TestPinnedWorker(t *testing.T) {
	t.Parallel()
	cfg := WorkerConfig{ID: 0, Iterations: 10}
	got := PinnedWorker(cfg, SumWork)
	if got != 45 {
		t.Errorf("PinnedWorker result = %d, want 45", got)
	}
}

func TestRunPool(t *testing.T) {
	t.Parallel()
	const n = 4
	const iters = 10
	results := RunPool(n, iters)
	if len(results) != n {
		t.Fatalf("RunPool returned %d results, want %d", len(results), n)
	}
	want := 45 // sum(0..9)
	for i, v := range results {
		if v != want {
			t.Errorf("results[%d] = %d, want %d", i, v, want)
		}
	}
}

func TestSetAffinityNoError(t *testing.T) {
	t.Parallel()
	// SetAffinity(0) should succeed on Linux (pinned to core 0)
	// and return nil on other platforms (no-op).
	// We do not assert success because in CI environments,
	// some containers restrict affinity syscalls.
	_ = SetAffinity(0)
}
```

### Exercise 3: Example and demo

Create `example_test.go`:

```go
// example_test.go
package pinning_test

import (
	"example.com/pinning"
	"fmt"
)

func ExampleSumWork() {
	cfg := pinning.WorkerConfig{ID: 0, Iterations: 5}
	fmt.Println(pinning.SumWork(cfg))
	// Output:
	// 10
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"example.com/pinning"
	"fmt"
)

func main() {
	const n = 8
	const iters = 1_000_000

	fmt.Printf("running %d pinned workers, each summing [0, %d)\n", n, iters)
	results := pinning.RunPool(n, iters)

	for i, v := range results {
		fmt.Printf("worker %d: sum = %d\n", i, v)
	}

	// Demonstrate SetAffinity (cross-platform: no-op on non-Linux).
	err := pinning.SetAffinity(0)
	if err != nil {
		fmt.Printf("SetAffinity(0): %v (not fatal)\n", err)
	} else {
		fmt.Println("SetAffinity(0): OK (no-op on non-Linux, pinned to core 0 on Linux)")
	}
}
```

## Common Mistakes

**Wrong**: Calling `SetAffinity` without first calling `runtime.LockOSThread()`.

What happens: The goroutine may be moved to a different OS thread between the `SetAffinity` call and the work it is supposed to pin. The affinity mask is set on the original thread, but the goroutine runs on a different thread with its original (unpinned) affinity.

**Fix**: Always call `runtime.LockOSThread()` before `SetAffinity`. Use `defer runtime.UnlockOSThread()` to ensure cleanup.

---

**Wrong**: Pinning every goroutine in a general-purpose server.

What happens: Each pinned goroutine holds an M exclusively, burning OS threads. The work-stealing scheduler cannot rebalance. Under load, you may end up with more pinned threads than CPU cores, causing thrashing.

**Fix**: Use pinning only for latency-sensitive goroutines with real affinity requirements (e.g., packet I/O, real-time audio, NUMA-local data structures). Let the Go scheduler handle the rest.

---

**Wrong**: Placing `//go:build linux` code in a file without the matching `//go:build !linux` stub.

What happens: The `SetAffinity` function exists only on Linux. Code that calls `SetAffinity` in a shared file will fail to compile on macOS and Windows with "undefined: SetAffinity".

**Fix**: For every `//go:build <platform>` file that exports a symbol, create a corresponding `//go:build !<platform>` stub that provides the same signature with a safe default.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo:

```bash
go run ./cmd/demo
```

## Summary

- `runtime.LockOSThread()` pins a goroutine to its current OS thread — this is the cross-platform mechanism.
- `sched_setaffinity(2)` (Linux only) further pins the OS thread to a specific CPU core.
- Isolate Linux-specific affinity code with `//go:build linux` and provide a `//go:build !linux` no-op stub.
- CPU pinning benefits narrow latency-sensitive workloads; it harms general-purpose schedulability.
- NUMA topology is not exposed by the Go standard library; use `/sys/devices/system/cpu/` or platform tools.
- `PinnedWorker` is the cross-platform pattern: lock thread, do work, unlock thread.

## What's Next

Next: [Scheduler-Friendly Algorithms](../10-scheduler-friendly-algorithms/10-scheduler-friendly-algorithms.md).

## Resources

- [runtime.LockOSThread](https://pkg.go.dev/runtime#LockOSThread)
- [sched_setaffinity(2) man page](https://man7.org/linux/man-pages/man2/sched_setaffinity.2.html)
- [syscall package](https://pkg.go.dev/syscall)
- [Go build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints)
- [NUMA Topology — Linux kernel docs](https://www.kernel.org/doc/html/latest/admin-guide/mm/numa_memory_policy.html)
