# Exercise 6: A Per-Phase Timeout Budget That Yields An Honest Exit Code

A grace period is a single number — Kubernetes' `terminationGracePeriodSeconds` —
but shutdown has several phases, and one stuck phase must not devour the budget
and starve the rest. This module builds `ShutdownAll`: it divides a total budget
across ordered phases, bounds each one independently, records which phases were
force-closed, and returns a result the caller maps to an honest exit code — the
only signal the orchestrator reads about drain quality.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
phasebudget/               module example.com/phasebudget
  go.mod                   go 1.26
  phasebudget.go           Phase, Result, ShutdownAll(phases, total) Result
  cmd/
    demo/
      main.go              three phases, one over budget; print forced + exit code
  phasebudget_test.go      over-budget forced, bounded wall time, exit-code mapping
```

Files: `phasebudget.go`, `cmd/demo/main.go`, `phasebudget_test.go`.
Implement: `ShutdownAll(phases []Phase, total time.Duration) Result` giving each phase `total/len(phases)`, bounding it with `context.WithTimeoutCause`, and recording forced phases; `Result.ExitCode()`.
Test: an over-budget phase is force-closed and reports `context.DeadlineExceeded` via `context.Cause`; wall time stays within ~1.2x the budget; the exit code is non-zero when any phase was forced and zero otherwise.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/phasebudget/cmd/demo
cd ~/go-exercises/phasebudget
go mod init example.com/phasebudget
```

## Why per-phase budgets and a named cause

The invariant is that the *sum* of the per-phase budgets equals the total, so no
matter how badly one phase misbehaves, the phases after it still get their slice.
`ShutdownAll` computes `perPhase = total / len(phases)` and runs each phase under
its own `context.WithTimeoutCause`. Running the phase's drain in a goroutine and
`select`ing its completion against `ctx.Done()` is what makes the bound real even
if the drain function ignores its context: when the phase's deadline fires,
`ShutdownAll` records the phase as *forced*, attaches the cause, and moves to the
next phase immediately. A drain that respects its context stops sooner; a drain
that does not is cut off at its budget. Either way the next phase starts on time.

`context.WithTimeoutCause(parent, d, cause)` is the key API. When the deadline
fires, `ctx.Err()` is the generic `context.DeadlineExceeded`, but
`context.Cause(ctx)` returns the *named* cause you supplied — here a message that
says which phase blew which budget. That is the difference between a shutdown log
that reads "context deadline exceeded" (useless at 3am) and one that reads
`phase "worker-drain" exceeded its 10s budget` (immediately actionable). The named
cause still wraps `context.DeadlineExceeded`, so `errors.Is(cause,
context.DeadlineExceeded)` remains true for programmatic checks.

`Result` carries the list of forced phases and the joined error, and maps to an
exit code: zero when every phase drained within budget and nothing errored,
non-zero otherwise. The caller does `os.Exit(result.ExitCode())`. Always exiting
zero would hide a truncated drain from every dashboard that watches exit status;
the whole point of the budget machinery is to make "some cleanup was skipped"
visible downstream.

Create `phasebudget.go`:

```go
package phasebudget

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Phase is one ordered step of shutdown: stop ingress, drain workers, close the
// pool. Drain should honor its context, but ShutdownAll bounds it either way.
type Phase struct {
	Name  string
	Drain func(ctx context.Context) error
}

// Result reports the outcome of a full shutdown.
type Result struct {
	Forced []string // phases cut off at their budget, in order
	Err    error    // errors.Join of every phase failure (nil if fully clean)
}

// ExitCode maps the result to a process exit status: 0 when every phase drained
// within budget and none errored, 1 otherwise. This is the only signal the
// orchestrator reads about drain quality.
func (r Result) ExitCode() int {
	if r.Err == nil {
		return 0
	}
	return 1
}

// ShutdownAll runs each phase in order, giving each an equal slice of the total
// budget. A phase that exceeds its slice is force-closed (recorded in Forced)
// and shutdown proceeds to the next phase, so one stuck phase cannot starve the
// rest. It returns a Result whose ExitCode the caller passes to os.Exit.
func ShutdownAll(phases []Phase, total time.Duration) Result {
	var res Result
	if len(phases) == 0 {
		return res
	}
	perPhase := total / time.Duration(len(phases))

	var errs []error
	for _, p := range phases {
		cause := fmt.Errorf("phase %q exceeded its %v budget: %w", p.Name, perPhase, context.DeadlineExceeded)
		ctx, cancel := context.WithTimeoutCause(context.Background(), perPhase, cause)

		done := make(chan error, 1)
		go func() { done <- p.Drain(ctx) }()

		select {
		case err := <-done:
			if err != nil {
				errs = append(errs, fmt.Errorf("phase %q: %w", p.Name, err))
			}
		case <-ctx.Done():
			res.Forced = append(res.Forced, p.Name)
			errs = append(errs, context.Cause(ctx))
		}
		cancel()
	}

	res.Err = errors.Join(errs...)
	return res
}
```

## The runnable demo

The demo runs three phases with a 150ms total budget (50ms each): two drain
quickly, one ignores its context and blocks, so it is force-closed at its slice.
The demo prints the forced phases and the resulting exit code.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/phasebudget"
)

func main() {
	phases := []phasebudget.Phase{
		{Name: "http-drain", Drain: func(ctx context.Context) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		}},
		{Name: "worker-drain", Drain: func(ctx context.Context) error {
			// Ignores ctx and blocks; will be force-closed at its budget.
			time.Sleep(2 * time.Second)
			return nil
		}},
		{Name: "pool-close", Drain: func(ctx context.Context) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		}},
	}

	res := phasebudget.ShutdownAll(phases, 150*time.Millisecond)
	fmt.Println("forced phases:", res.Forced)
	fmt.Println("exit code:", res.ExitCode())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
forced phases: [worker-drain]
exit code: 1
```

## Tests

`TestOverBudgetPhaseIsForced` runs three phases with one that ignores its context;
it asserts that phase is in `Forced`, that its recorded cause satisfies
`errors.Is(err, context.DeadlineExceeded)`, and that the total wall time stays
within ~1.2x the budget (proving phases do not serially overrun).
`TestAllCleanExitsZero` runs only fast phases and asserts an empty `Forced`, a
`nil` error, and exit code 0. `TestPhaseErrorExitsNonZero` returns a real
non-deadline error from a phase and asserts a non-zero exit code without a forced
phase.

Create `phasebudget_test.go`:

```go
package phasebudget

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestOverBudgetPhaseIsForced(t *testing.T) {
	t.Parallel()

	const total = 120 * time.Millisecond // 40ms per phase
	phases := []Phase{
		{Name: "fast-a", Drain: func(ctx context.Context) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		}},
		{Name: "stuck", Drain: func(ctx context.Context) error {
			time.Sleep(2 * time.Second) // ignores ctx; must be force-closed
			return nil
		}},
		{Name: "fast-b", Drain: func(ctx context.Context) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		}},
	}

	start := time.Now()
	res := ShutdownAll(phases, total)
	elapsed := time.Since(start)

	if !slices.Contains(res.Forced, "stuck") {
		t.Fatalf("Forced = %v, want it to contain \"stuck\"", res.Forced)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want it to wrap context.DeadlineExceeded", res.Err)
	}
	if max := total * 12 / 10; elapsed > max {
		t.Fatalf("wall time %v exceeded 1.2x budget %v; a stuck phase overran", elapsed, max)
	}
	if res.ExitCode() == 0 {
		t.Fatal("ExitCode = 0, want non-zero when a phase was forced")
	}
}

func TestAllCleanExitsZero(t *testing.T) {
	t.Parallel()

	phases := []Phase{
		{Name: "a", Drain: func(ctx context.Context) error { return nil }},
		{Name: "b", Drain: func(ctx context.Context) error { return nil }},
	}

	res := ShutdownAll(phases, 100*time.Millisecond)
	if len(res.Forced) != 0 {
		t.Fatalf("Forced = %v, want empty", res.Forced)
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if res.ExitCode() != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode())
	}
}

func TestPhaseErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	errClose := errors.New("pool close: connection reset")
	phases := []Phase{
		{Name: "a", Drain: func(ctx context.Context) error { return nil }},
		{Name: "pool", Drain: func(ctx context.Context) error { return errClose }},
	}

	res := ShutdownAll(phases, 100*time.Millisecond)
	if len(res.Forced) != 0 {
		t.Fatalf("Forced = %v, want empty (the error was not a timeout)", res.Forced)
	}
	if !errors.Is(res.Err, errClose) {
		t.Fatalf("Err = %v, want it to wrap errClose", res.Err)
	}
	if res.ExitCode() == 0 {
		t.Fatal("ExitCode = 0, want non-zero when a phase errored")
	}
}

func ExampleResult_ExitCode() {
	phases := []Phase{
		{Name: "a", Drain: func(ctx context.Context) error { return nil }},
	}
	res := ShutdownAll(phases, 50*time.Millisecond)
	println(res.ExitCode())
	// Output:
}
```

## Review

`ShutdownAll` is correct when the budget is genuinely partitioned and the exit
code is honest. Partitioning: `TestOverBudgetPhaseIsForced` proves a phase that
ignores its context is cut off at its slice and the whole shutdown still finishes
within ~1.2x the budget, so later phases are not starved. Honesty: the exit code
is non-zero whenever any phase was forced (`TestOverBudgetPhaseIsForced`) or
errored (`TestPhaseErrorExitsNonZero`), and zero only when everything drained
clean (`TestAllCleanExitsZero`). `context.WithTimeoutCause` plus `context.Cause`
gives the operator a named reason instead of a bare deadline error. The mistakes
to avoid: sharing one deadline across all phases (a stuck phase then starves the
rest), and always exiting zero (dashboards can no longer see a truncated drain).
Run `go test -race`; each phase's drain runs in its own goroutine.

## Resources

- [context.WithTimeoutCause](https://pkg.go.dev/context#WithTimeoutCause) — a bounded context whose expiry carries a named cause.
- [context.Cause](https://pkg.go.dev/context#Cause) — retrieving the specific reason a context was cancelled.
- [Kubernetes: Pod termination and terminationGracePeriodSeconds](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) — the total budget the phases are carved from.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-errgroup-supervised-lifecycle.md](05-errgroup-supervised-lifecycle.md) | Next: [07-inflight-request-tracking-drain.md](07-inflight-request-tracking-drain.md)
