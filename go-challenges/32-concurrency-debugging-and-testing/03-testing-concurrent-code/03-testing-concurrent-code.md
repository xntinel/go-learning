# 3. Testing Concurrent Code

A test that passes 999 times and fails once is worse than a test that always fails: it trains you to dismiss failures and hides real bugs behind random luck. The culprit is almost always `time.Sleep` used as a synchronization mechanism. This lesson replaces sleep-based assertions with channel barriers, WaitGroups, and structured timeouts, then stress-tests with `-count=100 -race` to prove the tests are deterministic. The artifact is a concrete worker pool with a full test suite that exercises completion, concurrency, shutdown, and error propagation.

```text
workerpool/
  go.mod
  pool.go
  pool_test.go
  cmd/demo/
    main.go
```

## Concepts

### Why Sleep-Based Tests Are Wrong

`time.Sleep` is not a synchronization primitive. It is a guess about timing. On a loaded CI machine or a machine with a different scheduler, the sleep may expire before the goroutine it is waiting for has executed. The result is a test that is non-deterministic and machine-speed-dependent.

The correct model is: a goroutine signals when it has reached a state, and the test waits for that signal. The signal is a channel send, a `WaitGroup.Done`, or a context cancellation. These are determined by program state, not by wall-clock time.

### Channel Barriers for Proving Concurrency

A barrier is a synchronization point where all participants must arrive before any can continue. To prove that N workers run concurrently rather than sequentially, inject a barrier into the work function: each worker signals arrival, the test collects all N signals, then releases all workers. If fewer than N signals arrive within the deadline, not all workers were active simultaneously.

```
test             worker 1    worker 2    worker N
  |                 |           |           |
  |-submit job 1--->|           |           |
  |-submit job 2-------------->|           |
  |-submit job N---------------------------------->|
  |                 |           |           |
  |<-barrier signal-|           |           |
  |<-barrier signal------------|           |
  |<-barrier signal------------------------|
  | all N arrived; release them
  |-release-------->|---------->|---------->|
```

### Timeout Assertions

`time.After` is appropriate as a test deadline (preventing the test from hanging forever) but never as the primary assertion mechanism. The structure is always:

```go
select {
case <-signal:
    // success
case <-time.After(5 * time.Second):
    t.Fatalf("timeout: expected signal within 5s")
}
```

The 5-second deadline is long enough for any reasonable scheduler; the test passes in milliseconds when the code is correct.

### The Race Detector and Count Mode

`go test -race -count=100 ./...` runs each test 100 times back-to-back with the race detector active. This stresses goroutine scheduling and surfaces races that appear in fewer than 1% of runs. A test that passes `-count=100 -race` is a deterministic test.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/32-concurrency-debugging-and-testing/03-testing-concurrent-code/03-testing-concurrent-code/cmd/demo
cd go-solutions/32-concurrency-debugging-and-testing/03-testing-concurrent-code/03-testing-concurrent-code
```

### Exercise 1: Worker Pool

Create `pool.go`:

```go
package workerpool

import (
	"errors"
	"fmt"
	"sync"
)

// ErrPoolClosed is returned when Submit is called after Shutdown.
var ErrPoolClosed = errors.New("pool is closed")

// Job carries an identifier and an optional payload.
type Job struct {
	ID      int
	Payload string
}

// Result holds the output of a processed job.
type Result struct {
	JobID int
	Value string
	Err   error
}

// Pool is a fixed-size worker pool. Jobs are submitted via Submit and
// results are read from Results. Call Shutdown to drain and close.
type Pool struct {
	jobs    chan Job
	results chan Result
	wg      sync.WaitGroup
	once    sync.Once
	mu      sync.Mutex
	isDown  bool
}

// New creates a pool with the given number of workers. handler is called
// once per job; it must be safe to call concurrently.
func New(workers int, handler func(Job) Result) *Pool {
	p := &Pool{
		jobs:    make(chan Job, workers*2),
		results: make(chan Result, workers*2),
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for job := range p.jobs {
				p.results <- handler(job)
			}
		}()
	}
	return p
}

// Submit enqueues a job. Returns ErrPoolClosed if the pool has been shut down.
// Submit is safe to call concurrently. It must not be called after Shutdown
// returns (Shutdown closes p.jobs under p.mu; Submit holds p.mu across the
// send, so the two operations are mutually exclusive).
func (p *Pool) Submit(job Job) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isDown {
		return fmt.Errorf("submit job %d: %w", job.ID, ErrPoolClosed)
	}
	// The send is safe: p.mu is held, and Shutdown sets isDown and closes
	// p.jobs under the same mutex, so p.jobs cannot be closed while we hold
	// p.mu and isDown is false.
	p.jobs <- job
	return nil
}

// Results returns the channel from which callers read processed results.
// The channel is closed after Shutdown completes.
func (p *Pool) Results() <-chan Result {
	return p.results
}

// Shutdown stops accepting new jobs, waits for all workers to finish,
// and closes the Results channel. Safe to call multiple times.
func (p *Pool) Shutdown() {
	p.once.Do(func() {
		p.mu.Lock()
		p.isDown = true
		close(p.jobs) // closed under p.mu so Submit cannot send after close
		p.mu.Unlock()
		p.wg.Wait()
		close(p.results)
	})
}
```

### Exercise 2: Test Suite Without Sleep

Create `pool_test.go`:

```go
package workerpool

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitResult collects one result from the pool within 5 seconds.
func waitResult(t *testing.T, p *Pool) Result {
	t.Helper()
	select {
	case r, ok := <-p.Results():
		if !ok {
			t.Fatal("results channel closed unexpectedly")
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for result")
		return Result{}
	}
}

// TestAllJobsComplete verifies that every submitted job produces exactly one result.
// Results are drained concurrently with submission to avoid filling the buffer.
func TestAllJobsComplete(t *testing.T) {
	t.Parallel()

	const n = 50
	p := New(4, func(j Job) Result {
		return Result{JobID: j.ID, Value: fmt.Sprintf("done-%d", j.ID)}
	})

	// Drain results in a separate goroutine so workers are never blocked.
	seen := make(map[int]bool, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := range p.Results() {
			mu.Lock()
			if seen[r.JobID] {
				mu.Unlock()
				t.Errorf("duplicate result for job %d", r.JobID)
				continue
			}
			seen[r.JobID] = true
			mu.Unlock()
		}
	}()

	for i := 0; i < n; i++ {
		if err := p.Submit(Job{ID: i}); err != nil {
			t.Fatalf("Submit(%d): %v", i, err)
		}
	}

	p.Shutdown() // closes jobs channel, waits for workers, closes results channel
	wg.Wait()    // wait for drain goroutine to finish

	if len(seen) != n {
		t.Fatalf("got %d results, want %d", len(seen), n)
	}
}

// TestWorkersRunConcurrently proves workers are active simultaneously using a barrier.
func TestWorkersRunConcurrently(t *testing.T) {
	t.Parallel()

	const numWorkers = 4
	atBarrier := make(chan struct{}, numWorkers)
	release := make(chan struct{})

	p := New(numWorkers, func(_ Job) Result {
		atBarrier <- struct{}{} // signal arrival
		<-release               // wait for release
		return Result{}
	})

	for i := 0; i < numWorkers; i++ {
		if err := p.Submit(Job{ID: i}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	// Collect all arrival signals — no sleep, no guess.
	for i := 0; i < numWorkers; i++ {
		select {
		case <-atBarrier:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d workers reached the barrier", i, numWorkers)
		}
	}
	// All numWorkers goroutines are simultaneously blocked at the barrier.
	close(release)

	p.Shutdown()
	// Drain remaining results.
	for range p.Results() {
	}
}

// TestShutdownDrainsAll verifies Shutdown waits for all in-flight jobs.
// Results are drained concurrently to prevent the results buffer from filling.
func TestShutdownDrainsAll(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64
	p := New(4, func(j Job) Result {
		processed.Add(1)
		return Result{JobID: j.ID}
	})

	// Drain results concurrently.
	var drainWg sync.WaitGroup
	drainWg.Add(1)
	go func() {
		defer drainWg.Done()
		for range p.Results() {
		}
	}()

	const n = 100
	for i := 0; i < n; i++ {
		if err := p.Submit(Job{ID: i}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	p.Shutdown()
	drainWg.Wait()

	// After Shutdown returns and drain completes, all handlers have run.
	if got := processed.Load(); got != n {
		t.Fatalf("processed %d, want %d", got, n)
	}
}

// TestSubmitAfterShutdownReturnsError verifies the closed path.
func TestSubmitAfterShutdownReturnsError(t *testing.T) {
	t.Parallel()

	p := New(2, func(j Job) Result { return Result{JobID: j.ID} })
	p.Shutdown()

	err := p.Submit(Job{ID: 99})
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("err = %v, want ErrPoolClosed", err)
	}
}

// TestErrorPropagation verifies that handler errors are returned to the caller.
func TestErrorPropagation(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("handler error")
	p := New(2, func(j Job) Result {
		if j.ID < 0 {
			return Result{JobID: j.ID, Err: fmt.Errorf("negative id: %w", wantErr)}
		}
		return Result{JobID: j.ID}
	})

	if err := p.Submit(Job{ID: -1}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	p.Shutdown()

	var foundErr error
	for r := range p.Results() {
		if r.JobID == -1 {
			foundErr = r.Err
		}
	}
	if !errors.Is(foundErr, wantErr) {
		t.Fatalf("err = %v, want wantErr", foundErr)
	}
}

// TestConcurrentSubmitters verifies the pool handles many concurrent callers.
func TestConcurrentSubmitters(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64
	p := New(8, func(j Job) Result {
		processed.Add(1)
		return Result{JobID: j.ID}
	})

	// Drain results concurrently to prevent the results buffer from filling
	// and blocking workers (which would in turn block submitters).
	var drainWg sync.WaitGroup
	drainWg.Add(1)
	go func() {
		defer drainWg.Done()
		for range p.Results() {
		}
	}()

	const submitters = 20
	const jobsPerSubmitter = 10
	var wg sync.WaitGroup

	for s := 0; s < submitters; s++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < jobsPerSubmitter; i++ {
				if err := p.Submit(Job{ID: base*jobsPerSubmitter + i}); err != nil {
					// Pool may close before all submitters finish in stress runs.
					return
				}
			}
		}(s)
	}

	wg.Wait()
	p.Shutdown()
	drainWg.Wait()
}

// ExamplePool_Submit demonstrates basic pool usage.
func ExamplePool_Submit() {
	p := New(2, func(j Job) Result {
		return Result{JobID: j.ID, Value: "ok"}
	})

	// Drain results concurrently before submitting so the buffer never fills.
	done := make(chan struct{})
	go func() {
		for range p.Results() {
		}
		close(done)
	}()

	_ = p.Submit(Job{ID: 1})
	p.Shutdown()
	<-done
	// Output:
}
```

Your turn: add `TestShutdownIsIdempotent` that calls `p.Shutdown()` three times from separate goroutines and asserts no panic occurs (use `sync.WaitGroup` to launch the three concurrent calls).

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/workerpool"
)

func main() {
	p := workerpool.New(4, func(j workerpool.Job) workerpool.Result {
		return workerpool.Result{
			JobID: j.ID,
			Value: fmt.Sprintf("processed: %s", j.Payload),
		}
	})

	done := make(chan struct{})
	go func() {
		for r := range p.Results() {
			fmt.Printf("job %d: %s\n", r.JobID, r.Value)
		}
		close(done)
	}()

	const n = 10
	for i := 0; i < n; i++ {
		if err := p.Submit(workerpool.Job{ID: i, Payload: fmt.Sprintf("item-%d", i)}); err != nil {
			fmt.Printf("submit error: %v\n", err)
		}
	}

	p.Shutdown()
	<-done
}
```

## Common Mistakes

### Using `time.Sleep` as Synchronization

Wrong:

```go
go worker()
time.Sleep(100 * time.Millisecond)
// assume worker has processed by now
checkResult()
```

What happens: on a loaded CI machine the worker may not have run yet. The test passes locally and fails in CI.

Fix: have the worker signal a channel or decrement a WaitGroup. The test waits on the signal, not a timer.

### Calling `t.Fatal` From a Non-Test Goroutine

Wrong:

```go
go func() {
	result := doWork()
	if result != want {
		t.Fatal("wrong result") // panic: Fail called from non-test goroutine
	}
}()
```

What happens: `t.Fatal` calls `runtime.Goexit()`, which only exits the current goroutine. Called from a goroutine that is not the test goroutine, it panics.

Fix: send the error back to the test goroutine via a channel and call `t.Fatal` there:

```go
errs := make(chan error, 1)
go func() {
	if result := doWork(); result != want {
		errs <- fmt.Errorf("got %v, want %v", result, want)
	}
	close(errs)
}()
if err := <-errs; err != nil {
	t.Fatal(err)
}
```

### Forgetting to Drain the Results Channel After Shutdown

Wrong: calling `p.Shutdown()` and then not reading `p.Results()`. The workers' sends into the results channel may block if the results buffer is full, causing `Shutdown` to deadlock.

Fix: drain the results channel concurrently with submitting, or drain it after `Shutdown` once the channel is closed.

## Verification

From `~/go-exercises/workerpool`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

For extra confidence run with `-count=100` to stress scheduling:

```bash
go test -race -count=100 -timeout 120s ./...
```

All tests must pass every run with zero `DATA RACE` lines.

## Summary

- Never use `time.Sleep` as a synchronization mechanism in tests; use channels, WaitGroups, or barriers.
- Barrier pattern: each goroutine signals a channel on arrival; the test collects all signals before releasing the barrier, proving concurrency.
- `time.After` is a test deadline (prevents hanging), not an assertion.
- `t.Fatal` must be called from the test goroutine; send errors back via a channel from spawned goroutines.
- `-race -count=100` stress-tests the scheduler and catches races that appear in fewer than 1% of runs.

## What's Next

Next: [Deadlock Detection Strategies](../04-deadlock-detection-strategies/04-deadlock-detection-strategies.md).

## Resources

- [testing package](https://pkg.go.dev/testing)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go Blog: The Go Memory Model](https://go.dev/ref/mem)
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)
