# Exercise 7: Exponential Backoff: Why You Cannot Multiply A Duration By An int

Backoff math is the canonical place engineers try to multiply a `time.Duration` by
a loop variable and get a compile error they did not expect. You will build a
`Backoff.Delay(attempt int)` that grows geometrically and caps at a maximum, learn
why `attempt * time.Second` does not compile while `2 * time.Second` does, and make
the growth overflow-safe.

## What you'll build

```text
backoff/                    independent module: example.com/backoff
  go.mod                    go 1.26
  backoff.go                type Backoff; Delay; Jittered (math/rand/v2)
  cmd/
    demo/
      main.go               prints the delay schedule for attempts 0..7
  backoff_test.go           growth, clamp, non-negative overflow, jitter bounds
```

Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
Implement: `type Backoff{Base, Max time.Duration}`, `Delay(attempt int)
time.Duration`, `Jittered(attempt int) time.Duration`.
Test: `Delay(0) == Base`, geometric growth, `Delay(large)` clamped to `Max`, never
negative, jitter within `[0, Delay]`, and a `var _ time.Duration = b.Delay(3)` pin.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backoff/cmd/demo
cd ~/go-exercises/backoff
go mod init example.com/backoff
go mod edit -go=1.26
```

## `time.Duration` is a defined int64, and that changes the arithmetic

`time.Duration` is declared as `type Duration int64`. It is therefore a *distinct*
type: it does not mix with a plain `int` in arithmetic without a conversion. The
reason `2 * time.Second` compiles is that `2` is an *untyped constant* — it conforms
to whatever type the other operand has, so the product is a `time.Duration`. But
`attempt * time.Second`, where `attempt` is an `int` variable, does **not** compile:
you are multiplying two *typed* values of different types (`int` and
`time.Duration`), which Go forbids. The fix is to convert:
`time.Duration(attempt) * time.Second`, or to work in the duration's own bits with a
shift, `base << attempt`, since shifting a `Duration` by an integer count is legal
(the shift count need not match the type).

The second hazard is overflow. Geometric growth doubles each attempt, and a
`time.Duration` is an `int64` nanosecond count that maxes out at roughly 292 years.
`base << attempt` for a large `attempt` overflows: the high bit flips and the delay
becomes *negative* — a backoff that sleeps a negative duration is an instant retry
storm. So `Delay` doubles in a loop and returns `Max` the moment the value overflows
(`d <= 0`) or exceeds the cap, using the `min` builtin (Go 1.21) for the final
clamp. The result is always in `[Base, Max]` and never negative, whatever
`attempt` you pass.

`Jittered` adds full jitter — a uniformly random duration in `[0, Delay(attempt)]` —
using `math/rand/v2`'s `rand.Int64N`, which is the recommended way to spread
retries so a fleet does not synchronize its wake-ups. It is a separate method so
`Delay` stays pure and testable while jitter is exercised only for its bounds.

Create `backoff.go`:

```go
package backoff

import (
	"math/rand/v2"
	"time"
)

// Backoff computes retry delays that grow geometrically from Base and never
// exceed Max.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay returns the delay for a zero-based attempt: Base * 2^attempt, clamped to
// Max. It is overflow-safe: doubling stops as soon as the value would exceed Max
// or wrap negative, so the result is always in [Base, Max] and never negative.
//
// Note: you cannot write `attempt * time.Second` here, because attempt is an int
// and time.Second is a time.Duration -- two distinct typed values will not
// multiply. Only untyped constants (as in `2 * time.Second`) cross freely; a
// runtime int must be converted, e.g. time.Duration(attempt) * time.Second.
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := b.Base
	for range attempt {
		d <<= 1
		if d <= 0 || d >= b.Max { // overflow wrapped, or reached the cap
			return b.Max
		}
	}
	return min(d, b.Max)
}

// Jittered returns a uniformly random duration in [0, Delay(attempt)], spreading
// retries so a fleet does not synchronize. It uses math/rand/v2.
func (b Backoff) Jittered(attempt int) time.Duration {
	d := b.Delay(attempt)
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/backoff"
)

func main() {
	b := backoff.Backoff{Base: 100 * time.Millisecond, Max: 10 * time.Second}
	for attempt := range 8 {
		fmt.Printf("attempt %d: %s\n", attempt, b.Delay(attempt))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 0: 100ms
attempt 1: 200ms
attempt 2: 400ms
attempt 3: 800ms
attempt 4: 1.6s
attempt 5: 3.2s
attempt 6: 6.4s
attempt 7: 10s
```

## Tests

The tests pin `Delay(0)` to `Base`, verify geometric growth, clamp a large attempt
to `Max`, prove no attempt (including one that would overflow) yields a negative
delay, and bound `Jittered` within `[0, Delay]`.

Create `backoff_test.go`:

```go
package backoff

import (
	"testing"
	"time"
)

func newBackoff() Backoff {
	return Backoff{Base: 100 * time.Millisecond, Max: 10 * time.Second}
}

func TestDelayGeometric(t *testing.T) {
	t.Parallel()

	b := newBackoff()
	var _ time.Duration = b.Delay(3) // pin

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{4, 1600 * time.Millisecond},
	}
	for _, c := range cases {
		if got := b.Delay(c.attempt); got != c.want {
			t.Errorf("Delay(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
}

func TestDelayClampedToMax(t *testing.T) {
	t.Parallel()

	b := newBackoff()
	for _, attempt := range []int{7, 8, 20, 100} {
		if got := b.Delay(attempt); got != b.Max {
			t.Errorf("Delay(%d) = %s, want Max %s", attempt, got, b.Max)
		}
	}
}

func TestDelayNeverNegative(t *testing.T) {
	t.Parallel()

	b := newBackoff()
	for _, attempt := range []int{-5, 0, 1, 62, 63, 64, 1000} {
		if got := b.Delay(attempt); got < 0 {
			t.Fatalf("Delay(%d) = %s, want non-negative", attempt, got)
		}
	}
}

func TestDelayNegativeAttemptIsBase(t *testing.T) {
	t.Parallel()

	b := newBackoff()
	if got := b.Delay(-1); got != b.Base {
		t.Fatalf("Delay(-1) = %s, want Base %s", got, b.Base)
	}
}

func TestJitteredWithinBounds(t *testing.T) {
	t.Parallel()

	b := newBackoff()
	for attempt := range 8 {
		ceiling := b.Delay(attempt)
		for range 100 {
			j := b.Jittered(attempt)
			if j < 0 || j > ceiling {
				t.Fatalf("Jittered(%d) = %s, want within [0, %s]", attempt, j, ceiling)
			}
		}
	}
}
```

## Review

`Delay` is correct when it multiplies by powers of two without ever letting a
`time.Duration` mix with a plain `int`, and when growth is clamped before it can
wrap negative. The compile-time lesson lives in the doc comment: `attempt *
time.Second` is rejected because both operands are typed and distinct; only an
untyped constant like `2` crosses freely, so a runtime multiplier needs
`time.Duration(attempt)` or a shift. The overflow guard is not paranoia — a
`Delay` that returns a negative duration turns exponential backoff into an
unbounded retry storm, which is exactly the failure backoff exists to prevent.

## Resources

- [time.Duration](https://pkg.go.dev/time#Duration) — the `int64` defined type and its arithmetic.
- [math/rand/v2](https://pkg.go.dev/math/rand/v2#Int64N) — `Int64N` for jitter.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter spreads retries.

---

Back to [06-rps-rate-metrics-truncation.md](06-rps-rate-metrics-truncation.md) | Next: [08-config-precedence-cmp-or.md](08-config-precedence-cmp-or.md)
