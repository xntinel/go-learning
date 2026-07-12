# Exercise 6: Propagate WHY a Fan-Out Was Aborted with context.WithCancelCause

When a fan-out stops, the caller's next decision depends entirely on *why*. A
first-failure means "one upstream is down, maybe retry that one". A deadline means
"we ran out of time, back off". A shutdown means "give up cleanly". Plain
`context.Canceled` collapses all three into one opaque value and forces the caller
to guess. `context.WithCancelCause` and `context.WithTimeoutCause` carry the
reason, and a single `context.Cause(ctx)` call recovers it. This module builds a
coordinator that distinguishes deadline from first-failure through one context.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
cancelcause/                 independent module: example.com/cancelcause
  go.mod                     go 1.26
  cancelcause.go             Job; ErrBatchTimeout; Coordinate (WithTimeoutCause + WithCancelCause)
  cmd/
    demo/
      main.go                runnable demo: first-failure cause vs deadline cause
  cancelcause_test.go        tests: Cause Is the failure, Cause Is the timeout, classification
```

Files: `cancelcause.go`, `cmd/demo/main.go`, `cancelcause_test.go`.
Implement: `Coordinate(ctx, timeout, jobs)` that layers `context.WithTimeoutCause` (deadline cause) under `context.WithCancelCause` (first-failure cause); a failing job calls `cancel(fmt.Errorf(...))`, and the result is `context.Cause(ctx)`.
Test: first-failure path — `context.Cause` `Is` the injected cause (not just `context.Canceled`); deadline path — `Cause` `Is` `ErrBatchTimeout`; a worker reads the cause to classify.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/09-error-handling-in-goroutines/06-cancel-cause-fanout/cmd/demo
cd go-solutions/10-error-handling/09-error-handling-in-goroutines/06-cancel-cause-fanout
go mod edit -go=1.26
```

### One context, two reasons

`context.WithCancelCause(parent)` returns a context and a `CancelCauseFunc` — a
`cancel(err error)` you call with an error explaining *why*. After cancellation,
`context.Cause(ctx)` returns that error instead of the generic `context.Canceled`
that `ctx.Err()` still returns. `context.WithTimeoutCause(parent, d, cause)` does
the same for a deadline: when the timeout fires, `context.Cause(ctx)` returns
`cause`. The crucial composition fact is that **cause propagates from parent to
child**: if you derive a cancel-cause context from a timeout-cause context, then
whichever fires *first* sets the cause, and `context.Cause` on the child reports
it. That is how one `Cause` call answers "deadline or first-failure?".

`Coordinate` layers exactly this. It wraps the caller's context in
`WithTimeoutCause(parent, timeout, ErrBatchTimeout)`, then wraps *that* in
`WithCancelCause`. Each job runs with the innermost context. A job that fails calls
`cancel(fmt.Errorf("job %q: %w", name, err))` — wrapping with `%w` so `errors.Is`
still finds the original error through the cause. After `wg.Wait`, the coordinator
reads `context.Cause(ctx)`:

- If a job failed first, the cancel-cause fired first and `Cause` is the wrapped
  job error — `errors.Is(cause, errBoom)` holds.
- If the timeout fired first, the parent timeout set the cause and `Cause` is
  `ErrBatchTimeout` — `errors.Is(cause, ErrBatchTimeout)` holds.
- If everything succeeded, nothing cancelled the context during the run, so `Cause`
  is nil and `Coordinate` returns nil.

Both `cancel` functions must be deferred so `go vet`'s lostcancel check is
satisfied and the context's resources are released; the deferred `cancel(nil)` runs
*after* the return value is computed, and since the first cancellation wins, a real
failure cause is never overwritten by the cleanup. Reading `context.Cause` inside a
worker (as the demo's slow job does) lets each worker log the specific reason it was
aborted — first-failure or timeout — instead of a bare "cancelled".

Create `cancelcause.go`:

```go
package cancelcause

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrBatchTimeout is the cause reported when the batch deadline fires before any
// job fails.
var ErrBatchTimeout = errors.New("batch deadline exceeded")

// Job is a named unit of work that must honor ctx.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Coordinate runs jobs concurrently with fail-fast semantics and a batch
// deadline, and returns the CAUSE of any abort: the wrapped first-failure error,
// ErrBatchTimeout on deadline, or nil if all jobs succeeded. Callers classify the
// result with errors.Is.
func Coordinate(parent context.Context, timeout time.Duration, jobs []Job) error {
	// Outer layer: the deadline, tagged with a distinguishable cause.
	tctx, cancelTimeout := context.WithTimeoutCause(parent, timeout, ErrBatchTimeout)
	defer cancelTimeout()

	// Inner layer: first-failure cancellation with a job-specific cause.
	ctx, cancel := context.WithCancelCause(tctx)
	defer cancel(nil)

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(func() {
			if err := j.Run(ctx); err != nil {
				// First cancel wins; later ones (including the deferred cleanup)
				// are no-ops, so the earliest real reason is preserved.
				cancel(fmt.Errorf("job %q: %w", j.Name, err))
			}
		})
	}
	wg.Wait()

	// Cause is nil unless the deadline or a job cancelled the context.
	return context.Cause(ctx)
}
```

### The runnable demo

The demo runs `Coordinate` twice. First, a job fails immediately while a slow
sibling watches the context; the reported cause is the failure, and the sibling
logs that it saw a first-failure cause. Second, all jobs block past a short
timeout; the reported cause is `ErrBatchTimeout`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/cancelcause"
)

func main() {
	failErr := errors.New("replica unreachable")

	// First-failure path.
	cause1 := cancelcause.Coordinate(context.Background(), time.Second, []cancelcause.Job{
		{Name: "replica-a", Run: func(ctx context.Context) error { return failErr }},
		{Name: "replica-b", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return context.Cause(ctx)
		}},
	})
	fmt.Printf("first-failure cause: %v\n", cause1)
	fmt.Printf("is a job failure (not timeout): %t\n", errors.Is(cause1, failErr))

	// Deadline path: every job blocks past the timeout.
	cause2 := cancelcause.Coordinate(context.Background(), 20*time.Millisecond, []cancelcause.Job{
		{Name: "slow-1", Run: func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }},
		{Name: "slow-2", Run: func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }},
	})
	fmt.Printf("deadline cause: %v\n", cause2)
	fmt.Printf("is the batch timeout: %t\n", errors.Is(cause2, cancelcause.ErrBatchTimeout))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first-failure cause: job "replica-a": replica unreachable
is a job failure (not timeout): true
deadline cause: batch deadline exceeded
is the batch timeout: true
```

### Tests

`TestFirstFailureCause` asserts the returned cause `Is` the injected sentinel and
is *not* `ErrBatchTimeout` — proving classification distinguishes a failure from a
deadline. `TestDeadlineCause` runs jobs that only unblock on cancellation with a
short timeout and asserts the cause `Is` `ErrBatchTimeout`. `TestAllSucceedNilCause`
pins that a clean run returns nil. `TestWorkerReadsCause` checks that a worker can
read `context.Cause` to learn the specific reason it was aborted. All run under
`-race`.

Create `cancelcause_test.go`:

```go
package cancelcause

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func TestFirstFailureCause(t *testing.T) {
	t.Parallel()
	cause := Coordinate(context.Background(), time.Second, []Job{
		{Name: "bad", Run: func(ctx context.Context) error { return errBoom }},
		{Name: "waiter", Run: func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }},
	})
	if !errors.Is(cause, errBoom) {
		t.Fatalf("cause = %v, want errors.Is(..., errBoom)", cause)
	}
	if errors.Is(cause, ErrBatchTimeout) {
		t.Fatalf("cause = %v, must not be classified as a timeout", cause)
	}
}

func TestDeadlineCause(t *testing.T) {
	t.Parallel()
	cause := Coordinate(context.Background(), 20*time.Millisecond, []Job{
		{Name: "slow-1", Run: func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }},
		{Name: "slow-2", Run: func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }},
	})
	if !errors.Is(cause, ErrBatchTimeout) {
		t.Fatalf("cause = %v, want ErrBatchTimeout", cause)
	}
	if errors.Is(cause, errBoom) {
		t.Fatalf("cause = %v, must not be classified as a job failure", cause)
	}
}

func TestAllSucceedNilCause(t *testing.T) {
	t.Parallel()
	cause := Coordinate(context.Background(), time.Second, []Job{
		{Name: "a", Run: func(ctx context.Context) error { return nil }},
		{Name: "b", Run: func(ctx context.Context) error { return nil }},
	})
	if cause != nil {
		t.Fatalf("cause = %v, want nil for an all-success run", cause)
	}
}

func TestWorkerReadsCause(t *testing.T) {
	t.Parallel()
	seen := make(chan error, 1)
	_ = Coordinate(context.Background(), time.Second, []Job{
		{Name: "bad", Run: func(ctx context.Context) error { return errBoom }},
		{Name: "observer", Run: func(ctx context.Context) error {
			<-ctx.Done()
			seen <- context.Cause(ctx)
			return ctx.Err()
		}},
	})
	select {
	case c := <-seen:
		if !errors.Is(c, errBoom) {
			t.Fatalf("worker saw cause %v, want errBoom", c)
		}
	case <-time.After(time.Second):
		t.Fatal("observer never read the cause")
	}
}
```

## Review

The coordinator is correct when one `context.Cause` call answers "why did we
stop?" with a classifiable error: the wrapped job failure on the first-failure
path, `ErrBatchTimeout` on the deadline path, nil on success — each pinned by a
dedicated test asserting the cause `Is` the right thing and `Is` *not* the other.
The mechanism is the two-layer context: `WithTimeoutCause` under `WithCancelCause`,
where cause propagates from parent to child so whichever fires first wins. Wrapping
the job error with `%w` inside `cancel(...)` is what keeps `errors.Is` working
through the cause. Both cancel functions must be deferred — `go vet` flags a dropped
cancel, and the deferred cleanup is a safe no-op because the first cancellation
wins. The failure this avoids is returning a bare `context.Canceled` that makes the
caller's retry logic guess. Run `go test -race` and `go vet ./...` to confirm.

## Resources

- [`context.WithCancelCause` and `context.Cause`](https://pkg.go.dev/context#WithCancelCause) — attaching and reading a cancellation reason.
- [`context.WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) — a deadline with a distinguishable cause.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — classifying the cause through `%w` wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-pipeline-error-propagation.md](07-pipeline-error-propagation.md)
