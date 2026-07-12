# Exercise 5: Graceful Drain vs Hard Stop

A service behind SIGTERM has a promise to keep: finish the requests it already
accepted, refuse new ones, and get out within a deadline. That is a graceful
drain, and it is a different operation from a hard stop that abandons in-flight
work. This exercise builds both on one request-reply service, with a lock-free
admission gate and a run loop that can either drain its buffered inbox or drop it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
draining/                  independent module: example.com/draining
  go.mod
  draining.go              type Service; Drain (finish accepted), Stop (abandon)
  cmd/
    demo/
      main.go              runnable demo: accept work, drain, reject the next call
  draining_test.go         drain-finishes-accepted, hard-stop, run-exits tests
```

- Files: `draining.go`, `cmd/demo/main.go`, `draining_test.go`.
- Implement: an `accepting` atomic gate; `Drain` (stop accepting, finish already-accepted requests, then exit); `Stop` (stop accepting, abandon the inbox, callers waiting get `ErrShuttingDown`); both wait on a `done` channel.
- Test: enqueue several requests, `Drain`, assert all accepted requests return valid responses while post-`Drain` calls return `ErrShuttingDown`; a hard `Stop` frees an in-flight caller with `ErrShuttingDown`; `Run` exits (done closed); counts show no request lost.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/09-channel-of-channels/05-graceful-drain-shutdown/cmd/demo
cd go-solutions/13-goroutines-and-channels/09-channel-of-channels/05-graceful-drain-shutdown
```

### Two shutdowns, one loop

A hard stop is easy: close a `stop` channel, `Run` returns immediately, and any
caller still waiting on a reply selects `stop` and gets `ErrShuttingDown`. A
graceful drain is the careful one. It must stop *new* work from entering while
guaranteeing that every request already handed to the service is still processed —
never lost, never rejected after the fact.

The admission gate is an `atomic.Bool` named `accepting`. `Call` checks it, and
`Drain`/`Stop` clear it. But a lock-free gate has an inherent race: a caller can
read `accepting == true`, and before it sends, `Drain` can flip the flag. If the
drain loop then exits before that late send lands, the request is lost. The fix is
a small handshake. `Call` registers itself in an `inflight` counter *after*
passing the gate and re-checks the gate; `Drain` clears the gate, spins until
`inflight` reaches zero, and only then closes the drain signal. Because Go's
atomic operations are sequentially consistent, this ordering guarantees that any
`Call` which will actually send has incremented `inflight` before `Drain` observes
zero — so its request is in the inbox before the drain loop starts, and the drain
loop, which processes everything currently buffered, will handle it. No lost
request.

The run loop has three arms. The normal arm processes a request. The `drain` arm
processes everything left in the buffered inbox (a non-blocking loop with a
`default` exit) and then returns — this is the "finish accepted work" path. The
`stop` arm returns at once, abandoning the inbox — this is the "drop in-flight"
path. `Call` selects its inbox send against `stop` (so a hard stop unblocks a
blocked sender) and selects its reply receive against `stop` (so a hard stop
unblocks a caller waiting on a worker that will never answer). The graceful path
never touches `stop`, so a drained request is guaranteed to reach its reply.

Create `draining.go`:

```go
package draining

import (
	"errors"
	"runtime"
	"sync/atomic"
	"time"
)

// ErrShuttingDown is returned to callers rejected by Drain or Stop.
var ErrShuttingDown = errors.New("draining: shutting down")

type request struct {
	n     int
	reply chan response
}

type response struct {
	value int
}

// Service is a request-reply service supporting graceful drain and hard stop.
type Service struct {
	inbox     chan request
	work      time.Duration
	accepting atomic.Bool
	inflight  atomic.Int64
	drain     chan struct{}
	stop      chan struct{}
	done      chan struct{}
}

// New returns a started Service. inboxCap sizes the buffered inbox; work is the
// per-request processing time.
func New(inboxCap int, work time.Duration) *Service {
	s := &Service{
		inbox: make(chan request, inboxCap),
		work:  work,
		drain: make(chan struct{}),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	s.accepting.Store(true)
	go s.run()
	return s
}

func (s *Service) handle(req request) {
	time.Sleep(s.work)
	req.reply <- response{value: req.n * 2}
}

func (s *Service) run() {
	defer close(s.done)
	for {
		select {
		case req := <-s.inbox:
			s.handle(req)
		case <-s.drain:
			// Finish every already-accepted (buffered) request, then exit.
			for {
				select {
				case req := <-s.inbox:
					s.handle(req)
				default:
					return
				}
			}
		case <-s.stop:
			return // abandon whatever is still in the inbox
		}
	}
}

// Drain stops accepting new calls, finishes already-accepted requests, then
// waits for the run loop to exit.
func (s *Service) Drain() {
	s.accepting.Store(false)
	for s.inflight.Load() > 0 { // wait for in-flight sends to land
		runtime.Gosched()
	}
	close(s.drain)
	<-s.done
}

// Stop stops accepting new calls, abandons the inbox, and waits for the run loop
// to exit. Callers still waiting on a reply receive ErrShuttingDown.
func (s *Service) Stop() {
	s.accepting.Store(false)
	close(s.stop)
	<-s.done
}

// Call sends n and returns the doubled value, or ErrShuttingDown if the service
// has stopped accepting or was hard-stopped mid-flight.
func (s *Service) Call(n int) (int, error) {
	if !s.accepting.Load() {
		return 0, ErrShuttingDown
	}
	s.inflight.Add(1)
	if !s.accepting.Load() { // re-check: Drain may have flipped it
		s.inflight.Add(-1)
		return 0, ErrShuttingDown
	}

	reply := make(chan response, 1)
	select {
	case s.inbox <- request{n: n, reply: reply}:
		s.inflight.Add(-1)
	case <-s.stop:
		s.inflight.Add(-1)
		return 0, ErrShuttingDown
	}

	select {
	case resp := <-reply:
		return resp.value, nil
	case <-s.stop:
		return 0, ErrShuttingDown
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/draining"
)

func main() {
	s := draining.New(8, 10*time.Millisecond)

	// Accept three requests concurrently.
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := s.Call(i); err == nil {
				fmt.Printf("accepted call -> %d\n", v)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let them enter the inbox

	// Drain in the background and wait for the accepted work to finish, so the
	// accepted replies all print before the rejected line.
	go s.Drain()
	wg.Wait()

	// A call issued after the drain has begun is rejected.
	if _, err := s.Call(99); errors.Is(err, draining.ErrShuttingDown) {
		fmt.Println("post-drain call: rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the three accepted lines may appear in any order):

```
accepted call -> 2
accepted call -> 4
accepted call -> 6
post-drain call: rejected
```

### Tests

`TestDrainFinishesAccepted` accepts several concurrent calls into a buffered
inbox, drains, and asserts all of them returned valid responses (nothing lost)
while a call issued after `Drain` returns `ErrShuttingDown`.
`TestHardStopRejectsInFlight` puts one call in flight against a slow worker, hard
stops, and asserts the in-flight caller gets `ErrShuttingDown` and the run loop
exits. Both assert `done` is closed so the goroutine is joined.

Create `draining_test.go`:

```go
package draining

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainFinishesAccepted(t *testing.T) {
	t.Parallel()
	const n = 8
	s := New(16, 5*time.Millisecond)

	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Call(i)
			switch {
			case err == nil && v == i*2:
				ok.Add(1)
			case errors.Is(err, ErrShuttingDown):
				rejected.Add(1)
			default:
				t.Errorf("Call(%d) = %d, %v", i, v, err)
			}
		}()
	}

	time.Sleep(30 * time.Millisecond) // let all n enter the buffered inbox
	s.Drain()
	wg.Wait()

	if ok.Load() != n {
		t.Fatalf("accepted %d requests, want all %d finished (rejected=%d)",
			ok.Load(), n, rejected.Load())
	}

	if _, err := s.Call(1); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("post-drain Call error = %v, want ErrShuttingDown", err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("run loop did not exit after Drain")
	}
}

func TestHardStopRejectsInFlight(t *testing.T) {
	t.Parallel()
	s := New(4, 50*time.Millisecond)

	errc := make(chan error, 1)
	go func() {
		_, err := s.Call(1)
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond) // call is now in flight in the worker

	s.Stop()

	if err := <-errc; !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("in-flight Call error = %v, want ErrShuttingDown", err)
	}
	if _, err := s.Call(1); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("post-stop Call error = %v, want ErrShuttingDown", err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("run loop did not exit after Stop")
	}
}
```

## Review

The drain is correct when every accepted request finishes and every post-drain
call is rejected, with nothing lost in between. `TestDrainFinishesAccepted`
encodes "nothing lost" as `ok == n`: all eight buffered requests complete after
`Drain`. The `inflight` handshake is what makes that safe — without the
spin-until-zero step, a call that passed the gate a moment before `Drain` could
land its request after the drain loop already exited, and `ok` would be less than
`n`. The hard-stop test proves the other shape: an in-flight caller is freed with
`ErrShuttingDown` rather than blocking on a worker that never answers, because the
reply receive selects on `stop`.

The mistakes to avoid: never let `Shutdown`-style methods return before the run
loop has exited — both `Drain` and `Stop` block on `<-done`, or a test could
observe a still-running goroutine. Never drop the re-check after incrementing
`inflight`; it closes the window where `Drain` flips the gate between the first
check and the send. And keep the graceful path off the `stop` channel, so a
drained request is guaranteed to reach its reply. Run `go test -race` to confirm
the atomic gate and the channel handoffs are race-free.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Bool` and `atomic.Int64` for the lock-free admission gate.
- [Go Memory Model](https://go.dev/ref/mem) — sequential consistency of atomic operations, which the drain handshake relies on.
- [`runtime.Gosched`](https://pkg.go.dev/runtime#Gosched) — yielding while spinning on the in-flight count.
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the stdlib's own graceful-drain-with-deadline model.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-bounded-inbox-backpressure.md](06-bounded-inbox-backpressure.md)
