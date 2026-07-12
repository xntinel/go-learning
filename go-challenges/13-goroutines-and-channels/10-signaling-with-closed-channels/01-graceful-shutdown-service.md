# Exercise 1: Graceful Shutdown With A Closed Stop Channel

The smallest complete shutdown primitive a backend owns: a worker that consumes
a stream of ints, accumulates a running sum, and stops on either the producer
closing the work channel (drain) or an explicit `Stop()` (forced). `Stop()` does
not merely *ask* the worker to stop — it blocks until the worker has actually
exited, so the caller never proceeds over an in-flight goroutine.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
closesignal/                 independent module: example.com/closesignal
  go.mod                     go mod init example.com/closesignal
  service.go                 type Service; New, Run(work) <-chan int, Stop()
  cmd/
    demo/
      main.go                runnable demo: drain path and forced-stop path
  service_test.go            drain, forced stop, idempotent Stop, wait-for-exit
```

Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
Implement: a `Service` with a `stop chan struct{}` closed by `Stop()` under a mutex (guarded by a `stopped` flag for idempotency) and a `done chan struct{}` the worker closes on exit so `Stop()` can block until the goroutine drains and returns.
Test: full-drain sum from a closed work channel; `Stop()` from a second goroutine yields the partial sum; double `Stop()` does not panic; `Stop()` returns only after the worker delivered its result.
Verify: `go test -count=1 -race ./...`

### The two-channel shutdown contract

The worker loop selects over two channels with two different owners. The caller
owns `stop` and closes it to *request* shutdown. The worker owns `done` and
closes it (via `defer`) to *acknowledge* it has exited. That split is the whole
design: closing `stop` broadcasts "please stop" to the one worker, and blocking
on `<-done` inside `Stop()` turns "requested" into "completed". A `Stop()` that
closed `stop` and returned immediately would be lying — the caller would race the
still-running goroutine.

`Run` returns an `out` channel (buffered size 1) on which the worker delivers its
final sum before exiting. The worker returns on either arm of the select: `stop`
closed (forced), or `work` closed with `ok == false` (drained). In both cases it
sends the accumulated sum and lets the deferred `close(out)` and `close(done)`
run. Because `out` is buffered, the worker never blocks sending its result even
if no one is reading yet, which is what lets `Stop()` wait on `done` without
deadlocking against a reader that has not arrived.

`Stop()` is idempotent by construction: a `stopped` flag under a mutex means the
first call closes `stop` and waits, and every later call is a no-op that returns
at once. Without the flag, a second `close(stop)` would panic — the exact
double-close bug that pages you when a `defer`, a signal handler, and an error
path all try to stop the same service.

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/01-graceful-shutdown-service/cmd/demo
cd go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/01-graceful-shutdown-service
```

Create `service.go`:

```go
package closesignal

import (
	"sync"
)

// Service accumulates ints from a work channel and stops on either the work
// channel closing (drain) or an explicit Stop (forced). The stop signal is a
// closed channel; the worker acknowledges its exit by closing done.
type Service struct {
	mu      sync.Mutex
	stopped bool
	stop    chan struct{}
	done    chan struct{}
}

// New returns a ready Service. The stop and done channels are chan struct{}:
// the close is the entire signal, so there is no value to send or drain.
func New() *Service {
	return &Service{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Run starts the worker and returns a channel that will receive the final sum
// exactly once, then be closed. The worker exits when work is closed (drain) or
// stop is closed (forced).
func (s *Service) Run(work <-chan int) <-chan int {
	out := make(chan int, 1)
	go func() {
		defer close(out)
		defer close(s.done)
		var sum int
		for {
			select {
			case <-s.stop:
				out <- sum
				return
			case v, ok := <-work:
				if !ok {
					out <- sum
					return
				}
				sum += v
			}
		}
	}()
	return out
}

// Stop requests shutdown and blocks until the worker has exited. It is
// idempotent: only the first call closes stop; later calls return immediately.
func (s *Service) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stop)
	s.mu.Unlock()
	<-s.done
}
```

The mutex is released before the `<-s.done` wait on purpose: holding it across
the wait would serialize concurrent `Stop()` callers behind the blocking receive
for no reason. The `stopped` flag already guarantees exactly one close; the
later callers fall through to their own `<-s.done`, which returns immediately
once the worker has closed `done`.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/closesignal"
)

func main() {
	// Drain path: producer closes work, worker finishes what remains.
	s := closesignal.New()
	work := make(chan int, 4)
	for _, v := range []int{10, 20, 30, 40} {
		work <- v
	}
	close(work)
	out := s.Run(work)
	fmt.Printf("drained sum: %d\n", <-out)

	// Forced path: two values land, then Stop broadcasts shutdown.
	s2 := closesignal.New()
	live := make(chan int)
	out2 := s2.Run(live)
	go func() {
		live <- 5
		live <- 7
		s2.Stop()
	}()
	fmt.Printf("partial sum after stop: %d\n", <-out2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained sum: 100
partial sum after stop: 12
```

The forced path is deterministic because `live` is unbuffered: each `live <- v`
returns only after the worker has received it, and the worker is a single
sequential goroutine, so by the time `Stop()` runs both `5` and `7` have already
been added.

### Tests

Create `service_test.go`:

```go
package closesignal

import (
	"fmt"
	"testing"
	"time"
)

func TestRunProcessesWork(t *testing.T) {
	t.Parallel()

	s := New()
	work := make(chan int, 3)
	work <- 1
	work <- 2
	work <- 3
	close(work)

	out := s.Run(work)
	if sum := <-out; sum != 6 {
		t.Fatalf("sum = %d, want 6", sum)
	}
}

func TestStopSignalsShutdown(t *testing.T) {
	t.Parallel()

	s := New()
	work := make(chan int) // unbuffered, nothing sent

	out := s.Run(work)
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.Stop()
	}()

	if sum := <-out; sum != 0 {
		t.Fatalf("sum = %d, want 0", sum)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	s := New()
	work := make(chan int)

	_ = s.Run(work)
	s.Stop()
	s.Stop() // must not panic on the second close
}

func TestRunReturnsAfterWorkClose(t *testing.T) {
	t.Parallel()

	s := New()
	work := make(chan int, 2)
	work <- 10
	work <- 20
	close(work)

	out := s.Run(work)
	if sum := <-out; sum != 30 {
		t.Fatalf("sum = %d, want 30", sum)
	}
}

// TestStopWaitsForGoroutine pins the contract that Stop returns only after the
// worker has delivered its result and exited. Because out is buffered and the
// worker sends before close(done), if Stop honored the <-done wait the value is
// already present and a non-blocking receive succeeds; a Stop that returned
// early would let the default case fire.
func TestStopWaitsForGoroutine(t *testing.T) {
	t.Parallel()

	s := New()
	work := make(chan int)

	out := s.Run(work)
	s.Stop()

	select {
	case _, ok := <-out:
		if !ok {
			t.Fatal("out closed without a delivered sum")
		}
	default:
		t.Fatal("Stop returned before the worker delivered its result")
	}
}

func ExampleService() {
	s := New()
	work := make(chan int, 3)
	work <- 1
	work <- 2
	work <- 3
	close(work)

	out := s.Run(work)
	fmt.Println(<-out)
	// Output: 6
}
```

## Review

The service is correct when the worker has exactly two exits and both deliver the
sum: `stop` closed and `work` closed. `Stop()` is correct when it never panics on
a repeat call and never returns before the worker has exited — the two properties
`TestStopIsIdempotent` and `TestStopWaitsForGoroutine` pin. The common way to get
this wrong is a `Stop()` that closes `stop` and returns; it passes a casual smoke
test and then flakes under load because the caller races the goroutine. The
second trap is closing `stop` without the `stopped` guard, which panics the
moment two shutdown paths overlap. Run `go test -race` to confirm the mutex
actually serializes the close and no send-on-closed slips through.

## Resources

- [The Go Programming Language Specification: Close](https://go.dev/ref/spec#Close) — the exact semantics of `close`, receiving from a closed channel, and the panics.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of `done` channels and clean goroutine exit.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the idempotent close.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-broadcast-stop-to-worker-pool.md](02-broadcast-stop-to-worker-pool.md)
