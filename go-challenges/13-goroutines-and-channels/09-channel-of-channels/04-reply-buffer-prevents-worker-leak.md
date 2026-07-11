# Exercise 4: Why the Reply Channel Is Buffered: No Leak on Caller Timeout

Every prior exercise buffered the reply channel at capacity one and called it an
invariant. This exercise proves why by building both variants side by side: a
service whose reply buffer you can set to zero or one, and tests that show the
unbuffered variant wedges a worker the moment a caller times out, while the
buffered variant keeps serving and leaks nothing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
replybuffer/               independent module: example.com/replybuffer
  go.mod
  replybuffer.go           type Service; reply buffer size is configurable
  cmd/
    demo/
      main.go              runnable demo: buffered service survives an abandon
  replybuffer_test.go      buffered-serves, unbuffered-stalls, no-goroutine-leak
```

- Files: `replybuffer.go`, `cmd/demo/main.go`, `replybuffer_test.go`.
- Implement: a pool `Service` with a configurable reply-buffer size; a worker whose reply send selects on `quit` so shutdown always frees it; `Call(ctx, n)` that honors the context.
- Test: with buffer 1, a call that abandons on timeout frees the worker and a later call succeeds; with buffer 0, an abandoned call wedges the worker so a later call times out (the leak, bounded by a deadline so the test fails loudly); a `runtime.NumGoroutine` check shows no accumulation after shutdown.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/replybuffer/cmd/demo
cd ~/go-exercises/replybuffer
go mod init example.com/replybuffer
```

### The leak, made concrete

A worker finishes a request and sends the response on the caller's reply channel.
If that channel is unbuffered and the caller has already returned — its deadline
fired, it stopped receiving — the send has no receiver and never will. On an
unbuffered channel the send blocks until a receiver appears, so the worker parks
on that send forever. That goroutine is now leaked: it is not processing new
requests, and if it is one of a fixed pool, the pool has permanently lost a
worker. Under a steady rate of caller timeouts, the pool bleeds workers until it
can serve nothing at all, and the process's goroutine count climbs without bound.

A capacity-one buffer removes the failure. The worker's send drops the response
into the buffer and returns immediately, receiver or not; the abandoned channel
and its one buffered value are garbage-collected together. The worker loops back
to the request channel and serves the next caller. One slot is exactly right
because there is exactly one response per request.

To demonstrate both without permanently hanging the test, the worker's reply send
selects against `quit`, so even a wedged unbuffered worker is freed at shutdown —
the leak is real between timeout and shutdown, which is what the test observes,
but the module still cleans up. The size of the reply buffer is the single knob:
`New(workers, buffer, work)`.

Create `replybuffer.go`:

```go
package replybuffer

import (
	"context"
	"sync"
	"time"
)

type request struct {
	n     int
	reply chan response
}

type response struct {
	value int
}

// Service is a fixed pool of workers. The reply-channel buffer size is
// configurable so tests can contrast the buffered and unbuffered behavior.
type Service struct {
	requests chan request
	buffer   int
	work     time.Duration
	quit     chan struct{}
	wg       sync.WaitGroup
}

// New returns a Service with the given number of workers, reply-buffer size, and
// per-request processing time. Start it with Start.
func New(workers, buffer int, work time.Duration) *Service {
	s := &Service{
		requests: make(chan request),
		buffer:   buffer,
		work:     work,
		quit:     make(chan struct{}),
	}
	s.wg.Add(workers)
	for range workers {
		go s.worker()
	}
	return s
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case req := <-s.requests:
			time.Sleep(s.work)
			// The reply send selects on quit so a wedged worker (unbuffered
			// reply, abandoned caller) is still freed at Shutdown.
			select {
			case req.reply <- response{value: req.n * 2}:
			case <-s.quit:
				return
			}
		case <-s.quit:
			return
		}
	}
}

// Shutdown stops all workers and waits for them to exit.
func (s *Service) Shutdown() {
	close(s.quit)
	s.wg.Wait()
}

// Call sends n and returns the doubled value. The reply channel is buffered with
// the service's configured size.
func (s *Service) Call(ctx context.Context, n int) (int, error) {
	reply := make(chan response, s.buffer)
	select {
	case s.requests <- request{n: n, reply: reply}:
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, context.Canceled
	}
	select {
	case resp := <-reply:
		return resp.value, nil
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, context.Canceled
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/replybuffer"
)

func main() {
	s := replybuffer.New(1, 1, 20*time.Millisecond) // buffered reply
	defer s.Shutdown()

	// Abandon a call by timing it out.
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel1()
	if _, err := s.Call(ctx1, 1); errors.Is(err, context.DeadlineExceeded) {
		fmt.Println("first call abandoned")
	}

	// The single worker is not wedged: a later call still succeeds.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if v, err := s.Call(ctx2, 21); err == nil {
		fmt.Printf("later call: %d\n", v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first call abandoned
later call: 42
```

### Tests

`TestBufferedServesAfterAbandon` uses buffer 1 and a single worker: it times a
call out, then asserts a later call succeeds — the worker was freed by the buffer.
`TestUnbufferedStallsAfterAbandon` uses buffer 0 and a single worker: it times a
call out, then asserts a later call *also* times out, because the sole worker is
wedged on the abandoned reply send. That is the leak, and the second call's own
deadline makes the test fail loudly instead of hanging. `TestNoGoroutineLeak`
fires many timing-out calls at a buffered pool, shuts it down, and asserts the
goroutine count returns to its baseline — no workers left stuck.

Create `replybuffer_test.go`:

```go
package replybuffer

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestBufferedServesAfterAbandon(t *testing.T) {
	t.Parallel()
	s := New(1, 1, 30*time.Millisecond) // buffered reply
	defer s.Shutdown()

	ctx1, cancel1 := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel1()
	if _, err := s.Call(ctx1, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want DeadlineExceeded", err)
	}

	ctx2, cancel2 := context.WithTimeout(t.Context(), time.Second)
	defer cancel2()
	got, err := s.Call(ctx2, 21)
	if err != nil {
		t.Fatalf("later call error = %v, want success (worker not wedged)", err)
	}
	if got != 42 {
		t.Fatalf("later call = %d, want 42", got)
	}
}

func TestUnbufferedStallsAfterAbandon(t *testing.T) {
	t.Parallel()
	s := New(1, 0, 30*time.Millisecond) // UNbuffered reply
	defer s.Shutdown()

	ctx1, cancel1 := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel1()
	if _, err := s.Call(ctx1, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want DeadlineExceeded", err)
	}

	// The sole worker is now wedged forever on the abandoned reply send, so the
	// next call cannot be served and times out too. The deadline bounds the test.
	ctx2, cancel2 := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel2()
	if _, err := s.Call(ctx2, 21); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("later call error = %v, want DeadlineExceeded (worker wedged)", err)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	s := New(4, 1, 20*time.Millisecond) // buffered reply
	for range 50 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		_, _ = s.Call(ctx, 1)
		cancel()
	}
	s.Shutdown()

	// Exiting goroutines linger in the count briefly; GC and poll back to base.
	var n int
	for range 100 {
		runtime.GC()
		n = runtime.NumGoroutine()
		if n <= base {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutine count = %d, did not return to baseline %d", n, base)
}
```

`TestNoGoroutineLeak` does not call `t.Parallel()`: it samples the process-wide
goroutine count, which other parallel tests would perturb.

## Review

The invariant is one line of code — `make(chan response, s.buffer)` with
`buffer == 1` — and two tests pin its consequence. `TestBufferedServesAfterAbandon`
shows the worker survives an abandoned caller; `TestUnbufferedStallsAfterAbandon`
shows it does not when the buffer is zero. The contrast is the entire lesson: an
unbuffered reply turns every caller timeout into a wedged worker, and a pool
under load loses workers one abandoned call at a time.

The mistakes to avoid: never size the reply channel by "how many callers" — it is
always exactly one, because there is one response per request; and do not confuse
buffering the *reply* (correct, bounded, per-request) with buffering the *inbox*
(a different concern, covered in the backpressure exercise). Run
`go test -race`; the buffered handoff is race-free, and the goroutine-count test
confirms no worker is left parked after shutdown.

## Resources

- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the coarse count the leak test polls.
- [`runtime.GC`](https://pkg.go.dev/runtime#GC) — force a collection so exited goroutines are finalized before counting.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — send semantics on buffered vs unbuffered channels.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — abandoned receivers and the leaks they cause.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-graceful-drain-shutdown.md](05-graceful-drain-shutdown.md)
