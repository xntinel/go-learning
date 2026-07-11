# Exercise 21: Channel Circuit Breaker

A payment service calls an external API. When that API goes down, every request
hangs or fails slowly, consuming goroutines and dragging the whole system with
it. A circuit breaker stops the bleeding: after N consecutive failures it
"opens" and rejects requests immediately, without touching the failing API;
after a cooldown it lets a single probe through (half-open); if the probe
succeeds it closes and normal traffic resumes. The interesting part in Go is the
implementation -- instead of guarding the state with a mutex, you give it to a
single goroutine and talk to it over a command channel, so there are no locks,
no races, and no forgotten unlocks.

## What you'll build

```text
21-channel-circuit-breaker/
  main.go        a circuit breaker whose Closed/Open/HalfOpen state
                 lives in one goroutine behind a command channel
```

- Build: a channel-driven circuit breaker with cooldown and probe recovery.
- Implement: `CircuitBreaker` with a `run` loop owning `state` and `consecutiveFailures`; `callRequest`/`callResponse` carrying a per-call reply channel; the Open-to-HalfOpen cooldown transition; and a timestamped transition log.
- Verify: `go run main.go` (and `go run -race main.go` on the logged variant)

### Why a command channel beats a mutex here

The traditional implementation uses a mutex to protect shared state (failure
count, current state, last failure time). But a single goroutine with a command
channel is more natural in Go: all state lives inside one goroutine, commands
arrive via channel, and responses go back through per-request reply channels. No
locks, no races, no forgotten unlocks.

This is the state machine pattern from exercise 16, applied to a real
infrastructure concern. The command channel serializes all access, and the
goroutine is the single owner of all mutable state.

## Step 1 -- Closed and Open States

Build the core circuit breaker with two states: Closed (allow all calls) and Open (reject all calls). After `MaxFailures` consecutive failures, the breaker opens.

```go
package main

import (
	"fmt"
	"time"
)

const (
	maxFailures      = 3
	breakerLoopDelay = 10 * time.Millisecond
)

// CircuitState represents the current state of the circuit breaker.
type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	default:
		return "UNKNOWN"
	}
}

// BreakerConfig holds the circuit breaker configuration.
type BreakerConfig struct {
	MaxFailures int
}

// callRequest is a command sent to the breaker goroutine.
type callRequest struct {
	succeeded bool
	reply     chan callResponse
}

// callResponse tells the caller whether the request is allowed.
type callResponse struct {
	allowed bool
	state   CircuitState
}

// CircuitBreaker protects a downstream service from cascading failures.
// All state is managed by a single goroutine via the command channel.
type CircuitBreaker struct {
	config BreakerConfig
	cmds   chan callRequest
}

// NewCircuitBreaker creates and starts a circuit breaker.
func NewCircuitBreaker(config BreakerConfig) *CircuitBreaker {
	cb := &CircuitBreaker{
		config: config,
		cmds:   make(chan callRequest),
	}
	go cb.run()
	return cb
}

// run is the single goroutine that owns all breaker state.
func (cb *CircuitBreaker) run() {
	state := StateClosed
	consecutiveFailures := 0

	for cmd := range cb.cmds {
		switch state {
		case StateClosed:
			cmd.reply <- callResponse{allowed: true, state: state}
			if cmd.succeeded {
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
				if consecutiveFailures >= cb.config.MaxFailures {
					state = StateOpen
					fmt.Printf("  [breaker] TRIPPED: %d consecutive failures -> %s\n",
						consecutiveFailures, state)
				}
			}
		case StateOpen:
			cmd.reply <- callResponse{allowed: false, state: state}
		}
	}
}

// Call asks the breaker whether a request should proceed.
// After the external call completes, the caller reports success/failure.
func (cb *CircuitBreaker) Call(succeeded bool) (bool, CircuitState) {
	reply := make(chan callResponse, 1)
	cb.cmds <- callRequest{succeeded: succeeded, reply: reply}
	resp := <-reply
	return resp.allowed, resp.state
}

func main() {
	cb := NewCircuitBreaker(BreakerConfig{MaxFailures: maxFailures})

	calls := []struct {
		id      int
		success bool
	}{
		{1, true}, {2, true}, {3, false}, {4, false}, {5, false},
		{6, true}, {7, true},
	}

	for _, c := range calls {
		allowed, state := cb.Call(c.success)
		outcome := "SUCCESS"
		if !c.success {
			outcome = "FAILURE"
		}
		action := "ALLOWED"
		if !allowed {
			action = "REJECTED"
		}
		fmt.Printf("  call %d: %s -> %s (breaker: %s)\n",
			c.id, outcome, action, state)
	}

	time.Sleep(breakerLoopDelay)
}
```

Key observations:
- All state (`state`, `consecutiveFailures`) lives inside `run()` -- no shared memory
- Each call sends a command and waits for a response via its own reply channel
- After 3 consecutive failures, the breaker opens and rejects all subsequent calls
- Success resets the failure counter in the Closed state

### Verification
```bash
go run main.go
# Expected:
# Calls 1-2 succeed (allowed), call 3-5 fail, breaker trips after call 5
# Calls 6-7 are rejected (breaker is OPEN)
```

## Step 2 -- Add HalfOpen State with Cooldown

After opening, the breaker should not stay open forever. After a cooldown period, transition to HalfOpen: allow exactly one probe request. If the probe succeeds, close the circuit. If it fails, open again.

```go
package main

import (
	"errors"
	"fmt"
	"time"
)

const (
	stepTwoMaxFailures = 3
	cooldownDuration   = 500 * time.Millisecond
)

var ErrCircuitOpen = errors.New("circuit breaker is open")

type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	default:
		return "UNKNOWN"
	}
}

type BreakerConfig struct {
	MaxFailures      int
	CooldownDuration time.Duration
}

type callRequest struct {
	succeeded bool
	reply     chan callResponse
}

type callResponse struct {
	allowed bool
	state   CircuitState
}

type CircuitBreaker struct {
	config BreakerConfig
	cmds   chan callRequest
}

func NewCircuitBreaker(config BreakerConfig) *CircuitBreaker {
	cb := &CircuitBreaker{
		config: config,
		cmds:   make(chan callRequest),
	}
	go cb.run()
	return cb
}

func (cb *CircuitBreaker) run() {
	state := StateClosed
	consecutiveFailures := 0
	var openedAt time.Time

	for cmd := range cb.cmds {
		switch state {
		case StateClosed:
			cmd.reply <- callResponse{allowed: true, state: state}
			if cmd.succeeded {
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
				if consecutiveFailures >= cb.config.MaxFailures {
					state = StateOpen
					openedAt = time.Now()
					fmt.Printf("  [breaker] TRIPPED -> %s (cooldown: %v)\n",
						state, cb.config.CooldownDuration)
				}
			}

		case StateOpen:
			if time.Since(openedAt) >= cb.config.CooldownDuration {
				state = StateHalfOpen
				fmt.Printf("  [breaker] COOLDOWN EXPIRED -> %s (allowing probe)\n", state)
				cmd.reply <- callResponse{allowed: true, state: state}
				if cmd.succeeded {
					state = StateClosed
					consecutiveFailures = 0
					fmt.Printf("  [breaker] PROBE SUCCESS -> %s\n", state)
				} else {
					state = StateOpen
					openedAt = time.Now()
					fmt.Printf("  [breaker] PROBE FAILED -> %s (reset cooldown)\n", state)
				}
			} else {
				cmd.reply <- callResponse{allowed: false, state: state}
			}

		case StateHalfOpen:
			// Only one probe allowed; handled inline in StateOpen transition.
			// If we reach here, another request arrived during the probe.
			cmd.reply <- callResponse{allowed: false, state: state}
		}
	}
}

func (cb *CircuitBreaker) Call(succeeded bool) (bool, CircuitState) {
	reply := make(chan callResponse, 1)
	cb.cmds <- callRequest{succeeded: succeeded, reply: reply}
	resp := <-reply
	return resp.allowed, resp.state
}

func main() {
	cb := NewCircuitBreaker(BreakerConfig{
		MaxFailures:      stepTwoMaxFailures,
		CooldownDuration: cooldownDuration,
	})

	fmt.Println("=== Phase 1: Trip the breaker ===")
	for i := 1; i <= 4; i++ {
		allowed, state := cb.Call(false)
		fmt.Printf("  call %d: FAILURE -> allowed=%v (breaker: %s)\n", i, allowed, state)
	}

	fmt.Println()
	fmt.Println("=== Phase 2: Rejected while open ===")
	allowed, state := cb.Call(true)
	fmt.Printf("  call 5: SUCCESS -> allowed=%v (breaker: %s)\n", allowed, state)

	fmt.Println()
	fmt.Println("=== Phase 3: Wait for cooldown ===")
	time.Sleep(cooldownDuration + 50*time.Millisecond)

	fmt.Println()
	fmt.Println("=== Phase 4: Probe succeeds ===")
	allowed, state = cb.Call(true)
	fmt.Printf("  call 6: SUCCESS -> allowed=%v (breaker: %s)\n", allowed, state)

	fmt.Println()
	fmt.Println("=== Phase 5: Normal traffic resumes ===")
	for i := 7; i <= 9; i++ {
		allowed, state = cb.Call(true)
		fmt.Printf("  call %d: SUCCESS -> allowed=%v (breaker: %s)\n", i, allowed, state)
	}
}
```

### Verification
```bash
go run main.go
# Expected:
# Phase 1: calls 1-3 allowed (failing), breaker trips after call 3, call 4 rejected
# Phase 2: call 5 rejected (still open, cooldown not elapsed)
# Phase 3: wait for cooldown
# Phase 4: call 6 allowed as probe (half-open), succeeds -> circuit closes
# Phase 5: calls 7-9 allowed (circuit is closed again)
```

## Step 3 -- Simulated Payment Scenario: Failure and Recovery

Simulate 20 calls to a payment API. Calls 5-10 fail (API outage), the rest succeed. Observe the breaker opening, rejecting calls during the outage, then recovering via half-open probe after cooldown.

```go
package main

import (
	"fmt"
	"time"
)

const (
	scenarioMaxFailures = 3
	scenarioCooldown    = 400 * time.Millisecond
	callInterval        = 100 * time.Millisecond
	totalCalls          = 20
	failureStart        = 5
	failureEnd          = 10
)

type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	default:
		return "UNKNOWN"
	}
}

type BreakerConfig struct {
	MaxFailures      int
	CooldownDuration time.Duration
}

type callRequest struct {
	succeeded bool
	reply     chan callResponse
}

type callResponse struct {
	allowed bool
	state   CircuitState
}

type CircuitBreaker struct {
	config BreakerConfig
	cmds   chan callRequest
}

func NewCircuitBreaker(config BreakerConfig) *CircuitBreaker {
	cb := &CircuitBreaker{
		config: config,
		cmds:   make(chan callRequest),
	}
	go cb.run()
	return cb
}

func (cb *CircuitBreaker) run() {
	state := StateClosed
	consecutiveFailures := 0
	var openedAt time.Time

	for cmd := range cb.cmds {
		switch state {
		case StateClosed:
			cmd.reply <- callResponse{allowed: true, state: state}
			if cmd.succeeded {
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
				if consecutiveFailures >= cb.config.MaxFailures {
					state = StateOpen
					openedAt = time.Now()
				}
			}

		case StateOpen:
			if time.Since(openedAt) >= cb.config.CooldownDuration {
				state = StateHalfOpen
				cmd.reply <- callResponse{allowed: true, state: state}
				if cmd.succeeded {
					state = StateClosed
					consecutiveFailures = 0
				} else {
					state = StateOpen
					openedAt = time.Now()
				}
			} else {
				cmd.reply <- callResponse{allowed: false, state: state}
			}

		case StateHalfOpen:
			cmd.reply <- callResponse{allowed: false, state: state}
		}
	}
}

func (cb *CircuitBreaker) Call(succeeded bool) (bool, CircuitState) {
	reply := make(chan callResponse, 1)
	cb.cmds <- callRequest{succeeded: succeeded, reply: reply}
	resp := <-reply
	return resp.allowed, resp.state
}

// simulatePaymentAPI returns true if the API is healthy for this call number.
func simulatePaymentAPI(callNum int) bool {
	return callNum < failureStart || callNum > failureEnd
}

func main() {
	cb := NewCircuitBreaker(BreakerConfig{
		MaxFailures:      scenarioMaxFailures,
		CooldownDuration: scenarioCooldown,
	})

	epoch := time.Now()
	allowed, rejected, succeeded, failed := 0, 0, 0, 0

	fmt.Printf("%-6s %-8s %-10s %-10s %-10s\n",
		"CALL", "TIME", "API", "BREAKER", "OUTCOME")
	fmt.Println("----------------------------------------------------")

	for i := 1; i <= totalCalls; i++ {
		apiHealthy := simulatePaymentAPI(i)
		wasAllowed, state := cb.Call(apiHealthy)

		elapsed := time.Since(epoch).Round(time.Millisecond)
		apiStatus := "OK"
		if !apiHealthy {
			apiStatus = "FAIL"
		}
		outcome := ""
		if wasAllowed && apiHealthy {
			outcome = "SUCCESS"
			allowed++
			succeeded++
		} else if wasAllowed && !apiHealthy {
			outcome = "FAILED"
			allowed++
			failed++
		} else {
			outcome = "REJECTED"
			rejected++
		}

		fmt.Printf("%-6d %-8s %-10s %-10s %-10s\n",
			i, elapsed, apiStatus, state, outcome)

		time.Sleep(callInterval)
	}

	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Printf("Total calls:  %d\n", totalCalls)
	fmt.Printf("Allowed:      %d (succeeded: %d, failed: %d)\n", allowed, succeeded, failed)
	fmt.Printf("Rejected:     %d (saved from calling failing API)\n", rejected)
}
```

### Verification
```bash
go run main.go
# Expected:
# Calls 1-4: allowed, 1-4 succeed
# Calls 5-7: allowed (failing), breaker trips after call 7
# Calls 8-10: rejected (breaker OPEN, API still down)
# After cooldown: probe allowed, API is healthy again -> circuit closes
# Remaining calls: allowed and succeed
```

## Step 4 -- State Transition Timeline

Add a transition log to the breaker that records every state change with timestamps. Print the full timeline at the end to provide a clear picture of breaker behavior.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	timelineMaxFailures = 3
	timelineCooldown    = 400 * time.Millisecond
	timelineCallDelay   = 100 * time.Millisecond
	timelineTotalCalls  = 20
	timelineFailStart   = 5
	timelineFailEnd     = 10
)

type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	default:
		return "UNKNOWN"
	}
}

type BreakerConfig struct {
	MaxFailures      int
	CooldownDuration time.Duration
}

// Transition records a state change in the circuit breaker.
type Transition struct {
	Timestamp time.Duration
	From      CircuitState
	To        CircuitState
	Reason    string
}

type callRequest struct {
	succeeded bool
	reply     chan callResponse
}

type callResponse struct {
	allowed bool
	state   CircuitState
}

type CircuitBreaker struct {
	config      BreakerConfig
	cmds        chan callRequest
	epoch       time.Time
	mu          sync.Mutex
	transitions []Transition
}

func NewCircuitBreaker(config BreakerConfig, epoch time.Time) *CircuitBreaker {
	cb := &CircuitBreaker{
		config: config,
		cmds:   make(chan callRequest),
		epoch:  epoch,
	}
	go cb.run()
	return cb
}

func (cb *CircuitBreaker) recordTransition(from, to CircuitState, reason string) {
	cb.mu.Lock()
	cb.transitions = append(cb.transitions, Transition{
		Timestamp: time.Since(cb.epoch).Round(time.Millisecond),
		From:      from,
		To:        to,
		Reason:    reason,
	})
	cb.mu.Unlock()
}

func (cb *CircuitBreaker) run() {
	state := StateClosed
	consecutiveFailures := 0
	var openedAt time.Time

	for cmd := range cb.cmds {
		switch state {
		case StateClosed:
			cmd.reply <- callResponse{allowed: true, state: state}
			if cmd.succeeded {
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
				if consecutiveFailures >= cb.config.MaxFailures {
					prev := state
					state = StateOpen
					openedAt = time.Now()
					cb.recordTransition(prev, state,
						fmt.Sprintf("%d consecutive failures", consecutiveFailures))
				}
			}

		case StateOpen:
			if time.Since(openedAt) >= cb.config.CooldownDuration {
				prev := state
				state = StateHalfOpen
				cb.recordTransition(prev, state, "cooldown expired")

				cmd.reply <- callResponse{allowed: true, state: state}
				prev = state
				if cmd.succeeded {
					state = StateClosed
					consecutiveFailures = 0
					cb.recordTransition(prev, state, "probe succeeded")
				} else {
					state = StateOpen
					openedAt = time.Now()
					cb.recordTransition(prev, state, "probe failed")
				}
			} else {
				cmd.reply <- callResponse{allowed: false, state: state}
			}

		case StateHalfOpen:
			cmd.reply <- callResponse{allowed: false, state: state}
		}
	}
}

func (cb *CircuitBreaker) Call(succeeded bool) (bool, CircuitState) {
	reply := make(chan callResponse, 1)
	cb.cmds <- callRequest{succeeded: succeeded, reply: reply}
	resp := <-reply
	return resp.allowed, resp.state
}

// Transitions returns a copy of the transition log.
func (cb *CircuitBreaker) Transitions() []Transition {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	result := make([]Transition, len(cb.transitions))
	copy(result, cb.transitions)
	return result
}

func simulateAPI(callNum int) bool {
	return callNum < timelineFailStart || callNum > timelineFailEnd
}

func main() {
	epoch := time.Now()
	cb := NewCircuitBreaker(BreakerConfig{
		MaxFailures:      timelineMaxFailures,
		CooldownDuration: timelineCooldown,
	}, epoch)

	allowed, rejected := 0, 0

	fmt.Printf("%-5s %-8s %-6s %-11s %-10s\n",
		"CALL", "TIME", "API", "BREAKER", "OUTCOME")
	fmt.Println("---------------------------------------------")

	for i := 1; i <= timelineTotalCalls; i++ {
		apiOK := simulateAPI(i)
		wasAllowed, state := cb.Call(apiOK)

		elapsed := time.Since(epoch).Round(time.Millisecond)
		api := "OK"
		if !apiOK {
			api = "FAIL"
		}
		outcome := "REJECTED"
		if wasAllowed && apiOK {
			outcome = "SUCCESS"
			allowed++
		} else if wasAllowed {
			outcome = "FAILED"
			allowed++
		} else {
			rejected++
		}

		fmt.Printf("%-5d %-8s %-6s %-11s %-10s\n",
			i, elapsed, api, state, outcome)

		time.Sleep(timelineCallDelay)
	}

	fmt.Println()
	fmt.Println("=== State Transition Timeline ===")
	fmt.Printf("%-10s %-12s %-12s %s\n", "TIME", "FROM", "TO", "REASON")
	fmt.Println("------------------------------------------------")
	for _, t := range cb.Transitions() {
		fmt.Printf("%-10s %-12s %-12s %s\n",
			t.Timestamp, t.From, t.To, t.Reason)
	}

	fmt.Printf("\n=== Results ===\n")
	fmt.Printf("Allowed: %d | Rejected: %d | Total: %d\n",
		allowed, rejected, timelineTotalCalls)
	fmt.Printf("Rejected calls avoided hitting the failing API\n")
}
```

### Verification
```bash
go run -race main.go
# Expected:
# Call log shows correct allowed/rejected behavior
# Transition timeline shows:
#   CLOSED -> OPEN (3 consecutive failures)
#   OPEN -> HALF-OPEN (cooldown expired)
#   HALF-OPEN -> CLOSED (probe succeeded)
# No race warnings
```

## Common Mistakes

### Using Shared Variables Instead of a Command Channel

**Wrong:**
```go
type CircuitBreaker struct {
    mu       sync.Mutex
    state    CircuitState
    failures int
}

func (cb *CircuitBreaker) Call() bool {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    // complex state logic under a lock
}
```

**What happens:** It works, but the lock-based approach scatters state logic across multiple methods. Forgetting to lock in one path causes data races. The channel-based approach confines all state logic to a single goroutine.

**Fix:** Use a command channel. One goroutine owns all state, no locks needed:
```go
func (cb *CircuitBreaker) run() {
    state := StateClosed // only this goroutine touches state
    for cmd := range cb.cmds {
        // all logic here, no locks
    }
}
```

### Blocking the Caller When the Circuit Is Open

**Wrong:**
```go
case StateOpen:
    time.Sleep(cb.config.CooldownDuration) // blocks the caller!
    // then try half-open
```

**What happens:** Every rejected call blocks for the full cooldown duration. The caller wanted a fast rejection.

**Fix:** Check elapsed time and reject immediately if cooldown has not expired:
```go
case StateOpen:
    if time.Since(openedAt) >= cb.config.CooldownDuration {
        // transition to half-open
    } else {
        cmd.reply <- callResponse{allowed: false, state: state}
    }
```

### Allowing Multiple Probes in HalfOpen

**Wrong:**
```go
case StateHalfOpen:
    cmd.reply <- callResponse{allowed: true, state: state} // every call is a probe!
```

**What happens:** Multiple concurrent calls are sent to the failing API during half-open, defeating the purpose of limiting to one probe.

**Fix:** Allow exactly one probe (handled in the Open-to-HalfOpen transition), reject all other requests while in HalfOpen.

## Review

The breaker is a three-state machine -- Closed allows calls, Open rejects them,
HalfOpen admits a single probe -- and the entire design decision that makes it
clean is that one goroutine owns `state`, `consecutiveFailures`, and `openedAt`.
Every caller sends a `callRequest` with its own buffered reply channel and waits
for a `callResponse`, so the `run` loop serializes all access without a lock in
sight. The transitions follow the failure story: `MaxFailures` consecutive
failures trip Closed to Open and stamp the open time; once `CooldownDuration`
has elapsed, the next call flips Open to HalfOpen and carries the lone probe; a
successful probe closes the circuit and resets the counter, a failed probe
reopens it with a fresh cooldown. The transition log, guarded by its own small
mutex only because a caller reads it from outside, gives an audit trail for
debugging and monitoring.

Check yourself on the three questions the design turns on. Why does a single
goroutine with a command channel remove the need for any mutex on breaker state?
What exactly happens when the HalfOpen probe fails, and how does the cooldown
get reset? And why must the reply channel be buffered with capacity one rather
than unbuffered? Answer those and you understand not just this breaker but the
command-channel idiom underneath it.

## Resources
- [Martin Fowler: Circuit Breaker](https://martinfowler.com/bliki/CircuitBreaker.html) -- the canonical description of the three states and their transitions.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the command-channel and state-owning-goroutine idioms this builds on.
- [Microsoft: Circuit Breaker pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) -- production considerations: half-open probing, timeouts, and monitoring.

---

Back to [Concurrency](../../concurrency.md) | Next: [22-channel-request-multiplexer](../22-channel-request-multiplexer/22-channel-request-multiplexer.md)
