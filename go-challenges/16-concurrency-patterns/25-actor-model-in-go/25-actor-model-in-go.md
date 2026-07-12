# 25. Actor Model in Go

The actor model is a concurrency model where the unit of computation is an
*actor*: an entity with private state, a mailbox (message queue), and a
behaviour function that processes one message at a time. Actors communicate
exclusively by sending messages to each other's mailboxes. There is no shared
memory and therefore no need for mutexes. Go's goroutines and channels map
naturally onto this model: a goroutine per actor, a buffered channel per
mailbox.

```text
actor/
  go.mod
  internal/actor/actor.go
  internal/actor/actor_test.go
  cmd/demo/main.go
```

## Concepts

### Actors Eliminate Shared-State Bugs by Design

When multiple goroutines share a variable, every access must be protected by a
mutex or atomic operation, or a race condition occurs. The actor model removes
the shared variable: each actor owns its state and no other goroutine touches
it. Mutation happens only inside the actor's message handler. Because message
processing is sequential (one message at a time from the channel), the handler
itself needs no synchronisation primitives.

### Mailbox as a Buffered Channel

A mailbox is `make(chan Message, cap)`. The capacity controls the maximum number
of unprocessed messages before `Send` blocks. A capacity of 0 (unbuffered) means
`Send` blocks until the actor is ready to receive, which couples sender and
receiver tightly. A large capacity allows the sender to continue without waiting
but risks unbounded memory growth. Choose capacity based on the expected burst
size and acceptable latency.

### Sequential Processing Guarantees Ordering

Because an actor processes messages one at a time from its channel, messages
sent by a single sender are processed in the order they were sent (FIFO channel
semantics). Messages from different senders may interleave in any order, just
as goroutine scheduling is non-deterministic.

### Panic Recovery and Failure Isolation

A panic inside a message handler kills the actor's goroutine if unrecovered. The
pattern is to `defer` a recover inside the message loop:

```
defer func() {
    if r := recover(); r != nil {
        // report failure; optionally restart
    }
}()
```

This isolates the failure: the panicking actor dies but other actors continue.
The actor system can detect the failure via an error channel and decide to
restart the actor or escalate.

### PoisonPill for Graceful Shutdown

Sending a sentinel value (`PoisonPill`) to an actor's mailbox tells it to drain
remaining messages and then stop. The actor processes all pending messages
before the `PoisonPill` reaches it because channel FIFO ordering guarantees
that earlier sends arrive first. After processing the `PoisonPill`, the actor
closes its mailbox, signalling to the system that it has stopped.

### System as Registry

An `ActorSystem` maps names to actors. `Spawn` creates a new actor and
registers it; `Send` looks up the actor by name and enqueues the message;
`Stop` sends a `PoisonPill` and waits for the actor to finish. The registry
needs a `sync.RWMutex` because `Spawn` and `Stop` write while `Send` reads.

## Exercises

### Exercise 1: Actor and System Types

Create `internal/actor/actor.go`:

```go
package actor

import (
	"fmt"
	"sync"
)

// Message is the type carried by every actor mailbox.
type Message any

// PoisonPill is a sentinel message that tells an actor to stop after
// processing all messages currently ahead of it in the mailbox.
type PoisonPill struct{}

// Handler is the function an actor calls for each received message.
type Handler func(msg Message)

// actor is an internal type that holds the goroutine state.
type actor struct {
	name    string
	mailbox chan Message
	done    chan struct{}
}

func newActor(name string, h Handler, cap int) *actor {
	a := &actor{
		name:    name,
		mailbox: make(chan Message, cap),
		done:    make(chan struct{}),
	}
	go a.run(h)
	return a
}

func (a *actor) run(h Handler) {
	defer close(a.done)
	for msg := range a.mailbox {
		if _, ok := msg.(PoisonPill); ok {
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("actor %s: recovered panic: %v\n", a.name, r)
				}
			}()
			h(msg)
		}()
	}
}

// System manages a set of named actors.
type System struct {
	mu     sync.RWMutex
	actors map[string]*actor
}

// NewSystem creates an empty actor system.
func NewSystem() *System {
	return &System{actors: make(map[string]*actor)}
}

// Spawn creates a new actor with the given name, handler, and mailbox capacity.
// It panics if name is already registered.
func (s *System) Spawn(name string, h Handler, opts ...int) {
	cap := 64
	if len(opts) > 0 {
		cap = opts[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.actors[name]; exists {
		panic("actor already registered: " + name)
	}
	s.actors[name] = newActor(name, h, cap)
}

// Send enqueues msg in the named actor's mailbox. It returns an error if the
// actor does not exist.
func (s *System) Send(name string, msg Message) error {
	s.mu.RLock()
	a, ok := s.actors[name]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("actor not found: %s", name)
	}
	a.mailbox <- msg
	return nil
}

// Stop sends a PoisonPill to the named actor and waits for it to finish
// processing all pending messages. It removes the actor from the registry.
func (s *System) Stop(name string) {
	s.mu.Lock()
	a, ok := s.actors[name]
	if ok {
		delete(s.actors, name)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	a.mailbox <- PoisonPill{}
	<-a.done
}
```

### Exercise 2: Tests

Create `internal/actor/actor_test.go`:

```go
package actor_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/actor/internal/actor"
)

var errNotFound = errors.New("actor not found")

// TestMessageDelivery verifies that messages are delivered to the handler in
// order and all messages arrive before Stop returns.
func TestMessageDelivery(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()

	var mu sync.Mutex
	var received []int

	sys.Spawn("counter", func(msg actor.Message) {
		v, ok := msg.(int)
		if !ok {
			return
		}
		mu.Lock()
		received = append(received, v)
		mu.Unlock()
	})

	for i := 0; i < 10; i++ {
		sys.Send("counter", i) //nolint:errcheck
	}
	sys.Stop("counter")

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 10 {
		t.Fatalf("received %d messages, want 10", len(received))
	}
	for i, v := range received {
		if v != i {
			t.Fatalf("message[%d] = %d, want %d", i, v, i)
		}
	}
}

// TestPanicRecovery verifies that a panicking handler does not crash the actor
// or the system, and subsequent messages are still processed.
func TestPanicRecovery(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()
	var count atomic.Int64

	sys.Spawn("panicker", func(msg actor.Message) {
		if msg == "panic" {
			panic("intentional panic")
		}
		count.Add(1)
	})

	sys.Send("panicker", "panic")  //nolint:errcheck
	sys.Send("panicker", "normal") //nolint:errcheck
	sys.Send("panicker", "normal") //nolint:errcheck
	sys.Stop("panicker")

	if got := count.Load(); got != 2 {
		t.Fatalf("processed %d normal messages, want 2", got)
	}
}

// TestSendToUnknownActor verifies that Send returns an error for unknown names.
func TestSendToUnknownActor(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()
	err := sys.Send("nonexistent", "hello")
	if err == nil {
		t.Fatal("expected error for unknown actor, got nil")
	}
}

// TestStopDrainsMailbox verifies that Stop waits for all queued messages to be
// processed before returning.
func TestStopDrainsMailbox(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()
	var count atomic.Int64

	sys.Spawn("drain", func(msg actor.Message) {
		time.Sleep(time.Millisecond)
		count.Add(1)
	})

	const n = 20
	for i := 0; i < n; i++ {
		sys.Send("drain", i) //nolint:errcheck
	}
	sys.Stop("drain")

	if got := count.Load(); got != n {
		t.Fatalf("drained %d messages, want %d", got, n)
	}
}

func ExampleSystem_Spawn() {
	sys := actor.NewSystem()
	done := make(chan struct{})

	sys.Spawn("a", func(msg actor.Message) {
		if _, ok := msg.(struct{}); ok {
			close(done)
			return
		}
		fmt.Println("received:", msg)
	})

	sys.Send("a", "hello")    //nolint:errcheck
	sys.Send("a", struct{}{}) //nolint:errcheck
	<-done
	sys.Stop("a")
	// Output: received: hello
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/actor/internal/actor"
)

func main() {
	sys := actor.NewSystem()
	var total atomic.Int64

	sys.Spawn("adder", func(msg actor.Message) {
		if v, ok := msg.(int); ok {
			total.Add(int64(v))
		}
	})

	for i := 1; i <= 10; i++ {
		sys.Send("adder", i) //nolint:errcheck
	}
	sys.Stop("adder")

	fmt.Printf("sum of 1..10 = %d\n", total.Load())
}
```

## Common Mistakes

### Accessing Actor State From Outside the Handler

Wrong: reading or writing an actor's state variable from a goroutine that is not
the actor's message handler.

What happens: a data race detected by `-race` or, worse, a silent corruption
that is not detected.

Fix: all state reads and writes must happen inside the handler. If the caller
needs a result, send a reply message to a reply-to channel or actor.

### Sending on a Stopped Actor's Mailbox

Wrong: calling `Send` after `Stop` returns, or closing the mailbox from outside
the actor.

What happens: a send on a closed channel panics, or the message is enqueued but
the handler goroutine has already exited and will never read it.

Fix: the `System` removes the actor from its registry in `Stop` before closing
the mailbox. Callers that call `Send` after `Stop` get an error rather than a
panic.

### Blocking the Handler With a Long Computation

Wrong: performing a slow database query or network call inside the message
handler without concurrency.

What happens: the actor's mailbox fills up and senders block. One slow operation
blocks all subsequent messages for that actor.

Fix: spawn a separate goroutine from inside the handler for the slow work, then
send the result back via a reply message. The actor continues processing other
messages while the work is in progress.

### Unbounded Mailbox Capacity

Wrong: `make(chan Message)` (unbounded via large buffer) so that `Send` never
blocks.

What happens: if the producer is faster than the consumer, messages accumulate
indefinitely and the process runs out of memory.

Fix: choose a bounded capacity that reflects the maximum acceptable backlog.
When the mailbox is full, `Send` blocks, propagating backpressure to the sender.
Alternatively, log and drop the message if blocking is unacceptable.

## Verification

From `~/go-exercises/actor`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit 0. The race detector confirms that actor state is
never accessed from outside the handler goroutine.

## Summary

- An actor is a goroutine with a buffered channel (mailbox) and a sequential
  message handler; it owns its state exclusively.
- `System` provides Spawn, Send, and Stop to manage actors by name.
- `PoisonPill` drains the mailbox and then terminates the actor goroutine.
- Panics inside handlers are recovered per-message; the actor continues
  processing subsequent messages.
- Actors eliminate shared-state races by construction: no variable is touched
  by more than one goroutine.

## What's Next

Next: [CSP vs Actor Model](../26-csp-vs-actor/26-csp-vs-actor.md).

## Resources

- [Actor model (Wikipedia)](https://en.wikipedia.org/wiki/Actor_model) - conceptual foundation and history
- [Rob Pike: Concurrency is not Parallelism](https://go.dev/talks/2012/waza.slide) - goroutines as communicating processes
- [Proto.Actor for Go](https://github.com/asynkron/protoactor-go) - a production actor framework in Go
- [Erlang/OTP supervision trees](https://www.erlang.org/doc/design_principles/sup_princ) - the gold standard for actor lifecycle management
- [Go sync package](https://pkg.go.dev/sync) - primitives used in the registry implementation
