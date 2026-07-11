# Exercise 1: A Directional Producer/Transform/Consumer ETL Pipeline

The foundation module. A three-stage ETL pipeline — produce, transform, consume —
built entirely from directional channels. Each stage's signature states, in the
type system, whether it feeds a channel or drains one, and the compiler enforces
that the producer can never read its output and the consumer can never write to
its input.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
etl/                         independent module: example.com/etl
  go.mod                     go 1.26
  etl.go                     Produce(out chan<- int, ...); Consume(in <-chan int) []int;
                             Transform(in <-chan int, out chan<- int, fn); Pipeline
  cmd/
    demo/
      main.go                runnable demo: produce, transform, consume
  etl_test.go                table-driven tests, -race, empty-input contract
```

Files: `etl.go`, `cmd/demo/main.go`, `etl_test.go`.
Implement: `Produce`, `Consume`, `Transform`, `Pipeline`, wired with `chan<-` and `<-chan`.
Test: produce/consume round-trip, transform applies a function, pipeline chains all three, and the empty-input close contract.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/etl/cmd/demo
cd ~/go-exercises/etl
go mod init example.com/etl
```

### Why direction on every stage

`Produce(out chan<- int, values []int)` takes a *send-only* channel: it spawns a
goroutine that writes each value and then `close`s the channel on completion.
Because `out` is send-only, closing is legal (the producer owns the channel) but
receiving is a compile error — `Produce` physically cannot read back what it
wrote. `Consume(in <-chan int) []int` takes a *receive-only* channel and drains
it with `for v := range in`, which terminates exactly when the producer closes.
`Consume` cannot send on `in` or close it, so it can never corrupt the producer's
stream. `Transform` sits between the two: it drains a `<-chan int`, applies `fn`,
and feeds a `chan<- int` it owns and closes.

The caller in `Pipeline` holds bidirectional `chan int` values and passes them in;
the narrowing to `chan<- int` or `<-chan int` happens implicitly at each call
boundary. The buffered channels (`make(chan int, len(values))`) mean the whole
pipeline can run without any stage blocking on the others in the demo, which
keeps output ordering deterministic.

One design note on the empty-input contract: `Produce` with an empty slice still
launches its goroutine, writes nothing, and closes. `Consume` then ranges over an
already-closable channel, appends nothing, and returns a `nil` slice of length
zero. That "empty in, empty out, channel still closed" path is what
`TestConsumeClosesAfterProduce` pins.

Create `etl.go`:

```go
package etl

// Produce launches a goroutine that writes every value to out and then closes
// it. out is send-only: Produce owns the channel, so closing is legal but
// receiving would be a compile error.
func Produce(out chan<- int, values []int) {
	go func() {
		defer close(out)
		for _, v := range values {
			out <- v
		}
	}()
}

// Consume drains in until it is closed and returns everything it read. in is
// receive-only: Consume can neither send on it nor close it.
func Consume(in <-chan int) []int {
	var out []int
	for v := range in {
		out = append(out, v)
	}
	return out
}

// Transform drains in, applies fn to each value, and feeds out, which it owns
// and closes on completion.
func Transform(in <-chan int, out chan<- int, fn func(int) int) {
	go func() {
		defer close(out)
		for v := range in {
			out <- fn(v)
		}
	}()
}

// Pipeline chains Produce -> Transform -> Consume over buffered channels.
func Pipeline(values []int, fn func(int) int) []int {
	a := make(chan int, len(values))
	b := make(chan int, len(values))
	Produce(a, values)
	Transform(a, b, fn)
	return Consume(b)
}
```

The compile-fail contract is worth stating explicitly. If you uncomment the line
below inside `Produce`, the build fails with `invalid operation: cannot receive
from send-only channel out (variable of type chan<- int)`. That compiler error
*is* the API contract — it is why direction is enforcement, not documentation.

```go
// Inside Produce, this does NOT compile:
//   v := <-out // invalid operation: cannot receive from send-only channel out
```

### The runnable demo

Because the channels are buffered to the input length, values flow through the
three stages in order, so the demo output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/etl"
)

func main() {
	in := []int{10, 20, 30}
	out := etl.Pipeline(in, func(v int) int { return v * 2 })
	fmt.Printf("in:  %v\n", in)
	fmt.Printf("out: %v\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in:  [10 20 30]
out: [20 40 60]
```

### Tests

The tests prove each stage in isolation and the pipeline end to end. Because
`Produce` and `Transform` run in their own goroutines, the buffered channels let
the tests read results deterministically; a `sort` guards against any incidental
reordering. `TestConsumeClosesAfterProduce` pins the empty-input contract from
the walkthrough: produce nothing, and `Consume` still returns cleanly once the
producer closes.

Create `etl_test.go`:

```go
package etl

import (
	"slices"
	"testing"
)

func collectSorted(in <-chan int) []int {
	got := Consume(in)
	slices.Sort(got)
	return got
}

func TestProduceAndConsume(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 5)
	Produce(ch, []int{5, 4, 3, 2, 1})
	got := collectSorted(ch)
	want := []int{1, 2, 3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("Consume = %v, want %v", got, want)
	}
}

func TestTransformAppliesFunction(t *testing.T) {
	t.Parallel()

	a := make(chan int, 3)
	b := make(chan int, 3)
	Produce(a, []int{1, 2, 3})
	Transform(a, b, func(v int) int { return v * 10 })
	got := collectSorted(b)
	want := []int{10, 20, 30}
	if !slices.Equal(got, want) {
		t.Fatalf("Transform result = %v, want %v", got, want)
	}
}

func TestPipelineChainsProduceTransformConsume(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []int
		fn   func(int) int
		want []int
	}{
		{"square", []int{1, 2, 3, 4}, func(v int) int { return v * v }, []int{1, 4, 9, 16}},
		{"negate", []int{1, 2, 3}, func(v int) int { return -v }, []int{-3, -2, -1}},
		{"identity", []int{7}, func(v int) int { return v }, []int{7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Pipeline(tc.in, tc.fn)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Pipeline(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestConsumeClosesAfterProduce(t *testing.T) {
	t.Parallel()

	ch := make(chan int)
	Produce(ch, nil)
	got := Consume(ch)
	if len(got) != 0 {
		t.Fatalf("Consume of empty produce = %v, want empty", got)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after empty Produce")
	}
}
```

## Review

The pipeline is correct when each stage honors its direction: `Produce` only
writes and closes, `Consume` only reads, and `Transform` reads from one side and
writes-then-closes the other. The empty-input test is the subtle one — a
producer that writes nothing must still close, or `Consume` would block forever
on a `range` that never ends. The most common real bug this shape prevents is a
consumer that closes the channel it is draining; because `Consume` receives a
`<-chan int`, that mistake cannot compile. Run `go test -race` to confirm the
handoffs between goroutines are clean.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the three channel types and the send/receive/close rules per direction.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — idiomatic producer/consumer channel use.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the multi-stage directional pipeline this exercise is modeled on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-event-source-generator.md](02-event-source-generator.md)
