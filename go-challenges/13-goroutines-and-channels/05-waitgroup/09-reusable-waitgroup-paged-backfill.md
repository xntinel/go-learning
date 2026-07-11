# Exercise 9: Reusing One WaitGroup Across Sequential Parallel Batches (Paged Backfill)

A backfill migration rarely loads the whole table into memory — it processes records
page by page. Each page's records are handled in parallel, but the pages themselves run
strictly in sequence: page k+1 must not start until page k has fully drained. This is
the canonical case for *reusing* a single WaitGroup: it is legal to `Add` a new round,
but only after the previous `Wait` has returned. This module builds that backfill and
proves the pages never overlap.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
backfill/                  independent module: example.com/backfill
  go.mod                   go 1.25
  backfill.go              Backfill processes pages sequentially, records in parallel
  cmd/
    demo/
      main.go              runnable demo: 3 pages, report processed count
  backfill_test.go         each record once; pages strictly sequential; ctx cancel
```

- Files: `backfill.go`, `cmd/demo/main.go`, `backfill_test.go`.
- Implement: `Backfill(ctx, pages, process) (int, error)` — one WaitGroup reused per page, `wg.Wait()` fully draining each page before the next `Add`; check `ctx.Err()` between pages.
- Test: every record across all pages processed exactly once; pages execute strictly sequentially (recorded page indices are non-decreasing); a cancel-between-pages subtest asserting early stop with `ctx.Err()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backfill/cmd/demo
cd ~/go-exercises/backfill
go mod init example.com/backfill
go mod edit -go=1.25
```

### The reuse rule: fully drain each round before the next Add

A `WaitGroup` may be reused for a new round of work, but only once its counter is back
to zero *and* the previous `Wait` has returned. The memory model is explicit: beginning
a new `Add` cycle while a prior `Wait` is still in progress is a race on the counter's
reuse. Paged processing satisfies the rule naturally *if you keep the structure strict*:
launch all of page k's goroutines, `wg.Wait()` until they finish, and only then move to
the loop iteration that `Add`s page k+1. The `Wait` is the barrier between pages.

This gives the backfill a useful property for free: at most one page's worth of
goroutines exist at any instant. Memory and connection pressure are bounded by the page
size, not the table size, which is exactly why real backfills page. Contrast this with
starting page k+1's goroutines before page k drains — you would both violate the reuse
rule and blow past the intended concurrency bound.

Between pages the code checks `ctx.Err()`. If the context was cancelled while page k
ran, the loop returns before `Add`-ing page k+1, so a cancelled backfill stops at a page
boundary and reports `context.Canceled`. The records already processed stay processed;
the backfill is resumable from the next page.

Create `backfill.go`:

```go
package backfill

import (
	"context"
	"sync"
	"sync/atomic"
)

// Processor handles a single record. A non-nil error is counted as a failure but
// does not stop the backfill.
type Processor func(ctx context.Context, record int) error

// Backfill processes pages of records. Within a page, records run in parallel; the
// pages themselves run strictly in sequence, reusing one WaitGroup. It returns the
// number of records processed and ctx.Err() if it stopped early due to cancellation.
func Backfill(ctx context.Context, pages [][]int, process Processor) (int, error) {
	var wg sync.WaitGroup
	var processed atomic.Int64

	for _, page := range pages {
		if err := ctx.Err(); err != nil {
			return int(processed.Load()), err
		}
		for _, rec := range page {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = process(ctx, rec)
				processed.Add(1)
			}()
		}
		wg.Wait() // fully drain this page before reusing wg for the next
	}
	return int(processed.Load()), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/backfill"
)

func main() {
	pages := [][]int{
		{0, 1, 2},
		{3, 4, 5},
		{6, 7},
	}
	process := func(ctx context.Context, record int) error { return nil }

	n, err := backfill.Backfill(context.Background(), pages, process)
	fmt.Printf("processed=%d err=%v\n", n, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed=8 err=<nil>
```

### Tests

`TestBackfillProcessesEveryRecordOnce` asserts every record across all pages is
processed exactly once (a set with no duplicates and no misses).
`TestBackfillPagesAreSequential` records the page index each record belonged to, in
completion order, and asserts the sequence is non-decreasing — page 0's records all
complete before any page 1 record starts, which can only be true if `Wait` genuinely
barriers between pages. `TestBackfillCancelBetweenPages` cancels the context during the
first page and asserts the backfill stops at the page boundary, returning
`context.Canceled` with only the first page processed.

Create `backfill_test.go`:

```go
package backfill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestBackfillProcessesEveryRecordOnce(t *testing.T) {
	t.Parallel()

	pages := [][]int{{0, 1, 2}, {3, 4, 5, 6}, {7, 8}}

	var mu sync.Mutex
	seen := map[int]int{}
	process := func(ctx context.Context, record int) error {
		mu.Lock()
		seen[record]++
		mu.Unlock()
		return nil
	}

	n, err := Backfill(context.Background(), pages, process)
	if err != nil {
		t.Fatalf("Backfill err = %v, want nil", err)
	}
	if n != 9 {
		t.Fatalf("processed = %d, want 9", n)
	}
	for rec := range 9 {
		if seen[rec] != 1 {
			t.Fatalf("record %d processed %d times, want 1", rec, seen[rec])
		}
	}
}

func TestBackfillPagesAreSequential(t *testing.T) {
	t.Parallel()

	pages := [][]int{{0, 0, 0}, {1, 1, 1}, {2, 2, 2}} // value == page index

	var mu sync.Mutex
	var order []int
	process := func(ctx context.Context, pageIdx int) error {
		mu.Lock()
		order = append(order, pageIdx)
		mu.Unlock()
		return nil
	}

	if _, err := Backfill(context.Background(), pages, process); err != nil {
		t.Fatalf("Backfill err = %v, want nil", err)
	}
	// Within a page order is unspecified, but across pages it must be strict:
	// the recorded page indices must be non-decreasing.
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Fatalf("pages overlapped: order = %v (index %d < %d)", order, order[i], order[i-1])
		}
	}
}

func TestBackfillCancelBetweenPages(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	pages := [][]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}}

	var mu sync.Mutex
	var count int
	process := func(ctx context.Context, record int) error {
		mu.Lock()
		count++
		c := count
		mu.Unlock()
		if c == 3 { // cancel once the first page's work is done
			cancel()
		}
		return nil
	}

	n, err := Backfill(ctx, pages, process)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Backfill err = %v, want context.Canceled", err)
	}
	if n != 3 {
		t.Fatalf("processed = %d, want 3 (only the first page)", n)
	}
}

func ExampleBackfill() {
	pages := [][]int{{1}, {2}}
	process := func(ctx context.Context, record int) error { return nil }
	n, err := Backfill(context.Background(), pages, process)
	fmt.Println(n, err)
	// Output: 2 <nil>
}
```

## Review

The backfill is correct when every record is processed exactly once (no duplicate, no
miss), the recorded page indices are non-decreasing (pages never overlap), and a
mid-page cancellation stops the run at the next page boundary with `context.Canceled`
and only the completed pages counted. The sequential test is the interesting one: it
turns "pages run in order" into an assertion by recording completion order and checking
monotonicity, which holds only because `Wait` truly barriers each round.

The rule to internalize: one WaitGroup can serve many rounds, but each round must fully
drain — `Wait` must return — before you `Add` the next. Interleaving a new round's `Add`
with a still-running `Wait` is a reuse race. As long as the page loop keeps `Wait` as the
barrier, a single WaitGroup drives the entire backfill safely. Run `go test -race` to
confirm.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the reuse rule (counter back to zero, `Wait` returned).
- [Go memory model](https://go.dev/ref/mem) — why a new round must not race an in-progress `Wait`.
- [`context.Context`](https://pkg.go.dev/context#Context) — `Err` for the between-pages cancellation check.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-fan-out-fan-in-channel-close.md](08-fan-out-fan-in-channel-close.md) | Next: [10-panic-safe-worker-wg.md](10-panic-safe-worker-wg.md)
