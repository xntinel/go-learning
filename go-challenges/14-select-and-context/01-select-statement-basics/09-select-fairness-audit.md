# Exercise 9: Prove Pseudo-Random Fairness — Audit That No Channel Starves

The whole lesson rests on one runtime guarantee: when several `select` cases are
ready, the runtime makes a *uniform pseudo-random* choice, and that is what stops
one source from starving another over a long run. This module turns that claim into
a measurement. It runs a two-ready-case `select` hundreds of thousands of times,
returns the selection distribution, and asserts the split is near 50/50 — the
empirical basis for the rule "never assume case order implies priority."

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
fairness/                       module example.com/fairness
  go.mod                        go 1.26
  fairness.go                   Audit(iterations) Distribution; WithinTolerance, MinCount
  cmd/
    demo/
      main.go                   run an audit; print balance verdicts
  fairness_test.go              roughly-uniform, neither-starves, Example
```

Files: `fairness.go`, `cmd/demo/main.go`, `fairness_test.go`.
Implement: `Audit(iterations int) Distribution` over two always-ready cases, plus `WithinTolerance` and `MinCount` helpers.
Test: over ≥100k iterations each case is chosen within a tolerance band of 50%, and neither case is starved.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/01-select-statement-basics/09-select-fairness-audit/cmd/demo
cd go-solutions/14-select-and-context/01-select-statement-basics/09-select-fairness-audit
```

## Measuring the guarantee instead of trusting it

`Audit` sets up two buffered channels and, on every iteration, makes both receive
cases ready by putting a value in each, then runs a two-case `select` and records
which case won. Because both cases are always ready, the winner is decided purely
by the runtime's arbitration — this isolates the fairness mechanism with no timing
or scheduling noise mixed in. After the `select`, the case that was *not* chosen
still holds its value, so `Audit` drains both channels with non-blocking receives
before the next iteration refills them; otherwise the next send on the un-drained
channel would block.

Over a large number of iterations the counts converge to a near-even split. The
correct assertion is a *tolerance band*, not an exact ratio: the choice is
pseudo-random, so demanding exactly 50.000% would be over-fitting and would flake.
Asserting "each case lands within, say, 45%-55%" documents the guarantee honestly —
it proves neither case is starving without pretending the distribution is
deterministic. With 100k+ iterations the real deviation is a fraction of a percent,
so a 5% band essentially never trips by chance while still catching a genuinely
broken, biased selection.

`MinCount` backs the anti-starvation claim directly: it returns the smaller of the
two counts, and a test asserts it is comfortably above zero. A `select` that
secretly favored the first-listed case would show up here as a starved second case.

Create `fairness.go`:

```go
package fairness

import "math"

// Distribution records how a two-case select chose over an audit run.
type Distribution struct {
	Counts [2]int // Counts[i] is how often case i was chosen
	Total  int
}

// Audit runs a select over two always-ready receive cases `iterations` times and
// returns how often each case was chosen. Both cases are ready on every iteration,
// so the outcome is decided solely by the runtime's uniform pseudo-random choice.
func Audit(iterations int) Distribution {
	a := make(chan int, 1)
	b := make(chan int, 1)

	var d Distribution
	for range iterations {
		a <- 0 // make both receive cases ready
		b <- 0
		select {
		case <-a:
			d.Counts[0]++
		case <-b:
			d.Counts[1]++
		}
		// The unchosen case still holds its value; drain both before refilling.
		select {
		case <-a:
		default:
		}
		select {
		case <-b:
		default:
		}
	}
	d.Total = iterations
	return d
}

// WithinTolerance reports whether case 0's share of the total is within tol of the
// ideal 0.5 (so tol == 0.05 means the 45%-55% band).
func (d Distribution) WithinTolerance(tol float64) bool {
	if d.Total == 0 {
		return false
	}
	share := float64(d.Counts[0]) / float64(d.Total)
	return math.Abs(share-0.5) <= tol
}

// MinCount returns the smaller of the two case counts. A value well above zero is
// evidence that neither case was starved.
func (d Distribution) MinCount() int {
	if d.Counts[0] < d.Counts[1] {
		return d.Counts[0]
	}
	return d.Counts[1]
}
```

## The runnable demo

The exact counts vary run to run, so the demo prints verdicts (balance and
non-starvation), which are deterministic for a large audit, rather than raw
numbers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fairness"
)

func main() {
	d := fairness.Audit(200_000)
	fmt.Printf("iterations: %d\n", d.Total)
	fmt.Printf("balanced within 5%%: %v\n", d.WithinTolerance(0.05))
	fmt.Printf("neither starved: %v\n", d.MinCount() > 0)
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
iterations: 200000
balanced within 5%: true
neither starved: true
```

## Tests

`TestSelectionIsRoughlyUniform` runs a large audit and asserts case 0's share sits
inside a 5% tolerance band of 50% — the documented pseudo-random-fair-choice
guarantee, asserted without over-fitting to an exact ratio. `TestNeitherCaseStarves`
asserts the minimum count is far above zero (at least a quarter of the total, a
deliberately generous margin), the direct anti-starvation claim. `ExampleAudit`
pins a deterministic boolean under a wide tolerance. These tests are inherently
probabilistic by nature, but the bands are wide enough relative to the iteration
count that they do not flake; they still run cleanly under `-race`.

Create `fairness_test.go`:

```go
package fairness

import (
	"fmt"
	"testing"
)

func TestSelectionIsRoughlyUniform(t *testing.T) {
	t.Parallel()

	const iterations = 200_000
	d := Audit(iterations)

	if d.Total != iterations {
		t.Fatalf("Total = %d, want %d", d.Total, iterations)
	}
	if !d.WithinTolerance(0.05) {
		t.Fatalf("selection not within 5%% of 50/50: counts = %v of %d", d.Counts, d.Total)
	}
}

func TestNeitherCaseStarves(t *testing.T) {
	t.Parallel()

	const iterations = 200_000
	d := Audit(iterations)

	// A generous floor: with a fair choice each case gets ~50%; a quarter is a
	// wide margin that only a genuinely starving select would fall below.
	floor := iterations / 4
	if d.MinCount() < floor {
		t.Fatalf("a case starved: counts = %v, min %d below floor %d", d.Counts, d.MinCount(), floor)
	}
}

func ExampleAudit() {
	d := Audit(100_000)
	// A wide tolerance makes this deterministic despite the pseudo-random choice.
	fmt.Println(d.Total, d.WithinTolerance(0.1))
	// Output: 100000 true
}
```

## Review

The audit is doing its job when a large run lands well inside the tolerance band and
`MinCount` stays far from zero — concrete evidence that the runtime's choice among
ready cases is uniform, not order-biased. The design discipline here is statistical
honesty: assert a *band*, never an exact ratio, and size the band generously
relative to the iteration count so the test measures the guarantee without flaking
on the pseudo-random tail. This is the empirical footing for the lesson's recurring
rule — listing order carries no priority in a `select`, so any priority you need
must be built explicitly, which is where lesson 08 of this chapter picks up.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — "a uniform pseudo-random selection" among ready cases, stated normatively.
- [A Tour of Go: Select](https://go.dev/tour/concurrency/5) — the introductory model this module measures.
- [math.Abs](https://pkg.go.dev/math#Abs) — the tolerance computation in `WithinTolerance`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-pubsub-broadcast-dispatcher.md](08-pubsub-broadcast-dispatcher.md) | Next: [../02-select-with-default/00-concepts.md](../02-select-with-default/00-concepts.md)
