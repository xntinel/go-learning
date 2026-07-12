# Exercise 1: A Bounded-Buffer Stream Pipeline (Generate → Square → Filter → Collect)

An ingest service that transforms a stream of records — normalize each one, drop the
invalid ones, hand the survivors to the next stage — is a channel pipeline. Each
stage is a goroutine that reads from an input channel and writes to a
bounded-capacity output channel, closing its output from the sender side when the
input is drained. This is the canonical Go pipeline, reframed as the ETL transform
stage a backend actually runs.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
pipeline/                    module: example.com/pipeline
  go.mod                     go 1.26
  pipeline.go                Generate, Square, Filter, Collect, Run (bounded stage channels)
  cmd/
    demo/
      main.go                runs Run(6, even) and prints the transformed set
  pipeline_test.go           multiset assertions, empty-input, stage-closure contract
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Generate(n) <-chan int`, `Square(<-chan int) <-chan int`, `Filter(<-chan int, pred) <-chan int`, `Collect(<-chan int) []int`, `Run(n, pred) []int`.
- Test: multiset assertion of the squared/filtered output, `Run(0)` returns empty, and the pinned `Square(Generate(5)) == [0 1 4 9 16]` contract.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why each stage owns a bounded output and closes it from the sender

A stage is a function that takes a receive-only input channel and returns a
receive-only output channel, launching a goroutine that reads every input value,
transforms it, and writes to the output. The two rules that make this safe and
leak-free are: the stage that *owns* the output channel is the only one that closes
it, and it closes via `defer close(out)` so the close happens exactly when the
`range in` loop ends — that is, when the upstream stage closed its output and the
input drained. Closing from the sender turns the close into a correct end-of-stream
broadcast: the *next* stage's `range` terminates, propagating the shutdown down the
chain without any explicit coordination.

The return type is `<-chan int` (receive-only) on purpose. It documents in the type
system that the caller may only receive, never send or close — a downstream stage
physically cannot corrupt an upstream channel. This is the direction-typing that the
next lesson covers, applied here as a safety rail.

Each stage's output is *bounded*: `make(chan int, cap)`. Within a single-goroutine-
per-stage chain the buffer is a small throughput smoother — it lets `Square` run a
few values ahead of `Filter` instead of rendezvousing on every single value — but it
is deliberately small, not a giant queue. `Generate` sizes its buffer to `n` so the
producer can emit all `n` values without a consumer yet attached (useful for the
`Collect`-less test that ranges the channel directly); the interior stages use a
small fixed capacity because their job is streaming, not staging the whole dataset.

Ordering: because each stage is a single goroutine reading a single ordered input
and writing in order, the chain preserves order end to end. The tests still assert on
the *multiset* (sort before compare) so they stay robust — the contract is "these
values, squared and filtered", not "this exact slice order", and asserting the
weaker property keeps the test honest if you later widen a stage to a pool.

Create `pipeline.go`:

```go
package pipeline

// Generate emits 0..n-1 on a buffered channel sized to n, so the producer can
// finish without a consumer attached, then closes from the sender side.
func Generate(n int) <-chan int {
	out := make(chan int, n)
	go func() {
		defer close(out)
		for i := range n {
			out <- i
		}
	}()
	return out
}

// Square reads each input value and emits its square on a small bounded output.
func Square(in <-chan int) <-chan int {
	out := make(chan int, 8)
	go func() {
		defer close(out)
		for v := range in {
			out <- v * v
		}
	}()
	return out
}

// Filter forwards only values for which pred returns true.
func Filter(in <-chan int, pred func(int) bool) <-chan int {
	out := make(chan int, 8)
	go func() {
		defer close(out)
		for v := range in {
			if pred(v) {
				out <- v
			}
		}
	}()
	return out
}

// Collect drains the final stage into a slice; it returns once the channel is
// closed and empty.
func Collect(in <-chan int) []int {
	var out []int
	for v := range in {
		out = append(out, v)
	}
	return out
}

// Run wires the whole pipeline: generate n values, square them, keep those that
// satisfy pred, and collect the survivors.
func Run(n int, pred func(int) bool) []int {
	return Collect(Filter(Square(Generate(n)), pred))
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
	even := func(v int) bool { return v%2 == 0 }
	got := pipeline.Run(6, even)
	fmt.Printf("even squares of 0..5: %v\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
even squares of 0..5: [0 4 16]
```

(The squares of 0..5 are 0 1 4 9 16 25; the even ones are 0 4 16, in order.)

### Tests

`TestRunSquaresAndFilters` is the table-driven core: for several `(n, pred)` cases it
sorts the result and compares against the expected multiset. `TestRunWithNoValues`
pins the empty-input path — `Run(0)` yields a nil/empty slice, and every stage still
closes cleanly so nothing leaks. `TestSquareSquaresAllValues` is the preserved
stage-closure contract: it ranges `Square(Generate(5))` directly and pins
`[0 1 4 9 16]`, proving `Square` squares every value and closes so the `range`
terminates. Running under `-race` proves no stage writes after its `close`.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"fmt"
	"slices"
	"testing"
)

func TestRunSquaresAndFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		pred func(int) bool
		want []int
	}{
		{"greater than five", 5, func(v int) bool { return v > 5 }, []int{9, 16}},
		{"even squares", 6, func(v int) bool { return v%2 == 0 }, []int{0, 4, 16}},
		{"keep all", 4, func(int) bool { return true }, []int{0, 1, 4, 9}},
		{"keep none", 4, func(int) bool { return false }, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Run(tc.n, tc.pred)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Run(%d) = %v, want %v", tc.n, got, tc.want)
			}
		})
	}
}

func TestRunWithNoValues(t *testing.T) {
	t.Parallel()

	got := Run(0, func(int) bool { return true })
	if len(got) != 0 {
		t.Fatalf("Run(0) len = %d, want 0 (got %v)", len(got), got)
	}
}

func TestSquareSquaresAllValues(t *testing.T) {
	t.Parallel()

	var got []int
	for v := range Square(Generate(5)) {
		got = append(got, v)
	}
	want := []int{0, 1, 4, 9, 16}
	if !slices.Equal(got, want) {
		t.Fatalf("Square(Generate(5)) = %v, want %v", got, want)
	}
}

func ExampleRun() {
	got := Run(6, func(v int) bool { return v%2 == 0 })
	fmt.Println(got)
	// Output: [0 4 16]
}
```

## Review

The pipeline is correct when every stage closes its output from the sender via
`defer close(out)`, so each downstream `range` terminates and the whole chain shuts
down by propagation rather than coordination. The multiset assertions are deliberate:
the chain is order-preserving today, but the *contract* is the set of transformed
values, and asserting the weaker property keeps the tests honest under a future
widening of a stage into a pool. The two traps to avoid are closing a stage's output
from anywhere but its own goroutine (the next send would panic) and letting a stage
write after close (a `defer close` before the `range` prevents this by construction).
The `-race` run is what proves no stage writes to a channel another goroutine has
already closed.

## Resources

- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical stage/close pattern this exercise builds.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — send, receive, close, and receive-only direction typing.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered vs unbuffered and the close idiom.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-buffered-worker-pool.md](02-buffered-worker-pool.md)
