# Exercise 8: Bounded Fan-Out with errgroup and First-Error Cancellation

The hand-rolled `WaitGroup` + error-channel plumbing from the earlier exercises is
exactly what `golang.org/x/sync/errgroup` replaces. `errgroup.WithContext` gives
shared first-error cancellation, `SetLimit` bounds concurrency to protect a
downstream, and `Wait()` joins every goroutine — so a correctly-used errgroup cannot
leak. This exercise builds a bounded fan-out fetcher on it and proves both the
first-error behavior and the concurrency cap.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It imports `golang.org/x/sync/errgroup` and `go.uber.org/goleak`.

## What you'll build

```text
boundedfanout/               independent module: example.com/boundedfanout
  go.mod
  fetch.go                   func FetchAll(ctx, ids, limit, fetch) — errgroup + SetLimit
  cmd/
    demo/
      main.go                runnable demo: fetch a batch of ids
  fetch_test.go              all-success, first-error cancels, SetLimit caps, goleak
```

- Files: `fetch.go`, `cmd/demo/main.go`, `fetch_test.go`.
- Implement: `FetchAll` using `errgroup.WithContext` for shared cancellation and `SetLimit` to cap concurrency, returning per-id results or the first error.
- Test: all-success returns every result; the first error cancels the rest and `Wait` returns it with no leak; `SetLimit` actually caps concurrency (atomic max <= limit); a ctx-ignoring task delays `Wait`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get golang.org/x/sync/errgroup
go get go.uber.org/goleak@v1.3.0
```

### Why errgroup cannot leak (when used correctly)

`errgroup.WithContext(ctx)` returns a `*Group` and a derived context. The first
`Go`-launched function to return a non-nil error cancels that derived context;
`Wait()` blocks until *every* launched function has returned and yields the first
error. Because `Wait()` joins everything, there is no path where `FetchAll` returns
while a task goroutine is still alive — the leak window is closed by construction.

The critical caveat, and the one real way to still leak with errgroup: **the tasks
must watch the derived context.** Cancellation is a signal, not a force. If a task
does a bare blocking receive or an un-cancellable call, cancelling the derived context
does nothing, and `Wait()` blocks until that task finishes on its own — the fan-out is
effectively serialized on the slowest, un-cancellable task. The rule from the concepts
file holds here too: pass the derived context into every task and select on its
`Done()`.

`SetLimit(n)` caps how many task goroutines run at once. Without it, `FetchAll` over
ten thousand ids launches ten thousand concurrent calls and stampedes the downstream
(a database connection pool, a rate-limited API). With it, at most `n` run
concurrently, and `Go` blocks the launcher until a slot frees. Each task writes its
own result slot (`results[i]`), so no lock is needed and the write is race-free.

Create `fetch.go`:

```go
package boundedfanout

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// FetchFunc fetches one id. It must honor ctx so the group's first-error
// cancellation actually stops in-flight work.
type FetchFunc func(ctx context.Context, id int) (int, error)

// FetchAll fetches every id concurrently, at most `limit` at a time (limit <= 0
// means unbounded). It returns the results in id order, or the first error, in
// which case the remaining tasks are cancelled. It never leaks a goroutine:
// Wait joins every task before returning.
func FetchAll(ctx context.Context, ids []int, limit int, fetch FetchFunc) ([]int, error) {
	g, ctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}

	results := make([]int, len(ids))
	for i, id := range ids {
		g.Go(func() error {
			v, err := fetch(ctx, id)
			if err != nil {
				return err
			}
			results[i] = v // distinct index per task: no data race
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/boundedfanout"
)

func main() {
	ids := []int{1, 2, 3, 4, 5}
	fetch := func(ctx context.Context, id int) (int, error) { return id * 10, nil }

	out, err := boundedfanout.FetchAll(context.Background(), ids, 2, fetch)
	if err != nil {
		fmt.Println("fetch:", err)
		return
	}
	fmt.Println("results:", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
results: [10 20 30 40 50]
```

### The tests

`TestMain` installs `goleak.VerifyTestMain`. `TestAllSuccess` checks every id is
fetched in order. `TestFirstErrorCancels` makes one task fail and asserts the losers
observe the derived context's cancellation, `Wait` returns the first error, and — with
goleak watching — nothing leaks. `TestSetLimitCaps` instruments an atomic max-concurrency
counter and asserts it never exceeds the limit. `TestCtxIgnoringTaskDelaysWait` is the
honest reproduce: a task that ignores the context keeps `Wait` blocked even after
cancellation, until it is released.

Create `fetch_test.go`:

```go
package boundedfanout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sync/errgroup"
)

var errFetch = errors.New("fetch failed")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestAllSuccess(t *testing.T) {
	ids := []int{1, 2, 3, 4}
	fetch := func(ctx context.Context, id int) (int, error) { return id * 10, nil }

	out, err := FetchAll(context.Background(), ids, 2, fetch)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	want := []int{10, 20, 30, 40}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out = %v, want %v", out, want)
		}
	}
}

func TestFirstErrorCancels(t *testing.T) {
	var sawCancel atomic.Bool
	ids := []int{1, 2, 3}
	fetch := func(ctx context.Context, id int) (int, error) {
		if id == 1 {
			return 0, errFetch
		}
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return 0, ctx.Err()
		case <-time.After(time.Second):
			return id, nil
		}
	}

	_, err := FetchAll(context.Background(), ids, 0, fetch)
	if !errors.Is(err, errFetch) {
		t.Fatalf("FetchAll error = %v, want errFetch", err)
	}
	if !sawCancel.Load() {
		t.Fatal("losing tasks did not observe cancellation")
	}
}

func TestSetLimitCaps(t *testing.T) {
	const limit = 3
	var cur, max atomic.Int64

	ids := make([]int, 50)
	for i := range ids {
		ids[i] = i
	}
	fetch := func(ctx context.Context, id int) (int, error) {
		c := cur.Add(1)
		for {
			m := max.Load()
			if c <= m || max.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		cur.Add(-1)
		return id, nil
	}

	if _, err := FetchAll(context.Background(), ids, limit, fetch); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if got := max.Load(); got > limit {
		t.Fatalf("observed max concurrency = %d, want <= %d", got, limit)
	}
}

func TestCtxIgnoringTaskDelaysWait(t *testing.T) {
	g, ctx := errgroup.WithContext(context.Background())
	_ = ctx
	release := make(chan struct{})

	// The first task fails and cancels the derived context.
	g.Go(func() error { return errFetch })
	// The second task ignores ctx entirely: a real bug that stalls Wait.
	g.Go(func() error { <-release; return nil })

	waitDone := make(chan error, 1)
	go func() { waitDone <- g.Wait() }()

	select {
	case <-waitDone:
		t.Fatal("Wait returned while a ctx-ignoring task was still blocked")
	case <-time.After(50 * time.Millisecond):
		// expected: Wait is stuck on the task that will not watch ctx
	}

	close(release) // release it so the group joins and the test stays clean
	if err := <-waitDone; !errors.Is(err, errFetch) {
		t.Fatalf("Wait error = %v, want errFetch", err)
	}
}
```

## Review

`FetchAll` is correct when it returns only after every task has returned: `Wait()`
provides that join, so the fan-out cannot leak as long as every task honors the derived
context. `TestFirstErrorCancels` proves the first error cancels the rest and they exit;
`TestSetLimitCaps` proves `SetLimit` bounds concurrency; `TestCtxIgnoringTaskDelaysWait`
proves the one caveat — a task that ignores cancellation stalls `Wait`.

The mistakes to avoid: do not launch tasks that ignore the derived context; cancellation
cannot force a blocked goroutine to stop, so `Wait` waits for it. Do not fan out
unbounded against a limited downstream; `SetLimit` is how you protect it. And do not
reach back for hand-rolled `WaitGroup` + error channels when errgroup already gives you
join-everything semantics and first-error cancellation. Run under `-race`; each task
writes a distinct result index, and the concurrency counter is atomic.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `Wait`, `SetLimit`, `TryGo`.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the cancellation semantics errgroup packages up.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-ticker-timer-leak-poller.md](07-ticker-timer-leak-poller.md) | Next: [09-graceful-shutdown-supervisor.md](09-graceful-shutdown-supervisor.md)
