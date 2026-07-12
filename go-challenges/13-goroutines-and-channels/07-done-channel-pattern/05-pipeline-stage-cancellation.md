# Exercise 5: Propagate Cancellation Through a Multi-Stage Pipeline

A pipeline chains stages: each stage reads from the previous stage's output and writes to its
own. The production requirement is that one closed `done` channel, shared by every stage, unwinds
the *whole* pipeline top to bottom without leaking any stage. This is the streaming-ETL / export
shape: a client requests a large result set, the server streams it through transform stages, and
when the client disconnects mid-stream every stage must stop producing immediately.

## What you'll build

```text
pipelinecancel/                    independent module: example.com/pipelinecancel
  go.mod
  pipeline.go                      generate(done, nums...) -> square(done, in); every send guarded by done
  cmd/
    demo/
      main.go                      runnable demo: generate -> square -> sink, print squares
  pipeline_test.go                 full-run, early-cancel (no-leak), send-respects-done; -race
```

Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
Implement: `generate(done <-chan struct{}, nums ...int) <-chan int` and `square(done <-chan struct{}, in <-chan int) <-chan int`, each stage owning and closing its own output, and every send wrapped in `select` against `done`.
Test: consuming the whole pipeline yields the squares; reading one value and closing `done` lets both stages exit (proven by the final channel closing); an unread sink plus closed `done` still drains.
Verify: `go test -count=1 -race ./...`

### Every stage owns its output and guards its send

Two rules make a pipeline cancellable. First, each stage owns exactly one channel — its output —
and closes it (via `defer close(out)`) when it returns, whether it returned because its input
drained or because `done` fired. A stage never closes its input; that belongs upstream. Second,
every send a stage makes is selected against `done`:

```go
select {
case out <- v:
case <-done:
	return
}
```

This is what lets cancellation propagate. When the sink stops reading, `square` blocks on its send
to `out`; `close(done)` lets that send lose to `<-done`, so `square` returns and closes its output.
`square` returning means it stops receiving from `generate`'s output, so `generate`'s own guarded
send now has no receiver — but `generate` is also selecting on `done`, so it too returns and closes.
The close cascades because every stage is watching the same `done`. Omit the guard on any stage's
send and that stage leaks the moment the downstream consumer pauses, and the leak propagates
upstream as a chain of blocked sends.

`generate` also guards its send even though it is the source: it produces from a slice, and if the
pipeline is cancelled before the slice is exhausted, the guard is what lets it stop early instead of
blocking on a send nobody will receive.

Create `pipeline.go`:

```go
package pipelinecancel

// generate is the source stage. It emits nums one at a time, stopping early if
// done is closed. It owns and closes its output channel.
func generate(done <-chan struct{}, nums ...int) <-chan int {
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

// square is a transform stage. It reads from in, emits n*n, and stops when in is
// drained or done is closed. It owns and closes its output; it never closes in.
func square(done <-chan struct{}, in <-chan int) <-chan int {
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

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipelinecancel"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	// generate -> square -> sink (the range loop below).
	squares := pipelinecancel.Squares(done, 1, 2, 3, 4, 5)

	var out []int
	for v := range squares {
		out = append(out, v)
	}
	fmt.Println("squares:", out)
}
```

The demo calls an exported `Squares` helper that wires the two unexported stages together, because
`cmd/demo` is a separate package and cannot reach unexported functions. Add it to `pipeline.go`:

Append to `pipeline.go`:

```go
// Squares wires generate into square, exposing the assembled pipeline as one
// channel. Closing done unwinds both stages.
func Squares(done <-chan struct{}, nums ...int) <-chan int {
	return square(done, generate(done, nums...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
squares: [1 4 9 16 25]
```

### Tests

`TestPipelineFullRun` consumes the assembled pipeline to completion and checks the squares in order
(a single-consumer pipeline preserves order, unlike fan-in). `TestPipelineEarlyCancel` is the leak
proof: it builds a long pipeline, reads a single value from the final stage, closes `done`, and then
drains the final channel until it closes — which happens only if both `square` and `generate`
returned and ran their `defer close`. `TestStageSendRespectsDone` never reads a value at all: it
builds the pipeline, immediately closes `done`, and drains — proving that a stage whose send has no
receiver still exits because the send is guarded.

Create `pipeline_test.go`:

```go
package pipelinecancel

import "testing"

func TestPipelineFullRun(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)

	squares := Squares(done, 1, 2, 3, 4, 5)

	want := []int{1, 4, 9, 16, 25}
	i := 0
	for v := range squares {
		if i >= len(want) || v != want[i] {
			t.Fatalf("value %d = %d, want %d", i, v, want[i])
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("got %d values, want %d", i, len(want))
	}
}

func TestPipelineEarlyCancel(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})

	nums := make([]int, 1000)
	for i := range nums {
		nums[i] = i
	}
	squares := Squares(done, nums...)

	// Take exactly one value, then abandon the stream and cancel the pipeline.
	<-squares
	close(done)

	// The final channel closes only if both stages returned and ran defer close.
	// A stage with an unguarded send would block and this drain would hang.
	for range squares {
	}
}

func TestStageSendRespectsDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	squares := Squares(done, 1, 2, 3, 4, 5)

	// Never read a value; cancel immediately. Each stage's first send has no
	// receiver, so only the guarded send lets the stages exit.
	close(done)
	for range squares {
	}
}
```

## Review

The pipeline is correct when consuming it fully yields the transformed stream in order and when one
`close(done)` unwinds every stage regardless of where each was blocked. The two cancellation tests
are the point: both terminate only because every stage's send is guarded against `done`, so a
regression to a bare `out <- v` anywhere in the chain turns them into hangs. Order is deterministic
here because a linear single-consumer pipeline forwards values in sequence — contrast the fan-in
merge, where order is not. Each stage closing only its own output, and only on return, is what keeps
the close cascade panic-free. Run `go test -race` to confirm the stages hand off values without a
data race.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-merge-fan-in-cancel.md](04-merge-fan-in-cancel.md) | Next: [06-nonblocking-send-avoid-leak.md](06-nonblocking-send-avoid-leak.md)
