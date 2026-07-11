# Exercise 26: Graceful Worker Drain with Timeout Enforcement

Shutting a server down cleanly means giving in-flight background workers a
chance to finish — but "a chance" cannot mean "forever." If even one worker
ignores the stop signal, a naive `wg.Wait()` blocks the shutdown path
indefinitely and the process never exits. This exercise builds a `Shutdown`
that races the drain against a deadline, and uses a named `drained bool`
result so a single deferred closure turns "we didn't make it in time" into an
error, no matter which branch of the race wins.

**Nivel: Avanzado** — validacion normal (caso cooperativo y caso con timeout, sin sleeps reales).

## What you'll build

```text
drain/                     independent module: example.com/drain
  go.mod
  drain.go                 Server; Go (register worker); Shutdown (named drained, deferred timeout error)
  cmd/demo/
    main.go                 runnable demo: a cooperative worker and one that misses the deadline
  drain_test.go             drains a cooperative worker, times out on one that ignores stop
```

- Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
- Implement: `(*Server) Shutdown(deadline <-chan time.Time) (drained bool, err error)` that closes a stop channel, races `sync.WaitGroup.Wait()` against `deadline`, and whose deferred closure sets `err` whenever `drained` is still false.
- Test: a worker that respects `stop` drains before a deadline that never fires; a worker that ignores `stop` loses the race against a deadline that has already fired.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/drain/cmd/demo
cd ~/go-exercises/drain
go mod init example.com/drain
go mod edit -go=1.24
```

### One race, one named result, one place that turns a miss into an error

```go
select {
case <-done:
    drained = true
    return
case <-deadline:
    return
}
```

`Shutdown` never blocks past `deadline`: it starts a goroutine that closes
`done` once every registered worker has returned, then selects between that
and the deadline channel. Whichever fires first decides the outcome, and
because `drained` is a named result, the deferred closure registered at the
top of the function runs after that decision and can act on it uniformly —
if `drained` is still `false`, wrap it into a non-nil `err`. The caller never
has to guess which of two return statements produced the timeout error;
there is only one, and it lives next to the named result it inspects.

The `deadline` parameter is a `<-chan time.Time` rather than a
`time.Duration` — the same shape `time.After` returns. Production code passes
`time.After(d)`; tests pass a channel they control by hand (closing it, or
never touching it), so the outcome of the race is decided by goroutine
scheduling order, not by wall-clock timing, keeping the test deterministic.

Create `drain.go`:

```go
package drain

import (
	"errors"
	"sync"
	"time"
)

// ErrDrainTimeout is returned when Shutdown's deadline fires before every
// worker has drained.
var ErrDrainTimeout = errors.New("shutdown: timed out waiting for workers to drain")

// Server tracks running background workers and drains them on shutdown.
type Server struct {
	mu      sync.Mutex
	stopped bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewServer returns a Server ready to accept workers.
func NewServer() *Server {
	return &Server{stop: make(chan struct{})}
}

// Go starts fn as a tracked worker. fn must return (not just loop forever)
// once the stop channel it receives is closed.
func (s *Server) Go(fn func(stop <-chan struct{})) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(s.stop)
	}()
}

// Shutdown signals every worker to stop and waits for them to drain. deadline
// is a channel that fires (as time.After would) when the caller's timeout
// budget is exhausted; production code passes time.After(d), tests pass a
// channel they control directly so the outcome is deterministic.
//
// drained is a named result: a single deferred closure inspects it once
// Shutdown is about to return and, whenever the wait lost the race against
// deadline, turns that fact into a non-nil err. This enforces the deadline in
// exactly one place: Shutdown always returns promptly, even if some worker
// never drains, so the caller is never left blocked on a zombie goroutine.
func (s *Server) Shutdown(deadline <-chan time.Time) (drained bool, err error) {
	defer func() {
		if !drained {
			err = ErrDrainTimeout
		}
	}()

	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		drained = true
		return
	case <-deadline:
		return
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/drain"
)

func main() {
	// A cooperative worker: exits promptly once told to stop.
	fast := drain.NewServer()
	fast.Go(func(stop <-chan struct{}) {
		<-stop
	})
	drained, err := fast.Shutdown(time.After(time.Second))
	fmt.Printf("fast worker: drained=%v err=%v\n", drained, err)

	// An uncooperative worker: ignores stop and never returns before the
	// deadline fires.
	slow := drain.NewServer()
	block := make(chan struct{})
	defer close(block) // let the leaked goroutine exit before the demo ends
	slow.Go(func(stop <-chan struct{}) {
		<-block
	})
	deadline := make(chan time.Time)
	close(deadline) // simulate an already-expired deadline, deterministically
	drained, err = slow.Shutdown(deadline)
	fmt.Printf("slow worker: drained=%v err=%v\n", drained, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast worker: drained=true err=<nil>
slow worker: drained=false err=shutdown: timed out waiting for workers to drain
```

### Tests

Create `drain_test.go`:

```go
package drain

import (
	"testing"
	"time"
)

func TestShutdownDrainsCooperativeWorker(t *testing.T) {
	t.Parallel()

	s := NewServer()
	started := make(chan struct{})
	s.Go(func(stop <-chan struct{}) {
		close(started)
		<-stop
	})
	<-started

	deadline := make(chan time.Time) // never fires: the worker must win the race
	drained, err := s.Shutdown(deadline)
	if !drained {
		t.Fatal("drained = false, want true for a cooperative worker")
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestShutdownTimesOutOnUncooperativeWorker(t *testing.T) {
	t.Parallel()

	s := NewServer()
	started := make(chan struct{})
	block := make(chan struct{})
	defer close(block) // release the goroutine so it doesn't leak past the test
	s.Go(func(stop <-chan struct{}) {
		close(started)
		<-block // ignores stop on purpose
	})
	<-started

	deadline := make(chan time.Time)
	close(deadline) // deterministic stand-in for an already-expired timer
	drained, err := s.Shutdown(deadline)
	if drained {
		t.Fatal("drained = true, want false when the deadline fires first")
	}
	if err != ErrDrainTimeout {
		t.Fatalf("err = %v, want ErrDrainTimeout", err)
	}
}

func TestShutdownIsSafeToCallOnce(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.Go(func(stop <-chan struct{}) {
		<-stop
	})
	deadline := make(chan time.Time)
	drained, err := s.Shutdown(deadline)
	if !drained || err != nil {
		t.Fatalf("first Shutdown = (%v, %v), want (true, nil)", drained, err)
	}
}
```

## Review

`Shutdown` is correct when a cooperative worker always drains before a
deadline that never fires, and an uncooperative one never blocks the caller
past a deadline that has already fired — the two outcomes the tests pin
down. The named `drained` result is what makes the timeout logic sit in one
place: the deferred closure is the only code that decides whether to attach
`ErrDrainTimeout`, so a future third exit path (say, a context cancellation)
only has to set `drained` correctly and inherits the same error handling for
free. The mistake to avoid is calling `wg.Wait()` directly instead of racing
it against `deadline` in a `select` — that brings back the exact problem
this exercise solves, an indefinite hang on one stuck worker, only now with
extra bookkeeping around it that never gets to run.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup)
- [`time.After`](https://pkg.go.dev/time#After)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-transaction-snapshot-restore-on-abort.md](25-transaction-snapshot-restore-on-abort.md) | Next: [27-topic-subscription-unsubscribe-on-handler-error.md](27-topic-subscription-unsubscribe-on-handler-error.md)
