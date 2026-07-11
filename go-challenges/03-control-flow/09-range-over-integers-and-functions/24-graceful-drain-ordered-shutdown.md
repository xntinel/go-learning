# Exercise 24: Queue Drain on Shutdown — Ordered Processing with Context Deadline and Cleanup

**Nivel: Intermedio** — validacion rapida (un test corto).

A worker service told to shut down should not abandon its in-flight queue
mid-item, but it also should not ignore the shutdown signal and insist on
draining everything before exiting -- it needs to stop promptly at the next
safe boundary and report exactly how much work it left behind so the caller
can requeue it or log it. Modeling the drain as an `iter.Seq[Result]` that
checks a context's deadline before each item and runs its cleanup hook
through a single `defer`, regardless of which of the three ways it can end,
gives that behavior without a separate goroutine or a `select` in the hot
loop. This exercise is an independent module with its own `go mod init`.

## What you'll build

```text
drain/                     independent module: example.com/graceful-drain-ordered-shutdown
  go.mod                   module example.com/graceful-drain-ordered-shutdown
  drain.go                 Result, Drain
  cmd/
    demo/
      main.go              runnable demo: a shutdown signal firing mid-queue
  drain_test.go            FIFO order, context deadline, consumer break, cleanup-once
```

Implement: `Drain(ctx context.Context, queue []string, process func(item string) bool, onShutdown func(remaining int)) iter.Seq[Result]` yielding one `Result{Item, Processed}` per queue item in order, stopping the moment `ctx` is done.
Test: a queue drained with no cancellation yields all items in FIFO order and calls `onShutdown(0)` exactly once; a context cancelled by `process` mid-queue stops before the next item and reports the correct remaining count; a consumer break also triggers `onShutdown` exactly once with the correct remaining count.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/graceful-drain-ordered-shutdown/cmd/demo
cd ~/go-exercises/graceful-drain-ordered-shutdown
go mod init example.com/graceful-drain-ordered-shutdown
go mod edit -go=1.24
```

`remaining` starts at `len(queue)` and is decremented once per item actually
processed; `onShutdown(remaining)` is wired up via a single `defer` set
before the loop begins, which is what guarantees it runs exactly once no
matter which of the three exit paths fires: the queue drains fully
(`remaining` reaches 0 naturally), the context becomes done partway through
(the `ctx.Err() != nil` check returns before decrementing further), or the
consumer breaks out of the range early (`yield` returns `false` and the
function returns). Checking `ctx.Err()` at the *top* of each iteration,
before calling `process`, is what stops the drain from starting one more
item after the deadline has already passed -- it does not abort an
in-flight `process` call, but it refuses to begin a new one, which is the
usual contract for "graceful": finish what you started, do not start
anything new.

Create `drain.go`:

```go
package drain

import (
	"context"
	"iter"
)

// Result is the outcome of trying to process one queued item.
type Result struct {
	Item      string
	Processed bool
}

// Drain yields one Result per item in queue, in FIFO order, calling process
// on each item as long as ctx is not yet done. The moment ctx.Err() becomes
// non-nil -- a deadline passed, a shutdown signal cancelled it -- Drain stops
// processing and yielding immediately, leaving the rest of the queue
// undrained rather than racing to finish it. onShutdown is called exactly
// once, via a defer set up before the loop starts, with the number of items
// that were never processed; because it is deferred, it runs whether Drain
// exits by exhausting the queue, by the context becoming done, or by the
// consumer breaking out of the range early, which is the single cleanup
// path a graceful shutdown needs regardless of which of those three reasons
// triggered it.
func Drain(ctx context.Context, queue []string, process func(item string) bool, onShutdown func(remaining int)) iter.Seq[Result] {
	return func(yield func(Result) bool) {
		remaining := len(queue)
		defer func() { onShutdown(remaining) }()

		for _, item := range queue {
			if ctx.Err() != nil {
				return
			}
			processed := process(item)
			remaining--
			if !yield(Result{Item: item, Processed: processed}) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/graceful-drain-ordered-shutdown"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	queue := []string{"job-1", "job-2", "job-3", "job-4"}

	process := func(item string) bool {
		if item == "job-2" {
			cancel() // simulate a shutdown signal arriving mid-drain
		}
		return true
	}

	for r := range drain.Drain(ctx, queue, process, func(remaining int) {
		fmt.Printf("shutdown: %d item(s) left undrained\n", remaining)
	}) {
		fmt.Printf("processed=%v item=%s\n", r.Processed, r.Item)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
processed=true item=job-1
processed=true item=job-2
shutdown: 2 item(s) left undrained
```

`job-1` processes normally. `job-2` also processes and is yielded -- the
shutdown signal fires *during* its processing, but that in-flight item is
allowed to finish and be reported. Only when the loop reaches `job-3` does
the `ctx.Err() != nil` check catch the now-cancelled context and stop, so
`job-3` and `job-4` are never touched, and the deferred `onShutdown` reports
those 2 items as left undrained.

### Tests

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"testing"
)

func TestDrainProcessesQueueInFIFOOrder(t *testing.T) {
	t.Parallel()

	queue := []string{"a", "b", "c"}
	shutdowns := 0
	remainingAtShutdown := -1

	var got []Result
	for r := range Drain(context.Background(), queue, func(string) bool { return true }, func(remaining int) {
		shutdowns++
		remainingAtShutdown = remaining
	}) {
		got = append(got, r)
	}

	want := []Result{{"a", true}, {"b", true}, {"c", true}}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if shutdowns != 1 {
		t.Fatalf("onShutdown called %d times, want 1", shutdowns)
	}
	if remainingAtShutdown != 0 {
		t.Fatalf("remainingAtShutdown = %d, want 0", remainingAtShutdown)
	}
}

func TestDrainStopsOnContextDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	queue := []string{"a", "b", "c", "d"}

	process := func(item string) bool {
		if item == "b" {
			cancel()
		}
		return true
	}

	shutdowns := 0
	remainingAtShutdown := -1
	var got []Result
	for r := range Drain(ctx, queue, process, func(remaining int) {
		shutdowns++
		remainingAtShutdown = remaining
	}) {
		got = append(got, r)
	}

	// "a" and "b" are processed and yielded; the ctx.Err() check before "c"
	// stops the loop before it is ever touched.
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if shutdowns != 1 {
		t.Fatalf("onShutdown called %d times, want 1", shutdowns)
	}
	if remainingAtShutdown != 2 {
		t.Fatalf("remainingAtShutdown = %d, want 2", remainingAtShutdown)
	}
}

func TestDrainStopsOnConsumerBreak(t *testing.T) {
	t.Parallel()

	queue := []string{"a", "b", "c"}
	shutdowns := 0
	remainingAtShutdown := -1

	count := 0
	for range Drain(context.Background(), queue, func(string) bool { return true }, func(remaining int) {
		shutdowns++
		remainingAtShutdown = remaining
	}) {
		count++
		if count == 1 {
			break
		}
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if shutdowns != 1 {
		t.Fatalf("onShutdown called %d times, want 1", shutdowns)
	}
	if remainingAtShutdown != 2 {
		t.Fatalf("remainingAtShutdown = %d, want 2", remainingAtShutdown)
	}
}
```

## Review

The detail that makes `onShutdown` trustworthy is that it is wired up with a
single `defer` at the very top of the iterator body, before the loop even
starts, rather than as a call placed after the loop. Placing it after the
loop would mean it never runs when the consumer breaks early or when the
`ctx.Err()` check returns -- both of which are `return` statements that skip
any code physically below the loop. The `defer` closes over `remaining` by
reference, so whatever value that variable holds at the moment of return --
whether that is 0 after a full drain, or some positive count after an early
stop -- is exactly the value `onShutdown` reports, without any extra
bookkeeping needed at each exit point.

## Resources

- [`context` package documentation](https://pkg.go.dev/context)
- [Go blog: context and cancellation](https://go.dev/blog/context)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-merge-join-multiple-sorted-streams.md](23-merge-join-multiple-sorted-streams.md) | Next: [25-cache-invalidation-cascade.md](25-cache-invalidation-cascade.md)
