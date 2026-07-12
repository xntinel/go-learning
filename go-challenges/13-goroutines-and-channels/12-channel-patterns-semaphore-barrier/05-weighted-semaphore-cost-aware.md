# Exercise 5: Weighted Semaphore for Heterogeneous Job Cost

A counting semaphore treats every job as equal, but real batches are not: a
500 MB image transform and a 1 KB JSON validation should not each count as "one".
When you must bound total *cost* rather than count — memory budget, CPU units,
API quota points — the tool is a weighted semaphore. This exercise uses
`golang.org/x/sync/semaphore.Weighted` to keep total in-flight weight under a
fixed budget.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It depends on `golang.org/x/sync`, which the gate fetches.

## What you'll build

```text
weighted/                   independent module: example.com/weighted
  go.mod                    go 1.26; requires golang.org/x/sync
  weighted.go               type Budget wrapping semaphore.Weighted; Run, TryRun
  cmd/
    demo/
      main.go               run jobs of mixed weight under a budget, print the within-budget invariant
  weighted_test.go          two heavy jobs cannot coexist; peak weight <= budget (-race)
```

- Files: `weighted.go`, `cmd/demo/main.go`, `weighted_test.go`.
- Implement: a `Budget` over `semaphore.Weighted` with `Run(ctx, weight, fn)` (blocking acquire of `weight`) and `TryRun(weight, fn)` (best-effort fast path), each releasing exactly the weight it acquired.
- Test: with a budget of 10 and jobs of weight `{6, 6, 3}`, the two 6-weight jobs cannot hold simultaneously while a 3 fits alongside a 6; the observed peak in-flight weight never exceeds 10; a cancelled context makes `Run` return `ctx.Err()` and acquire nothing.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/sync/semaphore
```

### Why weight, not count

A counting semaphore of size 8 lets eight jobs run no matter how big each is —
fine when jobs are uniform, dangerous when one job holds a 500 MB buffer and
another holds 1 KB. Eight of the big ones is 4 GB and an OOM. A weighted
semaphore instead has a total budget (say 10 units) and each `Acquire(ctx, n)`
takes `n` units, blocking until `n` are free. So a budget of 10 admits one
6-unit job and one 3-unit job together (9 ≤ 10) but never two 6-unit jobs (12 >
10). You size the budget to the resource (total memory, quota points) and weight
each job by its share.

`(*Weighted).Acquire(ctx, n)` blocks until `n` units are free or the context is
cancelled — on cancellation it returns `ctx.Err()` and acquires *nothing*, so the
budget is untouched. `(*Weighted).TryAcquire(n)` is the non-blocking form: it
takes `n` units if available and reports success, otherwise returns false without
waiting. `(*Weighted).Release(n)` returns `n` units. The balance rule is stricter
than for a counting semaphore: `Release(n)` must pass the *same* `n` that was
acquired. Over-release corrupts the budget upward (more concurrent weight than
intended); the library actually panics if you release more than is currently
held. Capture the weight in a variable and `defer b.sem.Release(w)` with that
variable so the two can never drift.

Create `weighted.go`:

```go
package weighted

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// Budget bounds the total in-flight weight of concurrent jobs, not their count.
// It wraps a weighted semaphore so callers work in units of cost.
type Budget struct {
	sem   *semaphore.Weighted
	total int64
}

// NewBudget returns a Budget admitting jobs whose combined weight is at most total.
func NewBudget(total int64) *Budget {
	return &Budget{sem: semaphore.NewWeighted(total), total: total}
}

// Total reports the configured weight budget.
func (b *Budget) Total() int64 { return b.total }

// Run acquires weight units of budget (blocking until available or ctx is
// cancelled), runs fn, then releases exactly that weight. If the context is
// cancelled while waiting, Run returns ctx.Err() and fn does not run.
func (b *Budget) Run(ctx context.Context, weight int64, fn func()) error {
	if err := b.sem.Acquire(ctx, weight); err != nil {
		return err
	}
	defer b.sem.Release(weight)
	fn()
	return nil
}

// TryRun runs fn only if weight units are immediately available. It reports
// whether fn ran; a false result means the budget was too full right now.
func (b *Budget) TryRun(weight int64, fn func()) bool {
	if !b.sem.TryAcquire(weight) {
		return false
	}
	defer b.sem.Release(weight)
	fn()
	return true
}
```

### The runnable demo

The demo runs a mix of heavy (weight 6) and light (weight 3) jobs against a
budget of 10, tracking the peak in-flight weight. Because 6+6 exceeds the budget,
the two heavy jobs are serialized while a light job may ride alongside one heavy
job. The exact peak depends on the scheduler (a light+heavy pairing reaches 9, a
light+light pairing only 6), so the demo prints the property that is *always*
true — the peak never exceeds the budget — rather than a specific number that
would vary run to run. The tests below assert this same invariant.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/weighted"
)

func main() {
	b := weighted.NewBudget(10)

	var inflight, peak atomic.Int64
	track := func(w int64, work func()) {
		cur := inflight.Add(w)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		work()
		inflight.Add(-w)
	}

	weights := []int64{6, 3, 6, 3}
	var wg sync.WaitGroup
	for _, w := range weights {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Run(context.Background(), w, func() {
				track(w, func() { time.Sleep(5 * time.Millisecond) })
			})
		}()
	}
	wg.Wait()

	fmt.Printf("budget=%d within_budget=%t\n",
		b.Total(), peak.Load() <= b.Total())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
budget=10 within_budget=true
```

### Tests

`TestTwoHeavyCannotCoexist` acquires weight 6 (holding it), then asserts a
`TryRun` of another weight 6 fails while a `TryRun` of weight 3 succeeds — proving
the budget arithmetic, not just a count. `TestPeakWeightWithinBudget` runs a mixed
batch under `-race` with an atomic peak-weight tracker and asserts the peak never
exceeds the budget. `TestRunRespectsCancelledContext` fills the budget, then calls
`Run` with an already-cancelled context and asserts it returns `context.Canceled`
and did not run `fn` — the acquire took nothing.

Create `weighted_test.go`:

```go
package weighted

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTwoHeavyCannotCoexist(t *testing.T) {
	t.Parallel()

	b := NewBudget(10)

	// Hold weight 6 by acquiring the underlying semaphore directly.
	if !b.sem.TryAcquire(6) {
		t.Fatal("first weight-6 acquire should succeed against budget 10")
	}
	defer b.sem.Release(6)

	// A second weight-6 job cannot fit (12 > 10)...
	if b.TryRun(6, func() {}) {
		t.Fatal("second weight-6 job should not fit alongside the first")
	}
	// ...but a weight-3 job fits alongside the held 6 (9 <= 10).
	ran := b.TryRun(3, func() {})
	if !ran {
		t.Fatal("weight-3 job should fit alongside a held weight-6 job")
	}
}

func TestPeakWeightWithinBudget(t *testing.T) {
	t.Parallel()

	const budget = 10
	b := NewBudget(budget)

	var inflight, peak atomic.Int64
	weights := []int64{6, 6, 3, 3, 6, 3}
	var wg sync.WaitGroup
	for _, w := range weights {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Run(context.Background(), w, func() {
				cur := inflight.Add(w)
				for {
					old := peak.Load()
					if cur <= old || peak.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				inflight.Add(-w)
			})
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > budget {
		t.Fatalf("peak in-flight weight = %d, want <= %d", got, budget)
	}
}

func TestRunRespectsCancelledContext(t *testing.T) {
	t.Parallel()

	b := NewBudget(10)
	if !b.sem.TryAcquire(10) { // fill the budget
		t.Fatal("failed to fill budget")
	}
	defer b.sem.Release(10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := false
	err := b.Run(ctx, 1, func() { ran = true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("fn ran despite cancelled context")
	}
}
```

## Review

The weighted semaphore is correct when the total in-flight weight never exceeds
the budget and every `Release` returns exactly the weight its `Acquire` took. The
`{6, 6, 3}` test is the intuition made concrete: two heavy jobs (12) exceed a
budget of 10, so they serialize, while a light job (3) rides alongside one heavy
job (9). The single most common corruption is a mismatched release — capturing
the weight in a variable and releasing that same variable via `defer` is the
discipline that prevents it. Cancellation matters as much as with the counting
semaphore: a `Run` blocked on a full budget must return `ctx.Err()` and take
nothing when the caller gives up. Run `-race` with the atomic peak-weight tracker
to prove the budget held.

## Resources

- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — `NewWeighted`, `Acquire`, `TryAcquire`, `Release`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounding concurrent work and honoring cancellation.
- [Go Memory Model](https://go.dev/ref/mem) — the synchronization guarantees behind acquire/release semantics.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-bounded-http-fanout-limiter.md](04-bounded-http-fanout-limiter.md) | Next: [06-errgroup-bounded-fanout.md](06-errgroup-bounded-fanout.md)
