# Exercise 5: Throttle a Firehose Consumer with a Token-Bucket Limiter

A bursty producer can overwhelm a downstream that has a quota — a third-party API
capped at 100 req/s, a database that melts above a write rate. The consumer's job
is to smooth the firehose to a sustained rate with a bounded burst. This exercise
ranges an inbound channel and gates each item through a `golang.org/x/time/rate`
token-bucket limiter.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ratelimited/                independent module: example.com/ratelimited
  go.mod                    go 1.26; requires golang.org/x/time
  consumer.go               ProcessWait (Wait per item); ProcessAllow (drop when no token)
  cmd/
    demo/
      main.go               drain a burst at a fixed rate
  consumer_test.go          elapsed lower bound, cancel stops mid-drain, Allow drops
```

Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
Implement: `ProcessWait(ctx, in, handle) (int, error)` that calls `limiter.Wait(ctx)` before each item (blocking to shape the rate), and `ProcessAllow(in, handle) (done, dropped int)` that uses `limiter.Allow()` and drops items when no token is available.
Test: with `rate.Every(10ms)` and burst 1, draining N items takes at least `(N-1)*10ms`; a cancelled context makes `Wait` return an error and stops the drain mid-stream; the `Allow` variant drops when the bucket is empty and records the count.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimited/cmd/demo
cd ~/go-exercises/ratelimited
go mod init example.com/ratelimited
go get golang.org/x/time/rate
```

### Wait versus Allow: block or drop

A token-bucket limiter refills tokens at a steady rate up to a burst capacity.
Each unit of work costs one token. There are two ways a ranging consumer can use
it, and they encode opposite policies:

- **`Wait(ctx)` — block to shape.** Before processing each item, the consumer
  calls `limiter.Wait(ctx)`, which blocks until a token is available (or `ctx` is
  cancelled). No work is dropped; the producer's burst is absorbed into the
  channel buffer and released downstream at the sustained rate. This is the right
  policy when every item must eventually be processed and you are protecting a
  quota, not shedding load. `Wait` returns an error when the context is cancelled,
  which is the consumer's signal to stop mid-drain — the one place cancellation
  reaches into an otherwise-simple range.

- **`Allow()` — drop to shed.** `limiter.Allow()` returns `true` if a token was
  available (and consumes it) or `false` immediately if not. A consumer that skips
  items on `false` sheds load rather than queuing it — the right policy for a
  firehose where stale items are worthless (live metrics, best-effort telemetry)
  and unbounded queuing would exhaust memory.

`ProcessWait` ranges `in`, and because it must observe cancellation via `Wait`, it
returns `(processed, error)` where the error is non-nil exactly when the context
was cancelled before the drain finished. `ProcessAllow` needs no context — it
never blocks — so it just returns the processed and dropped counts.

Create `consumer.go`:

```go
package ratelimited

import (
	"context"

	"golang.org/x/time/rate"
)

// Consumer throttles processing of an inbound stream to a limiter's rate.
type Consumer struct {
	limiter *rate.Limiter
}

// New builds a Consumer whose limiter allows r events per second with the given
// burst. Use rate.Every to express r as an interval (e.g. rate.Every(10*ms)).
func New(r rate.Limit, burst int) *Consumer {
	return &Consumer{limiter: rate.NewLimiter(r, burst)}
}

// ProcessWait ranges in and blocks on the limiter before each item, shaping a
// bursty producer to the sustained rate. It stops early and returns the limiter's
// error if ctx is cancelled; otherwise it returns the number processed and nil.
func (c *Consumer) ProcessWait(ctx context.Context, in <-chan int, handle func(int)) (int, error) {
	processed := 0
	for v := range in {
		if err := c.limiter.Wait(ctx); err != nil {
			return processed, err // context cancelled: stop mid-drain
		}
		handle(v)
		processed++
	}
	return processed, nil
}

// ProcessAllow ranges in and drops any item for which no token is available,
// shedding load instead of queuing it. It returns (processed, dropped).
func (c *Consumer) ProcessAllow(in <-chan int, handle func(int)) (int, int) {
	processed, dropped := 0, 0
	for v := range in {
		if c.limiter.Allow() {
			handle(v)
			processed++
		} else {
			dropped++
		}
	}
	return processed, dropped
}
```

### The runnable demo

The demo fills a buffered channel with five items instantly (the burst), then
drains them through a limiter set to one item every 30 ms with burst 1. The first
item goes immediately; the rest are released at 30 ms intervals, so the whole
drain takes roughly 120 ms.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	"example.com/ratelimited"
)

func main() {
	c := ratelimited.New(rate.Every(30*time.Millisecond), 1)

	in := make(chan int, 5)
	for i := 1; i <= 5; i++ {
		in <- i
	}
	close(in)

	start := time.Now()
	n, err := c.ProcessWait(context.Background(), in, func(v int) {
		fmt.Printf("processed %d at +%dms\n", v, time.Since(start).Milliseconds()/10*10)
	})
	fmt.Printf("done: %d processed, err=%v\n", n, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (timestamps are approximate; the point is the ~30 ms spacing):

```
processed 1 at +0ms
processed 2 at +30ms
processed 3 at +60ms
processed 4 at +90ms
processed 5 at +120ms
done: 5 processed, err=nil
```

### Tests

`TestWaitShapesRate` drains five items at `rate.Every(10ms)` burst 1 and asserts
the elapsed time is at least `(5-1)*10ms = 40ms`. It is a lower-bound assertion,
never an equality, because the scheduler adds slack you cannot predict — asserting
"exactly 40 ms" would flake constantly. `TestWaitStopsOnCancel` cancels the
context and asserts `ProcessWait` returns a non-nil error and processes fewer than
all items. `TestAllowDrops` bursts many items through a slow `Allow` limiter and
asserts most are dropped and `processed + dropped` equals the input count (nothing
vanishes unaccounted).

Create `consumer_test.go`:

```go
package ratelimited

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func filled(vals ...int) chan int {
	ch := make(chan int, len(vals))
	for _, v := range vals {
		ch <- v
	}
	close(ch)
	return ch
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range n {
		out[i] = i
	}
	return out
}

func TestWaitShapesRate(t *testing.T) {
	t.Parallel()
	const n = 5
	interval := 10 * time.Millisecond
	c := New(rate.Every(interval), 1)

	start := time.Now()
	got, err := c.ProcessWait(context.Background(), filled(seq(n)...), func(int) {})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ProcessWait err = %v, want nil", err)
	}
	if got != n {
		t.Fatalf("processed = %d, want %d", got, n)
	}
	// First item is free (burst 1); the remaining n-1 are spaced by interval.
	if min := time.Duration(n-1) * interval; elapsed < min {
		t.Fatalf("elapsed = %v, want at least %v (rate not enforced)", elapsed, min)
	}
}

func TestWaitStopsOnCancel(t *testing.T) {
	t.Parallel()
	// Slow limiter so Wait blocks; cancel forces it to error out mid-drain.
	c := New(rate.Every(time.Hour), 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	got, err := c.ProcessWait(ctx, filled(seq(10)...), func(int) {})
	if err == nil {
		t.Fatal("ProcessWait err = nil after cancel, want non-nil")
	}
	if got >= 10 {
		t.Fatalf("processed = %d after cancel, want < 10 (stopped mid-drain)", got)
	}
}

func TestAllowDrops(t *testing.T) {
	t.Parallel()
	const n = 20
	// Slow refill, burst 1: only the first item (and maybe a stray) gets a token.
	c := New(rate.Every(time.Hour), 1)

	processed, dropped := c.ProcessAllow(filled(seq(n)...), func(int) {})
	if processed+dropped != n {
		t.Fatalf("processed+dropped = %d, want %d (items unaccounted)", processed+dropped, n)
	}
	if dropped == 0 {
		t.Fatalf("dropped = 0, want > 0 (limiter should shed the burst)")
	}
}

func ExampleConsumer_ProcessAllow() {
	c := New(rate.Every(time.Hour), 1) // one token, then dry
	processed, dropped := c.ProcessAllow(filled(1, 2, 3), func(int) {})
	fmt.Printf("processed=%d dropped=%d\n", processed, dropped)
	// Output: processed=1 dropped=2
}
```

## Review

The consumer is correct when `Wait` shapes the sustained rate without dropping and
`Allow` sheds without blocking, and when cancellation cleanly stops the `Wait`
path. Assert timing only as a lower bound: the token bucket guarantees a *minimum*
spacing, and the scheduler can always add more, so `elapsed >= (n-1)*interval` is
the honest invariant and equality would flake. The cancel test is the one place a
range-based consumer observes cancellation — through `Wait`'s error, not a
`select`. `ProcessAllow`'s `processed + dropped == n` invariant is the accounting
proof that shedding loses nothing silently: every item is either handled or
counted as dropped.

## Resources

- [pkg.go.dev: golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — `Limiter`, `NewLimiter`, `Wait`, `Allow`, and `rate.Every`.
- [pkg.go.dev: rate.Limiter.Wait](https://pkg.go.dev/golang.org/x/time/rate#Limiter.Wait) — blocks until a token is available or the context is done.
- [Wikipedia: Token bucket](https://en.wikipedia.org/wiki/Token_bucket) — the refill-and-burst model the limiter implements.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-batch-flusher.md](04-batch-flusher.md) | Next: [06-graceful-shutdown-drain.md](06-graceful-shutdown-drain.md)
