# Exercise 1: Fan-Out Core

Fan-out is the practice of starting several goroutines that all read from one inbound channel, compute independently, and merge their results onto one outbound channel. This module builds the reusable core of that pattern: a generic `FanOut` that spreads any transform across N workers, paired with a cancellable `Generate` source.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
fanout.go            Generate (cancellable source), FanOut[In, Out] (N workers -> one channel)
cmd/
  demo/
    main.go          square 1..10 across 4 workers, sort the merged results, print count+sum
fanout_test.go       set correctness, exactly-once delivery under -race, single-worker ordering, zero-workers close
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `Generate(done <-chan struct{}, nums ...int) <-chan int` and the generic `FanOut[In, Out any](numW int, in <-chan In, fn func(In) Out) <-chan Out`.
- Test: every input appears on the output exactly once, one worker preserves order, zero workers still closes the output channel, all under `-race`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/02-fan-out-pattern/01-fan-out-core/cmd/demo && cd go-solutions/16-concurrency-patterns/02-fan-out-pattern/01-fan-out-core
```

### Why one inbound channel and one closer goroutine

The whole pattern rests on a property of Go channels: when many goroutines receive from the same channel, the runtime hands each value to exactly one of them. That is what makes fan-out safe without any locking. The producer sends each job once; the runtime partitions those jobs across whichever workers happen to be ready. Work is divided by arrival, not by content, and no two workers ever see the same value.

The hard part is not the workers, it is the lifetime of the shared output channel. Every worker writes to one `out` channel, but a channel may be closed only once, and only after the last send. If a worker closed `out` with `defer close(out)`, the first worker to finish would close it and the next worker's send would panic with `send on closed channel`. The fix is to make closing somebody else's job: a single dedicated goroutine waits on a `sync.WaitGroup` that every worker decrements with `defer wg.Done()`, and only when `wg.Wait` returns — meaning every worker has stopped sending — does it `close(out)`. The ordering rule that makes this correct is that `wg.Add(numW)` must run before any `go worker`; if `Add` raced with the workers, `Wait` could observe zero and close `out` while workers are still running.

`FanOut` is generic over the input and output types because fan-out is mechanism, not arithmetic: the same dispatcher-plus-closer skeleton squares integers, parses lines, or resizes images depending only on the `fn` you pass. The workers `range` over the input channel, so they drain naturally and exit when the producer closes it; that close cascades through every worker, each calls `wg.Done`, and the closer fires once.

`Generate` is the source, and it takes a `done` channel for one reason: a goroutine that sends on an unbuffered channel blocks until someone receives, so if the consumer stops reading early, the sender would block forever and leak. Selecting on `done` lets the source abandon a half-sent stream. This also explains the zero-workers case: with `numW == 0` nothing drains the input, the closer fires immediately and `out` closes, and the still-blocked `Generate` is released only when the caller closes `done` — which is exactly why the source must be cancellable.

Create `fanout.go`:

```go
package fanout

import "sync"

// Generate emits each of nums on the returned channel and then closes it. The
// done channel lets a caller abandon the stream early so the sending goroutine
// cannot leak when the consumer stops reading before the input is exhausted.
func Generate(done <-chan struct{}, nums ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, n := range nums {
			select {
			case out <- n:
			case <-done:
				return
			}
		}
	}()
	return out
}

// FanOut starts numW goroutines that all read from in, apply fn to every value,
// and write the results to one shared output channel. The output channel is
// closed exactly once, after every worker has returned, by a single dedicated
// closer goroutine. Order is not preserved: a result's position on the output
// channel reflects which worker finished first, not the input order.
func FanOut[In, Out any](numW int, in <-chan In, fn func(In) Out) <-chan Out {
	out := make(chan Out)

	var wg sync.WaitGroup
	wg.Add(numW)
	for range numW {
		go func() {
			defer wg.Done()
			for v := range in {
				out <- fn(v)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

Read `FanOut` as two cooperating groups of goroutines. The workers all loop `for v := range in`, so they share the input and each handles a disjoint subset of values; the closer is the only goroutine that touches `close(out)`, and it does so strictly after `wg.Wait`. Order is deliberately not preserved: a fast worker can finish job 5 before a slow worker finishes job 2, so results arrive in completion order, not input order. If you need input order back, that is the job of the next exercise, not of fan-out itself.

### The runnable demo

Because four workers race on the input, the order of results is nondeterministic, so a demo that printed them as they arrived could never have a truthful fixed expected output. The honest move is to collect the merged results and sort them: the set of results is deterministic even though their arrival order is not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/fan-out-core"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	in := fanout.Generate(done, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	out := fanout.FanOut(4, in, func(n int) int { return n * n })

	// Four workers race on the same input channel, so results arrive in a
	// nondeterministic order. Sort before printing to get a stable, truthful
	// expected output; the set of results is deterministic, the order is not.
	var got []int
	for v := range out {
		got = append(got, v)
	}
	sort.Ints(got)

	fmt.Printf("results (sorted): %v\n", got)
	sum := 0
	for _, v := range got {
		sum += v
	}
	fmt.Printf("count=%d sum=%d\n", len(got), sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
results (sorted): [1 4 9 16 25 36 49 64 81 100]
count=10 sum=385
```

### Tests

The tests pin the properties fan-out must have. `TestFanOutSquaresEveryInput` sorts the merged output and checks the value set. `TestFanOutDeliversEveryInputExactlyOnce` fans 1000 values across 8 workers and asserts each result appears exactly once — the core correctness claim, and the one the race detector backs by flagging any unsynchronised access to shared state. `TestFanOutOneWorkerPreservesOrder` documents that a single worker is just a sequential pipeline, so order survives. `TestFanOutZeroWorkersClosesOutput` proves the closer still closes `out` when there are no workers, so a consumer's `range` terminates instead of hanging.

Create `fanout_test.go`:

```go
package fanout

import (
	"sort"
	"testing"
)

func square(n int) int { return n * n }

func TestFanOutSquaresEveryInput(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	in := Generate(done, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	out := FanOut(4, in, square)

	var got []int
	for v := range out {
		got = append(got, v)
	}

	want := []int{1, 4, 9, 16, 25, 36, 49, 64, 81, 100}
	sort.Ints(got)
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted got %v, want %v", got, want)
		}
	}
}

func TestFanOutDeliversEveryInputExactlyOnce(t *testing.T) {
	t.Parallel()

	const n = 1000
	nums := make([]int, n)
	for i := range nums {
		nums[i] = i
	}

	done := make(chan struct{})
	defer close(done)

	in := Generate(done, nums...)
	out := FanOut(8, in, square)

	seen := make(map[int]int, n)
	for v := range out {
		seen[v]++
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct results, want %d", len(seen), n)
	}
	for v, c := range seen {
		if c != 1 {
			t.Fatalf("result %d appeared %d times, want exactly 1", v, c)
		}
	}
}

func TestFanOutOneWorkerPreservesOrder(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	in := Generate(done, 5, 6, 7, 8)
	out := FanOut(1, in, square)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	want := []int{25, 36, 49, 64}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (one worker must keep input order)", got, want)
		}
	}
}

func TestFanOutZeroWorkersClosesOutput(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	in := Generate(done, 1, 2, 3)
	out := FanOut(0, in, square)

	count := 0
	for range out {
		count++
	}
	if count != 0 {
		t.Fatalf("got %d results from 0 workers, want 0", count)
	}
}
```

## Review

The pattern is correct when exactly one goroutine ever closes the shared output and it does so only after `wg.Wait`. Confirm `wg.Add(numW)` runs before the worker loop, that workers close nothing, and that the closer is the single `go func() { wg.Wait(); close(out) }()`. The exactly-once test under `-race` is the real proof: every input must surface on the output once and only once, with no data race on the map or the channel.

Common mistakes for this pattern. The first is closing the output from inside a worker, which panics the moment a second worker sends. The second is forgetting the closer entirely and returning `out` directly, which hangs the consumer's `range` forever because the channel never closes. The third is adding a large buffer to `out` to paper over a slow consumer: the buffer fills, workers block on send, and the parallelism you wanted disappears — size buffers to a backlog you actually mean to hold, and apply real backpressure upstream instead. The fourth is a source goroutine with no cancellation: if the consumer stops early, an unbuffered send blocks forever and leaks the goroutine, which is why `Generate` selects on `done`.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical description of fan-out, fan-in, and the `done`-channel cancellation this module uses.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the exact semantics of `Add`/`Done`/`Wait` that the single closer goroutine relies on.
- [Go spec: Channel types and the close builtin](https://go.dev/ref/spec#Close) — why a channel may be closed once and what a send on a closed channel does.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-parallel-record-enrichment.md](02-parallel-record-enrichment.md)
