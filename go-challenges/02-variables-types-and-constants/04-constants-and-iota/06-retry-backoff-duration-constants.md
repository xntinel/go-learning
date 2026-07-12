# Exercise 6: Encode Retry Backoff and Timeout Budgets as Duration Constants

Every resilient client has a retry policy, and its numbers — base backoff, max
backoff, request timeout — belong in `time.Duration` constants, not scattered
literals. This module builds that policy and the exponential-backoff function,
with the one subtlety that separates a correct implementation from a subtly
broken one: shifting a duration overflows, so the growth must be capped.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
retry/                          module: example.com/retry
  go.mod                        go 1.26
  retry.go                      BaseBackoff/MaxBackoff/RequestTimeout, Backoff(attempt)
  cmd/
    demo/
      main.go                   prints the backoff schedule for the first attempts
  retry_test.go                 constant values, doubling, saturation, no overflow
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `BaseBackoff`, `MaxBackoff`, `RequestTimeout` duration constants and `Backoff(attempt int) time.Duration`.
Test: `Backoff(0) == BaseBackoff`; backoff doubles until it saturates at `MaxBackoff` and never exceeds it, including a large attempt that would overflow if uncapped.
Verify: `go test -count=1 ./...`

## Why durations, and where the overflow hides

`time.Duration` is an `int64` count of nanoseconds. That underpinning has two
consequences this module leans on.

The pleasant one: an untyped numeric constant multiplies cleanly with a duration
constant. `100 * time.Millisecond` is a `time.Duration`, because the untyped
`100` takes on the type of `time.Millisecond`. So the policy reads naturally:

```go
const (
	BaseBackoff    = 100 * time.Millisecond
	MaxBackoff     = 30 * time.Second
	RequestTimeout = 5 * time.Second
)
```

These are typed `time.Duration` constants (their type is inferred from
`time.Millisecond`/`time.Second`), self-documenting and comparable directly.

The unpleasant one: because a duration is a bounded `int64`, exponential backoff
by shifting — `BaseBackoff << attempt` — overflows. At a high enough attempt
count the shift pushes the value past `2^63` nanoseconds and it wraps to a
negative or nonsensical duration. A client that then "sleeps" for a negative
duration retries instantly in a tight loop, hammering a service that is already
struggling. This is a real outage pattern, not a theoretical one.

The fix is to double iteratively and stop the instant the value reaches the cap
or would overflow. Doubling in a loop and checking after each step means the
value never wraps unnoticed: as soon as `d` reaches `MaxBackoff` (or goes
non-positive from overflow), the function returns `MaxBackoff`. The final
`min(d, MaxBackoff)` is a belt-and-suspenders cap for the in-range path.

Create `retry.go`:

```go
package retry

import "time"

// Retry policy budget. These are typed time.Duration constants.
const (
	BaseBackoff    = 100 * time.Millisecond
	MaxBackoff     = 30 * time.Second
	RequestTimeout = 5 * time.Second
)

// Backoff returns the delay before the given retry attempt (0-based), doubling
// from BaseBackoff and saturating at MaxBackoff. It never overflows: the value
// is capped as soon as it reaches the ceiling.
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := BaseBackoff
	for range attempt {
		d <<= 1
		if d <= 0 || d >= MaxBackoff {
			return MaxBackoff
		}
	}
	return min(d, MaxBackoff)
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/retry"
)

func main() {
	fmt.Printf("request timeout: %s\n", retry.RequestTimeout)
	for attempt := range 11 {
		fmt.Printf("attempt %2d backoff %s\n", attempt, retry.Backoff(attempt))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request timeout: 5s
attempt  0 backoff 100ms
attempt  1 backoff 200ms
attempt  2 backoff 400ms
attempt  3 backoff 800ms
attempt  4 backoff 1.6s
attempt  5 backoff 3.2s
attempt  6 backoff 6.4s
attempt  7 backoff 12.8s
attempt  8 backoff 25.6s
attempt  9 backoff 30s
attempt 10 backoff 30s
```

## Tests

`TestConstants` pins the policy values. `TestBackoffDoubles` walks the schedule
and asserts each step is double the previous while below the cap. `TestBackoffSaturates`
asserts the value reaches and stays at `MaxBackoff`. `TestBackoffNoOverflow`
throws a huge attempt count at it and asserts the result is exactly `MaxBackoff`,
never a negative or wrapped duration.

Create `retry_test.go`:

```go
package retry

import (
	"testing"
	"time"
)

func TestConstants(t *testing.T) {
	t.Parallel()

	if BaseBackoff != 100*time.Millisecond {
		t.Fatalf("BaseBackoff = %s, want 100ms", BaseBackoff)
	}
	if MaxBackoff != 30*time.Second {
		t.Fatalf("MaxBackoff = %s, want 30s", MaxBackoff)
	}
	if RequestTimeout != 5*time.Second {
		t.Fatalf("RequestTimeout = %s, want 5s", RequestTimeout)
	}
}

func TestBackoffDoubles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{8, 25600 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := Backoff(tt.attempt); got != tt.want {
			t.Fatalf("Backoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func TestBackoffSaturates(t *testing.T) {
	t.Parallel()

	if got := Backoff(9); got != MaxBackoff {
		t.Fatalf("Backoff(9) = %s, want %s", got, MaxBackoff)
	}
	// Once saturated it must never exceed the cap for any larger attempt.
	for attempt := 9; attempt < 40; attempt++ {
		if got := Backoff(attempt); got != MaxBackoff {
			t.Fatalf("Backoff(%d) = %s, want %s", attempt, got, MaxBackoff)
		}
	}
}

func TestBackoffNoOverflow(t *testing.T) {
	t.Parallel()

	got := Backoff(1000)
	if got != MaxBackoff {
		t.Fatalf("Backoff(1000) = %s, want %s (overflow not capped)", got, MaxBackoff)
	}
	if got <= 0 {
		t.Fatalf("Backoff(1000) = %s, want a positive duration", got)
	}
}
```

## Review

The policy is correct when `Backoff(0)` is `BaseBackoff`, each step doubles, and
the value saturates at `MaxBackoff` without ever going negative. The overflow
cap is the load-bearing part: a naive `BaseBackoff << attempt` passes small-input
tests and then wraps negative at attempt 60-something, turning your retry loop
into a hot loop. `TestBackoffNoOverflow` exists precisely to catch that
regression. Duration constants keep the policy legible; `%s` formatting on a
`time.Duration` gives the human-readable `25.6s` for free in logs.

## Resources

- [time: Duration](https://pkg.go.dev/time#Duration)
- [Go Specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions)
- [Go Specification: Min and max built-ins](https://go.dev/ref/spec#Min_and_max)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-byte-size-limit-constants.md](05-byte-size-limit-constants.md) | Next: [07-untyped-const-boundaries-overflow.md](07-untyped-const-boundaries-overflow.md)
