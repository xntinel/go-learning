# Exercise 4: Adapter — a Function Type That Satisfies a Domain Interface

`http.HandlerFunc` is the canonical trick: a function type with a method that lets a
bare function satisfy an interface. This module applies that trick to a domain port —
a `JobProcessor` interface — so any plain function becomes a processor with no struct,
and shows how to decorate one processor with another while keeping the interface.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
worker/                     independent module: example.com/worker
  go.mod                    go 1.26
  worker.go                 Job, JobProcessor, ProcessorFunc, Worker, decorators
  cmd/
    demo/
      main.go               runnable demo: bare func as processor, with metrics
  worker_test.go            adapter, decorator, error propagation, compile-time tests
```

Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
Implement: `interface JobProcessor { Process(ctx, Job) error }`, `type ProcessorFunc func(context.Context, Job) error` with a `Process` method, a `Worker` that takes a `JobProcessor`, and a metrics/retry decorator.
Test: a `ProcessorFunc` used where a `JobProcessor` is expected; a counting decorator composes; an error propagates unchanged; `var _ JobProcessor = ProcessorFunc(nil)` compiles.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/worker/cmd/demo
cd ~/go-exercises/worker
go mod init example.com/worker
```

### The adapter, and why it removes struct boilerplate

The domain has a port: anything that processes a job. As an interface it is

```go
type JobProcessor interface {
	Process(ctx context.Context, j Job) error
}
```

Without the adapter, every processor — even a one-line one — needs a named struct
with a `Process` method. The adapter eliminates that:

```go
type ProcessorFunc func(ctx context.Context, j Job) error

func (f ProcessorFunc) Process(ctx context.Context, j Job) error {
	return f(ctx, j)
}
```

`ProcessorFunc` is a function type; its `Process` method calls the receiver. So a bare
function `func(ctx, j) error` becomes a `JobProcessor` via `ProcessorFunc(fn)` — no
struct. The compile-time assertion `var _ JobProcessor = ProcessorFunc(nil)` documents
and enforces that the adapter satisfies the interface; if someone changes the interface
signature, this line fails to compile immediately.

The second half is decoration. Because both the interface and the adapter share one
signature, you can wrap a `JobProcessor` in another `JobProcessor` that adds behavior —
metrics, retry, tracing — and callers never know. `WithMetrics(next, counter)` returns
a `ProcessorFunc` that increments the counter and delegates to `next`. The `Worker`
depends only on `JobProcessor`, so it accepts a bare `ProcessorFunc`, a struct, or a
decorated stack identically. An error returned by the underlying function passes
through the adapter unchanged — the `Process` method returns exactly what the receiver
returns, so `errors.Is` still matches the original sentinel at the top of the stack.

Create `worker.go`:

```go
package worker

import (
	"context"
	"sync/atomic"
)

// Job is a unit of work identified by ID with an opaque payload.
type Job struct {
	ID      string
	Payload string
}

// JobProcessor is the domain port: anything that can process a job.
type JobProcessor interface {
	Process(ctx context.Context, j Job) error
}

// ProcessorFunc adapts a bare function to JobProcessor, the http.HandlerFunc trick.
type ProcessorFunc func(ctx context.Context, j Job) error

// Process satisfies JobProcessor by calling the receiver.
func (f ProcessorFunc) Process(ctx context.Context, j Job) error {
	return f(ctx, j)
}

// Compile-time proof that the adapter satisfies the interface.
var _ JobProcessor = ProcessorFunc(nil)

// Worker runs jobs through any JobProcessor.
type Worker struct {
	proc JobProcessor
}

func NewWorker(proc JobProcessor) *Worker {
	return &Worker{proc: proc}
}

// Run processes each job in order, stopping at the first error.
func (w *Worker) Run(ctx context.Context, jobs []Job) error {
	for _, j := range jobs {
		if err := w.proc.Process(ctx, j); err != nil {
			return err
		}
	}
	return nil
}

// WithMetrics decorates a processor, counting every invocation. The returned
// value is itself a JobProcessor, so the Worker cannot tell it apart.
func WithMetrics(next JobProcessor, calls *atomic.Int64) JobProcessor {
	return ProcessorFunc(func(ctx context.Context, j Job) error {
		calls.Add(1)
		return next.Process(ctx, j)
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/worker"
)

func main() {
	// A bare function becomes a JobProcessor via the adapter.
	base := worker.ProcessorFunc(func(ctx context.Context, j worker.Job) error {
		fmt.Printf("processing %s: %s\n", j.ID, j.Payload)
		return nil
	})

	var calls atomic.Int64
	decorated := worker.WithMetrics(base, &calls)

	w := worker.NewWorker(decorated)
	jobs := []worker.Job{
		{ID: "j1", Payload: "resize-image"},
		{ID: "j2", Payload: "send-email"},
	}
	if err := w.Run(context.Background(), jobs); err != nil {
		panic(err)
	}
	fmt.Printf("processed %d jobs\n", calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processing j1: resize-image
processing j2: send-email
processed 2 jobs
```

### Tests

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

var errBadPayload = errors.New("bad payload")

func TestAdapterUsedAsInterface(t *testing.T) {
	t.Parallel()
	var gotJob Job
	var gotCtxKey any
	type ctxKey struct{}
	proc := ProcessorFunc(func(ctx context.Context, j Job) error {
		gotJob = j
		gotCtxKey = ctx.Value(ctxKey{})
		return nil
	})

	ctx := context.WithValue(context.Background(), ctxKey{}, "trace-1")
	w := NewWorker(proc) // a bare func passed where JobProcessor is expected
	if err := w.Run(ctx, []Job{{ID: "j1", Payload: "p"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotJob.ID != "j1" {
		t.Errorf("job ID = %q, want j1", gotJob.ID)
	}
	if gotCtxKey != "trace-1" {
		t.Errorf("ctx value = %v, want trace-1", gotCtxKey)
	}
}

func TestDecoratorComposes(t *testing.T) {
	t.Parallel()
	var underlying int
	base := ProcessorFunc(func(ctx context.Context, j Job) error {
		underlying++
		return nil
	})
	var calls atomic.Int64
	w := NewWorker(WithMetrics(base, &calls))

	jobs := []Job{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if err := w.Run(context.Background(), jobs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("metrics counted %d, want 3", calls.Load())
	}
	if underlying != 3 {
		t.Errorf("underlying ran %d times, want 3", underlying)
	}
}

func TestErrorPropagatesUnchanged(t *testing.T) {
	t.Parallel()
	base := ProcessorFunc(func(ctx context.Context, j Job) error {
		return errBadPayload
	})
	var calls atomic.Int64
	w := NewWorker(WithMetrics(base, &calls)) // through the decorator too

	err := w.Run(context.Background(), []Job{{ID: "j1"}})
	if !errors.Is(err, errBadPayload) {
		t.Fatalf("err = %v, want errBadPayload through the adapter", err)
	}
}

// TestInterfaceSatisfaction is a compile-time check surfaced as a runtime no-op.
func TestInterfaceSatisfaction(t *testing.T) {
	t.Parallel()
	var _ JobProcessor = ProcessorFunc(nil)
	var _ JobProcessor = NewWorkerProc()
}

// NewWorkerProc returns a struct-based processor to contrast with the adapter.
func NewWorkerProc() JobProcessor { return structProc{} }

type structProc struct{}

func (structProc) Process(ctx context.Context, j Job) error { return nil }

func ExampleProcessorFunc() {
	p := ProcessorFunc(func(ctx context.Context, j Job) error {
		fmt.Println("processed", j.ID)
		return nil
	})
	_ = p.Process(context.Background(), Job{ID: "x"})
	// Output: processed x
}
```

## Review

The adapter is correct when a bare function used as a `JobProcessor` receives exactly
the job and context the worker was given — `TestAdapterUsedAsInterface` threads a
context value through and reads it back inside the function, proving the adapter does
not swallow or replace the context. Decoration must be transparent: `WithMetrics`
returns a `JobProcessor`, the worker depends only on the interface, and both the
metrics counter and the underlying function run the same number of times. Error
transparency is the subtle property — the `Process` method returns whatever the
receiver returns, so a sentinel wrapped nowhere still matches `errors.Is` after passing
through the adapter and the decorator. The `var _ JobProcessor = ProcessorFunc(nil)`
line is not decoration; it is a compile-time contract that fails the build the instant
the interface and adapter signatures diverge.

## Resources

- [net/http.HandlerFunc (the original adapter)](https://pkg.go.dev/net/http#HandlerFunc)
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets)
- [Effective Go: interfaces and methods](https://go.dev/doc/effective_go#interface_methods)
- [errors.Is](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-http-middleware-chain.md](03-http-middleware-chain.md) | Next: [05-retry-with-operation-callback.md](05-retry-with-operation-callback.md)
