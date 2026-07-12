# Exercise 6: Graceful Shutdown: Stop Intake, Then Drain In-Flight Items

When a service receives SIGTERM it should stop accepting new work but finish the
work it already accepted — dropping an accepted job means a lost payment or an
orphaned upload. The pattern is: on shutdown, stop intake (close the channel), and
let the worker keep ranging until the buffer is empty. This exercise builds that
graceful drain and contrasts it with a hard cancel that abandons in-flight items.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
gracefuldrain/              independent module: example.com/gracefuldrain
  go.mod                    go 1.26
  server.go                 type Server (Start/Submit/Wait); DrainHard contrast
  cmd/
    demo/
      main.go               submit, cancel, drain, reject post-shutdown submit
  server_test.go            graceful drains all; hard cancel conserves but may drop
```

Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
Implement: a `Server` whose worker goroutine ranges an intake channel; `Start(ctx)` wires `context.AfterFunc(ctx, stopIntake)` so cancellation closes intake exactly once; `Submit` rejects work once shutting down; `Wait` blocks until the drain finishes. Plus `DrainHard(ctx, in)` that abandons buffered items on cancel.
Test: after graceful shutdown every buffered job is processed and the intake is fully drained; `DrainHard` conserves items (processed + remaining == total) but may leave some unprocessed; `-race` clean; `Wait` returns only after the drain completes.
Verify: `go test -count=1 -race ./...`

### Why graceful drain is a plain range, and hard cancel is not

The graceful path is deliberately a plain `for j := range intake`. It has no
`select` on `ctx.Done()`, because reacting to shutdown by *stopping the loop*
would drop accepted work — the opposite of graceful. Instead, shutdown reaches the
loop indirectly: `context.AfterFunc(ctx, stopIntake)` registers a callback that
fires when `ctx` is cancelled, and that callback closes the intake channel.
Closing intake is what ends the range — but only after every buffered job has been
delivered and processed. So cancellation triggers a *close*, and the close lets
the range finish naturally. That is the essence of graceful shutdown: stop new
work, drain old work.

`context.AfterFunc` returns a `stop` function that cancels the registration; we
`defer stop()` so that if the worker exits for another reason (intake closed by
someone else) the callback is not left dangling. The close itself must happen
exactly once — a second `close` panics — so `stopIntake` guards with a `closed`
flag under a mutex. That same mutex makes `Submit` safe: it checks the flag and
sends under the lock, so a `Submit` can never race the close into a
`send on closed channel` panic. Sending while holding the lock is the correct
idiom here precisely because it serializes send-versus-close.

`DrainHard` is the contrast: a `for-select` that returns the instant `ctx` is
cancelled, leaving whatever is still buffered unprocessed. It is the right choice
when abandoning in-flight work is acceptable (a read-only cache warmer) and the
wrong choice when it is not (anything that mutates state). The exercise ships both
so the difference is a test, not a claim.

Create `server.go`:

```go
package gracefuldrain

import (
	"context"
	"sync"
)

// Job is a unit of accepted work.
type Job struct {
	ID int
}

// Server accepts jobs on a buffered intake channel and processes them in a single
// worker goroutine that ranges the channel. On shutdown it stops accepting new
// jobs, then drains the ones already buffered before the worker returns.
type Server struct {
	intake chan Job

	mu     sync.Mutex // guards closed and serializes send-vs-close
	closed bool

	wg sync.WaitGroup

	pmu       sync.Mutex
	processed []Job
}

// NewServer builds a Server whose intake buffers up to size jobs.
func NewServer(size int) *Server {
	return &Server{intake: make(chan Job, size)}
}

// Start launches the worker and arms graceful shutdown: cancelling ctx closes the
// intake, which ends the worker's range once the buffer drains.
func (s *Server) Start(ctx context.Context) {
	stop := context.AfterFunc(ctx, s.stopIntake)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer stop()
		for j := range s.intake { // graceful: drains buffered jobs before ending
			s.handle(j)
		}
	}()
}

// Submit enqueues a job, or returns false if the server is shutting down.
func (s *Server) Submit(j Job) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.intake <- j // safe under mu: closed cannot happen concurrently
	return true
}

// stopIntake closes the intake exactly once. It is the AfterFunc callback.
func (s *Server) stopIntake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.intake)
	}
}

func (s *Server) handle(j Job) {
	s.pmu.Lock()
	s.processed = append(s.processed, j)
	s.pmu.Unlock()
}

// Wait blocks until the worker has drained the intake and returned.
func (s *Server) Wait() { s.wg.Wait() }

// Processed returns a copy of the jobs handled so far.
func (s *Server) Processed() []Job {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	out := make([]Job, len(s.processed))
	copy(out, s.processed)
	return out
}

// DrainHard processes jobs until ctx is cancelled, then returns immediately,
// abandoning any jobs still buffered in in. This is the non-graceful contrast.
func DrainHard(ctx context.Context, in <-chan Job) []Job {
	var out []Job
	for {
		select {
		case <-ctx.Done():
			return out
		case j, ok := <-in:
			if !ok {
				return out
			}
			out = append(out, j)
		}
	}
}
```

### The runnable demo

The demo submits five jobs, then cancels the context to simulate SIGTERM. The
worker drains all five (graceful), `Wait` returns only after the drain, and a
post-shutdown `Submit` is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/gracefuldrain"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	s := gracefuldrain.NewServer(10)
	s.Start(ctx)

	for i := 1; i <= 5; i++ {
		s.Submit(gracefuldrain.Job{ID: i})
	}

	cancel() // SIGTERM: stop intake, drain in-flight
	s.Wait()

	fmt.Printf("processed %d jobs before exit\n", len(s.Processed()))
	if !s.Submit(gracefuldrain.Job{ID: 99}) {
		fmt.Println("post-shutdown submit rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 5 jobs before exit
post-shutdown submit rejected
```

### Tests

`TestGracefulDrainsAll` submits five jobs, cancels, waits, and asserts all five
were processed and the intake is fully drained — the drain-completeness guarantee.
`TestGracefulWaitBlocksUntilDrained` is the same shape but also asserts that once
`Wait` returns, `len(intake) == 0`, proving `Wait` does not return early.
`TestDrainHardConservesButMayDrop` feeds five buffered jobs, cancels, and asserts
`processed + remaining == 5` — nothing is lost to the void even though some may go
unprocessed, the exact behavior of a hard cancel.

Create `server_test.go`:

```go
package gracefuldrain

import (
	"context"
	"fmt"
	"testing"
)

func TestGracefulDrainsAll(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s := NewServer(5)
	s.Start(ctx)

	for i := 1; i <= 5; i++ {
		if !s.Submit(Job{ID: i}) {
			t.Fatalf("Submit(%d) rejected before shutdown", i)
		}
	}

	cancel()
	s.Wait()

	if got := len(s.Processed()); got != 5 {
		t.Fatalf("processed = %d, want 5 (graceful must not drop accepted work)", got)
	}
	if n := len(s.intake); n != 0 {
		t.Fatalf("intake not drained: %d jobs left", n)
	}
}

func TestGracefulWaitBlocksUntilDrained(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s := NewServer(20)
	s.Start(ctx)
	for i := range 20 {
		s.Submit(Job{ID: i})
	}

	cancel()
	s.Wait() // must not return until the range has fully drained intake

	if n := len(s.intake); n != 0 {
		t.Fatalf("Wait returned with %d jobs still buffered", n)
	}
	if got := len(s.Processed()); got != 20 {
		t.Fatalf("processed = %d, want 20", got)
	}
}

func TestDrainHardConservesButMayDrop(t *testing.T) {
	t.Parallel()
	in := make(chan Job, 5)
	for i := range 5 {
		in <- Job{ID: i}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: DrainHard may abandon buffered jobs

	processed := DrainHard(ctx, in)
	if len(processed)+len(in) != 5 {
		t.Fatalf("processed(%d) + remaining(%d) != 5: items lost", len(processed), len(in))
	}
}

func ExampleServer() {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewServer(4)
	s.Start(ctx)
	s.Submit(Job{ID: 1})
	s.Submit(Job{ID: 2})

	cancel()
	s.Wait()
	fmt.Println(len(s.Processed()))
	// Output: 2
}
```

## Review

The server is correct when a graceful shutdown processes every accepted job and
`Wait` returns only after the intake is empty. `TestGracefulDrainsAll` and the
`len(intake) == 0` assertion together are the drain-completeness proof — the whole
value proposition of graceful shutdown. `DrainHard`'s conservation invariant
(`processed + remaining == total`) shows the honest trade-off: a hard cancel loses
no item to corruption, but it may leave accepted work unprocessed, which is
exactly what graceful drain prevents. The single close is guarded by `stopIntake`
so `context.AfterFunc` firing cannot double-close, and `Submit` sends under the
same lock so it can never race the close into a panic. Run under `-race` to
confirm the send-versus-close serialization holds.

## Resources

- [pkg.go.dev: context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — run a callback when a context is cancelled, and the `stop` function it returns.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — waiting for the worker goroutine to finish draining.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — draining versus abandoning on shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-rate-limited-consumer.md](05-rate-limited-consumer.md) | Next: [07-pipeline-stage.md](07-pipeline-stage.md)
