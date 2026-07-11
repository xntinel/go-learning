# 9. Circuit Breaker with Half-Open State

A circuit breaker is a state machine that sits in front of a downstream dependency. When that dependency starts failing repeatedly, the breaker trips to the Open state and immediately rejects new calls without waiting for a timeout. After a configurable cooldown it enters Half-Open, allows a small number of probe requests through, and resets to Closed only if the probes succeed. The implementation challenge is making every state transition atomic under concurrent load while keeping the timer logic correct.

```text
breaker/
  go.mod
  breaker.go
  breaker_test.go
  cmd/demo/main.go
```

## Concepts

### The Three-State Machine

A circuit breaker has exactly three states:

- Closed: calls pass through normally; failures are counted.
- Open: calls are rejected immediately with `ErrCircuitOpen`; no downstream traffic.
- Half-Open: a limited number of probe calls are allowed; a success resets to Closed, a failure returns to Open.

The transition rules are:

```
Closed  --[failures >= threshold]--> Open
Open    --[timeout elapsed]--------> Half-Open
Half-Open --[probe succeeds]-------> Closed
Half-Open --[probe fails]----------> Open
```

### Why Half-Open Exists

Without a Half-Open probe, you have two bad options: stay Open forever (never recover) or reset directly to Closed after a timeout (flood the still-recovering downstream with full traffic). Half-Open lets a single probe validate the downstream before reopening fully. It is the key insight of Fowler's original pattern.

### Mutex Discipline

Every field that participates in a state decision — `state`, `failures`, `lastOpen`, `halfOpenProbes` — must be read and written inside the same mutex. The common mistake is reading `state` outside the lock to decide whether to call the function, then acquiring the lock inside to record the result. The two-lock pattern creates a race: two goroutines can both see Closed, both call the downstream, and both record results with no visibility into each other's writes. The correct approach is a single `allowRequest` call under the lock that atomically reads the state and, if the timeout has elapsed in Open, transitions to Half-Open.

### Failure Classification

Not every error from the downstream should count toward the failure threshold. A canceled context (`context.Canceled`) means the caller gave up, not that the downstream is broken. A 429 Too Many Requests from an HTTP endpoint is a rate-limit, not a fault. The breaker should accept a `ShouldTrip` predicate so the caller controls what counts.

### Trade-offs

A breaker adds latency on the happy path (a mutex acquire per call). It also introduces a new failure mode: if `FailureThreshold` is too low or `Timeout` is too short, a healthy downstream gets cut off during a transient spike. Tune both values with production traffic data, not intuition.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/breaker/cmd/demo
cd ~/go-exercises/breaker
go mod init example.com/breaker
```

This is a library, not a program: there is no top-level `main`. Verify with `go test`.

### Exercise 1: Define the State Type and Sentinel Error

Create `breaker.go`:

```go
package breaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrOpen is returned by Execute when the circuit is Open and the call is
// rejected without reaching the downstream.
var ErrOpen = errors.New("circuit breaker open")

// State is the current operating mode of a CircuitBreaker.
type State int

const (
	// StateClosed means calls pass through normally.
	StateClosed State = iota
	// StateOpen means calls are rejected immediately.
	StateOpen
	// StateHalfOpen means a limited number of probes are allowed.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// ShouldTripFunc is a predicate that decides whether a non-nil error from the
// downstream counts as a failure for threshold purposes. Return false for
// errors that do not indicate a downstream fault (e.g. context.Canceled, 429).
type ShouldTripFunc func(err error) bool

// DefaultShouldTrip counts every non-nil error as a failure.
func DefaultShouldTrip(err error) bool {
	return err != nil
}

// Config holds the parameters for a CircuitBreaker.
type Config struct {
	// FailureThreshold is the number of consecutive failures in Closed state
	// that trips the breaker to Open. Default: 5.
	FailureThreshold int
	// SuccessThreshold is the number of consecutive successes in Half-Open
	// state required to reset to Closed. Default: 2.
	SuccessThreshold int
	// Timeout is how long the breaker stays Open before transitioning to
	// Half-Open. Default: 30s.
	Timeout time.Duration
	// ShouldTrip decides whether an error counts as a failure. Defaults to
	// DefaultShouldTrip (all non-nil errors count).
	ShouldTrip ShouldTripFunc
	// OnStateChange is called after every state transition. May be nil.
	OnStateChange func(from, to State)
}

func (c *Config) applyDefaults() {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.ShouldTrip == nil {
		c.ShouldTrip = DefaultShouldTrip
	}
}

// CircuitBreaker is a concurrent-safe three-state fault-isolation gate.
// The zero value is not usable; construct with New.
type CircuitBreaker struct {
	cfg       Config
	mu        sync.Mutex
	state     State
	failures  int
	successes int
	openedAt  time.Time
}

// New returns a CircuitBreaker configured with cfg.
func New(cfg Config) *CircuitBreaker {
	cfg.applyDefaults()
	return &CircuitBreaker{cfg: cfg}
}

// State returns the current state. Safe for concurrent use.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Failures returns the current consecutive failure count. Safe for concurrent use.
func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}

// allowRequest is called under cb.mu. It returns true if the call may proceed
// and, as a side-effect, transitions Open -> Half-Open when the timeout has
// elapsed.
func (cb *CircuitBreaker) allowRequest(now time.Time) bool {
	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		if now.Sub(cb.openedAt) >= cb.cfg.Timeout {
			cb.transition(StateHalfOpen)
			return true
		}
		return false
	case StateHalfOpen:
		return true
	default:
		return false
	}
}

func (cb *CircuitBreaker) transition(next State) {
	from := cb.state
	cb.state = next
	if next == StateOpen {
		cb.openedAt = time.Now()
	}
	if cb.cfg.OnStateChange != nil {
		cb.cfg.OnStateChange(from, next)
	}
}

// Execute calls fn if the circuit allows it. If the circuit is Open, Execute
// returns ErrOpen without calling fn. The error returned by fn is passed through
// to the caller; the breaker records a failure or success based on cfg.ShouldTrip.
func (cb *CircuitBreaker) Execute(fn func() error) error {
	cb.mu.Lock()
	allow := cb.allowRequest(time.Now())
	cb.mu.Unlock()

	if !allow {
		return fmt.Errorf("%w", ErrOpen)
	}

	err := fn()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.cfg.ShouldTrip(err) {
		cb.successes = 0
		cb.failures++
		switch cb.state {
		case StateClosed:
			if cb.failures >= cb.cfg.FailureThreshold {
				cb.transition(StateOpen)
				cb.failures = 0
			}
		case StateHalfOpen:
			cb.transition(StateOpen)
			cb.failures = 0
		}
	} else {
		cb.failures = 0
		cb.successes++
		if cb.state == StateHalfOpen && cb.successes >= cb.cfg.SuccessThreshold {
			cb.successes = 0
			cb.transition(StateClosed)
		}
	}

	return err
}
```

Defaults are applied inside `applyDefaults` so the zero `Config{}` is safe and the caller only sets what differs from the defaults.

### Exercise 2: Test the State Machine Contract

Create `breaker_test.go`:

```go
package breaker

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var errDownstream = errors.New("downstream unavailable")

func alwaysFail() error    { return errDownstream }
func alwaysSucceed() error { return nil }

func TestClosedPassesCalls(t *testing.T) {
	t.Parallel()

	cb := New(Config{FailureThreshold: 3})
	err := cb.Execute(alwaysSucceed)
	if err != nil {
		t.Fatalf("Execute on closed breaker returned %v", err)
	}
	if cb.State() != StateClosed {
		t.Fatalf("state = %s, want closed", cb.State())
	}
}

func TestTripsToOpenAfterThreshold(t *testing.T) {
	t.Parallel()

	const threshold = 3
	cb := New(Config{FailureThreshold: threshold})

	for i := 0; i < threshold; i++ {
		_ = cb.Execute(alwaysFail)
	}

	if cb.State() != StateOpen {
		t.Fatalf("state = %s after %d failures, want open", cb.State(), threshold)
	}
}

func TestOpenRejectsWithErrOpen(t *testing.T) {
	t.Parallel()

	cb := New(Config{FailureThreshold: 1})
	_ = cb.Execute(alwaysFail)

	err := cb.Execute(alwaysSucceed)
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}

func TestHalfOpenAfterTimeout(t *testing.T) {
	t.Parallel()

	cb := New(Config{
		FailureThreshold: 1,
		Timeout:          10 * time.Millisecond,
	})
	_ = cb.Execute(alwaysFail)

	time.Sleep(20 * time.Millisecond)

	// The first call after the timeout should be allowed (Half-Open probe).
	called := false
	err := cb.Execute(func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Execute after timeout: %v", err)
	}
	if !called {
		t.Fatal("probe function was not called in half-open")
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	t.Parallel()

	cb := New(Config{
		FailureThreshold: 1,
		Timeout:          10 * time.Millisecond,
	})
	_ = cb.Execute(alwaysFail)
	time.Sleep(20 * time.Millisecond)

	_ = cb.Execute(alwaysFail) // probe fails

	if cb.State() != StateOpen {
		t.Fatalf("state = %s after failed probe, want open", cb.State())
	}
}

func TestHalfOpenSuccessClosesBreaker(t *testing.T) {
	t.Parallel()

	cb := New(Config{
		FailureThreshold: 1,
		SuccessThreshold: 2,
		Timeout:          10 * time.Millisecond,
	})
	_ = cb.Execute(alwaysFail)
	time.Sleep(20 * time.Millisecond)

	// Two consecutive successes should close the breaker.
	for i := 0; i < 2; i++ {
		if err := cb.Execute(alwaysSucceed); err != nil {
			t.Fatalf("Execute #%d: %v", i+1, err)
		}
	}

	if cb.State() != StateClosed {
		t.Fatalf("state = %s after %d successes, want closed", cb.State(), 2)
	}
}

func TestShouldTripPredicateFiltersErrors(t *testing.T) {
	t.Parallel()

	// Only errors wrapping errDownstream count; other errors are ignored.
	cb := New(Config{
		FailureThreshold: 2,
		ShouldTrip: func(err error) bool {
			return errors.Is(err, errDownstream)
		},
	})

	ignoredErr := errors.New("caller canceled")
	// Three ignored errors must not trip the breaker.
	for i := 0; i < 3; i++ {
		_ = cb.Execute(func() error { return ignoredErr })
	}
	if cb.State() != StateClosed {
		t.Fatalf("state = %s after ignored errors, want closed", cb.State())
	}

	// Two counted failures should trip.
	for i := 0; i < 2; i++ {
		_ = cb.Execute(alwaysFail)
	}
	if cb.State() != StateOpen {
		t.Fatalf("state = %s, want open", cb.State())
	}
}

func TestOnStateChangeCallback(t *testing.T) {
	t.Parallel()

	var transitions []string
	cb := New(Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		Timeout:          10 * time.Millisecond,
		OnStateChange: func(from, to State) {
			transitions = append(transitions, fmt.Sprintf("%s->%s", from, to))
		},
	})

	_ = cb.Execute(alwaysFail) // closed -> open
	time.Sleep(20 * time.Millisecond)
	_ = cb.Execute(alwaysSucceed) // open -> half-open -> closed (probe)

	want := []string{"closed->open", "open->half-open", "half-open->closed"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i, tr := range transitions {
		if tr != want[i] {
			t.Errorf("transitions[%d] = %q, want %q", i, tr, want[i])
		}
	}
}

func TestTableDrivenThresholds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		threshold int
		failures  int
		wantOpen  bool
	}{
		{"below threshold", 5, 4, false},
		{"at threshold", 5, 5, true},
		{"above threshold", 5, 6, true},
		{"threshold of one", 1, 1, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cb := New(Config{FailureThreshold: tc.threshold})
			for i := 0; i < tc.failures; i++ {
				_ = cb.Execute(alwaysFail)
			}
			open := cb.State() == StateOpen
			if open != tc.wantOpen {
				t.Fatalf("after %d failures with threshold %d: open=%v, want %v",
					tc.failures, tc.threshold, open, tc.wantOpen)
			}
		})
	}
}

func ExampleCircuitBreaker_Execute() {
	cb := New(Config{FailureThreshold: 2, Timeout: time.Hour})

	// Two failures trip the breaker.
	for i := 0; i < 2; i++ {
		_ = cb.Execute(func() error { return errDownstream })
	}

	err := cb.Execute(func() error { return nil })
	fmt.Println(errors.Is(err, ErrOpen))
	// Output: true
}
```

Your turn: add `TestClosedSuccessResetsFailureCount` — call `Execute` with two failures, then one success, then verify that the failure count has reset to zero and the breaker is still Closed. Use `cb.Failures()` to read the count.

### Exercise 3: Build the Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/breaker"
)

func main() {
	// flaky is a test server that returns 500 until healed.
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			http.Error(w, "service unavailable", http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	transitions := []string{}
	cb := breaker.New(breaker.Config{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          200 * time.Millisecond,
		ShouldTrip: func(err error) bool {
			return err != nil
		},
		OnStateChange: func(from, to breaker.State) {
			msg := fmt.Sprintf("%s -> %s", from, to)
			transitions = append(transitions, msg)
			log.Printf("[breaker] %s", msg)
		},
	})

	doGet := func(label string) {
		err := cb.Execute(func() error {
			resp, err := http.Get(srv.URL + "/api")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 500 {
				return fmt.Errorf("server error %d", resp.StatusCode)
			}
			return nil
		})
		if errors.Is(err, breaker.ErrOpen) {
			fmt.Printf("[%s] rejected (circuit open)\n", label)
		} else if err != nil {
			fmt.Printf("[%s] error: %v | state: %s\n", label, err, cb.State())
		} else {
			fmt.Printf("[%s] OK | state: %s\n", label, cb.State())
		}
	}

	fmt.Println("=== phase 1: healthy ===")
	for i := 0; i < 3; i++ {
		doGet(fmt.Sprintf("ok-%d", i+1))
	}

	fmt.Println("\n=== phase 2: service fails, breaker trips ===")
	healthy = false
	for i := 0; i < 4; i++ {
		doGet(fmt.Sprintf("fail-%d", i+1))
	}

	fmt.Println("\n=== phase 3: circuit open, calls rejected ===")
	for i := 0; i < 2; i++ {
		doGet(fmt.Sprintf("rejected-%d", i+1))
	}

	fmt.Println("\n=== phase 4: timeout elapses, service recovers, probe succeeds ===")
	time.Sleep(250 * time.Millisecond)
	healthy = true
	for i := 0; i < 4; i++ {
		doGet(fmt.Sprintf("probe-%d", i+1))
		time.Sleep(10 * time.Millisecond)
	}

	fmt.Printf("\ntransitions: %v\n", transitions)
}
```

Run the demo:

```bash
go run ./cmd/demo
```

You should observe the breaker move through all three states: Closed, Open (after 3 failures), rejected calls, then Half-Open (after 200 ms), then Closed again (after 2 successful probes).

## Common Mistakes

### Reading State Outside the Lock Before Deciding to Call

Wrong: read `cb.state` without the mutex, decide to call the function, then acquire the mutex to record the result.

What happens: two goroutines both observe `StateClosed`, both call the downstream, and both increment `failures` independently. The transition check fires twice and the state machine may skip states or transition twice.

Fix: `allowRequest` acquires the mutex, reads and possibly mutates `state`, and returns a bool — all in one critical section. The caller holds no lock during the downstream call (that would serialize all traffic), but the decision and record phases each hold the lock separately.

### Resetting Failures on Any Success in Closed State

Wrong: decrement `failures` on every success in Closed state.

What happens: a service that alternates fail/succeed/fail/succeed never trips, even if the overall failure rate is 50%.

Fix: reset `failures` to zero on a success (not decrement). Consecutive-failure semantics mean a single success resets the run; this is the standard circuit breaker contract. If you need rate-based semantics, use a sliding window instead.

### Using the Circuit Breaker Timeout as a Request Timeout

Wrong: passing `cfg.Timeout` to `http.Client.Timeout`.

What happens: the breaker timeout (how long to wait before probing) is conflated with the per-request deadline. A 30-second breaker timeout does not mean requests should wait 30 seconds.

Fix: set `http.Client.Timeout` to a small value (e.g. 2 s) independently of `cfg.Timeout`.

### Not Wrapping ErrOpen with `%w`

Wrong: `return ErrOpen` directly from `Execute`.

What happens: the caller cannot use `errors.Is(err, ErrOpen)` after wrapping with context (e.g. `fmt.Errorf("payment: %w", cb.Execute(...))`).

Fix: `Execute` returns `fmt.Errorf("%w", ErrOpen)` so the error chain is preserved.

## Verification

From `~/go-exercises/breaker`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must produce no output and exit 0. `go test` is the verification; there is no eyeballed output to trust.

## Summary

- A circuit breaker is a three-state machine: Closed (pass), Open (reject), Half-Open (probe).
- Every field involved in state decisions must be read and written under the same mutex.
- `allowRequest` transitions Open to Half-Open atomically when the timeout has elapsed; it is the only place that reads the clock.
- A `ShouldTrip` predicate decouples error classification from state logic, making the breaker reusable across transport types.
- `OnStateChange` hooks enable observability without coupling the breaker to a specific logger or metric system.
- The `ErrOpen` sentinel must be wrapped with `%w` so callers can unwrap through context chains.

## What's Next

Next: [Retry with Exponential Backoff and Jitter](../10-retry-exponential-backoff-jitter/10-retry-exponential-backoff-jitter.md).

## Resources

- [Circuit Breaker (Martin Fowler)](https://martinfowler.com/bliki/CircuitBreaker.html) — original formulation of the three-state machine and the Half-Open rationale.
- [sync package](https://pkg.go.dev/sync) — `sync.Mutex` documentation; Go memory model guarantees for lock/unlock.
- [errors package](https://pkg.go.dev/errors) — `errors.Is`, `errors.As`, `fmt.Errorf("%w", ...)` for error chain inspection.
- [testing package — Example functions](https://pkg.go.dev/testing#hdr-Examples) — how `// Output:` comments make `Example` functions auto-verified under `go test`.
- [Go Code Review Comments: Error Strings](https://go.dev/wiki/CodeReviewComments#error-strings) — conventions the lesson follows for error variable naming and wrapping.
