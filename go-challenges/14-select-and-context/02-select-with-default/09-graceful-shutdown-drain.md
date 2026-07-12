# Exercise 9: Drain Pending Work on SIGTERM Within a Shutdown Deadline

When Kubernetes sends SIGTERM, a well-behaved service stops accepting new work and
flushes what is already queued — but only within the grace period, after which it is
killed anyway. The correct primitive is a non-blocking drain bounded by a context
deadline: empty the buffer and exit, reporting how much was flushed versus
abandoned. The wrong primitive is a blocking `for range` on a channel whose producer
already stopped, which hangs the shutdown path past its deadline. This ties the
whole lesson to chapter 14's graceful-shutdown theme.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
drainserver/                independent module: example.com/drainserver
  go.mod                    go 1.26
  server.go                 type Server; TrySubmit, Shutdown(ctx) (flushed, abandoned)
  cmd/
    demo/
      main.go               queue work, shut down, report flushed vs abandoned
  server_test.go            full drain, deadline abandon, idempotent, -race accounting
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `New(queue int)`, `TrySubmit(func()) bool` (rejects once shutting down or full), `Pending() int`, and `Shutdown(ctx) (flushed, abandoned int)` — a non-blocking drain bounded by `ctx`, made idempotent with `sync.Once` and gated by an `atomic.Bool`.
- Test: a generous deadline flushes all queued jobs and further `TrySubmit` is rejected; a tight deadline with slow jobs abandons the remainder and returns promptly; a second `Shutdown` is a no-op; a `-race` run where `flushed + abandoned + pending` equals the accepted count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Stop accepting, then drain against the clock

`Shutdown` does two things in order. First it flips an `atomic.Bool` so `TrySubmit`
starts rejecting new work — a submitter that arrives during shutdown gets a clean
false, not a slot in a queue that is being torn down. Second it drains the already
-queued jobs in a loop whose every iteration is bounded by the shutdown context: an
outer `select` checks `ctx.Done()` before processing the next job, and when the
deadline passes it switches from *processing* the remainder to *counting* it as
abandoned via a non-blocking drain. That is the distinction the exercise is about —
"flush what fits in the deadline, count the rest, and return" rather than "block
until every job is done" (which ignores the deadline) or "block waiting for more
jobs" (which hangs when the producer has already stopped).

The `default` in the inner `select` is what makes the drain *non-blocking*: it stops
the instant the buffer is empty rather than waiting for a job that will never come.
Contrast the naive `for job := range s.jobs`: because nothing closes `jobs` during
shutdown, that loop blocks forever once the buffer empties, and the shutdown path
never returns. The non-blocking drain reads what is there and stops.

`sync.Once` makes `Shutdown` idempotent: a service may receive SIGTERM and also call
`Shutdown` from a defer, and running the drain twice would double-count or block on
an empty channel. The `Once` guarantees the drain body runs exactly once; the second
call returns the zero counts. The counters are the function's named returns, which
the `Once`-wrapped closure writes directly.

Note a deliberate limitation that mirrors reality: `Shutdown` runs each job
synchronously, so a single job that blocks *forever* would hang the drain — the
deadline can only be honored between jobs, not inside one. That is why request
handlers must themselves respect the context; a graceful-shutdown deadline is an
upper bound on well-behaved work, not a way to interrupt a wedged handler.

Create `server.go`:

```go
package drainserver

import (
	"context"
	"sync"
	"sync/atomic"
)

// Server accepts jobs into a bounded queue and, on Shutdown, drains the queue
// within a deadline, reporting how many jobs were flushed versus abandoned.
type Server struct {
	jobs      chan func()
	accepting atomic.Bool
	once      sync.Once
}

// New returns a Server accepting work into a queue of the given capacity.
func New(queue int) *Server {
	s := &Server{jobs: make(chan func(), queue)}
	s.accepting.Store(true)
	return s
}

// TrySubmit enqueues job if the server is still accepting and the queue has room.
// It returns false (without blocking) once Shutdown has begun or the queue is full.
func (s *Server) TrySubmit(job func()) bool {
	if !s.accepting.Load() {
		return false
	}
	select {
	case s.jobs <- job:
		return true
	default:
		return false
	}
}

// Pending reports how many jobs are currently buffered.
func (s *Server) Pending() int {
	return len(s.jobs)
}

// Shutdown stops accepting new work, then processes queued jobs until the queue is
// empty or ctx is done, whichever comes first. Jobs run past the deadline are
// counted as abandoned. It is idempotent: a second call is a no-op returning (0,0).
func (s *Server) Shutdown(ctx context.Context) (flushed, abandoned int) {
	s.once.Do(func() {
		s.accepting.Store(false)
		for {
			select {
			case <-ctx.Done():
				// Deadline reached: count the remainder without running it.
				for {
					select {
					case <-s.jobs:
						abandoned++
					default:
						return
					}
				}
			default:
				// Still within the deadline: run the next queued job if any.
				select {
				case job := <-s.jobs:
					job()
					flushed++
				default:
					return // queue drained
				}
			}
		}
	})
	return
}
```

### The runnable demo

The demo queues five fast jobs and shuts down with a generous deadline, so all five
flush and none are abandoned; a post-shutdown `TrySubmit` is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/drainserver"
)

func main() {
	s := drainserver.New(16)

	var done atomic.Int64
	for range 5 {
		s.TrySubmit(func() { done.Add(1) })
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	flushed, abandoned := s.Shutdown(ctx)

	fmt.Println("flushed:", flushed)
	fmt.Println("abandoned:", abandoned)
	fmt.Println("ran:", done.Load())
	fmt.Println("submit after shutdown:", s.TrySubmit(func() {}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed: 5
abandoned: 0
ran: 5
submit after shutdown: false
```

### Tests

`TestDrainsAllWithinDeadline` queues M fast jobs, shuts down with a long deadline,
and asserts all M flushed, zero abandoned, every job ran once, and post-shutdown
submits are rejected. `TestTightDeadlineAbandons` queues slow jobs under a tight
deadline and asserts `flushed < M`, `abandoned == M - flushed`, and that `Shutdown`
returns promptly rather than running every job. `TestShutdownIdempotent` asserts the
second call is a no-op. `TestConcurrentSubmitShutdown` runs submitters against a
concurrent shutdown under `-race` and asserts the conservation invariant
`flushed + abandoned + pending == accepted`.

Create `server_test.go`:

```go
package drainserver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainsAllWithinDeadline(t *testing.T) {
	t.Parallel()

	const m = 50
	s := New(m)
	var ran atomic.Int64
	for range m {
		if !s.TrySubmit(func() { ran.Add(1) }) {
			t.Fatal("submit rejected before shutdown")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flushed, abandoned := s.Shutdown(ctx)

	if flushed != m || abandoned != 0 {
		t.Fatalf("flushed=%d abandoned=%d, want %d and 0", flushed, abandoned, m)
	}
	if ran.Load() != m {
		t.Fatalf("ran %d jobs, want %d", ran.Load(), m)
	}
	if s.TrySubmit(func() {}) {
		t.Fatal("TrySubmit accepted after shutdown")
	}
}

func TestTightDeadlineAbandons(t *testing.T) {
	t.Parallel()

	const m = 30
	const jobDur = 20 * time.Millisecond
	const deadline = 50 * time.Millisecond

	s := New(m)
	for range m {
		s.TrySubmit(func() { time.Sleep(jobDur) })
	}

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	flushed, abandoned := s.Shutdown(ctx)
	elapsed := time.Since(start)

	if flushed >= m {
		t.Fatalf("flushed=%d, want fewer than %d under a tight deadline", flushed, m)
	}
	if flushed+abandoned != m {
		t.Fatalf("flushed=%d + abandoned=%d != %d (jobs lost)", flushed, abandoned, m)
	}
	// Prompt: does not run all M slow jobs; returns near the deadline.
	if elapsed > deadline+5*jobDur {
		t.Fatalf("Shutdown took %v, want near the %v deadline", elapsed, deadline)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	t.Parallel()

	const m = 10
	s := New(m)
	var ran atomic.Int64
	for range m {
		s.TrySubmit(func() { ran.Add(1) })
	}

	f1, a1 := s.Shutdown(context.Background())
	if f1 != m || a1 != 0 {
		t.Fatalf("first Shutdown: flushed=%d abandoned=%d, want %d and 0", f1, a1, m)
	}

	f2, a2 := s.Shutdown(context.Background())
	if f2 != 0 || a2 != 0 {
		t.Fatalf("second Shutdown: flushed=%d abandoned=%d, want 0 and 0 (no-op)", f2, a2)
	}
	if ran.Load() != m {
		t.Fatalf("jobs ran %d times, want %d (not re-run)", ran.Load(), m)
	}
}

func TestConcurrentSubmitShutdown(t *testing.T) {
	t.Parallel()

	s := New(256)
	var accepted atomic.Int64

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if s.TrySubmit(func() {}) {
					accepted.Add(1)
				}
			}
		}()
	}

	var flushed, abandoned int
	shutdownDone := make(chan struct{})
	go func() {
		flushed, abandoned = s.Shutdown(context.Background())
		close(shutdownDone)
	}()

	wg.Wait()
	<-shutdownDone

	// Every accepted job is flushed, abandoned, or still buffered (a late accept
	// that landed after the drain saw the queue empty). Nothing is lost.
	if flushed+abandoned+s.Pending() != int(accepted.Load()) {
		t.Fatalf("flushed=%d + abandoned=%d + pending=%d != accepted=%d",
			flushed, abandoned, s.Pending(), accepted.Load())
	}
}
```

## Review

The server is correct when shutdown conserves work and honors the deadline: with
time to spare every queued job runs exactly once and late submits are rejected; under
a tight deadline the flushed-plus-abandoned count still equals what was queued, and
`Shutdown` returns near the deadline instead of grinding through every slow job. The
central mistake the exercise inoculates against is draining with a blocking receive —
`for job := range s.jobs` never returns once the buffer empties because nothing
closes the channel, so the shutdown path hangs past its grace period and the
orchestrator hard-kills the process mid-flush. The non-blocking inner `select` with
its `default` is the fix. The concurrent test's conservation invariant —
`flushed + abandoned + pending == accepted` — is what proves no job is silently
dropped even in the genuine SIGTERM race where a submit lands just as the drain
finishes, and `sync.Once` plus the `atomic.Bool` are what keep a doubly-invoked
shutdown from double-counting or corrupting the accept gate.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the non-blocking drain and the `ctx.Done`/job `select`.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — idempotent shutdown.
- [`context`](https://pkg.go.dev/context) — `WithTimeout`/`Done` for the deadline-bounded drain.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — cancellation and draining across stages.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-timeout-with-select/00-concepts.md](../03-timeout-with-select/00-concepts.md)
