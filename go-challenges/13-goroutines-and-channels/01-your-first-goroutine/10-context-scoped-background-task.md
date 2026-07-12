# Exercise 10: Launch a Speculative Background Task That Stops on Request Cancellation

Your very first background goroutine often outlives the request that started it —
and that is a bug. A speculative cache prefetch or precompute launched to warm a
value for a request must not keep running after that request is cancelled; it
should abandon its work and return promptly. This exercise builds `StartPrefetch`,
a goroutine bound to the request context that terminates the instant the context
is cancelled, teaching the `ctx.Done()`-and-`select` termination contract.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
prefetch/                    independent module: example.com/prefetch
  go.mod
  prefetch.go                Result[V]; StartPrefetch(ctx, key, load) <-chan Result[V]
  cmd/
    demo/
      main.go                start a prefetch, receive the value
  prefetch_test.go           completes when ready; abandons on cancel; no leak
```

Files: `prefetch.go`, `cmd/demo/main.go`, `prefetch_test.go`.
Implement: `StartPrefetch[V any](ctx, key string, load func(ctx, string) (V, error)) <-chan Result[V]` that runs `load` in a goroutine and, via `select` on `ctx.Done()`, returns promptly with `ctx.Err()` if the context is cancelled — without publishing a value.
Test: happy path yields the loaded value; cancellation before `load` finishes yields `ctx.Err()` and no value; the goroutines terminate (baseline restored).
Verify: `go test -race -count=10 ./...`

### The ctx.Done() + select termination contract

A background goroutine must have a defined exit. For a request-scoped task, the
exit signal is the request's context: when the caller cancels (client hung up,
deadline exceeded, a sibling operation failed), `ctx.Done()` closes and the task
should stop. The pattern that makes this promptness possible is a `select` that
races the load's result against `ctx.Done()`:

```go
select {
case r := <-done:
	// load finished first
case <-ctx.Done():
	// cancelled first: return promptly, do not wait for load
}
```

Two details make this leak-safe and correct. First, `load` runs in its own inner
goroutine that sends its result on a channel *buffered to one*. The buffer is
critical: if the `select` picks the `ctx.Done()` branch, no one is left to receive
from `done`, and an unbuffered send would block the inner goroutine forever — a
leak. A buffer of one lets the inner goroutine deposit its result and exit even
when nobody is listening. Second, `load` should itself watch `ctx.Done()` so it
returns quickly on cancellation rather than running to completion; a well-behaved
loader is cancel-aware, and `StartPrefetch`'s `select` guarantees promptness even
if it is not.

On cancellation, `StartPrefetch` publishes `Result{Err: ctx.Err()}` and does *not*
publish a value — a cancelled prefetch must not surface a possibly-incomplete
result. The outer output channel is also buffered to one, so the outer goroutine
can publish and exit whether or not the caller ever receives. That is what lets
the goroutines terminate and the `NumGoroutine` baseline be restored.

Create `prefetch.go`:

```go
package prefetch

import "context"

// Result carries a prefetched value or the reason the prefetch did not complete.
type Result[V any] struct {
	Value V
	Err   error
}

// StartPrefetch runs load in a background goroutine bound to ctx and returns a
// channel that will receive exactly one Result. If ctx is cancelled before load
// finishes, the Result carries ctx.Err() and no value, and the goroutine returns
// promptly instead of running to completion.
func StartPrefetch[V any](ctx context.Context, key string, load func(context.Context, string) (V, error)) <-chan Result[V] {
	out := make(chan Result[V], 1)
	go func() {
		done := make(chan Result[V], 1) // buffered: inner goroutine never blocks
		go func() {
			v, err := load(ctx, key)
			done <- Result[V]{Value: v, Err: err}
		}()

		select {
		case r := <-done:
			if err := ctx.Err(); err != nil {
				out <- Result[V]{Err: err} // cancelled during load: abandon the value
				return
			}
			out <- r
		case <-ctx.Done():
			out <- Result[V]{Err: ctx.Err()}
		}
	}()
	return out
}
```

### The runnable demo

The demo starts a prefetch that succeeds and receives the value. The context is
never cancelled, so the happy path runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/prefetch"
)

func main() {
	load := func(ctx context.Context, key string) (string, error) {
		return strings.ToUpper(key), nil // stand-in for a real lookup
	}

	ch := prefetch.StartPrefetch(context.Background(), "session", load)
	r := <-ch

	if r.Err != nil {
		fmt.Printf("prefetch failed: %v\n", r.Err)
		return
	}
	fmt.Printf("prefetched: %s\n", r.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
prefetched: SESSION
```

### Tests

`TestPrefetchCompletes` lets a cancel-aware `load` finish and asserts the value is
delivered. `TestPrefetchAbandonsOnCancel` cancels the context while `load` is
blocked and asserts the result carries `context.Canceled` (via `errors.Is`) and no
value. `TestPrefetchNoLeakOnCancel` samples `NumGoroutine`, cancels a prefetch, and
asserts the count returns to baseline — proof both goroutines terminated.

Create `prefetch_test.go`:

```go
package prefetch

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// blockingLoad returns a load that completes when release is closed, or returns
// ctx.Err() if the context is cancelled first.
func blockingLoad(release <-chan struct{}) func(context.Context, string) (int, error) {
	return func(ctx context.Context, key string) (int, error) {
		select {
		case <-release:
			return 99, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func TestPrefetchCompletes(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	close(release) // load may finish immediately
	ch := StartPrefetch(context.Background(), "k", blockingLoad(release))

	r := <-ch
	if r.Err != nil {
		t.Fatalf("Err = %v, want nil", r.Err)
	}
	if r.Value != 99 {
		t.Fatalf("Value = %d, want 99", r.Value)
	}
}

func TestPrefetchAbandonsOnCancel(t *testing.T) {
	t.Parallel()

	release := make(chan struct{}) // never released: load blocks until cancel
	ctx, cancel := context.WithCancel(context.Background())
	ch := StartPrefetch(ctx, "k", blockingLoad(release))

	cancel()
	r := <-ch

	if !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", r.Err)
	}
	if r.Value != 0 {
		t.Fatalf("Value = %d, want 0 (no value published on cancel)", r.Value)
	}
}

func TestPrefetchNoLeakOnCancel(t *testing.T) {
	release := make(chan struct{})
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	ch := StartPrefetch(ctx, "k", blockingLoad(release))
	cancel()
	<-ch // drain the result so the outer goroutine has published and exited

	for range 2000 {
		if runtime.NumGoroutine() <= base {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("goroutines did not return to baseline: base=%d now=%d",
		base, runtime.NumGoroutine())
}
```

## Review

`StartPrefetch` is correct when the happy path delivers the loaded value, a
cancellation before completion delivers `ctx.Err()` and no value, and both
goroutines terminate afterward. The two buffered channels are load-bearing: the
inner `done` channel (buffer one) lets the loader deposit its result and exit even
when the `select` already chose the cancel branch, and the outer `out` channel
(buffer one) lets the publisher exit whether or not the caller receives — together
they are why there is no leak. The value-abandonment on cancel is a deliberate
correctness choice: a cancelled request must not observe a result computed against
a context that was already torn down. Run `-race -count=10` to exercise the
cancel-versus-completion race repeatedly; the result must be one or the other,
never a torn read.

## Resources

- [context package](https://pkg.go.dev/context)
- [Go Concurrency Patterns: Context](https://go.dev/blog/context)
- [Go Language Specification: Select statements](https://go.dev/ref/spec#Select_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-no-leak-goroutine-count-guard.md](09-no-leak-goroutine-count-guard.md) | Next: [11-outbox-relay-batch-contiguous-cursor.md](11-outbox-relay-batch-contiguous-cursor.md)
