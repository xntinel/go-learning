# Exercise 10: Time-Bounded Consumption Window with context Timeout

A scrape cycle or a poll loop must not run forever: it collects for a fixed
wall-clock window and then stops, returning whatever it gathered. This exercise
ranges via `for-select` bounded by a `context.WithTimeout`, returning the drained
items plus a flag saying whether it finished (the channel closed) or timed out.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
boundedwindow/              independent module: example.com/boundedwindow
  go.mod                    go 1.26
  consumer.go               type Item; Consume(ctx, ch) ([]Item, bool)
  cmd/
    demo/
      main.go               a collection cycle that finishes before its deadline
  consumer_test.go          finished-before-deadline, timed-out-partial, zero-timeout
```

Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
Implement: `Consume(ctx context.Context, ch <-chan Item) ([]Item, bool)` that ranges via `for-select`, returning `(items, true)` when the channel closes first and `(items, false)` when the context deadline fires first.
Test: a fast closed channel returns all items with `finished == true` before the deadline; a slow producer returns partial items with `finished == false` and `ctx.Err() == context.DeadlineExceeded`; a zero timeout returns immediately empty; `-race` clean with no leaked goroutine.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/06-ranging-over-channels/10-bounded-window-consumer/cmd/demo
cd go-solutions/13-goroutines-and-channels/06-ranging-over-channels/10-bounded-window-consumer
```

### Finished versus timed-out: the boolean matters

The two ways this loop can end are semantically different, and the caller needs to
know which happened. If the channel closes first, the consumer drained the entire
producer within the window — the cycle is *complete*, and a scheduler can move on.
If the deadline fires first, the consumer collected a *partial* result — there may
be more upstream, and the caller might log it, retry, or widen the next window. A
consumer that returned only the items would hide this distinction; returning
`(items, finished bool)` surfaces it.

The loop is a `for-select` over `ctx.Done()` and the item channel — the same
cancellation-multiplexing shape as before, but here the cancellation is a
*deadline* the caller installs with `context.WithTimeout`. When the deadline
fires, `ctx.Done()` closes and the `false` branch returns. When the producer
closes the channel, the `!ok` branch returns `true`. Note the subtlety the concept
notes flag: with both a buffered value and an expired deadline ready at once,
`select` picks pseudo-randomly, so the timeout is prompt, not exact-to-the-item —
tests assert bounds and the `finished` flag, not a precise count.

The consumer does not create the timeout; the caller does, and the caller owns the
`cancel` (via `defer cancel()`) and can inspect `ctx.Err()` afterward:
`context.DeadlineExceeded` confirms a timeout, `nil` confirms a clean finish. This
keeps the consumer reusable — it works with any cancellation source, a deadline
being just one.

Create `consumer.go`:

```go
package boundedwindow

import "context"

// Item is one collected unit (a scraped metric, a polled record).
type Item struct {
	ID int
}

// Consume drains ch until either it is closed (finished == true) or ctx's deadline
// fires (finished == false), returning the items gathered within the window. A
// plain range could not observe the deadline; this uses for-select.
func Consume(ctx context.Context, ch <-chan Item) ([]Item, bool) {
	var out []Item
	for {
		select {
		case <-ctx.Done():
			return out, false // window elapsed: partial result
		case item, ok := <-ch:
			if !ok {
				return out, true // producer done: complete result
			}
			out = append(out, item)
		}
	}
}
```

### The runnable demo

The demo collects from a producer that finishes well within a one-second window,
so it returns `finished == true` with all items.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/boundedwindow"
)

func main() {
	ch := make(chan boundedwindow.Item, 3)
	for i := 1; i <= 3; i++ {
		ch <- boundedwindow.Item{ID: i}
	}
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	items, finished := boundedwindow.Consume(ctx, ch)
	fmt.Printf("collected %d items, finished=%v\n", len(items), finished)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
collected 3 items, finished=true
```

### Tests

`TestFinishesBeforeDeadline` closes a fast channel and asserts all items with
`finished == true` and `ctx.Err() == nil`. `TestTimesOutPartial` fills a buffered
channel but never closes it, so after draining the buffer the consumer blocks and
the deadline fires: it asserts `finished == false`, the buffered items are
returned, and `ctx.Err() == context.DeadlineExceeded`. `TestZeroTimeout` uses a
zero-duration timeout and asserts an immediate empty, not-finished return.

Create `consumer_test.go`:

```go
package boundedwindow

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func filled(n int) chan Item {
	ch := make(chan Item, n)
	for i := range n {
		ch <- Item{ID: i}
	}
	return ch
}

func TestFinishesBeforeDeadline(t *testing.T) {
	t.Parallel()
	ch := filled(3)
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	items, finished := Consume(ctx, ch)
	if !finished {
		t.Fatal("finished = false, want true (channel closed before deadline)")
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if ctx.Err() != nil {
		t.Fatalf("ctx.Err() = %v, want nil", ctx.Err())
	}
}

func TestTimesOutPartial(t *testing.T) {
	t.Parallel()
	ch := filled(2) // buffered but NEVER closed: the consumer blocks after draining

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	items, finished := Consume(ctx, ch)
	if finished {
		t.Fatal("finished = true, want false (deadline should fire first)")
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (the buffered items, drained before timeout)", len(items))
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
}

func TestZeroTimeout(t *testing.T) {
	t.Parallel()
	// No item is ready (never-fed channel), so the only ready case is ctx.Done:
	// the zero-window return is deterministically empty. Note that if items were
	// buffered, select could drain one before noticing the expired deadline,
	// which is why this test uses an empty channel rather than a filled one.
	ch := make(chan Item)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	items, finished := Consume(ctx, ch)
	if finished {
		t.Fatal("finished = true, want false (zero window)")
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0 (deadline already passed)", len(items))
	}
}

func ExampleConsume() {
	ch := filled(2)
	close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	items, finished := Consume(ctx, ch)
	fmt.Printf("%d %v\n", len(items), finished)
	// Output: 2 true
}
```

## Review

The consumer is correct when it returns `true` exactly when the channel closed
first and `false` exactly when the deadline fired, with the items gathered so far
in both cases. `TestTimesOutPartial` is the sharp one: the never-closed buffered
channel forces the consumer to drain the two buffered items and then block, so the
deadline is what ends it — and `ctx.Err() == context.DeadlineExceeded` confirms the
reason. Do not tighten `TestZeroTimeout` into "returns before N nanoseconds"; a
zero deadline is already expired, so the `ctx.Done()` case is ready immediately,
but with buffered items also ready `select` may take one before noticing — assert
the flag and the bound, not an exact instant. Running under `-race` with no leaked
goroutine confirms the consumer returns promptly at the deadline rather than
parking forever on the never-closed channel.

## Resources

- [pkg.go.dev: context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — a deadline-bound context and its `cancel`.
- [pkg.go.dev: context.DeadlineExceeded](https://pkg.go.dev/context#pkg-variables) — the sentinel `ctx.Err()` returns on timeout.
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs) — passing a context to bound a unit of work.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-dedup-idempotent-consumer.md](09-dedup-idempotent-consumer.md) | Next: [11-streaming-export-terminal-writer-drain-on-error.md](11-streaming-export-terminal-writer-drain-on-error.md)
