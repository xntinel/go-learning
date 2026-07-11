# Exercise 16: Channel State Machine

Order fulfillment follows a strict lifecycle -- Created, Paid, Shipped, Delivered, with Cancelled as an escape hatch from certain states -- and several services touch the same order at once: the payment gateway marks it Paid, the warehouse marks it Shipped, the driver marks it Delivered, and the customer may cancel at any time. The traditional fix protects that state with a mutex, but mutexes compose poorly here: you must hold the lock across checking the current state, validating the transition, updating it, and appending to the history log, and one missed lock is a race while two held locks is a deadlock. This exercise takes the other road -- a single goroutine owns the state and serializes every change through a channel -- so the invariant is enforced by construction rather than by remembering to lock.

## What you'll build

```text
16-channel-state-machine/
  main.go        an order lifecycle owned by one goroutine: transition commands
                 with reply channels, an allowed-transitions map, a history query
```

- Build: an order state machine where one goroutine owns the state and clients drive it through a command channel.
- Implement: a `Transition`/`Command` struct with an embedded reply channel, `runStateMachine` validating against an `allowedTransitions` map, `requestTransition`, and a `queryHistory` command that returns a copied snapshot.
- Verify: `go run main.go`, and `go run -race main.go` on the multi-client steps.

### Why single-goroutine ownership removes the locks

The channel approach is simpler than the mutex one: a single goroutine owns the order state and listens for transition commands on a channel. Each command embeds a reply channel so the caller gets back a success or an error. No locks, no races, no deadlocks -- the state machine goroutine processes commands one at a time, sequentially. This is Go's "share memory by communicating" principle applied to state management.

Because exactly one goroutine ever reads or writes `current` and the history slice, there is nothing to protect and nothing to deadlock. Concurrency lives entirely in the mailbox: multiple clients can send simultaneously, but the machine drains them in order, so the "cancel versus ship" race is resolved by arrival order instead of by a lock the two callers must agree to share.

## Step 1 -- Single Order State Machine

Build a state machine for one order. A single goroutine owns the state and processes transitions from a command channel. Valid transitions are defined in a map. Invalid transitions return an error through the embedded reply channel.

```go
package main

import "fmt"

// OrderState represents a point in the order lifecycle.
type OrderState string

const (
	StateCreated   OrderState = "Created"
	StatePaid      OrderState = "Paid"
	StateShipped   OrderState = "Shipped"
	StateDelivered OrderState = "Delivered"
	StateCancelled OrderState = "Cancelled"
)

// Transition is a command sent to the state machine goroutine.
// The Reply channel carries back nil (success) or an error.
type Transition struct {
	ToState OrderState
	Reply   chan error
}

// allowedTransitions defines which state moves are legal.
// The key is the current state; the value is the set of reachable states.
var allowedTransitions = map[OrderState]map[OrderState]bool{
	StateCreated:   {StatePaid: true, StateCancelled: true},
	StatePaid:      {StateShipped: true, StateCancelled: true},
	StateShipped:   {StateDelivered: true},
	StateDelivered: {},
	StateCancelled: {},
}

// runStateMachine owns the order state. It reads transitions from the
// commands channel and replies with success or an error.
func runStateMachine(orderID string, commands <-chan Transition) {
	current := StateCreated
	fmt.Printf("[%s] state machine started in %s\n", orderID, current)

	for cmd := range commands {
		targets, exists := allowedTransitions[current]
		if !exists || !targets[cmd.ToState] {
			cmd.Reply <- fmt.Errorf(
				"invalid transition: %s -> %s", current, cmd.ToState,
			)
			continue
		}
		previous := current
		current = cmd.ToState
		fmt.Printf("[%s] %s -> %s\n", orderID, previous, current)
		cmd.Reply <- nil
	}

	fmt.Printf("[%s] state machine stopped in %s\n", orderID, current)
}

// requestTransition sends a transition command and waits for the reply.
func requestTransition(commands chan<- Transition, toState OrderState) error {
	reply := make(chan error, 1)
	commands <- Transition{ToState: toState, Reply: reply}
	return <-reply
}

func main() {
	commands := make(chan Transition)
	go runStateMachine("order-1001", commands)

	transitions := []OrderState{
		StatePaid,
		StateShipped,
		StateDelivered,
	}

	for _, state := range transitions {
		if err := requestTransition(commands, state); err != nil {
			fmt.Printf("ERROR: %v\n", err)
		}
	}

	close(commands)
	fmt.Println("Order lifecycle complete")
}
```

Key observations:
- The state machine goroutine is the only code that reads or writes `current` -- no mutex needed
- Each `Transition` embeds a `Reply chan error` so the caller gets synchronous feedback
- The reply channel is buffered (`cap 1`) so the state machine never blocks on reply even if the caller disappears
- `close(commands)` causes `range` to exit, shutting down the state machine cleanly

### Verification
```bash
go run main.go
# Expected:
#   [order-1001] state machine started in Created
#   [order-1001] Created -> Paid
#   [order-1001] Paid -> Shipped
#   [order-1001] Shipped -> Delivered
#   [order-1001] state machine stopped in Delivered
#   Order lifecycle complete
```

## Step 2 -- Multiple Concurrent Clients

In production, multiple services send transitions to the same order simultaneously. The payment gateway, warehouse, and customer portal all talk to the same state machine. Because the goroutine processes commands sequentially, transitions are serialized without locks.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const clientDelay = 50 * time.Millisecond

type OrderState string

const (
	StateCreated   OrderState = "Created"
	StatePaid      OrderState = "Paid"
	StateShipped   OrderState = "Shipped"
	StateDelivered OrderState = "Delivered"
	StateCancelled OrderState = "Cancelled"
)

type Transition struct {
	ToState OrderState
	Client  string
	Reply   chan error
}

var allowedTransitions = map[OrderState]map[OrderState]bool{
	StateCreated:   {StatePaid: true, StateCancelled: true},
	StatePaid:      {StateShipped: true, StateCancelled: true},
	StateShipped:   {StateDelivered: true},
	StateDelivered: {},
	StateCancelled: {},
}

func runStateMachine(orderID string, commands <-chan Transition) {
	current := StateCreated
	fmt.Printf("[%s] started in %s\n", orderID, current)

	for cmd := range commands {
		targets := allowedTransitions[current]
		if !targets[cmd.ToState] {
			cmd.Reply <- fmt.Errorf(
				"%s -> %s (requested by %s)", current, cmd.ToState, cmd.Client,
			)
			continue
		}
		previous := current
		current = cmd.ToState
		fmt.Printf("[%s] %s -> %s (by %s)\n", orderID, previous, current, cmd.Client)
		cmd.Reply <- nil
	}

	fmt.Printf("[%s] stopped in %s\n", orderID, current)
}

func requestTransition(commands chan<- Transition, toState OrderState, client string) error {
	reply := make(chan error, 1)
	commands <- Transition{ToState: toState, Client: client, Reply: reply}
	return <-reply
}

// simulateClient sends a transition after a short delay, simulating
// a real service making a request at an unpredictable time.
func simulateClient(commands chan<- Transition, toState OrderState, client string, delay time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()
	time.Sleep(delay)
	err := requestTransition(commands, toState, client)
	if err != nil {
		fmt.Printf("[%s] REJECTED: %v\n", client, err)
	}
}

func main() {
	commands := make(chan Transition)
	go runStateMachine("order-2001", commands)

	var wg sync.WaitGroup

	// Multiple clients send transitions concurrently.
	// The payment gateway pays, then the warehouse ships.
	// Meanwhile, the customer tries to cancel after payment.
	wg.Add(4)
	go simulateClient(commands, StatePaid, "payment-gateway", 0, &wg)
	go simulateClient(commands, StateCancelled, "customer-portal", clientDelay, &wg)
	go simulateClient(commands, StateShipped, "warehouse", 2*clientDelay, &wg)
	go simulateClient(commands, StateDelivered, "delivery-driver", 3*clientDelay, &wg)

	wg.Wait()
	close(commands)
	fmt.Println()
	fmt.Println("All clients finished")
}
```

The customer portal tries to cancel after the order is paid. Whether this succeeds depends on timing -- if the warehouse already shipped, the cancellation is rejected. The state machine serializes all requests, so there is no race between "cancel" and "ship".

### Verification
```bash
go run -race main.go
# Expected:
#   payment-gateway transitions Created -> Paid
#   customer-portal either cancels (Paid -> Cancelled) or gets rejected
#   warehouse either ships (Paid -> Shipped) or gets rejected
#   No race warnings
```

## Step 3 -- Transition History Log

Add a query command that returns the full transition history. The state machine goroutine maintains a log of all successful transitions and returns it on demand. This demonstrates that the command channel can carry different types of operations.

```go
package main

import (
	"fmt"
	"strings"
	"time"
)

type OrderState string

const (
	StateCreated   OrderState = "Created"
	StatePaid      OrderState = "Paid"
	StateShipped   OrderState = "Shipped"
	StateDelivered OrderState = "Delivered"
	StateCancelled OrderState = "Cancelled"
)

// HistoryEntry records a single state transition.
type HistoryEntry struct {
	From      OrderState
	To        OrderState
	Client    string
	Timestamp time.Time
}

// Command is a tagged union: either a transition request or a history query.
type Command struct {
	// Transition fields (used when IsQuery is false).
	ToState OrderState
	Client  string
	Reply   chan error

	// Query fields (used when IsQuery is true).
	IsQuery      bool
	HistoryReply chan []HistoryEntry
}

var allowedTransitions = map[OrderState]map[OrderState]bool{
	StateCreated:   {StatePaid: true, StateCancelled: true},
	StatePaid:      {StateShipped: true, StateCancelled: true},
	StateShipped:   {StateDelivered: true},
	StateDelivered: {},
	StateCancelled: {},
}

func runStateMachine(orderID string, commands <-chan Command) {
	current := StateCreated
	var history []HistoryEntry

	for cmd := range commands {
		if cmd.IsQuery {
			snapshot := make([]HistoryEntry, len(history))
			copy(snapshot, history)
			cmd.HistoryReply <- snapshot
			continue
		}

		targets := allowedTransitions[current]
		if !targets[cmd.ToState] {
			cmd.Reply <- fmt.Errorf("%s -> %s (by %s)", current, cmd.ToState, cmd.Client)
			continue
		}

		entry := HistoryEntry{
			From:      current,
			To:        cmd.ToState,
			Client:    cmd.Client,
			Timestamp: time.Now(),
		}
		history = append(history, entry)
		current = cmd.ToState
		fmt.Printf("[%s] %s -> %s (by %s)\n", orderID, entry.From, entry.To, entry.Client)
		cmd.Reply <- nil
	}
}

func requestTransition(commands chan<- Command, toState OrderState, client string) error {
	reply := make(chan error, 1)
	commands <- Command{ToState: toState, Client: client, Reply: reply}
	return <-reply
}

func queryHistory(commands chan<- Command) []HistoryEntry {
	reply := make(chan []HistoryEntry, 1)
	commands <- Command{IsQuery: true, HistoryReply: reply}
	return <-reply
}

func formatHistory(entries []HistoryEntry) string {
	if len(entries) == 0 {
		return "  (no transitions yet)"
	}
	var b strings.Builder
	for i, e := range entries {
		fmt.Fprintf(&b, "  %d. %s -> %s (by %s)\n", i+1, e.From, e.To, e.Client)
	}
	return b.String()
}

func main() {
	commands := make(chan Command)
	go runStateMachine("order-3001", commands)

	// Walk through the order lifecycle.
	steps := []struct {
		state  OrderState
		client string
	}{
		{StatePaid, "payment-gateway"},
		{StateShipped, "warehouse"},
		{StateDelivered, "delivery-driver"},
	}

	for _, step := range steps {
		if err := requestTransition(commands, step.state, step.client); err != nil {
			fmt.Printf("REJECTED: %v\n", err)
		}
	}

	// Query the full history.
	fmt.Println()
	fmt.Println("=== Transition History ===")
	history := queryHistory(commands)
	fmt.Print(formatHistory(history))
	fmt.Printf("\nTotal transitions: %d\n", len(history))

	close(commands)
}
```

The history query is just another command on the same channel. The state machine goroutine detects `IsQuery` and responds with a snapshot (copy) of the history. Because only one goroutine reads and writes the history slice, there is no race. The caller receives an independent copy, safe to use after the state machine shuts down.

### Verification
```bash
go run -race main.go
# Expected:
#   3 successful transitions logged
#   History shows: Created->Paid, Paid->Shipped, Shipped->Delivered
#   No race warnings
```

## Step 4 -- Invalid Transition Rejection Demo

Demonstrate the state machine's resilience by sending a sequence of valid and invalid transitions. Invalid moves are rejected with clear error messages while valid transitions proceed normally. This proves the state machine enforces its invariants even under stress.

```go
package main

import (
	"fmt"
	"strings"
	"time"
)

type OrderState string

const (
	StateCreated   OrderState = "Created"
	StatePaid      OrderState = "Paid"
	StateShipped   OrderState = "Shipped"
	StateDelivered OrderState = "Delivered"
	StateCancelled OrderState = "Cancelled"
)

type HistoryEntry struct {
	From   OrderState
	To     OrderState
	Client string
}

type Command struct {
	ToState      OrderState
	Client       string
	Reply        chan error
	IsQuery      bool
	HistoryReply chan []HistoryEntry
}

var allowedTransitions = map[OrderState]map[OrderState]bool{
	StateCreated:   {StatePaid: true, StateCancelled: true},
	StatePaid:      {StateShipped: true, StateCancelled: true},
	StateShipped:   {StateDelivered: true},
	StateDelivered: {},
	StateCancelled: {},
}

func runStateMachine(orderID string, commands <-chan Command) {
	current := StateCreated
	var history []HistoryEntry

	for cmd := range commands {
		if cmd.IsQuery {
			snapshot := make([]HistoryEntry, len(history))
			copy(snapshot, history)
			cmd.HistoryReply <- snapshot
			continue
		}

		targets := allowedTransitions[current]
		if !targets[cmd.ToState] {
			cmd.Reply <- fmt.Errorf(
				"[%s] INVALID: %s -> %s (by %s)",
				orderID, current, cmd.ToState, cmd.Client,
			)
			continue
		}

		entry := HistoryEntry{From: current, To: cmd.ToState, Client: cmd.Client}
		history = append(history, entry)
		previous := current
		current = cmd.ToState
		fmt.Printf("[%s] OK: %s -> %s (by %s)\n", orderID, previous, current, cmd.Client)
		cmd.Reply <- nil
	}
}

func requestTransition(commands chan<- Command, toState OrderState, client string) error {
	reply := make(chan error, 1)
	commands <- Command{ToState: toState, Client: client, Reply: reply}
	return <-reply
}

func queryHistory(commands chan<- Command) []HistoryEntry {
	reply := make(chan []HistoryEntry, 1)
	commands <- Command{IsQuery: true, HistoryReply: reply}
	return <-reply
}

func formatHistory(entries []HistoryEntry) string {
	if len(entries) == 0 {
		return "  (empty)"
	}
	var b strings.Builder
	for i, e := range entries {
		fmt.Fprintf(&b, "  %d. %s -> %s (by %s)\n", i+1, e.From, e.To, e.Client)
	}
	return b.String()
}

func main() {
	commands := make(chan Command)
	go runStateMachine("order-4001", commands)

	// Mix of valid and invalid transitions to exercise rejection logic.
	attempts := []struct {
		state  OrderState
		client string
	}{
		{StateShipped, "warehouse"},       // INVALID: cannot skip Paid
		{StateDelivered, "delivery"},      // INVALID: still Created
		{StatePaid, "payment-gateway"},    // VALID: Created -> Paid
		{StatePaid, "payment-gateway"},    // INVALID: already Paid
		{StateCreated, "admin-rollback"},  // INVALID: cannot go backwards
		{StateShipped, "warehouse"},       // VALID: Paid -> Shipped
		{StateCancelled, "customer"},      // INVALID: cannot cancel after Shipped
		{StateDelivered, "delivery"},      // VALID: Shipped -> Delivered
		{StateShipped, "warehouse"},       // INVALID: Delivered is terminal
	}

	fmt.Println("=== Attempting 9 Transitions ===")
	accepted, rejected := 0, 0
	for _, a := range attempts {
		err := requestTransition(commands, a.state, a.client)
		if err != nil {
			fmt.Printf("  REJECTED: %v\n", err)
			rejected++
		} else {
			accepted++
		}
	}

	fmt.Printf("\n=== Results: %d accepted, %d rejected ===\n", accepted, rejected)

	fmt.Println()
	fmt.Println("=== Final Transition History ===")
	history := queryHistory(commands)
	fmt.Print(formatHistory(history))

	close(commands)

	// Print the state machine's invariant.
	fmt.Println()
	fmt.Println("=== State Machine Guarantees ===")
	fmt.Println("- Single goroutine owns state: no mutex needed")
	fmt.Println("- Commands serialized: no race conditions")
	fmt.Println("- Invalid transitions rejected: state invariants enforced")
	fmt.Println("- History is append-only: full audit trail")

	// Small delay to let the state machine goroutine print its final message.
	time.Sleep(50 * time.Millisecond)
}
```

Out of 9 attempted transitions, only 3 are valid (Created->Paid, Paid->Shipped, Shipped->Delivered). The other 6 are rejected with clear error messages explaining which transition was attempted and why it failed. The state machine enforces its invariants without any locking code.

### Verification
```bash
go run -race main.go
# Expected:
#   3 accepted, 6 rejected
#   History shows exactly: Created->Paid, Paid->Shipped, Shipped->Delivered
#   No race warnings
```

## Common Mistakes

### Forgetting the Reply Channel Buffer

**Wrong:**
```go
reply := make(chan error) // unbuffered!
commands <- Command{ToState: StatePaid, Reply: reply}
// if the caller never reads reply, the state machine goroutine blocks forever
```

**What happens:** If the caller sends the command but crashes or times out before reading the reply, the state machine goroutine blocks on the unbuffered reply send. The entire state machine is stuck.

**Fix:** Always buffer the reply channel with capacity 1:
```go
reply := make(chan error, 1) // state machine can always send the reply
```

### Sharing State Between the State Machine and Callers

**Wrong:**
```go
var currentState OrderState // shared variable!

go func() {
    for cmd := range commands {
        currentState = cmd.ToState // written by goroutine
    }
}()

fmt.Println(currentState) // read by main -- DATA RACE
```

**What happens:** Two goroutines access the same variable without synchronization. The race detector will catch this.

**Fix:** All state access goes through the command channel. Use a query command to read state, just like transitions use commands to write state.

### Closing the Command Channel From a Client

**Wrong:**
```go
go func() {
    requestTransition(commands, StatePaid, "payment")
    close(commands) // client should not close this!
}()

// other clients now panic when sending
```

**What happens:** Another client tries to send a command after the channel is closed -- panic.

**Fix:** Only the coordinator (the goroutine that created the channel) should close it, after all clients have finished.

## Review

The design rests on one goroutine owning all mutable state and draining commands from a channel one at a time, which eliminates data races by construction rather than by discipline: since only that goroutine ever reads or writes `current` and the history slice, no mutex is needed and none can deadlock. Every transition command embeds a reply channel so callers get synchronous success-or-error feedback, and that reply channel is buffered with capacity 1 so the machine can always deposit its answer and move on even if the caller has vanished. The `allowedTransitions` map is the invariant made data: any move not listed for the current state is rejected, which is exactly why the order cannot slide backwards or skip a step. A history query reuses the same command channel and returns a copied snapshot, so callers read state without ever touching the machine's internals, and closing the command channel shuts the machine down cleanly.

Make sure you can explain why the reply channel needs capacity 1 -- what blocks if it is unbuffered and the caller times out -- and how the transitions map, not any imperative check, is what forbids going backwards. You should also be able to say precisely why a single owning goroutine removes the need for mutexes, and what would break if two goroutines shared the same `current` variable: the moment ownership splits, the race the whole design avoided reappears.

## Resources
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the principle this state machine embodies: pass a command, do not share the state.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- goroutine-owns-state patterns and channel-based coordination.
- [Rob Pike: Concurrency is not Parallelism](https://go.dev/talks/2012/waza.slide) -- why serializing work through one goroutine is a design tool, not a limitation.

---

Back to [Concurrency](../../concurrency.md) | Next: [17-channel-streaming-backpressure](../17-channel-streaming-backpressure/17-channel-streaming-backpressure.md)
