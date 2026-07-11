# 22. Channel-Based State Machine — Concepts

A state machine is a small program that is always in exactly one of a fixed set of states and moves between them only on named events. The interesting question in Go is not how to draw the diagram but how to make the "current state" safe under concurrency. The naive answer stores the state in a struct field and wraps every read and write in a mutex; the Go answer is to let a single goroutine own the state so that there is nothing to lock at all. This chapter builds three machines that share that idea from three angles: the classic `StateFn` pattern where the state is a function on the call stack, a connection/session machine where one owning goroutine answers both events and queries through a channel, and an order/job machine built from an explicit transition table that rejects illegal moves with a typed error. Read this file once and you will have the concepts each exercise leans on; each exercise then stands alone as its own Go module.

## Concepts

### Why a Mutex Is the Wrong Default for "Current State"

The instinct from other languages is to keep the state in a variable and guard it: `mu.Lock(); s.current = next; mu.Unlock()`. This works, but it pushes a surprising amount of correctness onto every call site. The lock must be held across the whole read-decide-write sequence, not just the write, or two callers can both read `Pending`, both decide to advance, and both write — losing one transition. The lock scope is easy to get wrong on an early return, and holding it during any I/O inside a transition serialises unrelated work. Under a high event rate the single mutex becomes the bottleneck the whole design contends on.

Go's preferred answer is the opposite: do not share the state at all. Hand the state to one goroutine and have every other goroutine talk to it through a channel. "Do not communicate by sharing memory; instead, share memory by communicating." When exactly one goroutine ever touches the state variable, there is no data race by construction, the race detector has nothing to find, and transitions are serialised for free because that goroutine processes one message at a time.

### The StateFn Pattern: State as a Function

The first machine represents each state as a function rather than as an enum value. The type is recursive:

```
type StateFn func(ctx context.Context, events <-chan Event) StateFn
```

A state function blocks on the events channel, processes one event, and returns the function for the next state. Returning `nil` terminates the machine. The driver loop is three lines:

```
for state := initial; state != nil; {
	state = state(ctx, events)
}
```

There is no `switch` over an enum and no `current` field. The "current state" is simply which function is presently on the call stack, and only the goroutine running the loop ever advances it. Rob Pike introduced this shape in his 2011 lexer talk: each lexer state is a `stateFn` that scans some input and returns the next `stateFn`. The pattern shines when each state has genuinely different handling logic, because that logic lives in its own named function instead of in one giant switch.

Two details make it robust. First, every state function selects on `ctx.Done()` alongside the events channel, so a cancelled context unblocks a machine that is waiting for an event that will never come; the function returns `nil` and the loop ends. Second, every receive uses the comma-ok form, `ev, ok := <-events`, and returns `nil` when `ok` is false. A closed channel otherwise yields the zero value forever, and a machine that does not check `ok` will spin processing empty events as if they were real.

### The Owning-Goroutine (Actor) Pattern: Events and Queries Through One Channel

The StateFn loop reads events but offers no clean way for an outside caller to ask "what state are you in right now?" without touching the state variable from another goroutine. The owning-goroutine pattern closes that gap. One goroutine runs a `for-select` loop over a command channel; the state and any counters are local variables inside that loop, never struct fields, so they are physically unreachable from any other goroutine. Callers do not read the state — they send a command and, when they need an answer, include a reply channel the owner writes the result back on.

```
type command struct {
	event EventType
	reply chan State // nil for fire-and-forget; non-nil for a query or a synchronous event
}
```

This is the crucial move for a connection or session machine: a *read* of the current state is itself a message. `State()` sends a query command with a fresh reply channel and blocks on it; the owner answers in the same serialised loop that processes events, so a status read can never race a transition. Because the state lives only on the owner's stack, the race detector confirms there is no shared memory even when hundreds of goroutines call `Submit` and `State` at once.

Termination needs care so callers never block forever. When the machine reaches a terminal state the owner replies (if a reply was requested), closes a `done` channel, and returns. Every caller method selects on both the send and `done`, so a `Submit` or `State` issued after shutdown returns immediately instead of deadlocking on a channel no one is reading.

### The Transition-Table Pattern: Explicit Moves and Illegal-Move Rejection

When the rule "which events are legal in which state" is the heart of the problem — as it is for an order or job lifecycle — encoding it as data is clearer than scattering it across functions or switch arms. A nested map is the whole specification:

```
var transitions = map[State]map[Event]State{
	StateCreated:    {EventStart: StateProcessing},
	StateProcessing: {EventComplete: StateDone, EventFail: StateFailed},
	StateFailed:     {EventRetry: StateProcessing},
}
```

Applying an event is a single lookup. If the inner map has no entry for the event, the move is illegal: the machine stays put and returns a typed error that names the state and the event, so the caller can distinguish "you asked for something impossible" from "the work failed." Terminal states (`StateDone`) simply have no outgoing entries, so every event is rejected there. New states and edges are added by editing the table, not by touching control flow, and the table can be walked to render a diagram or to assert reachability in a test.

This machine is still concurrency-safe the same way the others are: one owning goroutine holds the table of jobs (`map[JobID]State`) and applies every transition, while callers submit `Apply(id, event)` and receive the resulting error back over a reply channel. Many producers can drive many jobs at once; because only the owner touches the map, there is no lock and `-race` stays clean. The typed `*IllegalTransitionError` travels back to whichever producer asked for the bad move.

### Serialisation, Buffering, and Backpressure

All three machines rely on the same property: a channel delivers one message at a time, so the owning goroutine processes events in arrival order with no interleaving. The buffer size of that channel is a backpressure knob, not a correctness knob. An unbuffered channel makes every `Submit` wait until the owner is ready to receive, which couples the producer's pace to the machine's; a buffered channel lets producers run ahead by up to the buffer before they block. Either is correct. The classic deadlock is sending several events into an unbuffered channel from the same goroutine that is supposed to start the machine: the first send blocks because no one is receiving yet. The fix is to start the owner first, or to size the buffer to the number of events you enqueue before the owner runs.

## Common Mistakes

### Guarding the State With a Mutex Instead of Owning It

Wrong: `type Machine struct { mu sync.Mutex; current State }` with a `Lock`/`Unlock` around every read and write. The lock must wrap the entire read-decide-write step, the scope is easy to leak on an early return, and the single mutex serialises everything through one contended point.

Fix: give the state to one goroutine. In the StateFn machine the state is the function on the call stack; in the actor and transition-table machines it is a local variable in the owner's loop. Other goroutines communicate by sending on a channel. There is no lock because there is nothing shared.

### Reading the State Directly From Another Goroutine

Wrong: exposing `func (s *Session) State() State { return s.current }` where `s.current` is a struct field the owner also writes. This is a data race even if it "usually" returns the right value, and the race detector will flag it.

Fix: make a read a message too. `State()` sends a query command with a reply channel and blocks on it; the owner answers inside the same loop that handles events, so the read is serialised with every transition.

### Not Handling a Closed or Cancelled Channel

Wrong: `case ev := <-events:` without the comma-ok check, and no `ctx.Done()` case. A closed events channel then yields the zero `Event` forever and the machine busy-loops; a machine waiting on an event that never arrives can never be stopped.

Fix: use `case ev, ok := <-events:` and return `nil` (or break the loop) when `ok` is false, and always select on `ctx.Done()` so cancellation unblocks the wait.

### Treating an Illegal Transition as a Crash

Wrong: panicking, or silently advancing, when an event arrives that the current state does not allow. A panic takes down the owning goroutine; a silent advance corrupts the lifecycle.

Fix: reject the move without changing state and report it — return itself in the StateFn machine while recording the rejection, or return a typed `*IllegalTransitionError` from the transition-table machine. An illegal move is an expected input, not an exceptional condition.

### Deadlocking on an Unbuffered Channel Before the Owner Runs

Wrong: creating an unbuffered events channel and sending several events from the same goroutine before the machine's loop starts receiving. The first send blocks forever.

Fix: start the owning goroutine first, then send; or size the channel buffer to at least the number of events enqueued before the owner begins draining.

---

Next: [01-statefn-order-lifecycle.md](01-statefn-order-lifecycle.md)
