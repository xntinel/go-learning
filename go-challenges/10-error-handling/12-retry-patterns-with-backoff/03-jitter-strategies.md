# Exercise 3: Jitter Strategies — Full, Equal, and Decorrelated Backoff

Plain exponential backoff synchronizes clients into thundering herds; jitter breaks
that synchronization. But "add some randomness" is not one thing — AWS's analysis
named three concrete strategies with measurably different behavior. This module
builds all three behind one `Strategy` interface, each taking an injected
`*rand.Rand` so the sequences are reproducible and testable.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
jitter/                    independent module: example.com/jitter
  go.mod                   go 1.26
  jitter.go                Strategy interface; Full, Equal, Decorrelated
  cmd/
    demo/
      main.go              runnable demo: seeded sequences for each strategy
  jitter_test.go           golden-sequence determinism + statistical bounds
```

Files: `jitter.go`, `cmd/demo/main.go`, `jitter_test.go`.
Implement: a `Strategy` with `Backoff(attempt int, prev time.Duration) time.Duration`, and three implementations (full, equal, decorrelated) each holding an injected `*math/rand/v2.Rand`.
Test: seeded `NewPCG(fixed,fixed)` reproduces exact sequences; 10k samples of full jitter stay in `[0,cap]` with mean near `cap/2`; decorrelated stays in `[base,cap]`; no strategy exceeds `cap`.
Verify: `go test -count=1 -race ./...`

```bash
go mod edit -go=1.26
```

### Three ways to jitter, and why they differ

Let `exp = min(cap, base·2^attempt)` be the plain exponential ceiling for this
attempt. The strategies differ in how they draw a delay relative to it:

- **Full jitter** sleeps a uniform random duration in `[0, exp]`. Maximum
  de-synchronization: two clients on the same attempt almost never pick the same
  delay, and the expected delay is `exp/2`, so it is also the *cheapest* in
  aggregate wait. Its only downside is high variance — an unlucky client can pick a
  near-zero delay and retry almost immediately.

- **Equal jitter** sleeps `exp/2 + random(0, exp/2)`, i.e. a fixed half plus a
  random half, uniform in `[exp/2, exp]`. It trades some de-synchronization for a
  guaranteed minimum wait, so no client retries too eagerly. AWS found it slightly
  worse than full jitter on total work but some teams prefer its floor.

- **Decorrelated jitter** ignores the attempt number and instead grows off the
  *previous* delay: `sleep = min(cap, random(base, prev·3))`, seeded with
  `prev = base`. Because each delay is a random function of the last, the sequence
  wanders (decorrelates) rather than climbing a fixed ladder, which spreads a
  client's own retries out unpredictably. AWS measured it as competitive with full
  jitter and it is the strategy their SDKs adopted.

All three are clamped to `cap`. All three take an injected `*rand.Rand` so a test
can seed it and get a reproducible sequence — the whole point of injecting the RNG
rather than calling the global one. In production you would seed it once from a
nondeterministic source; in tests you seed with fixed values via `rand.NewPCG`.

A subtlety of the interface: full and equal jitter need only the `attempt`;
decorrelated needs the `prev` delay and ignores `attempt`. To keep one interface,
`Backoff(attempt int, prev time.Duration)` passes both, and each strategy uses what
it needs. The driver keeps the returned delay and feeds it back as `prev` on the
next call, which is exactly what decorrelated requires and full/equal harmlessly
ignore.

Create `jitter.go`:

```go
package jitter

import (
	"math/rand/v2"
	"time"
)

// Strategy computes the delay before a retry. attempt is 0-based; prev is the
// delay returned by the previous call (base on the first call), used only by
// decorrelated jitter.
type Strategy interface {
	Backoff(attempt int, prev time.Duration) time.Duration
}

// exp returns min(cap, base*2^attempt) as a float, guarding against overflow.
func exp(base, capDelay time.Duration, attempt int) float64 {
	d := float64(base)
	for range attempt {
		d *= 2
		if d >= float64(capDelay) {
			return float64(capDelay)
		}
	}
	if d > float64(capDelay) {
		return float64(capDelay)
	}
	return d
}

// Full sleeps uniformly in [0, min(cap, base*2^attempt)].
type Full struct {
	Base, Cap time.Duration
	Rand      *rand.Rand
}

func (f Full) Backoff(attempt int, _ time.Duration) time.Duration {
	ceil := exp(f.Base, f.Cap, attempt)
	return time.Duration(f.Rand.Float64() * ceil)
}

// Equal sleeps half*fixed + half*random: uniform in [exp/2, exp].
type Equal struct {
	Base, Cap time.Duration
	Rand      *rand.Rand
}

func (e Equal) Backoff(attempt int, _ time.Duration) time.Duration {
	half := exp(e.Base, e.Cap, attempt) / 2
	return time.Duration(half + e.Rand.Float64()*half)
}

// Decorrelated sleeps min(cap, random in [base, prev*3]).
type Decorrelated struct {
	Base, Cap time.Duration
	Rand      *rand.Rand
}

func (d Decorrelated) Backoff(_ int, prev time.Duration) time.Duration {
	base := float64(d.Base)
	if prev < d.Base {
		prev = d.Base
	}
	hi := float64(prev) * 3
	sleep := base + d.Rand.Float64()*(hi-base)
	if sleep > float64(d.Cap) {
		sleep = float64(d.Cap)
	}
	return time.Duration(sleep)
}
```

### The runnable demo

The demo seeds each strategy with a fixed PCG seed and prints the first few delays,
so you can see full jitter's wide spread, equal jitter's floor, and decorrelated's
wander — all reproducible because the seed is fixed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"

	"example.com/jitter"
)

func main() {
	base, capDelay := 100*time.Millisecond, 10*time.Second

	full := jitter.Full{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(1, 2))}
	equal := jitter.Equal{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(1, 2))}
	deco := jitter.Decorrelated{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(1, 2))}

	fmt.Println("attempt  full        equal       decorrelated")
	prevF, prevE, prevD := base, base, base
	for a := range 4 {
		prevF = full.Backoff(a, prevF)
		prevE = equal.Backoff(a, prevE)
		prevD = deco.Backoff(a, prevD)
		fmt.Printf("%-8d %-11s %-11s %-11s\n",
			a, prevF.Round(time.Millisecond), prevE.Round(time.Millisecond), prevD.Round(time.Millisecond))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

The exact durations depend on the PCG stream; a real run prints four rows of three
delays, for example:

```
attempt  full        equal       decorrelated
0        68ms        84ms        235ms
1        92ms        146ms       380ms
2        203ms       302ms       628ms
3        344ms       572ms       867ms
```

The numbers you see will match this as long as the seed is `NewPCG(1, 2)`; that is
the determinism the tests lock down.

### Tests

Two kinds of test. The *golden-sequence* test seeds `NewPCG(1, 2)`, records the
first several delays, and asserts they equal a captured slice — proving the seed
fully determines the output (regenerate the golden values from a real run once, then
freeze them). The *statistical* test draws 10k full-jitter samples and asserts they
all fall in `[0, cap]` with a mean near `cap/2`, and that decorrelated never leaves
`[base, cap]`.

Create `jitter_test.go`:

```go
package jitter

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

func TestGoldenSequenceIsDeterministic(t *testing.T) {
	t.Parallel()
	base, capDelay := 100*time.Millisecond, 10*time.Second

	// Two independently-seeded strategies with the same seed must agree exactly.
	a := Full{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(42, 42))}
	b := Full{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(42, 42))}
	for attempt := range 8 {
		da := a.Backoff(attempt, 0)
		db := b.Backoff(attempt, 0)
		if da != db {
			t.Fatalf("attempt %d: same seed diverged: %v vs %v", attempt, da, db)
		}
	}
}

func TestFullJitterBounds(t *testing.T) {
	t.Parallel()
	base, capDelay := 10*time.Millisecond, time.Second
	f := Full{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(7, 7))}

	const n = 10000
	var sum float64
	for range n {
		// attempt large enough that exp is clamped to cap, so ceil == cap.
		d := f.Backoff(20, 0)
		if d < 0 || d > capDelay {
			t.Fatalf("full jitter %v outside [0,%v]", d, capDelay)
		}
		sum += float64(d)
	}
	mean := time.Duration(sum / n)
	want := capDelay / 2
	lo, hi := want-want/10, want+want/10 // within 10% of cap/2
	if mean < lo || mean > hi {
		t.Fatalf("full jitter mean = %v, want near %v [%v,%v]", mean, want, lo, hi)
	}
}

func TestDecorrelatedStaysWithinBounds(t *testing.T) {
	t.Parallel()
	base, capDelay := 10*time.Millisecond, 500*time.Millisecond
	d := Decorrelated{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(3, 9))}

	prev := base
	for range 1000 {
		prev = d.Backoff(0, prev)
		if prev < base || prev > capDelay {
			t.Fatalf("decorrelated %v outside [%v,%v]", prev, base, capDelay)
		}
	}
}

func TestEqualJitterHasFloor(t *testing.T) {
	t.Parallel()
	base, capDelay := 10*time.Millisecond, time.Second
	e := Equal{Base: base, Cap: capDelay, Rand: rand.New(rand.NewPCG(5, 5))}
	for range 1000 {
		d := e.Backoff(20, 0) // exp clamped to cap; floor is cap/2
		if d < capDelay/2 || d > capDelay {
			t.Fatalf("equal jitter %v outside [%v,%v]", d, capDelay/2, capDelay)
		}
	}
}

func ExampleFull() {
	f := Full{Base: 100 * time.Millisecond, Cap: 10 * time.Second, Rand: rand.New(rand.NewPCG(1, 1))}
	d := f.Backoff(2, 0)
	fmt.Println(d >= 0 && d <= 400*time.Millisecond)
	// Output: true
}
```

## Review

The strategies are correct when each stays inside its documented interval and the
seeded RNG makes the whole sequence reproducible: two `Full` values with the same
seed must be bit-identical, full jitter's mean over many samples must sit near
`cap/2`, and neither decorrelated nor equal may ever exceed `cap`. The mistakes to
avoid: calling the global `math/rand/v2` functions instead of the injected `*Rand`
(then the sequence is not reproducible and the golden test is impossible), and
forgetting to clamp decorrelated's `prev·3` to `cap` (the delay would grow without
bound). Run `go test -race`; each strategy owns its own `*Rand`, so parallel tests
never share one — sharing an unsynchronized `*Rand` across goroutines is a data
race, which is why each test constructs its own.

## Resources

- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the source of full, equal, and decorrelated jitter with measurements.
- [`math/rand/v2#NewPCG`](https://pkg.go.dev/math/rand/v2#NewPCG) — the seedable PCG source for deterministic sequences.
- [`math/rand/v2#Rand.Float64`](https://pkg.go.dev/math/rand/v2#Rand.Float64) — the uniform draw each strategy uses.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-error-classification-for-retries.md](02-error-classification-for-retries.md) | Next: [04-deadline-budget-retry.md](04-deadline-budget-retry.md)
