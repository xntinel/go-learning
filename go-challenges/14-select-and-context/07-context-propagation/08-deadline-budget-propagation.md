# Exercise 8: Enforce a Shrinking Deadline Budget Across a Call Chain

An end-to-end SLO is a budget that shrinks as each hop spends it. A request with
200ms to live cannot let its auth, profile, and billing hops each help themselves
to a fresh generous timeout — every hop must derive a per-call deadline that can
only *reduce* the inherited one, and must fail fast when there is not enough budget
left to bother starting. This exercise builds that coordinator.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
budget/                      independent module: example.com/budget
  go.mod                     go 1.24
  budget.go                  Coordinator.hopContext (shrink-only, fail-fast floor,
                             default when no deadline); Client.Call; Run over hops
  cmd/
    demo/
      main.go                a chain that fits the budget and one that blows it
  budget_test.go             per-hop shrinks, fail-fast floor, default fallback, exceed propagates
```

Files: `budget.go`, `cmd/demo/main.go`, `budget_test.go`.
Implement: a `Coordinator` whose `hopContext(parent, want)` reads
`parent.Deadline()`, derives a child timeout that can only shrink the inherited
deadline, returns `ErrBudgetExhausted` when the remaining budget is below a floor,
and falls back to a default when the parent has no deadline; a `Run` that chains
hops sequentially.
Test: each derived hop's `Deadline()` is `<=` the parent's; a below-floor hop
fails fast without calling downstream; a deadline-free parent uses the default; a
chain that overruns propagates `context.DeadlineExceeded`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/07-context-propagation/08-deadline-budget-propagation/cmd/demo
cd go-solutions/14-select-and-context/07-context-propagation/08-deadline-budget-propagation
go mod edit -go=1.24
```

### Derive down, never up; and fail fast

A context's *effective* deadline is already the minimum of its own and its
parent's — so `context.WithTimeout(parent, 5*time.Second)` on a parent with 100ms
left correctly keeps the 100ms. The bug is the other direction: if the parent has
*no* deadline, or a looser one, that same call silently grants this hop a fresh
5-second budget that has nothing to do with the SLO. The discipline is to read the
inherited deadline first and only ever shorten:

```go
if dl, ok := parent.Deadline(); ok {
	remaining := time.Until(dl)
	if want > remaining {
		want = remaining // shrink to fit; never widen past the inherited deadline
	}
}
```

The second half is the fail-fast floor. There is no point starting a call that
needs 50ms when 3ms of budget remain — it will just block, blow the deadline, and
return an error you could have returned immediately without touching the
downstream. So before deriving the hop context, if `time.Until(deadline)` is below
a configured floor, return `ErrBudgetExhausted` and skip the call entirely. This
turns a guaranteed-doomed round-trip into an instant, cheap failure, which under
load is the difference between shedding gracefully and piling up blocked
goroutines.

Finally, a parent with no deadline at all (a background job, a request that never
set one) should not run unbounded — fall back to a configured default budget so
every hop is still bounded. `hopContext` encodes all three rules in one place, and
`Run` chains the hops so the budget genuinely shrinks hop over hop as wall-clock
time is spent.

Create `budget.go`:

```go
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrBudgetExhausted is returned when the remaining deadline budget is below the
// floor, so a hop refuses to start rather than doing doomed work.
var ErrBudgetExhausted = errors.New("budget: remaining below floor")

// Client models one downstream hop that respects its context.
type Client struct {
	name string
	work time.Duration
}

// NewClient returns a Client whose call takes work to complete.
func NewClient(name string, work time.Duration) *Client {
	return &Client{name: name, work: work}
}

// Call races the simulated work against ctx and wraps the cause on abort.
func (c *Client) Call(ctx context.Context) error {
	select {
	case <-time.After(c.work):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", c.name, ctx.Err())
	}
}

// hop pairs a nominal per-hop budget with the client to call.
type hop struct {
	want   time.Duration
	client *Client
}

// Coordinator runs a sequence of hops under a shrinking deadline budget.
type Coordinator struct {
	floor         time.Duration
	defaultBudget time.Duration
	hops          []hop
}

// NewCoordinator builds a Coordinator. floor is the minimum remaining budget a
// hop needs to start; defaultBudget bounds hops when the parent has no deadline.
func NewCoordinator(floor, defaultBudget time.Duration) *Coordinator {
	return &Coordinator{floor: floor, defaultBudget: defaultBudget}
}

// AddHop appends a hop with a nominal per-hop budget of want.
func (co *Coordinator) AddHop(want time.Duration, client *Client) {
	co.hops = append(co.hops, hop{want: want, client: client})
}

// hopContext derives a per-hop context from parent. It can only shrink the
// inherited deadline, refuses to start when the remaining budget is below the
// floor, and falls back to defaultBudget when the parent has no deadline.
func (co *Coordinator) hopContext(parent context.Context, want time.Duration) (context.Context, context.CancelFunc, error) {
	if dl, ok := parent.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining < co.floor {
			return nil, func() {}, ErrBudgetExhausted
		}
		if want > remaining {
			want = remaining // shrink only
		}
	} else {
		want = co.defaultBudget
	}
	ctx, cancel := context.WithTimeout(parent, want)
	return ctx, cancel, nil
}

// Run executes every hop in order under the shrinking budget. The first hop to
// fail (including a fail-fast ErrBudgetExhausted) stops the chain.
func (co *Coordinator) Run(ctx context.Context) error {
	for i, h := range co.hops {
		hctx, cancel, err := co.hopContext(ctx, h.want)
		if err != nil {
			return fmt.Errorf("hop %d (%s): %w", i, h.client.name, err)
		}
		err = h.client.Call(hctx)
		cancel()
		if err != nil {
			return fmt.Errorf("hop %d: %w", i, err)
		}
	}
	return nil
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

	"example.com/budget"
)

func build(hopWork time.Duration) *budget.Coordinator {
	co := budget.NewCoordinator(5*time.Millisecond, 50*time.Millisecond)
	co.AddHop(100*time.Millisecond, budget.NewClient("auth", hopWork))
	co.AddHop(100*time.Millisecond, budget.NewClient("profile", hopWork))
	co.AddHop(100*time.Millisecond, budget.NewClient("billing", hopWork))
	return co
}

func main() {
	// Fits the budget: 3 hops * 10ms of work under 200ms.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	fmt.Printf("generous: err=%v\n", build(10*time.Millisecond).Run(ctx))

	// Tight timeout: the first hop's work alone exceeds the whole budget.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel2()
	err2 := build(50 * time.Millisecond).Run(ctx2)
	fmt.Printf("timeout:  deadline=%v\n", errors.Is(err2, context.DeadlineExceeded))

	// Below floor: only 2ms of budget, floor is 5ms, so the first hop refuses.
	ctx3, cancel3 := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel3()
	err3 := build(10 * time.Millisecond).Run(ctx3)
	fmt.Printf("floor:    exhausted=%v\n", errors.Is(err3, budget.ErrBudgetExhausted))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
generous: err=<nil>
timeout:  deadline=true
floor:    exhausted=true
```

Each failing scenario hits exactly one classification: the tight-timeout run blows
its deadline inside the first hop (`deadline=true`), while the 2ms run never had
enough budget to start and fails fast at the floor (`exhausted=true`). A single
error chain carries one cause, so the two modes are demonstrated by two separate
runs.

### Tests

Create `budget_test.go`:

```go
package budget

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPerHopBudgetShrinks(t *testing.T) {
	t.Parallel()

	co := NewCoordinator(time.Millisecond, 50*time.Millisecond)
	parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	parentDL, _ := parent.Deadline()

	for _, want := range []time.Duration{20 * time.Millisecond, 500 * time.Millisecond} {
		hctx, hcancel, err := co.hopContext(parent, want)
		if err != nil {
			t.Fatalf("hopContext(want=%v): unexpected err %v", want, err)
		}
		childDL, ok := hctx.Deadline()
		if !ok {
			t.Fatalf("hop context has no deadline")
		}
		if childDL.After(parentDL) {
			t.Fatalf("child deadline %v is after parent %v (widened the budget)", childDL, parentDL)
		}
		hcancel()
	}
}

func TestFailsFastBelowFloor(t *testing.T) {
	t.Parallel()

	// The downstream would take 100ms; a fail-fast return proves it never ran.
	co := NewCoordinator(50*time.Millisecond, 50*time.Millisecond)
	co.AddHop(10*time.Millisecond, NewClient("downstream", 100*time.Millisecond))

	parent, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // drive the remaining budget below the floor

	start := time.Now()
	err := co.Run(parent)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Run err = %v, want ErrBudgetExhausted", err)
	}
	if elapsed > 20*time.Millisecond {
		t.Fatalf("fail-fast should be instant; took %v (did downstream run?)", elapsed)
	}
}

func TestNoDeadlineParentUsesDefault(t *testing.T) {
	t.Parallel()

	co := NewCoordinator(time.Millisecond, 30*time.Millisecond)
	hctx, cancel, err := co.hopContext(context.Background(), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("hopContext: %v", err)
	}
	defer cancel()

	dl, ok := hctx.Deadline()
	if !ok {
		t.Fatal("deadline-free parent should still yield a bounded hop deadline")
	}
	remaining := time.Until(dl)
	if remaining < 20*time.Millisecond || remaining > 40*time.Millisecond {
		t.Fatalf("default budget deadline off: remaining=%v, want ~30ms", remaining)
	}
}

func TestBudgetExceededPropagates(t *testing.T) {
	t.Parallel()

	co := NewCoordinator(5*time.Millisecond, 50*time.Millisecond)
	co.AddHop(100*time.Millisecond, NewClient("slow", 50*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()

	err := co.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want DeadlineExceeded", err)
	}
}

func ExampleCoordinator_Run() {
	co := NewCoordinator(5*time.Millisecond, 50*time.Millisecond)
	co.AddHop(10*time.Millisecond, NewClient("auth", 10*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // drive the remaining budget below the floor

	fmt.Println(errors.Is(co.Run(ctx), ErrBudgetExhausted))
	// Output: true
}
```

## Review

The coordinator is correct when a derived hop deadline is never later than the
inherited one and a below-floor hop never starts. `TestPerHopBudgetShrinks` checks
the invariant from both sides — a hop asking for less than the remaining budget and
a hop asking for far more both end up bounded by the parent — which is the "shrink
only, never widen" rule made testable. `TestFailsFastBelowFloor` proves the guard
fires before any downstream work, the thing that keeps a nearly-expired request
from wasting a round-trip. Watch the one subtlety the tests encode: the floor check
uses `time.Until(deadline)`, which can go negative if the parent is already past
its deadline, and a negative value is correctly still "below the floor". Run
`go test -race`; the coordinator holds no shared mutable state across hops, so a
clean race build confirms the sequential chaining is sound.

## Resources

- [`context.Context.Deadline`](https://pkg.go.dev/context#Context) — reading the inherited deadline.
- [`context.WithDeadline` and `context.WithTimeout`](https://pkg.go.dev/context#WithDeadline) — deriving a bounded child.
- [`time.Until`](https://pkg.go.dev/time#Until) — remaining budget from a deadline.

---

Prev: [07-detached-cleanup-withoutcancel.md](07-detached-cleanup-withoutcancel.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-ctx-first-fitness-test.md](09-ctx-first-fitness-test.md)
