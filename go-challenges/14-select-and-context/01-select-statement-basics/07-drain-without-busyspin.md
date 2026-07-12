# Exercise 7: Fix the Closed-Channel Busy-Spin in a Pipeline Stage

A transform stage sits between two goroutines in an ETL pipeline: it reads values
from upstream, transforms each, and forwards it downstream, closing its output when
the input is exhausted. Written naively with a bare `select`, it turns into a
100%-CPU busy-spin the moment upstream closes. This module builds the broken
version to see the bug, then the two correct versions — the `range` drain and the
nil-channel-disables-a-case trick for when you genuinely need a `select`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
pipelinestage/                  module example.com/pipelinestage
  go.mod                        go 1.26
  pipelinestage.go              Stage (range drain) + Merge2 (select with nil-disable)
  cmd/
    demo/
      main.go                   run a transform stage over a small input
  pipelinestage_test.go         drain-and-close, terminates-within-budget, nil-disable
```

Files: `pipelinestage.go`, `cmd/demo/main.go`, `pipelinestage_test.go`.
Implement: `Stage(in, transform)` draining with `for range`, and `Merge2(a, b, transform)` using a `select` that nils out each input as it closes.
Test: every upstream value is transformed and forwarded, output closes after input closes, and the loop terminates within a time budget instead of spinning.
Verify: `go test -count=1 -race ./...`

## The bug, then the fix

Here is the tempting, wrong way to write the stage. It is illustrative only — do
not build it:

```go
// BROKEN: busy-spins at 100% CPU after in is closed.
func Stage(in <-chan int, transform func(int) int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for {
			select {
			case v := <-in: // once in is closed, this case is READY every iteration
				out <- transform(v) // v is 0 forever; the loop never blocks, never exits
			}
		}
	}()
	return out
}
```

The trap is the closed-channel semantics from the concepts file: a receive on a
closed channel is always ready and returns the zero value. So after upstream
closes, `case v := <-in` fires on *every* loop iteration, instantly, forwarding a
stream of zeros and pinning a CPU core. It never blocks, so the goroutine never
parks, and it never exits, so `close(out)` never runs. Two bugs from one missing
piece of knowledge.

The idiomatic fix is `for v := range in`. Ranging over a channel pulls values until
the channel is drained and closed, then the loop exits on its own — no `select`, no
comma-ok, no spin. This is the correct shape whenever a stage has exactly one input:

```go
func Stage(in <-chan int, transform func(int) int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for v := range in {
			out <- transform(v)
		}
	}()
	return out
}
```

But sometimes a stage really does need a `select` — for instance when it merges two
inputs that close at different times. You cannot `range` two channels at once. Here
the fix for the spin is twofold: use comma-ok to *detect* the close, and then set
that channel variable to `nil` to *disable* its case. A receive from a `nil`
channel blocks forever, so a `nil` case is simply never chosen again — the `select`
keeps serving the other input without spinning on the closed one. When both inputs
have gone `nil`, the loop condition ends and the stage closes its output.

Create `pipelinestage.go`:

```go
package pipelinestage

// Stage reads every value from in, applies transform, and forwards the result to
// out, closing out once in is drained and closed. It drains with `for range`,
// the idiomatic single-input drain, so it never busy-spins on the closed channel.
func Stage(in <-chan int, transform func(int) int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for v := range in {
			out <- transform(v)
		}
	}()
	return out
}

// Merge2 transforms and forwards values from two inputs into one output. It needs
// a select because it cannot range two channels at once. Each input is set to nil
// once it closes: a receive from a nil channel blocks forever, so that case is
// disabled and never chosen again, which is what keeps the select from spinning on
// a closed channel. When both inputs are nil, the loop ends and out is closed.
func Merge2(a, b <-chan int, transform func(int) int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			select {
			case v, ok := <-a:
				if !ok {
					a = nil // disable this case; do not spin on the closed channel
					continue
				}
				out <- transform(v)
			case v, ok := <-b:
				if !ok {
					b = nil
					continue
				}
				out <- transform(v)
			}
		}
	}()
	return out
}
```

## The runnable demo

The demo runs a doubling stage over five values and prints the transformed stream
in order (a single `Stage` preserves input order).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipelinestage"
)

func main() {
	in := make(chan int, 5)
	for i := 1; i <= 5; i++ {
		in <- i
	}
	close(in)

	out := pipelinestage.Stage(in, func(v int) int { return v * 2 })
	for v := range out {
		fmt.Println(v)
	}
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
2
4
6
8
10
```

## Tests

`TestStageDrainsAndCloses` feeds a slice, closes the input, and asserts every value
is transformed, forwarded in order, and that the output channel closes.
`TestStageTerminatesWithinBudget` is the anti-spin guard: it drains the stage
inside a time budget and fails if the output does not close in time — which is
exactly what the broken bare-`select` version would do (spin forever, never close).
`TestMerge2NilDisablesClosedCase` closes the two inputs at different times and
asserts the merge forwards the union and still terminates, proving the nil-disable
trick keeps the `select` from spinning on the first-closed input.

Create `pipelinestage_test.go`:

```go
package pipelinestage

import (
	"sort"
	"testing"
	"time"
)

// drainWithin collects every value from ch until it closes, failing if that does
// not happen within the budget (the signature of a busy-spin that never closes).
func drainWithin(t *testing.T, ch <-chan int, budget time.Duration) []int {
	t.Helper()
	var got []int
	deadline := time.After(budget)
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return got // channel closed: the stage terminated cleanly
			}
			got = append(got, v)
		case <-deadline:
			t.Fatalf("stage did not close within %v; it is likely busy-spinning", budget)
			return nil
		}
	}
}

func TestStageDrainsAndCloses(t *testing.T) {
	t.Parallel()

	in := make(chan int, 4)
	for _, v := range []int{1, 2, 3, 4} {
		in <- v
	}
	close(in)

	out := Stage(in, func(v int) int { return v + 100 })
	got := drainWithin(t, out, time.Second)

	want := []int{101, 102, 103, 104}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] { // single-input Stage preserves order
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestStageTerminatesWithinBudget(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	close(in) // immediately closed and empty

	out := Stage(in, func(v int) int { return v })
	got := drainWithin(t, out, time.Second)
	if len(got) != 0 {
		t.Fatalf("got %v from an empty input, want none", got)
	}
	// Reaching here means out closed within the budget: no spin.
}

func TestMerge2NilDisablesClosedCase(t *testing.T) {
	t.Parallel()

	a := make(chan int, 2)
	b := make(chan int, 2)
	a <- 1
	a <- 2
	close(a) // a closes first; its case must be disabled, not spun on
	b <- 10
	b <- 20
	close(b)

	out := Merge2(a, b, func(v int) int { return v * 2 })
	got := drainWithin(t, out, time.Second)
	sort.Ints(got) // merge order is nondeterministic

	want := []int{2, 4, 20, 40}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
```

## Review

The stage is correct when it forwards every transformed value, closes its output
once input is exhausted, and — the property the whole module exists to protect —
*terminates* instead of spinning. The `range` drain is the right tool for one
input; the comma-ok-plus-nil pattern is the right tool for a `select` over several
inputs that close independently. The `drainWithin` budget is the machine-checkable
proof of non-spin: swap `Stage` for the broken bare-`select` version and the test
fails by timing out, which is the bug made visible. Never write a `for { select }`
with a single receive case and no close detection — that is the busy-spin, always.

## Resources

- [Go Specification: Receive operator](https://go.dev/ref/spec#Receive_operator) — closed-channel receive returns the zero value and `ok == false`.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — a nil channel operand is never ready, which disables its case.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — stages, draining, and closing outputs.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-hedged-replica-read.md](06-hedged-replica-read.md) | Next: [08-pubsub-broadcast-dispatcher.md](08-pubsub-broadcast-dispatcher.md)
