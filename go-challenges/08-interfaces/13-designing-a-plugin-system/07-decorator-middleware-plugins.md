# Exercise 7: Cross-Cutting Concerns via Plugin Decorators

Because a decorator implements the same `Plugin` interface it wraps, cross-cutting
behavior — logging, metrics, bounded retries — composes without touching plugin
code. This module builds three decorators that wrap any plugin, preserve its
`Name()`, and nest in any order.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
decorators/               independent module: example.com/decorators
  go.mod                  go 1.25
  plugin.go               Plugin interface with context-aware Process
  decorators.go           WithLogging, WithMetrics (atomic counters), WithRetry
  cmd/
    demo/
      main.go             stack all three over a flaky plugin, watch a retry recover
  decorators_test.go      retry-recovers, metrics-count, name-preserved, any-order tests
```

- Files: `plugin.go`, `decorators.go`, `cmd/demo/main.go`, `decorators_test.go`.
- Implement: `WithLogging`, `WithMetrics` (call count + failure count + total latency via `atomic.Int64`), and `WithRetry(p, attempts)` decorators, each satisfying `Plugin` by embedding one and each preserving `Name()`.
- Test: a plugin that fails twice then succeeds is made to succeed by `WithRetry(3)` with the metrics decorator counting three attempts; a plugin that always fails records one failure when metrics wraps the retry; `Name()` is unchanged through the stack; decorators nest in any order.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/decorators/cmd/demo
cd ~/go-exercises/decorators
go mod init example.com/decorators
go mod edit -go=1.25
```

### One interface, uniform behavior

A decorator is a `Plugin` that holds another `Plugin`. It delegates the methods it
does not care about straight through — crucially `Name()`, so the registry still
identifies the wrapped plugin by its real name — and wraps only `Process` to add
its behavior. Because the decorator satisfies the same interface, it is
substitutable everywhere a plugin is expected, and decorators stack:
`WithLogging(WithMetrics(WithRetry(p)))` is itself just a `Plugin`. This is the
same shape as `net/http` middleware, but expressed through a shared interface
instead of a function type.

The order of nesting is a real decision, not a formality, because each decorator
sees a different slice of reality:

- `WithRetry` re-invokes whatever it wraps on failure. If it wraps `WithMetrics`,
  the metrics decorator is called once per attempt, so its counters see every
  retry. If `WithMetrics` wraps `WithRetry`, the metrics decorator is called once
  for the whole retried operation, so it records a single call that either
  succeeds or fails after all internal retries.
- The tests below use both orderings deliberately: `WithRetry(WithMetrics(flaky))`
  to count attempts, and `WithMetrics(WithRetry(always))` to record one failure
  for an operation whose retries all exhausted.

`WithMetrics` uses `atomic.Int64` counters so it is safe to share across concurrent
`Process` calls without a mutex; `time.Since` measures per-call latency. `WithRetry`
loops up to `attempts` times, returning on the first success and the last error
otherwise, and checks `ctx.Err()` between attempts so a cancelled context stops the
retry loop instead of burning all attempts.

Create `plugin.go`:

```go
package decorators

import "context"

// Plugin is the contract every decorator both wraps and satisfies.
type Plugin interface {
	Name() string
	Process(ctx context.Context, input string) (string, error)
}
```

Create `decorators.go`:

```go
package decorators

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// logging wraps a Plugin and writes a line per Process call.
type logging struct {
	Plugin
	out io.Writer
}

// WithLogging returns p wrapped so each Process is logged to out. Name() is
// delegated to p unchanged.
func WithLogging(p Plugin, out io.Writer) Plugin {
	return logging{Plugin: p, out: out}
}

func (l logging) Process(ctx context.Context, input string) (string, error) {
	out, err := l.Plugin.Process(ctx, input)
	if err != nil {
		io.WriteString(l.out, l.Plugin.Name()+" error: "+err.Error()+"\n")
	} else {
		io.WriteString(l.out, l.Plugin.Name()+" ok\n")
	}
	return out, err
}

// Metrics wraps a Plugin and records call count, failure count, and total
// latency. Its counters are atomic and safe for concurrent Process calls.
type Metrics struct {
	Plugin
	calls     atomic.Int64
	failures  atomic.Int64
	latencyNs atomic.Int64
}

// WithMetrics returns p wrapped in a *Metrics whose counters can be read back.
func WithMetrics(p Plugin) *Metrics {
	return &Metrics{Plugin: p}
}

func (m *Metrics) Process(ctx context.Context, input string) (string, error) {
	m.calls.Add(1)
	start := time.Now()
	out, err := m.Plugin.Process(ctx, input)
	m.latencyNs.Add(int64(time.Since(start)))
	if err != nil {
		m.failures.Add(1)
	}
	return out, err
}

// Calls reports the number of Process invocations seen by this decorator.
func (m *Metrics) Calls() int64 { return m.calls.Load() }

// Failures reports the number of Process invocations that returned an error.
func (m *Metrics) Failures() int64 { return m.failures.Load() }

// retry wraps a Plugin and re-invokes Process up to attempts times.
type retry struct {
	Plugin
	attempts int
}

// WithRetry returns p wrapped so Process is retried up to attempts times,
// stopping early on success or on a cancelled context. attempts < 1 is treated
// as 1. Name() is delegated to p unchanged.
func WithRetry(p Plugin, attempts int) Plugin {
	if attempts < 1 {
		attempts = 1
	}
	return retry{Plugin: p, attempts: attempts}
}

func (r retry) Process(ctx context.Context, input string) (string, error) {
	var err error
	for range r.attempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		var out string
		out, err = r.Plugin.Process(ctx, input)
		if err == nil {
			return out, nil
		}
	}
	return "", err
}
```

Note that each decorator embeds `Plugin`, so `Name()` (and any method a decorator
does not override) is promoted from the embedded value automatically — the
registry identity is preserved for free, which is the invariant the tests check.

### The runnable demo

The demo builds a flaky plugin that fails its first two calls then succeeds, wraps
it in the full stack `WithRetry(WithLogging(WithMetrics(flaky), stdout), 3)`, and
runs it. Logging and metrics both sit *inside* the retry loop, so the retry
re-invokes them on each attempt: the logging decorator prints a line per attempt
and the metrics decorator counts all three attempts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"example.com/decorators"
)

// flaky fails its first failFirst Process calls, then succeeds.
type flaky struct {
	seen      int
	failFirst int
}

func (f *flaky) Name() string { return "flaky" }

func (f *flaky) Process(_ context.Context, input string) (string, error) {
	f.seen++
	if f.seen <= f.failFirst {
		return "", errors.New("transient")
	}
	return "ok:" + input, nil
}

func main() {
	m := decorators.WithMetrics(&flaky{failFirst: 2})
	stack := decorators.WithRetry(decorators.WithLogging(m, os.Stdout), 3)

	out, err := stack.Process(context.Background(), "req")
	fmt.Printf("result: out=%q err=%v\n", out, err)
	fmt.Printf("attempts=%d failures=%d name=%q\n", m.Calls(), m.Failures(), stack.Name())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
flaky error: transient
flaky error: transient
flaky ok
result: out="ok:req" err=<nil>
attempts=3 failures=2 name="flaky"
```

The logging decorator prints two failures and a success (the three attempts the
retry made), the result is the recovered value, and the metrics decorator — nested
*inside* the retry — counted three attempts with two failures. `Name()` is still
`"flaky"` through the whole stack.

### Tests

`TestRetryRecoversFlaky` wraps a fail-twice plugin in `WithRetry(WithMetrics(...), 3)`
and asserts success with three counted attempts and two counted failures.
`TestMetricsOutsideRetryRecordsOneFailure` wraps an always-fail plugin as
`WithMetrics(WithRetry(..., 3))` and asserts a single call that fails — the metrics
decorator sees the retried operation as one unit. `TestNamePreservedThroughStack`
asserts `Name()` survives every nesting order.

Create `decorators_test.go`:

```go
package decorators

import (
	"context"
	"errors"
	"io"
	"testing"
)

type flaky struct {
	seen      int
	failFirst int
}

func (f *flaky) Name() string { return "flaky" }

func (f *flaky) Process(_ context.Context, input string) (string, error) {
	f.seen++
	if f.seen <= f.failFirst {
		return "", errors.New("transient")
	}
	return "ok:" + input, nil
}

type always struct{}

func (always) Name() string { return "always" }
func (always) Process(_ context.Context, _ string) (string, error) {
	return "", errors.New("permanent")
}

func TestRetryRecoversFlaky(t *testing.T) {
	t.Parallel()

	m := WithMetrics(&flaky{failFirst: 2})
	p := WithRetry(m, 3) // metrics is INSIDE retry: it sees every attempt

	out, err := p.Process(context.Background(), "x")
	if err != nil {
		t.Fatalf("Process err = %v, want success after retries", err)
	}
	if out != "ok:x" {
		t.Fatalf("Process = %q, want ok:x", out)
	}
	if m.Calls() != 3 {
		t.Fatalf("Calls = %d, want 3 attempts", m.Calls())
	}
	if m.Failures() != 2 {
		t.Fatalf("Failures = %d, want 2", m.Failures())
	}
}

func TestMetricsOutsideRetryRecordsOneFailure(t *testing.T) {
	t.Parallel()

	m := WithMetrics(WithRetry(always{}, 3)) // metrics OUTSIDE retry: one unit

	_, err := m.Process(context.Background(), "x")
	if err == nil {
		t.Fatal("Process err = nil, want failure after exhausting retries")
	}
	if m.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1 (one retried operation)", m.Calls())
	}
	if m.Failures() != 1 {
		t.Fatalf("Failures = %d, want 1", m.Failures())
	}
}

func TestNamePreservedThroughStack(t *testing.T) {
	t.Parallel()

	base := &flaky{}
	stacks := []Plugin{
		WithLogging(WithRetry(WithMetrics(base), 2), io.Discard),
		WithRetry(WithLogging(WithMetrics(base), io.Discard), 2),
		WithMetrics(WithLogging(WithRetry(base, 2), io.Discard)),
	}
	for i, s := range stacks {
		if s.Name() != "flaky" {
			t.Fatalf("stack %d Name() = %q, want flaky", i, s.Name())
		}
	}
}

func TestRetryStopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	base := &countingFail{}
	p := WithRetry(base, 5)
	if _, err := p.Process(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if base.calls != 0 {
		t.Fatalf("plugin called %d times on a cancelled context, want 0", base.calls)
	}
}

type countingFail struct{ calls int }

func (c *countingFail) Name() string { return "counting" }
func (c *countingFail) Process(_ context.Context, _ string) (string, error) {
	c.calls++
	return "", errors.New("fail")
}
```

## Review

Decorators are correct when they are transparent: the stack satisfies `Plugin`,
`Name()` returns the wrapped plugin's real name in every nesting order, and only
`Process` gains behavior. The nesting-order tests are the conceptual payoff —
`WithRetry(WithMetrics(p))` counts every attempt because metrics is inside the
retry loop, while `WithMetrics(WithRetry(p))` counts one operation because metrics
is outside it; both are correct, and which you want depends on whether your
dashboard should show attempts or operations. Embedding the `Plugin` interface in
each decorator is what makes `Name()` preservation automatic; overriding only
`Process` keeps the decorator minimal. The atomic counters keep `WithMetrics`
race-free under concurrent load without a mutex, which the gate's `-race` run
checks.

## Resources

- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — interface embedding that promotes `Name()` through a decorator.
- [sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64) — lock-free counters for the metrics decorator.
- [Go Blog: net/http middleware patterns](https://go.dev/blog/routing-enhancements) — the same wrap-and-delegate shape in the standard library.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-ordered-shutdown-error-aggregation.md](06-ordered-shutdown-error-aggregation.md) | Next: [08-versioned-contract-negotiation.md](08-versioned-contract-negotiation.md)
