# Exercise 2: Bounded Exponential Backoff with Full Jitter

Retrying a flaky outbound call — an HTTP request to a payment provider, a
transient database error — is the textbook use of a *counted* loop, because the
retry budget is a hard resource limit. A retry loop with no cap is not a retry; it
is an outage generator that hammers a struggling dependency forever. This module
builds a `Retryer` whose counted `for attempt := range maxAttempts` loop caps the
budget, backs off exponentially with a ceiling, adds full jitter to avoid
thundering-herd synchronization, and cancels cleanly on context.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
backoff/                     module example.com/backoff
  go.mod
  backoff.go                 Retryer: New, Do(ctx, maxAttempts, op); ErrPermanent, ErrExhausted
  backoff_test.go            fake-sleep + seeded-rand suite, -race concurrent retry
  cmd/demo/
    main.go                  retries a flaky operation and prints the attempt trace
```

- Files: `backoff.go`, `backoff_test.go`, `cmd/demo/main.go`.
- Implement: `New(base, max)`, `Do(ctx, maxAttempts, op)` with a counted loop, exponential-with-cap backoff via `min`, full jitter, a cancelable timer wait, early stop on `ErrPermanent`, and `errors.Join(ErrExhausted, last)` on exhaustion.
- Test: inject a fake sleep and a seeded `rand.NewChaCha8` source so backoff is reproducible; assert success on attempt k (op called exactly k times), exhaustion, immediate return on a permanent error, and `ctx.Err()` on cancel mid-backoff; `-race` on a concurrent retry.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/02-exponential-backoff-retry/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/02-exponential-backoff-retry
```

### Why the counted loop is a budget, and why jitter matters

`for attempt := range maxAttempts` makes the budget the first thing a reader sees:
this loop runs at most `maxAttempts` times, full stop. That is the entire point of
choosing the counted form here. An unbounded `for {}` "retry until success" is the
classic incident: when the dependency is down, every client spins forever, and the
retries themselves keep it down.

The backoff delay grows exponentially — `base`, `2*base`, `4*base`, ... — but is
capped at `max` so it never grows without bound. The cap is computed with the
`min` builtin inside a small counted loop, which also sidesteps integer overflow
from a naive `base << attempt` shift: once the doubling reaches `max` it stays
there. On top of the capped delay we apply *full jitter*: the actual sleep is a
uniform random draw in `[0, delay)`. Without jitter, a fleet of clients that all
failed at the same instant would retry at the same instants forever (a
thundering herd); full jitter spreads them out. Reproducibility in tests comes
from injecting the random source — a seeded `rand.NewChaCha8` gives identical
draws every run.

Two early exits keep the loop honest. A *permanent* error (a 400 Bad Request, a
validation failure — anything a retry cannot fix) returns immediately: there is no
point spending the budget on a request that will always fail. And the wait between
attempts is cancelable: it is a `time.NewTimer` inside a `select` against
`ctx.Done()`, so a cancelled context ends the retry at once with `ctx.Err()`
rather than sleeping out the backoff.

Create `backoff.go`:

```go
package backoff

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

var (
	// ErrPermanent marks an error the retry loop must not retry. Wrap it:
	// fmt.Errorf("bad request: %w", ErrPermanent).
	ErrPermanent = errors.New("permanent error")
	// ErrExhausted is joined with the last error when the budget runs out.
	ErrExhausted = errors.New("retry budget exhausted")
	// ErrInvalidAttempts means maxAttempts was not positive.
	ErrInvalidAttempts = errors.New("maxAttempts must be positive")
)

// Retryer runs an operation with bounded exponential backoff and full jitter.
type Retryer struct {
	base time.Duration
	max  time.Duration
	// rand, when non-nil, makes jitter reproducible in tests. nil uses the
	// package-global source.
	rand *rand.Rand
	// sleep, when non-nil, replaces the real cancelable timer wait in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// New builds a Retryer with the given base delay and ceiling.
func New(base, max time.Duration) *Retryer {
	return &Retryer{base: base, max: max}
}

// Do runs op up to maxAttempts times. It returns nil on the first success,
// the error unchanged if it wraps ErrPermanent, ctx.Err() if the context is
// cancelled during a backoff, or errors.Join(ErrExhausted, lastErr) when the
// budget is spent.
func (r *Retryer) Do(ctx context.Context, maxAttempts int, op func() error) error {
	if maxAttempts <= 0 {
		return ErrInvalidAttempts
	}
	var last error
	for attempt := range maxAttempts {
		err := op()
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrPermanent) {
			return err
		}
		last = err
		if attempt == maxAttempts-1 {
			break
		}
		if err := r.wait(ctx, r.backoff(attempt)); err != nil {
			return err
		}
	}
	return errors.Join(ErrExhausted, last)
}

// backoff returns the jittered delay for a given zero-based attempt: an
// exponentially growing value capped at r.max, then a uniform draw in [0, delay).
func (r *Retryer) backoff(attempt int) time.Duration {
	d := r.base
	for range attempt {
		d = min(r.max, d*2)
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(r.int64N(int64(d)))
}

func (r *Retryer) int64N(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if r.rand != nil {
		return r.rand.Int64N(n)
	}
	return rand.Int64N(n)
}

// wait sleeps for d, returning early with ctx.Err() if the context is cancelled.
func (r *Retryer) wait(ctx context.Context, d time.Duration) error {
	if r.sleep != nil {
		return r.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
```

### The runnable demo

The demo retries an operation that fails twice and then succeeds, printing each
attempt. Because jitter is random, the demo prints only the attempt outcomes (not
the exact sleep durations), so its output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/backoff"
)

func main() {
	r := backoff.New(1*time.Millisecond, 20*time.Millisecond)

	attempts := 0
	op := func() error {
		attempts++
		if attempts < 3 {
			fmt.Printf("attempt %d: transient failure\n", attempts)
			return errors.New("connection reset")
		}
		fmt.Printf("attempt %d: success\n", attempts)
		return nil
	}

	if err := r.Do(context.Background(), 5, op); err != nil {
		fmt.Printf("gave up: %v\n", err)
		return
	}
	fmt.Printf("succeeded after %d attempts\n", attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
attempt 1: transient failure
attempt 2: transient failure
attempt 3: success
succeeded after 3 attempts
```

### Tests

Determinism comes from injecting both the sleep and the random source. The fake
sleep records each requested delay and never actually waits, so a five-attempt
retry runs instantly; a seeded `rand.NewChaCha8` makes the jittered delays
reproducible. `TestSucceedsAndStops` asserts the op is called exactly `k` times
when it succeeds on the `k`-th. `TestCancelDuringBackoff` uses a fake sleep that
cancels the context and returns `ctx.Err()`, proving the loop abandons its budget
on cancellation.

Create `backoff_test.go`:

```go
package backoff

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// newTestRetryer builds a Retryer with a reproducible rand source and a fake
// sleep that records delays without waiting.
func newTestRetryer(sleep func(context.Context, time.Duration) error) *Retryer {
	var seed [32]byte
	seed[0] = 42
	return &Retryer{
		base:  time.Second,
		max:   time.Minute,
		rand:  rand.New(rand.NewChaCha8(seed)),
		sleep: sleep,
	}
}

func TestSucceedsAndStops(t *testing.T) {
	t.Parallel()

	calls := 0
	r := newTestRetryer(func(context.Context, time.Duration) error { return nil })

	err := r.Do(context.Background(), 5, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want exactly 3", calls)
	}
}

func TestExhaustsBudget(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("still failing")
	calls := 0
	r := newTestRetryer(func(context.Context, time.Duration) error { return nil })

	err := r.Do(context.Background(), 4, func() error {
		calls++
		return sentinel
	})
	if calls != 4 {
		t.Fatalf("op called %d times, want 4 (the budget)", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want it to wrap ErrExhausted", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to also carry the last error", err)
	}
}

func TestPermanentErrorStopsImmediately(t *testing.T) {
	t.Parallel()

	calls := 0
	r := newTestRetryer(func(context.Context, time.Duration) error {
		t.Fatal("sleep must not be called on a permanent error")
		return nil
	})

	err := r.Do(context.Background(), 5, func() error {
		calls++
		return fmt.Errorf("bad request: %w", ErrPermanent)
	})
	if calls != 1 {
		t.Fatalf("op called %d times, want exactly 1", calls)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	r := newTestRetryer(func(ctx context.Context, _ time.Duration) error {
		cancel() // simulate cancellation arriving during the wait
		return ctx.Err()
	})

	calls := 0
	err := r.Do(ctx, 5, func() error {
		calls++
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (cancelled after the first backoff)", calls)
	}
}

func TestRejectsInvalidAttempts(t *testing.T) {
	t.Parallel()

	r := New(time.Millisecond, time.Second)
	if err := r.Do(context.Background(), 0, func() error { return nil }); !errors.Is(err, ErrInvalidAttempts) {
		t.Fatalf("err = %v, want ErrInvalidAttempts", err)
	}
}

func TestConcurrentRetry(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newTestRetryer(func(context.Context, time.Duration) error { return nil })
			_ = r.Do(context.Background(), 3, func() error { return errors.New("x") })
		}()
	}
	wg.Wait()
}

func ExampleRetryer_Do() {
	r := New(time.Millisecond, time.Second)
	err := r.Do(context.Background(), 3, func() error { return nil })
	fmt.Println(err)
	// Output: <nil>
}
```

## Review

The retry is correct when the loop is a visible, hard budget — `for attempt :=
range maxAttempts` — and every exit is accounted for: success returns `nil`, a
`ErrPermanent`-wrapping error returns immediately without spending the budget,
cancellation returns `ctx.Err()`, and exhaustion returns
`errors.Join(ErrExhausted, last)` so a caller can match both the "we gave up"
sentinel and the underlying failure with `errors.Is`. The backoff is capped with
`min` so it never overflows or grows unbounded, and full jitter is a uniform draw
in `[0, delay)` from an *injected* source so tests are reproducible. The wait is a
`time.NewTimer` in a `select` against `ctx.Done()` — never `time.After` in a loop.
`TestCancelDuringBackoff` proves the loop abandons its remaining budget the instant
the context is cancelled. Run `go test -count=1 -race ./...`.

## Resources

- [math/rand/v2](https://pkg.go.dev/math/rand/v2) — `Int64N` for full jitter and `NewChaCha8` for a seeded, reproducible source.
- [errors.Join](https://pkg.go.dev/errors#Join) — combining `ErrExhausted` with the last error so both match `errors.Is`.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter beats plain exponential backoff.
- [context package](https://pkg.go.dev/context) — the cancellation the backoff wait honors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-token-bucket-rate-limiter.md](01-token-bucket-rate-limiter.md) | Next: [03-batch-chunk-writer.md](03-batch-chunk-writer.md)
