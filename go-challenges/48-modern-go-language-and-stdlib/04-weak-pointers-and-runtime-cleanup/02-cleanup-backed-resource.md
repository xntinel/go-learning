# Exercise 2: A Cleanup-Backed Resource

The canonical use of `runtime.AddCleanup` is not pruning a cache; it is releasing an external handle a wrapper object owns, with the cleanup acting as a backstop for the day a caller forgets to `Close`. This exercise builds that pattern and exercises the three rules that make a cleanup work, including the `Cleanup.Stop()` that keeps the release exactly-once.

This module is fully self-contained. It begins with its own `go mod init`, defines the `Resource` type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
resource.go          Resource; OpenResource, Close   // runtime.AddCleanup + Cleanup.Stop
cmd/
  demo/
    main.go          Close the happy path, then forget one and watch the GC release it
resource_test.go     forgotten-resource backstop, Close stops the cleanup, AddCleanup fires
```

- Files: `resource.go`, `cmd/demo/main.go`, `resource_test.go`.
- Implement: `Resource` with `OpenResource(*atomic.Bool) *Resource` and `(*Resource).Close()`.
- Test: `resource_test.go` proves a forgotten resource is released by the GC backstop, that `Close` calls `Stop()` so the cleanup does not also fire, and that `AddCleanup` fires after collection.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p cleanup-backed-resource/cmd/demo && cd cleanup-backed-resource
go mod init example.com/cleanup-backed-resource
go mod edit -go=1.26
```

### The three rules, made concrete

A `Resource` wraps an external handle and frees it on `Close`; `AddCleanup` registers a backstop that frees it anyway if the caller forgets to `Close`. The pattern is a faithful exercise of the three rules from the concepts file.

First, the cleanup's `arg` is the *released flag*, never the `Resource` itself. Passing the object as `arg` would keep it reachable forever, so the GC would never reclaim it and the cleanup would never run — and `AddCleanup` panics outright on `arg == ptr`. By passing only the `*atomic.Bool` flag, the `Resource` stays collectible, which is the precondition for the backstop to ever fire. The same reasoning explains why the cleanup function captures nothing about the `Resource`: the closure `func(flag *atomic.Bool) { flag.Store(true) }` references only its parameter, so it does not pin `r`.

Second, `Close` calls the returned `Cleanup.Stop()` so the release happens exactly once. On the happy path the caller calls `Close`, which stores `true` and stops the pending cleanup, so the backstop never fires. On the forgotten path `Close` is never called, `r` becomes unreachable, and the cleanup fires and stores `true`. Either way the flag is set exactly once, by exactly one path. The `Stop()` guarantee holds here because `Close` is a method on the very object the cleanup is attached to: the receiver `r` keeps the object reachable across the `Stop()` call, which is the precise condition under which `Stop()` is guaranteed to cancel a not-yet-queued cleanup.

Third, because cleanups are best-effort, this is framed as a *safety net*, not the primary release path. A cleanup is not guaranteed to run before the program exits, so real code still releases through `Close`/`defer`; the cleanup only covers the bug where a caller forgot. Never invert this and treat the cleanup as the mandatory release — that is the one use `AddCleanup` is explicitly unfit for.

The demo models the external handle with a `*atomic.Bool` "released" flag instead of a real file so the module stays dependency-free and the two paths are directly observable; in production the flag would be the actual `Close()` of a socket, file, or mmap region.

Create `resource.go`:

```go
package weakcache

import (
	"runtime"
	"sync/atomic"
)

// Resource wraps an external handle. Close releases it; the registered cleanup
// is a safety net that releases it if the caller forgets to Close. Close calls
// Stop on the cleanup so the release happens exactly once.
type Resource struct {
	cleanup  runtime.Cleanup
	released *atomic.Bool
}

// OpenResource opens a resource. The released flag is set to true by whichever
// path frees it: Close, or the GC safety net. The flag (not the Resource) is
// passed as the cleanup arg, so the Resource stays collectible.
func OpenResource(released *atomic.Bool) *Resource {
	r := &Resource{released: released}
	r.cleanup = runtime.AddCleanup(r, func(flag *atomic.Bool) {
		flag.Store(true)
	}, released)
	return r
}

// Close releases the resource now and cancels the safety-net cleanup so it does
// not also fire later.
func (r *Resource) Close() {
	r.cleanup.Stop()
	r.released.Store(true)
}
```

### The runnable demo

This demo runs both paths so the exactly-once contract is visible. First it opens a resource and calls `Close`, showing the flag is set and the backstop is stopped. Then it opens a second resource inside a function and lets it go out of scope without `Close`, polls `runtime.GC()` until the backstop fires, and shows the flag set by the cleanup instead.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	weakcache "example.com/cleanup-backed-resource"
)

func main() {
	// Happy path: Close releases now and Stop()s the backstop.
	var closed atomic.Bool
	r := weakcache.OpenResource(&closed)
	r.Close()
	fmt.Printf("after Close: released=%t\n", closed.Load())

	// Forgotten path: the GC backstop releases it.
	var forgotten atomic.Bool
	func() {
		res := weakcache.OpenResource(&forgotten)
		runtime.KeepAlive(res)
	}() // res is forgotten here: Close was never called

	deadline := time.Now().Add(2 * time.Second)
	for !forgotten.Load() && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	fmt.Printf("after forgetting: released=%t\n", forgotten.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after Close: released=true
after forgetting: released=true
```

### Tests

The two paths are tested directly. `TestResourceCleanupReleasesForgotten` opens a resource, never closes it, lets it go unreachable, and polls `runtime.GC()` until the backstop sets the flag. `TestResourceCloseStopsCleanup` closes a resource, resets the flag, drops the reference, forces several GCs, and asserts the flag stays `false` — proving `Close` really stopped the cleanup. `TestAddCleanupFiresOnCollection` isolates the underlying mechanism: register a cleanup on a `[]byte`, drop it, GC, and confirm the cleanup runs with the expected `arg`. As in the cache module, the collectible value is a `[]byte`, never a tiny pointer-free value.

Create `resource_test.go`:

```go
package weakcache

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddCleanupFiresOnCollection(t *testing.T) {
	t.Parallel()

	done := make(chan int, 1)
	func() {
		buf := make([]byte, 1024)
		runtime.AddCleanup(&buf, func(n int) { done <- n }, len(buf))
		runtime.KeepAlive(&buf)
	}()

	runtime.GC()

	select {
	case n := <-done:
		if n != 1024 {
			t.Fatalf("cleanup arg = %d, want 1024", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not run after GC")
	}
}

func TestResourceCleanupReleasesForgotten(t *testing.T) {
	t.Parallel()

	var released atomic.Bool
	func() {
		r := OpenResource(&released)
		runtime.KeepAlive(r)
	}() // r is forgotten here: Close was never called

	deadline := time.Now().Add(2 * time.Second)
	for !released.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the cleanup did not release the forgotten resource")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

func TestResourceCloseStopsCleanup(t *testing.T) {
	t.Parallel()

	var released atomic.Bool
	r := OpenResource(&released)
	r.Close()
	if !released.Load() {
		t.Fatal("Close should release the resource")
	}

	released.Store(false) // reset: the stopped cleanup must not set it again
	r = nil
	for range 5 {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if released.Load() {
		t.Fatal("Close called Stop(); the cleanup must not fire")
	}
}
```

## Review

The pattern is correct when the flag is set exactly once by exactly one path. Confirm the cleanup's `arg` is the `*atomic.Bool` flag and never the `Resource`, and that the closure captures nothing about the `Resource` — otherwise the object stays reachable, the GC never reclaims it, and the backstop in `TestResourceCleanupReleasesForgotten` never fires (or `AddCleanup` panics on `arg == ptr`). Confirm `Close` calls `Stop()` *before* it stores the flag, and that `Stop()` is reliable here because `Close`'s receiver keeps the object reachable across the call; `TestResourceCloseStopsCleanup` proves the stopped cleanup does not fire by resetting the flag and forcing GCs. `TestAddCleanupFiresOnCollection` pins the underlying guarantee that a cleanup does run after collection, using a `[]byte` so the value is individually collectible.

Common mistakes for this feature. The first is passing the `Resource` (or capturing it in the closure) as the cleanup arg, which keeps it alive forever so the cleanup never runs — pass only the flag. The second is relying on the cleanup as the mandatory release path: cleanups are best-effort and may not run before exit, so correctness must come from `Close`/`defer` and the cleanup is only a backstop. The third is omitting `Stop()` in `Close`, which lets the release fire twice — harmless for an idempotent flag, but a double-free for a real handle. The fourth is testing the backstop with a tiny pointer-free value the runtime may never collect individually; use a `[]byte`.

## Resources

- [`runtime.AddCleanup`](https://pkg.go.dev/runtime#AddCleanup) — registering a cleanup, the `arg`-reachability rule, and the best-effort guarantee.
- [`runtime.Cleanup`](https://pkg.go.dev/runtime#Cleanup) — the handle `AddCleanup` returns and its `Stop()` method and preconditions.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — the release that added `runtime.AddCleanup` as the modern replacement for `SetFinalizer`.

---

Back to [01-weak-value-cache.md](01-weak-value-cache.md) | Up to [00-concepts.md](00-concepts.md)
