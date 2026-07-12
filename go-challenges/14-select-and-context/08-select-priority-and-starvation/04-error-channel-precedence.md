# Exercise 4: Fail-fast fan-in that checks the error channel before more results

A handler that fans a request out to N upstreams should abort the moment any one
fails, not keep aggregating. This exercise builds that scatter-gather: workers
report on a results channel and an errors channel, and the aggregator peeks the
error channel (and `ctx.Done()`) before consuming the next result, so the first
error cancels the shared context and returns immediately — with no goroutine leak.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fanin/                       module example.com/fanin
  go.mod
  fanin.go                   ErrUpstream; func Gather[T any](ctx, n, work) ([]T, error)
  cmd/
    demo/
      main.go                fans out 4 upstreams, one fails, prints the fail-fast error
  fanin_test.go              happy set, fail-fast + no-leak (timeout), cancel propagation
```

Files: `fanin.go`, `cmd/demo/main.go`, `fanin_test.go`.
Implement: `Gather[T any](ctx, n, work func(ctx, i) (T, error)) ([]T, error)` —
run `n` workers, collect their results, but abort on the first error, cancel the
shared context, and wait for all workers before returning.
Test: happy path aggregates all `n` results (order-independent); one worker's
error is returned promptly and cancels the rest with no leak (bounded by a
timeout); results already collected are discarded on error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/08-select-priority-and-starvation/04-error-channel-precedence/cmd/demo
cd go-solutions/14-select-and-context/08-select-priority-and-starvation/04-error-channel-precedence
```

### Fail-fast is priority over an error channel

A scatter-gather naturally has two channels flowing back from the workers:
results and errors. The uniform-random `select` would let a result be consumed
even when an error is already sitting in the error channel, delaying the abort.
The fix is the same priority idiom as everywhere else in this lesson: a
non-blocking peek over the error channel and `ctx.Done()` *before* the blocking
receive that might take a result. If an error is waiting, it wins; the aggregator
returns it, and — crucially — cancels the shared context so the remaining workers
stop.

That cancellation is what prevents a goroutine leak. Consider the failure mode: a
worker fails, the aggregator returns the error, but the shared context is left
live. The other workers are blocked trying to send their results on an unbuffered
channel that no one will ever read again. They block forever. The process now has
N-1 leaked goroutines per failed request. Under load that is an OOM.

Two design details make the abort clean:

- **The error channel is buffered to `n`.** A worker that fails after the
  aggregator has already returned must still be able to deposit its error and
  exit, rather than block on a full channel. Buffering to `n` guarantees every
  worker can report without blocking.
- **Every worker send is a `select` against `ctx.Done()`.** A worker sending a
  result on the unbuffered results channel, or an error on the (buffered) errors
  channel, always has `case <-ctx.Done(): return` as its escape, so cancelling the
  context releases every parked worker.

Finally, a `defer` that cancels the context and then `wg.Wait()`s runs on *every*
return path — happy or error — so `Gather` never returns while a worker is still
running. On the happy path the workers are already done and the wait is
instantaneous; on the error path the cancel unblocks them first.

Create `fanin.go`:

```go
package fanin

import (
	"context"
	"errors"
	"sync"
)

// ErrUpstream is a sentinel for a failed upstream call; wrap it with %w.
var ErrUpstream = errors.New("upstream call failed")

// Gather runs n workers, each producing a T or an error, and aggregates their
// results. It returns on the first error (or ctx cancellation), cancels the
// shared context so the remaining workers exit, and waits for all of them before
// returning, so no goroutine leaks. On error the partial results are discarded.
func Gather[T any](ctx context.Context, n int, work func(ctx context.Context, i int) (T, error)) ([]T, error) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	results := make(chan T)
	errs := make(chan error, n) // buffered so a late failer never blocks

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := work(ctx, i)
			if err != nil {
				select {
				case errs <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case results <- v:
			case <-ctx.Done():
			}
		}()
	}
	// Cancel siblings and wait for every worker on every return path.
	defer func() {
		cancel()
		wg.Wait()
	}()

	collected := make([]T, 0, n)
	for len(collected) < n {
		// Peek: an error (or cancellation) preempts consuming another result.
		select {
		case err := <-errs:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// Blocking: take an error, a cancellation, or the next result.
		select {
		case err := <-errs:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		case v := <-results:
			collected = append(collected, v)
		}
	}
	return collected, nil
}
```

### The runnable demo

The demo fans out four "upstream calls"; the third one fails deterministically.
`Gather` returns that error immediately and cancels the other three. Because the
failing worker's index is fixed, the printed error is deterministic even though
the workers run concurrently.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/fanin"
)

func main() {
	ctx := context.Background()
	results, err := fanin.Gather(ctx, 4, func(ctx context.Context, i int) (string, error) {
		if i == 2 {
			return "", fmt.Errorf("upstream %d: %w", i, fanin.ErrUpstream)
		}
		return fmt.Sprintf("resp-%d", i), nil
	})
	if err != nil {
		fmt.Println("gather failed:", err)
		return
	}
	fmt.Println("gathered:", results)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
gather failed: upstream 2: upstream call failed
```

### Tests

`TestGatherHappyPath` asserts all `n` results come back as an order-independent
set (the fan-in makes ordering nondeterministic, so the test builds a set). The
fail-fast test is the important one: one worker fails and the rest block on a
signal the test controls; the assertion is wrapped in a timeout so a leaked or
hung worker fails the test instead of hanging the suite. It also asserts the
returned slice is `nil` — partial results are discarded on error — and that the
returned error unwraps to the sentinel via `errors.Is`.

Create `fanin_test.go`:

```go
package fanin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestGatherHappyPath(t *testing.T) {
	t.Parallel()

	const n = 8
	got, err := Gather(context.Background(), n, func(_ context.Context, i int) (int, error) {
		return i * i, nil
	})
	if err != nil {
		t.Fatalf("Gather: unexpected error %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	set := make(map[int]bool, n)
	for _, v := range got {
		set[v] = true
	}
	for i := range n {
		if !set[i*i] {
			t.Fatalf("result %d (=%d) missing from %v", i, i*i, got)
		}
	}
}

func TestGatherFailFastNoLeak(t *testing.T) {
	t.Parallel()

	// Workers other than the failer block until released, proving Gather does
	// not require their results and cancels them on the first error.
	release := make(chan struct{})

	type out struct {
		res []string
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := Gather(context.Background(), 5, func(ctx context.Context, i int) (string, error) {
			if i == 3 {
				return "", fmt.Errorf("worker %d: %w", i, ErrUpstream)
			}
			select {
			case <-release: // never fires; workers must exit via ctx cancel
			case <-ctx.Done():
			}
			return "unused", nil
		})
		done <- out{res, err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, ErrUpstream) {
			t.Fatalf("err = %v, want ErrUpstream", got.err)
		}
		if got.res != nil {
			t.Fatalf("res = %v, want nil (partial results discarded on error)", got.res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Gather hung: workers leaked instead of being cancelled")
	}
	close(release)
}

func TestGatherContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Gather(ctx, 4, func(ctx context.Context, i int) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

## Review

The fan-in is correct when the happy path returns all `n` results as a set and the
error path returns the first error, `nil` results, and — the load-bearing property
— *does not leak*. `TestGatherFailFastNoLeak` proves the no-leak guarantee: the
non-failing workers block on a channel that never fires, so the only way `Gather`
can return within the timeout is by cancelling the shared context and having those
workers exit via `case <-ctx.Done()`. The two mistakes that break it are returning
on the first error without cancelling (the siblings block forever on the unread
results channel) and using an unbuffered error channel (a worker failing after the
aggregator moved on blocks trying to report). To aggregate *all* errors instead of
failing fast, collect them and combine with `errors.Join`; the fail-fast shape
here returns the first and cancels the rest. Run `go test -race` to confirm the
concurrent sends and the aggregator are synchronized.

## Resources

- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the shared cancellation that stops siblings on first error.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — waiting for every worker before returning, so nothing leaks.
- [Go Blog: Go Concurrency Patterns — pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in / fan-out and early-cancel patterns.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining multiple errors when you want all failures, not just the first.

---

Back to [03-graceful-shutdown-drain.md](03-graceful-shutdown-drain.md) | Next: [05-weighted-fair-scheduler.md](05-weighted-fair-scheduler.md)
