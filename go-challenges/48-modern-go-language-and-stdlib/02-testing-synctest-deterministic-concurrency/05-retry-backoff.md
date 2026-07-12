# Exercise 5: Retry With Exponential Backoff

Retry with exponential backoff is the canonical "sleep between attempts" pattern,
and testing it honestly is exactly where real time hurts: a test that actually
waited `1s + 2s + 4s` would take seven seconds and still be approximate. This
exercise adds the final synctest technique — *measuring cumulative virtual time
across several sleeps* and asserting it equals the exact backoff sum.

This module is fully self-contained. It begins with its own `go mod init`, defines
everything it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
retry/                     independent module: example.com/retry
  go.mod                   go 1.25 (synctest needs it)
  retry.go                 Do(attempts, base, fn) error  (sleeps base, 2*base, 4*base...)
  cmd/
    demo/
      main.go              runnable demo: retries until success, prints each attempt
  retry_test.go            synctest: measures total virtual time across the backoff
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Do(attempts int, base time.Duration, fn func() error) error` that calls `fn` up to `attempts` times, sleeping `base << i` between tries, returning `nil` on first success or the last error.
- Test: a bubble that captures `start := time.Now()`, runs the retries, and asserts `time.Since(start)` equals the exact backoff sum; a give-up test pinning the attempt count and returned error.
- Verify: `go test -count=1 -race ./...`

Set up the module (`testing/synctest` requires Go 1.25+):

```bash
go mod edit -go=1.25
```

### Backoff, and measuring elapsed virtual time

`Do` is the textbook loop: call `fn`; on success return `nil`; otherwise, unless
this was the final attempt, sleep `base << i` (that is `base * 2^i` — `base`,
`2*base`, `4*base`, ...) and try again; after the last failure return the last
error. The `if i < attempts-1` guard is what stops it from sleeping *after* the
final attempt, when there is nothing left to wait for. An `attempts` below 1 is
clamped to 1 so the function always calls `fn` at least once.

The distinct technique this adds over the earlier exercises is *measuring elapsed
virtual time across several sleeps*. The test captures `start := time.Now()` before
the retries and, after they finish, asserts `time.Since(start)` equals the exact
backoff sum — three failures with a one-second base sleep 1 s after attempt one and
2 s after attempt two, so the total is *exactly* 3 s. That assertion is only
meaningful because the bubble's clock advances by precisely the slept amount and
nothing else: there is no scheduler slack to fuzz the total, no real-time jitter to
widen it into an approximate range. Against the real clock this same assertion
would be both slow (seven real seconds for a 1/2/4 backoff) and flaky to the
millisecond; inside a bubble it is exact and instant.

Create `retry.go`:

```go
package retry

import "time"

// Do calls fn up to attempts times, sleeping base, 2*base, 4*base, ... between
// tries (exponential backoff). It returns nil on the first success, or the last
// error after the final attempt. An attempts value below 1 is treated as 1.
func Do(attempts int, base time.Duration, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := range attempts {
		if err = fn(); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(base << i) // base * 2^i
		}
	}
	return err
}
```

### The runnable demo

The demo runs against the real clock with a tiny 20 ms base so it finishes quickly:
it retries up to four times, fails the first two attempts, succeeds on the third,
and prints each attempt plus the final outcome.

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
	calls := 0
	err := retry.Do(4, 20*time.Millisecond, func() error {
		calls++
		fmt.Printf("attempt %d\n", calls)
		if calls < 3 {
			return errors.New("temporary")
		}
		return nil
	})
	fmt.Printf("done after %d attempts, err=%v\n", calls, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1
attempt 2
attempt 3
done after 3 attempts, err=<nil>
```

### Tests

`TestSucceedsAfterRetries` is the showcase: it fails twice then succeeds, and
asserts the elapsed virtual time is *exactly* 3 s (1 s after attempt one + 2 s
after attempt two), an assertion that is only exact because the bubble's clock has
no slack. It also pins the call count at 3 to prove the loop stopped on success and
did not sleep after the final call. `TestGivesUpWithLastError` exhausts all three
attempts with a permanent error and asserts the *last* error propagates out and
`fn` ran exactly `attempts` times.

Create `retry_test.go`:

```go
package retry

import (
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestSucceedsAfterRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()

		err := Do(5, time.Second, func() error {
			calls++
			if calls < 3 {
				return errors.New("flaky")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Do = %v, want nil", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
		// Backoff slept 1s after attempt 1 and 2s after attempt 2 = 3s exactly.
		if got := time.Since(start); got != 3*time.Second {
			t.Fatalf("elapsed = %v, want exactly 3s", got)
		}
	})
}

func TestGivesUpWithLastError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		calls := 0

		err := Do(3, time.Second, func() error {
			calls++
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Do = %v, want boom", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})
}

func Example() {
	calls := 0
	err := Do(3, time.Millisecond, func() error {
		calls++
		if calls < 2 {
			return errors.New("once")
		}
		return nil
	})
	fmt.Println(calls, err)
	// Output: 2 <nil>
}
```

## Review

`Do` is correct when the sleep schedule is `base << i` between tries and *not*
after the last one, and the cumulative-time assertion is what pins that exactly.
The classic off-by-one is sleeping after the final attempt: drop the
`if i < attempts-1` guard and the elapsed time jumps by one extra `base << (attempts-1)`,
which `TestSucceedsAfterRetries` would catch as a wrong total even though the
result is still correct. The second mistake is returning the wrong error on
give-up — `Do` must propagate the *last* `fn` error, which is why `err` is declared
outside the loop and reused. The exact 3 s assertion is the synctest payoff: it
only holds because virtual time advances by precisely the slept amount, so a flake
there means the code slept a different amount than intended. Run `go test -race`;
there is no shared state across goroutines here, but it confirms the test harness
itself is clean.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the bubble whose slack-free clock makes the exact elapsed-time assertion possible.
- [`time.Sleep`](https://pkg.go.dev/time#Sleep) — the call the bubble virtualizes so the backoff waits cost no real time.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why exponential backoff (and the jitter this minimal version omits) matters in practice.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-unique-value-interning/00-concepts.md](../03-unique-value-interning/00-concepts.md)
