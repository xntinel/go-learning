# Exercise 7: Composable Pipeline Stage: range In, Transform, Close Out

An ETL pipeline is a chain of stages, each ranging its input, transforming or
filtering, and passing results to the next. The contract that makes the chain shut
down cleanly is: each stage owns and closes its own output when its input drains.
This exercise builds a generic `Stage` and proves that closing the source cascades
a clean termination through every stage with no leaked goroutines.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pipelinestage/              independent module: example.com/pipelinestage
  go.mod                    go 1.26
  stage.go                  Stage[T,U](in, fn) <-chan U with defer close(out)
  cmd/
    demo/
      main.go               chain a map stage and a filter stage
  stage_test.go             chained transform+filter, empty input, no-leak under load
```

Files: `stage.go`, `cmd/demo/main.go`, `stage_test.go`.
Implement: `Stage[T, U any](in <-chan T, fn func(T) (U, bool)) <-chan U` — a goroutine that ranges `in`, applies `fn`, sends kept results to a fresh `out`, and `defer close(out)` when `in` drains.
Test: chaining a map stage then a filter stage produces the transformed-and-filtered result; an empty input closes the output immediately; running the pipeline repeatedly leaves no goroutine leak (a `runtime.NumGoroutine` settle check).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/06-ranging-over-channels/07-pipeline-stage/cmd/demo
cd go-solutions/13-goroutines-and-channels/06-ranging-over-channels/07-pipeline-stage
```

### The close-downstream-when-upstream-closes contract

A stage does exactly three things: range its input, apply a per-value function
that returns a result and a keep flag, and send kept results onward. The
load-bearing detail is the fourth thing it must do — close its output when its
input is exhausted. That is the `defer close(out)` at the top of the goroutine: it
fires when the `for v := range in` ends, which happens precisely when the upstream
closes its channel. So a single `close` at the head of the pipeline cascades: the
first stage's range ends and it closes its output, which ends the second stage's
range, which closes its output, and so on until the final consumer's `range`
terminates. No stage needs to know about cancellation; close *is* the shutdown
signal, propagated structurally.

Two rules keep this correct. First, a stage must read from `in` and write to a
*separate* `out` — never range and send on the same channel, which would feed the
loop its own output and never terminate. The generic signature enforces the split
by type when `T` and `U` differ, and enforces the discipline even when they are
the same. Second, `fn` returns `(U, bool)`: the bool is the filter. Returning
`false` drops the value (a filter stage), returning `true` keeps it (a map stage
keeps everything). One primitive expresses both map and filter, which is why
stages compose into arbitrary pipelines.

The failure mode this guards against is the goroutine leak: if a downstream
consumer stops ranging early (a `break`) without cancelling upstream, every
upstream stage blocks forever on its next send, and all their goroutines leak. The
no-leak test drains fully and repeatedly to prove that, on the happy path, every
stage goroutine exits.

Create `stage.go`:

```go
package pipelinestage

// Stage ranges in, applies fn to each value, and forwards the results for which
// fn returns keep=true onto a new output channel. It closes out when in drains,
// propagating shutdown to the next stage. fn expresses both map (always keep) and
// filter (keep conditionally).
func Stage[T, U any](in <-chan T, fn func(T) (U, bool)) <-chan U {
	out := make(chan U)
	go func() {
		defer close(out) // upstream closed -> our range ends -> we close downstream
		for v := range in {
			if u, keep := fn(v); keep {
				out <- u
			}
		}
	}()
	return out
}

// Source turns a slice into a closed-when-drained channel: the head of a pipeline.
func Source[T any](vals []T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for _, v := range vals {
			out <- v
		}
	}()
	return out
}

// Collect drains a channel into a slice: the tail of a pipeline.
func Collect[T any](in <-chan T) []T {
	var out []T
	for v := range in {
		out = append(out, v)
	}
	return out
}
```

### The runnable demo

The demo builds a three-link pipeline: a source of 1..5, a map stage that
multiplies by ten, and a filter stage that keeps values above 25. Draining the
final channel yields `[30 40 50]`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipelinestage"
)

func main() {
	src := pipelinestage.Source([]int{1, 2, 3, 4, 5})
	tenx := pipelinestage.Stage(src, func(v int) (int, bool) {
		return v * 10, true // map: keep everything
	})
	big := pipelinestage.Stage(tenx, func(v int) (int, bool) {
		return v, v > 25 // filter: keep values above 25
	})

	fmt.Println(pipelinestage.Collect(big))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[30 40 50]
```

### Tests

`TestMapThenFilter` chains the same two stages and asserts the final slice.
`TestEmptyInputClosesImmediately` feeds an empty source and asserts the pipeline
produces nothing and does not block — the empty source closes immediately, which
closes every downstream stage. `TestNoGoroutineLeak` runs a full pipeline many
times and asserts the goroutine count settles back to its baseline, proving each
stage's goroutine exits when its input closes. That test is not `t.Parallel`,
because `runtime.NumGoroutine` is a process-global count that concurrent parallel
tests would perturb.

Create `stage_test.go`:

```go
package pipelinestage

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestMapThenFilter(t *testing.T) {
	src := Source([]int{1, 2, 3, 4, 5})
	tenx := Stage(src, func(v int) (int, bool) { return v * 10, true })
	big := Stage(tenx, func(v int) (int, bool) { return v, v > 25 })

	got := Collect(big)
	want := []int{30, 40, 50}
	if !slices.Equal(got, want) {
		t.Fatalf("pipeline = %v, want %v", got, want)
	}
}

func TestEmptyInputClosesImmediately(t *testing.T) {
	src := Source([]int{})
	out := Stage(src, func(v int) (int, bool) { return v, true })

	got := Collect(out) // must not block
	if len(got) != 0 {
		t.Fatalf("empty pipeline = %v, want empty", got)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	for range 100 {
		src := Source([]int{1, 2, 3, 4, 5})
		mapped := Stage(src, func(v int) (int, bool) { return v * 2, true })
		filtered := Stage(mapped, func(v int) (int, bool) { return v, v%3 == 0 })
		_ = Collect(filtered) // full drain lets every stage goroutine exit
	}

	// Poll until the count settles back; leaked stages would keep it elevated.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline+2 {
		t.Fatalf("goroutines = %d, baseline %d: pipeline leaked", n, baseline)
	}
}

func ExampleStage() {
	src := Source([]int{1, 2, 3})
	doubled := Stage(src, func(v int) (int, bool) { return v * 2, true })
	fmt.Println(Collect(doubled))
	// Output: [2 4 6]
}
```

## Review

The pipeline is correct when a single close at the head terminates every stage and
the final `range` returns cleanly. `TestMapThenFilter` proves the transform-filter
semantics; `TestEmptyInputClosesImmediately` proves the close cascade even with no
data; `TestNoGoroutineLeak` proves the `defer close(out)` contract by showing the
goroutine count returns to baseline after a hundred full drains. The trap to avoid
is ranging and sending on the same channel — a stage must always read `in` and
write a distinct `out`. The `-race` flag confirms the hand-off between stages is
synchronized by the channels themselves, which it is: an unbuffered channel send
happens-before the paired receive.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical stage pattern and close propagation.
- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — ranging a channel until it is closed.
- [pkg.go.dev: runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the goroutine count used for the leak check.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-graceful-shutdown-drain.md](06-graceful-shutdown-drain.md) | Next: [08-result-consumer-error-join.md](08-result-consumer-error-join.md)
