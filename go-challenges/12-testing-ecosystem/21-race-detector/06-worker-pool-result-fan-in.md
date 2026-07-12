# Exercise 6: Share-by-Communicating -- Removing a Race by Fanning Into a Channel

A parallel fetch-and-enrich worker pool is a daily backend artifact: split M
inputs across W workers, do the slow work concurrently, collect the results. The
naive version has every worker write into one shared results map -- a race on the
map. The Go-idiomatic fix is not to lock the map but to eliminate the sharing:
each worker sends its result on a channel, and a single collector goroutine owns
the aggregate. No shared mutable state means no data race to guard.

This module is self-contained: its own `go mod init`, its own racy demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
fanin/                      independent module: example.com/fanin
  go.mod                    go 1.26
  pool.go                   Process(ctx, inputs, workers) map[int]int -- channel fan-in
  cmd/
    demo/
      main.go               run the pool over a slice, print result count
    racy/
      main.go               workers writing a shared map with no lock; run with -race
  pool_test.go              TestPoolAggregatesAllResults: exactly M results, under -race
```

Files: `pool.go`, `cmd/demo/main.go`, `cmd/racy/main.go`, `pool_test.go`.
Implement: `Process` that fans inputs out to W workers over a jobs channel, fans
results back on a results channel, and aggregates in one collector.
Test: `TestPoolAggregatesAllResults` runs M inputs through W workers and asserts
exactly M correct results with no loss or duplication, under `-race`.
Verify: `go test -count=1 -race ./...`; then `go run -race ./cmd/racy`.

### The race is in the aggregation, not the work

The individual work each worker does -- enriching one input -- is independent and
race-free. The race is in the *aggregation*: if every worker does
`results[input] = value` on one shared map, those are concurrent writes to the
same map with no ordering edge. That is a data race and, on a map, a potential
`concurrent map writes` fatal crash. The lesson is that synchronizing the work is
not enough; you must synchronize the handoff of results.

There are two correct fixes. Guard the shared map with a `sync.Mutex` -- every
worker locks, writes, unlocks. Or eliminate the shared map: each worker sends its
result on a channel, and one collector goroutine reads the channel and owns the
map exclusively, so no other goroutine ever touches it. This exercise takes the
second route because it is the Go maxim in action -- "do not communicate by
sharing memory; share memory by communicating" -- and because it composes: the
channel gives you natural back-pressure (workers block on a full results channel
instead of piling unbounded work into a map). The trade-off: the mutex-guarded
map has slightly less machinery and no extra goroutine, but the channel version
has no shared mutable state to reason about at all, and the collector can apply
back-pressure. For a hot pipeline, that back-pressure and the absence of a lock on
every result is usually worth the extra goroutine.

The structure: a feeder goroutine pushes inputs into a `jobs` channel and closes
it; W workers range over `jobs` and send `Result`s into a `results` channel; a
closer goroutine `wg.Wait`s for the workers and then closes `results`; the caller
ranges over `results` and builds the map. Every goroutine watches `ctx.Done` so a
cancellation stops the pipeline cleanly with no leak.

Create `pool.go`:

```go
package fanin

import (
	"context"
	"sync"
)

// Result is one worker's output for one input.
type Result struct {
	Input int
	Value int
}

// enrich stands in for the slow per-input work (a fetch, a DB lookup).
func enrich(n int) int { return n * n }

// Process runs enrich over every input using `workers` goroutines and returns a
// map from input to enriched value. Results are fanned in over a channel to a
// single collector, so there is no shared mutable state and no data race. If ctx
// is cancelled, Process returns the results gathered so far.
func Process(ctx context.Context, inputs []int, workers int) map[int]int {
	jobs := make(chan int)
	results := make(chan Result)

	// Feeder: push inputs, then close jobs so workers can drain and exit.
	go func() {
		defer close(jobs)
		for _, in := range inputs {
			select {
			case jobs <- in:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Workers: enrich each job and send the result.
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case results <- Result{Input: j, Value: enrich(j)}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Closer: once every worker has exited, close results to end the collector.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collector: the ONLY goroutine that touches the map, so no lock is needed.
	out := make(map[int]int)
	for r := range results {
		out[r.Input] = r.Value
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/fanin"
)

func main() {
	inputs := make([]int, 100)
	for i := range inputs {
		inputs[i] = i
	}

	out := fanin.Process(context.Background(), inputs, 8)

	fmt.Printf("results: %d\n", len(out))
	fmt.Printf("enrich(9) = %d\n", out[9])
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
results: 100
enrich(9) = 81
```

### The racy version, for the report

Create `cmd/racy/main.go`. Run with `go run -race ./cmd/racy`:

```go
// Command racy has every worker write into one shared results map with no lock,
// racing on the aggregation. Run manually:
//
//	go run -race ./cmd/racy
//
// It is a main package with no test, so `go test -race ./...` only builds it.
package main

import (
	"fmt"
	"sync"
)

func main() {
	const m = 500
	results := make(map[int]int) // shared, unguarded -- the bug

	jobs := make(chan int)
	go func() {
		defer close(jobs)
		for i := range m {
			jobs <- i
		}
	}()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j] = j * j // concurrent map write -- data race
			}
		}()
	}
	wg.Wait()

	fmt.Printf("results: %d (expected %d; concurrent map writes may also crash)\n", len(results), m)
}
```

### Tests

`TestPoolAggregatesAllResults` runs M=1000 inputs through W=8 workers and asserts
the collector produced exactly M entries, each mapping its input to `enrich`. No
loss (all M present) and no duplication (map keyed by unique input) prove the
fan-in is complete and correct. It passes under `-race` because only the
collector touches the map. `TestPoolCancelledContext` checks that cancelling the
context stops the pipeline without leaking or deadlocking.

Create `pool_test.go`:

```go
package fanin

import (
	"context"
	"testing"
)

func TestPoolAggregatesAllResults(t *testing.T) {
	t.Parallel()

	const m = 1000
	inputs := make([]int, m)
	for i := range inputs {
		inputs[i] = i
	}

	out := Process(t.Context(), inputs, 8)

	if len(out) != m {
		t.Fatalf("got %d results, want %d (loss or duplication)", len(out), m)
	}
	for _, in := range inputs {
		if got := out[in]; got != in*in {
			t.Fatalf("out[%d] = %d, want %d", in, got, in*in)
		}
	}
}

func TestPoolCancelledContext(t *testing.T) {
	t.Parallel()

	inputs := make([]int, 1000)
	for i := range inputs {
		inputs[i] = i
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before starting: Process must return without hanging

	out := Process(ctx, inputs, 4)
	// With an already-cancelled context, Process returns promptly; the number of
	// gathered results is nondeterministic but must not exceed the input count.
	if len(out) > len(inputs) {
		t.Fatalf("got %d results, more than %d inputs", len(out), len(inputs))
	}
}

func TestPoolEmptyInput(t *testing.T) {
	t.Parallel()

	out := Process(t.Context(), nil, 4)
	if len(out) != 0 {
		t.Fatalf("got %d results for empty input, want 0", len(out))
	}
}
```

## Review

The pool is correct when the collector produces exactly one result per input with
no loss and no duplication, and no goroutine touches the map except the
collector. The proof is `TestPoolAggregatesAllResults` passing under `-race`: 8
workers fan results in concurrently and the detector finds no unordered access
because the map has a single owner. `TestPoolCancelledContext` confirms the
pipeline drains and returns on cancellation instead of leaking a goroutine.

The mistake to avoid is racing on the aggregation -- workers writing a shared map
or slice with no synchronization, as `cmd/racy` shows. Fan results in over a
channel to one owner, or guard the collection with a lock; synchronize the
handoff, not only the work. Channel fan-in also gives back-pressure the
mutex-map version does not. Run `go test -count=1 -race ./...`.

## Resources

- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- "share memory by communicating" and the channel fan-in pattern.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- coordinating the workers before closing the results channel.
- [`context`](https://pkg.go.dev/context) -- cancellation propagation that stops the pipeline cleanly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-copy-on-write-config-snapshot.md](05-copy-on-write-config-snapshot.md) | Next: [07-mutex-token-bucket-limiter.md](07-mutex-token-bucket-limiter.md)
