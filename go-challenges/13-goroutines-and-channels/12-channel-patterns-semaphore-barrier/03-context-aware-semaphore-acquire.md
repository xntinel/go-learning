# Exercise 3: Cancellable Acquire for Request-Scoped Fan-Out

The bare-send semaphore from Exercise 1 has a fatal flaw behind an HTTP handler:
when the queue is full and slow, a caller's goroutine parks on the send long
after the client has disconnected. Under load that is a goroutine leak that ends
in OOM. This exercise builds the only correct semaphore for request-scoped work:
one whose `Acquire` selects on the caller's context.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ctxsem/                     independent module: example.com/ctxsem
  go.mod                    go 1.26
  ctxsem.go                 type Semaphore; Acquire, AcquireCtx, Release, TryAcquire
  cmd/
    demo/
      main.go               full semaphore + a short-deadline AcquireCtx, report the error
  ctxsem_test.go            deadline, cancel, slot-not-consumed, no-leak tests (-race)
```

- Files: `ctxsem.go`, `cmd/demo/main.go`, `ctxsem_test.go`.
- Implement: `AcquireCtx(ctx)` that returns `nil` on a slot, or `ctx.Err()` when the caller's deadline or cancellation fires while queued — without consuming a slot.
- Test: a full semaphore + a short-timeout `AcquireCtx` returns `context.DeadlineExceeded` and leaves the held slot untouched; a cancelled parent returns `context.Canceled` promptly; many queued callers all return when their context ends (no leak).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/12-channel-patterns-semaphore-barrier/03-context-aware-semaphore-acquire/cmd/demo
cd go-solutions/13-goroutines-and-channels/12-channel-patterns-semaphore-barrier/03-context-aware-semaphore-acquire
go mod edit -go=1.26
```

### Why the context case is not optional

Consider a handler that limits concurrent calls to a downstream API with a
size-N semaphore. A request comes in, the semaphore is full, and the handler
calls the bare-send `Acquire`. The send blocks. The client, meanwhile, hits its
own timeout and closes the connection — but the server goroutine is still parked
on that send, holding a request's worth of memory, waiting for a slot the caller
no longer wants. Multiply by every request that arrives during the slow period
and you have thousands of leaked goroutines and an OOM.

`AcquireCtx` fixes this by selecting on two cases: the send that acquires a slot,
and `ctx.Done()`. When the request's context is cancelled — by a deadline, by the
client disconnecting (`http.Request.Context()` is cancelled then), or by an
explicit `cancel()` — the `ctx.Done()` case wins, `AcquireCtx` returns
`ctx.Err()`, and the goroutine unwinds. Crucially, when that case wins, the send
never happened, so no slot was consumed: the semaphore's occupancy is exactly the
legitimately-held slots.

```go
func (s Semaphore) AcquireCtx(ctx context.Context) error {
	select {
	case s <- struct{}{}: // acquired a slot
		return nil
	case <-ctx.Done(): // caller gave up; no slot consumed
		return ctx.Err()
	}
}
```

There is one subtlety in `select` semantics worth stating: if both cases are
ready — a slot is free *and* the context is already done — `select` picks one at
random. In practice the interesting case is a *full* semaphore, where only
`ctx.Done()` can fire, so the behavior is deterministic: a queued caller whose
context ends always returns `ctx.Err()`. Keep the blocking `Acquire` and
`TryAcquire` from Exercise 1 available too — not every caller is request-scoped —
but reach for `AcquireCtx` for anything behind a handler.

Create `ctxsem.go`:

```go
package ctxsem

import "context"

// Semaphore is a counting semaphore backed by a buffered channel, with a
// context-aware Acquire for request-scoped work.
type Semaphore chan struct{}

// NewSemaphore returns a semaphore admitting at most n concurrent holders.
func NewSemaphore(n int) Semaphore {
	return make(Semaphore, n)
}

// Acquire takes a slot, blocking until one is free. Use it only for work that is
// not tied to a client that can disappear; behind an HTTP handler use AcquireCtx.
func (s Semaphore) Acquire() {
	s <- struct{}{}
}

// AcquireCtx takes a slot or returns ctx.Err() if the context is cancelled or
// its deadline fires while queued. When it returns an error, no slot was
// consumed, so the semaphore's occupancy is unchanged.
func (s Semaphore) AcquireCtx(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees one slot. Call it exactly once per successful Acquire/AcquireCtx.
func (s Semaphore) Release() {
	<-s
}

// TryAcquire takes a slot without blocking, reporting whether it succeeded.
func (s Semaphore) TryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}
```

### The runnable demo

The demo fills a size-1 semaphore, then calls `AcquireCtx` with a 20 ms timeout.
Because the only slot is held, the context deadline fires and `AcquireCtx`
returns `context.DeadlineExceeded`. It then releases the held slot and shows a
follow-up `TryAcquire` succeeds — proving the timed-out call consumed nothing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/ctxsem"
)

func main() {
	sem := ctxsem.NewSemaphore(1)
	sem.Acquire() // hold the only slot

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := sem.AcquireCtx(ctx)
	fmt.Printf("queued acquire: deadline_exceeded=%t\n", errors.Is(err, context.DeadlineExceeded))

	sem.Release() // free the legitimately-held slot
	fmt.Printf("slot free after release: %t\n", sem.TryAcquire())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
queued acquire: deadline_exceeded=true
slot free after release: true
```

### Tests

`TestAcquireCtxDeadline` fills the semaphore, calls `AcquireCtx` with a short
timeout, and asserts it returns `context.DeadlineExceeded` and did not consume
the held slot (after releasing it, exactly one slot is free).
`TestAcquireCtxCanceled` cancels the parent before the queued acquire and asserts
`context.Canceled` returns promptly. `TestNoGoroutineLeak` is the load-bearing
one: it fills the semaphore, launches many `AcquireCtx` calls that will all queue,
cancels their shared context, and joins every goroutine via a `WaitGroup` — if
any acquire had ignored the context, its goroutine would never return and the
`Wait` would hang the test.

Create `ctxsem_test.go`:

```go
package ctxsem

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAcquireCtxDeadline(t *testing.T) {
	t.Parallel()

	sem := NewSemaphore(1)
	sem.Acquire() // hold the only slot

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := sem.AcquireCtx(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireCtx err = %v, want DeadlineExceeded", err)
	}

	// The timed-out acquire must not have consumed a slot: after releasing the
	// one legitimately-held slot, exactly one slot is free.
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("expected one free slot after release; timed-out acquire leaked a slot")
	}
	if sem.TryAcquire() {
		t.Fatal("expected only one free slot; capacity corrupted")
	}
}

func TestAcquireCtxCanceled(t *testing.T) {
	t.Parallel()

	sem := NewSemaphore(1)
	sem.Acquire()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := sem.AcquireCtx(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireCtx err = %v, want Canceled", err)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	sem := NewSemaphore(1)
	sem.Acquire() // full: every queued AcquireCtx must wait

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sem.AcquireCtx(ctx); !errors.Is(err, context.Canceled) {
				t.Errorf("queued AcquireCtx err = %v, want Canceled", err)
			}
		}()
	}

	cancel()  // every queued caller must now return
	wg.Wait() // hangs if any AcquireCtx ignored the context
}
```

## Review

The cancellable semaphore is correct when a queued caller whose context ends
returns `ctx.Err()` *and* leaves the semaphore's occupancy untouched — the
`ctx.Done()` case winning means the send never ran, so no slot was taken. The
proof against leaks is the joined `WaitGroup`: 50 callers queue on a full
semaphore, one `cancel()` releases them all, and `Wait` returns only if every
goroutine unwound. This is the difference that matters in production: the bare
`Acquire` would leave those 50 goroutines parked forever. Keep both forms —
`Acquire` for background work that cannot be abandoned, `AcquireCtx` for anything
a client can cancel — and never put a bare `Acquire` on a request path.

## Resources

- [context package](https://pkg.go.dev/context) — `Context.Done`, `Context.Err`, `WithTimeout`, `WithCancel`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — why every blocking operation on a request path needs a cancellation channel.
- [net/http: Request.Context](https://pkg.go.dev/net/http#Request.Context) — the request context is cancelled when the client's connection goes away.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-waitgroup-barrier.md](02-waitgroup-barrier.md) | Next: [04-bounded-http-fanout-limiter.md](04-bounded-http-fanout-limiter.md)
