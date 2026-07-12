# Exercise 1: A Serialized Request-Reply Service (Sequence Allocator)

Almost every backend needs a source of unique, monotonic identifiers: a request
sequence number, a per-connection message ID, an in-process offset. This exercise
builds one as a serialized request-reply service — an actor that owns a counter
and hands out the next value on each call, with no mutex anywhere, and proves
under `-race` that a hundred concurrent callers still get unique, gap-free IDs.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
allocator/                 independent module: example.com/allocator
  go.mod
  allocator.go             type Service; New, Run, Shutdown, Call; ErrShuttingDown
  cmd/
    demo/
      main.go              runnable demo: allocate a few IDs, then shut down
  allocator_test.go        sequential, post-shutdown, and 100-way concurrent tests
```

- Files: `allocator.go`, `cmd/demo/main.go`, `allocator_test.go`.
- Implement: a `Service` whose single `Run` goroutine owns a `uint64` counter and answers each `Call` with the next ID; `New`, `Shutdown` (signal quit, join on done), and a buffered per-request reply channel.
- Test: sequential `Call` returns a strictly increasing sequence; `Call` after `Shutdown` returns `ErrShuttingDown` (asserted with `errors.Is`); 100 concurrent `Call`s return a gap-free set of size 100 under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/09-channel-of-channels/01-serialized-actor-service/cmd/demo
cd go-solutions/13-goroutines-and-channels/09-channel-of-channels/01-serialized-actor-service
```

### The actor that owns the counter

The counter is an ordinary `uint64` local variable inside `Run`. It is never a
struct field, never guarded by a lock, because exactly one goroutine — the one
running `Run` — ever reads or writes it. Each `Call` sends a `Request` carrying a
private `reply` channel; `Run` receives the request, increments the counter,
and sends the new value back on that reply channel. Because increments happen in a
single goroutine processing one request at a time, the sequence is strictly
increasing and gap-free by construction, even when a thousand callers race to
call `Call`. That is the actor guarantee, and `go test -race` is what proves the
absence of a data race: only one goroutine touches `counter`.

Three channels wire the lifecycle. `requests` is the inbox the service owns and
callers send to. `quit` is the shutdown signal; closing it tells `Run` to return.
`done` is the join handle; `Run` closes it (via `defer`) on the way out, so
`Shutdown` can wait for the goroutine to have *actually* finished rather than
merely having been told to. Signalling without joining is the classic
half-shutdown that leaks the goroutine; the `done` channel is what makes the join
possible.

`Call` creates its reply channel with capacity one. That single slot is what lets
the worker's send always complete: even if a future caller had abandoned the
receive, the value drops into the buffer and the worker moves on. Here `Call`
always receives its own reply, but the buffered channel is the invariant the rest
of this lesson builds on, so it is correct from the first exercise. `Call` also
selects its send against `quit`, so a caller that arrives after shutdown returns
`ErrShuttingDown` instead of blocking forever on a service that will never
receive.

Create `allocator.go`:

```go
package allocator

import "errors"

// ErrShuttingDown is returned by Call once the service has been shut down.
var ErrShuttingDown = errors.New("allocator: shutting down")

// Request carries a private reply channel; the service answers on it.
type Request struct {
	reply chan Response
}

// Response is the allocated ID or an error.
type Response struct {
	ID  uint64
	Err error
}

// Service is a serialized ID allocator. A single Run goroutine owns the counter,
// so no mutex is needed to keep allocations unique and gap-free.
type Service struct {
	requests chan Request
	quit     chan struct{}
	done     chan struct{}
}

// New returns a Service ready to be started with Run.
func New() *Service {
	return &Service{
		requests: make(chan Request),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Run is the actor loop: it owns the counter and answers one request at a time.
// Start it in its own goroutine: go s.Run().
func (s *Service) Run() {
	defer close(s.done)
	var counter uint64
	for {
		select {
		case req := <-s.requests:
			counter++
			req.reply <- Response{ID: counter}
		case <-s.quit:
			return
		}
	}
}

// Shutdown signals the loop to stop and waits for it to exit. It is safe to call
// once; a second call panics on the double close, matching stdlib conventions.
func (s *Service) Shutdown() {
	close(s.quit)
	<-s.done
}

// Call returns the next allocated ID, or ErrShuttingDown if the service stopped.
func (s *Service) Call() (uint64, error) {
	reply := make(chan Response, 1)
	select {
	case s.requests <- Request{reply: reply}:
	case <-s.quit:
		return 0, ErrShuttingDown
	}
	resp := <-reply
	return resp.ID, resp.Err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/allocator"
)

func main() {
	s := allocator.New()
	go s.Run()
	defer s.Shutdown()

	for range 3 {
		id, err := s.Call()
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Printf("allocated id %d\n", id)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
allocated id 1
allocated id 2
allocated id 3
```

### Tests

`TestCallSequential` proves the allocator hands out `1, 2, 3, ...` in order.
`TestCallReturnsErrorAfterShutdown` proves a call after shutdown reports
`ErrShuttingDown` via `errors.Is`. `TestCallConcurrent` is the contract that
matters: 100 goroutines each call once, and the collected IDs must form the exact
set `{1..100}` — unique and gap-free. Run under `-race` it also proves the counter
has no data race despite the total absence of a mutex, because only the `Run`
goroutine ever touches it.

Create `allocator_test.go`:

```go
package allocator

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestCallSequential(t *testing.T) {
	t.Parallel()
	s := New()
	go s.Run()
	defer s.Shutdown()

	for want := uint64(1); want <= 5; want++ {
		got, err := s.Call()
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		if got != want {
			t.Fatalf("Call() = %d, want %d", got, want)
		}
	}
}

func TestCallReturnsErrorAfterShutdown(t *testing.T) {
	t.Parallel()
	s := New()
	go s.Run()
	s.Shutdown()

	_, err := s.Call()
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Call() error = %v, want ErrShuttingDown", err)
	}
}

func TestCallConcurrent(t *testing.T) {
	t.Parallel()
	const n = 100
	s := New()
	go s.Run()
	defer s.Shutdown()

	ids := make([]uint64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Call()
			if err != nil {
				t.Errorf("Call() error = %v", err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	for want := uint64(1); want <= n; want++ {
		if !seen[want] {
			t.Fatalf("missing id %d: sequence has a gap", want)
		}
	}
}

func ExampleService_Call() {
	s := New()
	go s.Run()
	defer s.Shutdown()

	first, _ := s.Call()
	second, _ := s.Call()
	fmt.Println(first, second)
	// Output: 1 2
}
```

## Review

The allocator is correct when the counter is a pure function of how many requests
`Run` has processed: the k-th request receives ID k, with no gaps and no
duplicates, regardless of how many callers race. `TestCallConcurrent` encodes that
as a set equality, and running it under `-race` is what proves the design — not a
mutex — is what keeps the counter consistent. If that test ever reports a
duplicate or a gap, some code path is touching the counter from outside the `Run`
goroutine.

The mistakes to avoid are structural. Do not add a `sync.Mutex` around the
counter "to be safe" — the single-goroutine ownership is the whole point, and a
mutex here is dead weight that also invites someone to read the counter from
another goroutine. Do not make `Shutdown` close `quit` without the `<-done` join;
without it the `Run` goroutine may outlive `Shutdown` and leak. And keep the reply
channel buffered at capacity one even though this exercise always receives its own
reply — the later exercises rely on that invariant to survive caller timeouts.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — channels as first-class values, including channels of channels.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the request-with-reply-channel idiom.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.
- [Go Memory Model](https://go.dev/ref/mem) — why single-goroutine ownership removes the data race.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-chan-chan-worker-dispatcher.md](02-chan-chan-worker-dispatcher.md)
