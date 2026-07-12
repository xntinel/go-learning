# Exercise 10: Termination Reason As A First-Class Signal — Cause-Based Logging And Metrics

The whole point of cause-carrying cancellation is that *why* a pipeline stopped is
actionable data. An SRE wants to alert on stage-error terminations and ignore
routine user cancels; a dashboard wants a completion-vs-cancel-vs-timeout-vs-error
breakdown. `ctx.Err()` cannot provide this — it collapses everything to `Canceled`
or `DeadlineExceeded`. This capstone builds a thin wrapper that runs a pipeline,
classifies the termination into a typed `Outcome` by inspecting `context.Cause`,
then emits a structured `slog` line and increments a labelled counter.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
outcome/                     module example.com/outcome
  go.mod
  outcome.go                 type Outcome; Classify(ctx); Run(ctx, ...); Metrics counter
  cmd/
    demo/
      main.go                four terminations, prints each classified outcome + counts
  outcome_test.go            completed, user-cancel, deadline (WithTimeoutCause), stage-error
```

Files: `outcome.go`, `cmd/demo/main.go`, `outcome_test.go`.
Implement: `Outcome` enum, `Classify(ctx) Outcome` over `context.Cause` /
`context.Canceled` / `context.DeadlineExceeded`, and `Run` that executes a pipeline
func, classifies the result, logs it via `slog`, and bumps a `Metrics` counter.
Test: four scenarios (clean completion, user cancel, deadline via
`WithTimeoutCause`, stage error via `CancelCause`) each assert the classified
outcome, the counter label, and the `slog` reason attribute.
Verify: `go test -count=1 -race ./...`

### Classifying with Cause, not Err

`Classify` inspects the pipeline's context after it stops. The order of checks
matters, because a cancelled context always has *some* cause:

1. If `ctx.Err()` is `nil`, the pipeline was never cancelled — it ran to completion.
   Outcome: `Completed`.
2. Otherwise read `cause := context.Cause(ctx)`. When a deadline fired,
   `WithTimeoutCause` sets the cause to the sentinel you supplied (or, for a plain
   `WithTimeout`, to `context.DeadlineExceeded`). So `errors.Is(cause,
   ErrDeadline)` — where `ErrDeadline` is the sentinel the wrapper installs — marks
   a `DeadlineExceeded` outcome.
3. If the cause `errors.Is` a domain stage error (`ErrStage` here), it is a
   `StageError` — the outcome SREs alert on.
4. Otherwise the cause is `context.Canceled` (a plain `cancel()` with no specific
   cause, i.e. a user hang-up). Outcome: `UserCancelled`.

The reason the wrapper *installs* its own deadline sentinel is that it controls the
context construction: `Run` builds the pipeline context with
`context.WithTimeoutCause(parent, timeout, ErrDeadline)` when a timeout is
requested, so a fired deadline carries `ErrDeadline` and `Classify` can distinguish
it from a stage error even though `ctx.Err()` would say `DeadlineExceeded` for one
and `Canceled` for the other.

`Metrics` is a tiny labelled counter (a map guarded by a mutex — the stand-in for
an `expvar.Map` or a Prometheus `CounterVec`). `Run` increments the counter under
the outcome's label and emits a `slog` record carrying the outcome and, for a
failure, the underlying cause — so the log line and the metric always agree on why
the pipeline ended.

Create `outcome.go`:

```go
package outcome

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// Sentinels the wrapper classifies against.
var (
	// ErrDeadline is installed as the cause of the pipeline's deadline so a fired
	// timeout is distinguishable from other cancellations.
	ErrDeadline = errors.New("outcome: pipeline deadline exceeded")
	// ErrStage marks a stage failure; a real pipeline wraps its domain error with it.
	ErrStage = errors.New("outcome: stage error")
)

// Outcome is the classified reason a pipeline stopped.
type Outcome string

const (
	Completed     Outcome = "completed"
	UserCancelled Outcome = "user_cancelled"
	DeadlineHit   Outcome = "deadline_exceeded"
	StageError    Outcome = "stage_error"
)

// Classify inspects a stopped pipeline's context and returns the termination reason.
func Classify(ctx context.Context) Outcome {
	if ctx.Err() == nil {
		return Completed
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, ErrDeadline), errors.Is(cause, context.DeadlineExceeded):
		return DeadlineHit
	case errors.Is(cause, ErrStage):
		return StageError
	default:
		return UserCancelled
	}
}

// Metrics is a labelled counter keyed by Outcome (stand-in for a real CounterVec).
type Metrics struct {
	mu     sync.Mutex
	counts map[Outcome]int
}

func NewMetrics() *Metrics { return &Metrics{counts: map[Outcome]int{}} }

func (m *Metrics) inc(o Outcome) {
	m.mu.Lock()
	m.counts[o]++
	m.mu.Unlock()
}

// Count returns the number of terminations recorded under o.
func (m *Metrics) Count(o Outcome) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[o]
}

// Run executes pipeline with a context bounded by timeout (0 = no deadline). It
// classifies how the pipeline ended, increments the metric under that label, logs
// a structured record, and returns the Outcome. pipeline receives the context and
// the cancel-cause func so it can cancel with a typed reason (e.g. ErrStage).
func Run(
	parent context.Context,
	timeout timeoutSpec,
	logger *slog.Logger,
	m *Metrics,
	pipeline func(ctx context.Context, cancel context.CancelCauseFunc),
) Outcome {
	var ctx context.Context
	var cancel context.CancelCauseFunc
	if timeout.set {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeoutCause(parent, timeout.d, ErrDeadline)
		defer stop()
		// Wrap so the pipeline can also cancel with a typed cause.
		ctx, cancel = context.WithCancelCause(ctx)
	} else {
		ctx, cancel = context.WithCancelCause(parent)
	}
	defer cancel(nil)

	pipeline(ctx, cancel)

	o := Classify(ctx)
	m.inc(o)
	logger.Info("pipeline terminated",
		slog.String("outcome", string(o)),
		slog.Any("cause", context.Cause(ctx)),
	)
	return o
}
```

`timeoutSpec` is a tiny helper so `Run` can express "no timeout" without a magic
zero `time.Duration`. Its constructors, `NoTimeout()` and `After(d)`, read at the
call site as the two states they represent. It lives in its own file so `outcome.go`
does not need to import `time`.

Create `outcome_spec.go`:

```go
package outcome

import "time"

// timeoutSpec expresses an optional deadline for Run: set=false means no deadline.
type timeoutSpec struct {
	d   time.Duration
	set bool
}

// NoTimeout runs a pipeline with no deadline.
func NoTimeout() timeoutSpec { return timeoutSpec{} }

// After runs a pipeline with the given deadline.
func After(d time.Duration) timeoutSpec { return timeoutSpec{d: d, set: true} }
```

### The runnable demo

The demo runs four pipelines — one that completes, one cancelled by the user, one
that hits a deadline, one that fails a stage — and prints each classified outcome
plus the final counter breakdown. It logs to a discarding handler so the demo
output is just the outcomes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"example.com/outcome"
)

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := outcome.NewMetrics()
	ctx := context.Background()

	o1 := outcome.Run(ctx, outcome.NoTimeout(), logger, m,
		func(ctx context.Context, cancel context.CancelCauseFunc) {
			// completes: does nothing, never cancels
		})

	o2 := outcome.Run(ctx, outcome.NoTimeout(), logger, m,
		func(ctx context.Context, cancel context.CancelCauseFunc) {
			cancel(nil) // user hang-up: plain cancel, no cause
		})

	o3 := outcome.Run(ctx, outcome.After(10*time.Millisecond), logger, m,
		func(ctx context.Context, cancel context.CancelCauseFunc) {
			<-ctx.Done() // wait for the deadline to fire
		})

	o4 := outcome.Run(ctx, outcome.NoTimeout(), logger, m,
		func(ctx context.Context, cancel context.CancelCauseFunc) {
			cancel(fmt.Errorf("%w: enrich failed", outcome.ErrStage))
		})

	fmt.Printf("outcomes: %s %s %s %s\n", o1, o2, o3, o4)
	fmt.Printf("stage_error_count=%d user_cancelled_count=%d\n",
		m.Count(outcome.StageError), m.Count(outcome.UserCancelled))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
outcomes: completed user_cancelled deadline_exceeded stage_error
stage_error_count=1 user_cancelled_count=1
```

### Tests

Each test drives one termination and asserts the classified `Outcome`, the counter
label, and — for the failure cases — that the `slog` record carries the reason. The
`slog` assertion uses a custom handler that captures records into a slice, so the
test can read the emitted `outcome` and `cause` attributes without parsing text.

Create `outcome_test.go`:

```go
package outcome

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// capturing is a slog.Handler that records every attribute of every log call.
type capturing struct {
	records []map[string]any
}

func (c *capturing) Enabled(context.Context, slog.Level) bool { return true }
func (c *capturing) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capturing) WithGroup(string) slog.Handler            { return c }
func (c *capturing) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	c.records = append(c.records, m)
	return nil
}

func TestCompleted(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	o := Run(context.Background(), NoTimeout(), slog.New(&capturing{}), m,
		func(ctx context.Context, cancel context.CancelCauseFunc) {})
	if o != Completed {
		t.Fatalf("outcome = %q, want %q", o, Completed)
	}
	if m.Count(Completed) != 1 {
		t.Fatalf("Completed count = %d, want 1", m.Count(Completed))
	}
}

func TestUserCancelled(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	o := Run(context.Background(), NoTimeout(), slog.New(&capturing{}), m,
		func(ctx context.Context, cancel context.CancelCauseFunc) { cancel(nil) })
	if o != UserCancelled {
		t.Fatalf("outcome = %q, want %q", o, UserCancelled)
	}
	if m.Count(UserCancelled) != 1 {
		t.Fatalf("UserCancelled count = %d, want 1", m.Count(UserCancelled))
	}
}

func TestDeadlineExceeded(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	cap := &capturing{}
	o := Run(context.Background(), After(10*time.Millisecond), slog.New(cap), m,
		func(ctx context.Context, cancel context.CancelCauseFunc) { <-ctx.Done() })
	if o != DeadlineHit {
		t.Fatalf("outcome = %q, want %q", o, DeadlineHit)
	}
	if m.Count(DeadlineHit) != 1 {
		t.Fatalf("DeadlineHit count = %d, want 1", m.Count(DeadlineHit))
	}
	if got := cap.records[0]["outcome"]; got != string(DeadlineHit) {
		t.Fatalf("logged outcome = %v, want %q", got, DeadlineHit)
	}
}

func TestStageError(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	cap := &capturing{}
	o := Run(context.Background(), NoTimeout(), slog.New(cap), m,
		func(ctx context.Context, cancel context.CancelCauseFunc) {
			cancel(fmt.Errorf("%w: enrich failed", ErrStage))
		})
	if o != StageError {
		t.Fatalf("outcome = %q, want %q", o, StageError)
	}
	if m.Count(StageError) != 1 {
		t.Fatalf("StageError count = %d, want 1", m.Count(StageError))
	}
	cause, ok := cap.records[0]["cause"].(error)
	if !ok || !errors.Is(cause, ErrStage) {
		t.Fatalf("logged cause = %v, want errors.Is(ErrStage)", cap.records[0]["cause"])
	}
}
```

## Review

The wrapper is correct when each of the four terminations classifies to the right
`Outcome`, the counter increments under the matching label, and the `slog` record
carries the outcome and cause so logs and metrics never disagree. The design that
makes it work is installing `ErrDeadline` as the deadline's cause via
`WithTimeoutCause`, so `Classify` can tell a timeout from a stage error even though
`ctx.Err()` alone cannot — a user cancel and a stage error both read as
`context.Canceled`. That is the payoff of cause-carrying cancellation threaded
through the whole chapter: the reason a pipeline stopped is data an SRE can alert
on, not a string in a log. Run `go test -race`.

## Resources

- [`context.Cause`](https://pkg.go.dev/context#Cause) — the typed reason behind a cancellation, the basis for classification.
- [`context.WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) — installing a typed cause on a deadline.
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging and custom `Handler` for capturing records in tests.

---

Back to [09-graceful-drain-on-shutdown.md](09-graceful-drain-on-shutdown.md) | Next: [../13-context-leak-detection/00-concepts.md](../13-context-leak-detection/00-concepts.md)
