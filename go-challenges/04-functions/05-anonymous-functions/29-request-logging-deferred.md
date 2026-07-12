# Exercise 29: Distributed Request ID Logging via Deferred Closure Capture

**Nivel: Intermedio** — validacion rapida (un test corto).

Logging a request's latency and outcome after the fact usually means
threading a request ID and a start time through every return path by hand,
duplicating the log call at each one. This module builds `Run`, which reads
the request ID out of `ctx` once, starts a clock once, and defers a single
closure over those two locals that logs exactly once no matter how `fn`
exits.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
reqlog/                       module example.com/reqlog
  go.mod
  reqlog.go                    WithRequestID, RequestID, Logger, Run (deferred closure over locals)
  reqlog_test.go                 success/error lines, missing request ID, repeated calls
  cmd/demo/main.go              two requests through a fake stepping clock
```

- Files: `reqlog.go`, `reqlog_test.go`, `cmd/demo/main.go`.
- Implement: `WithRequestID`/`RequestID` over `context.Context`; `Logger` collecting lines in memory; `Run(ctx, logger, now, fn)` deferring a closure over `id` and `start` that logs latency and status.
- Test: a success and a failure each log the expected line; a missing request ID logs an empty one gracefully; repeated `Run` calls each log exactly once.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The deferred closure captures locals set up before it runs

`Run` does two things before it ever calls `fn`: it reads the request ID
out of `ctx` into `id`, and it records `start := now()`. Only then does it
defer a closure — and that closure closes over `id`, `start`, and the named
return `err`, none of which existed yet when `Run` was entered. By the time
the closure actually runs, `fn` has already executed (or panicked, or
returned early), so `err` holds the real final outcome and `now()` called a
second time gives the real end time — `elapsed := now().Sub(start)` is
computed from values the closure captured by reference, not by copying them
at defer time. Passing `now` in as a parameter, rather than calling
`time.Now()` directly, is what makes `elapsed` exactly reproducible in
tests: a fake clock that steps by a fixed amount each call turns latency
into an exact, assertable number instead of "some small positive duration."

Create `reqlog.go`:

```go
package reqlog

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 0

// WithRequestID attaches a request ID to ctx for downstream handlers and
// for Run to pick up when logging.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID extracts the request ID stashed by WithRequestID, or "" if none
// was set.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Logger collects emitted lines in memory so callers (and tests) can
// inspect exactly what was logged without scraping stdout.
type Logger struct {
	mu    sync.Mutex
	lines []string
}

func (l *Logger) log(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

// Lines returns a copy of every line logged so far, in order.
func (l *Logger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// Run executes fn under ctx, timing it with now (an injected clock so tests
// never depend on real elapsed time) and deferring a closure that closes
// over two locals set up before fn runs -- id and start -- to compute
// latency and log it exactly once, on every exit path. Because the closure
// reads the named return err only after fn has run (or after Run's own
// setup failed), it logs the correct final status whether fn succeeds,
// fails, or -- since a deferred closure still runs on a panic -- fn panics
// partway through.
func Run(ctx context.Context, logger *Logger, now func() time.Time, fn func() error) (err error) {
	id := RequestID(ctx)
	start := now()
	defer func() {
		elapsed := now().Sub(start)
		status := "ok"
		if err != nil {
			status = "error: " + err.Error()
		}
		logger.log(fmt.Sprintf("request_id=%s latency=%s status=%s", id, elapsed, status))
	}()

	err = fn()
	return err
}
```

### The runnable demo

The demo runs two requests through a fake clock that advances by a fixed
15ms step on every call, so the printed latencies are exact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/reqlog"
)

func main() {
	logger := &reqlog.Logger{}

	// A fake clock that advances by a fixed step each call, so latency
	// figures are exact and reproducible instead of depending on real time.
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		cur := t
		t = t.Add(15 * time.Millisecond)
		return cur
	}

	ctx := reqlog.WithRequestID(context.Background(), "req-100")
	_ = reqlog.Run(ctx, logger, now, func() error { return nil })

	ctx2 := reqlog.WithRequestID(context.Background(), "req-101")
	_ = reqlog.Run(ctx2, logger, now, func() error { return errors.New("upstream timeout") })

	for _, line := range logger.Lines() {
		fmt.Println(line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request_id=req-100 latency=15ms status=ok
request_id=req-101 latency=15ms status=error: upstream timeout
```

### Tests

`TestRunLogsRequestIDAndLatencyOnSuccess` and `TestRunLogsErrorStatus` each
use a stepping fake clock to assert an exact log line. `TestRunWithoutRequestIDLogsEmptyID`
checks a `ctx` with no request ID logs gracefully with an empty field
instead of panicking. `TestRunLogsOncePerCallAndAccumulates` checks three
separate `Run` calls each add exactly one line, proving the deferred
closure fires once per call, not once total.

Create `reqlog_test.go`:

```go
package reqlog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func stepClock(start time.Time, step time.Duration) func() time.Time {
	t := start
	return func() time.Time {
		cur := t
		t = t.Add(step)
		return cur
	}
}

func TestRunLogsRequestIDAndLatencyOnSuccess(t *testing.T) {
	t.Parallel()
	logger := &Logger{}
	now := stepClock(time.Unix(0, 0), 10*time.Millisecond)
	ctx := WithRequestID(context.Background(), "req-1")

	err := Run(ctx, logger, now, func() error { return nil })
	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}

	lines := logger.Lines()
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	want := "request_id=req-1 latency=10ms status=ok"
	if lines[0] != want {
		t.Fatalf("line = %q, want %q", lines[0], want)
	}
}

func TestRunLogsErrorStatus(t *testing.T) {
	t.Parallel()
	logger := &Logger{}
	now := stepClock(time.Unix(0, 0), 5*time.Millisecond)
	ctx := WithRequestID(context.Background(), "req-2")
	sentinel := errors.New("upstream timeout")

	err := Run(ctx, logger, now, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() err = %v, want %v", err, sentinel)
	}

	lines := logger.Lines()
	want := "request_id=req-2 latency=5ms status=error: upstream timeout"
	if lines[0] != want {
		t.Fatalf("line = %q, want %q", lines[0], want)
	}
}

func TestRunWithoutRequestIDLogsEmptyID(t *testing.T) {
	t.Parallel()
	logger := &Logger{}
	now := stepClock(time.Unix(0, 0), time.Millisecond)

	_ = Run(context.Background(), logger, now, func() error { return nil })

	want := "request_id= latency=1ms status=ok"
	if got := logger.Lines()[0]; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestRunLogsOncePerCallAndAccumulates(t *testing.T) {
	t.Parallel()
	logger := &Logger{}
	now := stepClock(time.Unix(0, 0), time.Millisecond)

	for i := 0; i < 3; i++ {
		_ = Run(context.Background(), logger, now, func() error { return nil })
	}

	if got := len(logger.Lines()); got != 3 {
		t.Fatalf("len(lines) = %d, want 3", got)
	}
}
```

## Review

`Run` is correct when every call it makes logs exactly one line carrying
the right request ID and the right elapsed time, regardless of whether
`fn` succeeded or failed. The injected `now` clock is what turns "latency
logging" from a property you can only eyeball into one you can assert
exactly — a stepping fake clock makes `elapsed` a deterministic number
instead of "whatever a few nanoseconds of real wall-clock happened to be."
The failure mode this pattern guards against is computing `elapsed` or
reading `id` *before* deferring, as local variables passed into the
closure instead of captured by it: that would freeze `start` and `id` at
the moment of the (non-existent, in this design) argument evaluation
rather than letting the closure read the live values, and — more subtly —
it would make it easy to compute `elapsed` too early, before `fn` has even
run.

## Resources

- [context.WithValue](https://pkg.go.dev/context#WithValue)
- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go blog: Go Concurrency Patterns: Context](https://go.dev/blog/context)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-backpressure-errgroup.md](28-backpressure-errgroup.md) | Next: [30-batch-timeout-callback.md](30-batch-timeout-callback.md)
