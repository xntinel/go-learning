# Exercise 3: Idempotent Cancel and Goroutine-Leak Proof

Two invariants underpin every use of `context.WithCancel`: cancelling a parent
cancels its children, and calling `cancel` twice is a harmless no-op. This module
builds thin, documented wrappers around those invariants and hardens them with an
explicit idempotency test and a goroutine-leak proof — the checks a senior
engineer runs before trusting a cancellation path in production.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
cancelbox/                 independent module: example.com/cancelbox
  go.mod                   module example.com/cancelbox
  cancelbox.go             RootCancel(parent); Reason(ctx); WaitDone(ctx) error
  cmd/
    demo/
      main.go              cancels once, cancels again, reports the reason
  cancelbox_test.go        parent-cancels-child, reason, idempotency, leak proof
```

- Files: `cancelbox.go`, `cmd/demo/main.go`, `cancelbox_test.go`.
- Implement: `RootCancel(parent)` (a documented `WithCancel`), `Reason(ctx)` (active vs cancelled), and `WaitDone(ctx)` (block until `Done`, return `ctx.Err()`).
- Test: a child cancels when its parent does; `Reason` flips on cancel; a double cancel keeps `ctx.Err()` at `context.Canceled`; no goroutine leaks after teardown.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p cancelbox/cmd/demo
cd cancelbox
go mod init example.com/cancelbox
```

### Why idempotent cancel and leak-freedom are the invariants that matter

`context.WithCancel` makes two promises that production code leans on constantly,
and both deserve an explicit test rather than trust.

The first is *transitivity*: a child derived from a parent is cancelled the
instant the parent is, without the child's own `cancel` being called. This is
what lets one top-level `cancel` at the end of a request collapse an entire
subtree of goroutines. `WaitDone` makes the invariant observable — it blocks on
`ctx.Done()` and returns `ctx.Err()` — so a test can start a goroutine on the
*child*, cancel the *parent*, and assert the goroutine returns `context.Canceled`.

The second is *idempotent cancel*: the docs guarantee that the second and later
calls to a `CancelFunc` are no-ops. This is not a curiosity — it is what makes the
"cancel on the happy path, plus a defensive `defer cancel()`" idiom legal, and
what makes patterns that may cancel from more than one place safe. `ctx.Err()`
after any number of cancels is still exactly `context.Canceled`; a second cancel
does not overwrite it, does not panic, does nothing. `TestCancelIsIdempotent`
pins this: cancel twice, assert the error is unchanged.

The third property is not a promise of the API but of *your* code: no goroutine
leaks after everything is torn down. `WaitDone` is the perfect thing to leak-test
because it parks a goroutine on `Done()` — if cancellation did not actually close
`Done()`, that goroutine would hang forever. `TestNoGoroutineLeak` captures
`runtime.NumGoroutine()` before, starts a waiter, cancels, and polls the count
back to baseline. If it never returns, the cancellation path is broken.

Create `cancelbox.go`:

```go
package cancelbox

import (
	"context"
	"fmt"
)

// RootCancel derives a cancellable child from parent. It is a thin, documented
// wrapper over context.WithCancel that exists to name intent at call sites; the
// returned CancelFunc must be called on every path (defer cancel()).
func RootCancel(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

// Reason reports whether ctx is still active or why it was cancelled. It reads
// ctx.Err(), so it returns "active" until cancellation and a message after.
func Reason(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("cancelled: %v", err)
	}
	return "active"
}

// WaitDone blocks until ctx is cancelled and returns ctx.Err(). It makes the
// cancellation of a context observable to a goroutine, which is what the leak
// and parent-cancels-child tests hang on.
func WaitDone(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
```

### The runnable demo

The demo cancels a context, cancels it a second time to show the no-op, and prints
the reason each step — demonstrating that `ctx.Err()` is stable across repeated
cancels.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/cancelbox"
)

func main() {
	ctx, cancel := cancelbox.RootCancel(context.Background())
	fmt.Println("before cancel:", cancelbox.Reason(ctx))

	cancel()
	fmt.Println("after cancel:", cancelbox.Reason(ctx))

	cancel() // idempotent: no panic, no change
	fmt.Println("after second cancel:", cancelbox.Reason(ctx))

	fmt.Println("is context.Canceled:", errors.Is(ctx.Err(), context.Canceled))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before cancel: active
after cancel: cancelled: context canceled
after second cancel: cancelled: context canceled
is context.Canceled: true
```

### Tests

`TestChildCancelledWhenParentCancelled` starts a waiter on the *child* and cancels
the *parent*, asserting the waiter returns `context.Canceled` — the transitivity
invariant. `TestReasonReportsActiveAndCancelled` checks the state flip.
`TestCancelIsIdempotent` cancels twice and asserts `ctx.Err()` stays
`context.Canceled`. `TestNoGoroutineLeak` captures the goroutine count, parks a
`WaitDone` waiter, cancels, and polls the count back to baseline.

Create `cancelbox_test.go`:

```go
package cancelbox

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestChildCancelledWhenParentCancelled(t *testing.T) {
	t.Parallel()

	parent, cancelParent := RootCancel(context.Background())
	child, cancelChild := RootCancel(parent)
	defer cancelChild()

	done := make(chan error, 1)
	go func() {
		done <- WaitDone(child)
	}()

	cancelParent()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("child err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("child did not observe parent cancellation within 1s")
	}
}

func TestReasonReportsActiveAndCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := RootCancel(context.Background())
	if got := Reason(ctx); got != "active" {
		t.Fatalf("Reason before cancel = %q, want %q", got, "active")
	}

	cancel()
	if got := Reason(ctx); got == "active" {
		t.Fatalf("Reason after cancel = %q, want a cancellation message", got)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := RootCancel(context.Background())

	cancel()
	first := ctx.Err()
	cancel() // no-op
	second := ctx.Err()

	if !errors.Is(first, context.Canceled) {
		t.Fatalf("err after first cancel = %v, want context.Canceled", first)
	}
	if !errors.Is(second, context.Canceled) {
		t.Fatalf("err after second cancel = %v, want context.Canceled", second)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	ctx, cancel := RootCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- WaitDone(ctx)
	}()

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitDone = %v, want context.Canceled", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: NumGoroutine=%d, base=%d",
				runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}
}
```

## Review

The two API invariants are correct when the child observes the parent's cancel
(transitivity) and a repeated cancel leaves `ctx.Err()` untouched at
`context.Canceled` (idempotency). Assert both with `errors.Is`, never `==`. The
leak proof is your own code's contract: a goroutine parked on `WaitDone` must
return once you cancel, and `runtime.NumGoroutine()` must fall back to the
baseline you captured before starting it — if it does not, either `Done()` never
closed or the waiter is stuck elsewhere. The common trap is treating a double
cancel as an error to guard against; it is explicitly legal, and pretending
otherwise leads to fragile "have I already cancelled?" bookkeeping. Run
`go test -race` to confirm the parent/child handoff is synchronized.

## Resources

- [context.WithCancel](https://pkg.go.dev/context#WithCancel) — the derived context and idempotent `CancelFunc`.
- [context package](https://pkg.go.dev/context) — `Context.Err`, `context.Canceled`, concurrency safety.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the count behind the leak proof.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-cancel-with-cause-fanout.md](04-cancel-with-cause-fanout.md)
