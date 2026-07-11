# 20. Batch Processing with Partial Failure

In batch operations — sending emails, processing records, uploading files — items fail
independently. Stopping the entire batch on the first failure wastes all the work done
so far and leaves the caller without a useful summary. The right approach: process all
items, isolate failures per item, retry where allowed, abort early if failures exceed a
threshold, and return a full report.

```text
batch/
  go.mod
  internal/batch/batch.go
  internal/batch/batch_test.go
  cmd/demo/main.go
```

## Concepts

### Bounded Parallelism

Spawning one goroutine per item is fine for ten items; catastrophic for a million.
A semaphore channel — `sem := make(chan struct{}, concurrency)` — lets at most N goroutines
run simultaneously. Each goroutine acquires a slot before starting and releases it when done.
The for-loop blocks when the semaphore is full, providing natural backpressure.

### Per-Item Error Isolation

Each item gets its own result slot. One item's failure must not prevent other items from
running. The classic mistake is a shared error variable protected by a mutex where the
first failure short-circuits the loop. Instead, allocate `results := make([]ItemResult, len(items))`
and write `results[i]` from the goroutine responsible for item i. No mutex needed because
each goroutine writes a distinct slot.

### Failure Threshold and Abort

A failure threshold converts a rate into a stop signal. After each failure, compute
`failedCount / completedCount`. If that ratio exceeds the threshold, set an atomic flag
and let all subsequent goroutines skip their work. The flag is atomic because many
goroutines read and write it concurrently. Items skipped this way are marked as aborted,
distinct from items that failed after retries.

### Retry with Backoff

Transient failures (network hiccup, database connection drop) often resolve on the next
attempt. A simple exponential backoff — `time.Sleep(time.Duration(attempt*attempt) * baseDelay)` —
avoids hammering a recovering service. Cap the backoff to prevent multi-minute waits on the
last retry. Retries count toward the item's result so callers can decide whether the item
was worth retrying.

### Sentinel Errors

Returning a raw `fmt.Errorf("aborted")` string is fragile: callers must compare error text.
Use a package-level sentinel: `var ErrAborted = errors.New("batch: item aborted")`. Callers
check with `errors.Is(err, batch.ErrAborted)` which is refactoring-safe and works through
wrapped errors.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/batch/internal/batch ~/go-exercises/batch/cmd/demo
cd ~/go-exercises/batch
go mod init example.com/batch
```

### Exercise 1: Core Types and Sentinel Errors

Create `internal/batch/batch.go`:

```go
package batch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrAborted is returned for items that were skipped because the failure
// threshold was exceeded before they could be processed.
var ErrAborted = errors.New("batch: item aborted")

// ItemResult holds the outcome for a single batch item.
type ItemResult struct {
	ID      int
	Success bool
	Err     error
	Retries int
}

// Config controls batch processing behaviour.
type Config struct {
	// MaxConcurrency limits how many items are processed simultaneously.
	// Zero or negative values default to 1.
	MaxConcurrency int

	// MaxRetries is the number of additional attempts after the first failure.
	// Zero means no retries.
	MaxRetries int

	// FailureThreshold is the fraction of completed items that may fail before
	// remaining items are aborted. 0.0 means abort on the first failure; 1.0
	// means never abort. Values outside [0,1] are clamped.
	FailureThreshold float64

	// RetryBaseDelay is the base duration for exponential backoff between
	// retries. Zero disables sleeping between retries.
	RetryBaseDelay time.Duration
}

// Report summarises the outcome of a batch run.
type Report struct {
	Total     int
	Succeeded int
	Failed    int
	Aborted   int
	Results   []ItemResult
	Duration  time.Duration
}

// ProcessorFunc is the function called for each item. It receives the item ID
// and returns an error on failure.
type ProcessorFunc func(ctx context.Context, id int) error

// Process runs fn on each item in ids according to cfg. It returns a Report
// regardless of failures.
func Process(ctx context.Context, ids []int, cfg Config, fn ProcessorFunc) *Report {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	}
	if cfg.FailureThreshold < 0 {
		cfg.FailureThreshold = 0
	}
	if cfg.FailureThreshold > 1 {
		cfg.FailureThreshold = 1
	}

	start := time.Now()
	results := make([]ItemResult, len(ids))
	sem := make(chan struct{}, cfg.MaxConcurrency)

	var (
		wg        sync.WaitGroup
		succeeded atomic.Int64
		failed    atomic.Int64
		abortFlag atomic.Bool
	)

	for i, id := range ids {
		if abortFlag.Load() {
			results[i] = ItemResult{ID: id, Err: ErrAborted}
			continue
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(idx, itemID int) {
			defer wg.Done()
			defer func() { <-sem }()

			if abortFlag.Load() {
				results[idx] = ItemResult{ID: itemID, Err: ErrAborted}
				return
			}

			var lastErr error
			retries := 0

			for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
				if attempt > 0 {
					retries++
					if cfg.RetryBaseDelay > 0 {
						delay := time.Duration(attempt*attempt) * cfg.RetryBaseDelay
						select {
						case <-ctx.Done():
							results[idx] = ItemResult{ID: itemID, Err: ctx.Err(), Retries: retries}
							return
						case <-time.After(delay):
						}
					}
				}
				lastErr = fn(ctx, itemID)
				if lastErr == nil {
					results[idx] = ItemResult{ID: itemID, Success: true, Retries: retries}
					succeeded.Add(1)
					return
				}
			}

			results[idx] = ItemResult{ID: itemID, Err: lastErr, Retries: retries}
			f := failed.Add(1)
			total := succeeded.Load() + f
			if total > 0 && float64(f)/float64(total) > cfg.FailureThreshold {
				abortFlag.Store(true)
			}
		}(i, id)
	}

	wg.Wait()

	r := &Report{
		Total:    len(ids),
		Results:  results,
		Duration: time.Since(start),
	}
	for _, res := range results {
		switch {
		case res.Success:
			r.Succeeded++
		case errors.Is(res.Err, ErrAborted):
			r.Aborted++
		default:
			r.Failed++
		}
	}
	return r
}
```

### Exercise 2: Table-Driven Tests

Create `internal/batch/batch_test.go`:

```go
package batch_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"example.com/batch/internal/batch"
)

// failIDs returns a ProcessorFunc that fails for any ID in the set.
func failIDs(ids ...int) batch.ProcessorFunc {
	set := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return func(_ context.Context, id int) error {
		if _, bad := set[id]; bad {
			return errors.New("injected failure")
		}
		return nil
	}
}

func TestProcess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ids       []int
		cfg       batch.Config
		fn        batch.ProcessorFunc
		wantSucc  int
		wantFail  int
		wantAbort int
	}{
		{
			name:     "all succeed",
			ids:      []int{1, 2, 3, 4, 5},
			cfg:      batch.Config{MaxConcurrency: 3, FailureThreshold: 1.0},
			fn:       failIDs(),
			wantSucc: 5,
		},
		{
			name:      "one fails, below threshold",
			ids:       []int{1, 2, 3, 4, 5},
			cfg:       batch.Config{MaxConcurrency: 1, FailureThreshold: 0.5},
			fn:        failIDs(3),
			wantSucc:  4,
			wantFail:  1,
			wantAbort: 0,
		},
		{
			// concurrency=1, threshold=0.1: item 1 fails -> ratio 1/1=1.0 > 0.1,
			// abort flag set; items 2-10 are all aborted at the loop head.
			name:      "failures exceed threshold, items aborted",
			ids:       []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			cfg:       batch.Config{MaxConcurrency: 1, FailureThreshold: 0.1},
			fn:        failIDs(1, 2),
			wantFail:  1,
			wantAbort: 9,
		},
		{
			name:     "retry recovers item",
			ids:      []int{1},
			cfg:      batch.Config{MaxConcurrency: 1, MaxRetries: 3, FailureThreshold: 1.0},
			fn:       failIDs(),
			wantSucc: 1,
		},
		{
			name:     "context already cancelled",
			ids:      []int{1, 2, 3},
			cfg:      batch.Config{MaxConcurrency: 2, MaxRetries: 0, FailureThreshold: 1.0},
			fn:       failIDs(),
			wantSucc: 3,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := batch.Process(context.Background(), tc.ids, tc.cfg, tc.fn)
			if r.Succeeded != tc.wantSucc {
				t.Errorf("Succeeded = %d, want %d", r.Succeeded, tc.wantSucc)
			}
			if r.Failed != tc.wantFail {
				t.Errorf("Failed = %d, want %d", r.Failed, tc.wantFail)
			}
			if r.Aborted != tc.wantAbort {
				t.Errorf("Aborted = %d, want %d", r.Aborted, tc.wantAbort)
			}
			if r.Total != len(tc.ids) {
				t.Errorf("Total = %d, want %d", r.Total, len(tc.ids))
			}
		})
	}
}

func TestErrAbortedSentinel(t *testing.T) {
	t.Parallel()

	// With threshold 0.0 and concurrency 1, item 1 fails, then all remaining are aborted.
	ids := []int{1, 2, 3}
	cfg := batch.Config{MaxConcurrency: 1, MaxRetries: 0, FailureThreshold: 0.0}
	r := batch.Process(context.Background(), ids, cfg, failIDs(1))

	abortedCount := 0
	for _, res := range r.Results {
		if errors.Is(res.Err, batch.ErrAborted) {
			abortedCount++
		}
	}
	if abortedCount == 0 {
		t.Error("expected at least one result with ErrAborted, got none")
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	slow := func(c context.Context, _ int) error {
		select {
		case <-c.Done():
			return c.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}

	ids := []int{1}
	cfg := batch.Config{MaxConcurrency: 1, MaxRetries: 2, RetryBaseDelay: time.Millisecond, FailureThreshold: 1.0}
	r := batch.Process(ctx, ids, cfg, slow)
	if r.Succeeded != 0 {
		t.Errorf("expected no successes after context cancel, got %d", r.Succeeded)
	}
}

func ExampleProcess() {
	ids := []int{1, 2, 3}
	cfg := batch.Config{MaxConcurrency: 2, MaxRetries: 0, FailureThreshold: 1.0}
	fn := func(_ context.Context, id int) error {
		return nil // all succeed
	}
	r := batch.Process(context.Background(), ids, cfg, fn)
	if r.Succeeded == 3 && r.Failed == 0 && r.Aborted == 0 {
		fmt.Println("all items succeeded")
	}
	// Output:
	// all items succeeded
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/batch/internal/batch"
)

func main() {
	ids := make([]int, 20)
	for i := range ids {
		ids[i] = i + 1
	}

	// Items 3, 7, 11 fail; threshold 0.5 so we should complete most of the batch.
	badSet := map[int]bool{3: true, 7: true, 11: true}
	fn := func(_ context.Context, id int) error {
		if badSet[id] {
			return fmt.Errorf("item %d: injected failure", id)
		}
		return nil
	}

	cfg := batch.Config{
		MaxConcurrency:   4,
		MaxRetries:       1,
		FailureThreshold: 0.5,
	}

	r := batch.Process(context.Background(), ids, cfg, fn)
	fmt.Printf("total=%d succeeded=%d failed=%d aborted=%d\n",
		r.Total, r.Succeeded, r.Failed, r.Aborted)

	for _, res := range r.Results {
		if res.Err != nil && !errors.Is(res.Err, batch.ErrAborted) {
			fmt.Printf("  item %d failed: %v (retries=%d)\n", res.ID, res.Err, res.Retries)
		}
	}
}
```

## Common Mistakes

### Writing to a Shared Result Slice Under a Mutex

Wrong: protecting the entire `results` slice with a single mutex and appending to it.

What happens: every goroutine serialises on the mutex when recording its result, turning
a parallel operation into a sequential one. The append also invalidates any prior index.

Fix: allocate `results := make([]ItemResult, len(ids))` and write `results[i]` from the
goroutine at index `i`. Each goroutine owns a distinct slot; no mutex is needed for the write.

### Comparing Error Strings to Detect Aborted Items

Wrong: `if res.Err != nil && res.Err.Error() == "aborted"`.

What happens: the comparison breaks when the error is wrapped with `%w` or when the string
is changed during refactoring.

Fix: use a package-level sentinel `var ErrAborted = errors.New(...)` and check with
`errors.Is(res.Err, batch.ErrAborted)`.

### Spawning One Goroutine Per Item Without Bounding

Wrong: `for _, id := range ids { go process(id) }` with no semaphore.

What happens: one million items spawn one million goroutines. Memory usage spikes and the
scheduler thrashes. File descriptors, database connections, and sockets may be exhausted.

Fix: use a semaphore channel of size `MaxConcurrency`. Acquire before launching and release
on `defer`.

### Checking the Abort Flag Only at the Loop Head

Wrong: reading `abortFlag` only in the for-loop before launching a goroutine.

What happens: goroutines that were already launched before the flag was set continue to run
and incur full retry cost even when the batch has been aborted.

Fix: check `abortFlag.Load()` at the start of the goroutine body as well, before doing any
real work.

## Verification

From `~/go-exercises/batch`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector validates that concurrent result writes and atomic
counter updates are safe.

## Summary

- Allocate one result slot per item and write from the owning goroutine — no mutex needed.
- Use a semaphore channel to bound parallelism regardless of batch size.
- Track successes and failures with `atomic.Int64`; check the ratio after each failure.
- Use a sentinel error (`ErrAborted`) so callers can distinguish aborted items from failed items.
- Return a `Report` in all cases — even after abort or context cancellation.

## What's Next

Next: [Graceful Goroutine Draining](../21-graceful-goroutine-draining/21-graceful-goroutine-draining.md).

## Resources

- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines)
- [sync/atomic package](https://pkg.go.dev/sync/atomic)
- [Retry pattern (Azure Architecture Center)](https://learn.microsoft.com/en-us/azure/architecture/patterns/retry)
- [errors package](https://pkg.go.dev/errors)
