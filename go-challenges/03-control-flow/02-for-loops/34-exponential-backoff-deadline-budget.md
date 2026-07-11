# Exercise 34: Exponential Backoff Constrained by Deadline

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Exercise 2 in this lesson built exponential backoff bounded by a fixed
attempt count. That is the wrong bound whenever the caller itself is
operating under a deadline — an HTTP handler with a 500ms budget, a gRPC
call with a context timeout — because a generous attempt count combined
with real exponential delays can burn through the caller's entire budget
on retries alone, long before the attempts run out. This module builds the
hybrid loop shape that a production retry helper actually needs: bounded
by *whichever* limit is tighter, attempts or deadline, checked on every
single pass so neither one can be silently ignored.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
backoff/                       module example.com/backoff
  go.mod                       go 1.24
  backoff.go                   Config; Do(cfg, fn) (int, error); ErrDeadlineExceeded; ErrAttemptsExhausted
  backoff_test.go                 succeeds on retry, exhausts attempts, stops at deadline first, delay caps, immediate success
  cmd/demo/
    main.go                      one call that recovers after two failures, one call that hits a tight deadline
```

- Files: `backoff.go`, `backoff_test.go`, `cmd/demo/main.go`.
- Implement: `Do(cfg Config, fn func() error) (int, error)` — a counted `for attempt := 1; attempt <= cfg.MaxAttempts; attempt++` loop with a `cfg.Now().Before(cfg.Deadline)` check at the top of every pass that returns immediately when the deadline has passed, exponential delay doubling capped at `cfg.MaxDelay`, and an injected `Jitter` function.
- Test: a call that fails once then succeeds; a call that exhausts every attempt without ever succeeding; a call whose deadline is reached before its attempt count runs out; the delay sequence caps correctly at `MaxDelay`; an immediately-successful call sleeps zero times.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backoff/cmd/demo
cd ~/go-exercises/backoff
go mod init example.com/backoff
go mod edit -go=1.24
```

### Why the deadline check has to be inside the loop, not around it

The tempting shortcut is to wrap the whole retry loop in an outer
`if cfg.Now().Before(cfg.Deadline)` and let a plain counted loop run
freely inside it — but that only checks the deadline *once*, before the
first attempt, and says nothing about attempt three, four, or five, each
of which could individually run past the deadline while the loop keeps
going regardless. The deadline has to be re-checked at the top of *every*
pass, which is exactly what turns this from "bounded by attempts, with an
occasional deadline glance" into "bounded by whichever limit the current
moment in time actually hits first." That is the hybrid termination proof
this exercise is built around: the loop's two exits are `attempt >
cfg.MaxAttempts` (falls off the `for` naturally) and `!cfg.Now().Before(cfg.Deadline)`
(an explicit early `return` from inside the body), and either one can fire
first depending on how the retries actually play out — `TestDoStopsAtDeadlineBeforeAttemptsRunOut`
sets `MaxAttempts` deliberately high specifically so the deadline is the
one that has to win.

The other detail that matters in practice is clamping the sleep itself to
the remaining budget: `if remaining := cfg.Deadline.Sub(cfg.Now()); wait >
remaining { wait = remaining }`. Without that clamp, a backoff delay that
happens to be larger than the time left before the deadline would sleep
the full delay anyway and then immediately hit the deadline check on the
next pass — burning wall-clock time on a sleep whose only possible outcome
is "deadline exceeded" one line later. Clamping the sleep means `Do`
returns (with a deadline error) right when the deadline actually arrives,
not some multiple of the base delay later.

Create `backoff.go`:

```go
package backoff

import (
	"errors"
	"fmt"
	"time"
)

// ErrDeadlineExceeded is wrapped into the returned error when the deadline
// passes before an attempt succeeds.
var ErrDeadlineExceeded = errors.New("backoff: deadline exceeded")

// ErrAttemptsExhausted is wrapped into the returned error when every
// attempt is used up without the deadline having passed.
var ErrAttemptsExhausted = errors.New("backoff: attempts exhausted")

// Config controls a retry-with-backoff run. Clock access and sleeping are
// both injected so tests can advance time deterministically instead of
// waiting on a real clock.
type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Deadline    time.Time
	Now         func() time.Time
	Sleep       func(time.Duration)
	Jitter      func(time.Duration) time.Duration // returns the actual delay to sleep for a given base delay
}

// Do retries fn with exponential backoff, bounded by whichever limit is
// tighter: MaxAttempts, or Deadline. It returns the number of attempts made
// and, on failure, an error wrapping either ErrDeadlineExceeded or
// ErrAttemptsExhausted alongside the last error fn returned.
//
// The loop is a hybrid of the two termination shapes from the concepts
// lesson: for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ gives it
// a counted upper bound, and the cfg.Now().Before(cfg.Deadline) check at the
// top of every pass gives it an independent, tighter bound that can end the
// loop before the counter ever runs out. Both checks are necessary: without
// the attempt count, a deadline far in the future would let a fast-failing
// fn retry an unbounded number of times before the deadline mattered;
// without the deadline check, a large MaxAttempts combined with a long
// backoff could keep retrying well past when the caller stopped caring.
func Do(cfg Config, fn func() error) (int, error) {
	var lastErr error
	delay := cfg.BaseDelay

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if !cfg.Now().Before(cfg.Deadline) {
			return attempt - 1, fmt.Errorf("%w: %d attempts made: %w", ErrDeadlineExceeded, attempt-1, lastErr)
		}

		err := fn()
		if err == nil {
			return attempt, nil
		}
		lastErr = err

		if attempt == cfg.MaxAttempts {
			break
		}

		wait := cfg.Jitter(delay)
		if remaining := cfg.Deadline.Sub(cfg.Now()); wait > remaining {
			wait = remaining // never sleep past the deadline pointlessly
		}
		cfg.Sleep(wait)

		delay *= 2
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}

	return cfg.MaxAttempts, fmt.Errorf("%w: %d attempts made: %w", ErrAttemptsExhausted, cfg.MaxAttempts, lastErr)
}
```

### The runnable demo

The demo runs two retries against a fixed, injected clock and a
deterministic `+10%` jitter function (standing in for a real seeded PRNG,
so the output below is exactly reproducible): one call fails twice then
succeeds well within its budget, and a second call is given a deadline so
tight that it is cut off by the deadline rather than by its (deliberately
generous) attempt count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/backoff"
)

func main() {
	t := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return t }
	sleep := func(d time.Duration) {
		fmt.Printf("  sleeping %v\n", d)
		t = t.Add(d)
	}
	// A fixed +10%% jitter, standing in for a real seeded PRNG so this demo's
	// output is exactly reproducible.
	jitter := func(d time.Duration) time.Duration { return d + d/10 }

	calls := 0
	fn := func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("attempt %d: upstream timeout", calls)
		}
		return nil
	}

	cfg := backoff.Config{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		Deadline:    t.Add(10 * time.Second),
		Now:         now,
		Sleep:       sleep,
		Jitter:      jitter,
	}

	fmt.Println("retrying an upstream call that fails twice then succeeds:")
	attempts, err := backoff.Do(cfg, fn)
	fmt.Printf("result: attempts=%d err=%v\n", attempts, err)

	fmt.Println("\nretrying an upstream call with a very tight deadline:")
	calls = 0
	fn2 := func() error {
		calls++
		return fmt.Errorf("attempt %d: still down", calls)
	}
	cfg.Deadline = t.Add(150 * time.Millisecond)
	cfg.MaxAttempts = 100
	attempts, err = backoff.Do(cfg, fn2)
	fmt.Printf("result: attempts=%d err=%v\n", attempts, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
retrying an upstream call that fails twice then succeeds:
  sleeping 110ms
  sleeping 220ms
result: attempts=3 err=<nil>

retrying an upstream call with a very tight deadline:
  sleeping 110ms
  sleeping 40ms
result: attempts=2 err=backoff: deadline exceeded: 2 attempts made: attempt 2: still down
```

### Tests

`TestDoSucceedsOnSecondAttempt` and `TestDoExhaustsMaxAttempts` cover the
two ordinary attempt-bounded outcomes, with a no-op `Jitter` so the exact
sleep durations are assertable. `TestDoStopsAtDeadlineBeforeAttemptsRunOut`
is the load-bearing test for this exercise: it sets `MaxAttempts` far
higher than the deadline could ever allow, and confirms the loop stops
because of the deadline, not the counter.
`TestDoDelayCapsAtMaxDelay` checks the full doubling-then-capped delay
sequence value by value. `TestDoImmediateSuccessMakesNoCalls` confirms the
zero-retry path never touches `Sleep` at all.

Create `backoff_test.go`:

```go
package backoff

import (
	"errors"
	"testing"
	"time"
)

type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func noJitter(d time.Duration) time.Duration { return d }

func TestDoSucceedsOnSecondAttempt(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	var sleeps []time.Duration

	calls := 0
	fn := func() error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}

	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    time.Second,
		Deadline:    clock.t.Add(time.Hour),
		Now:         clock.Now,
		Sleep:       func(d time.Duration) { sleeps = append(sleeps, d); clock.Advance(d) },
		Jitter:      noJitter,
	}

	attempts, err := Do(cfg, fn)
	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(sleeps) != 1 || sleeps[0] != 10*time.Millisecond {
		t.Fatalf("sleeps = %v, want [10ms]", sleeps)
	}
}

func TestDoExhaustsMaxAttempts(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	errDown := errors.New("upstream down")
	fn := func() error { return errDown }

	cfg := Config{
		MaxAttempts: 4,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Second,
		Deadline:    clock.t.Add(time.Hour),
		Now:         clock.Now,
		Sleep:       func(d time.Duration) { clock.Advance(d) },
		Jitter:      noJitter,
	}

	attempts, err := Do(cfg, fn)
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	if !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("err = %v, want wrapping ErrAttemptsExhausted", err)
	}
	if !errors.Is(err, errDown) {
		t.Fatalf("err = %v, want wrapping %v", err, errDown)
	}
}

func TestDoStopsAtDeadlineBeforeAttemptsRunOut(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	errDown := errors.New("upstream down")
	fn := func() error { return errDown }

	cfg := Config{
		MaxAttempts: 100,
		BaseDelay:   50 * time.Millisecond,
		MaxDelay:    time.Second,
		Deadline:    clock.t.Add(120 * time.Millisecond),
		Now:         clock.Now,
		Sleep:       func(d time.Duration) { clock.Advance(d) },
		Jitter:      noJitter,
	}

	attempts, err := Do(cfg, fn)
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want wrapping ErrDeadlineExceeded", err)
	}
	if attempts >= 100 {
		t.Fatalf("attempts = %d, want well under MaxAttempts (deadline should stop it first)", attempts)
	}
	if attempts == 0 {
		t.Fatal("attempts = 0, want at least one attempt before the deadline check stops it")
	}
}

func TestDoDelayCapsAtMaxDelay(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	var sleeps []time.Duration
	errDown := errors.New("upstream down")
	fn := func() error { return errDown }

	cfg := Config{
		MaxAttempts: 6,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    300 * time.Millisecond,
		Deadline:    clock.t.Add(time.Hour),
		Now:         clock.Now,
		Sleep:       func(d time.Duration) { sleeps = append(sleeps, d); clock.Advance(d) },
		Jitter:      noJitter,
	}

	if _, err := Do(cfg, fn); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("err = %v, want wrapping ErrAttemptsExhausted", err)
	}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
		300 * time.Millisecond,
		300 * time.Millisecond,
	}
	if len(sleeps) != len(want) {
		t.Fatalf("len(sleeps) = %d, want %d (%v)", len(sleeps), len(want), sleeps)
	}
	for i, w := range want {
		if sleeps[i] != w {
			t.Fatalf("sleeps[%d] = %v, want %v (full: %v)", i, sleeps[i], w, sleeps)
		}
	}
}

func TestDoImmediateSuccessMakesNoCalls(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	sleepCalls := 0

	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Second,
		Deadline:    clock.t.Add(time.Hour),
		Now:         clock.Now,
		Sleep:       func(time.Duration) { sleepCalls++ },
		Jitter:      noJitter,
	}

	attempts, err := Do(cfg, func() error { return nil })
	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if sleepCalls != 0 {
		t.Fatalf("Sleep called %d times, want 0", sleepCalls)
	}
}
```

## Review

`Do` is correct when it never sleeps or retries past `cfg.Deadline`, never
exceeds `cfg.MaxAttempts`, and always reports whichever bound actually
stopped it via the right sentinel error. The common mistake this design
avoids is checking the deadline only once, before the retry loop starts —
that version passes a trivial "deadline is already in the past" test but
lets every subsequent attempt run unchecked, so a slow, repeatedly-failing
`fn` combined with a long backoff schedule can blow through the caller's
deadline by a wide margin before the loop notices anything is wrong. Run
`go test -count=1 ./...`.

## Resources

- [AWS Architecture Blog: Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the backoff-plus-jitter shape this module implements.
- [context package](https://pkg.go.dev/context) — `context.Context.Deadline`, the production analogue of this module's injected `Deadline`/`Now`.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the hybrid counted/condition loop and its two independent exits.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-request-singleflight-deduplication.md](33-request-singleflight-deduplication.md) | Next: [35-feature-flag-targeting-rule-engine.md](35-feature-flag-targeting-rule-engine.md)
