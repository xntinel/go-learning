# Exercise 1: Request-Budget Toolkit — Wrappers, WaitUntilDone, ProcessItems

Every service accumulates a small internal package of context helpers that encode
the team's conventions for time-budgeting work. This exercise builds that package:
intent-documenting `WithTimeout`/`WithDeadline` wrappers, a generic
`WaitUntilDone` that races a context against a result channel, and a `ProcessItems`
loop that honors cancellation on every iteration so a long CPU loop cannot ignore
the deadline.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and test suite. Nothing here imports any
other exercise.

## What you'll build

```text
context-deadline/                    independent module: example.com/deadlines
  go.mod                             go 1.26
  internal/
    deadlines/
      deadlines.go                   WithTimeout, WithDeadline, WaitUntilDone[T], ProcessItems[T]
      deadlines_test.go              full contract suite + ExampleWithTimeout, -race
  cmd/
    demo/
      main.go                        runnable demo: absolute deadline + nested shortest-wins
```

- Files: `internal/deadlines/deadlines.go`, `cmd/demo/main.go`, `internal/deadlines/deadlines_test.go`.
- Implement: `WithTimeout`/`WithDeadline` wrappers, `WaitUntilDone[T](ctx, ch) (T, bool)`, `ProcessItems[T](ctx, items, work) (int, error)`.
- Test: deadline fires at the requested time and reports `DeadlineExceeded`; absolute deadline within slack; nested shortest-wins; `WaitUntilDone` both directions; `ProcessItems` stops early and completes when fast; inherited-parent-deadline; `ExampleWithTimeout`.
- Verify: `go test -count=1 -race ./...`

### The three patterns and why they belong together

The wrappers `WithTimeout` and `WithDeadline` are deliberately thin — they forward
to the stdlib and exist only to document intent at the call site and give the team
one import to reach for. That is a real convention: a shared package where the
timeout helpers live keeps every caller consistent and gives you one place to add
cross-cutting behavior (a metric, a default budget) later.

`WaitUntilDone[T]` is the "race a context against a channel" shape that appears
everywhere a goroutine produces a single result you might have to abandon. The
`select` blocks on two cases: the result channel and `ctx.Done()`. Whichever is
ready first wins. If the channel delivers, you get `(value, true)`; if the context
expires first, you get `(zero, false)`. The generic parameter lets it wrap any
result type without an `interface{}` and a type assertion at every call site. The
subtle contract is the `ok` from the channel receive: `case v, ok := <-ch` returns
`ok == false` if the channel was closed without a value, which `WaitUntilDone`
faithfully passes through, distinct from the context-expiry `false`.

`ProcessItems[T]` is the concrete embodiment of "cancellation is cooperative." It
walks a slice and, before doing the work for each item, runs a non-blocking
`select` on `ctx.Done()`. If the context has expired, it returns the count
processed so far wrapped around `ctx.Err()` with `%w`, so a caller can both see how
far it got and discriminate the cause with `errors.Is`. Without that per-iteration
check, a CPU-bound loop would run to completion regardless of the deadline. The
`default` case is what makes the check non-blocking: it does not wait for
cancellation, it only asks "has it already happened?" and proceeds otherwise.

Create `internal/deadlines/deadlines.go`:

```go
package deadlines

import (
	"context"
	"fmt"
	"time"
)

// WithTimeout derives a context that cancels after d. It is a thin, intent-
// documenting wrapper over context.WithTimeout so callers reach for one import.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// WithDeadline derives a context that cancels at the absolute time t.
func WithDeadline(parent context.Context, t time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, t)
}

// WaitUntilDone blocks until ctx is done or the channel delivers a value. It
// returns (value, true) when the channel wins and (zero, false) when the
// context expires or the channel is closed without a value.
func WaitUntilDone[T any](ctx context.Context, ch <-chan T) (T, bool) {
	var zero T
	select {
	case v, ok := <-ch:
		return v, ok
	case <-ctx.Done():
		return zero, false
	}
}

// ProcessItems iterates over items and stops early when ctx is done. It checks
// cancellation before each item so a long loop honors the deadline between work
// units, returning how many it processed and ctx.Err() wrapped with %w.
func ProcessItems[T any](ctx context.Context, items []T, work func(T)) (int, error) {
	processed := 0
	for _, item := range items {
		select {
		case <-ctx.Done():
			return processed, fmt.Errorf("processed %d/%d: %w", processed, len(items), ctx.Err())
		default:
		}
		work(item)
		processed++
	}
	return processed, nil
}
```

### The runnable demo

The demo exercises two patterns against the wall clock: an absolute
`WithDeadline` fires ~80ms out, then a nested pair proves the 40ms inner deadline
fires while the 1-second outer is still alive.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/deadlines/internal/deadlines"
)

func main() {
	dl := time.Now().Add(80 * time.Millisecond)
	ctx, cancel := deadlines.WithDeadline(context.Background(), dl)
	defer cancel()
	<-ctx.Done()
	fmt.Println("absolute deadline:", ctx.Err())

	outer, cancelOuter := deadlines.WithTimeout(context.Background(), time.Second)
	defer cancelOuter()
	inner, cancelInner := deadlines.WithTimeout(outer, 40*time.Millisecond)
	defer cancelInner()

	<-inner.Done()
	fmt.Println("inner:", inner.Err(), "| outer still alive:", outer.Err() == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
absolute deadline: context deadline exceeded
inner: context deadline exceeded | outer still alive: true
```

### Tests

The suite pins every contract. `TestWithTimeoutCancelsAfterDuration` and
`TestWithDeadlineUsesAbsoluteTime` prove both forms fire at the requested time and
report `DeadlineExceeded`, and that `Deadline()` reports the requested instant
within slack. `TestNestedTimeoutShortestWins` proves earliest-wins: the 30ms inner
fires while the 200ms outer is still nil. `TestWaitUntilDone*` exercises the
channel-vs-context race in both directions. `TestProcessItems*` proves the
per-iteration check short-circuits a long loop yet completes a fast one.
`TestWithDeadlineInheritsEarlierParentDeadline` proves a child timeout longer than
the parent inherits the parent's effective deadline exactly. `ExampleWithTimeout`
is verified by its `// Output:` comment.

Create `internal/deadlines/deadlines_test.go`:

```go
package deadlines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWithTimeoutCancelsAfterDuration(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	<-ctx.Done()
	elapsed := time.Since(start)

	if elapsed < 25*time.Millisecond {
		t.Fatalf("ctx.Done() fired in %v, want ~30ms", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
}

func TestWithDeadlineUsesAbsoluteTime(t *testing.T) {
	t.Parallel()

	deadline := time.Now().Add(40 * time.Millisecond)
	ctx, cancel := WithDeadline(context.Background(), deadline)
	defer cancel()

	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("ctx.Deadline(): ok = false, want true")
	}
	delta := got.Sub(deadline)
	if delta < -5*time.Millisecond || delta > 5*time.Millisecond {
		t.Fatalf("ctx.Deadline() differs from requested by %v", delta)
	}

	start := time.Now()
	<-ctx.Done()
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Fatalf("ctx.Done() fired in %v, want ~40ms", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
}

func TestNestedTimeoutShortestWins(t *testing.T) {
	t.Parallel()

	outer, cancelOuter := WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelOuter()
	inner, cancelInner := WithTimeout(outer, 30*time.Millisecond)
	defer cancelInner()

	start := time.Now()
	<-inner.Done()
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("inner cancelled in %v, want ~30ms", elapsed)
	}
	if !errors.Is(inner.Err(), context.DeadlineExceeded) {
		t.Fatalf("inner.Err() = %v, want DeadlineExceeded", inner.Err())
	}

	if outer.Err() != nil {
		t.Fatalf("outer.Err() = %v while inner has fired, want nil", outer.Err())
	}
}

func TestWithDeadlineInheritsEarlierParentDeadline(t *testing.T) {
	t.Parallel()

	parent, cancelParent := WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelParent()
	parentDeadline, ok := parent.Deadline()
	if !ok {
		t.Fatal("parent.Deadline(): ok = false, want true")
	}

	// Child asks for a full second, far longer than the parent's 10ms budget.
	child, cancelChild := WithTimeout(parent, time.Second)
	defer cancelChild()

	childDeadline, ok := child.Deadline()
	if !ok {
		t.Fatal("child.Deadline(): ok = false, want true")
	}
	// The child cannot outlive the parent: its effective deadline is the parent's.
	if !childDeadline.Equal(parentDeadline) {
		t.Fatalf("child.Deadline() = %v, want parent's %v", childDeadline, parentDeadline)
	}
}

func TestWaitUntilDoneChannelWins(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch := make(chan string, 1)
	ch <- "delivered"

	v, ok := WaitUntilDone(ctx, ch)
	if !ok {
		t.Fatal("WaitUntilDone: ok = false, want true (channel delivered)")
	}
	if v != "delivered" {
		t.Fatalf("WaitUntilDone: v = %q, want %q", v, "delivered")
	}
}

func TestWaitUntilDoneTimeoutWins(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	ch := make(chan string)
	v, ok := WaitUntilDone(ctx, ch)
	if ok {
		t.Fatalf("WaitUntilDone: ok = true (with v=%q), want false", v)
	}
}

func TestProcessItemsStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	items := make([]int, 20)
	for i := range items {
		items[i] = i
	}

	n, err := ProcessItems(ctx, items, func(int) {
		time.Sleep(5 * time.Millisecond)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ProcessItems: err = %v, want DeadlineExceeded", err)
	}
	if n >= len(items) {
		t.Fatalf("ProcessItems: n = %d, want < %d", n, len(items))
	}
	if n < 1 {
		t.Fatalf("ProcessItems: n = %d, want >= 1 (timing slack)", n)
	}
}

func TestProcessItemsCompletesWhenFast(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithTimeout(context.Background(), time.Second)
	defer cancel()

	items := []int{1, 2, 3, 4, 5}
	n, err := ProcessItems(ctx, items, func(int) {})
	if err != nil {
		t.Fatalf("ProcessItems: err = %v, want nil", err)
	}
	if n != len(items) {
		t.Fatalf("ProcessItems: n = %d, want %d", n, len(items))
	}
}

func ExampleWithTimeout() {
	ctx, cancel := WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	<-ctx.Done()
	fmt.Println("deadline:", ctx.Err())
	// Output: deadline: context deadline exceeded
}
```

## Review

The toolkit is correct when each helper honors its narrow contract. `WithTimeout`
and `WithDeadline` fire at the requested instant and report
`context.DeadlineExceeded`, which the first two tests assert directly. The nested
test proves the earliest-deadline-wins rule that governs every propagation in the
rest of the lesson: the 30ms inner fires while the 200ms outer stays nil, and the
inherited-parent test shows the flip side — a child that asks for a second still
inherits the parent's 10ms deadline exactly, so `child.Deadline().Equal(parent's)`
holds. `WaitUntilDone` must resolve the channel-vs-context race deterministically in
both directions, and `ProcessItems` must stop a long loop early yet complete a fast
one, proving the per-iteration check both fires and stays out of the way.

The mistakes to avoid are the ones the tests are shaped to catch. Do not drop the
`%w` in `ProcessItems`: without it `errors.Is(err, context.DeadlineExceeded)` fails
and a caller cannot tell why the loop stopped. Do not remove the `default` from the
cancellation `select` — that would turn the non-blocking check into a block that
waits for cancellation on every iteration. And keep `defer cancel()` on every
derived context even in tests; `go vet`'s `lostcancel` flags the omission. Run
`go test -race` to confirm the whole suite is clean.

## Resources

- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the relative-budget constructor and its equivalence to WithDeadline.
- [context.WithDeadline](https://pkg.go.dev/context#WithDeadline) — the absolute-deadline constructor and the earliest-deadline-wins rule.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — the canonical treatment of context propagation and cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-outbound-http-timeout.md](02-outbound-http-timeout.md)
