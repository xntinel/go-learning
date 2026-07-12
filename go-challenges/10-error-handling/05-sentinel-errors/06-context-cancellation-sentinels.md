# Exercise 6: Distinguish Cancellation From Deadline In A Worker

`context.Canceled` and `context.DeadlineExceeded` look similar and mean opposite
things operationally: cancellation is a clean caller-initiated stop (no alert),
a deadline is an SLA breach (count a timeout, maybe page). This exercise builds a
worker that classifies the two with `errors.Is` — never `==` — and updates
metrics accordingly, and shows `context.Cause` recovering the reason attached at
cancel time.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
worker/                       independent module: example.com/worker
  go.mod                      go 1.26
  worker.go                   Metrics; Run classifies ctx errors via errors.Is
  cmd/
    demo/
      main.go                 cancel vs deadline vs cause, prints classification
  worker_test.go              cancel-is-clean, deadline-is-timeout, wrapped-still-classifies
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: `Run(ctx, *Metrics, unit)` that runs `unit` and, via `errors.Is`, counts a cancel as `Cancels` (no timeout) and a deadline as `Timeouts`.
- Test: cancel the context (assert `context.Canceled`, `Timeouts` untouched); let a deadline elapse (assert `context.DeadlineExceeded`, `Timeouts`==1); a wrapped deadline error (assert `errors.Is` still classifies where `==` fails).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/05-sentinel-errors/06-context-cancellation-sentinels/cmd/demo
cd go-solutions/10-error-handling/05-sentinel-errors/06-context-cancellation-sentinels
```

### Two sentinels, two operational meanings

When a unit of work stops early, *why* it stopped decides how the system should
react. If the caller cancelled — the HTTP client disconnected, a parent shut
down, a sibling in an errgroup failed — that is a clean, expected stop: you tear
down and move on, you do not fire an alert. If instead the deadline elapsed, the
work exceeded its time budget: that is a latency/SLA event you count as a
timeout, graph, and possibly alert on. Collapsing the two (treating every
context error as "timeout", or every one as "cancel") corrupts your metrics and
either buries real SLA breaches or pages on ordinary disconnects.

`Run` distinguishes them with `errors.Is`, and the reason it must be `errors.Is`
and not `==` is the whole point of the last test. A downstream library — an HTTP
client, a database driver — routinely wraps `ctx.Err()` in its own error before
returning it. The moment it does, `err == context.DeadlineExceeded` is `false`
even though the deadline is exactly what happened, and a `==`-based classifier
silently misfiles the event. `errors.Is(err, context.DeadlineExceeded)` walks the
chain and still recognizes it.

`context.Cause` adds a second capability. When a context is cancelled via
`context.WithCancelCause(parent)` and `cancel(reason)`, `ctx.Err()` still reports
the generic `context.Canceled`, but `context.Cause(ctx)` returns the specific
`reason` you attached — letting you log *why* without changing what `errors.Is`
classifies.

Create `worker.go`:

```go
package worker

import (
	"context"
	"errors"
	"fmt"
)

// Metrics accumulates worker outcomes. Cancels and Timeouts are deliberately
// separate: a cancel is a clean stop, a timeout is an SLA event.
type Metrics struct {
	Successes int
	Cancels   int
	Timeouts  int
}

// Run executes unit under ctx and classifies the outcome. It uses errors.Is, not
// ==, so classification survives a downstream library wrapping the context error.
func Run(ctx context.Context, m *Metrics, unit func(ctx context.Context) error) error {
	err := unit(ctx)
	switch {
	case err == nil:
		m.Successes++
		return nil
	case errors.Is(err, context.Canceled):
		m.Cancels++
		return fmt.Errorf("unit cancelled: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		m.Timeouts++
		return fmt.Errorf("unit timed out: %w", err)
	default:
		return fmt.Errorf("unit failed: %w", err)
	}
}
```

### The runnable demo

The demo runs three cases: a cancelled context (clean stop, no timeout counted),
an elapsed deadline (timeout counted), and a `WithCancelCause` context showing
`ctx.Err()` versus `context.Cause(ctx)`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/worker"
)

func waitDone(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func main() {
	// Cancellation: caller gave up. Clean stop, no timeout metric.
	var m worker.Metrics
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := worker.Run(ctx, &m, waitDone)
	fmt.Printf("cancel:   canceled=%v timeouts=%d cancels=%d\n",
		errors.Is(err, context.Canceled), m.Timeouts, m.Cancels)

	// Deadline: SLA breach. Counts as a timeout.
	m = worker.Metrics{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel2()
	err = worker.Run(ctx2, &m, waitDone)
	fmt.Printf("deadline: timeout=%v timeouts=%d\n",
		errors.Is(err, context.DeadlineExceeded), m.Timeouts)

	// context.Cause surfaces the reason attached at cancel time.
	reason := errors.New("shutdown requested")
	ctx3, cancel3 := context.WithCancelCause(context.Background())
	cancel3(reason)
	fmt.Printf("cause:    err=%v cause=%v\n", ctx3.Err(), context.Cause(ctx3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cancel:   canceled=true timeouts=0 cancels=1
deadline: timeout=true timeouts=1
cause:    err=context canceled cause=shutdown requested
```

### Tests

`TestCancellationIsClean` cancels the context and asserts the cancel is counted
but no timeout is. `TestDeadlineIsTimeout` lets a short real deadline elapse (the
unit waits on `ctx.Done`, so this is not flaky) and asserts the timeout metric.
`TestClassifiesWrappedError` feeds a wrapped `context.DeadlineExceeded` and shows
`errors.Is` still classifies it while `==` would not.

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func waitDone(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestCancellationIsClean(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var m Metrics
	err := Run(ctx, &m, waitDone)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if m.Timeouts != 0 {
		t.Fatalf("Timeouts = %d, want 0 (a cancel is not a timeout)", m.Timeouts)
	}
	if m.Cancels != 1 {
		t.Fatalf("Cancels = %d, want 1", m.Cancels)
	}
}

func TestDeadlineIsTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	var m Metrics
	err := Run(ctx, &m, waitDone)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if m.Timeouts != 1 {
		t.Fatalf("Timeouts = %d, want 1", m.Timeouts)
	}
	if m.Cancels != 0 {
		t.Fatalf("Cancels = %d, want 0", m.Cancels)
	}
}

func TestClassifiesWrappedError(t *testing.T) {
	t.Parallel()

	unit := func(context.Context) error {
		return fmt.Errorf("downstream rpc: %w", context.DeadlineExceeded)
	}
	var m Metrics
	err := Run(t.Context(), &m, unit)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is must classify a wrapped deadline; got %v", err)
	}
	if m.Timeouts != 1 {
		t.Fatalf("Timeouts = %d, want 1", m.Timeouts)
	}
	// == would fail on the wrapped value; errors.Is is why classification holds.
	if errors.Is(err, context.Canceled) {
		t.Fatal("a deadline must not be classified as a cancel")
	}
}

func ExampleRun_cancel() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var m Metrics
	err := Run(ctx, &m, func(c context.Context) error { return c.Err() })
	fmt.Println(errors.Is(err, context.Canceled), m.Cancels)
	// Output: true 1
}
```

## Review

The worker is correct when a cancel increments `Cancels` and leaves `Timeouts`
at zero, a deadline does the reverse, and both classifications hold even when the
context error arrives wrapped — which is exactly what `TestClassifiesWrappedError`
guarantees. The bug this exercise inoculates against is `if err == ctx.Err()` or
`if err == context.DeadlineExceeded`: correct until the first library that wraps
the value, then silently wrong, mis-filing SLA breaches as clean stops. Always
`errors.Is`; reach for `context.Cause` when you need the specific reason behind a
generic `context.Canceled`.

## Resources

- [`context` variables](https://pkg.go.dev/context#pkg-variables) — `Canceled` and `DeadlineExceeded`.
- [`context.Cause`](https://pkg.go.dev/context#Cause) and [`WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — attaching and recovering a cancellation reason.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — why wrap-aware comparison beats `==` for context errors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-custom-is-method.md](05-custom-is-method.md) | Next: [07-join-validation-pipeline.md](07-join-validation-pipeline.md)
