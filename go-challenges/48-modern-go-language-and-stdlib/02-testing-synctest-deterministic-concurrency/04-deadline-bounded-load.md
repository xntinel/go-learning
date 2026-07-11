# Exercise 4: Bounding a Slow Load With a Deadline

A cache miss triggers a load — a database query, an RPC — that might hang, so real
caches bound it with a deadline. `LoadWithDeadline` races the load against a
timeout and returns whichever wins. Writing it correctly forces two lessons that
`synctest` makes visible and testable instantly: the load must be *cancellable*,
and the result channel must be *buffered*.

This module is fully self-contained. It begins with its own `go mod init`, defines
everything it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
deadline/                  independent module: example.com/deadline
  go.mod                   go 1.25 (synctest needs it)
  deadline.go              LoadWithDeadline[V](timeout, load func(ctx)) (V, error)
  cmd/
    demo/
      main.go              runnable demo: a slow load is cut off at the deadline
  deadline_test.go         synctest: fast path passes through + virtualized timeout fires
```

- Files: `deadline.go`, `cmd/demo/main.go`, `deadline_test.go`.
- Implement: `LoadWithDeadline[V any](timeout time.Duration, load func(context.Context) (V, error)) (V, error)` that runs `load` in a goroutine with a `context.WithTimeout` and selects between its result and `ctx.Done()`.
- Test: a bubble that checks a fast load passes its value through and a never-returning (but cancellable) load hits `context.DeadlineExceeded` at exactly the virtual timeout.
- Verify: `go test -count=1 -race ./...`

Set up the module (`testing/synctest` requires Go 1.25+):

```bash
mkdir -p ~/go-exercises/deadline/cmd/demo
cd ~/go-exercises/deadline
go mod init example.com/deadline
go mod edit -go=1.25
```

### Cancellable load, buffered channel, virtualized timeout

`LoadWithDeadline` builds a `context.WithTimeout`, launches the load in a goroutine
that sends its `(value, err)` to a channel, and `select`s between that channel and
`ctx.Done()`. Two design choices are not optional, and `synctest` is what turns
both from "good practice" into "the test deadlocks if you get it wrong".

The load must be *cancellable*. It receives the `context.Context` and a
well-behaved one watches `ctx.Done()`, so when the deadline fires the load
goroutine returns at the same instant instead of running on. A load with no way to
stop becomes a leaked goroutine — and under `synctest`, a goroutine still blocked
when the bubble's root goroutine exits stops virtual time and is reported as a
deadlock. The slow-path test below uses exactly such a load (`<-ctx.Done()`) to
prove it exits cleanly.

The result channel must be *buffered*, size 1. When the `select` picks `ctx.Done()`
the load goroutine is still alive and about to send its result; with an unbuffered
channel that send would block forever because the `select` has already moved on and
no one is receiving. A buffer of one lets the loser of the race deposit its result
and exit, so the goroutine drains and the bubble closes. This is the "give the
losing branch somewhere to go" rule made concrete.

Finally, `context.WithTimeout` builds its deadline on a timer, which the bubble
virtualizes like any other. The timeout path is therefore tested instantly and
deterministically — no "sleep slightly longer than the timeout and hope". The
deadline fires at exactly the virtual instant you asked for.

Create `deadline.go`:

```go
package deadline

import (
	"context"
	"time"
)

// LoadWithDeadline runs load with a deadline. If load returns before the
// timeout, its result passes through; otherwise the context is cancelled and
// LoadWithDeadline returns the context error. A well-behaved load watches
// ctx.Done, so its goroutine exits promptly instead of leaking -- which is also
// what lets a synctest bubble drain cleanly.
func LoadWithDeadline[V any](timeout time.Duration, load func(context.Context) (V, error)) (V, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	type result struct {
		value V
		err   error
	}
	ch := make(chan result, 1) // buffered so the loser of the select can still send and exit
	go func() {
		value, err := load(ctx)
		ch <- result{value, err}
	}()

	select {
	case r := <-ch:
		return r.value, r.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}
```

### The runnable demo

The demo runs against the real clock and shows the timeout branch winning: it gives
a 50 ms deadline to a load that would take a full second, but the load respects
cancellation, so it is cut off and `LoadWithDeadline` returns the context error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/deadline"
)

func main() {
	// A slow load that respects cancellation: it is cut off at the deadline.
	v, err := deadline.LoadWithDeadline(50*time.Millisecond, func(ctx context.Context) (int, error) {
		select {
		case <-time.After(time.Second): // would take a second
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	fmt.Printf("value=%d err=%v\n", v, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value=0 err=context deadline exceeded
```

### Tests

`TestLoadWithDeadline` exercises both branches in one bubble. The fast load returns
`42` immediately and must pass through untouched. The slow load blocks on
`<-ctx.Done()` and never produces a value, so the timeout must fire at exactly one
virtual second and the call must return `context.DeadlineExceeded` with the zero
value. That the slow load exits the moment the deadline fires is what lets the
bubble drain; an uncancellable load here would hang and be reported as a deadlock.

Create `deadline_test.go`:

```go
package deadline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestLoadWithDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// A load that returns before the deadline passes its value through.
		v, err := LoadWithDeadline(time.Second, func(context.Context) (int, error) {
			return 42, nil
		})
		if err != nil || v != 42 {
			t.Fatalf("fast load = %d,%v; want 42,nil", v, err)
		}

		// A cancellable load that never produces a value hits the deadline.
		v, err = LoadWithDeadline(time.Second, func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || v != 0 {
			t.Fatalf("slow load = %d,%v; want 0, DeadlineExceeded", v, err)
		}
	})
}

func Example() {
	v, err := LoadWithDeadline(time.Second, func(context.Context) (string, error) {
		return "ready", nil
	})
	fmt.Println(v, err)
	// Output: ready <nil>
}
```

## Review

The function is correct when the fast path returns the load's own result and the
slow path returns `ctx.Err()` at exactly the virtual deadline, with no goroutine
left behind. The two failure modes are the ones the design comments call out.
Drop the buffer on `ch` and the timeout-branch test deadlocks: the load goroutine
blocks forever trying to send to a channel no one receives, and `synctest.Test`
reports it. Make the load uncancellable (ignore `ctx`) and the same thing happens
once it outlives the deadline — which is why the slow-path test deliberately uses a
load that watches `ctx.Done()`. Confirm the timeout is detected with
`errors.Is(err, context.DeadlineExceeded)` rather than a string compare, and run
`go test -race` to confirm the goroutine's send and the parent's receive are
properly synchronized.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — the deadline timer the bubble virtualizes so the timeout branch fires instantly.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — why a leaked or uncancellable load goroutine turns into a reported deadlock.
- [Go Concurrency Patterns: Context](https://go.dev/blog/context) — the cancellation model the load goroutine relies on to exit at the deadline.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-retry-backoff.md](05-retry-backoff.md)
