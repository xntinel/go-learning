# Exercise 8: Throttle An Outbound Webhook Sender To A Downstream Budget

A worker pool caps how many calls run *at once*, but a third party usually
enforces a *rate* — say 50 requests per second — independent of your concurrency.
Ten workers all idle-waiting on the network can still blow a per-second quota. This
exercise builds a pooled webhook sender that bounds both: a fixed worker pool for
the concurrency cap and a shared `rate.Limiter` for the throughput cap, so the pool
never exceeds the downstream's rate regardless of worker count.

This module is fully self-contained. It uses `golang.org/x/time/rate`.

## What you'll build

```text
webhook/                   independent module: example.com/webhook
  go.mod                   go 1.25; require golang.org/x/time
  sender.go                type Sender; NewSender, Submit, Send, TrySend, Close
  cmd/
    demo/
      main.go              runnable demo: send 4 webhooks under a rate cap
  sender_test.go           throttle-rate, canceled-ctx, allow-drops tests, -race
```

- Files: `sender.go`, `cmd/demo/main.go`, `sender_test.go`.
- Implement: a `Sender` combining a fixed worker pool with a shared `rate.Limiter`; each worker calls `limiter.Wait(ctx)` before a send, with a blocking `Send`, a non-blocking `TrySend` using `Allow`, and `Submit`/`Close`.
- Test: a burst of sends is throttled to the configured rate (measured by timestamps), `Send` returns an error and does not send when the context is canceled, and `TrySend` drops when no token is available.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/webhook/cmd/demo
cd ~/go-exercises/webhook
go mod init example.com/webhook
go get golang.org/x/time/rate
```

### Concurrency cap and rate cap are orthogonal

A `rate.Limiter` is a token bucket: it refills at `limit` tokens per second up to a
`burst` capacity, and `Wait(ctx)` blocks until a token is available (or the context
is done). `rate.Every(d)` converts an interval into a `rate.Limit` — `rate.Every(20*time.Millisecond)`
is 50 tokens per second. Because the limiter is *shared* by all workers, it governs
the aggregate: no matter how many workers call `Wait` concurrently, tokens are
handed out at the configured rate, so the combined outbound rate stays under the
cap. The worker pool separately caps how many sends are *in flight* at once. You
need both: the pool protects against too many simultaneous open connections; the
limiter protects the downstream's per-second quota. Confusing the two — assuming a
concurrency limit also limits rate — is how a service with "only 10 workers"
nonetheless trips a 50 req/s quota when each request is fast.

`Send` is the blocking path: `limiter.Wait(ctx)` parks until a token frees, then
calls the send function. If the context is cancelled while waiting (or already
cancelled on entry), `Wait` returns the context error and `Send` never sends —
important so a shutdown does not fire off a webhook whose result no one will read.
`TrySend` is the non-blocking path built on `limiter.Allow()`, which returns
`false` immediately when no token is available; the send is *dropped* rather than
queued, which is what you want for low-value, high-volume events where a late send
is worse than none.

Create `sender.go`:

```go
package webhook

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// SendFunc issues one outbound call.
type SendFunc func(context.Context) error

// Sender is a fixed worker pool whose sends are additionally throttled by a
// shared rate limiter, so the aggregate outbound rate stays under a budget.
type Sender struct {
	ctx     context.Context
	cancel  context.CancelFunc
	limiter *rate.Limiter
	jobs    chan SendFunc
	onErr   func(error)
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

// NewSender starts workers goroutines sharing a limiter of limit tokens/sec with
// the given burst. onErr, if non-nil, receives each send's error.
func NewSender(workers int, limit rate.Limit, burst int, onErr func(error)) *Sender {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sender{
		ctx:     ctx,
		cancel:  cancel,
		limiter: rate.NewLimiter(limit, burst),
		jobs:    make(chan SendFunc, workers*2),
		onErr:   onErr,
	}
	for range workers {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *Sender) worker() {
	defer s.wg.Done()
	for job := range s.jobs {
		if err := s.Send(s.ctx, job); err != nil && s.onErr != nil {
			s.onErr(err)
		}
	}
}

// Send waits for a rate token, then issues the call. It returns the context
// error without sending if ctx is done while waiting.
func (s *Sender) Send(ctx context.Context, fn SendFunc) error {
	if err := s.limiter.Wait(ctx); err != nil {
		return err
	}
	return fn(ctx)
}

// TrySend issues the call only if a token is immediately available; otherwise it
// drops the send and returns (false, nil).
func (s *Sender) TrySend(fn SendFunc) (bool, error) {
	if !s.limiter.Allow() {
		return false, nil
	}
	return true, fn(context.Background())
}

// Submit enqueues fn for a worker, returning false after Close.
func (s *Sender) Submit(fn SendFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.jobs <- fn
	return true
}

// Close drains queued sends (still rate-limited) and then releases resources.
func (s *Sender) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.jobs)
	s.mu.Unlock()
	s.wg.Wait()
	s.cancel()
}
```

### The runnable demo

The demo sends four webhooks through a sender limited to 50/sec and prints how many
succeeded. Because the limiter throttles them, they go out spread over time, but
all four are delivered.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/webhook"
	"golang.org/x/time/rate"
)

func main() {
	s := webhook.NewSender(3, rate.Every(20*time.Millisecond), 1, nil) // 50/sec
	var sent atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		s.Submit(func(_ context.Context) error {
			defer wg.Done()
			sent.Add(1)
			return nil
		})
	}
	wg.Wait()
	s.Close()
	fmt.Printf("sent: %d\n", sent.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sent: 4
```

### Tests

`TestThrottlesRate` sends six jobs through a sender limited to one token per 20ms
(burst 1) across three workers, records each send timestamp, and asserts the span
from first to last is at least roughly `(n-1)` intervals — proof the shared limiter
paced them despite the extra workers. Tolerances are loose to avoid flakiness.
`TestSendCanceledContext` calls `Send` with an already-cancelled context and asserts
it returns `context.Canceled` and never invokes the send function.
`TestTrySendDrops` exhausts the single burst token and asserts the next `TrySend`
returns `false` and does not send.

Create `sender_test.go`:

```go
package webhook

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestThrottlesRate(t *testing.T) {
	t.Parallel()

	const interval = 20 * time.Millisecond
	s := NewSender(3, rate.Every(interval), 1, nil)
	defer s.Close()

	var mu sync.Mutex
	var times []time.Time
	var wg sync.WaitGroup
	const n = 6
	for range n {
		wg.Add(1)
		s.Submit(func(_ context.Context) error {
			defer wg.Done()
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
			return nil
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(times) != n {
		t.Fatalf("recorded %d sends, want %d", len(times), n)
	}
	first, last := times[0], times[0]
	for _, ts := range times {
		if ts.Before(first) {
			first = ts
		}
		if ts.After(last) {
			last = ts
		}
	}
	span := last.Sub(first)
	// With burst 1, n sends need about (n-1) intervals. Allow generous slack.
	if min := time.Duration(n-1) * interval * 7 / 10; span < min {
		t.Fatalf("span = %v for %d sends at %v; want >= %v (not throttled)", span, n, interval, min)
	}
}

func TestSendCanceledContext(t *testing.T) {
	t.Parallel()

	s := NewSender(1, rate.Every(time.Millisecond), 1, nil)
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sent atomic.Bool
	err := s.Send(ctx, func(context.Context) error {
		sent.Store(true)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send err = %v, want context.Canceled", err)
	}
	if sent.Load() {
		t.Fatal("send fired despite canceled context")
	}
}

func TestTrySendDrops(t *testing.T) {
	t.Parallel()

	// Very slow refill, burst 1: first TrySend consumes the token, next drops.
	s := NewSender(1, rate.Every(time.Hour), 1, nil)
	defer s.Close()

	ok, err := s.TrySend(func(context.Context) error { return nil })
	if err != nil || !ok {
		t.Fatalf("first TrySend = %v,%v; want true,nil", ok, err)
	}

	var sent atomic.Bool
	ok, err = s.TrySend(func(context.Context) error {
		sent.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("second TrySend err = %v, want nil", err)
	}
	if ok {
		t.Fatal("second TrySend sent despite no token available")
	}
	if sent.Load() {
		t.Fatal("dropped send should not have run")
	}
}
```

## Review

The sender is correct when concurrency and rate are bounded independently. The
worker count caps in-flight sends; the shared limiter caps the aggregate rate, which
`TestThrottlesRate` proves by showing six sends span at least several intervals even
with three workers — remove the limiter and they would all fire at once. `Send`
honors cancellation (`TestSendCanceledContext`), so a shutdown does not emit a
webhook mid-drain, and `TrySend` drops rather than queues when the bucket is empty
(`TestTrySendDrops`).

The mistakes to avoid: assuming the worker count also limits the rate (it does not —
that is what the limiter is for); giving each worker its *own* limiter instead of
sharing one (then N workers get N times the intended rate); and using `Wait` where
you meant `Allow` (blocking when you wanted to drop, or vice versa). Keep timing
assertions loose — the `7/10` slack in the test tolerates scheduler jitter. Run
`-race` to confirm the shared timestamp slice and the limiter are accessed cleanly.

## Resources

- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — `Limiter`, `NewLimiter`, `Every`, `Wait`, `Allow`.
- [`context`](https://pkg.go.dev/context) — the cancellation `Wait` observes.
- [Go Blog: Rate limiting](https://go.dev/blog/rate-limiting) — token-bucket rate limiting in practice.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-panic-safe-workers.md](07-panic-safe-workers.md) | Next: [09-graceful-shutdown-drain.md](09-graceful-shutdown-drain.md)
