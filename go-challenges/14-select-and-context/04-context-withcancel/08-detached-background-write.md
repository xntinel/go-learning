# Exercise 8: WithoutCancel — A Best-Effort Audit Write That Outlives the Request

Some side effects must survive the very request that triggered them. An audit
record of a cancelled request, a metric for an aborted operation, a cache warm
kicked off at the end of a handler — if you hand these the request's context,
cancelling the request kills the write, and you lose exactly the record you most
wanted. `context.WithoutCancel` (Go 1.21) detaches from the parent's cancellation
while keeping its values, and wrapping the detached context in its own timeout
keeps the background work bounded.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
audit/                     independent module: example.com/audit
  go.mod                   module example.com/audit
  audit.go                 WithTraceID; TraceID; HandleRequest (WithoutCancel+timeout)
  cmd/
    demo/
      main.go              cancels the request; the detached audit still writes
  audit_test.go            survives-parent-cancel, own-deadline, values-preserved
```

- Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
- Implement: `HandleRequest` that launches an audit write on a `context.WithoutCancel(ctx)` context wrapped in its own `context.WithTimeout`, carrying the request's trace id through.
- Test: the write completes even after the parent is cancelled and sees the trace id; a slow write hits the detached timeout and returns `context.DeadlineExceeded`, not the parent's cancellation; a value on the parent is readable on the detached context.
- Verify: `go test -count=1 -race ./...`

### Why WithoutCancel, then WithTimeout

Fire-and-forget side effects have two conflicting requirements. They must keep the
request's *values* — the trace id, request id, and auth principal that make the
audit record meaningful and correlatable. But they must not inherit the request's
*lifetime*, because the request is often already over (or actively being
cancelled) at the moment the write starts. Hand the write `ctx` directly and a
cancel kills it; hand it a fresh `context.Background()` and you lose the trace id.

`context.WithoutCancel(parent)` threads this needle. The context it returns
delegates `Value` lookups to `parent` — so `TraceID` still resolves — but its
`Done()` never closes from the parent and it reports no deadline from the parent.
It is "the same values, a severed lifetime". That is precisely what a detached
best-effort write wants.

The severed lifetime introduces the second discipline: a context with no deadline
can run forever. A detached write that hangs on a slow disk or a wedged downstream
would leak indefinitely. So you immediately wrap the detached context in its own
`context.WithTimeout`, giving the background work a bound that is *independent* of
the request. The layering is: `parent` (request, may be cancelled) →
`WithoutCancel(parent)` (values kept, cancellation severed) →
`WithTimeout(detached, d)` (own deadline). The write watches this innermost
context, so it stops for its *own* timeout, never for the request's cancel. Note
the layering order matters — `WithoutCancel` must come first; wrapping a timeout
around the parent and then calling `WithoutCancel` would drop the timeout too.

Create `audit.go`:

```go
package audit

import (
	"context"
	"time"
)

type ctxKey int

const traceKey ctxKey = 0

// WithTraceID returns a copy of ctx carrying the trace id.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey, id)
}

// TraceID returns the trace id carried by ctx, or "" if none is set.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey).(string)
	return id
}

// HandleRequest performs the request's main work and then launches a detached
// audit write that must complete even if ctx is cancelled. The write runs on a
// context derived via WithoutCancel(ctx) — immune to the request's cancellation
// but still carrying its values — bounded by its own timeout. The returned
// channel yields the write's result.
func HandleRequest(ctx context.Context, write func(ctx context.Context, traceID string) error, auditTimeout time.Duration) <-chan error {
	// The request's main work would happen here, using ctx directly.

	done := make(chan error, 1)
	detached := context.WithoutCancel(ctx)
	go func() {
		wctx, cancel := context.WithTimeout(detached, auditTimeout)
		defer cancel()
		done <- write(wctx, TraceID(wctx))
	}()
	return done
}
```

### The runnable demo

The demo sets a trace id, launches the request, then cancels the parent
immediately — the detached audit write still runs to completion and still sees the
trace id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/audit"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = audit.WithTraceID(ctx, "req-42")

	observed := make(chan string, 1)
	write := func(wctx context.Context, traceID string) error {
		observed <- traceID
		return nil
	}

	done := audit.HandleRequest(ctx, write, time.Second)
	fmt.Println("main work done for trace", audit.TraceID(ctx))

	cancel() // the request ends; the audit must still complete

	<-done
	fmt.Println("audit write saw trace", <-observed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
main work done for trace req-42
audit write saw trace req-42
```

### Tests

`TestBackgroundWriteSurvivesParentCancel` cancels the parent right after launching
the request and asserts the detached write still completes and observed the trace
id. `TestDetachedHasOwnDeadline` gives a write that blocks a tiny audit timeout and
asserts it returns `context.DeadlineExceeded` — its own deadline, not the parent's
cancel. `TestValuesPreserved` checks the trace id survives the detach.

Create `audit_test.go`:

```go
package audit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundWriteSurvivesParentCancel(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	parent = WithTraceID(parent, "trace-123")

	gotTrace := make(chan string, 1)
	write := func(ctx context.Context, traceID string) error {
		gotTrace <- traceID
		return nil
	}

	done := HandleRequest(parent, write, time.Second)
	cancel() // parent gone before the write finishes

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("detached write err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached write did not complete after parent cancel")
	}
	if got := <-gotTrace; got != "trace-123" {
		t.Fatalf("write saw trace %q, want %q", got, "trace-123")
	}
}

func TestDetachedHasOwnDeadline(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	write := func(ctx context.Context, traceID string) error {
		<-ctx.Done() // block until the detached timeout fires
		return ctx.Err()
	}

	done := HandleRequest(parent, write, 10*time.Millisecond)

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("detached write err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached write did not hit its own deadline")
	}
}

func TestValuesPreserved(t *testing.T) {
	t.Parallel()

	parent := WithTraceID(context.Background(), "trace-abc")
	detached := context.WithoutCancel(parent)

	if got := TraceID(detached); got != "trace-abc" {
		t.Fatalf("TraceID on detached ctx = %q, want %q", got, "trace-abc")
	}
	if detached.Done() != nil {
		t.Fatal("detached context should have a nil Done() (no cancellation)")
	}
}
```

## Review

The detach is correct when the audit write completes despite a cancelled parent,
still reads the trace id, and stops for its *own* deadline rather than the
parent's cancel. The layering order is the crux: `WithoutCancel` first (sever the
lifetime, keep the values), then `WithTimeout` (bound the detached work). Two
mistakes to avoid: detaching with a bare `context.Background()`, which drops the
trace id and every other request value; and using `WithoutCancel` with no timeout,
which lets the background write run unbounded. `TestValuesPreserved` pins the value
delegation and confirms the detached `Done()` is nil (never cancelled by the
parent); `TestDetachedHasOwnDeadline` pins the independent bound. Run
`go test -race`.

## Resources

- [context.WithoutCancel](https://pkg.go.dev/context#WithoutCancel) — detach cancellation while keeping values.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — bounding the detached work.
- [context.WithValue](https://pkg.go.dev/context#WithValue) — request-scoped values like the trace id.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-first-error-parallel-runner.md](09-first-error-parallel-runner.md)
