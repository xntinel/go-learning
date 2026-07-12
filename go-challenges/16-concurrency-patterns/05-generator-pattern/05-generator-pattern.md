# 5. Generator Pattern: Channel-Returning Functions

A generator is a function that returns a channel and produces values on demand in a background goroutine. Consumers receive values as they become available, without needing to know how they are produced. The pattern encapsulates the goroutine lifecycle inside the function, making the API clean for consumers.

```text
gen/
  go.mod
  internal/integer/integer.go
  internal/integer/integer_test.go
  internal/fib/fib.go
  internal/fib/fib_test.go
  internal/compose/compose.go
  internal/compose/compose_test.go
  cmd/gendemo/main.go
```

The package exposes three generators: `integer.Sequence` (a finite range of consecutive ints), `fib.Sequence` (the Fibonacci sequence up to a max), and `compose.Square` / `compose.Sum` (pipeline stages that consume a generator and produce a transformed one). The `cmd/gendemo` CLI runs all three end-to-end. The captured output is the lesson's documentation.

## Concepts

### The Pattern: Function Returns A Channel, Goroutine Produces

A generator is a function that returns `<-chan T`. Inside the function, a goroutine produces values and sends them on the channel. The function returns the channel immediately; the goroutine runs in the background. When the goroutine is done, it closes the channel; the consumer's `for v := range ch` loop exits.

The lifecycle of the goroutine is encapsulated inside the function. The caller does not need to know how the values are produced; it just consumes from the channel.

### Finite And Infinite Sequences

A finite generator has a known end: `integer.Sequence(n)` produces `0..n-1` and closes. An infinite generator produces forever: `integer.Infinite()` produces `0, 1, 2, ...`. The consumer of an infinite generator must break out of the receive loop when it has enough values, otherwise the goroutine leaks.

### Generators Are Composable

The output of one generator is a `<-chan int`. Another generator can take that channel as input. The lesson's `compose.Square` is a pipeline stage: it consumes from an `int` channel and produces a channel of their squares. `compose.Sum` produces a running sum. The composition is function composition on channels.

### The Pattern Is Idiomatic Go

A generator is a goroutine inside a function. The channel is the return value. The `for v := range ch` is the consumer. The pattern is the idiomatic way to express a producer/consumer pipeline in Go.

## Exercises

### Exercise 1: The Integer Generator

Create `internal/integer/integer.go`:

```go
package integer

// Sequence returns a channel that produces 0, 1, 2, ..., n-1 and
// then is closed. If n <= 0, the channel is closed immediately.
func Sequence(n int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := 0; i < n; i++ {
			out <- i
		}
	}()
	return out
}

// Infinite returns a channel that produces 0, 1, 2, ... forever.
// The caller must consume a finite prefix; otherwise the goroutine
// leaks.
func Infinite() <-chan int {
	out := make(chan int)
	go func() {
		for i := 0; ; i++ {
			out <- i
		}
	}()
	return out
}
```

`Sequence` and `Infinite` differ only in the loop bound. `Sequence` is bounded; `Infinite` is unbounded and the consumer is responsible for stopping the receive loop.

### Exercise 2: Test The Integer Generator

Create `internal/integer/integer_test.go`:

```go
package integer

import "testing"

func TestSequenceProducesAllValues(t *testing.T) {
	t.Parallel()
	got := make([]int, 0)
	for v := range Sequence(5) {
		got = append(got, v)
	}
	want := []int{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], w)
		}
	}
}

func TestSequenceEmpty(t *testing.T) {
	t.Parallel()
	got := make([]int, 0)
	for v := range Sequence(0) {
		got = append(got, v)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestInfiniteProducesAtLeastN(t *testing.T) {
	t.Parallel()
	count := 0
	for v := range Infinite() {
		if v >= 100 {
			break
		}
		count++
	}
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
}
```

`TestSequenceProducesAllValues` is the lesson's main test: it proves the generator produces the expected values in order. `TestSequenceEmpty` proves the boundary (n=0). `TestInfiniteProducesAtLeastN` proves the infinite generator can be consumed for at least 100 values.

### Exercise 3: The Fibonacci Generator

Create `internal/fib/fib.go`:

```go
package fib

// Sequence returns a channel that produces 0, 1, 1, 2, 3, 5, 8, ...
// up to the first value >= max. The first value sent is 0 (the seed).
func Sequence(max int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		a, b := 0, 1
		for {
			if a >= max {
				return
			}
			out <- a
			a, b = b, a+b
		}
	}()
	return out
}
```

`Sequence(max)` is the lesson's bounded variant: it produces values up to (but not including) the first value >= max.

### Exercise 4: Test The Fibonacci Generator

Create `internal/fib/fib_test.go`:

```go
package fib

import "testing"

func TestSequenceProducesCorrectPrefix(t *testing.T) {
	t.Parallel()
	got := make([]int, 0)
	for v := range Sequence(50) {
		got = append(got, v)
	}
	want := []int{0, 1, 1, 2, 3, 5, 8, 13, 21, 34}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], w)
		}
	}
}

func TestSequenceStopsAtMax(t *testing.T) {
	t.Parallel()
	count := 0
	for range Sequence(100) {
		count++
	}
	// Fibonacci up to 100: 0,1,1,2,3,5,8,13,21,34,55,89 -> 12 values
	if count != 12 {
		t.Fatalf("count = %d, want 12", count)
	}
}

func TestSequenceMaxZero(t *testing.T) {
	t.Parallel()
	count := 0
	for range Sequence(0) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}
```

`TestSequenceProducesCorrectPrefix` is the lesson's main Fibonacci test: it pins the first 10 values. `TestSequenceStopsAtMax` proves the count matches the bound (12 values below 100). `TestSequenceMaxZero` proves the edge case (no values when max is 0).

### Exercise 5: The Composable Generators

Create `internal/compose/compose.go`:

```go
package compose

// Square returns a channel that consumes from in and produces the
// square of each value. It closes when in is closed.
func Square(in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for v := range in {
			out <- v * v
		}
	}()
	return out
}

// Sum returns a channel that produces the running sum of the values
// from in. It closes when in is closed.
func Sum(in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		var s int
		for v := range in {
			s += v
			out <- s
		}
	}()
	return out
}
```

`Square` and `Sum` are pipeline stages. They consume from an input channel and produce a transformed output channel. The output channel closes when the input channel closes.

### Exercise 6: Test The Composable Generators

Create `internal/compose/compose_test.go`:

```go
package compose

import (
	"reflect"
	"testing"

	"example.com/gen/internal/integer"
)

func TestSquareSquaresAll(t *testing.T) {
	t.Parallel()
	got := make([]int, 0)
	for v := range Square(integer.Sequence(5)) {
		got = append(got, v)
	}
	want := []int{0, 1, 4, 9, 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSumProducesRunningTotals(t *testing.T) {
	t.Parallel()
	got := make([]int, 0)
	for v := range Sum(integer.Sequence(5)) {
		got = append(got, v)
	}
	want := []int{0, 1, 3, 6, 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSquareThenSum(t *testing.T) {
	t.Parallel()
	// squares 0..4 = 0,1,4,9,16; running sum = 0,1,5,14,30
	got := make([]int, 0)
	for v := range Sum(Square(integer.Sequence(5))) {
		got = append(got, v)
	}
	want := []int{0, 1, 5, 14, 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
```

`TestSquareSquaresAll` is the lesson's composition test: it proves `Square(integer.Sequence(5))` produces `[0, 1, 4, 9, 16]`. `TestSquareThenSum` proves the full pipeline: `Sum(Square(integer.Sequence(5)))` produces the running sum of squares.

### Exercise 7: Run It End To End

Create `cmd/gendemo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/gen/internal/compose"
	"example.com/gen/internal/fib"
	"example.com/gen/internal/integer"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	mode := "all"
	if len(args) > 1 {
		mode = args[1]
	}
	switch mode {
	case "all":
		return runAll()
	case "integer":
		return runInteger()
	case "fib":
		return runFib()
	case "compose":
		return runCompose()
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}

func runInteger() error {
	fmt.Println("=== integer.Sequence(5) ===")
	for v := range integer.Sequence(5) {
		fmt.Printf("  %d\n", v)
	}
	return nil
}

func runFib() error {
	fmt.Println("=== fib.Sequence(50) ===")
	for v := range fib.Sequence(50) {
		fmt.Printf("  %d\n", v)
	}
	return nil
}

func runCompose() error {
	fmt.Println("=== Sum(Square(integer.Sequence(5))) ===")
	for v := range compose.Sum(compose.Square(integer.Sequence(5))) {
		fmt.Printf("  %d\n", v)
	}
	return nil
}

func runAll() error {
	for _, m := range []string{"integer", "fib", "compose"} {
		if err := run([]string{"gendemo", m}); err != nil {
			return err
		}
	}
	return nil
}
```

Run it from `~/go-exercises/gen`:

```bash
go run ./cmd/gendemo integer
```

Expected output (captured by the author on Go 1.26):

```text
=== integer.Sequence(5) ===
  0
  1
  2
  3
  4
```

```bash
go run ./cmd/gendemo fib
```

Expected output:

```text
=== fib.Sequence(50) ===
  0
  1
  1
  2
  3
  5
  8
  13
  21
  34
```

```bash
go run ./cmd/gendemo compose
```

Expected output:

```text
=== Sum(Square(integer.Sequence(5))) ===
  0
  1
  5
  14
  30
```

The composed pipeline is the lesson's most interesting demo: integers 0..4 → squares 0,1,4,9,16 → running sum 0,1,5,14,30.

## Common Mistakes

### Returning A Channel That Is Never Closed

Wrong: a generator that sends values but never closes the channel. The consumer's `for v := range ch` loop never exits; the goroutine leaks.

Fix: `defer close(out)` in the goroutine. The channel closes when the goroutine returns, and the consumer's loop exits. The lesson's pattern uses `defer close(out)` everywhere.

### Forgetting That An Infinite Generator Needs A Stop

Wrong: `for v := range integer.Infinite() { fmt.Println(v) }`. The loop runs forever; the goroutine leaks.

Fix: break out of the loop when the consumer has enough values. `TestInfiniteProducesAtLeastN` does this: `if v >= 100 { break }`. Production code uses a context or a fixed count.

### Mixing Generator And Iterator Semantics

Wrong: a generator that buffers all values internally before returning. The consumer cannot see values as they are produced.

Fix: the generator produces values on the channel as they are available. The consumer receives them as they arrive. Buffering defeats the lazy-evaluation purpose of the pattern.

### Returning The Channel Before The Goroutine Starts

Wrong: a generator that returns the channel and then starts the goroutine. The consumer might call `for v := range ch` and find no values yet; the goroutine eventually starts and the consumer sees them. The race is benign, but the order of operations is fragile.

Fix: the lesson's pattern starts the goroutine inside the function and returns the channel. The consumer's `for v := range ch` blocks until the first value is sent. There is no race.

## Verification

Run this from `~/go-exercises/gen`:

```bash
test -z "$(gofmt -l .)"
go test -count=1 -race ./...
go vet ./...
go build ./...
go run ./cmd/gendemo integer
go run ./cmd/gendemo fib
go run ./cmd/gendemo compose
```

`go build ./...` proves the `cmd/gendemo` binary compiles. The three `go run` steps produce the captured output above. The test suite pins the contract: finite sequence, infinite sequence, Fibonacci, square, sum, and the composed pipeline.

The optional "swap the channel for `iter.Seq`" exercise (not in the tests) is left to the reader: Go 1.23+ has `iter.Seq[V any]` in the `iter` package, and a generator can return a `Seq` instead of a channel. The composed pipeline then uses `for v := range` directly on the seq.

## Summary

- A generator is a function that returns a channel and produces values on demand in a goroutine.
- A finite generator closes the channel when done; an infinite generator needs the consumer to break out.
- Generators are composable: the output of one is the input of another.
- The pattern is idiomatic Go: `for v := range ch` is the consumer's contract.
- The lesson's three packages (`integer`, `fib`, `compose`) are the building blocks of any producer/consumer pipeline.

## What's Next

Next: [errgroup Basic Usage](../06-errgroup-basic-usage/06-errgroup-basic-usage.md).

## Resources

- [Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Go Concurrency Patterns: Context](https://go.dev/blog/context)
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency)
