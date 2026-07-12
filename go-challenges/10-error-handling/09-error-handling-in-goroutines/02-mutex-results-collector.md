# Exercise 2: Collect Every Outcome with a Mutex-Protected Slice

A reconciliation job does not want the first error — it wants the full ledger:
which of the ten thousand rows synced, which failed, and why, so the report is
complete and the next run can retry exactly the failures. That is the collect-all
pattern in its purest form, and the natural place to record a per-item outcome
from many goroutines is a slice guarded by a `sync.Mutex`. This module builds that
collector and proves, under `-race`, that the mutex is load-bearing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
collector/                   independent module: example.com/collector
  go.mod                     go 1.26
  collector.go               Job, Result; RunWithResults (mutex-protected append)
  cmd/
    demo/
      main.go                runnable demo: run a mixed batch, print per-job outcomes
  collector_test.go          tests: len == jobs, exact failure count, -race safety
```

Files: `collector.go`, `cmd/demo/main.go`, `collector_test.go`.
Implement: `RunWithResults(ctx, jobs)` that runs every job concurrently and appends a `Result{Job, Err}` under a `sync.Mutex`, returning the complete slice.
Test: `len(results) == len(jobs)`; exactly the expected number of results carry a non-nil `Err`; the whole thing is clean under `go test -race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a mutex, and why it is not optional

`RunWithResults` runs all jobs concurrently, and each goroutine appends one
`Result` to a shared slice. `append` on a shared slice from multiple goroutines is
a textbook data race: two goroutines can read the same length, write to the same
backing-array slot, and one write is lost — or the slice header is updated
non-atomically and the slice is corrupted outright. The `sync.Mutex` serializes
the append so exactly `len(jobs)` results land, one per job. This is not
belt-and-suspenders; remove the `Lock`/`Unlock` and the code fails `go test -race`
and, worse, silently drops results in production under load.

Contrast this with the fail-fast runner of the previous exercise. There, the first
error cancels the rest and the aggregate is a single joined error. Here, nothing
cancels: every job runs to completion and every outcome is recorded, because a
reconciliation report that stopped at the first failure would be useless — you
need to know *all* the rows that failed, not just the first. The choice between
these two runners is the choice between "all must succeed" and "report every
outcome", and it is the single most important decision in concurrent error
handling.

`Result` carries the job name alongside its error (nil for success), so the caller
gets a labeled per-job ledger. The order of results is scheduler-dependent — the
mutex guarantees safety, not ordering — so callers that need a stable order sort
by `Job` afterward. The tests therefore assert on counts and membership, never on
position.

Create `collector.go`:

```go
package collector

import (
	"context"
	"sync"
)

// Job is a named unit of work.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Result is one job's outcome. Err is nil on success.
type Result struct {
	Job string
	Err error
}

// RunWithResults runs every job concurrently and returns one Result per job,
// success and failure alike. The append is serialized by a mutex, so exactly
// len(jobs) results are recorded with no data race. Result order is not
// specified; sort by Job if a stable order is needed.
func RunWithResults(ctx context.Context, jobs []Job) []Result {
	var (
		mu      sync.Mutex
		results = make([]Result, 0, len(jobs))
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Go(func() {
			err := j.Run(ctx)
			mu.Lock()
			results = append(results, Result{Job: j.Name, Err: err})
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}
```

### The runnable demo

The demo runs a five-job batch where two jobs fail, then prints a sorted ledger so
the output is stable, plus a summary count. Sorting is the caller's job because the
collector does not promise order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"example.com/collector"
)

func main() {
	jobs := []collector.Job{
		{Name: "row-1", Run: func(ctx context.Context) error { return nil }},
		{Name: "row-2", Run: func(ctx context.Context) error { return errors.New("constraint violation") }},
		{Name: "row-3", Run: func(ctx context.Context) error { return nil }},
		{Name: "row-4", Run: func(ctx context.Context) error { return errors.New("deadlock, retry") }},
		{Name: "row-5", Run: func(ctx context.Context) error { return nil }},
	}

	results := collector.RunWithResults(context.Background(), jobs)
	sort.Slice(results, func(i, j int) bool { return results[i].Job < results[j].Job })

	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Printf("%s: FAIL: %v\n", r.Job, r.Err)
		} else {
			fmt.Printf("%s: ok\n", r.Job)
		}
	}
	fmt.Printf("processed %d, failed %d\n", len(results), failed)
}
```

Run it:

```bash
go run ./cmd/demo
```

The per-job lines are sorted, so the output is stable across runs. Expected
output:

```
row-1: ok
row-2: FAIL: constraint violation
row-3: ok
row-4: FAIL: deadlock, retry
row-5: ok
processed 5, failed 2
```

### Tests

`TestCollectsEveryOutcome` proves the two invariants that make a report
trustworthy: exactly `len(jobs)` results come back (no lost append), and exactly
the expected number carry a non-nil error. `TestConcurrentAppendIsRaceFree` fans
out a hundred jobs so `go test -race` has real concurrency to inspect; if the
mutex were removed, this test would fail the race detector in CI, which is how you
demonstrate the mutex is doing real work. The tests key results by job name into a
map rather than asserting on slice position, because order is not guaranteed.

Create `collector_test.go`:

```go
package collector

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

var errBoom = errors.New("boom")

func TestCollectsEveryOutcome(t *testing.T) {
	t.Parallel()
	jobs := []Job{
		{Name: "a", Run: func(ctx context.Context) error { return nil }},
		{Name: "b", Run: func(ctx context.Context) error { return errBoom }},
		{Name: "c", Run: func(ctx context.Context) error { return nil }},
		{Name: "d", Run: func(ctx context.Context) error { return errBoom }},
	}
	results := RunWithResults(context.Background(), jobs)

	if len(results) != len(jobs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(jobs))
	}

	byName := make(map[string]error, len(results))
	for _, r := range results {
		byName[r.Job] = r.Err
	}
	failed := 0
	for _, err := range byName {
		if err != nil {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("failed count = %d, want 2", failed)
	}
	if _, ok := byName["b"]; !ok {
		t.Fatal("result for job b missing")
	}
}

func TestConcurrentAppendIsRaceFree(t *testing.T) {
	t.Parallel()
	jobs := make([]Job, 100)
	for i := range jobs {
		name := fmt.Sprintf("job-%d", i)
		jobs[i] = Job{Name: name, Run: func(ctx context.Context) error { return nil }}
	}
	results := RunWithResults(context.Background(), jobs)
	if len(results) != 100 {
		t.Fatalf("len(results) = %d, want 100 (a lost append means the mutex failed)", len(results))
	}
}

func ExampleRunWithResults() {
	jobs := []Job{{Name: "only", Run: func(ctx context.Context) error { return nil }}}
	results := RunWithResults(context.Background(), jobs)
	fmt.Printf("%s err=%v\n", results[0].Job, results[0].Err)
	// Output: only err=<nil>
}
```

## Review

The collector is correct when it records exactly one outcome per job with no lost
writes, which the length assertion pins directly: a hundred jobs must yield a
hundred results, and any fewer means an `append` was lost to a race. The mutex is
the entire mechanism — its presence is what turns a data race into a serialized
append, and `go test -race` is the proof. The trap here is the temptation to
"optimize" by dropping the lock because the appends "look fast"; that is precisely
the silent-drop bug, invisible until production load makes two goroutines collide.
Note also what this runner deliberately does *not* do: it never cancels on
failure, because collect-all means every job runs. If you find yourself wanting to
stop early, you wanted the fail-fast runner instead. Run `go test -race` and
`go vet ./...` to confirm.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock that serializes the shared append.
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` catches an unguarded append.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 fan-out helper used here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-per-goroutine-panic-recovery.md](03-per-goroutine-panic-recovery.md)
