# Exercise 4: Safe Idempotent Close With sync.Once

A resource handle — a connection, a stream, a subscription — routinely has its
`Close()` invoked from several paths at once: a `defer` on the happy path, a
signal handler on SIGTERM, an error branch that bails early. Closing a channel
twice panics, so the close must happen exactly once no matter how many goroutines
race into it. `sync.Once` is the standard guard, and this exercise builds the
handle that uses it and the test that proves it under `-race`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
onceclose/                   independent module: example.com/onceclose
  go.mod                     go mod init example.com/onceclose
  resource.go                type Resource; New, Close, Done, CloseCount
  cmd/
    demo/
      main.go                runnable demo: many goroutines race Close
  resource_test.go           N concurrent Close: one close, one side-effect
```

Files: `resource.go`, `cmd/demo/main.go`, `resource_test.go`.
Implement: a `Resource` wrapping a `done chan struct{}` and a real teardown side-effect, whose `Close()` may be called concurrently from many goroutines and closes exactly once via `sync.Once`.
Test: fire `N` concurrent `Close()` calls; assert no panic, the channel is closed (a receiver sees a single close), and the teardown side-effect ran exactly once (counter `== 1`).
Verify: `go test -count=1 -race ./...`

### Why `sync.Once`, and what the naive version does

The naive handle closes the channel directly:

```go
// Wrong: double close panics.
func (r *Resource) Close() {
	close(r.done) // second caller: "panic: close of closed channel"
}
```

Two goroutines calling this race to `close`, and the loser panics. You cannot fix
it with a plain `if !closed { close() }` either — the check and the close are not
atomic, so two goroutines can both pass the check before either closes. `sync.Once`
exists precisely for this: `once.Do(f)` runs `f` on the first call, and every
concurrent or later call blocks until that first `f` has returned, then does
nothing. The teardown — closing the channel and releasing the underlying resource
— goes inside the `Do`, so it runs exactly once and every caller observes a fully
closed handle when `Close()` returns.

The `CloseCount` counter is not production code; it is the observable proof for
the test. The teardown increments it inside the `Once.Do`, so an assertion of
`CloseCount() == 1` after `N` concurrent `Close()` calls is a direct measurement
that the guarded body ran a single time. Run under `-race` and the naive
unguarded version would both panic and report a data race; the guarded one is
clean.

Set up the module:

```bash
mkdir -p ~/go-exercises/onceclose/cmd/demo
cd ~/go-exercises/onceclose
go mod init example.com/onceclose
```

Create `resource.go`:

```go
package onceclose

import (
	"sync"
	"sync/atomic"
)

// Resource is a handle whose Close may be invoked from many shutdown paths at
// once (defer, signal handler, error path). It closes exactly once.
type Resource struct {
	done  chan struct{}
	once  sync.Once
	count atomic.Int64
}

// New returns an open Resource.
func New() *Resource {
	return &Resource{done: make(chan struct{})}
}

// Done returns a channel that is closed when the resource is closed. Callers can
// select on it to observe teardown.
func (r *Resource) Done() <-chan struct{} { return r.done }

// Close releases the resource exactly once. It is safe to call concurrently and
// repeatedly; only the first call performs the teardown, and it never panics on
// a double close.
func (r *Resource) Close() error {
	r.once.Do(func() {
		r.count.Add(1) // the real teardown side-effect goes here
		close(r.done)
	})
	return nil
}

// CloseCount reports how many times the guarded teardown ran. It is 1 after any
// number of Close calls, and is the observable proof of single-close.
func (r *Resource) CloseCount() int64 { return r.count.Load() }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/onceclose"
)

func main() {
	r := onceclose.New()

	// 100 goroutines all race to Close the same handle.
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Close()
		}()
	}
	wg.Wait()

	// Done is closed, so this receive does not block.
	<-r.Done()
	fmt.Printf("close side-effects: %d\n", r.CloseCount())
	fmt.Println("done channel observed closed")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
close side-effects: 1
done channel observed closed
```

### Tests

Create `resource_test.go`:

```go
package onceclose

import (
	"sync"
	"testing"
)

func TestConcurrentCloseRunsOnce(t *testing.T) {
	t.Parallel()

	r := New()
	const callers = 200
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Close(); err != nil {
				t.Errorf("Close returned error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := r.CloseCount(); got != 1 {
		t.Fatalf("CloseCount = %d after %d concurrent Close calls, want 1", got, callers)
	}
}

func TestDoneClosedAfterClose(t *testing.T) {
	t.Parallel()

	r := New()
	select {
	case <-r.Done():
		t.Fatal("Done closed before Close")
	default:
	}

	_ = r.Close()

	select {
	case <-r.Done():
		// expected: closed channel yields immediately
	default:
		t.Fatal("Done not closed after Close")
	}
}

func TestRepeatedCloseNoPanic(t *testing.T) {
	t.Parallel()

	r := New()
	_ = r.Close()
	_ = r.Close()
	_ = r.Close()
	if got := r.CloseCount(); got != 1 {
		t.Fatalf("CloseCount = %d, want 1", got)
	}
}
```

## Review

The handle is correct when `CloseCount()` is exactly `1` after any number of
concurrent `Close()` calls and `Done()` is observably closed afterward — the two
tests measure precisely those facts. The failure this design prevents is the
double-close panic: put `close(r.done)` outside the `Once` and the 200-caller
test crashes and reports a data race. Remember that a plain boolean guard is not a
fix, because the read-check-close sequence is not atomic; `sync.Once` is the
minimal correct tool. Run `go test -race` — it is what turns a latent unsynchronized
close into a hard failure instead of a lucky pass.

## Resources

- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) — `Do` runs its function exactly once, even under concurrency.
- [The Go Programming Language Specification: Close](https://go.dev/ref/spec#Close) — closing an already-closed channel panics, which is what `Once` guards against.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — closing a channel as the shutdown signal for a resource.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-context-to-done-bridge.md](05-context-to-done-bridge.md)
