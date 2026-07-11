# Exercise 1: Stage Primitives — Generate, Transform, Collect With Cause-Carrying Cancellation

Every streaming pipeline is built from three stage shapes: a *source* that emits
work, one or more *mapping* stages that transform (and may reject) each item, and
a *sink* that collects the results. This module builds all three for an ingest of
ledger record IDs: `Generate` emits record IDs, `Transform` validates and doubles
each amount and can reject a bad record by cancelling the whole pipeline via a
cause-carrying cancel, and `Collect` drains the results — including any buffered
values still in flight after `ctx.Done()`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pipeline/                    module example.com/pipeline
  go.mod
  pipeline.go                Generate, Transform (CancelCauseFunc), Collect; var ErrTransform
  cmd/
    demo/
      main.go                happy path, rejection-with-Cause, and a full run
  pipeline_test.go           emit-all, stop-on-cancel, doubles, cause-on-reject, collect paths, Example
```

Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
Implement: `Generate(ctx, start, count) <-chan int`, `Transform(ctx, cancel, in,
reject) <-chan int`, `Collect(ctx, ch) ([]int, error)`, and the sentinel
`ErrTransform`.
Test: emit-all-values, stop-on-cancel without deadlock, transform doubles,
rejection sets `context.Cause` to `ErrTransform`, `Collect` returns all on
success, `Collect` stops on `DeadlineExceeded`, full-pipeline propagates
`ErrTransform` as Cause, and an `Example`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pipeline/cmd/demo
cd ~/go-exercises/pipeline
go mod init example.com/pipeline
```

### Why each stage owns and closes its own output

Each stage function creates its output channel, launches exactly one goroutine
whose first statement is `defer close(out)`, and returns `out` immediately while
the goroutine runs in the background. Close is thereby coupled to the writer's
exit: the `range in` ending, an error `return`, or a `return` on `ctx.Done()` all
run the deferred `close(out)`, and the downstream stage's `range out` ends
cleanly. No other goroutine ever touches `close(out)` — that is what keeps the
"send on closed channel" panic impossible by construction.

`Generate` is the source. It walks `[start, start+count)` and sends each value,
but the send is inside a `select` that also watches `ctx.Done()`, so if the
consumer stops reading (because the context was cancelled), the generator returns
instead of blocking forever on the send.

`Transform` is the mapping stage that can *reject*. It reads each value, and on a
rejected value it calls `cancelCause(fmt.Errorf("%w: %d", ErrTransform, v))` and
returns. That single call cancels the shared context; every other stage sees
`ctx.Done()` and unwinds, and the caller later reads the specific reason with
`context.Cause`. On an accepted value it does the same select-send as `Generate`.

`Collect` is the sink, and it is the subtle one. On the happy path it ranges the
channel until close and returns `ctx.Err()` (nil if never cancelled). But when
`ctx.Done()` fires, there may still be values buffered in the channel that upstream
stages managed to send before they noticed the cancel; a naive `return` would drop
them. So `Collect`'s `ctx.Done()` branch does a non-blocking drain — a `select`
with a `default` — pulling every already-available value before it returns, so the
caller still receives whatever the pipeline produced before it stopped.

Create `pipeline.go`:

```go
package pipeline

import (
	"context"
	"fmt"
)

// ErrTransform is the sentinel a Transform stage cancels the pipeline with when
// it rejects a value. Callers assert it via errors.Is(context.Cause(ctx), ErrTransform).
var ErrTransform = fmt.Errorf("pipeline: transform rejected value")

// Generate emits integers in [start, start+count) on its output channel and
// closes it when the range ends or ctx is cancelled. The send is select-guarded
// so a stopped consumer never leaves the generator blocked on a send.
func Generate(ctx context.Context, start, count int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := start; i < start+count; i++ {
			select {
			case out <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Transform reads integers from in, doubles them, and rejects any value equal to
// reject. On rejection it cancels the shared context with a cause wrapping
// ErrTransform, then returns; downstream stages observe ctx.Done and unwind.
func Transform(
	ctx context.Context,
	cancelCause context.CancelCauseFunc,
	in <-chan int,
	reject int,
) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for v := range in {
			if v == reject {
				cancelCause(fmt.Errorf("%w: %d", ErrTransform, v))
				return
			}
			select {
			case out <- v * 2:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Collect drains ch into a slice and returns it with ctx.Err(). When ctx is
// cancelled it still non-blockingly drains any values already buffered in ch, so
// the caller receives everything the pipeline produced before stopping.
func Collect(ctx context.Context, ch <-chan int) ([]int, error) {
	var out []int
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return out, ctx.Err()
			}
			out = append(out, v)
		case <-ctx.Done():
			for {
				select {
				case v, ok := <-ch:
					if !ok {
						return out, ctx.Err()
					}
					out = append(out, v)
				default:
					return out, ctx.Err()
				}
			}
		}
	}
}
```

### The runnable demo

The demo runs three scenarios: a happy ingest, a rejection that surfaces the cause
as `ErrTransform`, and a clean full run. It prints the collected values and, for
the rejection case, proves `context.Cause` carries the typed reason.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/pipeline"
)

func main() {
	fmt.Println("happy path: ingest 1..5, double")
	ctx, cancel := context.WithCancelCause(context.Background())
	in := pipeline.Generate(ctx, 1, 5)
	out := pipeline.Transform(ctx, cancel, in, -1) // -1 never appears
	got, err := pipeline.Collect(ctx, out)
	cancel(nil)
	fmt.Printf("  values=%v err=%v\n", got, err)

	fmt.Println("rejection: ingest 1..20, reject record 4")
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	in2 := pipeline.Generate(ctx2, 1, 20)
	out2 := pipeline.Transform(ctx2, cancel2, in2, 4)
	got2, _ := pipeline.Collect(ctx2, out2)
	cancel2(nil)
	cause := context.Cause(ctx2)
	fmt.Printf("  values=%v\n", got2)
	fmt.Printf("  cause=%v is_ErrTransform=%v\n", cause, errors.Is(cause, pipeline.ErrTransform))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
happy path: ingest 1..5, double
  values=[2 4 6 8 10] err=<nil>
rejection: ingest 1..20, reject record 4
  values=[2 4 6]
  cause=pipeline: transform rejected value: 4 is_ErrTransform=true
```

### Tests

`TestTransformCancelsPipelineOnRejection` is the center of gravity: after a
rejection, `context.Cause` must return an error that `errors.Is` matches against
`ErrTransform`. `TestGenerateStopsOnCancel` proves the select-guarded send lets
the generator exit on cancel without deadlocking. `TestCollectStopsOnContextTimeout`
proves the sink honors a deadline and still returns the values it collected first.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestGenerateEmitsAllValues(t *testing.T) {
	t.Parallel()

	ch := Generate(context.Background(), 1, 5)
	var got []int
	for v := range ch {
		got = append(got, v)
	}
	want := []int{1, 2, 3, 4, 5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Generate = %v, want %v", got, want)
	}
}

func TestGenerateStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	ch := Generate(ctx, 1, 1000)
	<-ch
	cancel()
	for range ch { // drains and unblocks the generator; no deadlock == pass
	}
}

func TestTransformDoublesValues(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	in := Generate(ctx, 1, 4)
	out := Transform(ctx, cancel, in, -1)
	var got []int
	for v := range out {
		got = append(got, v)
	}
	want := []int{2, 4, 6, 8}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Transform = %v, want %v", got, want)
	}
}

func TestTransformCancelsPipelineOnRejection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	in := Generate(ctx, 1, 100)
	out := Transform(ctx, cancel, in, 5)
	for range out {
	}

	cause := context.Cause(ctx)
	if !errors.Is(cause, ErrTransform) {
		t.Fatalf("Cause = %v, want errors.Is(ErrTransform)", cause)
	}
}

func TestCollectReturnsAllValuesOnSuccess(t *testing.T) {
	t.Parallel()

	in := Generate(context.Background(), 10, 3)
	got, err := Collect(context.Background(), in)
	if err != nil {
		t.Fatalf("Collect err = %v, want nil", err)
	}
	if len(got) != 3 {
		t.Fatalf("Collect returned %d values, want 3: %v", len(got), got)
	}
}

func TestCollectStopsOnContextTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	slow := make(chan int, 1)
	go func() {
		defer close(slow)
		for i := 0; ; i++ {
			select {
			case slow <- i:
				time.Sleep(10 * time.Millisecond)
			case <-ctx.Done():
				return
			}
		}
	}()

	got, err := Collect(ctx, slow)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Collect err = %v, want DeadlineExceeded", err)
	}
	if len(got) == 0 {
		t.Fatal("Collect gathered no values before the deadline")
	}
}

func TestFullPipelinePropagatesCause(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	in := Generate(ctx, 1, 20)
	out := Transform(ctx, cancel, in, 7)
	if _, err := Collect(ctx, out); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect err = %v, want nil or Canceled", err)
	}
	if cause := context.Cause(ctx); !errors.Is(cause, ErrTransform) {
		t.Fatalf("Cause = %v, want ErrTransform", cause)
	}
}

func Example() {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	in := Generate(ctx, 1, 3)
	out := Transform(ctx, cancel, in, -1)
	got, _ := Collect(ctx, out)
	fmt.Println(got)
	// Output: [2 4 6]
}
```

## Review

The three stages are correct when each closes only its own output, every send is
select-guarded against `ctx.Done()`, and the reason for stopping survives as
`context.Cause`. The rejection test is authoritative: after `Transform` rejects a
value, `context.Cause` returns the wrapped `ErrTransform`, while `ctx.Err()` would
only say `context.Canceled` and could not distinguish a rejection from a user
cancel. `Collect`'s cancel branch draining buffered values is what keeps the sink
honest under a deadline — dropping the drain would lose work the pipeline already
did. Run `go test -race`; a leak or a wrong-goroutine close shows up there even
when the functional assertions pass.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical source for the stage/close pattern.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — the cause-carrying cancel used by `Transform`.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — retrieving the typed termination reason.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fan-out-worker-pool-waitgroup-close.md](02-fan-out-worker-pool-waitgroup-close.md)
