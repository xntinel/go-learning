# Exercise 8: Fan-Out With Per-Branch Sub-Budgets From One Request Deadline

An aggregation endpoint fans out to several independent sources and must return
within the request budget even if one source is slow. This exercise builds that
fan-out: each branch derives its context from the shared parent deadline so no branch
outlives the request, slow branches yield `DeadlineExceeded` and fold into a partial
result while fast branches succeed, and the whole thing aggregates safely under a
`WaitGroup` and a mutex with per-branch error collection via `errors.Join`.

This module is fully self-contained. It has its own `go mod init`, defines every type
it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
profile-fanout/                      independent module: example.com/profilefanout
  go.mod                             go 1.26
  fanout.go                          Source, Result, AggregateProfile; per-branch sub-budgets
  cmd/
    demo/
      main.go                        runnable demo: two fast branches, one slow -> partial
  fanout_test.go                     partial-result, branch-deadline bound, wall-time, -race
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: a `Source{Name, Timeout, Fetch}` and `AggregateProfile(ctx, budget, sources) Result` that derives a parent `WithTimeout`, launches each branch under its own `WithTimeout(parent, s.Timeout)`, collects successes into a map and failures via `errors.Join`, and returns the effective deadline.
- Test: two fast branches present in the data, one slow branch recorded as a `DeadlineExceeded` partial via `errors.Join`; total wall time near the parent budget, not the slow branch's full duration; every branch context's deadline is no later than the parent's.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/profile-fanout/cmd/demo
cd ~/go-exercises/profile-fanout
go mod init example.com/profilefanout
```

### One request budget, many branches

The parent `context.WithTimeout(ctx, budget)` establishes the request's total budget.
Every branch then derives *its own* sub-context with `context.WithTimeout(parent,
s.Timeout)`. Because of the earliest-deadline-wins rule, a branch whose per-branch
timeout is longer than the parent's remaining budget silently inherits the parent's
deadline — so a branch can never outlive the request, no matter what timeout it asks
for. This is the property that keeps a fan-out honest: the aggregate returns when the
request budget elapses, and any branch still in flight is cancelled at that instant
rather than dragging the response past its SLO. Deriving per-branch contexts (rather
than sharing one) also lets you give a known-slow source a *tighter* budget than the
others, and it means cancelling one branch's context does not disturb its siblings.

The branches run concurrently, each in its own goroutine tracked by a `WaitGroup`.
Their results land in shared state — a map of successes and a slice of errors — so
access is guarded by a mutex. When a branch succeeds, its value goes in the map keyed
by source name; when it fails (a timeout, a downstream error), the error is wrapped
with the branch name and appended to the error slice. After `wg.Wait()`, the errors
are folded with `errors.Join`, which produces a single error whose `Unwrap` and
`errors.Is` see every joined member — so `errors.Is(result.Err, context.DeadlineExceeded)`
is true if any branch timed out, while the map still holds every branch that
succeeded. That is the shape of a graceful partial result: return what you have, and
report what you lost, in one value.

`errors.Join(nil)` (or `errors.Join()` with no failures) returns nil, so a fully
successful aggregation has a nil `Err`. The `Result` also carries the parent's
effective `Deadline`, which lets a caller — and the test — verify that every branch
was bounded by it.

Create `fanout.go`:

```go
package profilefanout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Source is one downstream dependency in the fan-out. Timeout is its per-branch
// sub-budget, which is clamped to the parent deadline.
type Source struct {
	Name    string
	Timeout time.Duration
	Fetch   func(ctx context.Context) (string, error)
}

// Result is the aggregated outcome: the successful branches by name, the joined
// errors from failed branches, and the request's effective deadline.
type Result struct {
	Data     map[string]string
	Err      error
	Deadline time.Time
}

// AggregateProfile fans out to every source concurrently under one shared budget.
// Each branch inherits the earliest deadline, so none outlives the request.
func AggregateProfile(ctx context.Context, budget time.Duration, sources []Source) Result {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline, _ := ctx.Deadline()

	var (
		mu   sync.Mutex
		data = make(map[string]string)
		errs []error
		wg   sync.WaitGroup
	)

	for _, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bctx, bcancel := context.WithTimeout(ctx, s.Timeout)
			defer bcancel()

			v, err := s.Fetch(bctx)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
				return
			}
			data[s.Name] = v
		}()
	}
	wg.Wait()

	return Result{Data: data, Err: errors.Join(errs...), Deadline: deadline}
}
```

### The runnable demo

The demo aggregates three sources — two fast, one that blocks past the 40ms budget —
and prints the partial data plus whether the joined error is a deadline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/profilefanout"
)

func fast(v string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) { return v, nil }
}

func slow() func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return "late", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func main() {
	sources := []profilefanout.Source{
		{Name: "name", Timeout: time.Second, Fetch: fast("alice")},
		{Name: "email", Timeout: time.Second, Fetch: fast("alice@example.com")},
		{Name: "prefs", Timeout: time.Second, Fetch: slow()},
	}

	start := time.Now()
	r := profilefanout.AggregateProfile(context.Background(), 40*time.Millisecond, sources)
	elapsed := time.Since(start)

	fmt.Printf("name=%q email=%q\n", r.Data["name"], r.Data["email"])
	fmt.Printf("prefs present=%v deadline-err=%v fast=%v\n",
		r.Data["prefs"] != "", errors.Is(r.Err, context.DeadlineExceeded), elapsed < 200*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name="alice" email="alice@example.com"
prefs present=false deadline-err=true fast=true
```

### Tests

The test sources capture the deadline of the context they were handed, so the test can
assert every branch was bounded by the parent. `TestPartialResultOnSlowBranch` asserts
the two fast branches are present, the slow branch is absent, and the joined error
`errors.Is` `context.DeadlineExceeded`. `TestWallTimeNearBudget` asserts the aggregate
returns near the budget, not the slow branch's 500ms. `TestBranchDeadlineBoundedByParent`
asserts each captured branch deadline is no later than the parent's.
`TestAllFastNoError` asserts a fully successful fan-out has a nil error.

Create `fanout_test.go`:

```go
package profilefanout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// capturing wraps a fetch func, recording the deadline of the ctx it received.
func capturing(mu *sync.Mutex, seen *[]time.Time, inner func(context.Context) (string, error)) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if dl, ok := ctx.Deadline(); ok {
			mu.Lock()
			*seen = append(*seen, dl)
			mu.Unlock()
		}
		return inner(ctx)
	}
}

func fast(v string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) { return v, nil }
}

func slow() func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return "late", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func TestPartialResultOnSlowBranch(t *testing.T) {
	t.Parallel()
	sources := []Source{
		{Name: "name", Timeout: time.Second, Fetch: fast("alice")},
		{Name: "email", Timeout: time.Second, Fetch: fast("a@example.com")},
		{Name: "prefs", Timeout: time.Second, Fetch: slow()},
	}
	r := AggregateProfile(context.Background(), 40*time.Millisecond, sources)

	if r.Data["name"] != "alice" || r.Data["email"] != "a@example.com" {
		t.Fatalf("fast branches missing: %v", r.Data)
	}
	if _, ok := r.Data["prefs"]; ok {
		t.Fatalf("slow branch should be absent, got %q", r.Data["prefs"])
	}
	if !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want a DeadlineExceeded partial", r.Err)
	}
}

func TestWallTimeNearBudget(t *testing.T) {
	t.Parallel()
	sources := []Source{
		{Name: "fast", Timeout: time.Second, Fetch: fast("v")},
		{Name: "slow", Timeout: time.Second, Fetch: slow()},
	}
	start := time.Now()
	_ = AggregateProfile(context.Background(), 40*time.Millisecond, sources)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("aggregate took %v, want near the 40ms budget, not the slow 500ms", elapsed)
	}
}

func TestBranchDeadlineBoundedByParent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []time.Time
	sources := []Source{
		{Name: "a", Timeout: time.Hour, Fetch: capturing(&mu, &seen, fast("1"))},
		{Name: "b", Timeout: time.Hour, Fetch: capturing(&mu, &seen, fast("2"))},
	}
	r := AggregateProfile(context.Background(), 50*time.Millisecond, sources)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("captured %d branch deadlines, want 2", len(seen))
	}
	for i, dl := range seen {
		if dl.After(r.Deadline) {
			t.Fatalf("branch %d deadline %v is after parent %v", i, dl, r.Deadline)
		}
	}
}

func TestAllFastNoError(t *testing.T) {
	t.Parallel()
	sources := []Source{
		{Name: "a", Timeout: time.Second, Fetch: fast("1")},
		{Name: "b", Timeout: time.Second, Fetch: fast("2")},
	}
	r := AggregateProfile(context.Background(), time.Second, sources)
	if r.Err != nil {
		t.Fatalf("Err = %v, want nil for an all-success fan-out", r.Err)
	}
	if len(r.Data) != 2 {
		t.Fatalf("Data = %v, want both branches", r.Data)
	}
}
```

## Review

The fan-out is correct when one slow branch cannot delay the aggregate and cannot
escape the request budget. Each branch derives `WithTimeout(parent, s.Timeout)`, so a
branch asking for an hour still dies with the 50ms parent — `TestBranchDeadlineBoundedByParent`
proves every captured branch deadline is no later than the parent's. The partial-result
shape is the payoff: `TestPartialResultOnSlowBranch` shows the fast branches present in
the map while the slow one is folded into a joined `DeadlineExceeded`, and
`TestWallTimeNearBudget` confirms the whole call returns near the budget, not the slow
branch's full duration.

The mistakes to avoid: giving a branch a fixed timeout and assuming it extends the
request (it cannot — the parent wins); sharing mutable result state across goroutines
without the mutex (a data race the detector would catch); and treating any branch
failure as a total failure instead of a partial. `errors.Join` is the right tool
because it preserves `errors.Is` across every member and returns nil when there are no
failures. Run `go test -race`; the concurrent map/slice writes are guarded and the
capture helper is mutex-safe.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — folding per-branch errors into one that preserves errors.Is across members.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — waiting for every branch goroutine to finish.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — deriving per-branch sub-budgets that inherit the parent deadline.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — propagating one request's cancellation across a fan-out.

---

Back to [07-deadline-afterfunc-cleanup.md](07-deadline-afterfunc-cleanup.md) | Next: [09-cache-fallback-fast-timeout.md](09-cache-fallback-fast-timeout.md)
