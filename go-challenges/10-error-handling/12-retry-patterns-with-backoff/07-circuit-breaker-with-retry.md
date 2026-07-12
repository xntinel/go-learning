# Exercise 7: Circuit Breaker — Stop Retrying a Dependency That Is Down

Retries handle brief blips; they are exactly the wrong tool for a sustained outage,
where they deliver multiples of normal load to a service that most needs to be left
alone. A circuit breaker is the complementary guardrail: after enough consecutive
failures it trips open and *fast-fails* every call without even invoking the
operation, so a hard-down dependency receives near-zero traffic and gets room to
recover. This module builds a three-state breaker composed with a retry call.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
breaker/                   independent module: example.com/breaker
  go.mod                   go 1.26
  breaker.go               Breaker: closed/open/half-open, injected clock
  cmd/
    demo/
      main.go              runnable demo: trips open, cools down, half-open probe
  breaker_test.go          tests: opens after N, fast-fails, half-open probe, -race
```

Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
Implement: a `Breaker` with `Do(ctx, op)` that opens after `Threshold` consecutive failures, returns `ErrOpen` without calling `op` while open, moves to half-open after `Cooldown`, and lets one probe decide close/re-open. Clock is injected (`now func() time.Time`).
Test: N consecutive failures open it and freeze the op call count; advancing past cooldown allows exactly one probe; probe success closes, probe failure re-opens; concurrency under `-race`.
Verify: `go test -count=1 -race ./...`

```bash
go mod edit -go=1.26
```

### Three states and the transitions between them

A circuit breaker is a small state machine guarding a dependency:

- **Closed** is the normal state: calls pass through to the operation. A counter
  tracks *consecutive* failures. A success resets it to zero; reaching `Threshold`
  consecutive failures trips the breaker to **open**.

- **Open** is the protective state: every call *fast-fails* with `ErrOpen` without
  invoking the operation at all. This is the entire point — a down dependency stops
  receiving traffic, including retry traffic. The breaker records *when* it opened;
  after `Cooldown` elapses it becomes eligible to test recovery.

- **Half-open** is the probing state: the breaker lets a *single* call through to see
  whether the dependency has recovered. If that probe succeeds, the breaker closes
  and normal traffic resumes. If it fails, the breaker re-opens and the cooldown
  restarts. Half-open is deliberately stingy: it admits one probe, not a flood,
  because a dependency that just came back must not be immediately re-buried under
  the backlog.

The transition from open to half-open is *time-based* and evaluated lazily inside
`Do`: when a call arrives and the state is open, the breaker checks whether
`now - openedAt >= Cooldown`; if so it flips to half-open and admits the caller as
the probe. This is why the clock must be *injected*: a test needs to advance time
past the cooldown without waiting for real seconds. `Breaker` holds a
`now func() time.Time` defaulting to `time.Now`; the test swaps in a controllable
one.

The concurrency contract matters. `Do` may be called from many goroutines; all state
transitions happen under a single `sync.Mutex`. The subtle rule in half-open: only
*one* goroutine may be the probe. The breaker admits the first caller that finds the
state half-open (or that triggers the open-to-half-open flip) and makes every other
concurrent caller fast-fail with `ErrOpen` until the probe resolves. Without this,
"half-open" would admit a herd instead of a single probe.

Create `breaker.go`:

```go
package breaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned when the breaker is open (or half-open with a probe already
// in flight) and the call is fast-failed without invoking the operation.
var ErrOpen = errors.New("circuit breaker is open")

type state int

const (
	closedState state = iota
	openState
	halfOpenState
)

// Op is the guarded operation.
type Op func(ctx context.Context) error

// Breaker is a three-state circuit breaker.
type Breaker struct {
	Threshold int           // consecutive failures that trip it open
	Cooldown  time.Duration // time open before a probe is allowed
	Now       func() time.Time

	mu            sync.Mutex
	state         state
	failures      int
	openedAt      time.Time
	probeInFlight bool
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Do runs op through the breaker. It returns ErrOpen without calling op when the
// breaker is protecting the dependency.
func (b *Breaker) Do(ctx context.Context, op Op) error {
	if err := b.beforeCall(); err != nil {
		return err
	}
	err := op(ctx)
	b.afterCall(err)
	return err
}

// beforeCall decides whether this call may proceed and updates state accordingly.
func (b *Breaker) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case openState:
		if b.now().Sub(b.openedAt) >= b.Cooldown {
			// Cooldown elapsed: promote to half-open and admit this caller as probe.
			b.state = halfOpenState
			b.probeInFlight = true
			return nil
		}
		return ErrOpen
	case halfOpenState:
		if b.probeInFlight {
			return ErrOpen // a probe is already testing recovery
		}
		b.probeInFlight = true
		return nil
	default: // closed
		return nil
	}
}

// afterCall records the outcome and performs any state transition.
func (b *Breaker) afterCall(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case halfOpenState:
		b.probeInFlight = false
		if err != nil {
			// Probe failed: re-open and restart the cooldown.
			b.state = openState
			b.openedAt = b.now()
			b.failures = b.Threshold
			return
		}
		// Probe succeeded: close and reset.
		b.state = closedState
		b.failures = 0
	default: // closed
		if err != nil {
			b.failures++
			if b.failures >= b.Threshold {
				b.state = openState
				b.openedAt = b.now()
			}
			return
		}
		b.failures = 0
	}
}

// State returns a human-readable state name, for observability.
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case openState:
		return "open"
	case halfOpenState:
		return "half-open"
	default:
		return "closed"
	}
}
```

### The runnable demo

The demo uses a controllable clock so the cooldown is instant. It drives three
failures to trip the breaker open, shows a fast-failed call, advances the clock past
the cooldown, and lets a successful probe close it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/breaker"
)

func main() {
	now := time.Now()
	b := &breaker.Breaker{
		Threshold: 3,
		Cooldown:  30 * time.Second,
		Now:       func() time.Time { return now },
	}
	ctx := context.Background()
	fail := func(context.Context) error { return errors.New("dependency down") }
	ok := func(context.Context) error { return nil }

	for i := range 3 {
		_ = b.Do(ctx, fail)
		fmt.Printf("after failure %d: state=%s\n", i+1, b.State())
	}

	var opCalled bool
	err := b.Do(ctx, func(context.Context) error { opCalled = true; return nil })
	fmt.Printf("while open: err=%v op-invoked=%v\n", err, opCalled)

	now = now.Add(31 * time.Second) // advance past cooldown
	err = b.Do(ctx, ok)
	fmt.Printf("after cooldown probe: err=%v state=%s\n", err, b.State())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after failure 1: state=closed
after failure 2: state=closed
after failure 3: state=open
while open: err=circuit breaker is open op-invoked=false
after cooldown probe: err=<nil> state=closed
```

### Tests

The tests use an injected clock so cooldown is deterministic. `TestOpensAndFastFails`
drives `Threshold` failures, asserts the state is open, then asserts a subsequent
call returns `ErrOpen` *without* invoking the op (the call counter is frozen).
`TestHalfOpenProbe` advances past the cooldown and asserts exactly one probe is
admitted, that a successful probe closes the breaker, and that a failing probe
re-opens it. `TestConcurrent` hammers the breaker from many goroutines under `-race`.

Create `breaker_test.go`:

```go
package breaker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errDown = errors.New("down")

func TestOpensAndFastFails(t *testing.T) {
	t.Parallel()
	b := &Breaker{Threshold: 3, Cooldown: time.Minute, Now: time.Now}
	ctx := context.Background()

	var calls atomic.Int32
	fail := func(context.Context) error { calls.Add(1); return errDown }

	for range 3 {
		_ = b.Do(ctx, fail)
	}
	if b.State() != "open" {
		t.Fatalf("state = %s, want open", b.State())
	}
	callsBefore := calls.Load()

	err := b.Do(ctx, fail)
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
	if calls.Load() != callsBefore {
		t.Fatalf("op was invoked while open (calls %d -> %d)", callsBefore, calls.Load())
	}
}

func TestHalfOpenProbeSuccessCloses(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := &Breaker{Threshold: 2, Cooldown: 30 * time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()

	for range 2 {
		_ = b.Do(ctx, func(context.Context) error { return errDown })
	}
	if b.State() != "open" {
		t.Fatalf("state = %s, want open", b.State())
	}

	now = now.Add(31 * time.Second) // past cooldown
	err := b.Do(ctx, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("probe err = %v, want nil", err)
	}
	if b.State() != "closed" {
		t.Fatalf("state = %s after successful probe, want closed", b.State())
	}
}

func TestHalfOpenProbeFailureReopens(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := &Breaker{Threshold: 2, Cooldown: 30 * time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()

	for range 2 {
		_ = b.Do(ctx, func(context.Context) error { return errDown })
	}
	now = now.Add(31 * time.Second)
	err := b.Do(ctx, func(context.Context) error { return errDown })
	if !errors.Is(err, errDown) {
		t.Fatalf("probe err = %v, want errDown", err)
	}
	if b.State() != "open" {
		t.Fatalf("state = %s after failed probe, want open", b.State())
	}
}

func TestOnlyOneProbeInHalfOpen(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := &Breaker{Threshold: 1, Cooldown: 10 * time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()

	_ = b.Do(ctx, func(context.Context) error { return errDown }) // opens (threshold 1)
	now = now.Add(11 * time.Second)

	// Block the probe so we can prove a second concurrent caller is rejected.
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- b.Do(ctx, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	// While the probe is in flight, a second caller must be fast-failed.
	if err := b.Do(ctx, func(context.Context) error { return nil }); !errors.Is(err, ErrOpen) {
		t.Fatalf("second caller err = %v, want ErrOpen (only one probe)", err)
	}
	close(release)

	// The done channel synchronizes the probe goroutine's completion.
	if probeErr := <-done; probeErr != nil {
		t.Fatalf("probe err = %v, want nil", probeErr)
	}
	if b.State() != "closed" {
		t.Fatalf("state = %s after successful probe, want closed", b.State())
	}
}

func TestConcurrent(t *testing.T) {
	t.Parallel()
	b := &Breaker{Threshold: 5, Cooldown: time.Millisecond, Now: time.Now}
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Do(ctx, func(context.Context) error {
				if i%2 == 0 {
					return errDown
				}
				return nil
			})
		}()
	}
	wg.Wait()
	_ = b.State() // must not race
}
```

## Review

The breaker is correct when open truly means zero load: a call while open returns
`ErrOpen` and the operation's call counter does not advance. The half-open contract
is the subtle part — exactly one probe is admitted, its success closes the breaker
and its failure re-opens it with a fresh cooldown, and concurrent callers during a
probe are fast-failed rather than piling on. The injected clock is what makes the
cooldown testable without real sleeps. The mistake this design prevents: pairing
aggressive retries with no breaker, so a sustained outage receives multiples of
normal load and becomes a self-sustaining metastable failure. Run `go test -race`;
all state lives under one mutex, so the concurrency test must be clean.

## Resources

- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the canonical description of the three states.
- [`sync#Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the breaker's state.
- [Google SRE Book: Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/) — why breakers stop cascades.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-retryable-http-transport.md](06-retryable-http-transport.md) | Next: [08-retry-budget-token-bucket.md](08-retry-budget-token-bucket.md)
