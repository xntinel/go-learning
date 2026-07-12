# Exercise 5: Resilience Decorators — Retry, Circuit Breaker, Timeout

Production service calls fail in three different ways that want three different responses: a transient blip wants a retry with backoff, a dependency that is down wants a circuit breaker that stops hammering it, and a call that hangs wants a context timeout. Each is a decorator over the same generic `Op[T]` signature, so they compose into one resilient call. This exercise builds all three — and proves, under `go test -race`, that the breaker opens after N consecutive failures and half-opens only after a cooldown, using an injectable clock so the time-based state machine is deterministic instead of dependent on `time.Sleep`.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
resilience.go        Op[T], Clock, ErrTransient/ErrCircuitOpen, IsRetryable,
                     Backoff, Sleeper/SleepWithContext, WithTimeout[T],
                     WithRetry[T], breaker[T] state machine, WithBreaker[T]
cmd/
  demo/
    main.go          compose retry+breaker+timeout; show the breaker stopping
                     a retry storm once the dependency is declared down
resilience_test.go   backoff schedule, breaker opens after N (race), half-open
                     after cooldown via fake clock, half-open failure reopens,
                     and retry+breaker short-circuits a doomed call
```

- Files: `resilience.go`, `cmd/demo/main.go`, `resilience_test.go`.
- Implement: `WithTimeout[T]`, `WithRetry[T]` (exponential backoff via an injectable `Sleeper`), and `WithBreaker[T]` (a `Closed -> Open -> HalfOpen` state machine guarded by a mutex and driven by an injectable `Clock`), all keeping the `Op[T]` signature so they nest in any order.
- Test: the backoff produces the expected delay schedule; the breaker opens after N consecutive failures and rejects further calls with `ErrCircuitOpen` without invoking the operation; after the clock advances past the cooldown it admits exactly one probe (half-open) and closes on success or reopens on failure; the breaker is race-free under concurrent callers; and a retry around an open breaker stops instead of storming.
- Verify: `go test -race ./...`

### One signature, three decorators

The generic operation is `type Op[T any] func(context.Context) (T, error)` — a cancellable call returning a value of any type. A decorator is any `func(Op[T]) Op[T]`: it takes an operation and returns one of the same shape with behavior added around the call. Because input and output types match, the decorators nest exactly like HTTP middleware, and because `T` is a type parameter, one implementation works for `Op[int]`, `Op[*Response]`, and everything else with no `interface{}`. Each error path must return `var zero T`, the type parameter's zero value, because no literal is valid for all `T`.

`WithTimeout` derives a child context with `context.WithTimeout`, runs the operation in a goroutine, and selects between the context's `Done` channel and the operation's result. The result channel is buffered with capacity one so that when the timeout branch wins and the decorator returns, the operation's goroutine can still send its eventual result into the buffer and exit instead of blocking forever — an unbuffered channel would leak one goroutine on every timeout. The wrapped operation must also honor `ctx.Done()` for the cancellation to actually stop its work.

### Retry with backoff, and a deterministic clock seam

`WithRetry` loops up to `maxAttempts`, returns immediately on success and on any error its `retryable` predicate rejects, and between attempts waits a growing delay computed by `Backoff`. The backoff is exponential: attempt one waits `Base`, attempt two waits `Base * Factor`, and so on, each capped at `Max`. Real production backoff adds jitter to avoid synchronized retries; this version keeps the schedule deterministic so a test can assert it exactly, and notes jitter as the production extension.

The wait is the seam that makes retry testable. Instead of calling `time.Sleep` directly, `WithRetry` calls an injected `Sleeper func(context.Context, time.Duration) error`. Production passes `SleepWithContext`, which waits on a timer but returns early if the context is cancelled. A test passes a fake sleeper that records the requested durations and returns instantly, so the test asserts the backoff schedule without spending real wall-clock time and without flaking on a slow machine. The sleeper also returns the context error, so a cancelled context aborts the retry loop between attempts rather than waiting out a doomed backoff.

`IsRetryable` is the classification seam: it treats `ErrTransient` and `context.DeadlineExceeded` as worth another attempt, and — critically — treats `ErrCircuitOpen` as permanent. That last rule is what makes retry compose safely with the breaker: once the breaker has opened, every further attempt returns `ErrCircuitOpen` instantly, and a retry loop that treated it as retryable would spin through its whole budget hammering a fast-failing breaker. Declaring it non-retryable turns the open breaker into an immediate stop.

### The circuit breaker state machine

A circuit breaker is a three-state machine that protects a failing dependency from a thundering herd. In `Closed` state calls pass through and consecutive failures are counted; when the count reaches `maxFailures` the breaker trips to `Open` and records when. In `Open` state every call fails fast with `ErrCircuitOpen` — the operation is not even invoked — until the `cooldown` has elapsed. The first call after the cooldown moves the breaker to `HalfOpen` and is admitted as a single probe; if it succeeds the breaker closes and the failure count resets, and if it fails the breaker reopens and restarts the cooldown. The half-open state is deliberately stingy: only one probe is allowed through at a time, so a dependency that is still down is touched once per cooldown rather than by every waiting caller.

Two design choices make this correct and testable. First, all state transitions happen under a mutex, so the breaker is safe to share across goroutines — which the `-race` test exercises directly. Second, the cooldown is measured against an injected `Clock` interface rather than `time.Now`, so a test can construct a fake clock, trip the breaker, advance the clock past the cooldown with no real waiting, and assert the half-open transition deterministically. The operation runs outside the lock (the breaker must not hold its mutex across a slow network call), and the result is folded back into the state under the lock afterward; a `probing` flag elected under the lock ensures only one goroutine holds the half-open probe at a time.

Create `resilience.go`:

```go
// Package resilience provides generic decorators for any cancellable service
// call: timeout via context, retry with exponential backoff, and a circuit
// breaker. All keep the Op[T] signature so they compose in any order.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Op is a cancellable operation returning a value of any type T or an error.
// Every decorator below takes an Op and returns an Op of the same type.
type Op[T any] func(context.Context) (T, error)

// Clock is the time source the breaker reads. Production uses SystemClock; tests
// inject a fake clock to drive the cooldown deterministically.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real wall clock.
type SystemClock struct{}

// Now reports the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// ErrTransient marks a failure worth retrying. Real code classifies network
// resets, 503s, and lock contention; here a sentinel keeps the seam visible.
var ErrTransient = errors.New("resilience: transient error")

// ErrCircuitOpen is returned by an open breaker without invoking the operation.
// IsRetryable treats it as permanent so a retry loop stops instead of storming.
var ErrCircuitOpen = errors.New("resilience: circuit open")

// IsRetryable is the default retry predicate. Transient failures and deadline
// timeouts are worth another attempt; an open circuit and everything else are not.
func IsRetryable(err error) bool {
	if errors.Is(err, ErrCircuitOpen) {
		return false
	}
	return errors.Is(err, ErrTransient) || errors.Is(err, context.DeadlineExceeded)
}

// Backoff describes an exponential delay schedule: attempt n waits
// Base*Factor^(n-1), capped at Max. Production would add jitter; this stays
// deterministic so tests can assert the schedule.
type Backoff struct {
	Base   time.Duration
	Factor float64
	Max    time.Duration
}

func (b Backoff) delay(attempt int) time.Duration {
	d := float64(b.Base)
	for i := 1; i < attempt; i++ {
		d *= b.Factor
		if b.Max > 0 && d >= float64(b.Max) {
			return b.Max
		}
	}
	return time.Duration(d)
}

// Sleeper waits for d but aborts early if ctx is cancelled, returning ctx.Err().
// Injecting it lets tests record the schedule without spending real time.
type Sleeper func(ctx context.Context, d time.Duration) error

// SleepWithContext is the production Sleeper: a timer that the context can cancel.
func SleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WithRetry returns an Op that calls op up to maxAttempts times, retrying only
// errors retryable accepts and waiting bo.delay(attempt) between tries via sleep.
// It wraps the last error with %w when attempts are exhausted.
func WithRetry[T any](op Op[T], maxAttempts int, bo Backoff, sleep Sleeper, retryable func(error) bool) Op[T] {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if retryable == nil {
		retryable = IsRetryable
	}
	if sleep == nil {
		sleep = SleepWithContext
	}
	return func(ctx context.Context) (T, error) {
		var zero T
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			v, err := op(ctx)
			if err == nil {
				return v, nil
			}
			if !retryable(err) {
				return zero, err
			}
			lastErr = err
			if attempt < maxAttempts {
				if serr := sleep(ctx, bo.delay(attempt)); serr != nil {
					return zero, serr
				}
			}
		}
		return zero, fmt.Errorf("resilience: exhausted %d attempts: %w", maxAttempts, lastErr)
	}
}

// WithTimeout returns an Op that bounds op to d. It runs op in a goroutine and
// races its result against the derived context's deadline. The result channel is
// buffered so the goroutine can always send and exit even after a timeout, which
// is what prevents a goroutine leak.
func WithTimeout[T any](op Op[T], d time.Duration) Op[T] {
	return func(ctx context.Context) (T, error) {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()

		type result struct {
			v   T
			err error
		}
		ch := make(chan result, 1)
		go func() {
			v, err := op(ctx)
			ch <- result{v, err}
		}()

		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case res := <-ch:
			return res.v, res.err
		}
	}
}

type state int

const (
	stateClosed state = iota
	stateOpen
	stateHalfOpen
)

// breaker is the shared circuit-breaker state. All fields are guarded by mu; the
// wrapped op runs outside the lock so the breaker never holds its mutex across a
// slow call.
type breaker[T any] struct {
	mu          sync.Mutex
	op          Op[T]
	maxFailures int
	cooldown    time.Duration
	clock       Clock

	state    state
	failures int
	openedAt time.Time
	probing  bool
}

func (b *breaker[T]) call(ctx context.Context) (T, error) {
	var zero T

	b.mu.Lock()
	switch b.state {
	case stateOpen:
		if b.clock.Now().Sub(b.openedAt) < b.cooldown {
			b.mu.Unlock()
			return zero, ErrCircuitOpen
		}
		// Cooldown elapsed: become half-open and elect this call as the probe.
		b.state = stateHalfOpen
		b.probing = true
	case stateHalfOpen:
		if b.probing {
			b.mu.Unlock()
			return zero, ErrCircuitOpen
		}
		b.probing = true
	case stateClosed:
		// fall through and run the op
	}
	b.mu.Unlock()

	v, err := b.op(ctx)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if err != nil {
		b.failures++
		if b.state == stateHalfOpen || b.failures >= b.maxFailures {
			b.state = stateOpen
			b.openedAt = b.clock.Now()
		}
		return zero, err
	}
	b.failures = 0
	b.state = stateClosed
	return v, nil
}

// WithBreaker returns an Op guarded by a circuit breaker that trips open after
// maxFailures consecutive failures, fails fast with ErrCircuitOpen for cooldown,
// then admits a single probe (half-open) measured against clock.
func WithBreaker[T any](op Op[T], maxFailures int, cooldown time.Duration, clock Clock) Op[T] {
	if maxFailures < 1 {
		maxFailures = 1
	}
	if clock == nil {
		clock = SystemClock{}
	}
	b := &breaker[T]{
		op:          op,
		maxFailures: maxFailures,
		cooldown:    cooldown,
		clock:       clock,
	}
	return b.call
}
```

### The runnable demo

The demo composes all three decorators as `retry(breaker(timeout(op)))` over a dependency that is permanently down. Each retry attempt passes through the breaker; the breaker counts two failures, trips open, and the third attempt fails fast with `ErrCircuitOpen`. Because `IsRetryable` declares `ErrCircuitOpen` permanent, the retry loop stops there instead of burning its remaining budget. The call count proves the breaker converted a five-attempt retry storm into two real calls plus one instant rejection.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/resilience"
)

func main() {
	var calls atomic.Int64
	down := func(context.Context) (string, error) {
		calls.Add(1)
		return "", resilience.ErrTransient // dependency is down
	}

	// timeout innermost, breaker around it (counts failures), retry outermost.
	timed := resilience.WithTimeout(resilience.Op[string](down), 50*time.Millisecond)
	guarded := resilience.WithBreaker(timed, 2, time.Second, resilience.SystemClock{})
	resilient := resilience.WithRetry(guarded, 5, resilience.Backoff{
		Base:   time.Millisecond,
		Factor: 2,
		Max:    10 * time.Millisecond,
	}, nil, resilience.IsRetryable)

	_, err := resilient(context.Background())
	fmt.Printf("stopped on open circuit: %v\n", errors.Is(err, resilience.ErrCircuitOpen))
	fmt.Printf("real calls made: %d (not 5)\n", calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stopped on open circuit: true
real calls made: 2 (not 5)
```

The retry budget was five, but the dependency was only actually called twice: the first two attempts failed and tripped the breaker, and the third attempt was rejected by the open breaker with `ErrCircuitOpen`, which `IsRetryable` treats as permanent, ending the loop. That is the whole reason to compose a breaker inside a retry — the breaker is the thing that knows when retrying is pointless.

### Tests

The breaker tests drive a fake clock so the cooldown is deterministic, and the concurrency test shares one breaker across many goroutines under `-race` to prove the state machine has no data race. The backoff test injects a recording sleeper to assert the exact delay schedule without spending real time.

Create `resilience_test.go`:

```go
package resilience

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced, race-safe clock for the breaker tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBackoff_Schedule(t *testing.T) {
	t.Parallel()

	var delays []time.Duration
	rec := func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	op := func(context.Context) (int, error) { return 0, ErrTransient }

	bo := Backoff{Base: time.Millisecond, Factor: 2, Max: 8 * time.Millisecond}
	_, _ = WithRetry(Op[int](op), 5, bo, rec, IsRetryable)(context.Background())

	// 5 attempts -> 4 inter-attempt waits: 1ms, 2ms, 4ms, then capped at 8ms.
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("recorded %d delays, want %d: %v", len(delays), len(want), delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
}

func TestBreaker_OpensAfterNFailures(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	var calls int
	op := func(context.Context) (int, error) {
		calls++
		return 0, ErrTransient
	}
	br := WithBreaker(Op[int](op), 3, time.Second, clk)

	for i := 1; i <= 3; i++ {
		if _, err := br(context.Background()); !errors.Is(err, ErrTransient) {
			t.Fatalf("call %d err = %v, want ErrTransient", i, err)
		}
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
	// Breaker is now open: the next call must fail fast without invoking op.
	if _, err := br(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("post-trip err = %v, want ErrCircuitOpen", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times after trip, want still 3", calls)
	}
}

func TestBreaker_HalfOpensAfterCooldownThenCloses(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	var fail atomic.Bool
	fail.Store(true)
	var calls int
	op := func(context.Context) (int, error) {
		calls++
		if fail.Load() {
			return 0, ErrTransient
		}
		return 42, nil
	}
	br := WithBreaker(Op[int](op), 2, time.Second, clk)

	// Trip it open.
	_, _ = br(context.Background())
	_, _ = br(context.Background())
	if _, err := br(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("want open, got %v", err)
	}

	// Within cooldown: still open.
	clk.Advance(500 * time.Millisecond)
	if _, err := br(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("within cooldown want open, got %v", err)
	}

	// Past cooldown: half-open probe is admitted; make it succeed -> close.
	clk.Advance(600 * time.Millisecond)
	fail.Store(false)
	got, err := br(context.Background())
	if err != nil || got != 42 {
		t.Fatalf("probe = (%d, %v), want (42, nil)", got, err)
	}
	// Closed again: a normal call runs the op.
	before := calls
	if _, err := br(context.Background()); err != nil {
		t.Fatalf("closed call err = %v, want nil", err)
	}
	if calls != before+1 {
		t.Fatalf("closed call did not invoke op")
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	op := func(context.Context) (int, error) { return 0, ErrTransient }
	br := WithBreaker(Op[int](op), 2, time.Second, clk)

	_, _ = br(context.Background())
	_, _ = br(context.Background()) // open
	clk.Advance(2 * time.Second)    // past cooldown

	// Half-open probe fails -> reopen immediately.
	if _, err := br(context.Background()); !errors.Is(err, ErrTransient) {
		t.Fatalf("probe err = %v, want ErrTransient", err)
	}
	// Without advancing the clock again, the breaker must be open.
	if _, err := br(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("after failed probe err = %v, want ErrCircuitOpen", err)
	}
}

func TestBreaker_ConcurrentIsRaceFree(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	var calls atomic.Int64
	op := func(context.Context) (int, error) {
		calls.Add(1)
		return 0, ErrTransient
	}
	br := WithBreaker(Op[int](op), 5, time.Second, clk)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = br(context.Background())
		}()
	}
	wg.Wait()

	// The breaker is open now; one more call must fail fast and never hit op.
	before := calls.Load()
	if _, err := br(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("after 100 failures err = %v, want ErrCircuitOpen", err)
	}
	if calls.Load() != before {
		t.Fatalf("op invoked while open")
	}
}

func TestRetryAroundBreaker_StopsOnOpen(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	var calls int
	op := func(context.Context) (int, error) {
		calls++
		return 0, ErrTransient
	}
	guarded := WithBreaker(Op[int](op), 2, time.Second, clk)

	noSleep := func(context.Context, time.Duration) error { return nil }
	_, err := WithRetry(guarded, 5, Backoff{Base: time.Millisecond}, noSleep, IsRetryable)(context.Background())

	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	// Budget was 5, but the breaker tripped after 2 real calls and the 3rd
	// attempt got ErrCircuitOpen, which IsRetryable treats as permanent.
	if calls != 2 {
		t.Fatalf("op called %d times, want 2 (breaker stopped the storm)", calls)
	}
}
```

## Review

The decorators are correct when each keeps the `Op[T]` signature exactly, so they nest in any order, and every error path returns `var zero T` rather than a typed literal. The breaker's correctness rests on two seams. The first is the injectable `Clock`: `TestBreaker_HalfOpensAfterCooldownThenCloses` trips the breaker, advances a fake clock 500ms (still open), then 600ms more (past the one-second cooldown) and proves the next call is admitted as a half-open probe — none of which would be deterministic against `time.Now`. The second is the mutex: `TestBreaker_ConcurrentIsRaceFree` shares one breaker across a hundred goroutines under `-race`, which is what proves the state transitions are serialized; the operation deliberately runs outside the lock so the breaker never blocks all callers behind one slow call, and the `probing` flag keeps half-open to a single probe.

The composition test encodes the reason these are separate decorators. `retry(breaker(...))` lets the breaker stop a doomed retry loop: once it opens, `ErrCircuitOpen` is returned instantly and `IsRetryable` declares it permanent, so the loop ends after two real calls instead of five. Reverse the nesting to `breaker(retry(...))` and the breaker would see one composite call per retry-loop, a different and usually weaker policy — which you want is a composition-order decision, exactly like the HTTP chain's, and the decorators themselves never change. The backoff schedule is asserted with a recording sleeper so the test pins `1ms, 2ms, 4ms, 8ms` (capped) without real waiting; production would swap in `SleepWithContext` and add jitter, and the only thing that changes is the injected `Sleeper`.

## Resources

- [Introduction to generics](https://go.dev/blog/intro-generics) — type parameters and inference, what makes one decorator work for every `T`.
- [Circuit Breaker (Martin Fowler)](https://martinfowler.com/bliki/CircuitBreaker.html) — the canonical description of the closed/open/half-open state machine this exercise implements.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — deriving a deadline-bounded child context and why `cancel` must always be called.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — buffered channels and avoiding goroutine leaks when a consumer stops early.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-production-api-gateway-stack.md](04-production-api-gateway-stack.md)
