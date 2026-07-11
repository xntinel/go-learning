# Exercise 5: Bounded Fan-Out With errgroup — First-Error Cancellation And Concurrency Limit

Hand-rolling `WaitGroup` + `CancelCause` for every fan-out gets repetitive, and it
is easy to forget the bound that keeps concurrency from exhausting a connection
pool. `golang.org/x/sync/errgroup` packages both: `WithContext` gives a context
cancelled on the first error, and `SetLimit(n)` caps concurrent goroutines. This
module re-implements bounded enrichment — call a rate-limited API for each of N
records, at most `n` in flight, aborting all on the first hard failure — and
contrasts it with the `CancelCause` version.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports `golang.org/x/sync/errgroup`.

## What you'll build

```text
enrich/                      module example.com/enrich
  go.mod                     requires golang.org/x/sync
  enrich.go                  func Enrich(ctx, ids, limit, call) ([]Result, error); peak gauge
  cmd/
    demo/
      main.go                enriches 8 ids with limit 3, prints peak concurrency
  enrich_test.go             happy path, first-error aborts siblings, limit enforced, TryGo
```

Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
Implement: `Enrich(ctx, ids, limit, call) ([]Result, error)` over
`errgroup.WithContext` + `SetLimit(limit)`, returning the first error and
cancelling siblings, with a concurrency gauge that never exceeds `limit`.
Test: all results on success, an injected failure makes `Wait` return that error
(`errors.Is`) and cancels siblings, `SetLimit` enforced by a peak-concurrency
gauge, `TryGo` returns false at the limit.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/enrich/cmd/demo
cd ~/go-exercises/enrich
go mod init example.com/enrich
go get golang.org/x/sync/errgroup
```

### What errgroup gives you, and what it does not

`g, gctx := errgroup.WithContext(ctx)` returns a group and a derived context.
Every function you launch with `g.Go(func() error { ... })` runs concurrently; the
*first* one to return a non-nil error causes `gctx` to be cancelled, and
`g.Wait()` returns that first error after all launched functions have finished. So
a fan-out where any unit's hard failure should abort the rest is `errgroup`'s exact
use case: each unit watches `gctx.Done()`, so when one fails the others stop.

`g.SetLimit(n)` bounds the number of concurrently active functions. With a limit
set, `g.Go` *blocks* until a slot is free before starting the function — so a
fan-out over 10 000 IDs with `SetLimit(8)` never has more than 8 calls in flight,
no matter how fast you feed it. That is the bound that protects a connection pool
or a downstream quota. `g.TryGo(f)` is the non-blocking variant: it starts `f` only
if a slot is free right now, returning `true` if it did and `false` if the group is
at its limit — useful when you want to shed rather than queue.

The limitation to keep honest: `Wait` returns exactly one error — the first. If two
units fail, the second error is lost, and `Wait`'s error does not tell you *which*
unit failed. When the caller needs a typed reason or the failing unit's identity,
either record per-unit errors in a slice you own, or feed the error into a shared
`context.CancelCauseFunc` and read `context.Cause`. `errgroup` is the right default
for fail-fast; `CancelCause` is what you add when the reason must be data.

Here `Enrich` writes each result into a pre-sized slice at its own index (so no
mutex is needed for the results — each goroutine owns one slot), and tracks a peak
concurrency gauge with an atomic to prove the limit holds.

Create `enrich.go`:

```go
package enrich

import (
	"context"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
)

// Result is one enriched record.
type Result struct {
	ID    int
	Value int
}

// Enrich runs call for every id with at most limit concurrent calls. It returns
// the results in id order and the first error; on any error the shared context is
// cancelled so in-flight calls can abort. peak, if non-nil, receives the observed
// maximum concurrency.
func Enrich(
	ctx context.Context,
	ids []int,
	limit int,
	call func(ctx context.Context, id int) (int, error),
	peak *int64,
) ([]Result, error) {
	results := make([]Result, len(ids))
	var inFlight atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for i, id := range ids {
		g.Go(func() error {
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			if peak != nil {
				for {
					old := atomic.LoadInt64(peak)
					if cur <= old || atomic.CompareAndSwapInt64(peak, old, cur) {
						break
					}
				}
			}
			v, err := call(gctx, id)
			if err != nil {
				return err
			}
			results[i] = Result{ID: id, Value: v}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
```

### The runnable demo

The demo enriches eight IDs with a limit of three, where each call sleeps briefly
so several overlap, and prints the peak concurrency the gauge observed — which must
not exceed the limit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/enrich"
)

func main() {
	ids := []int{1, 2, 3, 4, 5, 6, 7, 8}
	var peak int64

	call := func(ctx context.Context, id int) (int, error) {
		select {
		case <-time.After(5 * time.Millisecond):
			return id * 10, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	results, err := enrich.Enrich(context.Background(), ids, 3, call, &peak)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	sum := 0
	for _, r := range results {
		sum += r.Value
	}
	fmt.Printf("enriched=%d sum=%d peak<=3=%v\n", len(results), sum, peak <= 3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enriched=8 sum=360 peak<=3=true
```

### Tests

`TestEnrichHappyPath` asserts all results come back in order. `TestFirstErrorAborts`
injects a failure on one ID and asserts `Wait` returns that specific error
(`errors.Is`) and that siblings saw the cancellation (a counter of `ctx.Done`
observers). `TestLimitEnforced` runs many overlapping calls and asserts the peak
gauge never exceeds the limit. `TestTryGoAtLimit` fills a limited group and asserts
`TryGo` returns false.

Create `enrich_test.go`:

```go
package enrich

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

var errInjected = errors.New("enrich: injected failure")

func TestEnrichHappyPath(t *testing.T) {
	t.Parallel()

	ids := []int{1, 2, 3, 4}
	call := func(ctx context.Context, id int) (int, error) { return id * 2, nil }

	got, err := Enrich(context.Background(), ids, 2, call, nil)
	if err != nil {
		t.Fatalf("Enrich err = %v, want nil", err)
	}
	want := []Result{{1, 2}, {2, 4}, {3, 6}, {4, 8}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFirstErrorAborts(t *testing.T) {
	t.Parallel()

	ids := []int{1, 2, 3, 4, 5, 6, 7, 8}
	var cancelled atomic.Int64

	call := func(ctx context.Context, id int) (int, error) {
		if id == 3 {
			return 0, errInjected
		}
		// Others block until the shared context is cancelled by the failure.
		select {
		case <-ctx.Done():
			cancelled.Add(1)
			return 0, ctx.Err()
		case <-time.After(time.Second):
			return id, nil
		}
	}

	_, err := Enrich(context.Background(), ids, len(ids), call, nil)
	if !errors.Is(err, errInjected) {
		t.Fatalf("Enrich err = %v, want errInjected", err)
	}
	if cancelled.Load() == 0 {
		t.Fatal("no sibling observed cancellation after the first error")
	}
}

func TestLimitEnforced(t *testing.T) {
	t.Parallel()

	ids := make([]int, 50)
	for i := range ids {
		ids[i] = i
	}
	var peak int64
	call := func(ctx context.Context, id int) (int, error) {
		time.Sleep(2 * time.Millisecond) // hold the slot so overlap is real
		return id, nil
	}

	_, err := Enrich(context.Background(), ids, 4, call, &peak)
	if err != nil {
		t.Fatalf("Enrich err = %v, want nil", err)
	}
	if peak > 4 {
		t.Fatalf("peak concurrency = %d, want <= 4", peak)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency = %d, want real overlap (>= 2)", peak)
	}
}

func TestTryGoAtLimit(t *testing.T) {
	t.Parallel()

	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(1)

	var mu sync.Mutex
	mu.Lock()
	// Occupy the single slot with a function that blocks until we unlock.
	started := g.TryGo(func() error {
		mu.Lock()
		mu.Unlock()
		return nil
	})
	if !started {
		t.Fatal("first TryGo returned false with a free slot")
	}
	if g.TryGo(func() error { return nil }) {
		t.Fatal("second TryGo returned true while at the limit")
	}
	mu.Unlock()
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait err = %v, want nil", err)
	}
}
```

## Review

The bounded fan-out is correct when it returns all results on success, returns the
first error (and cancels siblings) on failure, and never exceeds `SetLimit`. The
peak-gauge test is the resource-safety proof — it is what a code reviewer would ask
for to trust the limit protects the downstream. Remember `Wait` collapses failures
to the first error; if the demo needed a typed reason it would pair errgroup with
`context.Cause`, exactly as Exercise 1's `Transform` does with `CancelCause`. Writing
each result into its own slice index avoids a shared-map mutex, so `-race` stays
clean; the atomic peak gauge is the only shared mutable state and uses a CAS loop.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `TryGo`, `SetLimit`, `Wait`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the hand-rolled version errgroup replaces.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — pairing errgroup with a typed reason when `Wait`'s single error is not enough.

---

Back to [04-fan-in-merge-sources.md](04-fan-in-merge-sources.md) | Next: [06-batching-flush-stage.md](06-batching-flush-stage.md)
