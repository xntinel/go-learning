# Exercise 2: Session Lifecycle Through One Owning Goroutine

A connection or session has a lifecycle — Idle, Connecting, Connected, Closed — that many goroutines want to drive and observe at once: one goroutine dials, a heartbeat goroutine pings, a supervisor asks "are we connected?". The senior way to keep that race-free is not a mutex around a `state` field but a single goroutine that *owns* the state, with every event and every status read delivered to it as a message. This exercise builds that owner, including the move that makes reads safe: a status query is itself a command with a reply channel.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
session.go           Session, State, EventType, Stats, New, Do, State, Stats, Done (owning goroutine)
cmd/
  demo/
    main.go          dial, reject a too-early ping, establish, 100 concurrent pings, close
session_test.go      rejection, happy path, concurrent pings under -race, terminal Close, ctx cancel
```

- Files: `session.go`, `cmd/demo/main.go`, `session_test.go`.
- Implement: `New(ctx)` starts an owning goroutine; `Do(event)` submits an event and returns the resulting state; `State()` and `Stats()` are queries answered by the owner; `Done()` reports termination.
- Test: a too-early ping is rejected, the happy path reaches Connected then Closed, hundreds of concurrent `Do`/`State` calls are race-free with an exact ping count, Close is terminal, and a cancelled context stops the owner.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/22-channel-based-state-machine/02-session-lifecycle-owning-goroutine/cmd/demo && cd go-solutions/16-concurrency-patterns/22-channel-based-state-machine/02-session-lifecycle-owning-goroutine
```

### Why the state must not live in the struct

The tempting design puts `state State` and a `sync.Mutex` on the `Session` struct. It works until a supervisor calls `State()` while the dialer calls a transition: now two goroutines touch the same field and you are one missed lock away from a data race. Even with the lock correct, a status read contends on the same mutex as every transition. The owning-goroutine design sidesteps both problems by making the state physically unreachable from any goroutine but one.

The `Session` struct holds *no state at all* — only channels:

```text
type Session struct {
	cmds chan command  // every event and every query travels here
	done chan struct{} // closed when the owner exits
}
```

The current state and the counters are local variables inside the owner's `for-select` loop. Because they live on that goroutine's stack and are never published, the race detector has nothing to flag no matter how many goroutines call in. A caller communicates by sending a `command`; when it wants an answer it includes a reply channel the owner writes back on:

```text
type command struct {
	event EventType    // "" means a pure query: report state/stats, do not transition
	reply chan snapshot
}
```

This is the key idea: a read is a message. `State()` sends a command with an empty event and a fresh reply channel, then blocks on that channel. The owner answers from inside the same loop that processes transitions, so a status read is serialised with every state change and can never observe a half-applied transition.

The transition rule itself is an ordinary function — Idle dials to Connecting, Connecting either establishes to Connected or fails back to Idle, Connected pings itself or times out back to Idle, and Close from anywhere is terminal. It returns the next state and an `ok` flag; `ok == false` means the event was illegal in that state and the owner records a rejection instead of moving. An illegal event is normal input (a ping that arrives before the link is up), not a panic.

Two things keep callers from ever blocking forever. The command channel is unbuffered, so a send rendezvous with the owner's receive; if the owner has exited, the send cannot complete and the caller's `select` falls through to `done`. And the owner, on reaching `Closed`, replies first, then returns — its `defer close(done)` then unblocks every caller still waiting, each of which reports `Closed`.

Create `session.go`:

```go
package session

import "context"

// State names a position in the session lifecycle.
type State string

const (
	StateIdle       State = "Idle"
	StateConnecting State = "Connecting"
	StateConnected  State = "Connected"
	StateClosed     State = "Closed"
)

// EventType names a transition trigger.
type EventType string

const (
	EventDial        EventType = "Dial"
	EventEstablished EventType = "Established"
	EventFailed      EventType = "Failed"
	EventPing        EventType = "Ping"
	EventTimeout     EventType = "Timeout"
	EventClose       EventType = "Close"
)

// Stats is a snapshot of the owner's internal counters.
type Stats struct {
	Dials       int
	Established int
	Failures    int
	Pings       int
	Rejected    int
}

func (s *Stats) count(e EventType) {
	switch e {
	case EventDial:
		s.Dials++
	case EventEstablished:
		s.Established++
	case EventFailed:
		s.Failures++
	case EventPing:
		s.Pings++
	}
}

// snapshot is what the owner sends back on a reply channel.
type snapshot struct {
	State State
	Stats Stats
}

// command is one message to the owning goroutine. An empty event is a pure
// query; reply is nil for fire-and-forget and non-nil to receive a snapshot.
type command struct {
	event EventType
	reply chan snapshot
}

// Session is a handle to a session state machine. It holds no mutable state of
// its own: the state and counters live on the owning goroutine's stack.
type Session struct {
	cmds chan command
	done chan struct{}
}

// New starts the owning goroutine and returns a handle. The goroutine exits when
// the session reaches Closed or when ctx is cancelled.
func New(ctx context.Context) *Session {
	s := &Session{
		cmds: make(chan command),
		done: make(chan struct{}),
	}
	go s.run(ctx)
	return s
}

// transition returns the next state for an event and whether the event was legal.
func transition(state State, e EventType) (State, bool) {
	switch state {
	case StateIdle:
		switch e {
		case EventDial:
			return StateConnecting, true
		case EventClose:
			return StateClosed, true
		}
	case StateConnecting:
		switch e {
		case EventEstablished:
			return StateConnected, true
		case EventFailed:
			return StateIdle, true
		case EventClose:
			return StateClosed, true
		}
	case StateConnected:
		switch e {
		case EventPing:
			return StateConnected, true
		case EventTimeout:
			return StateIdle, true
		case EventClose:
			return StateClosed, true
		}
	case StateClosed:
		// terminal: every event is illegal
	}
	return state, false
}

func (s *Session) run(ctx context.Context) {
	defer close(s.done)
	state := StateIdle
	var stats Stats
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-s.cmds:
			if cmd.event != "" {
				if next, ok := transition(state, cmd.event); ok {
					stats.count(cmd.event)
					state = next
				} else {
					stats.Rejected++
				}
			}
			if cmd.reply != nil {
				cmd.reply <- snapshot{State: state, Stats: stats}
			}
			if state == StateClosed {
				return
			}
		}
	}
}

// send delivers cmd to the owner and, if cmd.reply is set, waits for the answer.
// If the owner has already exited, it reports a Closed snapshot.
func (s *Session) send(cmd command) snapshot {
	select {
	case s.cmds <- cmd:
	case <-s.done:
		return snapshot{State: StateClosed}
	}
	if cmd.reply == nil {
		return snapshot{}
	}
	select {
	case snap := <-cmd.reply:
		return snap
	case <-s.done:
		return snapshot{State: StateClosed}
	}
}

// Do submits an event and returns the resulting state.
func (s *Session) Do(e EventType) State {
	return s.send(command{event: e, reply: make(chan snapshot, 1)}).State
}

// State returns the current state without causing a transition.
func (s *Session) State() State {
	return s.send(command{reply: make(chan snapshot, 1)}).State
}

// Stats returns a snapshot of the owner's counters.
func (s *Session) Stats() Stats {
	return s.send(command{reply: make(chan snapshot, 1)}).Stats
}

// Done is closed when the owning goroutine has exited.
func (s *Session) Done() <-chan struct{} {
	return s.done
}
```

The reply channel is buffered with capacity one so the owner's `cmd.reply <- snapshot{...}` never blocks, even in the rare case where the caller has already taken the `done` branch and walked away; the buffered value is simply discarded when the channel is garbage-collected. The unbuffered `cmds` channel is what guarantees the caller and owner agree on whether a command was delivered: the send completes only when the owner receives it, so if the owner has exited there is no false "delivered" and the caller correctly reports `Closed`.

### The runnable demo

The demo exercises the whole lifecycle and, crucially, fires 100 concurrent pings from separate goroutines to show the owner serialises them with no lock. A ping sent while still Connecting is rejected, the state stays Connecting, and the rejection shows up in the counters. After the pings the count is exactly 100 because every one lands while Connected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/session-lifecycle"
)

func main() {
	s := session.New(context.Background())

	fmt.Println("start:", s.State())
	fmt.Println("dial:", s.Do(session.EventDial))
	fmt.Println("ping too early:", s.Do(session.EventPing)) // illegal in Connecting
	fmt.Println("established:", s.Do(session.EventEstablished))

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Do(session.EventPing)
		}()
	}
	wg.Wait()

	fmt.Println("after 100 pings:", s.State())
	st := s.Stats()
	fmt.Printf("stats: dials=%d established=%d pings=%d rejected=%d\n",
		st.Dials, st.Established, st.Pings, st.Rejected)

	fmt.Println("close:", s.Do(session.EventClose))
	<-s.Done()
	fmt.Println("after close:", s.State())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: Idle
dial: Connecting
ping too early: Connecting
established: Connected
after 100 pings: Connected
stats: dials=1 established=1 pings=100 rejected=1
close: Closed
after close: Closed
```

### Tests

The tests pin the contract and prove the race-freedom that is the whole point. A ping while Connecting is rejected and leaves the state unchanged. The happy path reaches Connected then Closed. The concurrency test fires hundreds of `Do(Ping)` and `State()` calls from many goroutines at once and asserts both that the state is still Connected and that the ping count is exactly the number of pings sent — under `-race`, which is what certifies the owned state is never touched concurrently. Close is terminal: a later event does not revive the session. A cancelled context stops the owner and subsequent calls report Closed.

Create `session_test.go`:

```go
package session_test

import (
	"context"
	"sync"
	"testing"

	"example.com/session-lifecycle"
)

func TestPingRejectedWhileConnecting(t *testing.T) {
	t.Parallel()

	s := session.New(t.Context())
	if got := s.Do(session.EventDial); got != session.StateConnecting {
		t.Fatalf("after Dial = %q, want Connecting", got)
	}
	if got := s.Do(session.EventPing); got != session.StateConnecting {
		t.Fatalf("ping while Connecting = %q, want Connecting (rejected)", got)
	}
	if st := s.Stats(); st.Rejected != 1 || st.Pings != 0 {
		t.Fatalf("stats = %+v, want Rejected=1 Pings=0", st)
	}
}

func TestHappyPath(t *testing.T) {
	t.Parallel()

	s := session.New(t.Context())
	s.Do(session.EventDial)
	if got := s.Do(session.EventEstablished); got != session.StateConnected {
		t.Fatalf("after Established = %q, want Connected", got)
	}
	if got := s.Do(session.EventClose); got != session.StateClosed {
		t.Fatalf("after Close = %q, want Closed", got)
	}
	<-s.Done()
}

func TestConcurrentPingsNoRace(t *testing.T) {
	t.Parallel()

	s := session.New(t.Context())
	s.Do(session.EventDial)
	s.Do(session.EventEstablished)

	const pings = 500
	var wg sync.WaitGroup
	for range pings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Do(session.EventPing)
		}()
	}
	// Concurrent readers race the writers; the owner serialises them all.
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.State()
		}()
	}
	wg.Wait()

	if got := s.State(); got != session.StateConnected {
		t.Errorf("state = %q, want Connected", got)
	}
	if st := s.Stats(); st.Pings != pings {
		t.Errorf("Pings = %d, want %d", st.Pings, pings)
	}
}

func TestCloseIsTerminal(t *testing.T) {
	t.Parallel()

	s := session.New(t.Context())
	if got := s.Do(session.EventClose); got != session.StateClosed {
		t.Fatalf("Close = %q, want Closed", got)
	}
	<-s.Done()
	// A later event must not revive the session.
	if got := s.Do(session.EventDial); got != session.StateClosed {
		t.Errorf("Dial after Close = %q, want Closed", got)
	}
	if got := s.State(); got != session.StateClosed {
		t.Errorf("State after Close = %q, want Closed", got)
	}
}

func TestContextCancellationStopsOwner(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s := session.New(ctx)
	if got := s.Do(session.EventDial); got != session.StateConnecting {
		t.Fatalf("after Dial = %q, want Connecting", got)
	}

	cancel()
	<-s.Done()

	if got := s.State(); got != session.StateClosed {
		t.Errorf("State after cancel = %q, want Closed", got)
	}
}
```

## Review

The design is correct when the `Session` struct carries no state field at all: the state and counters live only on the owner's stack, so the `-race` run across hundreds of concurrent `Do` and `State` calls is clean by construction, not by careful locking. Confirm that a status read is a real message — `State()` and `Stats()` send a command and block on a reply — so a read can never observe a transition in progress. Confirm Close is terminal: the owner replies, then returns, and `defer close(done)` lets every later or in-flight caller report `Closed` instead of deadlocking. The concurrency test's exact ping count is the signal that no transition was lost or double-applied.

Common mistakes for this pattern. The first is putting `state` back on the struct "just for `State()`" — that single field reintroduces the race the whole design exists to remove; keep reads as messages. The second is buffering the `cmds` channel, which breaks the rendezvous: a send could then "succeed" into a buffer the exited owner never drains, and the caller would block on a reply that never comes. The third is forgetting that the owner must reply *before* it returns on Close, and that `done` must be closed on exit; skip either and a caller waits forever. The fourth is leaking the owner: always stop it, either by reaching Closed or by cancelling the context passed to `New` (the tests use `t.Context()` so the owner is torn down when the test ends).

## Resources

- [Go Concurrency Patterns (Rob Pike, 2012)](https://go.dev/talks/2012/concurrency.slide) — the owning-goroutine idea: confine state to one goroutine and communicate by channels.
- [Effective Go: Share by communicating](https://go.dev/doc/effective_go#sharing) — "Do not communicate by sharing memory; share memory by communicating," the principle this exercise applies to reads as well as writes.
- [`context` package](https://pkg.go.dev/context) — cancellation as the shutdown signal for a long-lived owning goroutine.

---

Back to [01-statefn-order-lifecycle.md](01-statefn-order-lifecycle.md) | Next: [03-order-job-transition-table.md](03-order-job-transition-table.md)
