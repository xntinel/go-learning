# 8. runtime.SetFinalizer

`runtime.SetFinalizer` attaches a cleanup function to a heap-allocated object that runs after the object becomes unreachable. It is one of the most misused APIs in Go: finalizers are non-deterministic, cannot be ordered safely in cycles, block a single dedicated goroutine, and provide no guarantee of execution before program exit. Go 1.24 introduced `runtime.AddCleanup` as a safer, more composable alternative. This lesson teaches when (and when not) to use finalizers, the object-resurrection trap, the `KeepAlive` requirement, and the canonical safety-net pattern.

```text
setfinalizer/
  go.mod
  resource.go
  resource_test.go
  cmd/demo/main.go
```

## Concepts

### How SetFinalizer Works

`runtime.SetFinalizer(obj, f)` registers `f` to be called when `obj` becomes unreachable. The signature is:

```go
func SetFinalizer(obj any, finalizer any)
```

`obj` must be a pointer to a heap-allocated object (allocated by `new`, a composite literal address, or a local variable address). `finalizer` must be a function that accepts a single argument to which `obj`'s type can be assigned.

After the GC detects that `obj` is unreachable, it:
1. Clears the finalizer registration.
2. Schedules `finalizer(obj)` in a dedicated finalizer goroutine.
3. Makes `obj` reachable again until the finalizer returns.

Because the object is kept alive until the finalizer runs, collection is deferred to a subsequent GC cycle. The object is freed only if it is still unreachable after the finalizer returns and no new finalizer has been registered.

`SetFinalizer(obj, nil)` removes the finalizer without running it.

### Finalizer Goroutine and Sequencing

A single goroutine runs all finalizers for the program, sequentially in registration order. A finalizer that blocks delays every subsequent finalizer. If a finalizer needs to do long work, it should start a new goroutine:

```go
runtime.SetFinalizer(r, func(r *Resource) {
	go func() {
		// long cleanup work here
	}()
})
```

### Dependency Ordering

If A holds a pointer to B and both have finalizers, the runtime guarantees that A's finalizer runs before B's finalizer (it respects the pointer graph). However, if A and B form a cycle (A points to B, B points to A), neither finalizer is guaranteed to run at all, because there is no safe ordering. Avoid finalizers on types that participate in pointer cycles.

### Object Resurrection

A finalizer can store its argument into a global or other reachable location, making the object reachable again. This is called resurrection. After resurrection, the object lives without a finalizer; if it becomes unreachable again, it is collected without any cleanup. Resurrection is rarely intentional and is a source of subtle leaks.

### KeepAlive: The Hidden Reachability Trap

The Go compiler may optimize away references. A function argument or receiver may become unreachable at the last point the function mentions it — even before the function returns:

```go
type File struct{ fd int }

func read(f *File) {
	// Compiler may decide f is unreachable here, before syscall.Read returns.
	syscall.Read(f.fd, buf[:])
	// The finalizer could close f.fd before syscall.Read finishes.
}
```

`runtime.KeepAlive(f)` marks the point up to which `f` must remain reachable:

```go
func read(f *File) {
	syscall.Read(f.fd, buf[:])
	runtime.KeepAlive(f) // f.fd is valid at least until here
}
```

### AddCleanup: The Modern Alternative

Go 1.24 introduced `runtime.AddCleanup`:

```go
func AddCleanup[T, S any](ptr *T, cleanup func(S), arg S) Cleanup
```

Cleanups differ from finalizers in three key ways:
- They receive a separate `arg` rather than the pointer itself, eliminating the resurrection trap.
- Multiple cleanups can be registered on the same object; they run concurrently rather than in a single sequential goroutine.
- The returned `Cleanup` value has a `Stop()` method to cancel the cleanup if the object is still live.

New code should prefer `AddCleanup` over `SetFinalizer`.

### When Not to Use Finalizers

- Flushing in-memory buffers: not guaranteed to run before exit, so data is silently lost.
- Objects with zero-byte size: may share an address with another object; finalizer may never fire.
- Objects in package-level variable initializers: may be linker-allocated, not heap-allocated.
- Cyclic object graphs: no safe ordering; finalizer may never run.
- Any logic that must run on a known schedule: use `defer` or explicit `Close()`.

The canonical pattern is: `Close()` for normal cleanup, `SetFinalizer` as a last-resort leak detector that logs or panics on misclosure.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/setfinalizer/cmd/demo
cd ~/go-exercises/setfinalizer
go mod init example.com/setfinalizer
```

This is a library with a `cmd/demo` program. The library is verified with `go test`; the demo is for manual inspection.

### Exercise 1: A Managed Resource With Explicit Close

The primary cleanup path is `Close()`. The finalizer is a safety net that detects leaks.

Create `resource.go`:

```go
package setfinalizer

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
)

// ErrAlreadyClosed is returned when Close is called more than once.
var ErrAlreadyClosed = errors.New("resource already closed")

// Resource represents an external resource (e.g. a file descriptor or
// network connection). Close must be called to release it. A finalizer
// registered by New logs a warning if the caller forgets.
type Resource struct {
	mu     sync.Mutex
	name   string
	closed bool
	leaked bool // set by finalizer
}

// New allocates a Resource and registers a finalizer that logs a warning
// if Close was never called. The finalizer is a safety net, not the
// primary cleanup mechanism.
func New(name string) *Resource {
	r := &Resource{name: name}
	runtime.SetFinalizer(r, (*Resource).finalize)
	return r
}

// finalize is the last-resort cleanup. It runs in the finalizer goroutine
// some time after r becomes unreachable.
func (r *Resource) finalize() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.leaked = true
		fmt.Fprintf(os.Stderr, "WARNING: resource %q was not closed\n", r.name)
		r.closed = true
	}
}

// Close releases the resource and cancels the finalizer.
// It is idempotent: the second call returns ErrAlreadyClosed.
func (r *Resource) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("close %q: %w", r.name, ErrAlreadyClosed)
	}
	r.closed = true
	// Remove the finalizer so it does not run after explicit Close.
	runtime.SetFinalizer(r, nil)
	return nil
}

// Name returns the resource name.
func (r *Resource) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.name
}

// IsClosed reports whether the resource has been closed.
func (r *Resource) IsClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// WasLeaked reports whether the finalizer detected a missing Close call.
// This is only meaningful after the finalizer has run.
func (r *Resource) WasLeaked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leaked
}
```

### Exercise 2: Test the Resource Contract

Create `resource_test.go`:

```go
package setfinalizer

import (
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewCreatesOpenResource(t *testing.T) {
	t.Parallel()

	r := New("db-conn")
	if r.IsClosed() {
		t.Fatal("new resource should not be closed")
	}
	if r.Name() != "db-conn" {
		t.Fatalf("Name = %q, want db-conn", r.Name())
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	r := New("net-sock")
	if err := r.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	err := r.Close()
	if !errors.Is(err, ErrAlreadyClosed) {
		t.Fatalf("second Close() err = %v, want ErrAlreadyClosed", err)
	}
}

func TestCloseMarksResourceClosed(t *testing.T) {
	t.Parallel()

	r := New("file")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !r.IsClosed() {
		t.Fatal("IsClosed should be true after Close")
	}
}

// TestFinalizerDetectsLeak verifies that the finalizer sets the leaked flag
// when Close is never called. It uses an atomic counter as an observable
// side-channel so the assertion does not depend on capturing stdout/stderr.
func TestFinalizerDetectsLeak(t *testing.T) {
	// Not parallel: exercises the GC and sleeps.

	var leakCount atomic.Int32

	// newTracked wraps New and replaces the default finalizer with one that
	// also increments leakCount, giving a deterministically observable signal
	// without relying on stderr output.
	// SetFinalizer(r, nil) must precede the re-registration: Go 1.24+
	// panics if SetFinalizer is called on a pointer that already has a
	// finalizer.
	newTracked := func(name string) *Resource {
		r := New(name)
		runtime.SetFinalizer(r, nil) // clear the finalizer set by New
		runtime.SetFinalizer(r, func(r *Resource) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.closed {
				r.leaked = true
				r.closed = true
				leakCount.Add(1)
			}
		})
		return r
	}

	// Allocate and immediately discard without Close.
	func() {
		r := newTracked("leaked-fd")
		_ = r
	}()

	// Two GC cycles: first schedules the finalizer, second frees the object.
	runtime.GC()
	runtime.GC()
	// Give the finalizer goroutine time to execute.
	time.Sleep(50 * time.Millisecond)

	if leakCount.Load() == 0 {
		t.Fatal("finalizer did not detect the leak: leakCount is still 0")
	}
}

func TestCloseCancelsFinalizerNoDoubleCleanup(t *testing.T) {
	// Not parallel: exercises the GC and sleeps.

	r := New("cancelme")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// Force GC. The finalizer should NOT run because we removed it in Close.
	runtime.GC()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	if r.WasLeaked() {
		t.Fatal("finalizer ran after Close removed it — double cleanup occurred")
	}
}

func ExampleNew() {
	r := New("example-conn")
	defer func() {
		if err := r.Close(); err != nil {
			return
		}
	}()
	// Output:
}
```

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"time"

	"example.com/setfinalizer"
)

func main() {
	// Demonstrate explicit Close path.
	r1 := setfinalizer.New("explicit-close")
	if err := r1.Close(); err != nil {
		fmt.Println("close error:", err)
	}
	fmt.Println("r1 closed:", r1.IsClosed())

	// Demonstrate double-close error.
	err := r1.Close()
	fmt.Println("second close error:", err)

	// Demonstrate finalizer as safety net.
	// Drop r2 without closing; the finalizer logs the leak.
	_ = setfinalizer.New("leaked-conn")
	runtime.GC()
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
}
```

Your turn: add `TestNewRejectsEmptyName` — modify `New` to return `(nil, error)` when `name` is empty, and write a test that calls `New("")` and checks the returned error is non-nil.

## Common Mistakes

### Relying on the Finalizer as the Primary Cleanup Path

Wrong: registering a finalizer and calling no explicit `Close`, expecting the GC to handle cleanup on a known schedule.

What happens: the finalizer may run seconds or minutes later, or not at all before the program exits. Resources such as file descriptors or network sockets are exhausted while the GC defers collection.

Fix: always call `Close()` (or use `defer r.Close()`). The finalizer is a backstop that logs or counts leaks; it is not a substitute for explicit cleanup.

### Forgetting SetFinalizer(obj, nil) in Close

Wrong: implementing `Close()` that marks the object closed but leaves the finalizer registered.

What happens: when the object is eventually collected the finalizer fires again, calling close-like logic a second time. For non-idempotent operations (e.g. `syscall.Close`), this corrupts the file-descriptor table.

Fix: call `runtime.SetFinalizer(r, nil)` inside `Close()` before returning.

### Finalizer on a Zero-Size Type

Wrong: registering a finalizer on `new(struct{})` or any type with zero-byte size.

What happens: the runtime may coalesce multiple zero-size allocations at the same address. The finalizer is not guaranteed to run.

Fix: do not use finalizers on zero-size types. Use a wrapper struct that includes at least one non-zero-size field.

### Accessing Object State in the Finalizer Without Synchronization

Wrong: reading mutable fields of the object in the finalizer without a mutex.

What happens: the finalizer goroutine races with the last goroutine to write the field. The race detector flags the data race.

Fix: protect all shared mutable state with a `sync.Mutex` that is locked both in the finalizer and in the methods that write the fields (as done in the `Resource` type above).

### Finalizers in Cyclic Structures

Wrong: objects A and B each point to each other and both have finalizers.

What happens: the runtime cannot determine a safe finalizer order for the cycle. Neither finalizer is guaranteed to run.

Fix: break the cycle (use weak pointers or redesign ownership) or remove the finalizer from at least one participant.

## Verification

From `~/go-exercises/setfinalizer`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Run `go run ./cmd/demo` to observe the finalizer warning printed to stderr.

## Summary

- `runtime.SetFinalizer(obj, f)` schedules `f` to run after `obj` becomes unreachable; a second GC cycle then frees the object.
- Finalizers run sequentially in a single goroutine; a blocking finalizer delays all subsequent finalizers.
- Object resurrection: a finalizer can make the object reachable again, deferring collection to the next cycle with no finalizer.
- Always call `runtime.SetFinalizer(obj, nil)` inside `Close()` to cancel the finalizer and prevent double cleanup.
- `runtime.KeepAlive(obj)` prevents the compiler from treating `obj` as unreachable before the specified point.
- Finalizers are not guaranteed to run before program exit, on zero-size types, on cyclic structures, or on package-level variable objects.
- Go 1.24 `runtime.AddCleanup` is safer: the cleanup receives a separate argument (no resurrection), multiple cleanups per object are allowed, and each cleanup runs in its own goroutine.
- The canonical pattern: explicit `Close()` for normal cleanup; finalizer for leak detection only.

## What's Next

Next: [Go Assembly: Plan9 Syntax](../09-go-assembly-basics/09-go-assembly-basics.md).

## Resources

- [pkg.go.dev/runtime#SetFinalizer](https://pkg.go.dev/runtime#SetFinalizer) — full signature, constraints, and KeepAlive example
- [pkg.go.dev/runtime#AddCleanup](https://pkg.go.dev/runtime#AddCleanup) — Go 1.24 replacement; prefer for new code
- [go.dev/ref/spec#Size_and_alignment_guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees) — why zero-size types are unsafe for finalizers
- [go.dev/doc/faq#finalizers](https://go.dev/doc/faq#finalizers) — official FAQ on finalizer guarantees and limitations
- [cs.opensource.google/go/go/+/master:src/os/file_unix.go](https://cs.opensource.google/go/go/+/master:src/os/file_unix.go) — os.File finalizer as a real-world safety-net example
