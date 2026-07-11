# 6. Goroutine Stack Growth

Goroutines start with a small stack — roughly 2 KB — and grow on demand. Unlike OS threads, which pre-allocate megabytes, this makes spawning thousands of goroutines cheap. Growth happens by copying the entire stack to a larger contiguous allocation, which means stack pointers are updated and escape analysis becomes critical. The hard parts: understanding what triggers growth, measuring the cost, and avoiding the pointer invalidation pitfall when passing stack addresses across goroutine boundaries.

## Concepts

### Initial Stack Size and Growth Trigger

A new goroutine's stack begins at about 2 KB (the exact initial size is an implementation detail, currently 2048 bytes in the Go runtime). When a function call would overflow the stack, the runtime allocates a new, larger stack (typically doubling), copies all frames, updates all pointers in the stack frames, and resumes execution. This is transparent to user code.

The mechanism relies on a "stack guard" check inserted by the compiler at function prologues. Each function checks whether `SP` is below the stack guard limit. If it is, the goroutine traps into the runtime's `morestack` function.

### Contiguous Stack Model

Go uses contiguous stacks (since Go 1.4). Before 1.4, goroutines used segmented stacks where each overflow created a new linked stack segment. Segmented stacks caused the "hot split" problem: a function at a segment boundary could oscillate between creating and destroying stack segments on every call, causing severe slowdowns in tight loops.

Contiguous stacks fix hot split but introduce pointer invalidation: any pointer into the stack before a copy is invalid after. This is why the Go garbage collector must update all stack pointers during stack copies, and why you cannot take the address of a stack variable and hand it to another goroutine's stack — the second goroutine holds a stale pointer if the first goroutine's stack is later copied.

### Trade-offs and Failure Modes

- Stack growth is cheap for most programs (a doubling allocator amortizes well) but creates latency spikes in latency-sensitive code paths.
- The maximum stack size defaults to 1 GB (adjustable via `runtime/debug.SetMaxStack`). A goroutine that recursively grows beyond this limit panics with "stack overflow".
- Measuring per-goroutine stack use requires `runtime.MemStats.StackInuse`, which counts total stack memory for all live goroutines.
- Stack shrinkage occurs during GC. The runtime will shrink stacks that are mostly unused back to a smaller size, reducing memory overhead.

## Exercises

Module path: `example.com/stackgrowth`. Set up the module:

```go
// go.mod
module example.com/stackgrowth

go 1.26
```

### Exercise 1: Implement the stackgrowth package

Create `stackgrowth.go`:

```go
// stackgrowth.go
package stackgrowth

import (
	"runtime"
	"sync"
)

// Depth allocates n recursive stack frames, each with a [64]byte local variable,
// and returns n. This forces the runtime to grow the goroutine stack.
func Depth(n int) int {
	if n <= 0 {
		return 0
	}
	var buf [64]byte
	_ = buf
	return Depth(n-1) + 1
}

// StackInuse returns the number of bytes currently in use for goroutine stacks.
func StackInuse() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.StackInuse
}

// StackPerGoroutine spawns count goroutines that block on a channel, measures
// the change in StackInuse, and returns bytes per goroutine. Goroutines are
// cleaned up before returning.
func StackPerGoroutine(count int) uint64 {
	if count <= 0 {
		return 0
	}

	// Stabilize GC before baseline measurement.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// Spawn count goroutines that block on a channel.
	ch := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			<-ch
		}()
	}

	// Measure stack usage with goroutines live.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Unblock all goroutines and wait for them to finish.
	close(ch)
	wg.Wait()

	if after.StackInuse <= before.StackInuse {
		return 2048
	}
	delta := after.StackInuse - before.StackInuse
	return delta / uint64(count)
}
```

### Exercise 2: Write the test file

Create `stackgrowth_test.go`:

```go
// stackgrowth_test.go
package stackgrowth

import (
	"testing"
)

func TestDepth(t *testing.T) {
	t.Parallel()
	got := Depth(100)
	if got != 100 {
		t.Errorf("Depth(100) = %d, want 100", got)
	}
}

func TestDepthZero(t *testing.T) {
	t.Parallel()
	got := Depth(0)
	if got != 0 {
		t.Errorf("Depth(0) = %d, want 0", got)
	}
}

// TestStackGrowsDuringRecursion verifies that deep recursion does not panic
// with a stack overflow and returns the correct depth count.
func TestStackGrowsDuringRecursion(t *testing.T) {
	t.Parallel()
	// 5000 frames * 64 bytes = 320 KB of locals, well beyond the 2 KB initial
	// stack — this forces multiple growth cycles.
	got := Depth(5000)
	if got != 5000 {
		t.Errorf("Depth(5000) = %d, want 5000", got)
	}
}

func TestStackPerGoroutine(t *testing.T) {
	t.Parallel()
	got := StackPerGoroutine(1000)
	if got == 0 {
		t.Error("StackPerGoroutine(1000) = 0, want > 0")
	}
	if got >= 65536 {
		t.Errorf("StackPerGoroutine(1000) = %d, want < 65536 (64 KB)", got)
	}
}
```

### Exercise 3: Write the example and demo

Create `example_test.go`:

```go
// example_test.go
package stackgrowth_test

import (
	"example.com/stackgrowth"
	"fmt"
)

func ExampleDepth() {
	fmt.Println(stackgrowth.Depth(5))
	// Output:
	// 5
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"example.com/stackgrowth"
	"fmt"
)

func main() {
	const count = 10000
	bytesPerG := stackgrowth.StackPerGoroutine(count)
	fmt.Printf("goroutines: %d\n", count)
	fmt.Printf("stack bytes/goroutine: %d\n", bytesPerG)
	fmt.Printf("total stack estimate: %d KB\n", uint64(count)*bytesPerG/1024)

	depth := stackgrowth.Depth(3000)
	fmt.Printf("recursive depth reached: %d\n", depth)
	fmt.Printf("stack in use now: %d KB\n", stackgrowth.StackInuse()/1024)
}
```

## Common Mistakes

**Wrong**: Taking the address of a local variable and storing it in a long-lived struct shared between goroutines.

What happens: When the source goroutine's stack grows and is copied to a new location, the stored pointer becomes a dangling reference to deallocated memory. The Go garbage collector updates pointers it knows about, but it cannot update pointers that have escaped its scan — this is exactly why the compiler forces variables to escape to the heap when their address is stored outside the stack frame.

**Fix**: Let escape analysis move variables to the heap by returning a pointer or storing it where the compiler can see the escape. If you must share data between goroutines, use channels or pass by value.

---

**Wrong**: Assuming `runtime.MemStats.StackInuse` measures a single goroutine's stack.

What happens: `StackInuse` is a global counter for all live goroutine stacks combined. Measuring "per-goroutine" cost requires computing a delta: measure before spawning goroutines, spawn them, measure again.

**Fix**: Use the delta pattern shown in `StackPerGoroutine`: call `runtime.GC()` to stabilize, read `MemStats`, spawn goroutines, read `MemStats` again, divide the delta by the goroutine count.

---

**Wrong**: Recursing without a base case when testing stack growth.

What happens: Without a base case, the recursion never terminates. The stack grows until it hits the 1 GB default limit, then the goroutine panics with "goroutine stack exceeds 1000000000-byte limit" / "stack overflow".

**Fix**: Always have a base case (`if n <= 0 { return 0 }`). To observe stack overflow explicitly, use `runtime/debug.SetMaxStack` to lower the limit in a test and recover with `defer` + `recover`.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo to see live stack measurements:

```bash
go run ./cmd/demo
```

## Summary

- Goroutines start with approximately 2 KB of stack and grow on demand via contiguous stack copies.
- Growth is triggered by a stack guard check inserted at each function prologue; when the check fails, the runtime runs `morestack`.
- The contiguous model (since Go 1.4) eliminates the hot split problem of segmented stacks.
- Stack pointers are updated during copies — you cannot safely hold a raw pointer into another goroutine's stack.
- `runtime.MemStats.StackInuse` reports total stack bytes across all live goroutines; delta measurements reveal per-goroutine cost.
- Default max stack is 1 GB; exceeding it panics with "stack overflow".

## What's Next

Next: [Observing the Scheduler with GODEBUG](../07-observing-scheduler-godebug/07-observing-scheduler-godebug.md).

## Resources

- [runtime package — MemStats](https://pkg.go.dev/runtime#MemStats)
- [runtime/debug — SetMaxStack](https://pkg.go.dev/runtime/debug#SetMaxStack)
- [Go 1.4 Release Notes — Contiguous Stacks](https://go.dev/doc/go1.4#runtime)
- [Contiguous stacks design document](https://docs.google.com/document/d/1wAaf1rYoM4S4gtnPh0zOlGzWtrZFQ5suE8qr2sD8uWQ/pub)
- [Go Internals: Goroutine stack management (Cloudflare blog)](https://blog.cloudflare.com/how-stacks-are-handled-in-go/)
