# Exercise 1: Merging Many Channels Into One

The canonical fan-in takes a variadic set of input channels and returns one output channel that carries every value from every input, closing only after the last input drains. This exercise builds that `Merge` together with the small producer and transformation stages a pipeline uses around it, so the package has a complete producer-merge-consumer surface you can run end to end.

This module is fully self-contained: it has its own `go mod init`, defines every symbol it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
fanin.go             Generate, Square, Merge (sync.WaitGroup fan-in)
cmd/
  demo/
    main.go          generate two streams, merge, square, print the sorted result
fanin_test.go        two-source correctness, empty-input close, race-free load, slowest-source wait
```

- Files: `fanin.go`, `cmd/demo/main.go`, `fanin_test.go`.
- Implement: `Merge(cs ...<-chan int) <-chan int`, plus the `Generate` and `Square` pipeline stages.
- Test: combine two sources (set-compared), assert `Merge()` with no inputs closes at once, push 8000 values through 16 sources under `-race` with no duplicates or losses, and prove the close waits for the slowest source.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/03-fan-in-pattern/01-merging-channels/cmd/demo && cd go-solutions/16-concurrency-patterns/03-fan-in-pattern/01-merging-channels
```

### Why a WaitGroup, and why the Add comes first

The merge starts one goroutine per input. Each goroutine ranges its input and forwards every value to the shared output; ranging ends exactly when that input closes, so the goroutine's lifetime is bounded by its source. The only hard part is closing the output: a send on a closed channel panics, so the output must be closed once, after every forwarder has stopped, and by code that is not itself a forwarder.

A `sync.WaitGroup` expresses "after every forwarder has stopped" directly. The counter is set to the number of inputs; each forwarder decrements it with `wg.Done` as it exits; a dedicated closer goroutine blocks in `wg.Wait` until the counter hits zero and then performs the single `close(out)`. The ordering is mandatory and is the part most people get wrong: `wg.Add(len(cs))` must run synchronously on the calling goroutine, before the forwarders spawn, and certainly before the closer spawns. If `Add` ran inside each forwarder instead, the closer could call `Wait` on a still-zero counter, see zero, close the output, and every forwarder would then panic on its first send. Because `Add` completes before the calling goroutine reaches any `go` statement, the counter is already correct when the closer's `Wait` first observes it.

The empty case falls out for free. With no inputs, `wg.Add(0)` leaves the counter at zero, the closer's `Wait` returns immediately, and `out` is closed before any value is sent — so `for v := range Merge()` simply ends. The order across sources is not preserved: each forwarder runs independently, so values from different inputs interleave in scheduler-decided order. Order within a single source is preserved, because that source's forwarder copies its values in receive order.

Create `fanin.go`:

```go
package fanin

import "sync"

// Generate returns a channel that emits each of nums in order, then closes.
// It is the producer stage used to feed the pipeline in tests and the demo.
func Generate(nums ...int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for _, n := range nums {
			out <- n
		}
	}()
	return out
}

// Square reads each value from in, emits its square, and closes out when in
// closes. It is an example upstream transformation stage; Merge does not depend
// on it, but a realistic pipeline places transforms before and after the merge.
func Square(in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for n := range in {
			out <- n * n
		}
	}()
	return out
}

// Merge multiplexes every input channel onto a single output channel. It starts
// one goroutine per input, each forwarding that input's values to out, and a
// dedicated closer goroutine that closes out only after every forwarder has
// finished. Order is preserved within each source but not across sources.
//
// Do not pass a nil channel: a goroutine ranging a nil channel blocks forever,
// never calls wg.Done, and out is never closed.
func Merge(cs ...<-chan int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup

	forward := func(c <-chan int) {
		defer wg.Done()
		for n := range c {
			out <- n
		}
	}

	// Add must complete before any goroutine that could call Wait exists.
	wg.Add(len(cs))
	for _, c := range cs {
		go forward(c)
	}

	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

`forward` decrements the counter with `defer wg.Done()` so the decrement happens on every exit path, even though the only exit here is the clean end of the range. The closer goroutine is the lone owner of `close(out)`. Returning `out` before the goroutines have forwarded anything is intentional: the caller ranges the result while the producers run, which is what makes a pipeline stream rather than buffer.

### The runnable demo

The demo wires the full surface: two generated streams are merged, the merged stream is squared, and the squared values are collected and sorted before printing. Sorting is what makes the output deterministic — the merge interleaves the two sources unpredictably, so the program sorts before comparing, exactly as the tests do.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/fanin"
)

func main() {
	a := fanin.Generate(1, 2, 3)
	b := fanin.Generate(10, 20, 30)
	squared := fanin.Square(fanin.Merge(a, b))

	var got []int
	for v := range squared {
		got = append(got, v)
	}
	sort.Ints(got)
	fmt.Println(got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[1 4 9 100 400 900]
```

### Tests

The tests pin the four properties a merge must have. `TestMergeCombinesTwoSources` set-compares the union of two streams, never asserting an order. `TestMergeEmptyInputsClosesImmediately` proves the no-input case closes rather than hanging. `TestMergeManyChannelsRaceFree` pushes 8000 distinct values through 16 sources and asserts every value arrives exactly once — run under `-race`, it is what catches a misordered `Add`. `TestMergeWaitsForSlowestSource` pairs a fast source with one that sleeps between sends and asserts all six values arrive, proving the closer waits on the slowest forwarder rather than closing early.

Create `fanin_test.go`:

```go
package fanin

import (
	"sort"
	"testing"
	"time"
)

func TestMergeCombinesTwoSources(t *testing.T) {
	t.Parallel()

	out := Merge(Generate(1, 2, 3), Generate(10, 20, 30))

	var got []int
	for v := range out {
		got = append(got, v)
	}

	sort.Ints(got)
	want := []int{1, 2, 3, 10, 20, 30}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeEmptyInputsClosesImmediately(t *testing.T) {
	t.Parallel()

	out := Merge()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed channel, got a value")
		}
	case <-time.After(time.Second):
		t.Fatal("Merge() with no inputs did not close")
	}
}

func TestMergeManyChannelsRaceFree(t *testing.T) {
	t.Parallel()

	const sources, perSource = 16, 500
	channels := make([]<-chan int, sources)
	for i := range sources {
		base := i * perSource
		nums := make([]int, perSource)
		for j := range perSource {
			nums[j] = base + j
		}
		channels[i] = Generate(nums...)
	}

	seen := make(map[int]bool)
	for v := range Merge(channels...) {
		if seen[v] {
			t.Fatalf("duplicate value %d", v)
		}
		seen[v] = true
	}
	if len(seen) != sources*perSource {
		t.Fatalf("got %d unique values, want %d", len(seen), sources*perSource)
	}
}

func TestMergeWaitsForSlowestSource(t *testing.T) {
	t.Parallel()

	fast := Generate(1, 2, 3)

	slow := make(chan int)
	go func() {
		defer close(slow)
		for i := range 3 {
			time.Sleep(2 * time.Millisecond)
			slow <- 100 + i
		}
	}()

	var got []int
	for v := range Merge(fast, slow) {
		got = append(got, v)
	}
	sort.Ints(got)
	want := []int{1, 2, 3, 100, 101, 102}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

## Review

The merge is correct when one rule holds: only the closer goroutine closes the output, and it does so only after `wg.Wait` returns. Confirm `wg.Add(len(cs))` runs before the forwarders spawn — moving it inside `forward` reintroduces the zero-counter race, which `-race` plus `TestMergeManyChannelsRaceFree` will flag. Confirm the empty case closes instead of hanging, which is what `TestMergeEmptyInputsClosesImmediately` guards with a timeout. The slow-source test proves the close is gated on the last forwarder, not the first: a closer that did not wait would drop the slow source's values and the set-compare would fail.

The common traps are all variations on closing wrong. Closing from inside a forwarder makes the first finisher kill the rest with a panic. Spawning the closer before counting the forwarders lets `Wait` see zero and close early. Asserting a fixed cross-source order produces a flaky test, because the interleaving is scheduler-decided — sort or set-compare instead. And never hand `Merge` a `nil` channel: the goroutine ranging it blocks forever, so the `WaitGroup` never reaches zero and the output never closes.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-out/fan-in article; its `merge` is the exact `sync.WaitGroup` recipe this exercise implements.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the contract behind `Add` before spawn, `Done` per worker, and `Wait` in the closer.
- [Go Concurrency Patterns (2012 talk)](https://go.dev/talks/2012/concurrency.slide) — Rob Pike's original framing of fan-in as multiplexing several channels onto one.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-ordered-shard-merge.md](02-ordered-shard-merge.md)
