# Exercise 6: Bounded Inbox and Load Shedding

An unbounded request queue is a time bomb: it absorbs an overload spike silently
and then takes the process down with it when memory runs out. The production
answer is a bounded inbox plus admission control — a non-blocking `TryCall` that
returns a busy error the instant the queue is full, so the service sheds load
deterministically instead of queueing without limit. This exercise builds that
and exposes the queue depth so an operator can watch it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
shedding/                  independent module: example.com/shedding
  go.mod
  shedding.go              type Service; TryCall (default -> ErrBusy), Pending, Capacity
  cmd/
    demo/
      main.go              runnable demo: fill the inbox, observe shedding
  shedding_test.go         sheds-when-full, serves-when-space, depth tests
```

- Files: `shedding.go`, `cmd/demo/main.go`, `shedding_test.go`.
- Implement: a buffered inbox; `TryCall` whose send is a `select` with a `default` that returns `ErrBusy` when the buffer is full; `Pending()` and `Capacity()` reporting `len` and `cap` of the inbox.
- Test: with no consumer running, fill the inbox and assert exactly `cap` calls are admitted and the rest return `ErrBusy`; assert `Pending`/`Capacity` match; start the worker, assert the admitted calls drain and succeed; a normal `TryCall` with space succeeds.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shedding/cmd/demo
cd ~/go-exercises/shedding
go mod init example.com/shedding
```

### Admission control with a non-blocking send

The inbox is a buffered channel, `make(chan request, capacity)`. Admission is a
`select` with a `default`:

```
select {
case s.inbox <- req:
	// admitted
default:
	return 0, ErrBusy
}
```

When the buffer has room the send succeeds and the request is admitted; when the
buffer is full the send would block, so the `default` arm runs immediately and the
caller is shed with `ErrBusy`. There is no waiting: `TryCall` either takes the
request now or refuses it now. That is the difference between backpressure and an
unbounded queue — the caller learns about overload synchronously and can react
(retry later, return 503, drop the work) instead of piling onto a queue that only
grows.

`Pending()` returns `len(inbox)` — how many requests are queued but not yet
picked up — and `Capacity()` returns `cap(inbox)`. Reading `len` and `cap` of a
channel is safe to do concurrently; they are the natural gauges to export for
autoscaling or alerting. The interesting property to test is that with no worker
consuming, exactly `capacity` calls are admitted before the rest shed: the
buffered channel fills to its capacity and no further, so the admitted count is
deterministic even though which callers win the race is not.

Create `shedding.go`:

```go
package shedding

import "errors"

// ErrBusy is returned by TryCall when the inbox is full (load shed).
var ErrBusy = errors.New("shedding: busy")

// ErrShuttingDown is returned when the service is shut down while a caller waits.
var ErrShuttingDown = errors.New("shedding: shutting down")

type request struct {
	n     int
	reply chan response
}

type response struct {
	value int
}

// Service admits requests into a bounded inbox and sheds load when it is full.
type Service struct {
	inbox chan request
	quit  chan struct{}
	done  chan struct{}
}

// New returns a Service with an inbox of the given capacity. Start the worker
// with go s.Run().
func New(capacity int) *Service {
	return &Service{
		inbox: make(chan request, capacity),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Run is the worker loop. Start it with: go s.Run().
func (s *Service) Run() {
	defer close(s.done)
	for {
		select {
		case req := <-s.inbox:
			req.reply <- response{value: req.n * 2}
		case <-s.quit:
			return
		}
	}
}

// Shutdown stops the worker and waits for it to exit.
func (s *Service) Shutdown() {
	close(s.quit)
	<-s.done
}

// Pending reports how many requests are queued but not yet picked up.
func (s *Service) Pending() int { return len(s.inbox) }

// Capacity reports the inbox capacity.
func (s *Service) Capacity() int { return cap(s.inbox) }

// TryCall admits the request if the inbox has room, else returns ErrBusy at once.
func (s *Service) TryCall(n int) (int, error) {
	reply := make(chan response, 1)
	select {
	case s.inbox <- request{n: n, reply: reply}:
		// admitted
	default:
		return 0, ErrBusy
	}
	select {
	case resp := <-reply:
		return resp.value, nil
	case <-s.quit:
		return 0, ErrShuttingDown
	}
}
```

### The runnable demo

The demo fills a small inbox before starting the worker, so you can watch the
shedding boundary directly: the first `cap` calls are admitted (and block in the
background waiting for a reply), and the next ones are shed with `ErrBusy`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"runtime"
	"sync"

	"example.com/shedding"
)

func main() {
	s := shedding.New(2)

	var wg sync.WaitGroup
	var shed int
	var mu sync.Mutex
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.TryCall(1); errors.Is(err, shedding.ErrBusy) {
				mu.Lock()
				shed++
				mu.Unlock()
			}
		}()
	}

	// Wait until the 3 losers have been shed before starting the worker, so the
	// shed count is deterministic (the 2 winners hold the full buffer meanwhile).
	for {
		mu.Lock()
		shedAll := shed == 3
		mu.Unlock()
		if shedAll {
			break
		}
		runtime.Gosched()
	}
	go s.Run() // start draining so the admitted calls complete
	wg.Wait()

	fmt.Printf("capacity: %d\n", s.Capacity())
	fmt.Printf("shed: %d\n", shed)
	s.Shutdown()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
capacity: 2
shed: 3
```

### Tests

`TestShedsWhenFull` is the core test. With no worker running, it fires
`capacity+extra` `TryCall`s concurrently; exactly `capacity` are admitted (and
block waiting for a reply), and the rest return `ErrBusy`. It waits until the
expected number have been shed, asserts `Pending == Capacity`, then starts the
worker so the admitted calls drain and succeed. `TestServesWhenSpace` is the happy
path: a `TryCall` with room returns the doubled value. `TestExample` documents the
gauges.

Create `shedding_test.go`:

```go
package shedding

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestShedsWhenFull(t *testing.T) {
	t.Parallel()
	const capacity = 3
	const callers = 8
	s := New(capacity) // worker not started yet

	var admitted, shed atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.TryCall(21)
			if errors.Is(err, ErrBusy) {
				shed.Add(1)
			} else if err == nil {
				admitted.Add(1)
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	// With no consumer, the buffer fills to capacity and the rest are shed.
	wantShed := int64(callers - capacity)
	for shed.Load() < wantShed {
		runtime.Gosched()
	}

	if got := s.Pending(); got != capacity {
		t.Fatalf("Pending() = %d, want %d", got, capacity)
	}
	if got := s.Capacity(); got != capacity {
		t.Fatalf("Capacity() = %d, want %d", got, capacity)
	}

	go s.Run() // now drain the admitted calls
	wg.Wait()

	if admitted.Load() != capacity {
		t.Fatalf("admitted = %d, want %d", admitted.Load(), capacity)
	}
	if shed.Load() != wantShed {
		t.Fatalf("shed = %d, want %d", shed.Load(), wantShed)
	}
	s.Shutdown()
}

func TestServesWhenSpace(t *testing.T) {
	t.Parallel()
	s := New(4)
	go s.Run()
	defer s.Shutdown()

	got, err := s.TryCall(21)
	if err != nil {
		t.Fatalf("TryCall error = %v", err)
	}
	if got != 42 {
		t.Fatalf("TryCall = %d, want 42", got)
	}
}

func ExampleService_Capacity() {
	s := New(2)
	fmt.Println(s.Capacity(), s.Pending())
	// Output: 2 0
}
```

## Review

The service is correct when it sheds load at exactly the right boundary: with the
worker paused, `capacity` calls are admitted and every further call returns
`ErrBusy` immediately. `TestShedsWhenFull` proves the count is deterministic — the
buffered channel fills to `cap` and no more, so `admitted == capacity` and
`shed == callers - capacity` regardless of scheduling. That determinism is the
whole value of a bounded inbox: overload has a defined limit and a defined
response.

The mistakes to avoid: do not confuse `TryCall`'s non-blocking *admission* with a
non-blocking *call* — the reply wait still blocks (with a `quit` escape), because
you asked to be served, you just refused to queue. Do not busy-wait on `Pending`
in production; the spin loops here exist only to make the test observe the full
buffer deterministically. And remember that an unbounded inbox is not resilience:
buffering without a cap just delays the failure into OOM. Run `go test -race`; the
admission select and the `len`/`cap` reads are race-free.

## Resources

- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the `default` clause that makes a send non-blocking.
- [Go spec: Length and capacity](https://go.dev/ref/spec#Length_and_capacity) — `len` and `cap` on channels.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels as bounded queues.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — why shedding load beats unbounded queueing.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-competing-consumer-pool.md](07-competing-consumer-pool.md)
