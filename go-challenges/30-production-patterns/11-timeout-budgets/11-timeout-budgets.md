# 11. Timeout Budgets

A request that fans out to three sequential downstream services under a single 2-second timeout can easily spend all 2 seconds on the first call, leaving the remaining two with effectively nothing. A timeout budget makes the remaining time explicit: it is a value you carry through the call chain, sub-allocate to each operation, and check before you start work that is not worth starting. The hard part is tracking the interplay between the parent deadline and the per-operation deadline without double-cancellation and without leaking cancel functions.

```text
budget/
  go.mod
  budget.go
  budget_test.go
  cmd/demo/main.go
```

## Concepts

### The Budget Model

A timeout budget is a wrapper around a `context.Context` that exposes how much spendable time remains. The standard library already provides the raw mechanism: `ctx.Deadline()` returns the absolute deadline, and `time.Until(deadline)` converts it to a remaining duration. A `Budget` type codifies that computation and adds two things the raw context does not: a reserved margin (time the handler needs after all downstream calls finish to assemble and write its response) and convenience allocators that derive child contexts capped to a fraction of what is left.

```
parent context  (deadline T)
      |
      v
  Budget (reserved = R)
      |-- Remaining() = time.Until(T) - R
      |-- Allocate(0.4) -> child context deadline = now + Remaining()*0.4
      |-- AllocateFixed(500ms) -> child context deadline capped at Remaining()
```

Callers sub-allocate the budget rather than hand the parent context directly. This means a slow first call does not silently consume the entire budget: the second call gets a fresh `context.WithTimeout` that is bounded by what is left, not by the original ceiling.

### Context Deadline Semantics

`context.WithDeadline(parent, d)` returns a context whose deadline is `min(parent.deadline, d)`. This is the crucial property: you cannot accidentally grant a child context more time than the parent allows. `context.WithTimeout(parent, d)` is shorthand for `context.WithDeadline(parent, time.Now().Add(d))`. Both return a `CancelFunc`; callers must call it to release timer resources, even when the deadline fires first.

When the deadline expires, `ctx.Err()` returns `context.DeadlineExceeded` and `ctx.Done()` is closed. Downstream code that passes the context to `http.Client.Do`, `sql.QueryContext`, or any other context-aware call sees the cancellation automatically.

### The Reserved Margin

Without a reserved margin, the budget allocates 100 % of the deadline to downstream calls. The handler then has zero time to serialize the response, write headers, or log the outcome. A practical value is 50-150 ms depending on the work done after the last downstream call. The margin is subtracted from every `Remaining()` call, so allocators see a smaller pool than the raw deadline suggests, and the handler keeps the margin for itself.

### When to Skip Work

`Budget.HasTimeFor(d)` answers "is there at least d left after the reserved margin?". Call it before low-priority operations: notifications, cache warming, or analytics events. If the answer is false, record the skip in the response and move on. Skipping is correct behavior, not an error.

### Propagating the Budget Across Service Boundaries

Services that call other services over HTTP benefit from forwarding the remaining budget in a request header. The receiving service constructs its own budget from that header instead of imposing a hard-coded timeout. The header value is the remaining milliseconds as a plain integer string. Header-based propagation is convention, not a protocol: both sides must agree on the header name.

## Exercises

This is a library, not a program. Verify it with `go test`.

### Exercise 1: The Budget Type

Create `budget.go`:

```go
package budget

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// HeaderBudget is the HTTP header used to propagate remaining budget to
// downstream services as an integer number of milliseconds.
const HeaderBudget = "X-Timeout-Budget-Ms"

// ErrNoBudget is returned when a Budget cannot be created from a context
// that has no deadline.
var ErrNoBudget = errors.New("budget: no time remaining")

// Budget tracks spendable time within a context deadline. It holds back a
// reserved margin so the caller always has time to assemble a response after
// all downstream calls finish.
type Budget struct {
	ctx      context.Context
	deadline time.Time
	reserved time.Duration
}

// New creates a Budget from the given context, using its deadline as the
// ceiling. If the context has no deadline, New returns ErrNoBudget. The
// reserved margin is subtracted from every Remaining call so allocators see
// only the spendable portion of the budget.
func New(ctx context.Context, reserved time.Duration) (*Budget, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		return nil, fmt.Errorf("%w: parent context has no deadline", ErrNoBudget)
	}
	return &Budget{ctx: ctx, deadline: dl, reserved: reserved}, nil
}

// WithTimeout creates a Budget by attaching a fresh deadline to the parent
// context. total is the end-to-end timeout; reserved is the margin held back
// from allocators. The returned CancelFunc must be called to release resources.
func WithTimeout(parent context.Context, total, reserved time.Duration) (*Budget, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, total)
	dl, _ := ctx.Deadline()
	return &Budget{ctx: ctx, deadline: dl, reserved: reserved}, cancel
}

// Remaining returns the time left in the budget after subtracting the
// reserved margin. It is never negative.
func (b *Budget) Remaining() time.Duration {
	r := time.Until(b.deadline) - b.reserved
	if r < 0 {
		return 0
	}
	return r
}

// Exhausted reports whether no spendable time remains.
func (b *Budget) Exhausted() bool {
	return b.Remaining() == 0
}

// HasTimeFor reports whether at least d of spendable time remains.
func (b *Budget) HasTimeFor(d time.Duration) bool {
	return b.Remaining() >= d
}

// Allocate returns a child context whose deadline is the given fraction of
// the remaining budget. fraction must be in (0, 1]. A minimum of 1 ms is
// enforced so the returned context is never immediately expired.
// The caller must call the returned CancelFunc.
func (b *Budget) Allocate(fraction float64) (context.Context, context.CancelFunc) {
	alloc := time.Duration(float64(b.Remaining()) * fraction)
	if alloc < time.Millisecond {
		alloc = time.Millisecond
	}
	return context.WithTimeout(b.ctx, alloc)
}

// AllocateFixed returns a child context whose deadline is desired, capped at
// the remaining budget. The caller must call the returned CancelFunc.
func (b *Budget) AllocateFixed(desired time.Duration) (context.Context, context.CancelFunc) {
	rem := b.Remaining()
	if desired > rem {
		desired = rem
	}
	if desired < time.Millisecond {
		desired = time.Millisecond
	}
	return context.WithTimeout(b.ctx, desired)
}

// SetOnRequest writes the remaining budget in milliseconds into req's
// HeaderBudget header so the downstream service can construct its own budget.
func (b *Budget) SetOnRequest(req *http.Request) {
	req.Header.Set(HeaderBudget, strconv.FormatInt(b.Remaining().Milliseconds(), 10))
}

// FromRequest constructs a Budget from the incoming HTTP request. It reads
// the HeaderBudget header if present; otherwise it falls back to the request
// context's deadline. reserved is the margin held back for response assembly.
func FromRequest(r *http.Request, reserved time.Duration) (*Budget, error) {
	header := r.Header.Get(HeaderBudget)
	if header != "" {
		ms, err := strconv.ParseInt(header, 10, 64)
		if err == nil && ms > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), time.Duration(ms)*time.Millisecond)
			defer cancel()
			dl, _ := ctx.Deadline()
			b := &Budget{ctx: r.Context(), deadline: dl, reserved: reserved}
			return b, nil
		}
	}
	return New(r.Context(), reserved)
}

// String returns a human-readable snapshot of the budget state for logging.
func (b *Budget) String() string {
	return fmt.Sprintf("Budget{remaining=%v reserved=%v}",
		b.Remaining().Round(time.Millisecond), b.reserved)
}
```

Defaults flow through the parent context. `Allocate` and `AllocateFixed` derive child contexts using `context.WithTimeout`, which obeys the parent ceiling automatically: if the parent deadline is closer than the requested timeout, the parent wins.

### Exercise 2: Tests and Example

Create `budget_test.go`:

```go
package budget

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestNewRejectsContextWithoutDeadline(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), 50*time.Millisecond)
	if !errors.Is(err, ErrNoBudget) {
		t.Fatalf("err = %v, want ErrNoBudget", err)
	}
}

func TestNewAcceptsContextWithDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	b, err := New(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Remaining() <= 0 {
		t.Fatal("Remaining should be positive immediately after construction")
	}
}

func TestRemainingSubtractsReserved(t *testing.T) {
	t.Parallel()

	const total = 500 * time.Millisecond
	const reserved = 100 * time.Millisecond
	b, cancel := WithTimeout(context.Background(), total, reserved)
	defer cancel()

	rem := b.Remaining()
	if rem >= total {
		t.Fatalf("Remaining %v >= total %v; reserved not subtracted", rem, total)
	}
	if rem > total-reserved {
		t.Fatalf("Remaining %v > total-reserved %v", rem, total-reserved)
	}
}

func TestExhaustedFalseWhenTimeLeft(t *testing.T) {
	t.Parallel()

	b, cancel := WithTimeout(context.Background(), time.Second, 50*time.Millisecond)
	defer cancel()

	if b.Exhausted() {
		t.Fatal("Exhausted should be false when budget is ample")
	}
}

func TestHasTimeFor(t *testing.T) {
	t.Parallel()

	// 10ms total, 5ms reserved -> ~5ms spendable.
	b, cancel := WithTimeout(context.Background(), 10*time.Millisecond, 5*time.Millisecond)
	defer cancel()

	if b.HasTimeFor(100 * time.Millisecond) {
		t.Fatal("HasTimeFor(100ms) should be false when only ~5ms remain")
	}
	if !b.HasTimeFor(1 * time.Millisecond) {
		t.Fatal("HasTimeFor(1ms) should be true when ~5ms remain")
	}
}

func TestAllocateChildCannotExceedParent(t *testing.T) {
	t.Parallel()

	b, cancel := WithTimeout(context.Background(), 200*time.Millisecond, 50*time.Millisecond)
	defer cancel()

	ctx, childCancel := b.Allocate(1.0)
	defer childCancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("child context should have a deadline")
	}
	parentDl, _ := b.ctx.Deadline()
	if dl.After(parentDl) {
		t.Fatalf("child deadline %v is after parent deadline %v", dl, parentDl)
	}
	if time.Until(dl) <= 0 {
		t.Fatal("child budget should be positive")
	}
}

func TestAllocateFixedCapsAtRemaining(t *testing.T) {
	t.Parallel()

	// 100ms total, 80ms reserved -> ~20ms spendable.
	b, cancel := WithTimeout(context.Background(), 100*time.Millisecond, 80*time.Millisecond)
	defer cancel()

	ctx, childCancel := b.AllocateFixed(500 * time.Millisecond)
	defer childCancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("child context should have a deadline")
	}
	parentDl, _ := b.ctx.Deadline()
	if dl.After(parentDl) {
		t.Fatalf("AllocateFixed exceeded parent deadline: child=%v parent=%v", dl, parentDl)
	}
}

var allocateFractionCases = []struct {
	name     string
	fraction float64
	total    time.Duration
	reserved time.Duration
}{
	{"half", 0.5, time.Second, 100 * time.Millisecond},
	{"full", 1.0, time.Second, 100 * time.Millisecond},
	{"tiny", 0.1, time.Second, 50 * time.Millisecond},
}

func TestAllocateFractions(t *testing.T) {
	t.Parallel()

	for _, tc := range allocateFractionCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, cancel := WithTimeout(context.Background(), tc.total, tc.reserved)
			defer cancel()

			ctx, childCancel := b.Allocate(tc.fraction)
			defer childCancel()

			_, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("%s: child context has no deadline", tc.name)
			}
		})
	}
}

func TestSetOnRequestWritesHeader(t *testing.T) {
	t.Parallel()

	b, cancel := WithTimeout(context.Background(), time.Second, 50*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	b.SetOnRequest(req)

	val := req.Header.Get(HeaderBudget)
	if val == "" {
		t.Fatal("SetOnRequest did not set the header")
	}
}

func ExampleBudget_Remaining() {
	b, cancel := WithTimeout(context.Background(), 2*time.Second, 100*time.Millisecond)
	defer cancel()

	if b.Remaining() > 0 {
		fmt.Println("budget has time remaining")
	}
	// Output:
	// budget has time remaining
}
```

Your turn: add `TestFromRequestFallsBackToContextDeadline`. Create an `*http.Request` with a context that has a 500 ms deadline and no budget header, call `FromRequest`, and assert the returned budget's `Remaining()` is positive and `HasTimeFor(10*time.Millisecond)` returns true.

### Exercise 3: The demo program

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/budget"
)

func main() {
	const total = 2 * time.Second
	const reserved = 100 * time.Millisecond

	ctx, rootCancel := context.WithTimeout(context.Background(), total)
	defer rootCancel()

	b, cancel := budget.WithTimeout(ctx, total, reserved)
	defer cancel()

	log.Printf("initial: %s", b)

	// Step 1: critical - allocate 40 % of the budget.
	step1Ctx, step1Cancel := b.Allocate(0.4)
	result1, err := simulateCall(step1Ctx, "user-profile", 120*time.Millisecond)
	step1Cancel()
	if err != nil {
		log.Printf("user-profile failed: %v", err)
	} else {
		log.Printf("user-profile: %s", result1)
	}

	log.Printf("after step 1: %s", b)

	// Step 2: critical - allocate 50 % of whatever remains.
	if b.Exhausted() {
		log.Printf("skipping recommendations: budget exhausted")
	} else {
		step2Ctx, step2Cancel := b.Allocate(0.5)
		result2, err := simulateCall(step2Ctx, "recommendations", 180*time.Millisecond)
		step2Cancel()
		if err != nil {
			log.Printf("recommendations failed: %v", err)
		} else {
			log.Printf("recommendations: %s", result2)
		}
	}

	log.Printf("after step 2: %s", b)

	// Step 3: non-critical - skip if fewer than 200 ms remain.
	if !b.HasTimeFor(200 * time.Millisecond) {
		log.Printf("skipping notifications: not enough budget")
	} else {
		step3Ctx, step3Cancel := b.Allocate(1.0)
		result3, err := simulateCall(step3Ctx, "notifications", 60*time.Millisecond)
		step3Cancel()
		if err != nil {
			log.Printf("notifications failed: %v", err)
		} else {
			log.Printf("notifications: %s", result3)
		}
	}

	log.Printf("final: %s", b)
}

// simulateCall mimics a downstream call by sleeping for latency.
func simulateCall(ctx context.Context, name string, latency time.Duration) (string, error) {
	select {
	case <-time.After(latency):
		return fmt.Sprintf("%s OK (%v)", name, latency), nil
	case <-ctx.Done():
		return "", fmt.Errorf("%s: %w", name, ctx.Err())
	}
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Passing the Parent Context Directly to Each Operation

Wrong: calling `context.WithTimeout(parentCtx, 2*time.Second)` for every downstream call regardless of how much time has already been consumed. Each call gets the full 2 seconds from the start time; if the first call takes 1.8 s, the second call silently inherits a deadline that is only 0.2 s away even though 2 s were requested.

Fix: always derive the per-operation context from `b.Allocate` or `b.AllocateFixed`. These methods call `time.Until(b.deadline)` to compute what is actually left and cap the child context at that value.

### Forgetting to Call the Child CancelFunc

Wrong:
```go
ctx, _ := b.Allocate(0.4)
resp, err := client.Do(req.WithContext(ctx))
```

`context.WithTimeout` allocates a timer that leaks until the parent deadline fires. Always defer the cancel:

```go
ctx, cancel := b.Allocate(0.4)
defer cancel()
resp, err := client.Do(req.WithContext(ctx))
```

### Setting a Reserved Margin of Zero

Wrong: `WithTimeout(parent, 5*time.Second, 0)`. The handler has zero guaranteed time after the last downstream call. Under load, response serialization or logging can exceed the deadline, causing the client to receive a truncated or missing response.

Fix: reserve at least 50 ms (100-150 ms is a common production default). The value depends on the cost of assembling the response.

### Treating `context.DeadlineExceeded` as an Unexpected Error

Wrong: logging `context.DeadlineExceeded` at ERROR level as if it were an application bug. Budget exhaustion is an expected outcome under load.

Fix: check `errors.Is(err, context.DeadlineExceeded)` and log at INFO or WARN. Return a structured response (e.g., `{"error":"timeout"}`) rather than a 500.

### Ignoring the Minimum Allocation in `Allocate`

Wrong: passing a fraction so small (or having so little time left) that the allocated context is immediately expired. The 1 ms minimum in `Allocate` prevents an instantly-cancelled context, but 1 ms is not enough for a real HTTP call.

Fix: check `b.HasTimeFor(minUsefulLatency)` before allocating to avoid sending requests that are guaranteed to time out.

## Verification

From `~/go-exercises/budget`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass before marking this lesson complete.

## Summary

- A timeout budget wraps a context deadline and exposes `Remaining()` after subtracting a reserved margin.
- `Allocate(fraction)` and `AllocateFixed(d)` derive child contexts that cannot exceed the parent deadline.
- Always call the child cancel function; leaked timers accumulate under load.
- Reserve 50-150 ms for response assembly so the handler always has time to write its response.
- Check `HasTimeFor` before low-priority operations; skip and record the reason rather than timing out.
- Propagate the remaining budget in a header (`X-Timeout-Budget-Ms`) so downstream services can budget their own work.

## What's Next

Next: [Connection Pool Health Monitoring](../12-connection-pool-health-monitoring/12-connection-pool-health-monitoring.md).

## Resources

- [pkg.go.dev/context](https://pkg.go.dev/context) - canonical package documentation: `WithDeadline`, `WithTimeout`, `Deadline()`, `DeadlineExceeded`
- [go.dev/blog/context](https://go.dev/blog/context) - the original context design post; explains deadline propagation across API boundaries
- [pkg.go.dev/net/http#NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext) - attaching a context to an outgoing HTTP request
- [Google SRE Book: Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/) - budget-based deadline propagation at scale
- [gRPC Deadlines Guide](https://grpc.io/docs/guides/deadlines/) - cross-service deadline propagation conventions
