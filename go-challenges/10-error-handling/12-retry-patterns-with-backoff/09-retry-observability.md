# Exercise 9: Observability — Counting Attempts, Recording Which Retry Won, and Structured Logs

A retry that no one can see is a latency and cost problem hiding in plain sight.
"p50 is fine but 30% of calls now retry twice" is a leading indicator that a
dependency is degrading, and it is completely invisible without instrumentation.
This module adds the hooks that make retries observable: an `OnRetry` callback,
attempt/retry/failure counters, and a structured `slog` line per attempt.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
retryobs/                  independent module: example.com/retryobs
  go.mod                   go 1.26
  retryobs.go              Client with OnRetry hook, Metrics counters, slog logging
  cmd/
    demo/
      main.go              runnable demo: prints structured logs + final counters
  retryobs_test.go         tests: log records per attempt; counters; OnRetry cadence
```

Files: `retryobs.go`, `cmd/demo/main.go`, `retryobs_test.go`.
Implement: a `Client.Do` that logs one `slog` record per attempt (attempt number, retryable-ness, delay), increments `Metrics{Attempts, Retries, Failures}` atomically, and invokes `OnRetry(attempt, err, delay)` before each retry.
Test: a `slog.Handler` over a `bytes.Buffer` yields one record per attempt with the expected attributes; counters after retried-then-succeeded are `attempts=3, retries=2, failures=0`; `OnRetry` is called exactly `attempts-1` times with increasing attempt numbers.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/retryobs/cmd/demo
cd ~/go-exercises/retryobs
go mod init example.com/retryobs
go mod edit -go=1.26
```

### What to emit, and why each signal matters

Three complementary signals turn a black-box retry loop into an observable one:

- **Counters** answer "how often". Three suffice: `Attempts` (every call to the op),
  `Retries` (attempts after the first), and `Failures` (calls that exhausted the
  budget). The ratio `Retries / Attempts` is the single number an ops dashboard
  watches — a rising ratio means a dependency is degrading well before it starts
  returning hard errors. Counters must be concurrency-safe; `atomic.Int64` is the
  minimal correct choice (a real service would expose them via `expvar` or a metrics
  library, but the storage is the same monotonic counter).

- **A structured log line per attempt** answers "what happened, exactly". Using
  `slog` with typed attributes (`slog.Int("attempt", n)`, `slog.Bool("retryable",
  ok)`, `slog.Duration("delay", d)`) means the line is machine-queryable: you can
  filter for `retryable=false` to find calls that gave up on a permanent error, or
  aggregate `delay` to see backoff behavior. A `fmt.Sprintf` log line cannot be
  queried this way.

- **An `OnRetry(attempt, err, delay)` callback** is the extension point. It fires
  once *before each retry* — exactly `attempts − 1` times for a call that made
  `attempts` attempts — carrying the attempt index, the error that triggered the
  retry, and the delay about to be waited. This is where a caller wires in a custom
  metric, a trace span event, or a targeted log without the retry client having to
  know about any of them. Its cadence is a contract the tests pin: monotonically
  increasing attempt numbers, one per retry, never on the final success or on a
  permanent failure.

The crucial observability signal that all of this exists to surface is *which
attempt finally won*. A call that succeeds on attempt 3 looks, to a naive caller,
identical to one that succeeds on attempt 1 — same return value, slightly more
latency. The counters and logs make the difference visible, and "succeeded on attempt
3" is the datum an on-call engineer needs when latency creeps up.

`slog` is used with a handler the caller injects, so the client does not dictate the
log destination. In tests we inject a `slog.NewTextHandler` over a `bytes.Buffer` and
assert on the captured output; in production the same client logs to stderr or a JSON
collector by swapping the handler.

Create `retryobs.go`:

```go
package retryobs

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Metrics holds concurrency-safe retry counters.
type Metrics struct {
	Attempts atomic.Int64 // every op invocation
	Retries  atomic.Int64 // attempts after the first
	Failures atomic.Int64 // calls that exhausted the budget
}

// Op is the retryable unit of work.
type Op func(ctx context.Context) error

// Client retries an Op with full observability: per-attempt slog records, atomic
// counters, and an OnRetry hook.
type Client struct {
	MaxAttempts int
	BaseDelay   time.Duration
	Logger      *slog.Logger
	Metrics     *Metrics
	Retryable   func(error) bool
	// OnRetry fires once before each retry (attempts-1 times total).
	OnRetry func(attempt int, err error, delay time.Duration)
}

func (c *Client) retryable(err error) bool {
	if c.Retryable == nil {
		return true
	}
	return c.Retryable(err)
}

// Do runs op up to MaxAttempts times, logging and counting each attempt.
func (c *Client) Do(ctx context.Context, op Op) error {
	var lastErr error
	for attempt := range c.MaxAttempts {
		if c.Metrics != nil {
			c.Metrics.Attempts.Add(1)
			if attempt > 0 {
				c.Metrics.Retries.Add(1)
			}
		}

		err := op(ctx)
		if err == nil {
			c.log(attempt, true, 0, "attempt succeeded")
			return nil
		}
		lastErr = err

		retryable := c.retryable(err)
		last := attempt == c.MaxAttempts-1
		var delay time.Duration
		if retryable && !last {
			delay = c.BaseDelay * time.Duration(1<<attempt)
		}
		c.log(attempt, retryable, delay, "attempt failed")

		if !retryable {
			return err
		}
		if last {
			break
		}
		if c.OnRetry != nil {
			c.OnRetry(attempt, err, delay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	if c.Metrics != nil {
		c.Metrics.Failures.Add(1)
	}
	return lastErr
}

func (c *Client) log(attempt int, retryable bool, delay time.Duration, msg string) {
	if c.Logger == nil {
		return
	}
	c.Logger.Info(msg,
		slog.Int("attempt", attempt),
		slog.Bool("retryable", retryable),
		slog.Duration("delay", delay),
	)
}
```

### The runnable demo

The demo logs to stdout with a text handler and drives an op that fails twice then
succeeds, so you see one log line per attempt and the final counters showing it
succeeded on attempt 3 (index 2).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"example.com/retryobs"
)

func main() {
	// Strip the time attribute so the output is stable for the expected block.
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	metrics := &retryobs.Metrics{}
	client := &retryobs.Client{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		Logger:      slog.New(handler),
		Metrics:     metrics,
		Retryable:   func(error) bool { return true },
	}

	calls := 0
	_ = client.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})

	fmt.Printf("attempts=%d retries=%d failures=%d\n",
		metrics.Attempts.Load(), metrics.Retries.Load(), metrics.Failures.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=INFO msg="attempt failed" attempt=0 retryable=true delay=1ms
level=INFO msg="attempt failed" attempt=1 retryable=true delay=2ms
level=INFO msg="attempt succeeded" attempt=2 retryable=true delay=0s
attempts=3 retries=2 failures=0
```

### Tests

`TestLogsOnePerAttempt` injects a text handler over a `bytes.Buffer` and asserts the
buffer contains one record per attempt with the expected `attempt=` and `retryable=`
attributes. `TestCountersRetriedThenSucceeded` asserts the exact counters
(`attempts=3, retries=2, failures=0`). `TestCountersExhausted` asserts `failures=1`
after the budget is spent. `TestOnRetryCadence` asserts `OnRetry` fires exactly
`attempts − 1` times with strictly increasing attempt numbers.

Create `retryobs_test.go`:

```go
package retryobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newBufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h), &buf
}

func TestLogsOnePerAttempt(t *testing.T) {
	t.Parallel()
	logger, buf := newBufferLogger()
	client := &Client{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		Logger:      logger,
		Retryable:   func(error) bool { return true },
	}
	calls := 0
	_ = client.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})

	lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if lines != 3 {
		t.Fatalf("log lines = %d, want 3 (one per attempt)\n%s", lines, buf.String())
	}
	for _, want := range []string{"attempt=0", "attempt=1", "attempt=2", `msg="attempt succeeded"`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log missing %q\n%s", want, buf.String())
		}
	}
}

func TestCountersRetriedThenSucceeded(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	client := &Client{MaxAttempts: 5, BaseDelay: time.Millisecond, Metrics: m, Retryable: func(error) bool { return true }}
	calls := 0
	err := client.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := m.Attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if got := m.Retries.Load(); got != 2 {
		t.Errorf("retries = %d, want 2", got)
	}
	if got := m.Failures.Load(); got != 0 {
		t.Errorf("failures = %d, want 0", got)
	}
}

func TestCountersExhausted(t *testing.T) {
	t.Parallel()
	m := &Metrics{}
	client := &Client{MaxAttempts: 3, BaseDelay: time.Millisecond, Metrics: m, Retryable: func(error) bool { return true }}
	err := client.Do(context.Background(), func(context.Context) error {
		return errors.New("always")
	})
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if got := m.Attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if got := m.Failures.Load(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
}

func TestOnRetryCadence(t *testing.T) {
	t.Parallel()
	var seen []int
	client := &Client{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		Retryable:   func(error) bool { return true },
		OnRetry: func(attempt int, err error, delay time.Duration) {
			seen = append(seen, attempt)
		},
	}
	calls := 0
	_ = client.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 4 { // 3 failures then success => 4 attempts
			return errors.New("transient")
		}
		return nil
	})
	// 4 attempts => OnRetry fired 3 times, attempts 0,1,2.
	if len(seen) != 3 {
		t.Fatalf("OnRetry fired %d times, want 3 (attempts-1)", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("OnRetry attempt numbers not increasing: %v", seen)
		}
	}
}

func TestOnRetryNotCalledOnPermanent(t *testing.T) {
	t.Parallel()
	var calls int
	client := &Client{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		Retryable:   func(error) bool { return false }, // permanent
		OnRetry:     func(int, error, time.Duration) { calls++ },
	}
	_ = client.Do(context.Background(), func(context.Context) error {
		return errors.New("permanent")
	})
	if calls != 0 {
		t.Fatalf("OnRetry called %d times on permanent error, want 0", calls)
	}
}

func ExampleClient_Do() {
	m := &Metrics{}
	client := &Client{MaxAttempts: 3, BaseDelay: time.Millisecond, Metrics: m, Retryable: func(error) bool { return true }}
	calls := 0
	_ = client.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 2 {
			return errors.New("transient")
		}
		return nil
	})
	fmt.Println(m.Attempts.Load(), m.Retries.Load(), m.Failures.Load())
	// Output: 2 1 0
}
```

## Review

The instrumentation is correct when the counters tell the truth: `attempts` counts
every op invocation, `retries` counts attempts after the first, and `failures`
increments only when the budget is exhausted — so a retried-then-succeeded call reads
`3, 2, 0` and an exhausted one reads `N, N-1, 1`. The log must carry one queryable
record per attempt, and `OnRetry` must fire exactly `attempts − 1` times with
increasing indices and never on a permanent failure. The mistake this design
prevents: swallowing the retry behavior so "succeeded on attempt 3" is invisible and
a degrading dependency goes unnoticed until it hard-fails. Run `go test -race`; the
counters are `atomic.Int64`, so concurrent `Do` calls increment them without a race.

## Resources

- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging with typed attributes.
- [`sync/atomic#Int64`](https://pkg.go.dev/sync/atomic#Int64) — concurrency-safe counters.
- [`expvar`](https://pkg.go.dev/expvar) — exposing counters over HTTP in production.
- [Go blog: Structured Logging with slog](https://go.dev/blog/slog) — the design and idioms of `slog`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-retry-budget-token-bucket.md](08-retry-budget-token-bucket.md) | Next: [10-deterministic-testing-clock.md](10-deterministic-testing-clock.md)
