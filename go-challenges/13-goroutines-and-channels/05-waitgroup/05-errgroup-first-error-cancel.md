# Exercise 5: Parallel Prefetch That Cancels All Peers on First Error

Sometimes collect-all is the wrong semantics. When a request fans out to several
dependencies and any one failing makes the whole request fail, you want *fail-fast*:
the first error should cancel the remaining in-flight calls so they stop wasting time
and resources. A plain WaitGroup cannot do that. `golang.org/x/sync/errgroup` can:
`WithContext` derives a context that is cancelled the moment any goroutine returns an
error, and `Wait` returns that first error. This module builds a request-scoped
prefetcher on that primitive.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
prefetch/                  independent module: example.com/prefetch
  go.mod                   go 1.25; requires golang.org/x/sync
  prefetch.go              Prefetch fans out; first error cancels peers
  cmd/
    demo/
      main.go              runnable demo: one failing dep cancels the rest
  prefetch_test.go         first-error propagation; peers see cancellation; all-ok
```

- Files: `prefetch.go`, `cmd/demo/main.go`, `prefetch_test.go`.
- Implement: `Prefetch(ctx, tasks []Task) error` — `errgroup.WithContext`, one `g.Go` per task, `Wait` returns the first error.
- Test: one probe errors after a short delay while peers block on `ctx.Done()`; assert `Wait` returns the injected error via `errors.Is` and peers recorded `context.Canceled`; an all-success case returning nil.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/05-errgroup-first-error-cancel/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/05-errgroup-first-error-cancel
go mod edit -go=1.25
go get golang.org/x/sync/errgroup
```

### How errgroup turns the first error into cancellation

`errgroup.WithContext(parent)` returns a `*Group` and a derived `ctx`. Each unit of
work is launched with `g.Go(func() error { ... })`. The first of those functions to
return a non-nil error causes the group to cancel `ctx` — so every other goroutine
that is watching `ctx.Done()` observes the cancellation and can abort early instead of
running to completion. `g.Wait()` blocks until all launched functions return and then
yields the *first* non-nil error (or nil if all succeeded).

The contract you must uphold as the author of each task: actually watch `ctx.Done()`.
errgroup cancels the context, but it cannot force a goroutine that ignores
cancellation to stop. A well-behaved task does its work inside a `select` (or passes
`ctx` down to an HTTP/DB call that honors it) so cancellation is prompt. A task that
loops on CPU without checking `ctx` will run to completion regardless — the
cancellation is cooperative.

This is the deliberate contrast with Exercise 1's runner. There, every job runs to
completion and every result is collected. Here, the first error short-circuits the
rest. Same fan-out shape, opposite join semantics — pick by whether a single failure
should abort the batch.

Create `prefetch.go`:

```go
package prefetch

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Task is one prefetch unit. It must watch ctx.Done and return promptly when the
// context is cancelled.
type Task struct {
	Name string
	Load func(ctx context.Context) error
}

// Prefetch runs every task concurrently and returns the first error, cancelling
// the shared context (and thus the remaining tasks) as soon as one fails. It
// returns nil only when every task succeeds.
func Prefetch(ctx context.Context, tasks []Task) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, t := range tasks {
		g.Go(func() error {
			return t.Load(ctx)
		})
	}
	return g.Wait()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/prefetch"
)

func main() {
	var canceledPeers int

	tasks := []prefetch.Task{
		{Name: "profile", Load: func(ctx context.Context) error {
			// Slow dependency that should be cancelled by the failure below.
			select {
			case <-time.After(time.Second):
				return nil
			case <-ctx.Done():
				canceledPeers++
				return ctx.Err()
			}
		}},
		{Name: "billing", Load: func(ctx context.Context) error {
			time.Sleep(10 * time.Millisecond)
			return errors.New("billing: 503 service unavailable")
		}},
	}

	err := prefetch.Prefetch(context.Background(), tasks)
	fmt.Printf("prefetch error: %v\n", err)
	fmt.Printf("peers cancelled: %d\n", canceledPeers)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
prefetch error: billing: 503 service unavailable
peers cancelled: 1
```

### Tests

`TestPrefetchFirstErrorCancelsPeers` injects one task that errors after a short delay
and one peer that blocks on `ctx.Done()`; it asserts `Prefetch` returns the injected
error via `errors.Is` and that the peer recorded `context.Canceled`.
`TestPrefetchAllSuccess` asserts a nil return when every task succeeds. Both run under
`-race`.

Create `prefetch_test.go`:

```go
package prefetch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errBilling = errors.New("billing failed")

func TestPrefetchFirstErrorCancelsPeers(t *testing.T) {
	t.Parallel()

	var peerCancelled atomic.Bool

	tasks := []Task{
		{Name: "peer", Load: func(ctx context.Context) error {
			select {
			case <-time.After(2 * time.Second):
				return nil
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.Canceled) {
					peerCancelled.Store(true)
				}
				return ctx.Err()
			}
		}},
		{Name: "failing", Load: func(ctx context.Context) error {
			time.Sleep(5 * time.Millisecond)
			return errBilling
		}},
	}

	err := Prefetch(context.Background(), tasks)
	if !errors.Is(err, errBilling) {
		t.Fatalf("Prefetch err = %v, want errBilling", err)
	}
	if !peerCancelled.Load() {
		t.Fatal("peer was not cancelled by the first error")
	}
}

func TestPrefetchAllSuccess(t *testing.T) {
	t.Parallel()

	var count atomic.Int64
	tasks := []Task{
		{Name: "a", Load: func(ctx context.Context) error { count.Add(1); return nil }},
		{Name: "b", Load: func(ctx context.Context) error { count.Add(1); return nil }},
		{Name: "c", Load: func(ctx context.Context) error { count.Add(1); return nil }},
	}
	if err := Prefetch(context.Background(), tasks); err != nil {
		t.Fatalf("Prefetch err = %v, want nil", err)
	}
	if got := count.Load(); got != 3 {
		t.Fatalf("ran %d tasks, want 3", got)
	}
}

func TestPrefetchEmpty(t *testing.T) {
	t.Parallel()

	if err := Prefetch(context.Background(), nil); err != nil {
		t.Fatalf("Prefetch(nil) = %v, want nil", err)
	}
}
```

## Review

The prefetcher is correct when `Wait` returns the first error via `errors.Is`, the
slow peer observes `context.Canceled` rather than running its full delay, and an
all-success batch returns nil. The cancellation assertion is the important one: it
proves errgroup wired the shared context and the peer honored it. If the peer ran its
full two-second sleep, either the context was not threaded or the task ignored
`ctx.Done()`.

Keep the semantics distinction sharp. errgroup is fail-fast: first error wins and
cancels the rest, and any later errors are discarded. When you instead need every
error (a batch report), do not reach for plain `WithContext` — use `SetLimit` plus
`errors.Join`, which is the next module. And always give each task a way to observe
cancellation, or the "cancel the peers" promise is empty.

## Resources

- [`errgroup.WithContext`](https://pkg.go.dev/golang.org/x/sync/errgroup#WithContext) — the group + cancelable context.
- [`errgroup.Group`](https://pkg.go.dev/golang.org/x/sync/errgroup#Group) — `Go` and `Wait` semantics (first error wins).
- [`context.Context`](https://pkg.go.dev/context#Context) — `Done` and `Err` for cooperative cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-scatter-gather-indexed-slice.md](04-scatter-gather-indexed-slice.md) | Next: [06-errgroup-setlimit-batch-processor.md](06-errgroup-setlimit-batch-processor.md)
