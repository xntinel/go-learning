# Exercise 5: Bound Concurrency over a Large Batch with errgroup.SetLimit

A nightly job that upserts ten thousand rows must not open ten thousand database
connections at once. Unbounded fan-out is a self-inflicted outage: it exhausts the
connection pool, hits the file-descriptor ceiling, and balloons memory the moment
the batch grows. `errgroup.SetLimit` turns the group into a bounded worker pool
with no extra machinery — `Go` blocks once the limit is reached — and `TryGo`
offers a non-blocking admission path for load-shedding. This module builds the
bounded processor and proves the live worker count never exceeds the limit.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
boundedpool/                 independent module: example.com/boundedpool
  go.mod                     go 1.26; requires golang.org/x/sync
  boundedpool.go             ProcessBatch (SetLimit-bounded); Metrics (atomic live/max/done)
  cmd/
    demo/
      main.go                runnable demo: 20 items, limit 4, report observed max concurrency
  boundedpool_test.go        tests: max <= limit, every item once, TryGo false when saturated
```

Files: `boundedpool.go`, `cmd/demo/main.go`, `boundedpool_test.go`.
Implement: `ProcessBatch(ctx, items, limit, work)` that uses `g.SetLimit(limit)` so `g.Go` blocks past the limit; instrument a `Metrics` struct with `sync/atomic` to track the live count, observed maximum, and completed count.
Test: for a batch larger than the limit, observed max `<=` limit; every item processed exactly once (`done == len(items)`); `TryGo` returns false when the group is saturated.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/boundedpool/cmd/demo
cd ~/go-exercises/boundedpool
go mod init example.com/boundedpool
go mod edit -go=1.26
go get golang.org/x/sync/errgroup
```

### How SetLimit bounds the pool

`g.SetLimit(n)` gives the group an internal semaphore of `n` tokens. Each `g.Go`
acquires a token before starting its goroutine and releases it when the func
returns; if all `n` tokens are held, `g.Go` *blocks* until one frees up. So a loop
that calls `g.Go` ten thousand times over a huge batch never has more than `n`
goroutines alive at once — the loop itself paces to the pool's capacity. This is
the difference between a controlled drip and a thundering herd: with a limit of 16,
at most 16 DB connections are ever in flight, no matter how large the batch. Call
`SetLimit` *before* the first `Go`; changing the limit while goroutines are active
panics.

`TryGo` is the non-blocking sibling: it attempts to acquire a token and, if none is
free, returns `false` immediately instead of blocking. That is the admission
control you reach for when you would rather shed load than queue it — a request
handler that returns "busy, retry later" rather than piling up work. Both share the
same semaphore, so `TryGo` returning false is exactly "the pool is at its limit
right now".

`ProcessBatch` wraps this into a batch processor: it sets the limit, submits one
`g.Go` per item, and returns `g.Wait`'s aggregate. The `Metrics` struct proves the
bound holds. Each worker does `live := m.Live.Add(1)` on entry and records the
running maximum with a compare-and-swap loop, then `m.Live.Add(-1)` and
`m.Done.Add(1)` on exit. Because `Live` never exceeds the number of tokens, the
observed maximum is `<=` limit — the invariant the test asserts. Using `sync/atomic`
rather than a mutex keeps the instrumentation cheap and, more importantly, itself
race-free under `-race`.

Create `boundedpool.go`:

```go
package boundedpool

import (
	"context"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
)

// Metrics records concurrency observations across the pool. All fields are
// updated with sync/atomic so they are safe to touch from every worker.
type Metrics struct {
	Live atomic.Int64 // currently-running workers
	Max  atomic.Int64 // highest Live ever observed
	Done atomic.Int64 // workers that completed
}

// enter marks a worker as running and updates the observed maximum.
func (m *Metrics) enter() {
	live := m.Live.Add(1)
	for {
		max := m.Max.Load()
		if live <= max || m.Max.CompareAndSwap(max, live) {
			break
		}
	}
}

// leave marks a worker as finished.
func (m *Metrics) leave() {
	m.Live.Add(-1)
	m.Done.Add(1)
}

// ProcessBatch runs work over every item with at most limit workers alive at
// once. g.SetLimit makes g.Go block once limit workers are in flight, so the
// batch drains at a controlled rate instead of spawning one goroutine per item.
// The returned Metrics prove the bound held.
func ProcessBatch[T any](ctx context.Context, items []T, limit int, work func(ctx context.Context, item T) error) (*Metrics, error) {
	m := &Metrics{}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for _, item := range items {
		g.Go(func() error {
			m.enter()
			defer m.leave()
			return work(ctx, item)
		})
	}
	return m, g.Wait()
}
```

### The runnable demo

The demo processes twenty items with a limit of four. Each worker sleeps briefly so
several are genuinely alive at once, then reports the observed maximum, which must
not exceed four.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/boundedpool"
)

func main() {
	items := make([]int, 20)
	for i := range items {
		items[i] = i
	}

	const limit = 4
	m, err := boundedpool.ProcessBatch(context.Background(), items, limit, func(ctx context.Context, item int) error {
		time.Sleep(5 * time.Millisecond) // simulate a DB upsert
		return nil
	})
	if err != nil {
		fmt.Println("batch error:", err)
		return
	}

	fmt.Printf("items processed: %d\n", m.Done.Load())
	fmt.Printf("limit: %d\n", limit)
	fmt.Printf("observed max concurrency <= limit: %t\n", m.Max.Load() <= limit)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
items processed: 20
limit: 4
observed max concurrency <= limit: true
```

### Tests

`TestBoundHolds` runs a batch of 50 with a limit of 5, each worker sleeping briefly
to force overlap, and asserts the observed maximum never exceeds 5 while every item
completes exactly once (`Done == 50`). `TestEveryItemProcessedOnce` uses an atomic
counter incremented inside `work` to prove no item is dropped or double-run.
`TestTryGoFalseWhenSaturated` pins the non-blocking admission contract: with a
limit of 1 and one slot occupied by a blocked worker, `TryGo` must return false;
after the blocker is released, `Wait` succeeds. That test builds the group directly
to exercise `TryGo`.

Create `boundedpool_test.go`:

```go
package boundedpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestBoundHolds(t *testing.T) {
	t.Parallel()
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	const limit = 5
	m, err := ProcessBatch(context.Background(), items, limit, func(ctx context.Context, item int) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("ProcessBatch() error = %v, want nil", err)
	}
	if got := m.Max.Load(); got > limit {
		t.Fatalf("observed max concurrency = %d, want <= %d", got, limit)
	}
	if got := m.Done.Load(); got != int64(len(items)) {
		t.Fatalf("done = %d, want %d", got, len(items))
	}
}

func TestEveryItemProcessedOnce(t *testing.T) {
	t.Parallel()
	items := make([]int, 200)
	for i := range items {
		items[i] = i
	}
	var processed atomic.Int64
	_, err := ProcessBatch(context.Background(), items, 8, func(ctx context.Context, item int) error {
		processed.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("ProcessBatch() error = %v, want nil", err)
	}
	if got := processed.Load(); got != int64(len(items)) {
		t.Fatalf("processed = %d, want %d exactly", got, len(items))
	}
}

func TestTryGoFalseWhenSaturated(t *testing.T) {
	t.Parallel()
	var g errgroup.Group
	g.SetLimit(1)

	release := make(chan struct{})
	// Occupy the single slot with a worker that blocks until released.
	g.Go(func() error {
		<-release
		return nil
	})

	// The pool is saturated, so a non-blocking admission must be refused.
	if g.TryGo(func() error { return nil }) {
		t.Fatal("TryGo returned true while the pool was saturated")
	}

	close(release)
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	// With the slot free again, TryGo is admitted.
	if !g.TryGo(func() error { return nil }) {
		t.Fatal("TryGo returned false after the pool drained")
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait() after TryGo error = %v, want nil", err)
	}
}
```

## Review

The pool is correct when the observed maximum concurrency never exceeds the limit
and every item is processed exactly once — the two assertions in `TestBoundHolds`
and `TestEveryItemProcessedOnce`. The bound comes entirely from `g.SetLimit`, which
must be called before the first `g.Go` and never while workers are live (doing so
panics). The `Metrics` counters must use `sync/atomic`, not plain `int64`, or the
instrumentation itself is the race `-race` catches. `TryGo` is the load-shedding
counterpart: it refuses admission instead of blocking when the pool is full, which
the saturation test pins deterministically by holding the single slot with a
blocked worker. The trap this exercise closes is the unbounded `for ... { g.Go }`
that spawns one goroutine per item and exhausts connections under load. Run
`go test -race` and `go vet ./...` to confirm.

## Resources

- [`errgroup.Group.SetLimit` and `TryGo`](https://pkg.go.dev/golang.org/x/sync/errgroup#Group.SetLimit) — bounding concurrency and non-blocking admission.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — the `Int64` counters used for race-free instrumentation.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded fan-out as a pattern.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-cancel-cause-fanout.md](06-cancel-cause-fanout.md)
