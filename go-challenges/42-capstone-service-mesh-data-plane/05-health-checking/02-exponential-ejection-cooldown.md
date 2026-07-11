# Exercise 2: The Exponential Ejection Cooldown

When a backend is ejected, restored, and immediately ejected again, the data plane must lengthen the timeout each time rather than bounce on a fixed interval. This exercise builds that backoff policy in isolation: a pure, overflow-safe `Cooldown` that doubles per ejection up to a cap, plus a jittered variant that spreads retries out so independent clients do not synchronize into a thundering herd.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
backoff.go           Cooldown(base, max, n), CooldownJitter(base, max, n, rng)
cmd/
  demo/
    main.go          print the cooldown schedule for ejections 1..6
backoff_test.go      the doubling sequence, the cap, overflow safety, jitter bounds
```

- Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
- Implement: `Cooldown(base, max time.Duration, n int) time.Duration` and `CooldownJitter(base, max time.Duration, n int, rng *rand.Rand) time.Duration`.
- Test: `backoff_test.go` checks the `50ms, 100ms, 200ms, ...` sequence, the cap, that a large `n` never overflows, that `n < 1` behaves as `n = 1`, and that every jittered value lands in `[d/2, d]`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p exponential-ejection-cooldown/cmd/demo && cd exponential-ejection-cooldown
go mod init example.com/exponential-ejection-cooldown
go mod edit -go=1.26
```

### Why a pure function, and how to double without overflowing

Pulling the cooldown out of the detector and into a pure function buys two things. It is trivially testable — feed it `n` and read the duration, no clock and no goroutines — and it makes the one subtle hazard visible: overflow. The schedule is `base * 2^(n-1)` capped at `max`, and the obvious implementation, `time.Duration(float64(base) * math.Pow(2, float64(n-1)))`, works for small `n` but is a trap. A `time.Duration` is an `int64` of nanoseconds; double it enough times and it wraps through zero into a negative duration, which as a cooldown means "restore immediately" — the precise opposite of backing off harder. Routing `math.Pow`'s result through `float64` also loses precision at large magnitudes.

The fix is to double in integer space and check the cap *before* each doubling, never after. The loop multiplies by two only while the current value is at most `max/2`; the instant the next double would exceed `max`, it returns `max` and never performs the multiplication that could overflow. Because `max` is itself a finite `int64`, `max/2` is always representable, so the guard `d > max/2` is exact and the function is total for every `n`, including absurd ones. This is the same shape as production retry policies: a bounded exponential, with the bound enforced as a precondition of each step rather than a clamp after the fact.

The deterministic schedule is correct but synchronizing: if a hundred clients all eject the same backend at the same instant, they all compute the same cooldown and all retry at the same instant, hammering the recovering backend in a periodic wave. Jitter breaks the synchronization by choosing a random point in a band below the computed value — here the "equal jitter" band `[d/2, d]`, so every retry still respects at least half the intended backoff but no two retries land together. The randomness source is injected (`*rand.Rand`) so the policy stays a pure function of its inputs and a test can seed it and assert exact bounds.

Create `backoff.go`:

```go
package backoff

import (
	"math/rand/v2"
	"time"
)

// Cooldown returns the ejection cooldown for the nth consecutive ejection
// (n counts from 1): min(base * 2^(n-1), max).
//
// It doubles in integer space and checks the cap before each doubling, so it
// never overflows the int64 nanosecond duration regardless of how large n is.
func Cooldown(base, max time.Duration, n int) time.Duration {
	if base <= 0 {
		return 0
	}
	if n < 1 {
		n = 1
	}
	d := base
	for i := 1; i < n; i++ {
		if d > max/2 {
			return max // the next double would exceed max (and might overflow)
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// CooldownJitter returns a randomized cooldown in [d/2, d], where d is the
// deterministic Cooldown for the same inputs. The band ("equal jitter") keeps
// at least half the intended backoff while spreading retries so independent
// callers do not synchronize. rng is injected for deterministic testing.
func CooldownJitter(base, max time.Duration, n int, rng *rand.Rand) time.Duration {
	d := Cooldown(base, max, n)
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rng.Int64N(int64(d/2)+1))
}
```

`Cooldown` reads as a guarded doubling: start at `base`, and for each ejection beyond the first, refuse to double once you are past half the cap. The `n < 1` normalization makes the first ejection (`n = 1`) and any nonsense input both yield `base`, so a caller that miscounts gets the floor rather than a panic or a zero. `CooldownJitter` layers on top without duplicating the schedule: it asks `Cooldown` for the ceiling, then returns a uniform draw in `[d/2, d]`. `rand.Int64N(int64(d/2)+1)` yields `[0, d/2]`, and adding `d/2` shifts it to `[d/2, d]`; the `+1` makes the top of the band reachable.

### The runnable demo

The demo prints the schedule the way an operator would read it in a config: base 30 s, cap 5 m, and the doubling marching up to the cap and flattening there. It uses `Cooldown` (not the jittered form) precisely because a schedule should be legible and reproducible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/exponential-ejection-cooldown"
)

func main() {
	base := 30 * time.Second
	max := 5 * time.Minute
	fmt.Println("base =", base, " max =", max)
	for n := 1; n <= 6; n++ {
		fmt.Printf("ejection #%d cooldown: %v\n", n, backoff.Cooldown(base, max, n))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
base = 30s  max = 5m0s
ejection #1 cooldown: 30s
ejection #2 cooldown: 1m0s
ejection #3 cooldown: 2m0s
ejection #4 cooldown: 4m0s
ejection #5 cooldown: 5m0s
ejection #6 cooldown: 5m0s
```

### Tests

`TestCooldownSequence` pins the exact `50ms, 100ms, 200ms, 200ms, 200ms` schedule for a 50 ms base and 200 ms cap, which covers both the doubling and the cap. `TestCooldownNoOverflow` is the one that justifies the integer-space loop: a one-second base with a one-hour cap at `n = 1000` must return exactly one hour and never a wrapped negative. `TestCooldownClampsLowN` checks the `n < 1` normalization. `TestCooldownJitterBounds` seeds a deterministic PRNG and asserts a thousand draws per `n` all land inside `[d/2, d]`.

Create `backoff_test.go`:

```go
package backoff

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestCooldownSequence(t *testing.T) {
	t.Parallel()
	base := 50 * time.Millisecond
	max := 200 * time.Millisecond
	want := []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		200 * time.Millisecond,
		200 * time.Millisecond,
	}
	for i, w := range want {
		if got := Cooldown(base, max, i+1); got != w {
			t.Errorf("Cooldown(n=%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestCooldownNoOverflow(t *testing.T) {
	t.Parallel()
	if got := Cooldown(time.Second, time.Hour, 1000); got != time.Hour {
		t.Fatalf("got %v, want exactly 1h with no overflow", got)
	}
}

func TestCooldownClampsLowN(t *testing.T) {
	t.Parallel()
	if got := Cooldown(50*time.Millisecond, time.Second, 0); got != 50*time.Millisecond {
		t.Fatalf("n=0 should behave as n=1, got %v", got)
	}
}

func TestCooldownJitterBounds(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	base := 100 * time.Millisecond
	max := time.Second
	for n := 1; n <= 5; n++ {
		d := Cooldown(base, max, n)
		for i := 0; i < 1000; i++ {
			j := CooldownJitter(base, max, n, rng)
			if j < d/2 || j > d {
				t.Fatalf("jitter %v out of [%v, %v] at n=%d", j, d/2, d, n)
			}
		}
	}
}
```

## Review

The schedule is correct when it doubles, caps, and never overflows. Confirm the `base, 2*base, 4*base, ...` progression up to `max`, that it stays at `max` afterward, and — the load-bearing case — that a large `n` returns exactly `max` rather than a wrapped negative duration, which is what the integer-space loop with a pre-double cap check guarantees. Confirm `n < 1` yields `base`, and that every `CooldownJitter` draw respects `[d/2, d]` so backoff is never undercut by more than half.

Common mistakes for this feature. Computing `base * math.Pow(2, n-1)` and clamping afterward overflows for large `n`: the multiplication wraps before the clamp ever runs, turning a long cooldown into a negative one. Checking the cap after doubling instead of before still performs the overflowing multiply. Omitting jitter leaves the deterministic schedule to synchronize independent clients into a periodic thundering herd against the recovering backend. And seeding the jitter from a global, unseeded source makes the test non-deterministic — inject the PRNG so the bounds assertion is reproducible.

## Resources

- [AWS: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — Marc Brooker's canonical analysis of why capped exponential backoff needs jitter and how the bands compare.
- [Envoy: outlier detection](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier) — the `base_ejection_time` × multiplier ejection schedule this exercise models.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — the injectable PRNG (`rand.Rand`, `Int64N`, `NewPCG`) used for deterministic jitter.

---

Back to [01-active-state-machine.md](01-active-state-machine.md) | Next: [03-outlier-ejection.md](03-outlier-ejection.md)
