# Exercise 16: Circuit Breaker State Transitions With Tagless Switch

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every service that calls a flaky downstream dependency eventually needs a
circuit breaker: stop hammering a dependency that is already failing, and
find out it has recovered without a human flipping a switch. This module
builds the classic three-state breaker — Closed (healthy) -> Open (failing
fast) -> HalfOpen (testing recovery) -> Closed — as a pair of tagless
switches over predicates: has the failure count crossed the threshold, has
the open-state timeout elapsed, did the recovery probe succeed. It is
self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
circuitbreaker/            independent module: example.com/circuit-breaker-state-machine
  go.mod                    go 1.24
  circuitbreaker.go          package circuitbreaker; State; Breaker; New(threshold, timeout) *Breaker; Allow, RecordSuccess, RecordFailure
  cmd/demo/main.go           runnable demo driving a breaker through a full open/half-open/closed cycle
  circuitbreaker_test.go     boundary-condition subtests plus a concurrent-access subtest
```

- Implement: a `Breaker` whose `Allow`, `RecordSuccess`, and `RecordFailure` methods use tagless switches to decide state transitions, guarded by a `sync.Mutex` so the whole thing is safe under concurrent calls.
- Test: subtests for the threshold boundary (one below vs. exactly at), the timeout boundary (before vs. after elapsed), both half-open outcomes (probe succeeds, probe fails), and a concurrency subtest that hammers the breaker from many goroutines.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/circuitbreaker/cmd/demo
cd ~/go-exercises/circuitbreaker
go mod init example.com/circuit-breaker-state-machine
go mod edit -go=1.24
```

### Why two tagless switches, not one big one

A circuit breaker has two separate decision points, and conflating them into
one switch is where most home-grown implementations go wrong. `Allow` decides
*whether a call is permitted right now* — that's where the Open-to-HalfOpen
transition belongs, because "has the timeout elapsed" is only worth checking
when someone is actually asking permission; a background timer that flips the
state on its own invites a race between the timer and a concurrent `Allow`
call. `RecordFailure` and `RecordSuccess` decide *what today's outcome means
for tomorrow's state* — a failure during `HalfOpen` re-opens immediately
regardless of the failure threshold, because a failed recovery probe is
disqualifying on its own; a failure during `Closed` only trips the breaker
once the accumulated count reaches the threshold. Both of `Allow` and
`RecordFailure` are tagless switches because the tag would have to be
`state == Open && timeoutElapsed`, a boolean, and once the condition is a
compound boolean, the tagless form is the honest way to write it rather than
faking it with `switch true`.

The mutex is not incidental. A breaker that reads `state` in one goroutine
while another goroutine is mid-write to `failures` is a data race by
definition — Go's race detector calls this out immediately (it's why the
concurrency test below exists), and in production a breaker with a data race
can flip open and closed inconsistently under load, which is worse than no
breaker at all.

Create `circuitbreaker.go`:

```go
// Package circuitbreaker implements a three-state circuit breaker (Closed,
// Open, HalfOpen) whose transitions are driven entirely by tagless switches
// over predicates: has the failure count crossed the threshold, has the
// open-state timeout elapsed, did the half-open probe succeed.
package circuitbreaker

import (
	"sync"
	"time"
)

// State is one of the three breaker states.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

// String renders a State for logs and test failure messages.
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker guards a call path with failure-threshold tripping and a timed
// recovery probe. All exported methods are safe for concurrent use.
type Breaker struct {
	mu               sync.Mutex
	state            State
	failures         int
	failureThreshold int
	resetTimeout     time.Duration
	openedAt         time.Time
	now              func() time.Time
}

// New builds a Breaker that opens after failureThreshold consecutive
// failures and waits resetTimeout before allowing a single half-open probe.
func New(failureThreshold int, resetTimeout time.Duration) *Breaker {
	return &Breaker{
		failureThreshold: failureThreshold,
		resetTimeout:     resetTimeout,
		now:              time.Now,
	}
}

// Allow reports whether the caller should attempt the guarded call right
// now. It also performs the Open -> HalfOpen transition: the only way that
// transition happens is a caller asking permission after the timeout has
// elapsed, which is why the switch below lives here rather than in a
// background timer.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch {
	case b.state == Open && b.now().Sub(b.openedAt) >= b.resetTimeout:
		b.state = HalfOpen
		return true
	case b.state == Open:
		return false
	default: // Closed or HalfOpen: let the call through
		return true
	}
}

// RecordSuccess reports a successful call. In HalfOpen this is the "probe
// succeeded" predicate that closes the breaker and clears its failure
// history; in Closed it simply resets the streak so unrelated, scattered
// failures don't accumulate toward the threshold.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		b.state = Closed
		b.failures = 0
	case Closed:
		b.failures = 0
	}
}

// RecordFailure reports a failed call. A failure during HalfOpen means the
// probe itself failed, so it re-opens immediately regardless of the
// threshold; a failure during Closed only trips the breaker once the
// accumulated count reaches failureThreshold.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	switch {
	case b.state == HalfOpen:
		b.state = Open
		b.openedAt = b.now()
		b.failures = 0
	case b.state == Closed && b.failures >= b.failureThreshold:
		b.state = Open
		b.openedAt = b.now()
	}
}

// State returns the breaker's current state, for logging and tests.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	circuitbreaker "example.com/circuit-breaker-state-machine"
)

func main() {
	b := circuitbreaker.New(3, 50*time.Millisecond)

	fmt.Println("start:", b.State())

	for i := 1; i <= 3; i++ {
		b.RecordFailure()
		fmt.Printf("after failure %d: %s\n", i, b.State())
	}

	fmt.Println("allow while open:", b.Allow())

	time.Sleep(60 * time.Millisecond)
	fmt.Println("allow after timeout:", b.Allow(), "state:", b.State())

	b.RecordFailure()
	fmt.Println("probe failed, state:", b.State())

	time.Sleep(60 * time.Millisecond)
	fmt.Println("allow after second timeout:", b.Allow(), "state:", b.State())

	b.RecordSuccess()
	fmt.Println("probe succeeded, state:", b.State())
}
```

Run `go run ./cmd/demo`, expected output:

```
start: closed
after failure 1: closed
after failure 2: closed
after failure 3: open
allow while open: false
allow after timeout: true state: half-open
probe failed, state: open
allow after second timeout: true state: half-open
probe succeeded, state: closed
```

### Tests

`TestBreakerBoundaryConditions` runs one subtest per edge: one failure below
threshold stays closed, exactly at threshold opens, `Allow` before the
timeout stays false while `Allow` after the timeout flips to `HalfOpen`, a
half-open success closes and clears the failure streak, and a half-open
failure re-opens immediately regardless of the threshold. The timeout
subtests inject a fake clock (`b.now`) instead of sleeping, so they're exact
and instant. `TestBreakerConcurrentAccess` fires fifty goroutines at the
breaker at once; because the assertion can't predict which state a
race-free but unordered mix of successes and failures lands on, it only
asserts that the breaker ends in one of the three valid states — the real
value of this subtest is that it runs clean when built with the race
detector.

Create `circuitbreaker_test.go`:

```go
package circuitbreaker

import (
	"sync"
	"testing"
	"time"
)

func TestBreakerBoundaryConditions(t *testing.T) {
	t.Parallel()

	t.Run("stays closed one failure below threshold", func(t *testing.T) {
		b := New(3, time.Second)
		b.RecordFailure()
		b.RecordFailure()
		if got := b.State(); got != Closed {
			t.Fatalf("state = %s, want closed", got)
		}
	})

	t.Run("opens exactly at threshold", func(t *testing.T) {
		b := New(3, time.Second)
		b.RecordFailure()
		b.RecordFailure()
		b.RecordFailure()
		if got := b.State(); got != Open {
			t.Fatalf("state = %s, want open", got)
		}
		if b.Allow() {
			t.Fatal("Allow() = true while open and timeout has not elapsed")
		}
	})

	t.Run("half-open only after timeout elapses", func(t *testing.T) {
		fakeNow := time.Now()
		b := New(1, 10*time.Millisecond)
		b.now = func() time.Time { return fakeNow }
		b.RecordFailure()
		if got := b.State(); got != Open {
			t.Fatalf("state = %s, want open", got)
		}
		if b.Allow() {
			t.Fatal("Allow() = true before timeout elapsed")
		}
		fakeNow = fakeNow.Add(10 * time.Millisecond)
		if !b.Allow() {
			t.Fatal("Allow() = false after timeout elapsed, want true (half-open probe)")
		}
		if got := b.State(); got != HalfOpen {
			t.Fatalf("state = %s, want half-open", got)
		}
	})

	t.Run("half-open probe success closes and clears failures", func(t *testing.T) {
		fakeNow := time.Now()
		b := New(1, time.Millisecond)
		b.now = func() time.Time { return fakeNow }
		b.RecordFailure()
		fakeNow = fakeNow.Add(time.Millisecond)
		b.Allow()
		b.RecordSuccess()
		if got := b.State(); got != Closed {
			t.Fatalf("state = %s, want closed", got)
		}
		if b.failures != 0 {
			t.Fatalf("failures = %d, want 0 (streak was cleared on success)", b.failures)
		}
	})

	t.Run("half-open probe failure reopens immediately", func(t *testing.T) {
		fakeNow := time.Now()
		b := New(5, time.Millisecond)
		b.now = func() time.Time { return fakeNow }
		for i := 0; i < 5; i++ {
			b.RecordFailure()
		}
		fakeNow = fakeNow.Add(time.Millisecond)
		b.Allow()
		if got := b.State(); got != HalfOpen {
			t.Fatalf("state = %s, want half-open", got)
		}
		b.RecordFailure()
		if got := b.State(); got != Open {
			t.Fatalf("state = %s, want open (probe failed, threshold irrelevant)", got)
		}
	})
}

// TestBreakerConcurrentAccess drives many goroutines through Allow,
// RecordSuccess, and RecordFailure at once. The assertion isn't which state
// the breaker lands in — with concurrent, unordered results that outcome
// isn't deterministic — it's that every method call is race-free (the mutex
// serializes state reads and writes) and the breaker ends in one of the
// three valid states, never a torn or impossible combination.
func TestBreakerConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := New(10, 5*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if b.Allow() {
				if n%3 == 0 {
					b.RecordFailure()
				} else {
					b.RecordSuccess()
				}
			}
		}(i)
	}
	wg.Wait()

	switch got := b.State(); got {
	case Closed, Open, HalfOpen:
		// any of these is a valid, consistent end state
	default:
		t.Fatalf("state = %v, want one of Closed, Open, HalfOpen", got)
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The breaker is correct when `Allow` is the only place the Open-to-HalfOpen
transition happens (never a background goroutine racing against it), when a
half-open failure re-opens unconditionally instead of being subject to the
threshold again, and when every state read or write goes through the mutex
so the race detector has nothing to report. Carry this forward: whenever a
transition depends on a compound condition — state equality *and* elapsed
time, state equality *and* a counter comparison — reach for the tagless
switch instead of nesting `if` statements inside a `switch state`, and
whenever a type has concurrent callers, write a test that actually launches
goroutines against it rather than trusting the mutex by inspection.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding shared state across goroutines.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the pattern this exercise implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-permission-level-cascade.md](15-permission-level-cascade.md) | Next: [17-connection-pool-health-classifier.md](17-connection-pool-health-classifier.md)
