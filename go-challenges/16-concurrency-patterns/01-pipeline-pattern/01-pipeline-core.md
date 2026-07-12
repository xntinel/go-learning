# Exercise 1: The Pipeline Core

A pipeline is built from one repeated shape — a function that takes an inbound channel and returns an outbound one — so the whole pattern is learnable from a single small package. This exercise builds that package: a source stage, three transform stages, a fan-in stage, and the close-and-cancel contract that ties them together, all verified by tests rather than by a printing `main`.

This module is fully self-contained. It begins with its own `go mod init`, defines every stage it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pipeline.go              Generate, Square, Filter, Multiply, Format, Merge
pipeline_test.go         close contract, composition, cancellation, fan-in under load
cmd/
  demo/
    main.go              compose Generate -> Filter -> Multiply -> Format and drain it
```

- Files: `pipeline.go`, `pipeline_test.go`, `cmd/demo/main.go`.
- Implement: the source `Generate`, the transforms `Square`, `Filter`, `Multiply`, `Format`, and the fan-in `Merge`, each honoring a shared `done` channel.
- Test: the close contract, end-to-end composition, leak-free cancellation, and a race-free fan-in under load.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/01-pipeline-pattern/01-pipeline-core/cmd/demo && cd go-solutions/16-concurrency-patterns/01-pipeline-pattern/01-pipeline-core
```

### Every stage has the same skeleton

Read any one stage and you have read them all. A stage makes its outbound channel, launches a goroutine with `defer close(out)` as its first line, loops over the inbound channel with `for n := range in`, and sends each transformed value inside a `select` that also watches `done`. The `defer close(out)` is the close contract: it runs once, on every return path, so the downstream `range` always terminates whether the stage finished naturally or was cancelled. The `select` is the cancellation contract: the moment `done` is closed, the send that was about to block instead takes the `<-done` branch and returns, the deferred close fires, and the channel closes cleanly with no leaked goroutine. `Generate` is the same skeleton with a `range nums` source loop instead of a `range in` channel loop, because a source has no inbound channel.

The outbound channel is returned as `<-chan int` (receive-only), which is what makes the stages compose safely: a downstream stage receives the channel but the type system forbids it from sending into or closing a channel it does not own. `Filter` shows that a stage's transform is just a function parameter — the `keep func(int) bool` predicate — so the same skeleton specializes to any per-element decision without a new type.

`Merge` is the one stage with multiple senders, and it needs the `WaitGroup` recipe instead of a bare `defer close`. Each inbound channel gets its own `output` goroutine that copies values to the shared `out` and calls `wg.Done()` when its channel drains; a separate closer goroutine waits for all of them and closes `out` exactly once. `wg.Add(len(cs))` runs before the goroutines launch, so the closer's `wg.Wait()` can never observe a premature zero. A `Merge` of zero channels closes `out` immediately, which is the correct empty case.

Create `pipeline.go`:

```go
// Package pipeline implements the Ajmani pipeline pattern: stages connected by
// channels, each closing its outbound channel when done and honoring a shared
// done channel for cancellation.
package pipeline

import (
	"fmt"
	"sync"
)

// Generate is the source stage. It emits each value in nums, stopping early if
// done is closed.
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

// Square emits the square of each inbound value.
func Square(done <-chan struct{}, in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for n := range in {
			select {
			case out <- n * n:
			case <-done:
				return
			}
		}
	}()
	return out
}

// Filter forwards only the values for which keep returns true.
func Filter(done <-chan struct{}, in <-chan int, keep func(int) bool) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for n := range in {
			if !keep(n) {
				continue
			}
			select {
			case out <- n:
			case <-done:
				return
			}
		}
	}()
	return out
}

// Multiply scales each inbound value by factor.
func Multiply(done <-chan struct{}, in <-chan int, factor int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for n := range in {
			select {
			case out <- n * factor:
			case <-done:
				return
			}
		}
	}()
	return out
}

// Format renders each inbound value as a "result: N" string. It is a sink-shaped
// transform: int in, string out.
func Format(done <-chan struct{}, in <-chan int) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for n := range in {
			select {
			case out <- fmt.Sprintf("result: %d", n):
			case <-done:
				return
			}
		}
	}()
	return out
}

// Merge fans several inbound channels into one. The outbound channel is closed
// only after every inbound channel has drained, using the WaitGroup recipe.
func Merge(done <-chan struct{}, cs ...<-chan int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup

	output := func(c <-chan int) {
		defer wg.Done()
		for n := range c {
			select {
			case out <- n:
			case <-done:
				return
			}
		}
	}

	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

### The runnable demo

A demo proves the package composes end to end. It builds a four-stage pipeline — generate ten numbers, keep the even ones, scale them by ten, format them — and drains the final channel with a `range`. The `defer close(done)` releases any goroutines still parked if `main` returned early; here it simply tidies up after a full drain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipeline"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	nums := pipeline.Generate(done, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	evens := pipeline.Filter(done, nums, func(n int) bool { return n%2 == 0 })
	scaled := pipeline.Multiply(done, evens, 10)
	out := pipeline.Format(done, scaled)

	for line := range out {
		fmt.Println(line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
result: 20
result: 40
result: 60
result: 80
result: 100
```

### Tests

The tests pin the four properties the package must have. `TestGenerateSquaresAndFormats` and `TestFilterKeepsEvenThenMultiplies` check that the stages compose and transform correctly. `TestFormatPrefix` pins the single-value formatting. `TestCancellationDrainsAndStops` closes `done` mid-stream and asserts the pipeline terminates without hanging and without leaking — it deliberately does not assert an exact count, because the select between send and cancel is non-deterministic. `TestMergeCombinesMultipleSources`, `TestMergeIsRaceFreeUnderLoad`, `TestMergeOfEmptyChannelsClosesImmediately`, and `TestMergeCloseAfterAllWriters` pin the fan-in close recipe, including the race-free behavior under load and the empty degenerate case.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func collectStrings(ch <-chan string) []string {
	var got []string
	for s := range ch {
		got = append(got, s)
	}
	return got
}

func TestGenerateSquaresAndFormats(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	out := Format(done, Square(done, Generate(done, 2, 3, 4, 5)))
	got := collectStrings(out)

	want := []string{"result: 4", "result: 9", "result: 16", "result: 25"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFilterKeepsEvenThenMultiplies(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	out := Format(done, Multiply(done, Filter(done, Generate(done, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10), func(n int) bool { return n%2 == 0 }), 10))

	want := []string{"result: 20", "result: 40", "result: 60", "result: 80", "result: 100"}
	got := collectStrings(out)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFormatPrefix(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	out := Format(done, Square(done, Generate(done, 7)))
	got := collectStrings(out)

	want := []string{"result: 49"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestCancellationDrainsAndStops verifies the leak-free cancellation contract:
// closing done mid-stream terminates upstream and the outbound channel still
// closes, so the drain loop ends instead of hanging. It does NOT assert an exact
// count: the select between "out <- v" and "<-done" is resolved randomly when
// both are ready, so one extra value may arrive after the close.
func TestCancellationDrainsAndStops(t *testing.T) {
	t.Parallel()

	// A source with no values closes immediately; nothing downstream emits.
	done := make(chan struct{})
	empty := Square(done, Generate(done))
	count := 0
	for range empty {
		count++
	}
	close(done)
	if count != 0 {
		t.Fatalf("empty pipeline emitted %d values, want 0", count)
	}

	// A long source cancelled after three values must still terminate. We collect
	// three, signal cancellation, then drain the rest and assert the loop ends.
	done2 := make(chan struct{})
	out := Square(done2, Generate(done2, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10))

	collected := 0
	for range out {
		collected++
		if collected == 3 {
			close(done2)
			break
		}
	}

	// Drain whatever is left. If a goroutine leaked or out never closed, this
	// loop would block and the test would time out.
	drained := 0
	for range out {
		drained++
	}
	if collected < 3 {
		t.Fatalf("collected %d before cancel, want at least 3", collected)
	}
	if collected+drained > 10 {
		t.Fatalf("received %d total, want at most 10", collected+drained)
	}
}

func TestMergeCombinesMultipleSources(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	a := Generate(done, 1, 2, 3)
	b := Generate(done, 10, 20, 30)
	out := Merge(done, a, b)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 6 {
		t.Fatalf("merged len = %d, want 6", len(got))
	}
	sum := 0
	for _, v := range got {
		sum += v
	}
	if sum != 66 {
		t.Fatalf("sum = %d, want 66", sum)
	}
}

func TestMergeIsRaceFreeUnderLoad(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	const sources = 8
	const perSource = 200
	var ins []<-chan int
	for i := 0; i < sources; i++ {
		base := i * perSource
		nums := make([]int, perSource)
		for j := 0; j < perSource; j++ {
			nums[j] = base + j
		}
		ins = append(ins, Generate(done, nums...))
	}

	out := Merge(done, ins...)
	seen := make(map[int]bool)
	for v := range out {
		if seen[v] {
			t.Fatalf("duplicate value %d", v)
		}
		seen[v] = true
	}
	if len(seen) != sources*perSource {
		t.Fatalf("got %d unique, want %d", len(seen), sources*perSource)
	}
}

func TestMergeOfEmptyChannelsClosesImmediately(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	out := Merge(done)
	select {
	case _, ok := <-out:
		if ok {
			t.Fatalf("expected closed channel, got value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Merge of empty inputs did not close")
	}
}

// TestMergeCloseAfterAllWriters guards the "forgot to wait for all writers" bug:
// closing before every sender is done would panic on send under -race.
func TestMergeCloseAfterAllWriters(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	var wg sync.WaitGroup
	wg.Add(1)
	in := make(chan int)
	go func() {
		defer wg.Done()
		defer close(in)
		for i := 0; i < 50; i++ {
			select {
			case in <- i:
			case <-done:
				return
			}
		}
	}()

	out := Merge(done, in)
	count := 0
	for range out {
		count++
	}
	wg.Wait()
	if count != 50 {
		t.Fatalf("count = %d, want 50", count)
	}
}
```

## Review

The package is correct when every stage obeys both halves of the contract. Confirm each transform opens its goroutine with `defer close(out)` so the downstream `range` always ends, and wraps every send in a `select` on `done` so an early consumer cannot leak it. The fan-in is the special case: `Merge` must call `wg.Add(len(cs))` before launching the per-channel goroutines and close `out` only from the closer goroutine after `wg.Wait()`, so a `Merge` of one channel, of many channels under load, and of zero channels all close exactly once. The whole package passing under `go test -race` is what establishes there is no close-before-send and no leaked goroutine.

Common mistakes for this feature. The sharpest is testing cancellation by asserting an exact count: because the select between `out <- v` and `<-done` is resolved at random when both are ready, an extra value can slip through after the close, so the only sound assertions are "it terminates" and "the count is bounded," which is what `TestCancellationDrainsAndStops` checks by draining to completion rather than pinning a number. The second is a double close — `defer close(out)` plus an explicit `close(out)` — which panics; the deferred close alone satisfies close-exactly-once. The third is closing from a receiver to stop a producer, which panics the next send; the `done` channel is the only correct stop signal. The fourth is launching the `Merge` goroutines before `wg.Add`, which lets the closer observe a premature zero and close `out` mid-send.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical write-up of the pattern, the close contract, and the `done`-channel cancellation idiom this exercise implements.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) — channels, goroutines, and the "do not communicate by sharing memory" model the pipeline rests on.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the `Add`/`Done`/`Wait` primitive that delays the fan-in close until every writer finishes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-etl-pipeline-service.md](02-etl-pipeline-service.md)
