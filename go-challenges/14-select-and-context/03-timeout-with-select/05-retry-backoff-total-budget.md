# Exercise 5: Retry with exponential backoff under a total deadline

Production retry code hides a subtle confusion: the per-attempt backoff sleep and
the overall time budget are two different clocks. This exercise builds
`RetryWithBudget`, which retries an operation with exponential backoff — sleeping
on a reusable, interruptible timer — while an outer overall-deadline timer caps the
entire sequence. It distinguishes the two clocks cleanly and cancels a pending
sleep on a done signal.

## What you'll build

```text
retrybudget/                  module example.com/retry
  go.mod
  retry.go                    ErrBudgetExhausted; RetryWithBudget(done, budget, base, op)
  cmd/demo/main.go            succeed-on-third, give-up-on-budget
  retry_test.go               early success, budget exhaustion, growing backoff, cancel
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `RetryWithBudget(done <-chan struct{}, budget, base time.Duration, op func() error) error`.
Test: succeeds on attempt 3 and stops; always-fails returns wrapped last error once the budget fires; backoff intervals grow; done cancels a pending sleep promptly.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retrybudget/cmd/demo
cd ~/go-exercises/retrybudget
go mod init example.com/retry
```

### Two timers, two questions

`RetryWithBudget` runs two timers at once, answering two questions. The
`deadline` timer, created once with `time.NewTimer(budget)`, is the overall cap:
"how long may the whole retry sequence run?" It fires exactly once. The `backoff`
timer is the per-attempt sleep between failures: "how long do I wait before trying
again?" It is reused and `Reset` to a doubling delay after each failure.

The backoff sleep is a `select`, not a `time.Sleep`, so it can be interrupted. It
waits on three cases: the overall `deadline.C` (give up, budget exhausted), the
`done` channel (caller cancelled), or the `backoff.C` tick (delay elapsed, try
again). A plain `time.Sleep(delay)` would be uninterruptible — a cancelled caller
would still wait out the full backoff before the loop noticed. Racing the sleep
against `done` and `deadline` in one `select` is what makes cancellation prompt.

The reusable backoff timer needs care at construction. It is created with a long
duration and immediately stopped so it does not fire before the first sleep; each
iteration re-arms it with the portable Stop-drain-Reset form. On this `go 1.23+`
module the drain is a no-op, but writing it keeps the code correct on older
toolchains. When the budget fires, the last error is wrapped with `%w` under
`ErrBudgetExhausted` using a double-`%w` `fmt.Errorf`, so the caller can match
*either* sentinel: `errors.Is(err, ErrBudgetExhausted)` to know the budget ran out,
and `errors.Is(err, <the op's error>)` to see the underlying cause.

The one honest limitation: the overall deadline is only checked between attempts,
during the backoff `select`. If a single `op()` call itself runs longer than the
whole budget, the function still waits for that call to return before noticing. In
production you would also bound each `op` with a per-attempt timeout (Exercise 4);
here the focus is the retry sequencing, so `op` is assumed to return promptly.

Create `retry.go`:

```go
package retry

import (
	"errors"
	"fmt"
	"time"
)

// ErrBudgetExhausted wraps the last error when the overall budget fires before a
// retry succeeds.
var ErrBudgetExhausted = errors.New("retry: budget exhausted")

// RetryWithBudget calls op repeatedly until it returns nil, the overall budget
// fires, or done is signalled. Between failures it waits an exponentially growing
// delay starting at base, on a reusable interruptible timer. On budget exhaustion
// it returns the last error wrapped under ErrBudgetExhausted; on cancellation it
// returns the last error wrapped with a cancellation message. A nil done channel
// simply never cancels.
func RetryWithBudget(done <-chan struct{}, budget, base time.Duration, op func() error) error {
	deadline := time.NewTimer(budget)
	defer deadline.Stop()

	// Reusable backoff timer, created stopped; always Reset before waiting.
	backoff := time.NewTimer(time.Hour)
	backoff.Stop()
	defer backoff.Stop()

	delay := base
	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := op(); err == nil {
			return nil
		} else {
			lastErr = err
		}

		// Re-arm the backoff timer (portable Stop-drain-Reset).
		if !backoff.Stop() {
			select {
			case <-backoff.C:
			default:
			}
		}
		backoff.Reset(delay)

		select {
		case <-deadline.C:
			return fmt.Errorf("%w after %d attempts: %w", ErrBudgetExhausted, attempt, lastErr)
		case <-done:
			return fmt.Errorf("retry: cancelled after %d attempts: %w", attempt, lastErr)
		case <-backoff.C:
			// delay elapsed; try again
		}
		delay *= 2
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/retry"
)

func main() {
	attempts := 0
	err := retry.RetryWithBudget(nil, time.Second, 0, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("connection refused")
		}
		return nil
	})
	fmt.Printf("succeeded after %d attempts, err=%v\n", attempts, err)

	err = retry.RetryWithBudget(nil, 80*time.Millisecond, 20*time.Millisecond, func() error {
		return errors.New("still down")
	})
	fmt.Printf("gave up, budgetExhausted=%v\n", errors.Is(err, retry.ErrBudgetExhausted))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
succeeded after 3 attempts, err=<nil>
gave up, budgetExhausted=true
```

The first call uses a one-second budget (so the overall timer never fires) with a
zero base delay, so its three attempts run back to back and the count is
deterministic. The second uses an 80ms budget with a 20ms base delay and always
fails, so the overall timer eventually fires and the budget is exhausted.

### Tests

`TestSucceedsEarly` proves the loop stops the instant `op` returns nil and does not
keep retrying. `TestBudgetExhausted` proves an always-failing op returns an error
that is *both* `ErrBudgetExhausted` and the wrapped underlying error, within a
generous multiple of the budget. `TestBackoffGrows` records attempt timestamps and
asserts the second gap is larger than the first — the exponential growth.
`TestCancelled` closes `done` mid-backoff and asserts the call returns promptly with
the underlying error wrapped, well before the (very long) budget would fire.

Create `retry_test.go`:

```go
package retry

import (
	"errors"
	"testing"
	"time"
)

func TestSucceedsEarly(t *testing.T) {
	t.Parallel()
	calls := 0
	err := RetryWithBudget(nil, time.Second, 10*time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestBudgetExhausted(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	start := time.Now()
	err := RetryWithBudget(nil, 100*time.Millisecond, 10*time.Millisecond, func() error {
		return errBoom
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want it to wrap errBoom", err)
	}
	if elapsed > 100*time.Millisecond+300*time.Millisecond {
		t.Fatalf("elapsed %v exceeds budget by too much", elapsed)
	}
}

func TestBackoffGrows(t *testing.T) {
	t.Parallel()
	var times []time.Time
	_ = RetryWithBudget(nil, 500*time.Millisecond, 20*time.Millisecond, func() error {
		times = append(times, time.Now())
		return errors.New("x")
	})
	if len(times) < 3 {
		t.Fatalf("recorded %d attempts, want >= 3", len(times))
	}
	d1 := times[1].Sub(times[0])
	d2 := times[2].Sub(times[1])
	if d2 <= d1 {
		t.Fatalf("backoff did not grow: d1=%v d2=%v", d1, d2)
	}
}

func TestCancelled(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	done := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()
	start := time.Now()
	err := RetryWithBudget(done, 10*time.Second, 500*time.Millisecond, func() error {
		return errBoom
	})
	elapsed := time.Since(start)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want it to wrap errBoom", err)
	}
	if errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("cancellation reported as budget exhaustion: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancellation not prompt: %v", elapsed)
	}
}
```

## Review

The retry loop is correct when the two clocks stay distinct: the backoff timer
governs the gap between attempts and grows each time, while the single overall
timer bounds the whole sequence and fires once. The classic production bug is
conflating them — resetting the "overall" timer inside the loop turns the total cap
into a per-attempt one, so an op that fails quickly forever never stops. The second
bug is an uninterruptible `time.Sleep` for backoff, which makes cancellation wait
out the full delay; the `select` over `done` and `deadline` is what keeps
`TestCancelled` prompt. Run `go test -race`; the shared `times`/`calls` variables
are touched only by the single caller goroutine (op runs synchronously inside
`RetryWithBudget`), so there is no race.

## Resources

- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — the reusable backoff timer.
- [`fmt.Errorf` with multiple `%w`](https://pkg.go.dev/fmt#Errorf) — wrapping two sentinels in one error.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching `ErrBudgetExhausted` and the underlying cause.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-downstream-call-sla.md](04-downstream-call-sla.md) | Next: [06-batch-flush-on-timer.md](06-batch-flush-on-timer.md)
