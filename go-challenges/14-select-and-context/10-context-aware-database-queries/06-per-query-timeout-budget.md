# Exercise 6: A Per-Call Timeout Deriver So One Slow Query Cannot Eat the Request Budget

A request deadline is a budget to be split across dependencies, not handed whole
to the first query. This exercise builds a `Runner` that derives a per-call
timeout capped at the smaller of a fixed per-call maximum and the parent's
remaining budget, and short-circuits with a fast, honest error when there is not
enough budget left to bother starting.

## What you'll build

```text
querybudget/                 independent module: example.com/querybudget
  go.mod                     go 1.25
  querybudget.go             Runner.Run: derive min(PerCall, remaining); skip if remaining <= MinSlack
  cmd/
    demo/
      main.go                per-call cap wins; near-empty budget skips
  querybudget_test.go        cap-wins, parent-wins, skip-on-exhausted, no-deadline, -race
```

Files: `querybudget.go`, `cmd/demo/main.go`, `querybudget_test.go`.
Implement: `Runner.Run(parent, q)` that derives a child context with `context.WithTimeout` capped at `min(PerCall, time.Until(parent.Deadline()))`, always calls the `CancelFunc`, and returns a wrapped `ErrBudgetExhausted` without running `q` when the remaining budget is `<= MinSlack`.
Test: with a 500ms parent and a 50ms cap, a 500ms query is cancelled in ~50ms (cap wins); with a 30ms parent the derived deadline is `<= 30ms` (parent wins); a near-empty budget skips the query entirely; a deadline-free parent uses the full per-call cap.
Verify: `go test -count=1 -race ./...`

### The two rules: cap the call, and skip work you cannot finish

The deriver does one small computation and gets two things right. First it caps:
the effective timeout is the *smaller* of the fixed per-call maximum and the time
remaining until the parent's own deadline.

```
timeout := r.PerCall
if dl, ok := parent.Deadline(); ok {
    if rem := time.Until(dl); rem < timeout {
        timeout = rem
    }
}
return context.WithTimeout(parent, timeout)
```

If the parent still has plenty of time, `PerCall` wins and one slow query can only
consume its share — the rest of the budget is preserved for the response encode,
a second query, a cache write. If the parent is nearly out of time, the remaining
budget wins and the derived context is even tighter. Either way the derived
context also inherits the parent's cancellation, so a client disconnect still
propagates.

Second, it skips. Before deriving anything, `Run` checks whether the remaining
budget is below a slack threshold; if so it returns a wrapped `ErrBudgetExhausted`
*without running the query at all*. Starting a query you already know will be
cancelled in a millisecond just burns a connection and a round trip for a
guaranteed failure. Returning fast and honestly is strictly better — the caller
learns "out of budget" immediately instead of after a doomed attempt.

The one discipline the compiler will not enforce for you: every `WithTimeout`
returns a `CancelFunc` that must be called, or the timer leaks until the parent is
done. `Run` uses `defer cancel()`. Run `go vet` — its `lostcancel` analyzer flags
a `CancelFunc` that escapes without being called, which is the mechanical check
for this class of leak.

Set up the module:

```bash
mkdir -p ~/go-exercises/querybudget/cmd/demo
cd ~/go-exercises/querybudget
go mod init example.com/querybudget
go mod edit -go=1.25
```

Create `querybudget.go`:

```go
package querybudget

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrBudgetExhausted is returned when the parent's remaining budget is below the
// slack threshold, so the query is skipped rather than started and cancelled.
var ErrBudgetExhausted = errors.New("querybudget: request budget exhausted")

// Query is a context-aware unit of work (a stand-in for a QueryContext call).
type Query func(ctx context.Context) (string, error)

// Runner applies a per-call timeout budget to each query.
type Runner struct {
	PerCall  time.Duration // hard cap on any single call
	MinSlack time.Duration // skip the call if remaining parent budget is this small or smaller
}

// Run derives a per-call context and runs q under it. It skips q with a wrapped
// ErrBudgetExhausted if the parent has too little time left to bother.
func (r *Runner) Run(parent context.Context, q Query) (string, error) {
	if dl, ok := parent.Deadline(); ok {
		if rem := time.Until(dl); rem <= r.MinSlack {
			return "", fmt.Errorf("querybudget.Run: %w (remaining %v)", ErrBudgetExhausted, rem)
		}
	}
	ctx, cancel := r.derive(parent)
	defer cancel()
	return q(ctx)
}

// derive returns a child context whose timeout is the smaller of PerCall and the
// parent's remaining budget.
func (r *Runner) derive(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := r.PerCall
	if dl, ok := parent.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			timeout = rem
		}
	}
	return context.WithTimeout(parent, timeout)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/querybudget"
)

func slow(latency time.Duration) querybudget.Query {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(latency):
			return "row", nil
		case <-ctx.Done():
			return "", fmt.Errorf("query: %w", ctx.Err())
		}
	}
}

func main() {
	r := &querybudget.Runner{PerCall: 50 * time.Millisecond, MinSlack: 10 * time.Millisecond}

	p1, c1 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer c1()
	start := time.Now()
	_, err := r.Run(p1, slow(500*time.Millisecond))
	fmt.Printf("cap wins: deadline=%v capped=%v\n",
		errors.Is(err, context.DeadlineExceeded), time.Since(start) < 120*time.Millisecond)

	p2, c2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer c2()
	time.Sleep(2 * time.Millisecond)
	_, err = r.Run(p2, slow(time.Millisecond))
	fmt.Printf("skip: exhausted=%v\n", errors.Is(err, querybudget.ErrBudgetExhausted))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cap wins: deadline=true capped=true
skip: exhausted=true
```

### Tests

`TestParentBudgetWins` inspects the derived deadline directly (the test is in
`package querybudget`, so it can call `derive`) to prove the cap math, rather than
relying only on timing.

Create `querybudget_test.go`:

```go
package querybudget

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func slow(latency time.Duration) Query {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(latency):
			return "row", nil
		case <-ctx.Done():
			return "", fmt.Errorf("query: %w", ctx.Err())
		}
	}
}

func TestPerCallCapWins(t *testing.T) {
	t.Parallel()
	r := &Runner{PerCall: 50 * time.Millisecond, MinSlack: 10 * time.Millisecond}

	parent, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(parent, slow(500*time.Millisecond))
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: err = %v, want DeadlineExceeded", err)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("per-call cap did not apply: took %v, want ~50ms", elapsed)
	}
}

func TestParentBudgetWins(t *testing.T) {
	t.Parallel()
	r := &Runner{PerCall: 50 * time.Millisecond, MinSlack: time.Millisecond}

	parent, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	parentDL, _ := parent.Deadline()

	ctx, cancelChild := r.derive(parent)
	defer cancelChild()

	childDL, ok := ctx.Deadline()
	if !ok {
		t.Fatal("derived context has no deadline")
	}
	if childDL.After(parentDL) {
		t.Fatalf("derived deadline %v is after parent %v; parent budget must win", childDL, parentDL)
	}
	if d := time.Until(childDL); d > 30*time.Millisecond {
		t.Fatalf("derived timeout %v exceeds parent's remaining 30ms", d)
	}
}

func TestSkipsWhenBudgetExhausted(t *testing.T) {
	t.Parallel()
	r := &Runner{PerCall: 50 * time.Millisecond, MinSlack: 20 * time.Millisecond}

	parent, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	called := false
	_, err := r.Run(parent, func(ctx context.Context) (string, error) {
		called = true
		return "row", nil
	})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Run: err = %v, want ErrBudgetExhausted", err)
	}
	if called {
		t.Fatal("query was run despite exhausted budget; it must be skipped")
	}
}

func TestNoDeadlineUsesPerCall(t *testing.T) {
	t.Parallel()
	r := &Runner{PerCall: 20 * time.Millisecond, MinSlack: 5 * time.Millisecond}

	start := time.Now()
	_, err := r.Run(context.Background(), slow(500*time.Millisecond))
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: err = %v, want DeadlineExceeded", err)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("per-call cap did not apply without a parent deadline: took %v", elapsed)
	}
}

func TestSuccessWithinBudget(t *testing.T) {
	t.Parallel()
	r := &Runner{PerCall: 100 * time.Millisecond, MinSlack: 5 * time.Millisecond}
	got, err := r.Run(context.Background(), slow(time.Millisecond))
	if err != nil {
		t.Fatalf("Run: err = %v, want nil", err)
	}
	if got != "row" {
		t.Fatalf("Run = %q, want row", got)
	}
}

func ExampleRunner_Run() {
	r := &Runner{PerCall: 50 * time.Millisecond, MinSlack: 5 * time.Millisecond}
	got, _ := r.Run(context.Background(), func(ctx context.Context) (string, error) {
		return "ok", nil
	})
	fmt.Println(got)
	// Output: ok
}
```

## Review

The deriver is correct when the effective timeout is always `min(PerCall, remaining)`
and no work starts once the budget is spent. `TestParentBudgetWins` checks the
math structurally — the derived deadline never falls after the parent's, and the
derived timeout never exceeds the parent's remaining time — so it does not depend
on flaky wall-clock timing. `TestSkipsWhenBudgetExhausted` uses a `called` flag to
prove the query is not merely cancelled but never invoked. Two failure modes this
prevents: forwarding the whole request budget so one slow query starves
everything after it, and leaking a `CancelFunc` (caught by `go vet`). Run `-race`
because the fake query and the timeout timer run on separate goroutines.

## Resources

- [context: WithTimeout](https://pkg.go.dev/context#WithTimeout) and [Context.Deadline](https://pkg.go.dev/context#Context) — deriving a capped child and reading the remaining budget.
- [time: Until](https://pkg.go.dev/time#Until) — the remaining-budget computation.
- [go vet: lostcancel](https://pkg.go.dev/cmd/vet) — the analyzer that flags an uncalled `CancelFunc`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-cancellation-cause-classifier.md](07-cancellation-cause-classifier.md)
