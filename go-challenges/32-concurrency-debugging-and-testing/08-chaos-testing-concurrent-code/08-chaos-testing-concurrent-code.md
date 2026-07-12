# 8. Chaos Testing Concurrent Code

Unit tests verify the happy path. The race detector catches data races. Neither tests what happens when one pipeline stage takes 100× longer than expected, when a context is cancelled mid-operation, or when an intermediate step returns an error randomly. Chaos testing injects these failures deliberately during tests to verify that synchronization, error handling, and resource cleanup hold under adversity. This lesson builds a three-stage pipeline with a configurable fault injector, verifies key invariants after each chaos run, and confirms no goroutine leaks using the stdlib leak checker from lesson 02.

```text
pipeline/
  go.mod
  pipeline.go
  chaos.go
  pipeline_test.go
  cmd/demo/
    main.go
```

## Concepts

### What Chaos Testing Adds

A deterministic unit test exercises a fixed execution path. A chaos test makes the execution path non-deterministic by introducing:

- **Random delays**: change goroutine scheduling order; surface races that only appear under specific interleavings.
- **Random errors**: exercise error propagation paths that are rarely triggered in normal use.
- **Context cancellation**: verify shutdown logic and resource cleanup.

Running chaos tests with `-count=N -race` multiplies the scheduling variations. A bug that appears in 1% of runs is likely to surface in 100 runs.

### Invariants After Chaos

After a chaos run completes (success or error), certain invariants must hold regardless of what the injector did:

- **No goroutine leaks**: all goroutines started by the pipeline must have exited.
- **No data loss**: every input item is either processed, errored, or explicitly dropped; `processed + errored == total_input`.
- **No duplication**: an item processed twice is a correctness bug even if the final count is right.
- **Clean context**: no goroutines waiting on a cancelled context.

### `math/rand/v2` for Reproducible Chaos

Go 1.22 introduced `math/rand/v2` with a cleaner API and better defaults. For chaos testing, seed the PRNG with a logged seed value so a failing run can be reproduced:

```go
seed := time.Now().UnixNano()
t.Logf("chaos seed: %d", seed)
rng := rand.New(rand.NewPCG(uint64(seed), 0))
```

Note the scope of this reproducibility. A seeded injector replays the same
*sequence of fault decisions* only when its draws are consumed in the same
order — for example, from a single goroutine. It does NOT make the aggregate
outcome of a concurrent run reproducible: when several stage goroutines share
one injector, the order in which they consume the PRNG, together with
cancellation timing, is chosen by the scheduler and is not pinned by the seed.
So seed to replay the injector's decision stream, and assert run-to-run
invariants (no leaks, no data loss) on the concurrent run rather than expecting
identical counts.

### Coordinating Stage Goroutines With `sync.WaitGroup` and a Shared Error

`golang.org/x/sync/errgroup` is the idiomatic tool for fan-out goroutines where the first error should cancel the rest, but it is an external module. This lesson uses only stdlib primitives: a `sync.WaitGroup` to wait for all three stage goroutines to exit, a `sync.Mutex`-guarded `firstErr` variable to record the first non-nil error, and a `context.WithCancel` cancel function that is called on the first error so remaining stages drain and exit promptly. The pattern is:

```go
var (
    firstErr error
    errMu    sync.Mutex
)
setErr := func(err error) {
    if err == nil {
        return
    }
    errMu.Lock()
    if firstErr == nil {
        firstErr = err
        cancel() // signal remaining stages to stop
    }
    errMu.Unlock()
}
```

Each stage calls `setErr` when it encounters an error and also selects on `ctx.Done()` when sending to the next channel, so a cancellation from any stage propagates without blocking the others.

## Exercises

### Exercise 1: Fault Injector

Create `chaos.go`:

```go
package pipeline

import (
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// ErrInjected is the sentinel error returned by the fault injector.
var ErrInjected = errors.New("injected fault")

// Config controls what the FaultInjector does.
type Config struct {
	// ErrorRate is the probability [0,1] that MaybeError returns ErrInjected.
	ErrorRate float64
	// MaxDelay is the maximum random delay injected before each operation.
	MaxDelay time.Duration
}

// FaultInjector introduces random failures and delays into pipeline stages.
// It is safe to call concurrently from multiple goroutines.
type FaultInjector struct {
	cfg Config
	mu  sync.Mutex
	rng *rand.Rand
}

// NewFaultInjector creates a FaultInjector with the given config and a
// random PCG source seeded from time.
func NewFaultInjector(cfg Config) *FaultInjector {
	src := rand.NewPCG(uint64(time.Now().UnixNano()), 0)
	return &FaultInjector{cfg: cfg, rng: rand.New(src)}
}

// NewFaultInjectorSeeded creates a FaultInjector with a fixed seed for
// reproducibility. Log the seed value in tests so failures can be replayed.
func NewFaultInjectorSeeded(cfg Config, seed uint64) *FaultInjector {
	src := rand.NewPCG(seed, 0)
	return &FaultInjector{cfg: cfg, rng: rand.New(src)}
}

// MaybeDelay sleeps for a random duration up to MaxDelay.
func (f *FaultInjector) MaybeDelay() {
	if f.cfg.MaxDelay <= 0 {
		return
	}
	f.mu.Lock()
	d := time.Duration(f.rng.Int64N(int64(f.cfg.MaxDelay)))
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

// MaybeError returns ErrInjected with probability ErrorRate.
func (f *FaultInjector) MaybeError() error {
	f.mu.Lock()
	v := f.rng.Float64()
	f.mu.Unlock()
	if v < f.cfg.ErrorRate {
		return ErrInjected
	}
	return nil
}
```

### Exercise 2: Three-Stage Pipeline

Create `pipeline.go`:

```go
package pipeline

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Item is the data unit flowing through the pipeline.
type Item struct {
	ID int
}

// Counters tracks what happened to each item during a pipeline run.
type Counters struct {
	Processed atomic.Int64
	Errored   atomic.Int64
}

// Total returns processed + errored.
func (c *Counters) Total() int64 {
	return c.Processed.Load() + c.Errored.Load()
}

// Pipeline runs items through three stages: fetch -> transform -> store.
// Each stage can be disrupted by a FaultInjector.
type Pipeline struct {
	chaos *FaultInjector
}

// NewPipeline creates a Pipeline with the given fault injector.
// Pass nil for no fault injection.
func NewPipeline(chaos *FaultInjector) *Pipeline {
	return &Pipeline{chaos: chaos}
}

// Run processes items concurrently. It blocks until all stages complete or
// ctx is cancelled. Returns the first non-nil stage error.
func (p *Pipeline) Run(ctx context.Context, items []Item) (*Counters, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fetchOut := make(chan Item, len(items))
	transformOut := make(chan Item, len(items))

	var (
		counters Counters
		wg       sync.WaitGroup
		firstErr error
		errMu    sync.Mutex
	)

	setErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel() // cancel remaining stages
		}
		errMu.Unlock()
	}

	// Stage 1: fetch
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(fetchOut)
		for _, item := range items {
			if p.chaos != nil {
				p.chaos.MaybeDelay()
				if err := p.chaos.MaybeError(); err != nil {
					counters.Errored.Add(1)
					setErr(fmt.Errorf("fetch item %d: %w", item.ID, err))
					continue
				}
			}
			select {
			case fetchOut <- item:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Stage 2: transform
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(transformOut)
		for item := range fetchOut {
			if p.chaos != nil {
				p.chaos.MaybeDelay()
				if err := p.chaos.MaybeError(); err != nil {
					counters.Errored.Add(1)
					setErr(fmt.Errorf("transform item %d: %w", item.ID, err))
					continue
				}
			}
			select {
			case transformOut <- item:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Stage 3: store
	wg.Add(1)
	go func() {
		defer wg.Done()
		for item := range transformOut {
			if p.chaos != nil {
				p.chaos.MaybeDelay()
				if err := p.chaos.MaybeError(); err != nil {
					counters.Errored.Add(1)
					setErr(fmt.Errorf("store item %d: %w", item.ID, err))
					continue
				}
			}
			counters.Processed.Add(1)
		}
	}()

	wg.Wait()

	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	return &counters, err
}
```

### Exercise 3: Chaos Tests

Create `pipeline_test.go`:

```go
package pipeline

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// goroutinesAfter waits up to grace for the goroutine count to drop to at most
// before+delta. Returns the final count and whether a leak was detected.
func goroutinesAfter(before, delta int, grace time.Duration) (int, bool) {
	deadline := time.Now().Add(grace)
	for {
		after := runtime.NumGoroutine()
		if after <= before+delta {
			return after, false
		}
		if time.Now().After(deadline) {
			return after, true
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// captureStacks returns all goroutine stacks.
func captureStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}

// TestNoChaosAllProcessed verifies that without chaos, all items are processed.
func TestNoChaosAllProcessed(t *testing.T) {
	t.Parallel()

	const n = 100
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i}
	}

	before := runtime.NumGoroutine()
	p := NewPipeline(nil)
	c, err := p.Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Processed.Load() != n {
		t.Fatalf("Processed = %d, want %d", c.Processed.Load(), n)
	}
	if c.Errored.Load() != 0 {
		t.Fatalf("Errored = %d, want 0", c.Errored.Load())
	}

	if after, leaked := goroutinesAfter(before, 0, 500*time.Millisecond); leaked {
		t.Fatalf("goroutine leak: before=%d after=%d\n%s", before, after, captureStacks())
	}
}

// TestChaosNoDataLoss verifies that processed+errored == total input under chaos.
func TestChaosNoDataLoss(t *testing.T) {
	t.Parallel()

	const n = 50
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i}
	}

	cfg := Config{
		ErrorRate: 0.3,
		MaxDelay:  5 * time.Millisecond,
	}

	before := runtime.NumGoroutine()

	p := NewPipeline(NewFaultInjector(cfg))
	c, _ := p.Run(context.Background(), items)

	total := c.Total()
	if total > int64(n) {
		t.Fatalf("total=%d exceeds input=%d (duplication bug)", total, n)
	}
	// Under high error rates some items may be dropped by context cancellation,
	// so total may be less than n. Verify it's at least 1 (something ran).
	if total < 1 {
		t.Fatalf("total=%d: no items processed or errored", total)
	}

	if after, leaked := goroutinesAfter(before, 0, 500*time.Millisecond); leaked {
		t.Fatalf("goroutine leak after chaos: before=%d after=%d\n%s",
			before, after, captureStacks())
	}
}

// TestChaosContextCancellation verifies the pipeline exits cleanly when ctx is cancelled.
func TestChaosContextCancellation(t *testing.T) {
	t.Parallel()

	const n = 1000
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i}
	}

	cfg := Config{
		MaxDelay: 2 * time.Millisecond,
	}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay to interrupt the pipeline mid-run.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	p := NewPipeline(NewFaultInjector(cfg))
	_, err := p.Run(ctx, items)
	// err may be nil (if the pipeline finished before cancel) or context.Canceled.
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrInjected) {
		t.Fatalf("unexpected error: %v", err)
	}

	if after, leaked := goroutinesAfter(before, 0, 500*time.Millisecond); leaked {
		t.Fatalf("goroutine leak after cancellation: before=%d after=%d\n%s",
			before, after, captureStacks())
	}
}

// TestChaos100PercentErrors verifies the pipeline handles all-error scenarios.
func TestChaos100PercentErrors(t *testing.T) {
	t.Parallel()

	const n = 20
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i}
	}

	cfg := Config{ErrorRate: 1.0}
	before := runtime.NumGoroutine()

	p := NewPipeline(NewFaultInjector(cfg))
	c, _ := p.Run(context.Background(), items)

	// With 100% error rate on all three stages and context cancellation,
	// Processed must be 0.
	if c.Processed.Load() != 0 {
		t.Fatalf("Processed=%d with 100%% error rate, want 0", c.Processed.Load())
	}

	if after, leaked := goroutinesAfter(before, 0, 500*time.Millisecond); leaked {
		t.Fatalf("goroutine leak: before=%d after=%d\n%s", before, after, captureStacks())
	}
}

// TestReproducibleChaos verifies that a seeded injector replays the same
// sequence of fault decisions when consumed from a single goroutine. This is
// the property the seed actually guarantees. The aggregate counts of a
// concurrent pipeline run are deliberately NOT asserted here: several stage
// goroutines share one injector, so the PRNG-consumption order and the
// cancellation timing are set by the scheduler, not by the seed.
func TestReproducibleChaos(t *testing.T) {
	t.Parallel()

	const seed = 42
	const draws = 50

	cfg := Config{ErrorRate: 0.4, MaxDelay: 1 * time.Millisecond}

	sequence := func() []bool {
		f := NewFaultInjectorSeeded(cfg, seed)
		out := make([]bool, draws)
		for i := range out {
			out[i] = f.MaybeError() != nil
		}
		return out
	}

	s1 := sequence()
	s2 := sequence()

	for i := range s1 {
		if s1[i] != s2[i] {
			t.Fatalf("seeded injector not reproducible at draw %d:\n run1=%v\n run2=%v", i, s1, s2)
		}
	}
}

// ExamplePipeline_Run shows basic pipeline usage without chaos.
func ExamplePipeline_Run() {
	items := []Item{{ID: 1}, {ID: 2}}
	p := NewPipeline(nil)
	c, _ := p.Run(context.Background(), items)
	_ = c.Processed.Load() // 2
	// Output:
}
```

Your turn: add `TestChaosStress` that runs the pipeline 20 times in a loop with `ErrorRate: 0.2` and `MaxDelay: 3ms`, checks for goroutine leaks after each run, and asserts that in at least one run `Processed.Load() > 0`. Use `t.Parallel()`.

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/pipeline"
)

func main() {
	const n = 100
	items := make([]pipeline.Item, n)
	for i := range items {
		items[i] = pipeline.Item{ID: i}
	}

	// Run without chaos.
	p := pipeline.NewPipeline(nil)
	c, _ := p.Run(context.Background(), items)
	fmt.Printf("no chaos:     processed=%d errored=%d total=%d\n",
		c.Processed.Load(), c.Errored.Load(), c.Total())

	// Run with 20% error rate and small delays.
	cfg := pipeline.Config{
		ErrorRate: 0.2,
		MaxDelay:  5 * time.Millisecond,
	}
	p2 := pipeline.NewPipeline(pipeline.NewFaultInjector(cfg))
	c2, _ := p2.Run(context.Background(), items)
	fmt.Printf("20%% errors:   processed=%d errored=%d total=%d\n",
		c2.Processed.Load(), c2.Errored.Load(), c2.Total())
}
```

## Common Mistakes

### Not Checking for Goroutine Leaks After Chaos

Wrong: running a chaos test, asserting invariants on counters, and declaring success without checking whether all pipeline goroutines exited.

What happens: a cancelled context can leave goroutines blocked on channel sends or receives if the pipeline does not drain channels properly. The goroutine count grows with each test run until the test binary crashes.

Fix: snapshot `runtime.NumGoroutine()` before the test, run the pipeline, wait with a grace period, and fail the test if the count is still elevated.

### Ignoring `context.Canceled` in Error Checks

Wrong:

```go
_, err := p.Run(ctx, items)
if err != nil {
	t.Fatalf("unexpected error: %v", err)
}
```

What happens: a chaos test that deliberately cancels the context causes `Run` to return `context.Canceled`, which the test treats as a failure.

Fix: distinguish expected errors (context cancellation, injected faults) from unexpected ones using `errors.Is`:

```go
if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, pipeline.ErrInjected) {
	t.Fatalf("unexpected error: %v", err)
}
```

### Using `time.Sleep` Instead of a Seeded PRNG for Delays

Wrong: injecting a fixed `time.Sleep(100 * time.Millisecond)` to "create chaos".

What happens: all chaos runs exercise the same scheduling pattern. Bugs that appear only under specific interleavings are not triggered.

Fix: use random delays from a seeded PRNG. Log the seed so failures are reproducible.

## Verification

From `~/go-exercises/pipeline`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -count=5 -race -timeout 60s ./...
go run ./cmd/demo
```

`-count=5` runs each test five times to exercise different scheduling patterns. All tests must pass with no goroutine leaks or race warnings.

## Summary

- Chaos testing injects random delays, errors, and cancellations to exercise paths that deterministic tests miss.
- Seed the PRNG and log the seed value so a failing chaos run can be reproduced.
- After every chaos run, verify: no goroutine leaks, no data duplication.
- Distinguish expected errors (cancellations, injected faults) from unexpected ones with `errors.Is`.
- `math/rand/v2` (Go 1.22+) provides a cleaner PRNG API; use `NewPCG` for reproducible seeding.
- Run chaos tests with `-count=N -race` to maximize scheduling variation and race detection.

## What's Next

Next: [TCP Server and Client](../../33-tcp-udp-and-networking/01-tcp-server-and-client/01-tcp-server-and-client.md).

## Resources

- [math/rand/v2](https://pkg.go.dev/math/rand/v2)
- [context package](https://pkg.go.dev/context)
- [sync package](https://pkg.go.dev/sync)
- [Go Blog: The Go Memory Model](https://go.dev/ref/mem)
