# Exercise 7: Stop a Multi-Stage Pipeline Cleanly on a Stage Error Without Leaking

A streaming ingestion pipeline chains stages — produce records, transform them,
write them to a sink — connected by channels. When the transform stage hits a bad
record, everything upstream must stop: the producer should not keep pushing into a
channel no one drains, and no goroutine should be left blocked forever on a full or
empty channel. This module implements the Go blog's pipeline-cancellation pattern
with a shared cancel-cause context, and proves both that the producer stops early
and that the whole pipeline returns without leaking.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pipeline/                    independent module: example.com/pipeline
  go.mod                     go 1.26
  pipeline.go                Run (producer -> transform -> sink) with cancel-cause + drain
  cmd/
    demo/
      main.go                runnable demo: transform fails at item 5, producer stops early
  pipeline_test.go           tests: error surfaces, producer bounded, returns before deadline
```

Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
Implement: `Run(ctx, n, transform)` — a three-stage pipeline where each stage watches `ctx.Done()`; the first transform error cancels the shared context so the producer stops sending, and no goroutine blocks forever.
Test: inject an error in transform; the pipeline returns that error; the producer stopped early (bounded produced count); the whole thing returns before a short deadline. No goroutine leaks.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The cancellation contract between stages

Three goroutines, two unbuffered channels: producer sends on `src`, transform reads
`src` and sends on `out`, sink reads `out`. Unbuffered channels make the stages
lockstep — the producer cannot send record `k+1` until transform has taken record
`k` — which is exactly the back-pressure a pipeline wants. The danger is what
happens on an error. When transform hits a bad record and stops reading `src`, the
producer is left blocked mid-send with no reader. Without a cancellation path that
is a permanent goroutine leak.

The fix is a shared `context.WithCancelCause`. Every stage's send and receive is
wrapped in a `select` that also watches `ctx.Done()`. When transform fails, it
calls `cancel(fmt.Errorf(...))` and returns, closing its outgoing channel via
`defer close(out)`. Two things then unwind cleanly: the sink's `range out` ends
because `out` closed, so the sink returns; and the producer's next
`select { case <-ctx.Done(): return; case src <- i: }` picks the `ctx.Done()` case
because the context is now cancelled, so the producer stops and closes `src`. No
goroutine is left blocked. `sync.WaitGroup.Go` runs all three stages, `wg.Wait()`
joins them, and `context.Cause(ctx)` reports the transform error (or nil on a clean
run).

The producer counts every successful send with an `atomic.Int64`. Because the
channels are unbuffered and the stages are lockstep, when transform fails on record
`k` the producer has sent exactly records `0..k` and is blocked trying to send
`k+1`, which it abandons on cancellation. So the produced count is bounded to
roughly the failing index — proof the producer stopped early rather than pushing
all `n` records into the void. That bounded count is what the test asserts, and it
is the observable signature of correct upstream cancellation.

Create `pipeline.go`:

```go
package pipeline

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Run streams records 0..n-1 through a producer -> transform -> sink pipeline.
// The first transform error cancels a shared context so the producer stops
// sending and every stage unwinds without leaking. It returns the number of
// records the producer emitted and the cause of any abort (nil on success).
func Run(ctx context.Context, n int, transform func(ctx context.Context, v int) (int, error)) (produced int, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var emitted atomic.Int64
	src := make(chan int)
	out := make(chan int)
	var wg sync.WaitGroup

	// Producer: emit records, but stop the moment the context is cancelled.
	wg.Go(func() {
		defer close(src)
		for i := range n {
			select {
			case <-ctx.Done():
				return
			case src <- i:
				emitted.Add(1)
			}
		}
	})

	// Transform: process each record; the first error cancels upstream.
	wg.Go(func() {
		defer close(out)
		for v := range src {
			r, terr := transform(ctx, v)
			if terr != nil {
				cancel(fmt.Errorf("transform record %d: %w", v, terr))
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- r:
			}
		}
	})

	// Sink: drain transformed records until the stage closes out.
	wg.Go(func() {
		for range out {
			// A real sink would write to storage here.
		}
	})

	wg.Wait()
	return int(emitted.Load()), context.Cause(ctx)
}
```

### The runnable demo

The demo streams 100 records; the transform fails on record 5. The pipeline returns
that error, and the producer's emitted count is far below 100 — it stopped almost
immediately rather than flooding the pipeline.

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
	badRecord := errors.New("malformed record")

	produced, err := pipeline.Run(context.Background(), 100, func(ctx context.Context, v int) (int, error) {
		if v == 5 {
			return 0, badRecord
		}
		return v * 2, nil
	})

	fmt.Printf("pipeline error: %v\n", err)
	fmt.Printf("is the injected failure: %t\n", errors.Is(err, badRecord))
	fmt.Printf("producer stopped early (emitted far below 100): %t\n", produced < 20)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pipeline error: transform record 5: malformed record
is the injected failure: true
producer stopped early (emitted far below 100): true
```

### Tests

`TestErrorSurfacesAndStopsProducer` injects a transform failure at record 5 and
asserts three things at once: the returned error `Is` the injected sentinel, the
produced count is small (the producer stopped early, not after all 1000 records),
and the produced count is at least 6 (records 0..5 were sent before the failure).
`TestCleanRunProducesAll` pins the happy path: with no failure, all `n` records
flow through and the error is nil. `TestReturnsBeforeDeadline` guards against a
leak — the whole pipeline must return well within a short deadline, which can only
happen if every stage unwound. All under `-race`.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errBadRecord = errors.New("bad record")

func TestErrorSurfacesAndStopsProducer(t *testing.T) {
	t.Parallel()
	produced, err := Run(context.Background(), 1000, func(ctx context.Context, v int) (int, error) {
		if v == 5 {
			return 0, errBadRecord
		}
		return v, nil
	})
	if !errors.Is(err, errBadRecord) {
		t.Fatalf("Run() error = %v, want errBadRecord", err)
	}
	if produced >= 1000 {
		t.Fatalf("produced = %d, want the producer to stop early (< 1000)", produced)
	}
	if produced < 6 {
		t.Fatalf("produced = %d, want at least 6 (records 0..5 sent before the failure)", produced)
	}
}

func TestCleanRunProducesAll(t *testing.T) {
	t.Parallel()
	produced, err := Run(context.Background(), 50, func(ctx context.Context, v int) (int, error) {
		return v + 1, nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if produced != 50 {
		t.Fatalf("produced = %d, want 50", produced)
	}
}

func TestReturnsBeforeDeadline(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	go func() {
		_, _ = Run(context.Background(), 1000, func(ctx context.Context, v int) (int, error) {
			if v == 0 {
				return 0, errBadRecord
			}
			return v, nil
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not return; a stage leaked on the error path")
	}
}
```

## Review

The pipeline is correct when a stage error does three things cleanly: it surfaces
as the returned cause (`errors.Is` against the sentinel), it stops the producer
early (the bounded produced count, at least the failing index and far below `n`),
and it lets the whole pipeline return without leaking (the deadline test). The
mechanism is the shared cancel-cause context threaded through every stage's
`select`: transform's `cancel(...)` plus `defer close(out)` unwinds the sink, and
the producer's `ctx.Done()` branch unwinds the producer. Remove the `ctx.Done()`
case from the producer's `select` and it blocks forever on a send no one reads —
the exact leak this pattern exists to prevent, which the next exercise catches
automatically with goleak. Note the unbuffered channels are deliberate: they make
the stages lockstep so the produced count is a tight, testable signal of early
stopping. Run `go test -race` and `go vet ./...` to confirm.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical stage/cancellation pattern this builds on.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — the shared cancellation carrying the stage error.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — joining the pipeline's stages.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-goroutine-leak-detection.md](08-goroutine-leak-detection.md)
