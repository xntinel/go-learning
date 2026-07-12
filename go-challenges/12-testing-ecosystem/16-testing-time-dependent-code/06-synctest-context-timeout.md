# Exercise 6: Deterministically test context.WithTimeout under synctest

Every backend wraps outbound work in a deadline: a database query, an HTTP call, a
lock acquisition, all bounded by `context.WithTimeout`. Testing the timeout branch
honestly is notoriously flaky — the usual hack is "sleep slightly longer than the
timeout and hope" — because context deadlines schedule against the runtime clock.
`testing/synctest` virtualizes that clock, so a 30-second budget fires in
microseconds and both the timely-success and the timed-out paths are exact and
instant.

## What you'll build

```text
reqtimeout/                    independent module: example.com/reqtimeout
  go.mod
  timeout.go                   DoWithTimeout(parent, budget, work) — WithTimeout + select
  cmd/
    demo/
      main.go                  run fast work and slow work under a real budget; print outcomes
  timeout_test.go              synctest: success under budget; DeadlineExceeded when work overruns
```

Files: `timeout.go`, `cmd/demo/main.go`, `timeout_test.go`.
Implement: `DoWithTimeout` that runs `work` under `context.WithTimeout` and returns its result if it finishes in time, or `context.DeadlineExceeded` if the deadline fires first.
Test: inside `synctest.Test` — a fast `work` returns its value with a nil error; a slow `work` overruns the budget and the wrapper returns `errors.Is(err, context.DeadlineExceeded)`. No real waiting.
Verify: `go test -count=1 -race ./...`

Set up the module (synctest is stable in Go 1.25):

```bash
mkdir -p go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/06-synctest-context-timeout/cmd/demo
cd go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/06-synctest-context-timeout
go mod edit -go=1.25
```

### The pattern, and the buffered channel that prevents a leak

`DoWithTimeout` derives a child context with the budget, runs `work` in a
goroutine, and `select`s between the work's result and `ctx.Done()`. Whichever
fires first wins: if `work` returns before the deadline, its result propagates; if
the deadline fires first, the wrapper returns `ctx.Err()`, which for a timeout is
`context.DeadlineExceeded`.

The result channel is *buffered with capacity one*, and this is not optional. When
the deadline wins the `select`, `DoWithTimeout` returns immediately, but the work
goroutine is still running. If the channel were unbuffered, that goroutine would
block forever trying to send its result to a receiver that has already left —
leaking the goroutine. Under `synctest` that leak is fatal: the bubble waits for
all goroutines to exit and reports a deadlock. A one-slot buffer lets the work
goroutine deposit its result and exit even when nobody is listening. `work` also
receives the child context and should watch `ctx.Done()`, so it stops promptly
when the deadline fires rather than running to completion — the standard
cancellation-propagation contract.

Under the bubble, the whole thing is deterministic. For the success case, `work`
does a `time.After` shorter than the budget; virtual time advances to that instant
and the result arrives first. For the timeout case, `work` waits longer than the
budget; virtual time advances to the budget, `ctx.Done()` closes, the wrapper
returns `DeadlineExceeded`, and `work`'s own `select` also sees `ctx.Done()` and
exits. No line of the test sleeps in real time.

Create `timeout.go`:

```go
package reqtimeout

import (
	"context"
	"time"
)

// DoWithTimeout runs work under a child context bounded by budget. It returns
// work's result if it finishes first, or ctx.Err() (context.DeadlineExceeded on
// timeout) if the deadline fires first.
func DoWithTimeout(parent context.Context, budget time.Duration, work func(ctx context.Context) (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	type result struct {
		value string
		err   error
	}
	ch := make(chan result, 1) // buffered: the loser of the select can still send and exit
	go func() {
		v, err := work(ctx)
		ch <- result{v, err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.value, r.err
	}
}
```

### The runnable demo

The demo runs two jobs under a real 50ms budget: a fast one that finishes in 10ms
and a slow one that would take 200ms. It prints the timely result and the timeout
error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/reqtimeout"
)

func work(d time.Duration, out string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(d):
			return out, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	budget := 50 * time.Millisecond

	v, err := reqtimeout.DoWithTimeout(context.Background(), budget, work(10*time.Millisecond, "fast-result"))
	fmt.Printf("fast: value=%q err=%v\n", v, err)

	_, err = reqtimeout.DoWithTimeout(context.Background(), budget, work(200*time.Millisecond, "slow-result"))
	fmt.Printf("slow: err=%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: value="fast-result" err=<nil>
slow: err=context deadline exceeded
```

### Tests

`TestCompletesBeforeDeadline` runs work that finishes well inside a 30-second
budget and asserts the value returns with a nil error — proving the success path
without waiting 30 seconds. `TestExceedsDeadline` runs work that outlasts the
budget and asserts `errors.Is(err, context.DeadlineExceeded)`. Both run inside a
bubble, so the 30-second budget is virtual and the tests finish instantly.

Create `timeout_test.go`:

```go
package reqtimeout

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// slowWork sleeps d (respecting ctx) then returns out.
func slowWork(d time.Duration, out string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(d):
			return out, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func TestCompletesBeforeDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		v, err := DoWithTimeout(context.Background(), 30*time.Second, slowWork(time.Second, "ok"))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if v != "ok" {
			t.Fatalf("value = %q, want %q", v, "ok")
		}
	})
}

func TestExceedsDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		v, err := DoWithTimeout(context.Background(), 30*time.Second, slowWork(time.Hour, "never"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if v != "" {
			t.Fatalf("value = %q on timeout, want empty", v)
		}
	})
}

func TestWorkErrorPropagates(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sentinel := errors.New("boom")
		_, err := DoWithTimeout(context.Background(), time.Minute, func(context.Context) (string, error) {
			return "", sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

// ExampleDoWithTimeout shows the timely-success path deterministically: work
// that finishes instantly returns its value and the budget never fires, so no
// real-time waiting or clock is involved.
func ExampleDoWithTimeout() {
	v, err := DoWithTimeout(context.Background(), time.Minute, func(context.Context) (string, error) {
		return "done", nil
	})
	fmt.Printf("value=%q err=%v\n", v, err)
	// Output:
	// value="done" err=<nil>
}
```

## Review

The wrapper is correct when it returns work's result on the timely path, its
error when work itself fails, and `context.DeadlineExceeded` when the budget
fires first — and when no goroutine leaks on any path. The bubble makes the
30-second budget a microsecond test and makes the timeout deterministic instead of
"sleep and hope." The two traps: an unbuffered result channel (the work goroutine
leaks after a timeout, and the bubble deadlocks), and work that ignores `ctx.Done`
(it runs to completion after the deadline, wasting resources — here it also keeps
the bubble busy). Buffer the channel and have work watch the context. Run
`go test -race`; the goroutine handoff over the channel must be clean.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) and [`context.DeadlineExceeded`](https://pkg.go.dev/context#pkg-variables) — the deadline and its sentinel error.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — virtualizing the deadline's timer.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the blog's context-deadline example and the buffered-channel caveat.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-synctest-ticker-worker.md](05-synctest-ticker-worker.md) | Next: [07-debounce-timer-reset.md](07-debounce-timer-reset.md)
