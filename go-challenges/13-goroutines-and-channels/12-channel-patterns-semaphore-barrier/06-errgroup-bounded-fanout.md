# Exercise 6: Bounded Parallel Enrichment with errgroup.SetLimit

Most hand-rolled fan-outs — a semaphore, a `WaitGroup`, and an error channel
stitched together — are reinventing `golang.org/x/sync/errgroup`. Its
`SetLimit(n)` *is* a semaphore, and on top of that it captures the first error
and cancels a shared context so siblings stop early. This exercise builds a
record-enrichment step: call an external service per record in parallel, capped
at `n` concurrent calls, with first-error propagation and cancellation for free.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It depends on `golang.org/x/sync`, which the gate fetches.

## What you'll build

```text
enrich/                     independent module: example.com/enrich
  go.mod                    go 1.26; requires golang.org/x/sync
  enrich.go                 type Record; EnrichAll(ctx, records, limit, enricher)
  cmd/
    demo/
      main.go               enrich a batch at a fixed limit, print peak concurrency
  enrich_test.go            peak<=limit; first error cancels siblings; TryGo (-race)
```

- Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
- Implement: `EnrichAll` using `errgroup.WithContext` + `Group.SetLimit(limit)`, filling a result slice, returning the first error and cancelling the shared context on failure.
- Test: enrich 30 records at `limit=5` and assert peak concurrency `<= 5` and no error; inject a failing record and assert `Wait` returns that error, the context is cancelled, and not-yet-started calls skip the enricher (fewer than 30 enricher invocations); assert `TryGo` returns false at the limit.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/sync/errgroup
```

### Why errgroup is the idiomatic bounded fan-out

`errgroup.WithContext(ctx)` returns a `*Group` and a derived context. The group's
`Go(func() error)` launches a function; `SetLimit(n)` caps how many run at once,
blocking `Go` when the group is at the limit until a slot frees — that is the
semaphore. What you get *in addition* is error handling: the first function to
return a non-nil error causes the derived context to be cancelled, and `Wait`
returns that first error after all launched functions finish. So a failed sibling
cancels the shared context, and every function that honors `ctx.Done()` stops
early instead of hammering a dependency that has already failed.

`TryGo(func() error) bool` attempts to launch without blocking: it returns false
if the group is already at its limit, letting you avoid parking the caller.
`SetLimit` must be called before any `Go`/`TryGo` (do not change the limit while
functions are active).

The design pattern for `EnrichAll`: derive the group and context, set the limit,
and for each record launch a function that first checks `ctx.Err()` — if the
context is already cancelled (because a sibling failed), the function skips the
actual enricher call and returns the error, so a failed batch stops calling the
downstream promptly. Results are written to per-index slots, so no shared write
races. `Wait` returns the first error (or nil).

Create `enrich.go`:

```go
package enrich

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Record is one input row plus its enriched output.
type Record struct {
	ID       string
	Enriched string
}

// Enricher fetches enrichment for a record's ID. It must honor ctx cancellation.
type Enricher func(ctx context.Context, id string) (string, error)

// EnrichAll enriches every record in parallel, at most limit calls at once. It
// returns the enriched records and the first enricher error, if any. On the
// first error the shared context is cancelled, so calls not yet started skip the
// enricher and return promptly.
func EnrichAll(ctx context.Context, records []Record, limit int, enricher Enricher) ([]Record, error) {
	out := make([]Record, len(records))
	copy(out, records)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)

	for i := range out {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err // a sibling already failed; skip the downstream call
			}
			v, err := enricher(ctx, out[i].ID)
			if err != nil {
				return err
			}
			out[i].Enriched = v
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return out, err
	}
	return out, nil
}
```

### The runnable demo

The demo enriches eight records at `limit=3` with a trivial enricher and reports
the peak concurrency observed, which the limit caps at 3.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"example.com/enrich"
)

func main() {
	records := make([]enrich.Record, 8)
	for i := range records {
		records[i] = enrich.Record{ID: fmt.Sprintf("r%d", i)}
	}

	var live, peak atomic.Int64
	enricher := func(ctx context.Context, id string) (string, error) {
		cur := live.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		live.Add(-1)
		return strings.ToUpper(id), nil
	}

	out, err := enrich.EnrichAll(context.Background(), records, 3, enricher)
	fmt.Printf("records=%d err=%v peak=%d\n", len(out), err, peak.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
records=8 err=<nil> peak=3
```

### Tests

`TestBoundsConcurrency` enriches 30 records at `limit=5` with an atomic peak
tracker and asserts the peak is `<= 5` and no error. `TestFirstErrorCancels`
injects a failing record (index 0), counts *actual* enricher invocations
separately from the launched functions, and asserts `Wait` returns the sentinel
error and that fewer than 30 enricher calls happened — because functions launched
after the cancellation see `ctx.Err()` and skip the call. `TestTryGoAtLimit`
proves `TryGo` returns false when the group is at its limit.

Create `enrich_test.go`:

```go
package enrich

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

var errEnrich = errors.New("enrich failed")

func makeRecords(n int) []Record {
	rs := make([]Record, n)
	for i := range rs {
		rs[i] = Record{ID: fmt.Sprintf("r%d", i)}
	}
	return rs
}

func TestBoundsConcurrency(t *testing.T) {
	t.Parallel()

	const n, limit = 30, 5
	var live, peak atomic.Int64
	enricher := func(ctx context.Context, id string) (string, error) {
		cur := live.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		live.Add(-1)
		return id + "!", nil
	}

	out, err := EnrichAll(context.Background(), makeRecords(n), limit, enricher)
	if err != nil {
		t.Fatalf("EnrichAll err = %v, want nil", err)
	}
	if len(out) != n {
		t.Fatalf("got %d records, want %d", len(out), n)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestFirstErrorCancels(t *testing.T) {
	t.Parallel()

	const n, limit = 30, 5
	var calls atomic.Int64
	enricher := func(ctx context.Context, id string) (string, error) {
		calls.Add(1)
		if id == "r0" {
			return "", errEnrich // fail fast, cancelling the shared context
		}
		time.Sleep(5 * time.Millisecond) // hold a slot so cancellation propagates
		return id + "!", nil
	}

	_, err := EnrichAll(context.Background(), makeRecords(n), limit, enricher)
	if !errors.Is(err, errEnrich) {
		t.Fatalf("EnrichAll err = %v, want errEnrich", err)
	}
	if got := calls.Load(); got >= n {
		t.Fatalf("enricher called %d times; expected fewer than %d after cancellation", got, n)
	}
}

func TestTryGoAtLimit(t *testing.T) {
	t.Parallel()

	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(1)

	started := make(chan struct{})
	release := make(chan struct{})
	g.Go(func() error {
		close(started)
		<-release
		return nil
	})
	<-started

	if g.TryGo(func() error { return nil }) {
		t.Fatal("TryGo should return false when the group is at its limit")
	}

	close(release)
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait err = %v, want nil", err)
	}
}
```

## Review

`errgroup.SetLimit` is the primitive to reach for before hand-rolling a bounded
fan-out: it is a semaphore that also propagates the first error and cancels the
derived context. The enrichment step proves both halves — the peak tracker shows
the limit held, and the failure test shows that a single failing record cancels
the context so later functions skip the downstream entirely, which is why the
enricher is called fewer than 30 times. The trap to avoid is writing `Go`
functions that ignore `ctx.Done()`: without the `ctx.Err()` check the functions
would keep calling a dependency that has already failed, wasting exactly the
requests cancellation exists to save. Always derive the context with
`errgroup.WithContext` and honor it inside every function. Run `-race` to confirm
the per-index writes and the peak tracker are clean.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `TryGo`, `SetLimit`, `Wait`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out-with-cancellation pattern errgroup packages.
- [Go Concurrency Patterns: Context](https://go.dev/blog/context) — how a shared context cancels sibling goroutines.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-weighted-semaphore-cost-aware.md](05-weighted-semaphore-cost-aware.md) | Next: [07-reusable-cyclic-barrier.md](07-reusable-cyclic-barrier.md)
