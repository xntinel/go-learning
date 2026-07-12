# Exercise 10: Fuzz An Exponential-Backoff Policy For Bounded Delays

A retry loop computes how long to wait before the next attempt with capped
exponential backoff. The naive `base << attempt` overflows for a large attempt
count, and an overflowed shift can wrap negative — a retry that sleeps a nonsense
duration or, worse, a negative one. This module builds a `Backoff` policy and
fuzzes the numeric-bounds property across a wide range of attempts: the delay is
always within `[0, max]`, for every attempt including negative and absurdly large.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
backoff/                   independent module: example.com/backoff
  go.mod                   module path
  backoff.go               Backoff(attempt int, base, max time.Duration) time.Duration
  cmd/
    demo/
      main.go              print the delay schedule for attempts 0..6
  backoff_test.go          TestBackoffSchedule, FuzzBackoffBounds, Example
```

Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
Implement: `Backoff(attempt int, base, max time.Duration) time.Duration` with an
overflow-safe shift.
Test: a schedule table test; `FuzzBackoffBounds` asserting `0 <= d <= max` across
`(int, int64, int64)` inputs.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzBackoffBounds -fuzztime=2s`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/06-fuzz-testing/10-backoff-bounds/cmd/demo
cd go-solutions/12-testing-ecosystem/06-fuzz-testing/10-backoff-bounds
```

### Detecting shift overflow before it happens

Capped exponential backoff is "double the delay each attempt, but never exceed a
ceiling": attempt 0 waits `base`, attempt 1 waits `2*base`, and so on until the
value reaches `max`, after which every attempt waits `max`. The obvious
implementation, `base << attempt`, is a landmine. `time.Duration` is an `int64`;
shift it far enough and the value marches into the sign bit and becomes negative,
so a high attempt count produces a *negative* delay that a naive `min(d, max)`
happily returns. That is a real incident: a retry loop that busy-spins because its
computed backoff went negative.

The fix is to detect the overflow *before* performing the shift, using
`math/bits.LeadingZeros64`. `base << attempt` stays a positive `int64` exactly
while `attempt < bits.LeadingZeros64(uint64(base))` — that many high bits are zero,
so the shift cannot reach the sign bit. When `attempt` is at or beyond that
threshold, the shift would overflow, so the policy skips it and returns `max`
directly. Combined with clamping non-positive `base` and negative `attempt`, the
result is provably in `[0, max]` for every input — which is exactly what the fuzz
target checks across attempts from negative through `1000` and beyond.

The fuzz property is a pure numeric bound: `0 <= Backoff(attempt, base, max) <=
max`, clamping `max` itself to zero if the fuzzer hands you a negative ceiling.
Seeding with `attempt` values of `0`, `1`, `63` (the exact shift-overflow edge for
a small base), `1000`, and negatives is what steers the engine at the boundaries
where the sign-bit and shift-overflow bugs live.

Create `backoff.go`:

```go
package backoff

import (
	"math/bits"
	"time"
)

// Backoff returns the delay before the given retry attempt using capped
// exponential backoff: base, 2*base, 4*base, ... never exceeding max, and never
// negative. attempt < 0 is treated as 0; a non-positive base yields 0.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if max < 0 {
		max = 0
	}
	if attempt < 0 {
		attempt = 0
	}
	d := max
	// base<<attempt stays a positive int64 only while attempt is below the
	// number of leading zero bits in base; beyond that the shift overflows.
	if attempt < bits.LeadingZeros64(uint64(base)) {
		if shifted := base << uint(attempt); shifted > 0 && shifted < max {
			d = shifted
		}
	}
	return d
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/backoff"
)

func main() {
	base := 100 * time.Millisecond
	max := 2 * time.Second
	for attempt := 0; attempt <= 6; attempt++ {
		fmt.Printf("attempt %d -> %v\n", attempt, backoff.Backoff(attempt, base, max))
	}
	fmt.Printf("attempt 1000 -> %v\n", backoff.Backoff(1000, base, max))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 0 -> 100ms
attempt 1 -> 200ms
attempt 2 -> 400ms
attempt 3 -> 800ms
attempt 4 -> 1.6s
attempt 5 -> 2s
attempt 6 -> 2s
attempt 1000 -> 2s
```

### Tests

Create `backoff_test.go`:

```go
package backoff

import (
	"fmt"
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	max := 2 * time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{4, 1600 * time.Millisecond},
		{5, 2 * time.Second},    // 3.2s capped to max
		{1000, 2 * time.Second}, // overflow -> capped to max
		{-1, 100 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("attempt=%d", tc.attempt), func(t *testing.T) {
			t.Parallel()
			if got := Backoff(tc.attempt, base, max); got != tc.want {
				t.Fatalf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

func FuzzBackoffBounds(f *testing.F) {
	seeds := []struct {
		attempt int
		base    int64
		max     int64
	}{
		{0, int64(100 * time.Millisecond), int64(2 * time.Second)},
		{1, int64(time.Second), int64(time.Minute)},
		{63, 1, int64(time.Hour)},
		{1000, int64(time.Second), int64(time.Minute)},
		{-1, int64(time.Second), int64(time.Minute)},
		{5, int64(time.Second), -1},
	}
	for _, s := range seeds {
		f.Add(s.attempt, s.base, s.max)
	}
	f.Fuzz(func(t *testing.T, attempt int, baseNanos, maxNanos int64) {
		base := time.Duration(baseNanos)
		max := time.Duration(maxNanos)
		d := Backoff(attempt, base, max)

		hi := max
		if hi < 0 {
			hi = 0
		}
		if d < 0 || d > hi {
			t.Fatalf("Backoff(%d, %v, %v) = %v, outside [0, %v]", attempt, base, max, d, hi)
		}
	})
}

func Example() {
	fmt.Println(Backoff(3, 100*time.Millisecond, time.Minute))
	// Output: 800ms
}
```

## Review

`Backoff` is correct when the returned delay is always within `[0, max]` for every
attempt count the fuzzer throws at it — including negative attempts and attempts
large enough that a naive `base << attempt` would wrap into the sign bit. The line
that earns the module is the `attempt < bits.LeadingZeros64(uint64(base))` guard,
which detects the shift overflow *before* it corrupts the value rather than trying
to repair a negative result afterward. `TestBackoffSchedule` pins the exact
doubling-then-capping curve; `FuzzBackoffBounds` proves the bound holds for the
attempt counts and ceilings you did not tabulate, which is where the sign-bit bug
hides. Run `go test -race ./...`, then
`go test -fuzz=FuzzBackoffBounds -fuzztime=2s`.

## Resources

- [`math/bits.LeadingZeros64`](https://pkg.go.dev/math/bits#LeadingZeros64) — the overflow-threshold computation behind the safe shift.
- [`time.Duration`](https://pkg.go.dev/time#Duration) — the `int64` nanosecond type whose sign bit the overflow corrupts.
- [Go Fuzzing reference](https://go.dev/doc/security/fuzz/) — numeric-bounds fuzzing across a wide integer range.

---

Back to [09-stateful-token-bucket.md](09-stateful-token-bucket.md) | Next: [../07-test-fixtures-and-testdata/00-concepts.md](../07-test-fixtures-and-testdata/00-concepts.md)
