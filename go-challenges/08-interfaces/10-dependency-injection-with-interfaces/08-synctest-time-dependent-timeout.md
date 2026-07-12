# Exercise 8: Deterministic Time Tests With testing/synctest For A Timeout Guard

Injecting a clock is one way to make time-dependent code testable; Go 1.25's
`testing/synctest` is the other. When the code legitimately uses real
`time.After`, `time.Ticker`, or `context.WithTimeout`, a synctest bubble
virtualizes the `time` package underneath it, so a two-second deadline fires
deterministically in microseconds with no injected clock at all. This exercise
builds a `WithTimeout` guard and a ticker-based `Poll`, then tests both under a
bubble.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests. It pins `go 1.25` because `testing/synctest` requires it.

## What you'll build

```text
timeout/                    independent module: example.com/timeout
  go.mod                    go 1.25 (testing/synctest needs it)
  timeout.go                WithTimeout (context.WithTimeout + select); Poll (time.Ticker)
  cmd/
    demo/
      main.go               real-time WithTimeout: a fast op and a slow op
  timeout_test.go           synctest bubble: deadline fires at exactly virtual 2s; Poll + synctest.Wait
```

- Files: `timeout.go`, `cmd/demo/main.go`, `timeout_test.go`.
- Implement: `WithTimeout(ctx, d, op)` that runs `op` under a `context.WithTimeout` and returns `ctx.Err()` if the deadline fires first; `Poll(ctx, interval, check)` that checks a condition on a `time.Ticker` until it holds or the context is done.
- Test: under `synctest.Test`, assert the timeout fires with `context.DeadlineExceeded` and that the *virtual* elapsed time is exactly the deadline; assert a fast op completes; use `synctest.Wait` to synchronize with a `Poll` running in a background goroutine.
- Verify: `go test -count=1 -race ./...`

Set up the module. `testing/synctest` requires Go 1.25+:

```bash
mkdir -p go-solutions/08-interfaces/10-dependency-injection-with-interfaces/08-synctest-time-dependent-timeout/cmd/demo
cd go-solutions/08-interfaces/10-dependency-injection-with-interfaces/08-synctest-time-dependent-timeout
go mod edit -go=1.25
```

### Injection versus virtualized time

The previous exercise injected a `Sleeper` so a fake could record the backoff
schedule. That is the right tool when you want to *control* time in production too,
or when you must build on a toolchain older than Go 1.25. But it has a cost: the
production code carries a `Sleeper` parameter that exists only for the test. When
the code legitimately wants to call `time.After` or `context.WithTimeout` directly,
`testing/synctest` removes that cost. Inside `synctest.Test(t, ...)`, the `time`
package is virtualized: timers, tickers, and the deadline behind
`context.WithTimeout` all run on a fake clock that advances only when every
goroutine in the bubble is durably blocked. So `WithTimeout(ctx, 2*time.Second,
blockingOp)` does not wait two real seconds — the bubble sees all goroutines
blocked, jumps the clock to the deadline, and the timeout fires instantly. The
production code has no clock parameter; the test still runs in microseconds.

`WithTimeout` itself is ordinary production code. It derives a
`context.WithTimeout` child, runs `op` in a goroutine, and selects between the
operation finishing and the context's deadline. The `done` channel is buffered
(size one) so that if the deadline wins the race, the operation goroutine can still
send its result and exit rather than leaking — which matters doubly under synctest,
where a leaked goroutine that never becomes durably blocked would hang the bubble.
`op` receives the derived context and is expected to honor it, so a cancellation
propagates into the operation.

`Poll` shows the ticker case and motivates `synctest.Wait`. It checks a condition,
then waits for the next tick, looping until the condition holds or the context is
cancelled. In the test, a background goroutine flips the condition after some
virtual time; `synctest.Wait` lets the test synchronize with the poller — it
returns only once the poller goroutine is durably blocked on its ticker — so the
test can assert the poller has *not* yet returned before the condition is set,
without a race and without a real sleep.

Create `timeout.go`:

```go
package timeout

import (
	"context"
	"time"
)

// WithTimeout runs op under a context that is cancelled after d. If op finishes
// first, its error is returned; if the deadline fires first, ctx.Err()
// (context.DeadlineExceeded) is returned. op must honor the context it is given.
func WithTimeout(ctx context.Context, d time.Duration, op func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	done := make(chan error, 1) // buffered so the op goroutine never leaks
	go func() { done <- op(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Poll calls check on each tick of interval until it returns true (nil) or the
// context is cancelled (ctx.Err()). The first check runs immediately.
func Poll(ctx context.Context, interval time.Duration, check func() bool) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if check() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
```

### The runnable demo

The demo runs `WithTimeout` against real time (not a bubble) so you can watch an
actual timeout: a fast operation completes, a slow one is cut off by the deadline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/timeout"
)

func main() {
	// Fast op finishes well within the deadline.
	err := timeout.WithTimeout(context.Background(), 50*time.Millisecond, func(ctx context.Context) error {
		select {
		case <-time.After(5 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		fmt.Println("fast op:", err)
	} else {
		fmt.Println("fast op: ok")
	}

	// Slow op is cut off by the deadline.
	err = timeout.WithTimeout(context.Background(), 20*time.Millisecond, func(ctx context.Context) error {
		select {
		case <-time.After(200 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	fmt.Println("slow op:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast op: ok
slow op: context deadline exceeded
```

### Tests

The synctest tests are where virtual time earns its keep. `TestTimeoutFires` asserts
not only that the deadline produces `context.DeadlineExceeded`, but that the
*virtual* elapsed time measured with `time.Since` is exactly the deadline — a
precision only possible because the bubble clock has no scheduler slack.
`TestCompletesBeforeTimeout` asserts a fast op returns nil. `TestPollWaitsForReady`
uses `synctest.Wait` to synchronize with a background `Poll` before asserting it has
not returned prematurely.

Create `timeout_test.go`:

```go
package timeout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestTimeoutFires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		err := WithTimeout(context.Background(), 2*time.Second, func(ctx context.Context) error {
			<-ctx.Done() // block until cancelled
			return ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("virtual elapsed = %v, want exactly 2s", elapsed)
		}
	})
}

func TestCompletesBeforeTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		err := WithTimeout(context.Background(), 2*time.Second, func(ctx context.Context) error {
			select {
			case <-time.After(500 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
}

func TestPollWaitsForReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var ready atomic.Bool
		resultCh := make(chan error, 1)
		go func() {
			resultCh <- Poll(context.Background(), 100*time.Millisecond, func() bool {
				return ready.Load()
			})
		}()

		// Wait until the poller has run its first (false) check and is durably
		// blocked on its ticker.
		synctest.Wait()
		select {
		case <-resultCh:
			t.Fatal("Poll returned before the resource was ready")
		default:
		}

		// Now make the resource ready; the next tick lets Poll observe it.
		ready.Store(true)
		if err := <-resultCh; err != nil {
			t.Fatalf("Poll: unexpected error: %v", err)
		}
	})
}
```

## Review

The guard is correct when the timeout is a pure consequence of the deadline: under
synctest, `TestTimeoutFires` sees `context.DeadlineExceeded` at exactly two virtual
seconds, and `TestCompletesBeforeTimeout` sees nil when the op is fast. The buffered
`done` channel is load-bearing — without it, a deadline that wins the race would
leave the op goroutine unable to send, leaking it and, under synctest, hanging the
bubble. `TestPollWaitsForReady` shows `synctest.Wait` synchronizing with a
background goroutine so the test reads its state without a race. The choice between
this and the injected-clock style of Exercise 7 is a real design decision: inject a
clock when you also need to control time in production or must support older
toolchains; reach for synctest when the code legitimately uses real timers and you
only need determinism in the test. Run `go test -race` to confirm.

## Resources

- [testing/synctest](https://pkg.go.dev/testing/synctest) — `synctest.Test`, `synctest.Wait`, and the fake clock.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the Go blog introduction, including the timeout and ticker patterns.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the deadline whose timer the bubble virtualizes.
- [time.Ticker](https://pkg.go.dev/time#Ticker) — the ticker `Poll` is built on, also virtualized inside a bubble.

---

Back to [07-injected-clock-retry-backoff.md](07-injected-clock-retry-backoff.md) | Next: [09-health-check-aggregator.md](09-health-check-aggregator.md)
