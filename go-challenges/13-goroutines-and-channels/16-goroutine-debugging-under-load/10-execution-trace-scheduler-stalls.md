# Exercise 10: Capture an execution trace to expose scheduler latency around a handler

When latency is lost to *scheduling* — a runnable goroutine waiting for a processor,
a GC pause on the critical path, a blocking syscall — profiles are the wrong tool.
The execution tracer records the scheduler timeline itself. This exercise wraps a
handler in a guarded trace capture, annotates the critical section with a Task and a
Region, and verifies the captured trace is a real, parseable trace file.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
tracing/                  independent module: example.com/tracing
  go.mod
  tracing.go              Capture (paired Start/Stop, guarded), Handle (Task + Region + Log)
  cmd/demo/main.go        capture a trace around a handler, show it has the trace header
  tracing_test.go         trace has magic header; a second concurrent Start fails
```

- Files: `tracing.go`, `cmd/demo/main.go`, `tracing_test.go`.
- Implement: `Capture(w, fn)` pairing `trace.Start(w)`/`trace.Stop()` with `defer` and returning an error joined with `ErrTracing` if a trace is already running; `Handle(ctx, name, work)` wrapping `work` in a `trace.NewTask` and a `trace.WithRegion`, logging phase markers.
- Test: a captured trace is non-empty and begins with the `go 1.` magic header; starting a second trace while one is running returns `ErrTracing`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/10-execution-trace-scheduler-stalls/cmd/demo
cd go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/10-execution-trace-scheduler-stalls
```

### When a trace beats a profile, and the Start/Stop contract

A profile is a statistical summary: "5% of samples were in this function." A trace is
the event log: every goroutine start, stop, block, and unblock; every GC start, stop,
and pause; every syscall entry and exit; per-processor utilization — all timestamped.
You reach for it when the question is *when* and *why was this goroutine not running*,
not *where did CPU go*. In `go tool trace` you can see a goroutine sit runnable while
all Ps are busy, or a GC pause land squarely on a request's critical path — the kind
of latency a CPU profile cannot explain because no CPU was being spent on your code.

Tracing is a global, singleton facility, and that shapes the API. `trace.Start(w)`
begins writing the binary trace to `w` and returns an error if a trace is *already*
running — there is exactly one tracer. `trace.Stop()` ends it. The two must be paired;
forget `Stop` and the trace file is truncated and unparseable, so `Capture` pairs them
with `defer` and returns `Start`'s error joined with a sentinel so callers can match
it with `errors.Is`. The captured bytes are a real trace file: it begins with the
header `go 1.<version> trace` followed by NUL bytes, which is how the test recognizes a
valid trace without depending on the exact runtime version.

`Handle` adds the user-level annotations that make a trace navigable.
`trace.NewTask(ctx, name)` creates a logical task spanning an operation (a request),
returning a derived context and a `*Task` you `End` with `defer`. `trace.WithRegion`
marks a synchronous span within the task — the critical section — and `trace.Log`
records point-in-time events. In `go tool trace` these appear as named spans over the
scheduler timeline, so you can line up "my critical section" against "the GC pause that
delayed it."

Create `tracing.go`:

```go
package tracing

import (
	"context"
	"errors"
	"io"
	"runtime/trace"
)

// ErrTracing indicates a trace could not be started, most often because one is
// already running (the tracer is a singleton).
var ErrTracing = errors.New("tracing: could not start trace")

// Capture runs fn while an execution trace is written to w. It pairs
// trace.Start/Stop with defer and returns ErrTracing (joined with the cause) if a
// trace is already running.
func Capture(w io.Writer, fn func()) error {
	if err := trace.Start(w); err != nil {
		return errors.Join(ErrTracing, err)
	}
	defer trace.Stop()
	fn()
	return nil
}

// Handle annotates work with a trace Task and a Region around the critical
// section, plus phase logs, so scheduler stalls inside it are visible in
// `go tool trace`.
func Handle(ctx context.Context, name string, work func(context.Context)) {
	ctx, task := trace.NewTask(ctx, name)
	defer task.End()
	trace.WithRegion(ctx, "critical-section", func() {
		trace.Log(ctx, "phase", "start")
		work(ctx)
		trace.Log(ctx, "phase", "done")
	})
}
```

### The runnable demo

The demo captures a trace around a handler and prints that the capture succeeded, that
bytes were written, and that the buffer begins with the trace magic header.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"

	"example.com/tracing"
)

func main() {
	var buf bytes.Buffer
	err := tracing.Capture(&buf, func() {
		tracing.Handle(context.Background(), "checkout", func(ctx context.Context) {
			// simulate the critical section
			sum := 0
			for i := range 1000 {
				sum += i
			}
			_ = sum
		})
	})

	fmt.Println("capture err:", err)
	fmt.Println("trace bytes captured:", buf.Len() > 0)
	fmt.Println("has trace header:", bytes.HasPrefix(buf.Bytes(), []byte("go 1.")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
capture err: <nil>
trace bytes captured: true
has trace header: true
```

### Tests

`TestCaptureProducesTrace` runs a handler inside `Capture` and asserts the result is a
non-empty, valid trace (the `go 1.` header). `TestConcurrentCaptureFails` starts a
trace directly, then calls `Capture` while it is running and asserts the error matches
`ErrTracing` — proving the singleton contract and the sentinel wrapping. These tests
touch the global tracer, so they run sequentially.

Create `tracing_test.go`:

```go
package tracing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/trace"
	"testing"
)

func TestCaptureProducesTrace(t *testing.T) {
	var buf bytes.Buffer
	err := Capture(&buf, func() {
		Handle(context.Background(), "req", func(ctx context.Context) {
			trace.Log(ctx, "unit", "work")
		})
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("trace buffer is empty")
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("go 1.")) {
		n := min(8, buf.Len())
		t.Errorf("trace missing magic header; first bytes: %q", buf.Bytes()[:n])
	}
}

func TestConcurrentCaptureFails(t *testing.T) {
	var outer bytes.Buffer
	if err := trace.Start(&outer); err != nil {
		t.Fatalf("outer trace.Start: %v", err)
	}
	defer trace.Stop()

	var inner bytes.Buffer
	err := Capture(&inner, func() {})
	if !errors.Is(err, ErrTracing) {
		t.Fatalf("Capture err = %v; want ErrTracing", err)
	}
}

func ExampleCapture() {
	var buf bytes.Buffer
	_ = Capture(&buf, func() {})
	fmt.Println(bytes.HasPrefix(buf.Bytes(), []byte("go 1.")))
	// Output: true
}
```

## Review

The capture is correct when it produces a real trace and enforces the singleton
contract. `TestCaptureProducesTrace` checks both that bytes were written and that they
form a valid trace — the `go 1.` header is the runtime's own magic, so an unparseable
or empty buffer (the signature of a missing `Stop`) fails the test.
`TestConcurrentCaptureFails` proves `Capture` surfaces the "already running" error
rather than silently swallowing it, and wraps it so `errors.Is(err, ErrTracing)`
matches. The discipline the exercise teaches is the `defer trace.Stop()` immediately
after a successful `Start`: a trace left open is worse than no trace, because the file
looks present but cannot be opened. Note the tests are sequential — two `trace.Start`
calls racing would be exactly the error condition, so they must not run in parallel.
Run `go test -race`.

## Resources

- [`runtime/trace`](https://pkg.go.dev/runtime/trace) — `Start`/`Stop`, `NewTask`, `WithRegion`, `Log`, and the note that `Start` errors if tracing is already enabled.
- [`go tool trace`](https://pkg.go.dev/cmd/trace) — opening a captured trace: the scheduler timeline, GC, and user Tasks/Regions.
- [Go Blog: More powerful Go execution traces](https://go.dev/blog/execution-traces-2024) — what the trace shows and when it beats a profile.

---

Prev: [09-dump-stacks-on-shutdown-timeout.md](09-dump-stacks-on-shutdown-timeout.md) | Back to [00-concepts.md](00-concepts.md) | Next: [11-consumer-drain-stall-state-histogram.md](11-consumer-drain-stall-state-histogram.md)
