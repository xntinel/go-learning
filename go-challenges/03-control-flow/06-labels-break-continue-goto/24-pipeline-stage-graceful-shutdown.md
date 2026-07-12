# Exercise 24: Drain in-flight work across pipeline stages on shutdown

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A streaming pipeline has several stages — ingest, transform, output — each
with its own queue, and each stage only accepts as much work as the stage
after it has room for: the same backpressure that keeps the pipeline stable
in steady state still applies during shutdown. When a stage runs out of
room to accept more, upstream cannot push through it, and the whole drain
has to stop right there rather than let earlier stages keep accumulating
work a downstream stage will never take. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
pipeline/                   independent module: example.com/pipeline
  go.mod                     go 1.24
  pipeline.go                 Stage, Shutdown
  cmd/
    demo/
      main.go                runnable demo: three stages, halted by downstream capacity
  pipeline_test.go            table test: no stages, single stage no downstream, empty queues, full flush, capacity halt mid-flush, zero-capacity halt on the first item
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Shutdown(stages []Stage) (flushed map[string]int, haltedAt string, halted bool)`, flushing each stage's queue into the next stage while consuming its capacity, and halting every remaining stage the moment capacity runs out.
- Test: no stages, a single stage with no downstream, empty queues, enough capacity for a full flush, a capacity limit that halts mid-flush, and a zero-capacity downstream that halts on the very first item.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the capacity halt needs to reach past the current stage entirely

`Shutdown` has two loops: the outer one walks stages in pipeline order, and
the inner one walks the items in the CURRENT stage's queue, flushing each
one into the next stage and decrementing that next stage's remaining
capacity. The moment that capacity hits zero, the current stage cannot push
any more work downstream — but stopping only the inner loop would leave the
outer loop free to move on to the NEXT stage and start flushing ITS queue,
even though the pipeline as a whole has already stalled. A labeled `break
stages`, fired from inside the per-item flush loop, is what actually
matches the real shape of a shutdown signal: once one stage cannot make
forward progress, every stage after it stops too, exactly like a `for`-
`select` drain loop that leaves on `ctx.Done()` instead of spinning forever
on a `select` that never resolves. The last stage in the pipeline has no
downstream to check (`i+1 < len(stages)` is false for it), so it always
flushes unconditionally — there is nowhere further for its output to be
blocked.

One caveat worth calling out: `Shutdown` mutates each stage's `Capacity`
field in place as it flushes, so callers should pass a slice they are
willing to have modified — never a shared slice read concurrently by
another goroutine.

Create `pipeline.go`:

```go
package pipeline

// Stage is one pipeline stage (ingest, transform, output, ...) with its own
// pending queue at shutdown time. Capacity is how many more items the NEXT
// stage downstream can still accept before it is full.
type Stage struct {
	Name     string
	Queue    []int
	Capacity int
}

// Shutdown drains stages in pipeline order. Each stage flushes its queued
// items one at a time into the next stage, consuming that next stage's
// remaining capacity as it goes — this is the backpressure the pipeline
// runs under even during shutdown. The moment the next stage's capacity
// runs out mid-flush, it cannot accept any more work from upstream: the
// drain halts right there, and a labeled break — fired from inside the
// per-item flush loop, one level below the per-stage loop — stops EVERY
// remaining stage's drain in the same statement, exactly like a shutdown
// signal cascading downstream through a real for-select pipeline.
func Shutdown(stages []Stage) (flushed map[string]int, haltedAt string, halted bool) {
	flushed = make(map[string]int)

stages:
	for i, s := range stages {
		for range s.Queue {
			if i+1 < len(stages) {
				if stages[i+1].Capacity <= 0 {
					haltedAt = s.Name
					halted = true
					break stages
				}
				stages[i+1].Capacity--
			}
			flushed[s.Name]++
		}
	}

	return flushed, haltedAt, halted
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipeline"
)

func main() {
	stages := []pipeline.Stage{
		{Name: "ingest", Queue: []int{1, 2, 3, 4}},
		{Name: "transform", Queue: []int{5, 6}, Capacity: 2}, // can accept 2 more from ingest
		{Name: "output", Queue: []int{7}, Capacity: 100},
	}

	flushed, haltedAt, halted := pipeline.Shutdown(stages)
	fmt.Println("flushed:", flushed)
	fmt.Println("halted:", halted, "at", haltedAt)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed: map[ingest:2]
halted: true at ingest
```

`ingest` has four items, but `transform` only has room for two more, so only
the first two are flushed before the drain halts. `transform`'s own two
queued items, and `output`'s one, are never even attempted — the halt
cascades all the way through, exactly as if every stage downstream had
received the same shutdown signal.

### Tests

`TestShutdown` covers no stages at all, a single stage with no downstream to
block it, stages with nothing queued, enough capacity for every stage to
flush completely, a capacity limit that halts partway through the first
stage, and a downstream with zero capacity that halts on the very first
item.

Create `pipeline_test.go`:

```go
package pipeline

import "testing"

func TestShutdown(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stages       []Stage
		wantFlushed  map[string]int
		wantHalted   bool
		wantHaltedAt string
	}{
		"no stages": {
			stages:      nil,
			wantFlushed: map[string]int{},
		},
		"a single stage with no downstream always fully flushes": {
			stages: []Stage{
				{Name: "output", Queue: []int{1, 2, 3}},
			},
			wantFlushed: map[string]int{"output": 3},
		},
		"empty queues flush nothing but do not halt": {
			stages: []Stage{
				{Name: "ingest", Queue: nil},
				{Name: "output", Queue: nil, Capacity: 5},
			},
			wantFlushed: map[string]int{},
		},
		"enough capacity lets every stage fully flush": {
			stages: []Stage{
				{Name: "ingest", Queue: []int{1, 2}},
				{Name: "transform", Queue: []int{3}, Capacity: 2},
				{Name: "output", Queue: []int{4}, Capacity: 5},
			},
			wantFlushed: map[string]int{"ingest": 2, "transform": 1, "output": 1},
		},
		"insufficient downstream capacity halts mid-flush and cascades": {
			stages: []Stage{
				{Name: "ingest", Queue: []int{1, 2, 3, 4}},
				{Name: "transform", Queue: []int{5, 6}, Capacity: 2},
				{Name: "output", Queue: []int{7}, Capacity: 100},
			},
			wantFlushed:  map[string]int{"ingest": 2},
			wantHalted:   true,
			wantHaltedAt: "ingest",
		},
		"zero downstream capacity halts on the very first item": {
			stages: []Stage{
				{Name: "ingest", Queue: []int{1}},
				{Name: "transform", Queue: nil, Capacity: 0},
			},
			wantFlushed:  map[string]int{},
			wantHalted:   true,
			wantHaltedAt: "ingest",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			flushed, haltedAt, halted := Shutdown(tc.stages)
			if len(flushed) != len(tc.wantFlushed) {
				t.Fatalf("flushed = %v, want %v", flushed, tc.wantFlushed)
			}
			for k, v := range tc.wantFlushed {
				if flushed[k] != v {
					t.Fatalf("flushed[%q] = %d, want %d", k, flushed[k], v)
				}
			}
			if halted != tc.wantHalted || haltedAt != tc.wantHaltedAt {
				t.Fatalf("halted,haltedAt = %v,%q want %v,%q", halted, haltedAt, tc.wantHalted, tc.wantHaltedAt)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The drain is correct when a capacity halt in an early stage stops every
stage after it, not just the one that hit the wall — the "insufficient
downstream capacity" test is the one to study, since `transform` and
`output` both have perfectly flushable items that are never touched. The
bug this exercise guards against is a `break` that only reaches the
per-item flush loop: the outer stage loop would then move on to `transform`
and start flushing IT into `output`, silently ignoring the fact that
`ingest` was left mid-drain — the opposite of a clean, cascading shutdown.
The last-stage boundary case (no downstream to check) confirms the sink end
of a pipeline never artificially blocks on a capacity check that does not
apply to it.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [Backpressure explained (Reactive Streams)](https://www.reactive-streams.org/) — the flow-control concept this pipeline's capacity check enforces even during shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-distributed-changelog-sync.md](23-distributed-changelog-sync.md) | Next: [25-sliding-window-rate-limiter-with-cleanup.md](25-sliding-window-rate-limiter-with-cleanup.md)
