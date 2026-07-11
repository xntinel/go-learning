# 19. Pipeline With Per-Stage Metrics: Observe Flow Without Changing It

A pipeline (lesson 01) moves values through stages. Per-stage metrics let you
observe the flow: how many items entered, how many exited, how long each
item spent in the stage, and how often the stage dropped items on a full
buffer. Metrics must be cheap (atomic counters, no locks) and must not
change the data path.

```text
pipemetrics/
  go.mod
  internal/pipemetrics/pipemetrics.go
  internal/pipemetrics/pipemetrics_test.go
  cmd/pipemetricsdemo/main.go
```

The package exposes `Stage` that wraps a stage function and records `In`,
`Out`, `Dropped`, and `LatencySum`. The lesson uses atomic counters so the
metrics are race-free under `-race`.

## Concepts

### Metrics Are Counters, Not Logs

A pipeline at 100k items/s cannot log every item. Counters aggregate:
increment on enter, increment on exit, increment on drop. The downstream
consumer reads the counters periodically.

### Use `atomic` For Counters

Every increment must be race-free. `sync/atomic` provides `AddInt64` on
`atomic.Int64` which is cheap and lock-free. The lesson uses `atomic.Int64`
throughout.

### Latency Needs A Clock

`time.Since(t)` at the entry, `time.Since(t)` at the exit, sum the delta.
For nanosecond precision, use `time.Now()`. For lower overhead, use
`runtime.nanotime()` via `time.Now()` (which is what `time.Since` calls
internally).

### Dropped Counts Backpressure

When a downstream stage is slow, an upstream stage's send blocks. With
buffered channels, sends fail when the buffer is full — those are "drops".
A `TrySend` variant records the drop and moves on.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/pipemetrics/internal/pipemetrics ~/go-exercises/pipemetrics/cmd/pipemetricsdemo
cd ~/go-exercises/pipemetrics
go mod init example.com/pipemetrics
```

### Exercise 1: The Stage Wrapper

Create `internal/pipemetrics/pipemetrics.go`:

```go
package pipemetrics

import (
	"sync/atomic"
	"time"
)

type Stage[T any] struct {
	Name string

	in         atomic.Int64
	out        atomic.Int64
	dropped    atomic.Int64
	latencySum atomic.Int64 // nanoseconds
}

func NewStage[T any](name string) *Stage[T] {
	return &Stage[T]{Name: name}
}

func (s *Stage[T]) In() int64         { return s.in.Load() }
func (s *Stage[T]) Out() int64        { return s.out.Load() }
func (s *Stage[T]) Dropped() int64    { return s.dropped.Load() }
func (s *Stage[T]) LatencySum() int64 { return s.latencySum.Load() }

// Snapshot returns the current counters; safe to call concurrently.
type Snapshot struct {
	Name       string
	In         int64
	Out        int64
	Dropped    int64
	LatencySum time.Duration
}

func (s *Stage[T]) Snapshot() Snapshot {
	return Snapshot{
		Name:       s.Name,
		In:         s.in.Load(),
		Out:        s.out.Load(),
		Dropped:    s.dropped.Load(),
		LatencySum: time.Duration(s.latencySum.Load()),
	}
}

// Wrap returns an inbound and outbound channel. The wrapper increments `in`
// on entry, `out` on successful send to `out`, and `dropped` when `out` is
// full (use a buffered `out` to make this meaningful). On exit (input closed
// or done fired), the wrapper closes `out` so downstream stages can drain.
func (s *Stage[T]) Wrap(in <-chan T, out chan T, done <-chan struct{}) {
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				s.in.Add(1)
				start := time.Now()
				select {
				case out <- v:
					s.out.Add(1)
					s.latencySum.Add(int64(time.Since(start)))
				default:
					s.dropped.Add(1)
				}
			}
		}
	}()
}

// RunBlock is the non-dropping variant: it waits for the send to complete or
// for `done` to fire.
func (s *Stage[T]) RunBlock(in <-chan T, out chan<- T, done <-chan struct{}) {
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				s.in.Add(1)
				start := time.Now()
				select {
				case out <- v:
					s.out.Add(1)
					s.latencySum.Add(int64(time.Since(start)))
				case <-done:
					return
				}
			}
		}
	}()
}
```

The wrapper increments counters without holding locks. `Wrap` records a
drop when the send cannot proceed (buffer full and no receiver); `RunBlock`
waits for capacity or cancellation.

### Exercise 2: Test The Contract

Create `internal/pipemetrics/pipemetrics_test.go`:

```go
package pipemetrics

import (
	"sync"
	"testing"
	"time"
)

func TestStageCountsInAndOut(t *testing.T) {
	t.Parallel()

	src := make(chan int, 5)
	for i := 1; i <= 5; i++ {
		src <- i
	}
	close(src)

	out := make(chan int, 5)
	done := make(chan struct{})
	defer close(done)

	s := NewStage[int]("test")
	s.Wrap(src, out, done)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 5 {
		t.Fatalf("got %v, want 1..5", got)
	}
	if s.In() != 5 {
		t.Fatalf("In = %d, want 5", s.In())
	}
	if s.Out() != 5 {
		t.Fatalf("Out = %d, want 5", s.Out())
	}
}

func TestStageDropsWhenBufferFull(t *testing.T) {
	t.Parallel()

	src := make(chan int, 10)
	for i := 0; i < 10; i++ {
		src <- i
	}
	close(src)

	// out is unbuffered with no reader, so every send drops.
	out := make(chan int)
	done := make(chan struct{})
	defer close(done)

	s := NewStage[int]("test")
	s.Wrap(src, out, done)

	// Give the wrapper time to process all items.
	time.Sleep(20 * time.Millisecond)

	if s.In() != 10 {
		t.Fatalf("In = %d, want 10", s.In())
	}
	if s.Dropped() != 10 {
		t.Fatalf("Dropped = %d, want 10", s.Dropped())
	}
}

func TestRunBlockWaitsForConsumer(t *testing.T) {
	t.Parallel()

	src := make(chan int, 3)
	src <- 1
	src <- 2
	src <- 3
	close(src)

	out := make(chan int, 3)
	done := make(chan struct{})
	defer close(done)

	s := NewStage[int]("test")
	s.RunBlock(src, out, done)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	if s.In() != 3 || s.Out() != 3 || s.Dropped() != 0 {
		t.Fatalf("counters wrong: in=%d out=%d dropped=%d", s.In(), s.Out(), s.Dropped())
	}
}

func TestLatencySumIsPositive(t *testing.T) {
	t.Parallel()

	src := make(chan int, 5)
	for i := 0; i < 5; i++ {
		src <- i
	}
	close(src)

	out := make(chan int, 5)
	done := make(chan struct{})
	defer close(done)

	s := NewStage[int]("test")
	s.Wrap(src, out, done)

	for range out {
	}
	if s.LatencySum() <= 0 {
		t.Fatalf("LatencySum = %d, want > 0", s.LatencySum())
	}
}

func TestStageIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 1000
	src := make(chan int, n)
	for i := 0; i < n; i++ {
		src <- i
	}
	close(src)

	out := make(chan int, n)
	done := make(chan struct{})
	defer close(done)

	s := NewStage[int]("test")
	s.Wrap(src, out, done)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range out {
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_ = s.Snapshot()
		}
	}()
	wg.Wait()
}

func TestSnapshotCapturesAllCounters(t *testing.T) {
	t.Parallel()

	s := NewStage[int]("snap")
	s.in.Store(7)
	s.out.Store(5)
	s.dropped.Store(2)
	s.latencySum.Store(int64(123 * time.Nanosecond))

	snap := s.Snapshot()
	if snap.Name != "snap" {
		t.Errorf("Name = %q, want snap", snap.Name)
	}
	if snap.In != 7 || snap.Out != 5 || snap.Dropped != 2 {
		t.Errorf("counters wrong: %+v", snap)
	}
	if snap.LatencySum != 123*time.Nanosecond {
		t.Errorf("LatencySum = %s, want 123ns", snap.LatencySum)
	}
}
```

`TestStageDropsWhenBufferFull` pins the drop semantics: with an unbuffered
output and no reader, every send is a drop. The test gives the wrapper
20ms to process all 10 items.

Your turn: add `TestStageReportsZeroOnEmptyInput` that closes the source
with no items and asserts all counters are zero.

### Exercise 3: Runnable Demo

Create `cmd/pipemetricsdemo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/pipemetrics/internal/pipemetrics"
)

func main() {
	src := make(chan int, 100)
	go func() {
		defer close(src)
		for i := 0; i < 100; i++ {
			src <- i
			time.Sleep(time.Millisecond)
		}
	}()

	stage1 := pipemetrics.NewStage[int]("emit")
	stage2 := pipemetrics.NewStage[int]("double")

	mid := make(chan int, 10)
	out := make(chan int, 100)
	done := make(chan struct{})
	defer close(done)

	stage1.Wrap(src, mid, done)
	stage2.RunBlock(mid, out, done)

	go func() {
		for v := range out {
			_ = v
		}
	}()

	time.Sleep(150 * time.Millisecond)

	for _, s := range []*pipemetrics.Stage[int]{stage1, stage2} {
		snap := s.Snapshot()
		fmt.Printf("[%s] in=%d out=%d dropped=%d latency=%s\n",
			snap.Name, snap.In, snap.Out, snap.Dropped, snap.LatencySum)
	}
}
```

## Common Mistakes

### Locking For Counter Increments

Wrong: `s.mu.Lock(); s.in++; s.mu.Unlock()` for every item.

What happens: 100k items/s means 100k lock acquisitions per second per
stage. The lock contention dominates runtime.

Fix: use `atomic.Int64`. Increment is a single CPU instruction.

### Recording Per-Item Latency

Wrong: appending each item's latency to a slice and averaging at the end.

What happens: a million-item pipeline has a million-element slice, all
under lock.

Fix: sum the latencies into a single `atomic.Int64`. The average is
`sum / count`, both cheap to read.

### Changing The Data Path To Add Metrics

Wrong: wrapping a stage in a goroutine just to record metrics.

What happens: the goroutine adds a hop in the data path; latency goes up;
the pipeline is no longer comparable to the unmeasured version.

Fix: increment counters at the existing send/receive sites. The wrapper
in this lesson adds no extra goroutine for the counter increment itself
(only one goroutine to run the loop).

### Treating Drops As Failures

Wrong: returning an error on every drop.

What happens: the caller cannot distinguish "downstream slow" from "data
corrupt".

Fix: a drop counter is the right metric. The application decides whether
the drop rate is acceptable.

## Verification

From `~/go-exercises/pipemetrics`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `TestStageIsRaceFree` runs concurrent snapshots against
live increments; the race detector pins the atomic operations.

## Summary

- Per-stage metrics observe flow without changing the data path.
- Use `atomic.Int64` for counters; never lock on the hot path.
- Track `in`, `out`, `dropped`, and `latencySum` per stage.
- A drop is a backpressure signal, not a failure.
- The `Snapshot` helper returns a value type safe for concurrent reads.

## What's Next

Next: [Batch Processing Partial Failure](../20-batch-processing-partial-failure/20-batch-processing-partial-failure.md).

## Resources

- [pkg.go.dev: sync/atomic](https://pkg.go.dev/sync/atomic)
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Prometheus instrumentation in Go](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus)