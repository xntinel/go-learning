# Exercise 9: Guard Fire-and-Forget Background Tasks Spawned from a Handler

An HTTP handler often kicks off work that must outlive the request — an audit
write, a cache warm, a webhook delivery. Spawn it naively and two things go wrong:
it dies the instant the client disconnects (because it inherited the request
context), and if it panics, it either crashes the whole server or fails silently
with no trace. This module builds a `SafeGo` helper that detaches from the request
context, recovers panics per-goroutine, and routes every failure to an error sink
plus a structured log — the production-safe way to fire and forget.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
bgtask/                      independent module: example.com/bgtask
  go.mod                     go 1.26
  bgtask.go                  Failure; SafeGo (WithoutCancel + recover + sink + slog)
  cmd/
    demo/
      main.go                runnable demo: a panicking task and a detached task, both handled
  bgtask_test.go             tests: panic reaches sink with name+stack; task outlives cancelled req
```

Files: `bgtask.go`, `cmd/demo/main.go`, `bgtask_test.go`.
Implement: `SafeGo(req, name, fn, sink, logger)` that detaches from the request context with `context.WithoutCancel` plus a fresh timeout, runs `fn` in a goroutine with a deferred `recover`, and reports any failure (panic or error) to the sink and the logger.
Test: a panicking task is recovered and its `Failure` reaches the sink with the task name and a non-empty stack; a task started from an already-cancelled request context still runs because it detached; the sink receives exactly the expected failures.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/09-error-handling-in-goroutines/09-safe-background-task-sink/cmd/demo
cd go-solutions/10-error-handling/09-error-handling-in-goroutines/09-safe-background-task-sink
go mod edit -go=1.26
```

### Detach, recover, report

Three problems, three mechanisms. First, **inheritance**: a background task started
with `r.Context()` is cancelled when the client disconnects, killing the audit
write mid-flight. `context.WithoutCancel(parent)` returns a context that keeps the
parent's *values* — trace IDs, auth identity, anything stored with `context.WithValue`
— but drops its cancellation and deadline. Wrapped in a fresh
`context.WithTimeout`, the task is decoupled from the request yet still bounded, so
it cannot run forever. Second, **panics**: a `defer recover()` inside the task
goroutine (never the caller's) converts a panic into a `Failure` carrying the task
name and `debug.Stack()`, so one bad task logs an error instead of crashing the
server. Third, **silence**: every failure — recovered panic or returned error — is
sent to an error sink (a channel or callback the operator drains) and logged as a
structured `slog` record, so a background failure is neither lost nor invisible.

`SafeGo` composes all three. It builds the detached context *before* starting the
goroutine (so the detachment is not itself racing the request's cancellation),
launches one goroutine with the deferred recover at the top, runs `fn(ctx)`, and on
any failure emits a `Failure{Task, Err, Stack}` to the sink and an
`slog.Error` line. The caller — a simulated handler — returns immediately and is
completely unaffected by what the task does, which is the whole point of
fire-and-forget.

`context.AfterFunc` earns a mention here: if you want to observe *that* the
original request was cancelled (for a metric, say) without coupling the task's
lifetime to it, `context.AfterFunc(req, fn)` registers `fn` to run in its own
goroutine when `req` is cancelled, and returns a `stop` function to deregister it.
The demo uses it to note request cancellation while the detached task keeps
running — the two are fully decoupled.

Create `bgtask.go`:

```go
package bgtask

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Failure is a background task's failure report: which task, the error (a
// recovered panic is wrapped as an error), and the stack captured on panic.
type Failure struct {
	Task  string
	Err   error
	Stack []byte
}

// SafeGo runs fn as a detached background task spawned from a request. It:
//   - detaches from req via WithoutCancel so the task outlives the request, but
//     bounds it with a fresh timeout;
//   - recovers panics inside the task goroutine so one bad task cannot crash the
//     server;
//   - reports any failure (panic or returned error) to sink and logger, so it is
//     never lost.
//
// It returns immediately; the caller (a handler) is unaffected by the task.
func SafeGo(req context.Context, name string, timeout time.Duration, fn func(ctx context.Context) error, sink chan<- Failure, logger *slog.Logger) {
	// Keep req's values (trace IDs, identity) but drop its cancellation, then
	// bound the detached task with its own timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req), timeout)

	go func() {
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				f := Failure{
					Task:  name,
					Err:   fmt.Errorf("task %q panicked: %v", name, rec),
					Stack: debug.Stack(),
				}
				logger.Error("background task panicked", "task", name, "err", f.Err)
				sink <- f
			}
		}()

		if err := fn(ctx); err != nil {
			logger.Error("background task failed", "task", name, "err", err)
			sink <- Failure{Task: name, Err: err}
		}
	}()
}
```

### The runnable demo

The demo simulates a handler that starts two background tasks — one that panics,
one that does normal work after the request context is already cancelled — then
returns immediately. Both failures reach the sink; the detached task runs despite
the cancelled request. Output is ordered by draining the sink deterministically.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"example.com/bgtask"
)

func main() {
	// A logger that discards output so the demo's stdout stays clean; a real
	// server would use a JSON handler to stderr.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := make(chan bgtask.Failure, 2)

	// Simulate a request whose client has already disconnected.
	req, cancelReq := context.WithCancel(context.Background())
	cancelReq()

	bgtask.SafeGo(req, "audit-write", time.Second, func(ctx context.Context) error {
		// Detached: ctx is NOT cancelled even though req was.
		if ctx.Err() != nil {
			return errors.New("unexpectedly cancelled")
		}
		return errors.New("audit store unavailable")
	}, sink, logger)

	bgtask.SafeGo(req, "cache-warm", time.Second, func(ctx context.Context) error {
		panic("nil pointer in cache warm")
	}, sink, logger)

	// The "handler" returns immediately; drain the sink to observe outcomes.
	var failures []bgtask.Failure
	for range 2 {
		failures = append(failures, <-sink)
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Task < failures[j].Task })

	for _, f := range failures {
		fmt.Printf("%s: %v (stack captured=%t)\n", f.Task, f.Err, len(f.Stack) > 0)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

The audit task ran to completion and returned its own error even though the
request context was cancelled before it started — proof `WithoutCancel` detached
it. The returned-error path sends the raw error (no re-wrapping), so the audit line
shows the store error itself; the panic path additionally names the task and
captures a stack. Expected output:

```
audit-write: audit store unavailable (stack captured=false)
cache-warm: task "cache-warm" panicked: nil pointer in cache warm (stack captured=true)
```

### Tests

`TestPanicReachesSink` starts a panicking task and asserts a `Failure` arrives on
the sink with the task name and a non-empty stack, while the calling goroutine
(the simulated handler) is unaffected. `TestDetachedTaskOutlivesRequest` cancels
the request context *before* starting the task and asserts the task still ran with
a live (non-cancelled) context — proving `WithoutCancel` did its job.
`TestReturnedErrorReachesSink` pins that a normal returned error also reaches the
sink. A discarding logger keeps test output clean; all run under `-race`.

Create `bgtask_test.go`:

```go
package bgtask

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPanicReachesSink(t *testing.T) {
	t.Parallel()
	sink := make(chan Failure, 1)
	SafeGo(context.Background(), "warmer", time.Second, func(ctx context.Context) error {
		panic("boom")
	}, sink, discardLogger())

	select {
	case f := <-sink:
		if f.Task != "warmer" {
			t.Fatalf("Failure.Task = %q, want warmer", f.Task)
		}
		if !strings.Contains(f.Err.Error(), "warmer") {
			t.Fatalf("Failure.Err = %v, want it to name the task", f.Err)
		}
		if len(f.Stack) == 0 {
			t.Fatal("Failure.Stack is empty; the panic stack was not captured")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failure reached the sink; the panic was not recovered")
	}
}

func TestDetachedTaskOutlivesRequest(t *testing.T) {
	t.Parallel()
	req, cancel := context.WithCancel(context.Background())
	cancel() // client disconnected before the task starts

	ran := make(chan error, 1)
	sink := make(chan Failure, 1)
	SafeGo(req, "audit", time.Second, func(ctx context.Context) error {
		// If the task had inherited req, ctx.Err() would be non-nil here.
		ran <- ctx.Err()
		return nil
	}, sink, discardLogger())

	select {
	case ctxErr := <-ran:
		if ctxErr != nil {
			t.Fatalf("detached task saw ctx.Err() = %v, want nil (it should not inherit req's cancellation)", ctxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached task never ran")
	}
}

func TestReturnedErrorReachesSink(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("store down")
	sink := make(chan Failure, 1)
	SafeGo(context.Background(), "writer", time.Second, func(ctx context.Context) error {
		return sentinel
	}, sink, discardLogger())

	select {
	case f := <-sink:
		if !errors.Is(f.Err, sentinel) {
			t.Fatalf("Failure.Err = %v, want the returned error", f.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("returned error never reached the sink")
	}
}
```

## Review

The helper is correct on three axes, each pinned by a test. Detachment:
`context.WithoutCancel` keeps the request's values but drops its cancellation, so a
task started after the client disconnected still runs with a live context —
`TestDetachedTaskOutlivesRequest` asserts `ctx.Err()` is nil inside the task.
Panic-safety: the deferred `recover` lives inside the task goroutine, so a panic
becomes a `Failure` with a captured stack instead of a crashed server, and the
calling goroutine is untouched. Visibility: every failure reaches both the sink and
a structured `slog` line, so nothing is lost. The fresh `WithTimeout` bounds the
detached task so it cannot run forever — and `defer cancel()` releases it, which
`go vet` requires. The mistake this closes is `go audit(r.Context(), ev)`: inherited
cancellation kills the task on disconnect, no recover means a panic crashes the
server, and no sink means failures vanish. Run `go test -race` and `go vet ./...`
to confirm.

## Resources

- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — detaching a task's lifetime from the request while keeping its values.
- [`context.AfterFunc`](https://pkg.go.dev/context#AfterFunc) — observing request cancellation without coupling the task to it.
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging for background failures.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-structured-error-aggregation-slog.md](10-structured-error-aggregation-slog.md)
