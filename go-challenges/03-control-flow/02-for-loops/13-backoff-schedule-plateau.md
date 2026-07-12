# Exercise 13: Precomputing a Backoff Schedule with a Plateau Break

**Nivel: Intermedio** — validacion rapida (un test corto).

Exercise 2 retried live, sleeping through an injected clock between attempts.
This module does the math half of that problem instead: given a fixed number
of attempts, precompute the whole delay schedule up front as a plain slice —
no clock, no waiting, pure arithmetic. The interesting part is a counted loop
that recognizes when doubling has stopped mattering and breaks early.

This module is fully self-contained: its own `go mod init` and one test file.

## What you'll build

```text
backoffschedule/              module example.com/backoffschedule
  go.mod                      go 1.24
  schedule.go                 Schedule(maxAttempts, base, maxDelay) []time.Duration
  schedule_test.go             equivalence vs. a naive reference, exact values
```

- Files: `schedule.go`, `schedule_test.go`.
- Implement: `Schedule(maxAttempts int, base, maxDelay time.Duration) []time.Duration` — a `for attempt := range maxAttempts` loop that doubles `delay` each iteration and, the moment `delay >= maxDelay`, fills the remainder of the slice with `maxDelay` and `break`s instead of continuing to double.
- Test: an equivalence check against a naive reference that clamps every element with `min` and never breaks, across five shapes (reaches cap exactly, base already over cap, never reaches cap, zero attempts, single attempt); a hand-checked exact-values case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the loop can stop doubling early

`delay *= 2` only ever grows or plateaus, it never shrinks. So once
`delay >= maxDelay`, every remaining attempt would clamp to exactly
`maxDelay` anyway — computing them one at a time gains nothing, and for a
large `maxAttempts` (or a small `base`), repeated doubling would eventually
overflow `time.Duration`. The loop detects the plateau at the top of each
iteration, fills every remaining slot with `maxDelay` in a small inner loop,
and `break`s out of the outer one. This is a counted loop whose *early exit*
is itself the optimization, and the thing that has to be proved is that the
optimization changes nothing observable — hence the naive-reference
equivalence test.

Create `schedule.go`:

```go
package backoff

import "time"

// Schedule computes maxAttempts backoff delays doubling from base and clamped
// to maxDelay: base, base*2, base*4, ... capped. A retrier that has already
// decided how many attempts it will make can precompute the whole schedule
// once instead of doing time math on every retry.
//
// Once a delay reaches maxDelay it stays there forever (doubling never
// decreases it), so the loop detects that plateau and fills the remainder
// directly instead of continuing to double a value that has nothing left to
// do and would eventually overflow time.Duration for a large maxAttempts.
func Schedule(maxAttempts int, base, maxDelay time.Duration) []time.Duration {
	sched := make([]time.Duration, maxAttempts)
	delay := base
	for attempt := range maxAttempts {
		if delay >= maxDelay {
			for i := attempt; i < maxAttempts; i++ {
				sched[i] = maxDelay
			}
			break
		}
		sched[attempt] = delay
		delay *= 2
	}
	return sched
}
```

### Tests

`TestScheduleMatchesNaive` is the load-bearing test: `naiveSchedule` computes
the same sequence the slow way, clamping every element with `min` and never
breaking early, and the test asserts `Schedule` produces byte-for-byte the
same slice across five shapes — including a `base` that already exceeds
`maxDelay` on attempt zero, and a schedule that never reaches the cap at all.
`TestScheduleExactValues` is a hand-checked sanity case: base 100ms doubling
to 200, 400, then hitting the 800ms cap and staying there.

Create `schedule_test.go`:

```go
package backoff

import (
	"testing"
	"time"
)

// naiveSchedule computes the same sequence without the early break, clamping
// every element with min instead of detecting the plateau. It is the
// reference the optimized Schedule must match exactly.
func naiveSchedule(maxAttempts int, base, maxDelay time.Duration) []time.Duration {
	sched := make([]time.Duration, maxAttempts)
	delay := base
	for i := range maxAttempts {
		sched[i] = min(delay, maxDelay)
		delay *= 2
	}
	return sched
}

func TestScheduleMatchesNaive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		maxAttempts    int
		base, maxDelay time.Duration
	}{
		{"reaches cap exactly", 6, 100 * time.Millisecond, 800 * time.Millisecond},
		{"base already over cap", 3, 1000 * time.Millisecond, 500 * time.Millisecond},
		{"never reaches cap", 4, 10 * time.Millisecond, 10 * time.Second},
		{"zero attempts", 0, 100 * time.Millisecond, 800 * time.Millisecond},
		{"single attempt", 1, 100 * time.Millisecond, 800 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Schedule(tc.maxAttempts, tc.base, tc.maxDelay)
			want := naiveSchedule(tc.maxAttempts, tc.base, tc.maxDelay)

			if len(got) != len(want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("Schedule[%d] = %v, want %v (full: got=%v want=%v)", i, got[i], want[i], got, want)
				}
			}
		})
	}
}

func TestScheduleExactValues(t *testing.T) {
	t.Parallel()

	got := Schedule(6, 100*time.Millisecond, 800*time.Millisecond)
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Schedule[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
```

## Review

`Schedule` is correct when it produces exactly what a naive, non-optimized
version would, and `TestScheduleMatchesNaive` proves that across every shape
that matters: a normal plateau, a `base` that starts over the cap, a
schedule that never plateaus, and the zero/one-attempt edges. The break is
only safe because doubling is monotonic — the moment `delay >= maxDelay` is
true once, it is true for every later attempt too, so filling the remainder
with a constant and stopping is provably equivalent to continuing the loop.
Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counted `for range n` form used here.
- [time package](https://pkg.go.dev/time) — `time.Duration` arithmetic and the `Millisecond`/`Second` constants.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-sliding-window-rate-counter.md](12-sliding-window-rate-counter.md) | Next: [14-pricing-fixed-point-convergence.md](14-pricing-fixed-point-convergence.md)
