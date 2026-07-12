# Exercise 5: A Buffered Channel as a Concurrency Limiter for Outbound Calls

When you fan out to a downstream — outbound HTTP calls, DB queries — you must cap how
many run at once, or you overwhelm the dependency and exhaust your own file
descriptors. The idiomatic limiter is a buffered `chan struct{}` of size N used as a
counting semaphore: send a token to acquire, receive to release. This exercise builds
that limiter, adds a context-aware `Acquire` that aborts on cancellation, and pins the
"never exceeds N concurrent" invariant.

This module is fully self-contained.

## What you'll build

```text
limiter/                     module: example.com/limiter
  go.mod                     go 1.26
  limiter.go                 type Limiter; Acquire(ctx), TryAcquire, Release, InFlight
  cmd/
    demo/
      main.go                fan out 8 tasks through a limit of 3, print max concurrency
  limiter_test.go            max-concurrency invariant, ctx-cancel abort, all tokens released
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a `Limiter` over `chan struct{}` with `Acquire(ctx) error`, `TryAcquire() bool`, `Release()`, `InFlight() int`.
- Test: track a live counter with `atomic.Int64` inside guarded work and assert observed max concurrency never exceeds N; cancel the context while full and assert `Acquire` returns `ctx.Err()` promptly; assert all tokens released at the end.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why chan struct{} is a counting semaphore, and why context matters

A buffered `chan struct{}` of capacity N is a counting semaphore where the buffer's
free slots *are* the available permits. Acquire = `sem <- struct{}{}`: the send
succeeds while fewer than N tokens are outstanding and blocks once the buffer is full,
which is exactly "block until a permit frees up". Release = `<-sem`: receiving a token
frees a slot, unblocking one waiting acquirer. `struct{}` is zero-width, so N tokens
cost essentially nothing; the *capacity* is the concurrency limit, nothing else needs
to track a count. The invariant falls out for free: at most N sends can be in the
buffer at once, so at most N goroutines can be between `Acquire` and `Release`.

The naive `Acquire` blocks forever if no permit ever frees. Real backends must respect
cancellation and deadlines, so `Acquire(ctx)` is a `select` over the token send and
`ctx.Done()`: whichever happens first wins. If the context is cancelled while all
permits are held, `Acquire` returns `ctx.Err()` (either `context.Canceled` or
`context.DeadlineExceeded`) *promptly*, instead of hanging until a permit that may
never come. This is the difference between a limiter that participates in graceful
shutdown and one that pins goroutines forever.

`Release` must be paired with a *successful* `Acquire` — releasing a token you never
acquired underflows the semaphore (a receive on an empty buffer would block, or worse,
free a slot you do not own and let concurrency exceed N). The discipline is
`Acquire`, then `defer Release()`, and only when `Acquire` returned nil. `TryAcquire`
is the non-blocking variant (`select`+`default`) for "run it if there is spare
capacity, otherwise skip".

The production-grade equivalents are `golang.org/x/sync/semaphore.Weighted` — which
adds *weighted* acquisition (`Acquire(ctx, n)` for a task that costs n permits) and a
context-aware API — and `errgroup.Group.SetLimit(n)`, which caps the goroutines a
group runs concurrently. Reach for those when you need weights, context integration
out of the box, or error propagation; the raw `chan struct{}` is the right tool when
you want a dependency-free limiter and understand exactly what it does. Exercise 9
uses `errgroup.SetLimit`.

Create `limiter.go`:

```go
package limiter

import "context"

// Limiter caps concurrency at N using a buffered channel of tokens as a counting
// semaphore. A successful Acquire must be paired with exactly one Release.
type Limiter struct {
	tokens chan struct{}
}

// New returns a limiter allowing at most n concurrent holders. It panics on n <= 0,
// which would be a nonsensical limit.
func New(n int) *Limiter {
	if n <= 0 {
		panic("limiter: n must be > 0")
	}
	return &Limiter{tokens: make(chan struct{}, n)}
}

// Acquire blocks until a permit is free or ctx is done. On ctx cancellation it
// returns ctx.Err() and acquires nothing.
func (l *Limiter) Acquire(ctx context.Context) error {
	select {
	case l.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire takes a permit without blocking. ok is false if none is free.
func (l *Limiter) TryAcquire() (ok bool) {
	select {
	case l.tokens <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a permit. Call it exactly once per successful Acquire/TryAcquire.
func (l *Limiter) Release() {
	select {
	case <-l.tokens:
	default:
		panic("limiter: Release without a matching Acquire")
	}
}

// InFlight reports the number of permits currently held (advisory, for metrics).
func (l *Limiter) InFlight() int { return len(l.tokens) }
```

### The runnable demo

The demo fans out 8 tasks through a limit of 3, using an `atomic.Int64` to record the
peak observed concurrency, and prints that it never exceeded 3.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/limiter"
)

func main() {
	const limit, tasks = 3, 8
	l := limiter.New(limit)

	var live, max atomic.Int64
	var wg sync.WaitGroup
	for range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background()); err != nil {
				return
			}
			defer l.Release()

			n := live.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	fmt.Printf("tasks=%d limit=%d peak_concurrency=%d\n", tasks, limit, max.Load())
	fmt.Printf("within limit: %v\n", max.Load() <= limit)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tasks=8 limit=3 peak_concurrency=3
within limit: true
```

### Tests

`TestNeverExceedsLimit` launches many goroutines through the limiter and uses an
`atomic.Int64` to observe live concurrency; the peak must never exceed N. This is the
core invariant, verified under `-race`. `TestAcquireRespectsContext` fills the limiter
and then calls `Acquire` with an already-cancelled context, asserting it returns
`ctx.Err()` promptly rather than hanging. `TestAllTokensReleased` runs a batch and
asserts `InFlight() == 0` at the end, proving every `Acquire` was matched by a
`Release`.

Create `limiter_test.go`:

```go
package limiter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNeverExceedsLimit(t *testing.T) {
	t.Parallel()

	const limit, goroutines = 4, 200
	l := New(limit)

	var live, max atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer l.Release()

			n := live.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	if got := max.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, exceeds limit %d", got, limit)
	}
}

func TestAcquireRespectsContext(t *testing.T) {
	t.Parallel()

	l := New(1)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the only permit is held

	done := make(chan error, 1)
	go func() { done <- l.Acquire(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return promptly on a cancelled context")
	}
}

func TestAllTokensReleased(t *testing.T) {
	t.Parallel()

	l := New(3)
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Acquire(context.Background())
			l.Release()
		}()
	}
	wg.Wait()

	if got := l.InFlight(); got != 0 {
		t.Fatalf("InFlight after batch = %d, want 0", got)
	}
}

func TestTryAcquire(t *testing.T) {
	t.Parallel()

	l := New(1)
	if !l.TryAcquire() {
		t.Fatal("first TryAcquire should succeed")
	}
	if l.TryAcquire() {
		t.Fatal("second TryAcquire should fail while full")
	}
	l.Release()
	if !l.TryAcquire() {
		t.Fatal("TryAcquire after Release should succeed")
	}
	l.Release()
}

func ExampleLimiter_TryAcquire() {
	l := New(1)
	fmt.Println(l.TryAcquire()) // takes the only permit
	fmt.Println(l.TryAcquire()) // full: no permit free
	l.Release()
	fmt.Println(l.TryAcquire()) // permit freed, succeeds again
	// Output:
	// true
	// false
	// true
}
```

## Review

The limiter is correct when the buffer capacity is the concurrency cap and every
successful `Acquire` is matched by exactly one `Release` — the `atomic.Int64` peak test
is the direct proof that no more than N goroutines are ever simultaneously past the
gate. The context-aware `Acquire` is what keeps a full limiter from pinning goroutines
forever: on a cancelled or deadlined context it returns `ctx.Err()` immediately. The
two mistakes to avoid are releasing a token you never acquired (it lets concurrency
exceed N or panics on the empty receive, which is why `Release` guards with
`select`+`default`) and blocking on `Acquire` with a background context in a path that
must honor shutdown — thread a real `ctx` through. When you need weights or a
library-grade API, `golang.org/x/sync/semaphore.Weighted` is the drop-in upgrade.

## Resources

- [pkg.go.dev: golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — `Weighted.Acquire`/`TryAcquire`/`Release`, the production alternative.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded parallelism with a semaphore channel.
- [pkg.go.dev: sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64` used to observe live concurrency in the test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-load-shedding-nonblocking-enqueue.md](04-load-shedding-nonblocking-enqueue.md) | Next: [06-batch-flush-buffer.md](06-batch-flush-buffer.md)
