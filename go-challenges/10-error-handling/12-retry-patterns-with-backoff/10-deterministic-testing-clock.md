# Exercise 10: Testing Retry Timing Without Real Sleeps — Injectable Clock and Seeded RNG

Every module so far dodged real sleeps with small durations or an injected wait
function. This final module builds the general solution: a `Clock` abstraction with a
real implementation and a controllable fake that advances virtual time on demand, plus
a seeded `*rand.Rand`, so a retry client's exact backoff sequence is asserted in
microseconds with zero flakes.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
retryclock/                independent module: example.com/retryclock
  go.mod                   go 1.26
  clock.go                 Clock interface; RealClock; FakeClock
  client.go                Client.Do depending on Clock + seeded RNG
  cmd/
    demo/
      main.go              runnable demo with the real clock
  retryclock_test.go       tests: exact sleep sequence via fake; reproducible jitter
```

Files: `clock.go`, `client.go`, `cmd/demo/main.go`, `retryclock_test.go`.
Implement: a `Clock` (`Now`, `After`, `Sleep`), a `RealClock`, a `FakeClock` that advances virtual time and fires pending timers, and a `Client.Do` that sleeps through the injected `Clock` and jitters through an injected `*math/rand/v2.Rand`.
Test: with the fake clock, assert `Do` slept the exact backoff sequence with no wall-clock delay; assert a fixed seed reproduces identical jittered delays; compile-time `var _ Clock` for both implementations; advancing the fake unblocks a pending timer.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/10-error-handling/12-retry-patterns-with-backoff/10-deterministic-testing-clock/cmd/demo
cd go-solutions/10-error-handling/12-retry-patterns-with-backoff/10-deterministic-testing-clock
go mod edit -go=1.26
```

### Injecting the two sources of nondeterminism

Retry timing has exactly two nondeterministic inputs: *the clock* (how long a sleep
actually takes) and *the RNG* (how much jitter is added). Inject both and the whole
loop becomes a pure function of its inputs, testable to the nanosecond.

**The `Clock` interface** abstracts the three time operations the retry loop uses:
`Now() time.Time`, `Sleep(ctx, d)` (wait `d` or until ctx ends), and — the tricky one
— a way to wait on a timer inside a `select`. Faking `*time.Timer` directly is not
possible (its `C` channel is fed by the runtime), so the interface exposes
`After(d) <-chan time.Time` instead: the client selects on the returned channel and
`ctx.Done()`, and the fake controls when that channel fires. `RealClock` implements
these with `time.Now`, a real timer, and `time.After`. `FakeClock` implements them
over a virtual clock that only moves when the test tells it to.

**The `FakeClock`** holds a virtual `now` and a list of pending waiters, each a
`(deadline, channel)` pair. `After(d)` registers a waiter at `now + d` and returns its
channel *without blocking*. `Advance(d)` moves `now` forward and fires (closes/sends
on) every waiter whose deadline has passed. Because advancing is explicit, a test can
step virtual time exactly to a backoff boundary and observe the client wake — no real
time passes, and there is no scheduler slack to flake on. The fake records every
requested sleep duration so a test can assert the *exact* backoff sequence the client
attempted.

The concurrency subtlety: the client runs the retry loop on one goroutine while the
test advances the clock on another. `FakeClock` guards its `now` and waiter list with
a mutex. The test pattern is: start the client in a goroutine, wait until it has
registered its next timer (the fake exposes a way to observe pending waiters), then
`Advance` past it. To keep this module's tests simple and robust, `Advance` fires all
due waiters and the client's `Sleep` blocks on the fake's channel; the test advances
in a loop until the client finishes.

**The seeded RNG** is the second injection. The client takes a `*math/rand/v2.Rand`;
production seeds it nondeterministically, tests seed it with `rand.NewPCG(seed, seed)`
so the jitter sequence is identical on every run. Two runs with the same seed produce
bit-identical delays — that is what makes asserting the sequence possible.

Create `clock.go`:

```go
package retryclock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock abstracts the time operations the retry loop needs, so tests can supply a
// controllable fake instead of the real wall clock.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d of clock-time has elapsed.
	After(d time.Duration) <-chan time.Time
	// Sleep blocks for d of clock-time or until ctx ends, whichever first.
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the production Clock backed by the time package.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// FakeClock is a controllable Clock whose time only advances on Advance.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	slept   []time.Duration // every duration passed to After/Sleep, in order
}

type waiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFakeClock starts a fake clock at the given instant.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After registers a waiter at now+d and returns its channel without blocking.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, &waiter{deadline: c.now.Add(d), ch: ch})
	return ch
}

// Sleep blocks until the returned After channel fires or ctx ends.
func (c *FakeClock) Sleep(ctx context.Context, d time.Duration) error {
	ch := c.After(d)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// Advance moves virtual time forward by d and fires every due waiter.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.deadline.After(c.now) {
			w.ch <- c.now
		} else {
			remaining = append(remaining, w)
		}
	}
	c.waiters = remaining
}

// Pending reports how many waiters are currently registered.
func (c *FakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Slept returns the sorted list of durations the client asked to sleep, for
// asserting the exact backoff sequence.
func (c *FakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.slept))
	copy(out, c.slept)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Compile-time proof both implementations satisfy Clock.
var (
	_ Clock = RealClock{}
	_ Clock = (*FakeClock)(nil)
)
```

Create `client.go`:

```go
package retryclock

import (
	"context"
	"math/rand/v2"
	"time"
)

// Op is the retryable unit of work.
type Op func(ctx context.Context) error

// Client retries an Op, taking all time and randomness through injected
// dependencies so its behavior is fully deterministic under test.
type Client struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Factor      float64
	Jitter      float64
	Clock       Clock
	Rand        *rand.Rand // nil => no jitter randomness (uses center delay)
	Retryable   func(error) bool
}

func (c *Client) retryable(err error) bool {
	if c.Retryable == nil {
		return true
	}
	return c.Retryable(err)
}

// Backoff computes the delay for an attempt using the injected RNG for jitter.
func (c *Client) Backoff(attempt int) time.Duration {
	d := float64(c.BaseDelay)
	for range attempt {
		d *= c.Factor
	}
	if d > float64(c.MaxDelay) {
		d = float64(c.MaxDelay)
	}
	if c.Jitter > 0 && c.Rand != nil {
		spread := d * c.Jitter
		d = d - spread + c.Rand.Float64()*2*spread
	}
	return time.Duration(d)
}

// Do runs op up to MaxAttempts times, sleeping the computed backoff through the
// injected Clock so tests advance virtual time instead of waiting.
func (c *Client) Do(ctx context.Context, op Op) error {
	var lastErr error
	for attempt := range c.MaxAttempts {
		err := op(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !c.retryable(err) {
			return err
		}
		if attempt == c.MaxAttempts-1 {
			break
		}
		if err := c.Clock.Sleep(ctx, c.Backoff(attempt)); err != nil {
			return err
		}
	}
	return lastErr
}
```

### The runnable demo

The demo uses the *real* clock so you can watch actual (short) backoffs, then prints
that it succeeded on attempt 3.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/retryclock"
)

func main() {
	client := &retryclock.Client{
		MaxAttempts: 5,
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
		Factor:      2.0,
		Jitter:      0,
		Clock:       retryclock.RealClock{},
		Retryable:   func(error) bool { return true },
	}

	attempt := 0
	start := time.Now()
	err := client.Do(context.Background(), func(context.Context) error {
		attempt++
		if attempt < 3 {
			return errors.New("transient")
		}
		return nil
	})
	fmt.Printf("succeeded on attempt %d, err=%v\n", attempt, err)
	fmt.Printf("real backoff elapsed >= 15ms: %v\n", time.Since(start) >= 15*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
succeeded on attempt 3, err=<nil>
real backoff elapsed >= 15ms: true
```

### Tests

`TestExactBackoffSequenceNoWallClock` runs `Do` with the fake clock in a goroutine,
advances virtual time to release each backoff, and asserts the fake recorded the exact
sleep sequence (`5ms, 10ms, 20ms`) with the whole test finishing in microseconds.
`TestSeededJitterIsReproducible` runs two clients with the same PCG seed and asserts
identical jittered delays. `TestAdvanceUnblocksTimer` proves a pending fake timer is
released by `Advance`. The compile-time `var _ Clock` assertions in `clock.go` prove
both implementations satisfy the interface.

Create `retryclock_test.go`:

```go
package retryclock

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

var errTransient = errors.New("transient")

func TestExactBackoffSequenceNoWallClock(t *testing.T) {
	t.Parallel()
	fake := NewFakeClock(time.Unix(0, 0))
	client := &Client{
		MaxAttempts: 4,
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    time.Second,
		Factor:      2.0,
		Jitter:      0,
		Clock:       fake,
		Retryable:   func(error) bool { return true },
	}

	done := make(chan error, 1)
	go func() {
		done <- client.Do(context.Background(), func(context.Context) error {
			return errTransient // always fails => uses all attempts
		})
	}()

	// Release each of the 3 backoffs (4 attempts => 3 sleeps) by advancing time.
	for range 3 {
		waitForPending(t, fake, 1)
		fake.Advance(time.Second) // enough to fire the next waiter
	}

	if err := <-done; !errors.Is(err, errTransient) {
		t.Fatalf("Do err = %v, want errTransient", err)
	}
	got := fake.Slept()
	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("slept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sleep[%d] = %v, want %v (full sequence %v)", i, got[i], want[i], got)
		}
	}
}

func TestSeededJitterIsReproducible(t *testing.T) {
	t.Parallel()
	newClient := func() *Client {
		return &Client{
			BaseDelay: 100 * time.Millisecond,
			MaxDelay:  10 * time.Second,
			Factor:    2.0,
			Jitter:    0.2,
			Rand:      rand.New(rand.NewPCG(99, 99)),
		}
	}
	a, b := newClient(), newClient()
	for attempt := range 6 {
		da, db := a.Backoff(attempt), b.Backoff(attempt)
		if da != db {
			t.Fatalf("attempt %d: same seed diverged %v vs %v", attempt, da, db)
		}
	}
}

func TestAdvanceUnblocksTimer(t *testing.T) {
	t.Parallel()
	fake := NewFakeClock(time.Unix(0, 0))
	ch := fake.After(100 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("timer fired before Advance")
	default:
	}
	fake.Advance(100 * time.Millisecond)
	select {
	case <-ch:
		// released
	case <-time.After(time.Second):
		t.Fatal("timer did not fire after Advance")
	}
}

func TestContextCancelStopsSleep(t *testing.T) {
	t.Parallel()
	fake := NewFakeClock(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fake.Sleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep err = %v, want context.Canceled", err)
	}
}

// waitForPending spins (cheaply) until the fake has n pending waiters, so the test
// advances time only after the client has registered its next timer.
func waitForPending(t *testing.T, fake *FakeClock, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for fake.Pending() < n {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for client to register a timer")
		}
		time.Sleep(time.Millisecond)
	}
}
```

## Review

The clock injection is correct when the fake-clock test asserts the exact backoff
sequence (`5ms, 10ms, 20ms`) and finishes in microseconds — proving no wall-clock time
passed while still verifying real timing logic. The seeded RNG is correct when two
clients with the same PCG seed produce bit-identical jittered delays, which is what
makes the sequence assertable at all. The compile-time `var _ Clock` lines prove the
real and fake clocks are interchangeable. The mistake this whole design prevents:
testing retries with real `time.Sleep`, giving slow, flaky tests that cannot assert
the sequence. Run `go test -race`; the fake clock guards its state with a mutex, so
the client goroutine and the advancing test goroutine never race.

## Resources

- [`time#Timer`](https://pkg.go.dev/time#Timer) — the real timer `RealClock` wraps.
- [`math/rand/v2#NewPCG`](https://pkg.go.dev/math/rand/v2#NewPCG) — the seedable source for reproducible jitter.
- [`context`](https://pkg.go.dev/context) — cancellation both clock implementations honor.
- [testing/synctest](https://pkg.go.dev/testing/synctest) — Go 1.25's built-in virtual clock, an alternative to hand-rolled clock injection.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-retry-observability.md](09-retry-observability.md) | Next: [../13-designing-an-error-hierarchy/00-concepts.md](../13-designing-an-error-hierarchy/00-concepts.md)
