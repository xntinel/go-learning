# Exercise 7: Draining a Buffered Work Channel on Graceful Shutdown

When a process receives SIGTERM it has a short grace period to finish what it already
accepted before it exits. Dropping in-flight work — requests you already told the
client you accepted — is a correctness bug, not just a blip. The buffered work channel
is what makes a clean drain possible: on shutdown you stop accepting, close the input,
and let the workers drain the jobs still sitting in the buffer. This exercise builds
that server and its `Shutdown(ctx)` path.

This module is fully self-contained.

## What you'll build

```text
drain/                       module: example.com/drain
  go.mod                     go 1.26
  drain.go                   type Server; New, Enqueue, Shutdown(ctx) (drained, err)
  cmd/
    demo/
      main.go                enqueue 5 jobs, shut down, print drained count
  drain_test.go              no-loss drain, deadline-exceeded partial drain, post-shutdown reject
```

- Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
- Implement: `Server` with a buffered jobs channel; `Enqueue(job) bool` (rejects after shutdown), `Shutdown(ctx) (drained int, err error)` that closes input, drains, and returns.
- Test: enqueue K, shut down with a generous deadline, assert all K drained; assert `Shutdown` returns `context.DeadlineExceeded` when work outlasts a tiny deadline; assert post-shutdown `Enqueue` is rejected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why buffered survives shutdown and unbuffered does not

Closing a channel does not throw away what is already in its buffer. When `Shutdown`
closes `jobs`, each worker's `for j := range jobs` keeps yielding the buffered values
until the buffer is empty, and only then does the `range` end and the worker return.
That is the entire mechanism of a clean drain: work that was accepted (it is sitting in
the buffer) is still processed after you stop accepting new work. Contrast an
*unbuffered* input: it holds nothing, so there is nothing to drain — any producer
blocked mid-handoff at shutdown simply fails to complete. Buffered-vs-unbuffered here
decides whether already-accepted work survives a shutdown, which is why an ingest path
that promises durability uses a buffered queue.

The shutdown sequence has a strict order. First, stop accepting: an `accepting` flag,
flipped under a mutex, makes `Enqueue` return `false` so callers know to retry
elsewhere or fail fast. Second, close `jobs` — exactly once, from the shutdown path,
which is the sole closer. The mutex does double duty: `Enqueue` does its
admission-check-and-send under the lock, and `Shutdown` flips the flag and closes under
the same lock, so a send can never race the close (no "send on closed channel" panic).
Third, wait for the workers to drain, but bounded by the caller's context: a goroutine
signals when `wg.Wait()` returns, and `Shutdown` selects between that and `ctx.Done()`.
If the workers finish first, it returns the drained count and `nil`. If the deadline
hits first, it returns however many drained so far and `ctx.Err()` — the operator gets
told the drain was incomplete rather than the process hanging past its grace period.

`processed` is an `atomic.Int64` incremented per job so the drained count is race-free
without holding the lock on the hot path. The idempotency guard (`if s.accepting`)
means a second `Shutdown` call does not double-close.

Create `drain.go`:

```go
package drain

import (
	"context"
	"sync"
	"sync/atomic"
)

// Server processes jobs through a pool of workers reading a buffered channel. On
// Shutdown it stops accepting, closes the channel, and lets workers drain the
// buffered jobs so nothing already accepted is dropped.
type Server struct {
	jobs    chan int
	process func(int) int

	mu        sync.Mutex
	accepting bool

	wg        sync.WaitGroup
	processed atomic.Int64
}

// New starts a server with a buffered jobs channel of bufSize and the given number
// of workers.
func New(bufSize, workers int, process func(int) int) *Server {
	s := &Server{
		jobs:      make(chan int, bufSize),
		process:   process,
		accepting: true,
	}
	for range workers {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *Server) worker() {
	defer s.wg.Done()
	for j := range s.jobs {
		_ = s.process(j)
		s.processed.Add(1)
	}
}

// Enqueue admits a job unless the server is shutting down or the buffer is full, in
// which case it returns false.
func (s *Server) Enqueue(job int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return false
	}
	select {
	case s.jobs <- job:
		return true
	default:
		return false
	}
}

// Shutdown stops accepting new work, closes the input, and waits for workers to
// drain the buffered jobs, bounded by ctx. It returns the number of jobs drained and
// ctx.Err() if the deadline hit before the drain finished.
func (s *Server) Shutdown(ctx context.Context) (drained int, err error) {
	s.mu.Lock()
	if s.accepting {
		s.accepting = false
		close(s.jobs)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return int(s.processed.Load()), nil
	case <-ctx.Done():
		return int(s.processed.Load()), ctx.Err()
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/drain"
)

func main() {
	s := drain.New(16, 3, func(n int) int { return n * n })

	for i := range 5 {
		s.Enqueue(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	drained, err := s.Shutdown(ctx)

	fmt.Printf("drained=%d err=%v\n", drained, err)
	fmt.Printf("post-shutdown enqueue accepted: %v\n", s.Enqueue(99))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained=5 err=<nil>
post-shutdown enqueue accepted: false
```

### Tests

`TestDrainsAllAcceptedWork` enqueues K jobs, shuts down with a generous deadline, and
asserts `drained == K` and `err == nil` — the no-loss guarantee. `TestDeadlineExceeded`
uses a slow `process` and a tiny deadline so the drain cannot finish in time, and
asserts `Shutdown` returns `context.DeadlineExceeded` with a partial count.
`TestRejectsAfterShutdown` asserts `Enqueue` returns `false` once shut down. The
`go test` timeout guards against a missing `close(jobs)` turning the drain into a hang.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDrainsAllAcceptedWork(t *testing.T) {
	t.Parallel()

	const k = 50
	s := New(k, 4, func(n int) int { return n * n })
	for i := range k {
		if !s.Enqueue(i) {
			t.Fatalf("Enqueue(%d) rejected before shutdown", i)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained, err := s.Shutdown(ctx)

	if err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if drained != k {
		t.Fatalf("drained = %d, want %d (work lost on shutdown)", drained, k)
	}
}

func TestDeadlineExceeded(t *testing.T) {
	t.Parallel()

	// A slow processor: the drain cannot finish inside a 1ms deadline.
	s := New(64, 1, func(n int) int {
		time.Sleep(50 * time.Millisecond)
		return n
	})
	for i := range 20 {
		s.Enqueue(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	drained, err := s.Shutdown(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}
	if drained > 20 {
		t.Fatalf("drained = %d, impossible (only 20 enqueued)", drained)
	}
}

func TestRejectsAfterShutdown(t *testing.T) {
	t.Parallel()

	s := New(4, 2, func(n int) int { return n })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if s.Enqueue(1) {
		t.Fatal("Enqueue accepted after shutdown; want rejected")
	}
}

func ExampleServer() {
	s := New(16, 3, func(n int) int { return n * n })
	for i := range 5 {
		s.Enqueue(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	drained, err := s.Shutdown(ctx)

	fmt.Println(drained, err, s.Enqueue(99)) // all 5 drained; post-shutdown enqueue rejected
	// Output: 5 <nil> false
}
```

## Review

The drain is correct when closing the buffered `jobs` channel lets each worker finish
the values still in the buffer before its `range` ends — that is what guarantees
accepted work is not dropped. The mutex serializing `Enqueue`'s send with `Shutdown`'s
close is what prevents a "send on closed channel" panic; the idempotency guard prevents
a double close. Bounding the drain with the caller's context is what keeps `Shutdown`
from hanging past the grace period: it returns `ctx.Err()` and a partial count instead.
The mistake to avoid is treating shutdown as "close and hope" with no deadline, or using
an unbuffered input and then being surprised that in-flight work vanishes — an unbuffered
channel has nothing to drain.

## Resources

- [pkg.go.dev: context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — bounding the drain with a deadline.
- [pkg.go.dev: net/http.Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the stdlib graceful-shutdown-with-context pattern this mirrors.
- [Go spec: close](https://go.dev/ref/spec#Close) — receivers drain buffered values before a closed channel reports not-ok.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-batch-flush-buffer.md](06-batch-flush-buffer.md) | Next: [08-backpressure-benchmark.md](08-backpressure-benchmark.md)
