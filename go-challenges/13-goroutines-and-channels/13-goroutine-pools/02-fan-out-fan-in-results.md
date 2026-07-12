# Exercise 2: Collect Per-Job Results And Errors From A Batch

A batch transform — normalize every row of an import, transcode every attachment,
validate every record — needs each output mapped back to the input it came from,
and each item allowed to fail independently. This exercise builds a processor that
fans a slice of inputs out to a fixed number of workers and fans results back in
as `[]Result{Index, Value, Err}`, preserving the input-index association so the
caller can correlate outputs to inputs.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
batch/                     independent module: example.com/batch
  go.mod                   go 1.25
  batch.go                 type Result; Process[T,R](ctx, inputs, workers, fn) []Result[R]
  cmd/
    demo/
      main.go              runnable demo: uppercase a batch, one input fails
  batch_test.go            index-correlation, per-item error, and no-leak tests, -race
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: a generic `Process` that runs a transform over every input across `workers` goroutines and returns one `Result` per input, each carrying the input index, the produced value, and any error.
- Test: every input yields exactly one result, results map back to the correct index, injected errors land on the right items while others succeed, and the workers all exit after the input channel drains.
- Verify: `go test -count=1 -race ./...`

### The index is the correlation key

When you fan a slice out to workers, the outputs come back in whatever order the
workers happen to finish — which is nondeterministic. If a `Result` carried only a
value, the caller could not tell which input produced it. So the unit of work
carries its index: a `job{index, value}` goes out on the jobs channel, and the
`Result{Index, Value, Err}` that comes back carries the same index. The caller can
then place each result at `results[r.Index]`, or sort by `Index`, and recover the
original order. This is the fan-out/fan-in pattern with an explicit correlation
key, and the key is the cheapest possible one: the position in the input slice.

There are two ways to reassemble the output without a data race. One is a results
channel that every worker sends on and a single collector goroutine (or the caller)
drains — serialization through the channel removes the race. The other is a
preallocated `[]Result` where each worker writes `out[job.index]` directly — no
lock needed because no two workers ever touch the same index. This exercise uses
the results-channel form because it composes naturally with `range` and a
`WaitGroup`, and because it makes the "exactly one result per input" property
obvious: the collector counts what it receives.

### Structure: dispatch, workers, collect

`Process` has three parts. First it launches `workers` goroutines, each ranging
over a shared `jobs` channel; for every job it calls the transform and sends a
`Result` on a shared `results` channel. Second it feeds every input (paired with
its index) onto `jobs` and closes `jobs`, so the workers' `range` loops end.
Third, a separate goroutine `wg.Wait()`s for the workers and then closes
`results`, which lets the caller's `range results` loop terminate cleanly. The
"close results after the workers finish, from a goroutine, so the caller can range"
shape is the canonical fan-in, and getting the close ordering right is what keeps
it from deadlocking or panicking.

The transform is `func(ctx, T) (R, error)`. Passing `ctx` in means a long batch
can be cancelled — a worker checks `ctx.Err()` before starting each job and bails
out early — though on the happy path it runs everything. Each item's error is
captured into its `Result.Err` rather than aborting the batch: a batch transform
usually wants "here is what succeeded and here is what failed," not "the first
failure hid all the rest." (When you *do* want first-error-cancels-all, that is
`errgroup`, the next exercise.)

Create `batch.go`:

```go
package batch

import (
	"context"
	"sync"
)

// Result pairs a transform's output with the index of the input that produced
// it, so callers can correlate outputs back to inputs. Err is non-nil if the
// transform failed for that input.
type Result[R any] struct {
	Index int
	Value R
	Err   error
}

type job[T any] struct {
	index int
	value T
}

// Process runs fn over every input across workers goroutines and returns one
// Result per input. Results arrive in completion order; each carries its input
// index. If ctx is cancelled, not-yet-started items report ctx.Err().
func Process[T, R any](ctx context.Context, inputs []T, workers int, fn func(context.Context, T) (R, error)) []Result[R] {
	jobs := make(chan job[T])
	results := make(chan Result[R])

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := ctx.Err(); err != nil {
					results <- Result[R]{Index: j.index, Err: err}
					continue
				}
				v, err := fn(ctx, j.value)
				results <- Result[R]{Index: j.index, Value: v, Err: err}
			}
		}()
	}

	go func() {
		for i, in := range inputs {
			jobs <- job[T]{index: i, value: in}
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]Result[R], 0, len(inputs))
	for r := range results {
		out = append(out, r)
	}
	return out
}
```

The dispatcher runs in its own goroutine so that feeding `jobs` and draining
`results` happen concurrently: with unbuffered channels, if `Process` fed all
jobs before reading any results, a worker trying to send a result would block, the
dispatcher would block on a full jobs channel, and the whole thing would deadlock.
Concurrent dispatch and collection is what keeps the pipeline flowing.

### The runnable demo

The demo uppercases a batch of strings, with a transform that rejects the empty
string, then sorts the results by index and prints each. Because results arrive
out of order, the sort restores input order for display.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"example.com/batch"
)

func main() {
	inputs := []string{"alpha", "", "gamma"}

	results := batch.Process(context.Background(), inputs, 2,
		func(_ context.Context, s string) (string, error) {
			if s == "" {
				return "", errors.New("empty input")
			}
			return strings.ToUpper(s), nil
		})

	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("[%d] error: %v\n", r.Index, r.Err)
			continue
		}
		fmt.Printf("[%d] %s\n", r.Index, r.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[0] ALPHA
[1] error: empty input
[2] GAMMA
```

### Tests

`TestEveryInputYieldsOneResult` runs a transform over 50 inputs and asserts
exactly 50 results come back, each index present exactly once — the completeness
property. `TestResultsMapToCorrectIndex` uses a transform whose output encodes its
input, sorts by index, and asserts each slot holds the value derived from that
index — the correlation property. `TestPerItemErrors` fails the odd indices and
asserts those results carry the sentinel error via `errors.Is` while the even ones
succeed. `TestNoGoroutineLeak` runs `Process` and, after it returns, samples
`runtime.NumGoroutine` to confirm the workers exited (the input channel drained and
the workers' `range` loops ended).

Create `batch_test.go`:

```go
package batch

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"testing"
	"time"
)

var errOdd = errors.New("odd index rejected")

func TestEveryInputYieldsOneResult(t *testing.T) {
	t.Parallel()

	inputs := make([]int, 50)
	for i := range inputs {
		inputs[i] = i
	}
	results := Process(context.Background(), inputs, 8,
		func(_ context.Context, n int) (int, error) { return n * n, nil })

	if len(results) != 50 {
		t.Fatalf("got %d results, want 50", len(results))
	}
	seen := make(map[int]bool)
	for _, r := range results {
		if seen[r.Index] {
			t.Fatalf("index %d appeared twice", r.Index)
		}
		seen[r.Index] = true
	}
	if len(seen) != 50 {
		t.Fatalf("distinct indices = %d, want 50", len(seen))
	}
}

func TestResultsMapToCorrectIndex(t *testing.T) {
	t.Parallel()

	inputs := []int{10, 20, 30, 40}
	results := Process(context.Background(), inputs, 3,
		func(_ context.Context, n int) (int, error) { return n + 1, nil })

	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("index %d: unexpected error %v", i, r.Err)
		}
		if want := inputs[i] + 1; r.Value != want {
			t.Fatalf("index %d: value = %d, want %d", i, r.Value, want)
		}
	}
}

func TestPerItemErrors(t *testing.T) {
	t.Parallel()

	inputs := []int{0, 1, 2, 3, 4}
	results := Process(context.Background(), inputs, 4,
		func(_ context.Context, n int) (int, error) {
			if n%2 == 1 {
				return 0, fmt.Errorf("value %d: %w", n, errOdd)
			}
			return n, nil
		})

	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	for i, r := range results {
		if i%2 == 1 {
			if !errors.Is(r.Err, errOdd) {
				t.Fatalf("index %d: err = %v, want errOdd", i, r.Err)
			}
			continue
		}
		if r.Err != nil {
			t.Fatalf("index %d: unexpected error %v", i, r.Err)
		}
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	inputs := []int{1, 2, 3, 4, 5}
	_ = Process(context.Background(), inputs, 4,
		func(_ context.Context, n int) (int, error) { return n, nil })

	time.Sleep(20 * time.Millisecond) // let any stragglers exit
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutines: before=%d after=%d (leak?)", before, after)
	}
}

func ExampleProcess() {
	results := Process(context.Background(), []int{2, 3}, 2,
		func(_ context.Context, n int) (int, error) { return n * 10, nil })
	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	for _, r := range results {
		fmt.Printf("%d=%d\n", r.Index, r.Value)
	}
	// Output:
	// 0=20
	// 1=30
}
```

## Review

The processor is correct when two properties hold: completeness, every input
produces exactly one result (the collector receives `len(inputs)` results because
every job sends exactly one), and correlation, each result's `Index` identifies
the input that produced it. `TestEveryInputYieldsOneResult` and
`TestResultsMapToCorrectIndex` check these; `TestPerItemErrors` checks that a
failing item degrades to a per-item error via `errors.Is` rather than aborting the
batch.

The failure modes are all about channel-close ordering. `close(jobs)` must happen
after the last input is dispatched, or the workers' `range` never ends. `close(results)`
must happen after every worker has finished, which is why a dedicated goroutine
`wg.Wait()`s before closing it — closing `results` from a worker would race the
others and could close a channel another worker is about to send on. And dispatch
must run concurrently with collection, or unbuffered sends deadlock. `TestNoGoroutineLeak`
confirms the workers actually exit; run everything under `-race` to confirm the
concurrent sends on `results` and the shared reads are clean.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out/fan-in pattern and closing the results channel from a waiter goroutine.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — ranging over a channel and close semantics.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — asserting a wrapped sentinel in the per-item error test.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-bounded-worker-pool.md](01-bounded-worker-pool.md) | Next: [03-errgroup-bounded-enrichment.md](03-errgroup-bounded-enrichment.md)
