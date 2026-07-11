# Exercise 3: Bounded Fan-Out Where the First Error Cancels Siblings

A handler that must fetch N records from an upstream, or fan a batch out to a
downstream, wants three things at once: run the tasks concurrently, cap how many
hit the dependency at a time, and abort the whole batch the instant one task
fails. `golang.org/x/sync/errgroup` gives all three. This exercise builds a
bounded, fail-fast batch fetcher on top of it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fanout/                    independent module: example.com/fanout
  go.mod                   requires golang.org/x/sync
  fanout.go                FetchAll(ctx, ids, limit, fetch) ([]string, error)
  cmd/
    demo/
      main.go              runnable demo: bounded fetch with a peak-concurrency probe
  fanout_test.go           limit-respected, first-error-cancels, all-success tests
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `FetchAll` using `errgroup.WithContext` + `SetLimit(limit)`; each task honors the derived `ctx`; `Wait` returns the first error and results are written by index (no shared-slice race).
- Test: at most `limit` tasks run concurrently (atomic peak counter); one task's error cancels its siblings and `Wait` returns that exact error via `errors.Is`; the all-success path returns `nil` with every result populated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout/cmd/demo
cd ~/go-exercises/fanout
go mod init example.com/fanout
go get golang.org/x/sync/errgroup
```

### Why errgroup and not a bare WaitGroup

A `sync.WaitGroup` fans out and joins, but it does not carry errors and it does
not cancel. Collecting the first error from a `WaitGroup` fan-out means a shared
error variable behind a mutex, plus your own cancellation plumbing. `errgroup`
packages exactly that:

- `errgroup.WithContext(ctx)` returns a `*Group` and a derived `ctx`. The first
  task that returns a non-nil error triggers the group to cancel that derived
  context. Every other task that is watching `ctx.Done()` then unblocks and
  returns early — that is the "first error cancels siblings" behavior, and it is
  why tasks must thread the *group's* ctx, not the original.
- `g.SetLimit(n)` caps concurrency: `g.Go` blocks until fewer than `n` tasks are
  running, so at most `n` goroutines touch the dependency at once. This is the
  difference between fanning 10,000 IDs into 10,000 simultaneous upstream
  connections (a self-inflicted outage) and holding to a sane pool of, say, 8.
- `g.Wait()` blocks until all tasks finish and returns the *first* non-nil error
  (subsequent errors, including the `context.Canceled` the siblings return, are
  discarded).

The results slice is written without a lock because each task writes a distinct
index `results[i]`; distinct indices of a slice are independent memory, so there
is no data race. This is a deliberately different pattern from appending to a
shared slice (which *would* need a lock). Under Go 1.22+ loop-variable scoping,
`i` and `id` are fresh per iteration, so capturing them in the closure is safe —
no `i := i` shadowing needed.

Create `fanout.go`:

```go
package fanout

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// FetchAll runs fetch over every id concurrently, at most limit at a time.
// It returns the results in id order. If any fetch returns an error, the
// group's context is cancelled so the remaining fetches can bail out, and
// FetchAll returns that first error.
func FetchAll(ctx context.Context, ids []int, limit int, fetch func(context.Context, int) (string, error)) ([]string, error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)

	results := make([]string, len(ids))
	for i, id := range ids {
		g.Go(func() error {
			r, err := fetch(ctx, id)
			if err != nil {
				return err
			}
			results[i] = r // distinct index per task: no lock needed
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
```

### The fail-fast versus run-to-completion choice

`errgroup.WithContext` gives you fail-fast: the first error tears the batch down.
That is right when the results are only useful together (render a page that needs
all its widgets) or when one failure means the others are wasted work. It is the
*wrong* default when you want every task's independent outcome — a batch importer
that should attempt all rows and report which failed. For that, use a plain
`errgroup.Group` (no `WithContext`) or a `WaitGroup` and collect a per-task result
struct, so one failure does not cancel the rest. Choose fail-fast when partial
results are useless; choose run-to-completion when each result stands alone.

`errgroup` also offers `g.TryGo(fn)`, which starts a task only if a concurrency
slot is free and returns `false` otherwise — useful for opportunistic work you
would rather drop than queue. `FetchAll` wants every id fetched, so it uses the
blocking `g.Go`.

### The runnable demo

The demo fetches six ids with a limit of two and, using an atomic counter,
reports the peak number of fetches that ran at the same time — which the limit
pins to two.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"example.com/fanout"
)

func main() {
	var active, peak atomic.Int64

	fetch := func(ctx context.Context, id int) (string, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return fmt.Sprintf("record-%d", id), nil
	}

	ids := []int{1, 2, 3, 4, 5, 6}
	results, err := fanout.FetchAll(context.Background(), ids, 2, fetch)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("results:", strings.Join(results, ","))
	fmt.Println("peak concurrency:", peak.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
results: record-1,record-2,record-3,record-4,record-5,record-6
peak concurrency: 2
```

### Tests

`TestRespectsLimit` proves the concurrency cap: a peak-concurrency probe (the same
atomic max-tracking used in the demo) must never exceed the limit. `TestFirstError`
proves fail-fast cancellation: one id returns a sentinel error while its siblings
block on the group's context; `Wait` returns the sentinel via `errors.Is`, and
the siblings observe the cancellation instead of running to their timeout. Because
the failing task returns immediately while the siblings are still blocked, the
sentinel is deterministically the *first* error `errgroup` records.
`TestAllSuccess` proves the happy path returns `nil` with every slot filled. The
error test uses `limit == len(ids)` so every task, including the failing one, gets
a slot — with a smaller limit the failing task might be queued behind siblings
that block forever on the not-yet-cancelled context, a self-inflicted deadlock.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var errUpstream = errors.New("upstream failed")

func TestRespectsLimit(t *testing.T) {
	t.Parallel()

	const limit = 3
	var active, peak atomic.Int64
	fetch := func(ctx context.Context, id int) (string, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		active.Add(-1)
		return fmt.Sprintf("r%d", id), nil
	}

	ids := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if _, err := FetchAll(context.Background(), ids, limit, fetch); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", p, limit)
	}
}

func TestFirstError(t *testing.T) {
	t.Parallel()

	var cancelled atomic.Int64
	fetch := func(ctx context.Context, id int) (string, error) {
		if id == 2 {
			return "", errUpstream // fails immediately
		}
		select {
		case <-ctx.Done():
			cancelled.Add(1)
			return "", ctx.Err()
		case <-time.After(time.Second):
			return "slow", nil
		}
	}

	ids := []int{1, 2, 3, 4}
	_, err := FetchAll(context.Background(), ids, len(ids), fetch)
	if !errors.Is(err, errUpstream) {
		t.Fatalf("FetchAll err = %v, want errUpstream", err)
	}
	if c := cancelled.Load(); c == 0 {
		t.Fatal("no sibling observed cancellation; fail-fast did not propagate")
	}
}

func TestAllSuccess(t *testing.T) {
	t.Parallel()

	fetch := func(ctx context.Context, id int) (string, error) {
		return fmt.Sprintf("r%d", id), nil
	}
	ids := []int{1, 2, 3}
	got, err := FetchAll(context.Background(), ids, 2, fetch)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	want := []string{"r1", "r2", "r3"}
	if len(got) != len(want) {
		t.Fatalf("len(results) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("results[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
```

## Review

The fetcher is correct when the three guarantees hold under `-race`. Concurrency
never exceeds `limit`, which the atomic peak probe pins. The first error cancels
the siblings, which `TestFirstError` proves by watching a sibling return
`ctx.Err()` and by matching the sentinel through `errgroup`'s first-error
semantics. And the all-success path fills every indexed slot with no lock,
because distinct slice indices are independent memory. The trap to avoid is
forgetting to thread the *group's* derived context into each task: a task that
closes over the original `ctx` never sees the cancellation, so fail-fast silently
degrades to run-to-completion. The second trap is combining `SetLimit` with tasks
that block on a not-yet-cancelled context when the failing task cannot get a slot
— size the limit so the failing task can always run. Run `go test -count=1 -race
./...`.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `Wait`, `SetLimit`, `TryGo`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the cancellation model errgroup builds on.
- [`context.Context`](https://pkg.go.dev/context) — the derived-context cancellation that drives fail-fast.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-graceful-http-shutdown.md](02-graceful-http-shutdown.md) | Next: [04-context-worker-pool.md](04-context-worker-pool.md)
