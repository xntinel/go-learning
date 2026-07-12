# Exercise 7: Graceful Shutdown — Draining In-Flight Requests With a WaitGroup Deadline

When a server receives a shutdown signal it must stop accepting new work and wait for
the requests already in flight to finish — but not forever. This module builds that
drain: a WaitGroup counts in-flight requests, `Shutdown(ctx)` flips a draining flag so
new requests are rejected, and it waits for the counter to reach zero or the context
deadline, whichever comes first. Because `wg.Wait()` is not cancelable, we run it in a
goroutine and `select` against `ctx.Done()`.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
drain/                     independent module: example.com/drain
  go.mod                   go 1.25
  drain.go                 Server tracks in-flight work; Shutdown drains with deadline
  cmd/
    demo/
      main.go              runnable demo: 3 requests drain cleanly
  drain_test.go            clean drain; deadline-exceeded; post-shutdown rejection
```

- Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
- Implement: `Serve(fn) bool` (rejects when draining), `Shutdown(ctx) error` — run `wg.Wait()` in a goroutine, `select` on a done channel vs `ctx.Done()`.
- Test: K slow requests drain cleanly under a generous deadline; post-shutdown `Serve` returns false; a short deadline returns `context.DeadlineExceeded` without blocking forever.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why Wait needs a goroutine, and why a mutex not just an atomic

`wg.Wait()` has no deadline. On a shutdown path you cannot afford to block on it
indefinitely — one wedged request would hang the whole process. The idiom is to move
the blocking call off the critical path: launch a goroutine that does `wg.Wait();
close(done)`, then `select` between `done` (drain finished) and `ctx.Done()` (deadline
hit). If the deadline wins, `Shutdown` returns `context.DeadlineExceeded`; the
in-flight goroutines keep running, but *you* stop waiting.

The second subtlety is the race between accepting a request and starting to drain. You
might reach for an `atomic.Bool` draining flag, but a bare atomic leaves a real
window: `Serve` reads the flag as false, and *before* it calls `wg.Add(1)`, `Shutdown`
flips the flag and calls `wg.Wait()` while the counter is zero. A positive `Add` that
begins when the counter is zero and races a `Wait` is undefined by the memory model —
a genuine bug the race detector can catch. The fix is to make "check the flag and
`Add`" atomic *with respect to* "set the flag": guard both with a `sync.Mutex`. Once
`Shutdown` has set `draining` under the lock, every later `Serve` observes it and is
rejected, and no `Add` can race the `Wait` that follows. This is the kind of detail
that separates a shutdown path that works under load from one that flakes.

Create `drain.go`:

```go
package drain

import (
	"context"
	"sync"
)

// Server tracks in-flight work so it can drain on shutdown.
type Server struct {
	mu       sync.Mutex
	draining bool
	wg       sync.WaitGroup
}

// Serve starts fn as an in-flight request and returns true. Once Shutdown has
// begun it rejects new work and returns false without running fn.
func (s *Server) Serve(fn func()) bool {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return false
	}
	s.wg.Add(1) // Add under the lock so it cannot race Shutdown's Wait
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		fn()
	}()
	return true
}

// Shutdown stops accepting new requests and waits for in-flight work to finish,
// or for ctx to expire. It returns nil on a clean drain, or ctx.Err() if the
// deadline is reached first. In-flight goroutines are not forcibly stopped.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
	"sync/atomic"
	"time"

	"example.com/drain"
)

func main() {
	var srv drain.Server
	var completed atomic.Int64

	for range 3 {
		srv.Serve(func() {
			time.Sleep(20 * time.Millisecond)
			completed.Add(1)
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := srv.Shutdown(ctx)
	accepted := srv.Serve(func() {}) // rejected: draining

	fmt.Printf("shutdown err: %v\n", err)
	fmt.Printf("completed: %d\n", completed.Load())
	fmt.Printf("post-shutdown accepted: %v\n", accepted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shutdown err: <nil>
completed: 3
post-shutdown accepted: false
```

### Tests

`TestShutdownDrainsCleanly` starts several slow requests, calls `Shutdown` with a
deadline longer than they take, and asserts a nil error, that every request completed,
and that a post-shutdown `Serve` is rejected. `TestShutdownDeadlineExceeded` starts a
request longer than the deadline and asserts `Shutdown` returns
`context.DeadlineExceeded` promptly rather than blocking for the full request; it then
waits for the straggler to finish so no goroutine leaks past the test.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownDrainsCleanly(t *testing.T) {
	t.Parallel()

	var srv Server
	var completed atomic.Int64

	const k = 5
	for range k {
		if !srv.Serve(func() {
			time.Sleep(10 * time.Millisecond)
			completed.Add(1)
		}) {
			t.Fatal("Serve rejected before shutdown")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if got := completed.Load(); got != k {
		t.Fatalf("completed = %d, want %d", got, k)
	}
	if srv.Serve(func() {}) {
		t.Fatal("Serve accepted work after shutdown")
	}
}

func TestShutdownDeadlineExceeded(t *testing.T) {
	t.Parallel()

	var srv Server
	finished := make(chan struct{})
	if !srv.Serve(func() {
		time.Sleep(80 * time.Millisecond) // longer than the deadline
		close(finished)
	}) {
		t.Fatal("Serve rejected before shutdown")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := srv.Shutdown(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 80*time.Millisecond {
		t.Fatalf("Shutdown blocked %v, should have returned near the deadline", elapsed)
	}

	<-finished // let the straggler finish so it does not outlive the test
}

func TestServeRejectedWhileDraining(t *testing.T) {
	t.Parallel()

	var srv Server
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown err = %v, want nil (nothing in flight)", err)
	}
	if srv.Serve(func() {}) {
		t.Fatal("Serve accepted work after shutdown")
	}
}
```

## Review

The drain is correct when a generous deadline yields a nil error with every in-flight
request completed, a post-shutdown `Serve` returns false, and a short deadline returns
`context.DeadlineExceeded` in about the deadline duration rather than the full request
time. The deadline test also asserts `Shutdown` returned quickly, which is the whole
point of racing `Wait` against `ctx.Done()`.

Two details make or break this under load. First, `Wait` must run in its own goroutine
so the `select` can prefer the deadline; blocking on `Wait` directly reintroduces the
unbounded hang. Second, the drain-check-and-`Add` must be atomic with respect to
setting the flag — the mutex here is not decoration, it closes the Add-races-Wait
window that a bare atomic would leave open. Run `go test -race` to confirm.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the in-flight counter.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — bounding the drain.
- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — the standard library's own graceful-drain contract, which this mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-errgroup-setlimit-batch-processor.md](06-errgroup-setlimit-batch-processor.md) | Next: [08-fan-out-fan-in-channel-close.md](08-fan-out-fan-in-channel-close.md)
