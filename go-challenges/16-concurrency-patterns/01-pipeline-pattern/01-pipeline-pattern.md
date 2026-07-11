# 1. Pipeline Pattern: Stages Connected By Channels

A pipeline is a series of stages connected by channels. Each stage is a group of
goroutines running the same function: it reads values from one or more inbound
channels, runs a transformation, and sends results on an outbound channel. The
first stage is a *source*; the last stage is a *sink*. This is the pattern
Sameer Ajmani documented as "Go Concurrency Patterns: Pipelines and
cancellation" on the Go blog.

```text
pipeline/
  go.mod
  internal/pipeline/pipeline.go
  internal/pipeline/pipeline_test.go
  cmd/pipelinedemo/main.go
```

The package exposes `Generate`, `Square`, `Filter`, `Multiply`, and `Format`
stages, plus a `Merge` fan-in used by later lessons. Each stage returns
`<-chan int` or `<-chan string`. The lesson's verification is `go test
-count=1 -race`, not a `main()` that prints.

## Concepts

### A Stage Is A Function Returning A Channel

A stage is `func stage(in <-chan T) <-chan U`. Inside, a goroutine loops over
`in` and sends results to `out`. When the loop finishes because `in` was closed,
the goroutine closes `out` with `defer close(out)`. The next stage's `for v :=
range in` exits because of the close, and so on down the chain. This is the
canonical Ajmani pattern: every stage closes its outbound channel when it is
done sending.

### Close Ownership Is Strict

Only the sender closes. The Go blog states it bluntly: "Sends on a closed
channel panic, so it's important to ensure all sends are done before calling
close." For a single-sender stage, `defer close(out)` is enough. For a fan-in
stage with multiple senders, the standard recipe is a `sync.WaitGroup` plus a
separate goroutine that closes after `wg.Wait()` returns. The lesson's
`pipeline.Merge` uses exactly that recipe.

### Cancellation Uses A `done` Channel Of `struct{}`

A consumer that stops early would otherwise leak the upstream goroutines
blocked on `out <- v`. The canonical fix is a `done <-chan struct{}` shared by
every stage. The consumer `defer close(done)`, and every send in every stage
becomes:

```go
select {
case out <- v:
case <-done:
    return
}
```

The empty struct costs zero bytes; the receive event is the signal. Closing
`done` broadcasts to every stage at once because a receive on a closed channel
always proceeds.

### Buffered Channels Are Not A Substitute For Cancellation

Adding `make(chan int, N)` to the outbound channel fixes a specific leak when
the producer's count is known. But as soon as `N` is wrong, or a new stage is
added, the leak returns. The Go blog warns this is "bad code": fragile and
hard to reason about. Prefer `done`.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/pipeline/internal/pipeline ~/go-exercises/pipeline/cmd/pipelinedemo
cd ~/go-exercises/pipeline
go mod init example.com/pipeline
```

This is a library, not a program. Verification is `go test`.

### Exercise 1: Generate, Square, And The Close Contract

Create `internal/pipeline/pipeline.go`:

```go
package pipeline

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
```

Each stage honours `done` on every send. The deferred `close(out)` runs on
every return path, including the cancellation branch.

### Exercise 2: Reusable Stages (Filter, Multiply, Format)

```go
package pipeline

import "fmt"

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
```

`Filter` is a stage the same way `Square` is: `func(in <-chan T) <-chan T`. The
predicate is a parameter, not a method on a struct, so the stage stays
composable with anything that produces or consumes `int` channels.

### Exercise 3: The Fan-In Stage

`Merge` is the fan-in primitive used in lessons 02 and 03. Multiple goroutines
write to a single channel, so the close must wait until every writer is done.
The textbook recipe is `WaitGroup`:

```go
package pipeline

import "sync"

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

`wg.Add` runs before the goroutines are started, so the close-goroutine's
`wg.Wait` is correct. If `len(cs)` is zero, the close-goroutine closes `out`
immediately.

### Exercise 4: Test The Contract

Create `internal/pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func collectStrings(t *testing.T, ch <-chan string) []string {
	t.Helper()
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
	got := collectStrings(t, out)

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
	got := collectStrings(t, out)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCancellationStopsUpstream(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	src := Generate(done) // no values; closed immediately

	count := 0
	for range Square(done, src) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}

	// Now verify that a pipeline with values exits when done is closed
	// mid-stream.
	done2 := make(chan struct{})
	src2 := Generate(done2, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	out := Square(done2, src2)

	var collected []int
	for v := range out {
		collected = append(collected, v)
		if len(collected) == 3 {
			close(done2)
		}
	}
	if len(collected) != 3 {
		t.Fatalf("collected %d values, want 3", len(collected))
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

// guard against the "forgot to wait for all writers" bug: close-before-done
// would panic on send and the test would crash under -race.
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

Your turn: add `TestFormatPrefix` that builds `Format(done, Square(done,
Generate(done, 7)))` and asserts the only output is `"result: 49"`.

### Exercise 5: Runnable Demo

Create `cmd/pipelinedemo/main.go`. The demo reads exported channels only, so it
cannot touch the goroutines inside the stages — it composes the pipeline and
drains it:

```go
package main

import (
	"fmt"

	"example.com/pipeline/internal/pipeline"
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

## Common Mistakes

### Closing The Outbound Channel Twice

Wrong: `defer close(out)` plus a second `close(out)` after the loop.

What happens: `panic: close of closed channel`.

Fix: rely on `defer close(out)` only. It runs once on every return path
including the cancellation branch.

### Closing A Channel You Do Not Own

Wrong: a downstream consumer calls `close(out)` to "stop" the producer.

What happens: the next send panics with `send on closed channel`.

Fix: use a `done <-chan struct{}` and `defer close(done)`. The producer selects
on `<-done` and returns; the close is owned by the consumer.

### Adding Buffers To Hide A Leak

Wrong: `make(chan int, 10000)` instead of `done`.

What happens: the leak is masked until the buffer fills or the consumer count
changes; the Go blog explicitly calls this fragile.

Fix: pass `done` to every stage and select on `<-done` around every send.

### Forgetting `wg.Add` Before `go output(c)`

Wrong: starting the close-goroutine first, then `wg.Add(len(cs))`.

What happens: a race where `wg.Wait()` returns zero before any `wg.Add` and
`out` is closed while goroutines are still sending.

Fix: `wg.Add(len(cs))` runs before the `go` statements, and the close-goroutine
starts after them, as in `pipeline.Merge` above.

## Verification

From `~/go-exercises/pipeline`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification; the `cmd/pipelinedemo`
binary is a smoke check that the library composes end to end.

## Summary

- A pipeline is a series of stages connected by channels; each stage is a
  goroutine running the same function.
- Each stage closes its outbound channel via `defer close(out)` once it has
  finished sending.
- Cancellation is broadcast by closing a shared `done <-chan struct{}`; every
  send in every stage selects on `<-done`.
- Fan-in needs `sync.WaitGroup` to delay the close until every writer is done.
- Buffered channels do not replace cancellation.

## What's Next

Next: [Fan-Out Pattern](../02-fan-out-pattern/02-fan-out-pattern.md).

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency)
- [Go talks: Concurrency Patterns (2012)](https://go.dev/talks/2012/concurrency.slide)