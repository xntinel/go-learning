# Exercise 9: Backoff Strategy as an Injectable First-Class Function

The delay between retries is a policy, and the best way to express a policy is a
first-class function. This exercise builds `Backoff func(attempt int) time.Duration`
with factories for constant and exponential delay, and a full-jitter decorator that
takes an injected `math/rand/v2` source so the randomness is deterministic in tests
and thundering-herd-safe in production.

## What you'll build

```text
backoff/                     independent module: example.com/backoff
  go.mod                     go 1.25
  backoff.go                 type Backoff; Constant, Exponential, FullJitter
  backoff_test.go            exponential sequence + clamp, constant, jitter bounds + exact seed, reproducibility
  cmd/demo/
    main.go                  prints a deterministic jittered schedule
```

- Files: `backoff.go`, `backoff_test.go`, `cmd/demo/main.go`.
- Implement: `Backoff func(int) time.Duration`, `Constant(d)`, `Exponential(base, cap)`, and `FullJitter(inner Backoff, rng *rand.Rand)`.
- Test: `Exponential` yields base, 2·base, 4·base... and clamps at cap; `Constant` ignores the attempt; `FullJitter` is within `[0, inner(attempt))` and equals the exact seeded sequence; two independent seeded sources reproduce.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why the randomness is a parameter

Exponential backoff alone has a failure mode at scale: if a dependency blips and a
thousand clients all back off by the same deterministic schedule, they retry in
synchronized waves — a thundering herd that keeps the dependency down. Full jitter
fixes this by choosing a random delay in `[0, exponential(attempt))`, spreading the
retries out. So production *needs* randomness here.

But a test that asserts an exact delay while the code reads a package-global
`rand` is flaky by construction: the global sequence depends on whatever else in
the process consumed it. The fix is to inject the source. `FullJitter` takes a
`*math/rand/v2.Rand`; production passes a source seeded from entropy, tests pass one
seeded deterministically with `rand.New(rand.NewPCG(seed1, seed2))`. The PCG
generator is fully specified, so the same seed produces the same sequence on every
run and every machine — the test can assert the exact jittered values. Same source,
two different jobs: entropy in production, a fixed seed in the test.

`Exponential(base, cap)` returns `base * 2^attempt` clamped at `cap`, computed by
doubling in a loop so it never overflows past the cap. `Constant(d)` ignores the
attempt entirely. `FullJitter(inner, rng)` decorates any `Backoff`: it computes the
inner delay and returns a random duration in `[0, inner)` drawn from the injected
source — a `Backoff -> Backoff` decorator, the same shape as the retry policies in
Exercise 1, so it slots straight into a retry loop as the pluggable delay.

Create `backoff.go`:

```go
package backoff

import (
	"math/rand/v2"
	"time"
)

// Backoff maps a zero-based attempt index to a delay before that attempt.
type Backoff func(attempt int) time.Duration

// Constant returns a Backoff that always waits d, ignoring the attempt.
func Constant(d time.Duration) Backoff {
	return func(int) time.Duration { return d }
}

// Exponential returns a Backoff of base * 2^attempt, clamped at cap. It doubles
// in a loop so it never overflows past cap.
func Exponential(base, cap time.Duration) Backoff {
	return func(attempt int) time.Duration {
		if attempt < 0 {
			attempt = 0
		}
		d := base
		for range attempt {
			if d >= cap {
				return cap
			}
			d *= 2
		}
		return min(d, cap)
	}
}

// FullJitter decorates inner with full jitter: it returns a random delay in
// [0, inner(attempt)) drawn from rng. Injecting rng makes the sequence
// deterministic in tests and random in production.
func FullJitter(inner Backoff, rng *rand.Rand) Backoff {
	return func(attempt int) time.Duration {
		d := inner(attempt)
		if d <= 0 {
			return 0
		}
		return time.Duration(rng.Int64N(int64(d)))
	}
}
```

### The runnable demo

The demo builds an exponential base schedule and a full-jitter version of it,
seeded deterministically so the output is stable, and prints both so you can see the
jitter fall inside the exponential envelope.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"

	"example.com/backoff"
)

func main() {
	base := backoff.Exponential(100*time.Millisecond, time.Second)
	rng := rand.New(rand.NewPCG(1, 2))
	jittered := backoff.FullJitter(base, rng)

	fmt.Println("attempt  base      jittered")
	for attempt := range 5 {
		fmt.Printf("%d        %-8s  %s\n", attempt, base(attempt), jittered(attempt).Round(time.Microsecond))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt  base      jittered
0        100ms     76.937ms
1        200ms     123.287ms
2        400ms     313.771ms
3        800ms     637.277ms
4        1s        234.276ms
```

The base column doubles until it clamps at one second; the jittered column is a
random draw below the base for that attempt, and because the source is seeded with
`NewPCG(1, 2)` the sequence is identical on every run.

### Tests

`Exponential` is checked against the exact doubling sequence and its clamp.
`Constant` is checked to ignore the attempt. `FullJitter` is checked two ways: every
value falls in `[0, inner)`, and the seeded sequence equals the exact expected
durations — the assertion the injected source makes possible. A reproducibility test
builds two independent sources with the same seed and asserts they produce identical
sequences.

Create `backoff_test.go`:

```go
package backoff

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestExponentialSequenceAndClamp(t *testing.T) {
	t.Parallel()

	b := Exponential(100*time.Millisecond, time.Second)
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second, // clamped
		time.Second, // stays clamped
	}
	for attempt, w := range want {
		if got := b(attempt); got != w {
			t.Fatalf("Exponential(%d) = %s, want %s", attempt, got, w)
		}
	}
}

func TestConstantIgnoresAttempt(t *testing.T) {
	t.Parallel()

	b := Constant(250 * time.Millisecond)
	for attempt := range 5 {
		if got := b(attempt); got != 250*time.Millisecond {
			t.Fatalf("Constant(%d) = %s, want 250ms", attempt, got)
		}
	}
}

func TestFullJitterWithinBounds(t *testing.T) {
	t.Parallel()

	inner := Exponential(100*time.Millisecond, time.Second)
	j := FullJitter(inner, rand.New(rand.NewPCG(99, 100)))
	for attempt := range 8 {
		got := j(attempt)
		if got < 0 || got >= inner(attempt) {
			t.Fatalf("FullJitter(%d) = %s, want in [0, %s)", attempt, got, inner(attempt))
		}
	}
}

func TestFullJitterExactSeededSequence(t *testing.T) {
	t.Parallel()

	inner := Exponential(100*time.Millisecond, time.Second)
	j := FullJitter(inner, rand.New(rand.NewPCG(1, 2)))
	// Exact nanosecond delays for seed NewPCG(1, 2).
	want := []time.Duration{
		76937326,
		123287244,
		313771200,
		637277262,
	}
	for attempt, w := range want {
		if got := j(attempt); got != w {
			t.Fatalf("FullJitter(%d) = %d ns, want %d ns", attempt, got, w)
		}
	}
}

func TestSeededSourcesReproduce(t *testing.T) {
	t.Parallel()

	inner := Exponential(100*time.Millisecond, time.Second)
	a := FullJitter(inner, rand.New(rand.NewPCG(7, 7)))
	b := FullJitter(inner, rand.New(rand.NewPCG(7, 7)))
	for attempt := range 10 {
		if a(attempt) != b(attempt) {
			t.Fatalf("attempt %d: sources diverged (%s vs %s)", attempt, a(attempt), b(attempt))
		}
	}
}
```

## Review

The backoff factories are correct when `Exponential` doubles until it clamps at
`cap`, `Constant` returns the same delay regardless of attempt, and `FullJitter`
draws a value strictly inside `[0, inner)`. The load-bearing design choice is
injecting the `*rand.Rand`: production seeds it from entropy to break up
synchronized retries, and the test seeds it with `NewPCG(1, 2)` so the exact
jittered sequence is assertable — a package-global source would make that assertion
flaky. `FullJitter` is a `Backoff -> Backoff` decorator, so it composes with the
other policies and drops into a retry loop as the delay strategy. Run
`go test -race`; the factories hold no shared mutable state, so the only care needed
is that each goroutine uses its own `*rand.Rand` (they are not safe for concurrent
use).

## Resources

- [math/rand/v2 package](https://pkg.go.dev/math/rand/v2) — `Rand`, `New`, `NewPCG`, `Int64N`.
- [AWS Architecture Blog: exponential backoff and jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter beats plain exponential backoff.
- [time package](https://pkg.go.dev/time) — `Duration` arithmetic and `Round`.

---

Back to [08-command-dispatcher-registry.md](08-command-dispatcher-registry.md) | Next: [10-middleware-chain-composer.md](10-middleware-chain-composer.md)
