# Exercise 1: The Basic Tee

A tee duplicates one stream into two: every value sent on the input arrives, unchanged and in order, on both outputs, and closing the input closes both outputs. This exercise builds the simplest form that is correct under the race detector, plus an `OrDone` wrapper so a consumer can stop reading without leaking the goroutine that feeds it.

## What you'll build

```text
tee.go               Tee[T] (duplicate a stream), OrDone[T] (cancellable read)
cmd/
  demo/
    main.go          fan one buffered source to two collectors, print both results
tee_test.go          both outputs get every value, source-close closes both, OrDone stops cleanly
```

- Files: `tee.go`, `cmd/demo/main.go`, `tee_test.go`.
- Implement: `Tee[T any](in <-chan T) (<-chan T, <-chan T)` and `OrDone[T any](done <-chan struct{}, c <-chan T) <-chan T`.
- Test: both outputs receive every value in order, closing the input closes both outputs, `OrDone` forwards until `done` fires, and a 1000-value run is clean under `-race`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/11-tee-channel-pattern/01-basic-tee/cmd/demo && cd go-solutions/16-concurrency-patterns/11-tee-channel-pattern/01-basic-tee
```

### Why the sequential form is correct, and what it costs

The whole tee is one goroutine that ranges over the input and, for each value, sends it to both outputs:

```go
for v := range in {
	out1 <- v
	out2 <- v
}
```

This is correct: every value reaches both outputs exactly once and in order, and when `in` closes the range loop ends and the deferred closes fire. It is worth being honest about the one cost, because the next exercises exist to address it. The two sends are sequential and unguarded, so within a single value the wrapper always sends to `out1` first; a consumer reading `out2` cannot receive value *v* until the `out1` consumer has taken it. And because both sends are blocking, the wrapper advances the input no faster than the slower of the two consumers. That is backpressure, and for two consumers running at similar speeds it is exactly right. The artificial out1-before-out2 ordering and the rate coupling are real limitations, but the cure is not to make this function cleverer — it is to choose a different tee when the situation demands it, which is what exercises 2 through 4 do. The simplest correct form is the right default, and naming its limits honestly is part of understanding it.

`Tee` is generic over the element type. A tee neither inspects nor transforms values, so there is no reason to specialize it to `int`; `[T any]` lets the same wrapper duplicate a stream of requests, events, or bytes with no change.

### OrDone: reading a channel you might want to abandon

A consumer wired to a tee output has a problem the tee cannot solve for it: if the consumer decides to stop early, simply abandoning its `for v := range out` loop leaves the wrapper blocked on its next send to that output, and the wrapper goroutine leaks. `OrDone` is the standard fix on the read side. It forwards values from a source channel until either the source closes or a `done` channel fires, and it selects on `done` even while trying to send the forwarded value, so it can never be wedged on a send to a consumer that has already left:

```go
for {
	select {
	case <-done:
		return
	case v, ok := <-c:
		if !ok {
			return
		}
		select {
		case out <- v:
		case <-done:
			return
		}
	}
}
```

The nested select on the send side is the part people omit. Without it, `OrDone` can read a value from `c`, then block forever trying to hand it to a downstream that stopped listening — the exact leak `OrDone` was meant to prevent, merely moved one channel downstream. With it, `done` unblocks both the read and the send, so the wrapper always exits.

Create `tee.go`:

```go
package tee

// Tee returns two channels that each receive every value sent on in, unchanged
// and in order. When in is closed, both outputs are closed.
//
// The two sends are sequential and blocking: within a single value the wrapper
// sends to out1 before out2, and it advances in no faster than the slower of
// the two consumers. This is the simplest correct form, not an isolating one;
// later exercises trade this simplicity for ordering and rate independence.
func Tee[T any](in <-chan T) (<-chan T, <-chan T) {
	out1 := make(chan T)
	out2 := make(chan T)
	go func() {
		defer close(out1)
		defer close(out2)
		for v := range in {
			out1 <- v
			out2 <- v
		}
	}()
	return out1, out2
}

// OrDone forwards values from c until c closes or done fires, then closes its
// output. It selects on done even while sending, so a consumer can stop reading
// without leaving this wrapper (or the producer feeding c) blocked on a send.
func OrDone[T any](done <-chan struct{}, c <-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case v, ok := <-c:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-done:
					return
				}
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo feeds a buffered, closed source into `Tee` and drains both outputs into slices before printing, so the output is deterministic: both consumers necessarily see the full ordered stream. Draining concurrently and printing after `wg.Wait()` avoids racing the two consumers' prints against each other.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/basic-tee"
)

func main() {
	src := make(chan int, 4)
	for i := 1; i <= 4; i++ {
		src <- i
	}
	close(src)

	a, b := tee.Tee(src)

	var gotA, gotB []int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for v := range a {
			gotA = append(gotA, v)
		}
	}()
	go func() {
		defer wg.Done()
		for v := range b {
			gotB = append(gotB, v)
		}
	}()
	wg.Wait()

	fmt.Printf("A received: %v\n", gotA)
	fmt.Printf("B received: %v\n", gotB)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
A received: [1 2 3 4]
B received: [1 2 3 4]
```

### Tests

The tests pin the three properties that define a basic tee. `TestTeeDeliversEveryValueToBothConsumers` proves both outputs receive every value in order. `TestTeeClosesBothOnSourceClose` proves an empty closed source produces two promptly-closed outputs and no spurious values. `TestTeeWithStrings` exercises the generic parameter with a non-numeric type. `TestOrDoneStopsForwarding` proves `OrDone` stops at `done` without leaking. `TestTeeIsRaceFree` drives a thousand values through two concurrent consumers under the race detector.

Create `tee_test.go`:

```go
package tee

import (
	"sync"
	"testing"
)

func collect[T any](c <-chan T) []T {
	var out []T
	for v := range c {
		out = append(out, v)
	}
	return out
}

func equal[T comparable](a, b []T) bool {
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

func TestTeeDeliversEveryValueToBothConsumers(t *testing.T) {
	t.Parallel()

	src := make(chan int, 6)
	for i := 1; i <= 6; i++ {
		src <- i
	}
	close(src)

	a, b := Tee(src)

	var gotA, gotB []int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); gotA = collect(a) }()
	go func() { defer wg.Done(); gotB = collect(b) }()
	wg.Wait()

	want := []int{1, 2, 3, 4, 5, 6}
	if !equal(gotA, want) {
		t.Fatalf("A got %v, want %v", gotA, want)
	}
	if !equal(gotB, want) {
		t.Fatalf("B got %v, want %v", gotB, want)
	}
}

func TestTeeClosesBothOnSourceClose(t *testing.T) {
	t.Parallel()

	src := make(chan int)
	close(src)

	a, b := Tee(src)
	for range a {
		t.Fatal("A emitted an unexpected value")
	}
	for range b {
		t.Fatal("B emitted an unexpected value")
	}
}

func TestTeeWithStrings(t *testing.T) {
	t.Parallel()

	src := make(chan string, 3)
	src <- "alpha"
	src <- "beta"
	src <- "gamma"
	close(src)

	a, b := Tee(src)

	var gotA, gotB []string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); gotA = collect(a) }()
	go func() { defer wg.Done(); gotB = collect(b) }()
	wg.Wait()

	want := []string{"alpha", "beta", "gamma"}
	if !equal(gotA, want) {
		t.Fatalf("A got %v, want %v", gotA, want)
	}
	if !equal(gotB, want) {
		t.Fatalf("B got %v, want %v", gotB, want)
	}
}

func TestOrDoneStopsForwarding(t *testing.T) {
	t.Parallel()

	c := make(chan int)
	done := make(chan struct{})
	out := OrDone(done, c)

	c <- 1
	if v := <-out; v != 1 {
		t.Fatalf("got %d, want 1", v)
	}

	// Stop reading and signal done; the forwarder must exit and close out.
	close(done)
	for range out {
		// Drain whatever was in flight; the loop must terminate.
	}
}

func TestTeeIsRaceFree(t *testing.T) {
	t.Parallel()

	const n = 1000
	src := make(chan int, n)
	for i := range n {
		src <- i
	}
	close(src)

	a, b := Tee(src)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); collect(a) }()
	go func() { defer wg.Done(); collect(b) }()
	wg.Wait()
}
```

## Review

The basic tee is correct when both outputs receive every value in order and both close when the source closes; the two range loops in the delivery test terminating with identical slices is that proof, and the empty-source test confirms no spurious value escapes and neither output is left open. The most common bug this exercise guards against is closing only one output: drop one of the two `defer close` lines and `TestTeeClosesBothOnSourceClose` hangs because the un-closed output's range loop never ends. Be honest about what this form does not give you — within a value it sends to `out1` first, and it runs at the slower consumer's pace — so that you reach for exercise 2's cancellation or exercises 3 and 4's drop-tolerant tees deliberately rather than by accident. The `OrDone` test pins the read-side teardown: closing `done` must let the forwarder exit even mid-send, which is why the send is itself inside a select. Running the whole file under `-race` with two concurrent consumers and a thousand values establishes that the wrapper has no data race on the shared stream.

## Resources

- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the source of the `OrDone`/done-channel idiom and the model for closing outputs when the input closes.
- [Go Blog: Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns) — fanning a single stream to multiple consumers and the select-based building blocks tees are made of.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the blocking semantics of sends and receives that make a sequential tee apply backpressure.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-per-consumer-cancellation.md](02-per-consumer-cancellation.md)
