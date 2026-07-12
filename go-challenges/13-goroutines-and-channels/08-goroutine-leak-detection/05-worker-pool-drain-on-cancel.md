# Exercise 5: A Bounded Worker Pool That Drains On Cancel

A worker pool is a jobs-in / results-out fan-out with a fixed number of workers. It
leaks in two classic ways: a worker blocks on a result send when the collector has
stopped reading, or a worker blocks on a job receive because the producer forgot to
close the jobs channel. This exercise builds a pool that is correct on both fronts —
it closes the jobs channel to signal completion, propagates the context, and joins
every worker — and proves it under `go.uber.org/goleak`.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It imports `go.uber.org/goleak`.

## What you'll build

```text
workerpool/                  independent module: example.com/workerpool
  go.mod
  pool.go                    func Run(ctx, workers, jobs, fn) — bounded, cancellable
  cmd/
    demo/
      main.go                runnable demo: square a batch with a pool
  pool_test.go               happy path, early cancel, forgot-to-close leak, goleak
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Run` starting a feeder and `workers` worker goroutines over a jobs channel, joining all of them via `sync.WaitGroup.Go` (Go 1.25) before returning.
- Test: happy path (all jobs processed), early cancel (context cancelled mid-run, every worker exits, no leak), a reproduce-the-leak test for the forgot-to-close shape, all under goleak.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/05-worker-pool-drain-on-cancel/cmd/demo
cd go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/05-worker-pool-drain-on-cancel
go get go.uber.org/goleak@v1.3.0
```

### The three exit paths, and how they compose

`Run` starts `workers + 1` goroutines: one feeder and `workers` workers. Each has a
guaranteed exit:

- The feeder sends every job on `jobsCh` with a `select` on `ctx.Done()`, and
  `defer close(jobsCh)`. So it exits when the jobs run out *or* the context is
  cancelled, and either way it closes `jobsCh`.
- Each worker does `for j := range jobsCh` — a bounded range that ends when the
  feeder closes the channel. Its result send is also `select`-ed on `ctx.Done()`, so
  a worker never strands on a send even if the collector stopped reading.

A separate closer goroutine does `wg.Wait()` then `close(resultsCh)`; the caller
ranges `resultsCh` until that close. This is the crucial join: the pool does not
return until the feeder and every worker have returned, so there is no window where
`Run` has returned but a worker is still alive. That is what makes the pool
leak-free under cancellation, and it is what goleak checks.

The result channel is buffered with one slot per job, so in the common case workers
never block on the send at all; the `ctx.Done()` case on the send is the belt to the
buffer's braces, covering a cancelled run where the collector has already stopped.
Workers are launched with `sync.WaitGroup.Go` (Go 1.25), which does the `Add(1)`,
runs the function, and calls `Done()` on return — removing the `Add`/`Done`
boilerplate and the classic Add-after-`go` race.

Create `pool.go`:

```go
package workerpool

import (
	"context"
	"errors"
	"sync"
)

// ErrNoWorkers is returned when workers < 1.
var ErrNoWorkers = errors.New("workerpool: workers must be >= 1")

// WorkFunc processes one job and returns its result.
type WorkFunc func(ctx context.Context, job int) int

// Run processes jobs with `workers` goroutines, returning the results in
// arbitrary order. It joins the feeder and every worker before returning, so it
// leaks no goroutine even when ctx is cancelled mid-run; in that case it returns
// the results gathered so far and ctx.Err().
func Run(ctx context.Context, workers int, jobs []int, fn WorkFunc) ([]int, error) {
	if workers < 1 {
		return nil, ErrNoWorkers
	}

	jobsCh := make(chan int)
	resultsCh := make(chan int, len(jobs))

	var wg sync.WaitGroup

	// Feeder: hand out jobs, stop early if cancelled, always close jobsCh.
	wg.Go(func() {
		defer close(jobsCh)
		for _, j := range jobs {
			select {
			case jobsCh <- j:
			case <-ctx.Done():
				return
			}
		}
	})

	// Workers: bounded range over jobsCh; ctx-aware result send.
	for range workers {
		wg.Go(func() {
			for j := range jobsCh {
				r := fn(ctx, j)
				select {
				case resultsCh <- r:
				case <-ctx.Done():
					return
				}
			}
		})
	}

	// Closer: once feeder and workers are done, close results so the range ends.
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	out := make([]int, 0, len(jobs))
	for r := range resultsCh {
		out = append(out, r)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	"example.com/workerpool"
)

func main() {
	jobs := []int{1, 2, 3, 4, 5}
	square := func(ctx context.Context, j int) int { return j * j }

	out, err := workerpool.Run(context.Background(), 3, jobs, square)
	if err != nil {
		fmt.Println("run:", err)
		return
	}
	sort.Ints(out) // pool order is arbitrary; sort for a stable demo
	fmt.Println("squares:", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
squares: [1 4 9 16 25]
```

### The tests

`TestMain` installs `goleak.VerifyTestMain`, so any test that leaks a goroutine fails
the whole package. `TestHappyPath` checks every job is processed. `TestEarlyCancel`
cancels the context while workers are mid-flight and asserts `Run` returns
`context.Canceled` and — because `VerifyTestMain` is watching — that every worker and
the feeder exited. `TestForgotToCloseJobsLeaks` reproduces the "receive from a channel
that is never closed" shape inline, shows `goleak.Find` detecting the parked workers,
then closes the channel and joins them, so the reproduction is honest *and* clean.

Create `pool_test.go`:

```go
package workerpool

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestHappyPath(t *testing.T) {
	jobs := []int{1, 2, 3, 4, 5, 6}
	double := func(ctx context.Context, j int) int { return j * 2 }

	out, err := Run(context.Background(), 3, jobs, double)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	sort.Ints(out)
	want := []int{2, 4, 6, 8, 10, 12}
	if len(out) != len(want) {
		t.Fatalf("got %d results, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out = %v, want %v", out, want)
		}
	}
}

func TestNoWorkers(t *testing.T) {
	_, err := Run(context.Background(), 0, []int{1}, func(context.Context, int) int { return 0 })
	if !errors.Is(err, ErrNoWorkers) {
		t.Fatalf("Run error = %v, want ErrNoWorkers", err)
	}
}

func TestEarlyCancel(t *testing.T) {
	// Many slow jobs so cancellation lands mid-run.
	jobs := make([]int, 200)
	for i := range jobs {
		jobs[i] = i
	}
	slow := func(ctx context.Context, j int) int {
		time.Sleep(2 * time.Millisecond)
		return j
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := Run(ctx, 4, jobs, slow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	// goleak.VerifyTestMain confirms the feeder and all workers exited.
}

func TestForgotToCloseJobsLeaks(t *testing.T) {
	// Reproduce the "receive from a channel that is never closed" shape inline.
	ignore := goleak.IgnoreCurrent()

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for range jobs { // BUG in the wild: nobody closes jobs
			}
		})
	}
	jobs <- 1 // one job is consumed; the workers then park on receive

	if err := goleak.Find(ignore); err == nil {
		t.Fatal("expected the workers to leak on an unclosed jobs channel")
	}

	// Clean up: closing jobs ends the range, so every worker exits and joins.
	close(jobs)
	wg.Wait()
}
```

## Review

The pool is correct when `Run` cannot return while any goroutine it started is still
alive: the feeder always closes `jobsCh`, the workers always end their bounded range,
and the closer's `wg.Wait()` gates the final `close(resultsCh)`. `TestEarlyCancel`
under `VerifyTestMain` is the proof — cancel mid-run and nothing is left behind.
`TestForgotToCloseJobsLeaks` shows the opposite: omit the close and the workers park
forever on the receive.

The mistakes to avoid: never leave a `for range jobsCh` without a guaranteed close of
`jobsCh`, and never send a result on a channel the collector might stop reading
without either a buffer or a `ctx.Done()` case. Do not forget the join — signalling
cancellation is not the same as waiting for the workers to finish. And launch workers
with `wg.Go` rather than a bare `go` plus `Add`/`Done`, which removes a whole class of
`WaitGroup` misuse. Run under `-race` with more than one worker to catch coordination
bugs.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`, `Find`, `IgnoreCurrent`.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 method that launches and joins a goroutine.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — closing the jobs channel to signal completion, and draining on cancel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-fanout-timeout-send-leak.md](04-fanout-timeout-send-leak.md) | Next: [06-broker-subscriber-unsubscribe.md](06-broker-subscriber-unsubscribe.md)
