# Exercise 7: Rate-Limited Egress Stage — Respect A Downstream Quota With x/time/rate

A fan-out enrichment stage that calls a third-party API must stay under that API's
requests-per-second, or the provider starts returning 429s and may throttle the
whole account. The standard tool is `golang.org/x/time/rate.Limiter`: a token
bucket that refills at rate `r` with burst `b`. The cancellation-safe way to gate a
stage on it is `limiter.Wait(ctx)`, which blocks for a token but returns promptly
when the context is cancelled. This module builds that egress stage and contrasts
`Wait` with `Allow` and `Reserve`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports `golang.org/x/time/rate`.

## What you'll build

```text
egress/                      module example.com/egress
  go.mod                     requires golang.org/x/time
  egress.go                  func RateLimit(ctx, in, lim) (<-chan int, <-chan error)
  cmd/
    demo/
      main.go                emits 6 items at 100/s, prints elapsed >= expected
  egress_test.go             throughput bound, cancel unblocks Wait, burst spike, no leak
```

Files: `egress.go`, `cmd/demo/main.go`, `egress_test.go`.
Implement: `RateLimit(ctx, in, lim) (<-chan int, <-chan error)` — gate each emission
through `lim.Wait(ctx)`, forwarding on success and reporting a cancellation error on
its error channel.
Test: N emissions under rate `r` take at least the expected duration, `Wait`
returns `context.Canceled` promptly when cancelled mid-wait, a burst allows an
initial spike, and no goroutine leaks after cancel.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/07-rate-limited-egress-stage/cmd/demo
cd go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/07-rate-limited-egress-stage
go get golang.org/x/time/rate
```

### Wait vs Allow vs Reserve inside a stage

A `*rate.Limiter` created with `rate.NewLimiter(r, b)` allows an average of `r`
events per second with bursts up to `b`. It offers three ways to consume a token,
and the choice is a behavioral decision, not a style one:

- `Wait(ctx)` blocks until a token is available (or the reservation would exceed
  the context deadline), returning `nil` when it got one and a context error when
  the context is cancelled first. This is *delay* semantics: nothing is dropped,
  the caller is paced. Inside a cancellable stage this is the right default,
  because a cancel unblocks the wait instead of leaving the goroutine parked.
- `Allow()` is non-blocking: it returns `true` if a token was available *right now*
  and `false` otherwise, consuming nothing on `false`. This is *drop* semantics —
  use it for load shedding, where over-quota work is discarded rather than queued.
- `Reserve()` returns a `*Reservation` carrying a `Delay()` you must sleep yourself
  before acting; it reserves the token immediately. It is the most flexible (you
  can `Cancel()` a reservation you decide not to use) but you own the wait, so you
  must make *that* sleep cancellation-safe. `Wait` is essentially `Reserve` plus a
  cancellable sleep, done correctly for you.

The egress stage uses `Wait(ctx)`. Each item, before being forwarded, waits for a
token; if the wait returns an error (the context was cancelled), the stage sends
that error on a side channel and returns. Because `Wait` honors the context, a
cancelled pipeline unblocks the stage instantly rather than blocking until the next
token would have been available — the difference between a shutdown that completes
in microseconds and one that stalls for the token interval.

The stage returns two channels: the forwarded values and an error channel that
carries the terminal cancellation error (buffered size 1 so the stage can send it
and exit without a reader). This keeps the value channel typed as plain `int` while
still surfacing why the stage stopped.

Create `egress.go`:

```go
package egress

import (
	"context"

	"golang.org/x/time/rate"
)

// RateLimit forwards items from in to its output no faster than lim allows,
// waiting for a token before each emission via lim.Wait(ctx). If ctx is cancelled
// while waiting, it sends the context error on the error channel and stops. The
// error channel is buffered (size 1) and receives at most one terminal error.
func RateLimit(ctx context.Context, in <-chan int, lim *rate.Limiter) (<-chan int, <-chan error) {
	out := make(chan int)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		for v := range in {
			if err := lim.Wait(ctx); err != nil {
				errc <- err
				return
			}
			select {
			case out <- v:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()
	return out, errc
}
```

### The runnable demo

The demo emits six items through a limiter of 100 events/second with burst 1, so
the emissions are paced roughly 10 ms apart, and prints whether the elapsed time
meets the lower bound the rate implies. `rate.Every` converts an interval into a
`rate.Limit`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	"example.com/egress"
)

func main() {
	in := make(chan int)
	go func() {
		defer close(in)
		for i := 1; i <= 6; i++ {
			in <- i
		}
	}()

	lim := rate.NewLimiter(rate.Every(10*time.Millisecond), 1) // 100/s, burst 1
	start := time.Now()
	out, _ := egress.RateLimit(context.Background(), in, lim)

	n := 0
	for range out {
		n++
	}
	elapsed := time.Since(start)
	// With burst 1, 6 items are paced ~5 intervals apart -> >= 50 ms.
	fmt.Printf("emitted=%d elapsed>=50ms=%v\n", n, elapsed >= 50*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
emitted=6 elapsed>=50ms=true
```

### Tests

`TestThroughputBounded` sends N items through a slow limiter and asserts the total
time is at least what the rate implies (with the burst accounted for).
`TestCancelUnblocksWait` cancels while the stage is parked in `Wait` and asserts the
error channel carries `context.Canceled` promptly, proving the wait is
cancellation-safe. `TestBurstAllowsInitialSpike` uses a burst larger than one and
asserts the first burst-sized group of items comes out nearly immediately.
`TestNoLeakAfterCancel` checks the goroutine baseline.

Create `egress_test.go`:

```go
package egress

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func source(n int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := 0; i < n; i++ {
			out <- i
		}
	}()
	return out
}

func TestThroughputBounded(t *testing.T) {
	t.Parallel()

	const n = 5
	lim := rate.NewLimiter(rate.Every(10*time.Millisecond), 1) // burst 1
	start := time.Now()

	out, _ := RateLimit(context.Background(), source(n), lim)
	got := 0
	for range out {
		got++
	}
	elapsed := time.Since(start)

	if got != n {
		t.Fatalf("emitted %d, want %d", got, n)
	}
	// Burst 1 means the first token is immediate; the remaining n-1 are paced
	// one interval apart -> at least (n-1)*10ms.
	min := time.Duration(n-1) * 10 * time.Millisecond
	if elapsed < min {
		t.Fatalf("elapsed = %v, want >= %v (rate not enforced)", elapsed, min)
	}
}

func TestCancelUnblocksWait(t *testing.T) {
	t.Parallel()

	// A very slow limiter: after the burst token, the next Wait parks for ~1s.
	lim := rate.NewLimiter(rate.Every(time.Second), 1)
	ctx, cancel := context.WithCancel(context.Background())

	// An input that keeps offering items so the stage reaches its second Wait.
	in := make(chan int)
	go func() {
		defer close(in)
		for i := 0; ; i++ {
			select {
			case in <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	out, errc := RateLimit(ctx, in, lim)
	<-out // first item passes on the burst token
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Wait did not unblock promptly on cancel (stage still parked)")
	}
}

func TestBurstAllowsInitialSpike(t *testing.T) {
	t.Parallel()

	// Burst 3: the first 3 tokens are available immediately.
	lim := rate.NewLimiter(rate.Every(50*time.Millisecond), 3)
	start := time.Now()

	out, _ := RateLimit(context.Background(), source(3), lim)
	for range out {
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("burst of 3 took %v, want near-immediate (< 40ms)", elapsed)
	}
}

func TestNoLeakAfterCancel(t *testing.T) {
	before := runtime.NumGoroutine()

	lim := rate.NewLimiter(rate.Every(time.Second), 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan int)
	go func() {
		defer close(in)
		for i := 0; ; i++ {
			select {
			case in <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	out, _ := RateLimit(ctx, in, lim)
	<-out
	cancel()
	for range out {
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if runtime.NumGoroutine() > before+2 {
		t.Fatalf("leak: before=%d after=%d", before, runtime.NumGoroutine())
	}
}
```

`TestNoLeakAfterCancel` runs serially (no `t.Parallel()`) because it reads the
process-global goroutine count.

## Review

The egress stage is correct when its output is paced to the limiter's rate, a burst
is allowed through immediately, and a cancel while parked in `Wait` unblocks the
stage promptly with `context.Canceled` on the error channel. `Wait(ctx)` is the
whole point: swap it for a `time.Sleep(interval)` between emits and the throughput
test still passes, but `TestCancelUnblocksWait` fails, because a sleeping stage
ignores the cancel until the sleep ends. Reserve `Allow` for load shedding and
`Reserve` for custom scheduling; for a cancellable pacing stage, `Wait` is the tool.
Run `go test -race`.

## Resources

- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — `NewLimiter`, `Wait`, `Allow`, `Reserve`, `Every`.
- [`rate.Limiter.Wait`](https://pkg.go.dev/golang.org/x/time/rate#Limiter.Wait) — the cancellation-safe token acquisition.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the stage shape the limiter plugs into.

---

Back to [06-batching-flush-stage.md](06-batching-flush-stage.md) | Next: [08-retry-with-backoff-stage.md](08-retry-with-backoff-stage.md)
