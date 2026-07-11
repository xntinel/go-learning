# 9. Trace Tool and Goroutine Scheduling

Execution traces answer questions that CPU profiles cannot: when goroutines were runnable, when they were blocked, when GC work overlapped with application work, and which synchronization points shaped latency. In this lesson you build a small concurrent pipeline as a library, add trace annotations around the real work, and verify the scheduling contract with tests instead of eyeballing terminal output.

```text
tracepipe/
  go.mod
  pipeline.go
  pipeline_test.go
  cmd/demo/main.go
```

## Concepts

### CPU Profiles And Traces Answer Different Questions

A CPU profile samples running goroutines. That is useful for finding hot functions, but it does not show why work was delayed before it reached the CPU. The execution tracer records runtime events such as goroutine creation, blocking, unblocking, syscall transitions, GC activity, and processor scheduling decisions. Use a trace when latency is dominated by waiting, handoff, fan-out, fan-in, or GC scheduling effects rather than by one obviously expensive function.

### Trace Annotations Make The Timeline Readable

The `runtime/trace` package can attach logical tasks and regions to trace events. `trace.NewTask` creates a task carried by a `context.Context`; `trace.WithRegion` records a named interval in the goroutine that runs the function. Names should be low-cardinality labels such as `produce`, `hash`, and `collect`, not per-request IDs. High-cardinality labels make traces harder to navigate and increase overhead.

### Pipeline Shape Determines Scheduling Pressure

An unbuffered pipeline forces a send and receive to rendezvous for each item. That can be correct, but it also creates synchronization blocking when one stage is bursty or slower than the others. Small bounded buffers let producers and workers absorb short bursts without turning every item into a scheduling handoff. Buffers are not free: they use memory and can hide backpressure if they are made too large.

### Tests Check The Contract; The Trace Explains The Timing

The library below does not assert that one trace is always faster than another. Timing varies by machine, load, and `GOMAXPROCS`. Tests verify deterministic contracts: validation, item counts, cancellation, and trace file creation. The trace viewer is then used to interpret scheduling behavior after the code is already known to be correct.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/tracepipe/cmd/demo
cd ~/go-exercises/tracepipe
go mod init tracepipe
```

This is a library package, not a `package main` exercise. The `cmd/demo` program is only a consumer of the exported API.

### Exercise 1: Build The Pipeline Library

Create `pipeline.go`:

```go
package tracepipe

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime/trace"
	"sync"
)

var (
	ErrInvalidItemCount = errors.New("item count must be positive")
	ErrInvalidWorkers   = errors.New("workers must be positive")
	ErrInvalidBuffer    = errors.New("buffer must not be negative")
	ErrNilWriter        = errors.New("trace writer must not be nil")
)

type Config struct {
	Items   int
	Workers int
	Buffer  int
	Rounds  int
}

type Result struct {
	Processed int
	Workers   int
	Buffer    int
	Digest    string
}

func Process(ctx context.Context, cfg Config) (Result, error) {
	if err := validate(cfg); err != nil {
		return Result{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	items := make(chan int, cfg.Buffer)
	out := make(chan [32]byte, cfg.Buffer)

	go func() {
		defer close(items)
		trace.WithRegion(ctx, "produce", func() {
			for i := 0; i < cfg.Items; i++ {
				select {
				case <-ctx.Done():
					return
				case items <- i:
				}
			}
		})
	}()

	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trace.WithRegion(ctx, "hash", func() {
				for item := range items {
					hash := hashItem(item, cfg.Rounds)
					select {
					case <-ctx.Done():
						return
					case out <- hash:
					}
				}
			})
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	result := Result{Workers: cfg.Workers, Buffer: cfg.Buffer}
	trace.WithRegion(ctx, "collect", func() {
		var final [32]byte
		for hash := range out {
			for i := range final {
				final[i] ^= hash[i]
			}
			result.Processed++
		}
		result.Digest = fmt.Sprintf("%x", final[:4])
	})

	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func TraceTo(ctx context.Context, w io.Writer, cfg Config) (Result, error) {
	if w == nil {
		return Result{}, fmt.Errorf("tracepipe: %w", ErrNilWriter)
	}
	if err := validate(cfg); err != nil {
		return Result{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := trace.Start(w); err != nil {
		return Result{}, fmt.Errorf("tracepipe: start trace: %w", err)
	}
	defer trace.Stop()

	ctx, task := trace.NewTask(ctx, "pipeline")
	defer task.End()

	var result Result
	var err error
	trace.WithRegion(ctx, "process", func() {
		result, err = Process(ctx, cfg)
	})
	return result, err
}

func validate(cfg Config) error {
	if cfg.Items <= 0 {
		return fmt.Errorf("tracepipe: %w: got %d", ErrInvalidItemCount, cfg.Items)
	}
	if cfg.Workers <= 0 {
		return fmt.Errorf("tracepipe: %w: got %d", ErrInvalidWorkers, cfg.Workers)
	}
	if cfg.Buffer < 0 {
		return fmt.Errorf("tracepipe: %w: got %d", ErrInvalidBuffer, cfg.Buffer)
	}
	return nil
}

func hashItem(item, rounds int) [32]byte {
	if rounds < 1 {
		rounds = 1
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("item-%d", item)))
	for i := 1; i < rounds; i++ {
		hash = sha256.Sum256(hash[:])
	}
	return hash
}
```

`Process` is intentionally deterministic: it returns a count and a folded digest, not timing claims. `TraceTo` wraps the same work in a runtime trace so `go tool trace` can explain where goroutines waited.

### Exercise 2: Test Validation, Cancellation, And Examples

Create `pipeline_test.go`:

```go
package tracepipe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestProcessCountsAllItems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "unbuffered", cfg: Config{Items: 8, Workers: 2, Buffer: 0, Rounds: 2}},
		{name: "buffered", cfg: Config{Items: 8, Workers: 2, Buffer: 4, Rounds: 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Process(context.Background(), tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got.Processed != tt.cfg.Items {
				t.Fatalf("Processed = %d, want %d", got.Processed, tt.cfg.Items)
			}
			if got.Workers != tt.cfg.Workers || got.Buffer != tt.cfg.Buffer {
				t.Fatalf("Result = %+v", got)
			}
			if got.Digest == "" {
				t.Fatal("Digest should be set")
			}
		})
	}
}

func TestProcessRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{name: "items", cfg: Config{Items: 0, Workers: 1}, want: ErrInvalidItemCount},
		{name: "workers", cfg: Config{Items: 1, Workers: 0}, want: ErrInvalidWorkers},
		{name: "buffer", cfg: Config{Items: 1, Workers: 1, Buffer: -1}, want: ErrInvalidBuffer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Process(context.Background(), tt.cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTraceToRejectsNilWriter(t *testing.T) {
	t.Parallel()

	_, err := TraceTo(context.Background(), nil, Config{Items: 1, Workers: 1})
	if !errors.Is(err, ErrNilWriter) {
		t.Fatalf("err = %v, want ErrNilWriter", err)
	}
}

func TestTraceToWritesTraceData(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	got, err := TraceTo(context.Background(), &buf, Config{Items: 4, Workers: 1, Buffer: 1, Rounds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Processed != 4 {
		t.Fatalf("Processed = %d, want 4", got.Processed)
	}
	if buf.Len() == 0 {
		t.Fatal("trace buffer should not be empty")
	}
}

func TestProcessReturnsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Process(ctx, Config{Items: 100, Workers: 2, Buffer: 1, Rounds: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func ExampleProcess() {
	result, _ := Process(context.Background(), Config{Items: 3, Workers: 1, Buffer: 1, Rounds: 1})
	fmt.Printf("processed=%d workers=%d buffer=%d\n", result.Processed, result.Workers, result.Buffer)
	// Output: processed=3 workers=1 buffer=1
}
```

The tests use `errors.Is` against wrapped sentinel validation errors. The example is also verified by `go test`.

### Exercise 3: Add A Demo That Uses Only Exported API

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"

	"tracepipe"
)

func main() {
	var trace bytes.Buffer
	result, err := tracepipe.TraceTo(context.Background(), &trace, tracepipe.Config{
		Items:   8,
		Workers: 2,
		Buffer:  4,
		Rounds:  2,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("processed=%d workers=%d trace-bytes=%d\n", result.Processed, result.Workers, trace.Len())
}
```

To inspect a real trace outside the tests, write the buffer to a file or use `go test -trace=trace.out`, then open it with `go tool trace trace.out`.

## Common Mistakes

### Treating A Trace As A Correctness Test

Wrong: looking at `go tool trace` first and deciding the code is correct because the timeline looks plausible.

Fix: run the test suite first. In this lesson, `Process` must pass validation, count, cancellation, and example tests before the trace is used for interpretation.

### Recording High-Cardinality Region Names

Wrong: creating region names such as `hash-item-38119`. The trace becomes noisy and the analysis views cannot group events usefully.

Fix: use stable labels such as `produce`, `hash`, `collect`, and attach detailed identifiers only when they are truly needed.

### Assuming Buffers Always Improve Throughput

Wrong: replacing every channel with a very large buffer and declaring the scheduling problem fixed.

Fix: use bounded buffers to reduce unnecessary handoff blocking, then measure. Buffers consume memory, affect latency, and can delay backpressure.

## Verification

Run this from `~/go-exercises/tracepipe`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Then collect an exploratory trace:

```bash
go test -run=TestProcessCountsAllItems -trace=trace.out
go tool trace trace.out
```

Use the trace viewer to compare goroutine blocking when `Buffer` is `0` versus a small positive value. Add one more table row to `TestProcessCountsAllItems` with `Workers: 4` and `Buffer: 8`.

## Summary

- Execution traces show goroutine scheduling, blocking, GC, and runtime events that CPU profiles do not show.
- `trace.NewTask` and `trace.WithRegion` make traces readable when labels are stable and low-cardinality.
- Channel buffering changes scheduling pressure but must be measured, not assumed.
- Tests prove the library contract; traces explain timing and blocking after correctness is established.

## What's Next

Next: [Memory Ballast and GOGC Tuning](../10-memory-ballast-gogc-tuning/10-memory-ballast-gogc-tuning.md).

## Resources

- [runtime/trace package](https://pkg.go.dev/runtime/trace)
- [Diagnostics: Execution tracer](https://go.dev/doc/diagnostics#execution-tracer)
- [cmd/trace package](https://pkg.go.dev/cmd/trace)
- [Go blog: Execution Traces in Go](https://go.dev/blog/execution-traces-2024)
