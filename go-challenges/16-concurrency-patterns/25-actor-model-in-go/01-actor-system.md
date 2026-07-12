# Exercise 1: A Reusable Actor System

This exercise builds the reusable substrate the rest of the chapter rests on: a
small actor system that spawns named actors, routes messages to them by name,
recovers per-message panics, and shuts an actor down cleanly without leaking its
goroutine. It is a complete, standalone Go module.

## What you'll build

```text
actor.go              Message, PoisonPill, Handler, System
                      NewSystem, (*System).Spawn, Send, Stop
cmd/
  demo/
    main.go           spawn an actor, sum 1..10 through its mailbox
actor_test.go         ordered delivery, panic recovery, unknown-actor error,
                      Stop drains the mailbox
```

- Files: `actor.go`, `cmd/demo/main.go`, `actor_test.go`.
- Implement: `Message`, `PoisonPill`, `Handler`, `System`, `NewSystem`, and the
  methods `Spawn`, `Send`, `Stop`.
- Test: `actor_test.go` asserts in-order delivery from one sender, that a
  panicking handler does not kill the actor, that `Send` to an unknown name
  returns an error, and that `Stop` processes every queued message first.
- Verify: `go test -race ./...`

### The design, one piece at a time

An actor is three fields: a `name`, a `mailbox` channel that carries work, and a
`done` channel that the actor closes as it exits so a caller can wait for it. The
goroutine started in `newActor` runs `run`, whose whole body is a `for range`
over the mailbox. Ranging a channel is the natural message loop: it blocks for
the next message, hands it to the handler, and ends the moment the channel is
closed and drained. That last property is what makes shutdown clean — closing
the mailbox is not an abrupt kill, it lets the loop finish every buffered message
and then fall out of the range.

Two ways to stop an actor appear here, and they are complementary. Closing the
mailbox is the blunt instrument used by `Stop` only after the actor is
unreachable. The `PoisonPill` is a sentinel value that rides the mailbox like any
other message; when `run` type-asserts a message to `PoisonPill` it returns
immediately, leaving any messages queued *after* the pill unprocessed. Because a
channel is FIFO, everything sent *before* the pill has already been handled by
the time the pill is pulled off — so the pill is a graceful "stop after the work
I have already accepted" marker, not a kill switch. This exercise's `Stop` sends
the pill and then waits on `done`, giving a synchronous "stopped and drained"
guarantee to the caller.

Panic recovery is per message and lives in `deliver`, a tiny helper whose only
job is to wrap one handler call in a `defer`/`recover`. Putting the recover in
its own function, rather than inline in the loop, means the `defer` runs at the
end of each *message* rather than at the end of the whole loop, so a panic on one
message is contained and the very next message is still processed. The recovered
actor keeps its state and keeps running; only the offending message is abandoned.

`System` is the registry. It is the one place a mutex is justified, because the
map from names to actors is genuinely shared: `Spawn` and `Stop` write it while
`Send` reads it. A `sync.RWMutex` lets many concurrent `Send` lookups proceed in
parallel and only serializes the rarer mutations. Note the ownership rule that
keeps shutdown panic-free: `Stop` deletes the actor from the map *before* it
sends the pill, so any `Send` that loses the race finds nothing under that name
and returns an error instead of writing to a mailbox whose actor is leaving.

Create `actor.go`:

```go
package actor

import (
	"fmt"
	"sync"
)

// Message is the type carried by every actor mailbox. Handlers type-assert it
// to the concrete types they expect.
type Message any

// PoisonPill is a sentinel message that tells an actor to stop after it has
// processed every message that was enqueued ahead of the pill.
type PoisonPill struct{}

// Handler processes one message. It is called by exactly one goroutine (the
// actor's), one message at a time, so it needs no internal synchronization.
type Handler func(msg Message)

// actor is the private goroutine state for one named actor.
type actor struct {
	name    string
	mailbox chan Message
	done    chan struct{}
}

func newActor(name string, h Handler, capacity int) *actor {
	a := &actor{
		name:    name,
		mailbox: make(chan Message, capacity),
		done:    make(chan struct{}),
	}
	go a.run(h)
	return a
}

// run is the message loop. It ends when it receives a PoisonPill or when the
// mailbox is closed and drained; either way it closes done on the way out so a
// caller blocked in Stop is released.
func (a *actor) run(h Handler) {
	defer close(a.done)
	for msg := range a.mailbox {
		if _, ok := msg.(PoisonPill); ok {
			return
		}
		a.deliver(h, msg)
	}
}

// deliver calls the handler for one message, recovering a panic so that a
// single bad message neither kills the actor nor stops later messages.
func (a *actor) deliver(h Handler, msg Message) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("actor %s: recovered panic: %v\n", a.name, r)
		}
	}()
	h(msg)
}

// System is a registry of named actors. The map is the only shared state, so it
// is the only thing guarded by a lock.
type System struct {
	mu     sync.RWMutex
	actors map[string]*actor
}

// NewSystem returns an empty actor system.
func NewSystem() *System {
	return &System{actors: make(map[string]*actor)}
}

// Spawn starts a new actor under name with the given handler. The optional
// argument sets the mailbox capacity (default 64). It panics if name is already
// registered, since silently replacing a live actor would orphan its goroutine.
func (s *System) Spawn(name string, h Handler, capacity ...int) {
	c := 64
	if len(capacity) > 0 {
		c = capacity[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.actors[name]; exists {
		panic("actor already registered: " + name)
	}
	s.actors[name] = newActor(name, h, c)
}

// Send enqueues msg in the named actor's mailbox, blocking if the mailbox is
// full. It returns an error if no actor is registered under name.
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

// Stop unregisters the named actor, sends it a PoisonPill, and waits for it to
// finish processing every message queued ahead of the pill. Removing the actor
// from the registry first guarantees no later Send can reach its mailbox.
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

### The runnable demo

The demo spawns one actor that adds every integer it receives into a private
accumulator, sends it 1 through 10, then stops it. Because `Stop` drains the
mailbox before returning, the sum is final by the time it prints — no sleep, no
polling. The accumulator is a plain `int64` updated only inside the handler, so
there is nothing to synchronize.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/actor-system"
)

func main() {
	sys := actor.NewSystem()

	var total int64
	sys.Spawn("adder", func(msg actor.Message) {
		if v, ok := msg.(int); ok {
			total += int64(v)
		}
	})

	for i := 1; i <= 10; i++ {
		_ = sys.Send("adder", i)
	}
	sys.Stop("adder")

	fmt.Printf("sum of 1..10 = %d\n", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sum of 1..10 = 55
```

### Tests

The tests pin the four behaviors that define a correct actor system. Ordered
delivery proves the FIFO guarantee from a single sender. Panic recovery proves
that a handler that panics on one message still processes the next. The
unknown-actor test proves `Send` reports a missing name rather than panicking.
The drain test proves `Stop` is synchronous over the whole backlog: twenty slow
messages are all processed before `Stop` returns. Every test runs under `-race`,
which is what certifies that no actor state is touched from outside its handler.

Create `actor_test.go`:

```go
package actor_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/actor-system"
)

// TestOrderedDelivery checks that messages from a single sender are handled in
// send order and that Stop waits for all of them.
func TestOrderedDelivery(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()

	var mu sync.Mutex
	var got []int
	sys.Spawn("counter", func(msg actor.Message) {
		v := msg.(int)
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
	})

	for i := 0; i < 10; i++ {
		_ = sys.Send("counter", i)
	}
	sys.Stop("counter")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 10 {
		t.Fatalf("got %d messages, want 10", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("message[%d] = %d, want %d", i, v, i)
		}
	}
}

// TestPanicRecovery checks that a panicking handler neither kills the actor nor
// drops the messages that follow the bad one.
func TestPanicRecovery(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()
	var count atomic.Int64
	sys.Spawn("panicker", func(msg actor.Message) {
		if msg == "panic" {
			panic("intentional")
		}
		count.Add(1)
	})

	_ = sys.Send("panicker", "panic")
	_ = sys.Send("panicker", "ok")
	_ = sys.Send("panicker", "ok")
	sys.Stop("panicker")

	if got := count.Load(); got != 2 {
		t.Fatalf("processed %d good messages, want 2", got)
	}
}

// TestSendUnknownActor checks that Send reports a missing actor as an error
// rather than panicking.
func TestSendUnknownActor(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()
	if err := sys.Send("nope", "hi"); err == nil {
		t.Fatal("expected error for unknown actor, got nil")
	}
}

// TestStopDrains checks that Stop returns only after every queued message has
// been processed, even when each one is slow.
func TestStopDrains(t *testing.T) {
	t.Parallel()

	sys := actor.NewSystem()
	var count atomic.Int64
	sys.Spawn("drain", func(msg actor.Message) {
		time.Sleep(time.Millisecond)
		count.Add(1)
	})

	const n = 20
	for i := 0; i < n; i++ {
		_ = sys.Send("drain", i)
	}
	sys.Stop("drain")

	if got := count.Load(); got != n {
		t.Fatalf("drained %d messages, want %d", got, n)
	}
}

func ExampleSystem_Send() {
	sys := actor.NewSystem()
	done := make(chan struct{})
	sys.Spawn("echo", func(msg actor.Message) {
		if _, ok := msg.(struct{}); ok {
			close(done)
			return
		}
		fmt.Println("got:", msg)
	})

	_ = sys.Send("echo", "hello")
	_ = sys.Send("echo", struct{}{})
	<-done
	sys.Stop("echo")
	// Output: got: hello
}
```

## Review

The system is correct when delivery is ordered per sender, a panic is contained
to its message, a `Send` to a missing name returns an error, and `Stop` is
synchronous over the entire backlog. The `-race` run is the real proof of the
central claim: it passes only because every actor's state is mutated solely
inside its handler goroutine, so there is no unsynchronized sharing to detect.

The mistakes to avoid here are structural. Putting the `recover` at the top of
`run` instead of inside the per-message `deliver` would catch only the first
panic and then end the loop, killing the actor on the first bad message. Sending
the `PoisonPill` before deleting the actor from the registry would open a window
where a concurrent `Send` writes to a mailbox whose actor is already leaving.
Closing the mailbox from `Stop` while other goroutines might still `Send` would
panic on a closed channel — which is exactly why this design uses a pill plus a
registry removal rather than a raw close. And reading the accumulator from
outside the handler (for example to print it before `Stop` returns) would be the
data race the whole model exists to prevent.

## Resources

- [Actor model (Wikipedia)](https://en.wikipedia.org/wiki/Actor_model) — the conceptual origin and the message-passing definition this module implements.
- [Share Memory By Communicating (Go blog)](https://go.dev/blog/codelab-share) — the Go-idiomatic case for channels over shared state, the principle behind one-goroutine-per-actor.
- [The Go Memory Model](https://go.dev/ref/mem) — what `-race` enforces and why single-goroutine ownership of state is race-free by construction.
- [sync package](https://pkg.go.dev/sync) — `sync.RWMutex`, used for the one genuinely shared structure, the registry.

---

Next: [02-stateful-account-actor.md](02-stateful-account-actor.md)
</content>
