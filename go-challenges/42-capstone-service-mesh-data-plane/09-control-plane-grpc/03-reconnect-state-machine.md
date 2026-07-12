# Exercise 3: Reconnect State Machine

When the control plane disappears, the data plane must keep serving traffic under its cached configuration and reconnect without stampeding the server the moment it returns. This exercise builds the two pieces that govern that behavior: a state machine that encodes the legal connection lifecycle and rejects illegal jumps, and an exponential-backoff function that spaces out reconnect attempts.

This module is fully self-contained: its own `go mod init`, all types defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
reconnect.go          ConnState + String, StateMachine (Transition/State), Backoff
cmd/
  demo/
    main.go           drive the lifecycle, reject an illegal jump, print backoffs
reconnect_test.go     happy path, invalid transitions, backoff values + monotonicity
```

- Files: `reconnect.go`, `cmd/demo/main.go`, `reconnect_test.go`.
- Implement: `ConnState` with `String`, `StateMachine` with `Transition` and `State`, and `Backoff(base, max time.Duration, attempt int) time.Duration`.
- Test: the full lifecycle is accepted, every illegal transition returns a wrapped `ErrInvalidTransition`, backoff doubles and caps at max, and the sequence never decreases.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a transition table and a capped doubling

The connection has exactly five states, and only a handful of moves between them make sense. A client that is `idle` may begin `connecting`; a `connecting` attempt either reaches `connected` or fails to `disconnected`; a `connected` stream can only drop to `disconnected`; a `disconnected` client moves to `reconnecting`; and a `reconnecting` client, once its backoff elapses, returns to `connecting`. Everything else is a bug. Encoding the legal edges as a map from state to its allowed successors turns "is this move legal" into a table lookup, and makes the illegal moves — `idle` straight to `connected`, `connecting` straight to `reconnecting` — return a wrapped sentinel error instead of silently corrupting the lifecycle. Wrapping the sentinel with `%w` lets callers test with `errors.Is(err, ErrInvalidTransition)` while still printing the offending `from -> to` pair.

Keeping connection state separate from configuration state is the design decision that makes an outage survivable. The proxy's cached snapshot (from Exercise 1) is unaffected by where the state machine sits; a control-plane outage degrades config freshness, not proxy availability. The state machine only answers "are we currently talking to the control plane, and if not, are we waiting to retry."

`Backoff` computes the wait for a given attempt by doubling: `base << attempt`, capped at `max`. Two guards keep it honest. Attempts beyond a shift ceiling (`maxShift = 30`) return `max` immediately, because shifting a `time.Duration` left by 60-odd bits is undefined and would wrap to a negative number. And even within range, a result that exceeds `max` or has gone non-positive from overflow collapses to `max`. The function is deliberately deterministic and jitter-free so it is testable; the caller is expected to add randomness on top — `Backoff(base, max, attempt) + rand.Int63n(int64(base))` — because synchronized, jitter-free reconnects are exactly the thundering herd that knocks over a control plane just as it recovers. What the function guarantees is monotonicity: the wait never shrinks as failures accumulate, so a persistently failing client backs off further and further rather than oscillating.

Create `reconnect.go`:

```go
package reconnect

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ConnState is the connection state of the data-plane xDS client.
type ConnState int

const (
	StateIdle         ConnState = iota // not yet started
	StateConnecting                    // dialing the control plane
	StateConnected                     // bidirectional stream established
	StateDisconnected                  // stream closed; proxy uses cached config
	StateReconnecting                  // waiting out backoff before next dial
)

// String implements fmt.Stringer.
func (s ConnState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateDisconnected:
		return "disconnected"
	case StateReconnecting:
		return "reconnecting"
	default:
		return fmt.Sprintf("ConnState(%d)", int(s))
	}
}

// ErrInvalidTransition is returned for a state change not permitted from the
// current state.
var ErrInvalidTransition = errors.New("reconnect: invalid client state transition")

// allowedTransitions lists every valid (current -> next) pair.
var allowedTransitions = map[ConnState][]ConnState{
	StateIdle:         {StateConnecting},
	StateConnecting:   {StateConnected, StateDisconnected},
	StateConnected:    {StateDisconnected},
	StateDisconnected: {StateReconnecting},
	StateReconnecting: {StateConnecting},
}

// StateMachine is a thread-safe xDS client connection state machine.
type StateMachine struct {
	mu    sync.Mutex
	state ConnState
}

// NewStateMachine returns a StateMachine in StateIdle.
func NewStateMachine() *StateMachine { return &StateMachine{state: StateIdle} }

// Transition moves to next or returns a wrapped ErrInvalidTransition.
func (sm *StateMachine) Transition(next ConnState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, allowed := range allowedTransitions[sm.state] {
		if allowed == next {
			sm.state = next
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, sm.state, next)
}

// State returns the current state.
func (sm *StateMachine) State() ConnState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state
}

// Backoff returns the exponential backoff duration for the given zero-indexed
// attempt. The duration doubles each attempt and is capped at max. Attempts
// beyond maxShift are capped immediately to avoid int64 overflow in the shift.
//
// The caller should add jitter to avoid thundering-herd reconnects:
//
//	wait = Backoff(base, max, attempt) + time.Duration(rand.Int63n(int64(base)))
func Backoff(base, max time.Duration, attempt int) time.Duration {
	if attempt <= 0 {
		return base
	}
	const maxShift = 30
	if attempt > maxShift {
		return max
	}
	d := base << uint(attempt)
	if d > max || d <= 0 { // d <= 0 guards against overflow on large base values
		return max
	}
	return d
}
```

### The runnable demo

The demo drives one full lifecycle — connect, drop, back off, redial — then shows that an illegal `connecting -> reconnecting` jump is rejected rather than applied, and prints the first five backoff durations doubling toward the cap.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/reconnect"
)

func main() {
	sm := reconnect.NewStateMachine()

	// Healthy connect, then a drop and a backed-off reconnect.
	lifecycle := []reconnect.ConnState{
		reconnect.StateConnecting,
		reconnect.StateConnected,
		reconnect.StateDisconnected,
		reconnect.StateReconnecting,
		reconnect.StateConnecting,
	}
	for _, step := range lifecycle {
		if err := sm.Transition(step); err != nil {
			fmt.Println("transition error:", err)
			return
		}
		fmt.Printf("state: %s\n", sm.State())
	}

	// An illegal jump is rejected, not applied.
	if err := sm.Transition(reconnect.StateReconnecting); err != nil {
		fmt.Println("rejected:", err)
	}

	// Reconnect waits grow until they hit the cap.
	base, max := 200*time.Millisecond, 30*time.Second
	for attempt := 0; attempt <= 4; attempt++ {
		fmt.Printf("attempt %d backoff: %s\n", attempt, reconnect.Backoff(base, max, attempt))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
state: connecting
state: connected
state: disconnected
state: reconnecting
state: connecting
rejected: reconnect: invalid client state transition: connecting -> reconnecting
attempt 0 backoff: 200ms
attempt 1 backoff: 400ms
attempt 2 backoff: 800ms
attempt 3 backoff: 1.6s
attempt 4 backoff: 3.2s
```

### Tests

The tests walk the full happy-path lifecycle, enumerate illegal transitions and assert each returns a wrapped `ErrInvalidTransition`, pin exact backoff values including the cap and the beyond-shift case, and prove the backoff sequence never decreases across attempts 0 through 14.

Create `reconnect_test.go`:

```go
package reconnect

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestStateMachineHappyPath(t *testing.T) {
	t.Parallel()
	sm := NewStateMachine()
	steps := []ConnState{
		StateConnecting, StateConnected, StateDisconnected,
		StateReconnecting, StateConnecting, StateConnected,
	}
	for _, next := range steps {
		if err := sm.Transition(next); err != nil {
			t.Fatalf("Transition(%s): %v", next, err)
		}
	}
	if got := sm.State(); got != StateConnected {
		t.Fatalf("State() = %s, want connected", got)
	}
}

func TestStateMachineRejectsInvalidTransitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup []ConnState
		next  ConnState
	}{
		{"idle->connected", nil, StateConnected},
		{"idle->disconnected", nil, StateDisconnected},
		{"connecting->reconnecting", []ConnState{StateConnecting}, StateReconnecting},
		{"connected->connecting", []ConnState{StateConnecting, StateConnected}, StateConnecting},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sm := NewStateMachine()
			for _, step := range tc.setup {
				if err := sm.Transition(step); err != nil {
					t.Fatalf("setup Transition(%s): %v", step, err)
				}
			}
			err := sm.Transition(tc.next)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("Transition(%s) err = %v, want ErrInvalidTransition", tc.next, err)
			}
		})
	}
}

func TestBackoffValues(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	max := 5 * time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{10, 5 * time.Second}, // capped at max
		{63, 5 * time.Second}, // beyond maxShift, always capped
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("attempt%d", tc.attempt), func(t *testing.T) {
			t.Parallel()
			if got := Backoff(base, max, tc.attempt); got != tc.want {
				t.Fatalf("Backoff(100ms, 5s, %d) = %s, want %s", tc.attempt, got, tc.want)
			}
		})
	}
}

func TestBackoffIsMonotonic(t *testing.T) {
	t.Parallel()
	prev := time.Duration(-1)
	for attempt := 0; attempt <= 14; attempt++ {
		got := Backoff(100*time.Millisecond, 30*time.Second, attempt)
		if got < prev {
			t.Fatalf("Backoff at attempt %d = %s decreased from %s", attempt, got, prev)
		}
		prev = got
	}
}

func ExampleConnState_String() {
	fmt.Println(StateConnecting)
	fmt.Println(StateConnected)
	fmt.Println(StateDisconnected)
	// Output:
	// connecting
	// connected
	// disconnected
}
```

## Review

The state machine is correct when only table-listed edges succeed and every other move returns a wrapped error without mutating the state. The common mistake is using a plain comparison instead of `errors.Is`, or returning a bare error that callers cannot classify; wrapping `ErrInvalidTransition` with `%w` and testing with `errors.Is` is what `TestStateMachineRejectsInvalidTransitions` enforces. The backoff's correctness rests on the overflow guards: drop the `maxShift` ceiling and `base << 64` wraps negative, making a "backoff" return a tiny or negative duration that turns the reconnect loop into a busy spin. `TestBackoffValues` pins the doubling and both cap paths, and `TestBackoffIsMonotonic` pins the property the whole reconnect strategy depends on — that the wait never decreases as failures pile up. Remember that the function is intentionally jitter-free; production code adds randomness at the call site so synchronized clients do not reconnect in lockstep.

## Resources

- [`errors.Is` and `%w` wrapping](https://pkg.go.dev/errors#Is) — the sentinel-error pattern the transition guard uses.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why a deterministic backoff needs added jitter to avoid the thundering herd.
- [`time.Duration`](https://pkg.go.dev/time#Duration) — the int64-nanosecond type whose shift behavior the overflow guards protect against.

---

Back to [02-ack-nack-protocol.md](02-ack-nack-protocol.md) | Next: [04-grpc-control-plane.md](04-grpc-control-plane.md)
