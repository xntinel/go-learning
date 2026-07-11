# Exercise 20: Hard Cancellation on Deadline Exceeded

`defer cancel()` right after `context.WithCancel` is a well-known idiom, but
on its own it only closes the `Done()` channel — it does not wait for the
goroutines watching that channel to actually stop. This exercise builds an
operation that fans work out to several goroutines and treats a deferred
`cancel()` plus a `WaitGroup.Wait()` as a matched pair: the cancel is the
hard stop signal, the wait is the guarantee that every worker has honored it
before the function hands back control.

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

## What you'll build

```text
harddeadline/                    independent module: example.com/harddeadline
  go.mod
  harddeadline.go                 Result; RunAll (deferred hard cancel + wait, named results/err)
  cmd/demo/
    main.go                       runnable demo: a normal run and an already-exceeded deadline
  harddeadline_test.go             normal completion, hard cancel on exceeded deadline, ordering
```

- Files: `harddeadline.go`, `cmd/demo/main.go`, `harddeadline_test.go`.
- Implement: `RunAll(parent context.Context, n int, work func(ctx context.Context, id int) bool) (results []Result, err error)` that derives a cancellable context, defers its cancel, waits for every worker, then reports `parent.Err()`.
- Test: a normal run where every worker completes; a run against an already-cancelled parent where every worker observes cancellation; a check that results are returned in worker-ID order.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/harddeadline/cmd/demo
cd ~/go-exercises/harddeadline
go mod init example.com/harddeadline
go mod edit -go=1.24
```

### Cancel is the signal, Wait is the guarantee

```go
ctx, cancel := context.WithCancel(parent)
defer cancel() // hard stop: guarantee every worker's ctx.Done() fires

...
wg.Wait() // wait for every worker to observe cancellation and exit

...
if parent.Err() != nil {
    err = parent.Err()
}
return
```

`defer cancel()` guarantees `ctx.Done()` closes on every exit from `RunAll`,
including a panic unwinding through it — that is the "hard" part: no worker
can be left believing the operation is still live once `RunAll` has returned
control. But `cancel()` alone does not make a goroutine stop; each worker
still has to notice `ctx.Done()` and return, and `RunAll` still has to wait
for that to happen. `wg.Wait()` before the return is what prevents `RunAll`
from handing back a `results` slice while a worker goroutine is still
running against a context that has already been torn down — the combination
of the two is the actual "no goroutine leak, no work after the deadline"
guarantee, not `cancel()` by itself.

To keep the test deterministic, the "deadline" here is an already-cancelled
parent context passed in by the caller — no real timer, no sleep, no flaky
race between a timeout and a goroutine's scheduling. The `select` in a
worker's `work` function against `ctx.Done()` is a guaranteed non-blocking
read the instant the parent is already cancelled, per the `context` package's
guarantee that a derived context's `Done()` channel is already closed if the
parent's already is.

Create `harddeadline.go`:

```go
package harddeadline

import (
	"context"
	"sort"
	"sync"
)

// Result records whether worker id completed its unit of work before the
// operation's context was cancelled.
type Result struct {
	ID   int
	Done bool
}

// RunAll launches n workers under a context derived from parent. If parent's
// deadline is exceeded (or it is cancelled for any other reason), every
// worker must observe that and stop — none may keep running after RunAll
// returns.
//
// A deferred cancel() is the hard stop: it fires on every exit path,
// including a panic unwinding through RunAll, so no worker can be left
// believing the operation is still live. Because cancel alone does not wait
// for goroutines to actually exit, wg.Wait() runs before the return so RunAll
// never hands back control while a worker is still using the (now cancelled)
// context — that combination, not cancel() alone, is what prevents a
// goroutine leak.
//
// err reports the parent context's error, if any, as a named result so the
// caller can distinguish "the deadline cut this short" from a clean run.
func RunAll(parent context.Context, n int, work func(ctx context.Context, id int) bool) (results []Result, err error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel() // hard stop: guarantee every worker's ctx.Done() fires

	var wg sync.WaitGroup
	var mu sync.Mutex
	collected := make([]Result, 0, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			done := work(ctx, id)
			mu.Lock()
			collected = append(collected, Result{ID: id, Done: done})
			mu.Unlock()
		}(i)
	}
	wg.Wait() // wait for every worker to observe cancellation and exit

	sort.Slice(collected, func(a, b int) bool { return collected[a].ID < collected[b].ID })
	results = collected

	if parent.Err() != nil {
		err = parent.Err()
	}
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/harddeadline"
)

func work(ctx context.Context, id int) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		return true
	}
}

func main() {
	results, err := harddeadline.RunAll(context.Background(), 4, work)
	fmt.Printf("normal run: err=%v results=%v\n", err, results)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate a deadline that has already been exceeded
	results, err = harddeadline.RunAll(ctx, 4, work)
	fmt.Printf("deadline exceeded: err=%v results=%v\n", err, results)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
normal run: err=<nil> results=[{0 true} {1 true} {2 true} {3 true}]
deadline exceeded: err=context canceled results=[{0 false} {1 false} {2 false} {3 false}]
```

### Tests

Create `harddeadline_test.go`:

```go
package harddeadline

import (
	"context"
	"errors"
	"testing"
)

func work(ctx context.Context, id int) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		return true
	}
}

func TestRunAllCompletesWithoutDeadline(t *testing.T) {
	t.Parallel()

	results, err := RunAll(context.Background(), 5, work)
	if err != nil {
		t.Fatalf("RunAll: unexpected error: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("len(results) = %d, want 5", len(results))
	}
	for _, r := range results {
		if !r.Done {
			t.Fatalf("result %+v: want Done=true when parent is never cancelled", r)
		}
	}
}

func TestRunAllHardCancelsOnDeadlineExceeded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate an already-exceeded deadline

	results, err := RunAll(ctx, 5, work)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(results) != 5 {
		t.Fatalf("len(results) = %d, want 5 (every worker still reports in)", len(results))
	}
	for _, r := range results {
		if r.Done {
			t.Fatalf("result %+v: want Done=false, ctx was already cancelled", r)
		}
	}
}

func TestRunAllResultsAreOrderedByID(t *testing.T) {
	t.Parallel()

	results, err := RunAll(context.Background(), 8, work)
	if err != nil {
		t.Fatalf("RunAll: unexpected error: %v", err)
	}
	for i, r := range results {
		if r.ID != i {
			t.Fatalf("results[%d].ID = %d, want %d (results must be sorted)", i, r.ID, i)
		}
	}
}
```

## Review

`RunAll` is correct when every worker's result comes back, sorted by ID, and
when a parent that was already cancelled leaves every worker reporting
`Done=false` while `err` reports `context.Canceled`. The named results
(`results`, `err`) exist so the deferred `cancel()` and the trailing
`parent.Err()` check both feed the same values the function ultimately
returns, keeping the "always cancel, always wait, always report the parent's
error" contract in one linear path. The mistake to avoid is treating
`defer cancel()` as sufficient on its own — without the preceding
`wg.Wait()`, `RunAll` could return while workers are still mutating
`collected`, which is exactly the kind of bug `-race` exists to catch.

## Resources

- [`context` package docs](https://pkg.go.dev/context)
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup)
- [Go Blog: Context](https://go.dev/blog/context)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-value-encoding-json-to-yaml-fallback.md](19-value-encoding-json-to-yaml-fallback.md) | Next: [21-batch-operation-success-position-log.md](21-batch-operation-success-position-log.md)
