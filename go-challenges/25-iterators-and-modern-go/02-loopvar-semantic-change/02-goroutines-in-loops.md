# Exercise 2: Goroutines in Loops

The goroutine-in-loop bug was the most dangerous form of the old loop-variable behavior, because it was not merely a wrong value but a genuine data race. This exercise builds a fan-out that launches one goroutine per element, runs it under the race detector, and proves that under Go 1.22 per-iteration scope the same code that used to race is now correct.

This module is fully self-contained. It begins with its own `go mod init`, defines every function it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
goroutines.go        FanOut launches one goroutine per element; ErrEmptyInput
cmd/
  demo/
    main.go          fan out, collect, print the sorted results deterministically
goroutines_test.go   race-clean fan-out test + parallel subtests over a per-iteration tc
example_test.go      ExampleFanOut with a verified // Output block
```

- Files: `goroutines.go`, `cmd/demo/main.go`, `goroutines_test.go`, `example_test.go`.
- Implement: `FanOut([]int) ([]int, error)` that launches one goroutine per element, each capturing that iteration's value, and returns the squared values sorted; plus the sentinel `ErrEmptyInput`.
- Test: assert the full sorted result for several inputs, drive parallel subtests over a per-iteration loop variable, and check the empty-input error with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/02-loopvar-semantic-change/02-goroutines-in-loops/cmd/demo && cd go-solutions/25-iterators-and-modern-go/02-loopvar-semantic-change/02-goroutines-in-loops
```

### Why this used to be a race, and why it no longer is

A goroutine launched inside a loop captures the loop variable exactly the way a closure does, but it adds the one ingredient that turns a logic bug into undefined behavior: concurrency. Under the pre-1.22 per-loop rule, `go func() { out <- v * v }()` captured the single shared `v`. The launched goroutines usually did not start running until the loop had already advanced or finished, so they read a later value — but worse, the loop's `range` step was *writing* the next element into `v` on the main goroutine at the same time the launched goroutines were *reading* `v`, with no synchronization between the two. That is the textbook definition of a data race: concurrent access to the same memory, at least one of them a write, unordered by any happens-before edge. `go test -race` reported it as a `WARNING: DATA RACE`, not as a failed assertion, and the program's output was not just wrong but undefined.

Under Go 1.22 each iteration gets its own `v`. The main goroutine copies the element into that iteration's private variable and never touches it again; the launched goroutine reads a variable that nothing else will ever write. The race is gone because the shared mutable cell that caused it no longer exists. The exact same source — `go func() { out <- v * v }()` with no `v := v` copy — is now correct, and the race detector confirms it stays silent. This is the heart of why the change was worth a rare break in backward compatibility: it converted a whole class of programs from undefined behavior to well-defined, correct behavior with no edit.

`FanOut` makes the per-iteration capture observable by squaring. Because the squares of distinct inputs are themselves distinct, recovering the full multiset `{1, 4, 9, 16}` from input `{1, 2, 3, 4}` is direct evidence that each goroutine saw a different value; under the old shared-variable bug the goroutines would have raced on a single `v` and the collected squares would have been wrong, typically several copies of the last element squared. The buffered channel sized to `len(values)` lets every goroutine send without blocking, the `WaitGroup` makes the main goroutine wait for all sends before it closes the channel, and the final sort gives the demo and tests a deterministic order to assert against despite the nondeterministic completion order of the goroutines.

Create `goroutines.go`:

```go
package loopvar

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrEmptyInput is returned when there is nothing to fan out over.
var ErrEmptyInput = errors.New("input must not be empty")

// FanOut launches one goroutine per element, each capturing that iteration's
// value, and returns the squared values sorted. In a module declaring go 1.22
// or later each goroutine reads its own per-iteration variable, so the fan-out
// is race-free under go test -race and every input contributes exactly once.
func FanOut(values []int) ([]int, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("fan out: %w", ErrEmptyInput)
	}

	out := make(chan int, len(values))
	var wg sync.WaitGroup
	for _, v := range values {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out <- v * v
		}()
	}
	wg.Wait()
	close(out)

	squares := make([]int, 0, len(values))
	for s := range out {
		squares = append(squares, s)
	}
	slices.Sort(squares)
	return squares, nil
}
```

### The runnable demo

The demo fans out over five integers and prints the collected squares. The goroutines finish in an unpredictable order, so `FanOut` sorts the result before returning, which is what makes the output deterministic and safe to pin in an Expected-output block.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loop-goroutines"
)

func main() {
	squares, err := loopvar.FanOut([]int{1, 2, 3, 4, 5})
	if err != nil {
		fmt.Println("fan out error:", err)
		return
	}
	fmt.Println(squares)
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```
[1 4 9 16 25]
```

The `-race` flag is the point: under the old semantics this program reported a data race and printed a wrong, nondeterministic slice; under per-iteration scope it runs clean.

### Tests

The tests assert the full sorted result, which proves each goroutine captured a distinct value, and they all run under `-race`, which proves there is no data race. `TestFanOutCases` deliberately drives parallel subtests over the loop variable `tc` with no `tc := tc` line: under per-iteration scope each parallel subtest sees its own `tc`, which is the modern, correct form of a pattern that was itself a classic instance of the old bug.

Create `goroutines_test.go`:

```go
package loopvar

import (
	"errors"
	"reflect"
	"testing"
)

func TestFanOutCapturesEachValue(t *testing.T) {
	t.Parallel()

	got, err := FanOut([]int{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 4, 9, 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FanOut = %v, want %v", got, want)
	}
}

func TestFanOutCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []int
		want []int
	}{
		{name: "single", in: []int{5}, want: []int{25}},
		{name: "ordered", in: []int{1, 2, 3}, want: []int{1, 4, 9}},
		{name: "unordered", in: []int{3, 1, 2}, want: []int{1, 4, 9}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := FanOut(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("FanOut(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFanOutEmpty(t *testing.T) {
	t.Parallel()

	if _, err := FanOut(nil); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("FanOut(nil) error = %v, want ErrEmptyInput", err)
	}
}
```

Create `example_test.go`:

```go
package loopvar

import "fmt"

func ExampleFanOut() {
	squares, _ := FanOut([]int{1, 2, 3, 4})
	fmt.Println(squares)
	// Output: [1 4 9 16]
}
```

## Review

The fan-out is correct when, run under `go test -race`, it reports no data race and returns the full set of distinct squares for every input. The race-freedom is the load-bearing claim: it holds only because each iteration's `v` is a separate variable that nothing writes after the goroutine reads it, which is precisely what Go 1.22 provides. If you compiled the same source in a module declaring `go 1.21`, the detector would flag the concurrent read of the shared `v` against the loop's write. The `WaitGroup` plus a channel buffered to `len(values)` ensures every goroutine can send and the main goroutine waits for all of them before closing the channel; dropping the buffer or the wait would deadlock or lose sends independent of the loop-variable question.

The mistake to avoid is reasoning about the old bug as "it always prints the last value." A data race has no guaranteed outcome, so that mental model is itself wrong; the only correct summary of the old behavior is "undefined." The second mistake is reintroducing a `v := v` or `tc := tc` copy here out of habit — it is unnecessary in a `go 1.26` module, and the parallel-subtest test exists precisely to show the copy-free form is now safe.

## Resources

- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — explains the goroutine-in-loop race specifically and why per-iteration scope removes it.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` instruments memory accesses and what a reported race means.
- [Go FAQ: What happens with closures running as goroutines?](https://go.dev/doc/faq#closures_and_goroutines) — the official note on capturing loop variables in goroutines, updated for the 1.22 behavior.

---

Back to [01-closures-over-loop-variables.md](01-closures-over-loop-variables.md) | Next: [03-pointer-capture-strategies.md](03-pointer-capture-strategies.md)
