# Exercise 6: Requests-Per-Second Aggregator: Integer Division Truncation

The flattest dashboards in production are drawn by integer division. You will build
a metrics rollup that computes requests-per-second and an error rate over a window,
and fix the classic bug where `count / windowSeconds` and `errors / total`
truncate to `0` or `1` because both operands are integers.

## What you'll build

```text
rollup/                     independent module: example.com/rollup
  go.mod                    go 1.26
  rollup.go                 type Window; Record; Snapshot -> float64 RPS/ErrorRate
  cmd/
    demo/
      main.go               records counts, prints the snapshot
  rollup_test.go            truncation contrast, error-rate, zero-window guard
```

Files: `rollup.go`, `cmd/demo/main.go`, `rollup_test.go`.
Implement: a `Window` accumulating request and error counts, and
`Snapshot() Stats` returning `float64` `RPS`, `ErrorRate`, and an integer
basis-points figure.
Test: `5 requests / 10s == 0.5` (not `0`), error rate `3/1000 == 0.003`, a
zero-window guard, and a `var _ float64 = s.RPS` pin.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

## Why the obvious expression is wrong

`RPS = requests / windowSeconds` reads correctly and computes wrongly. If
`requests` is an `int64` count and `windowSeconds` is derived as an integer, the
division is integer division: `5 / 10` is `0`, `19 / 10` is `1`, all fractional
information gone. A dashboard fed this value shows `0 req/s` for any endpoint doing
fewer than one request per second, which is most of them. The same trap hits the
error rate: `errors / total` with both `int64` collapses `3 / 1000` to `0` and
`999 / 1000` to `0` as well.

The fix is to move to floating point *before* dividing: convert one operand to
`float64`, and the whole expression is evaluated in `float64`.
`float64(requests) / window.Seconds()` is correct because `time.Duration.Seconds()`
already returns a `float64`, so the numerator conversion is what makes the division
floating. `float64(errors) / float64(total)` gives the true ratio. For a reporting
figure that must be an integer — error rate in basis points (hundredths of a
percent) — you compute the float ratio, multiply by `10000`, and `math.Round` to
the nearest integer, converting to `int` only at the very end.

Two guards matter. The denominator can be zero: a window of zero duration, or a
`total` of zero requests. Integer division by zero *panics*; float division by zero
yields `+Inf` or `NaN`, which then poisons every downstream aggregate. So
`Snapshot` checks for an empty window and a zero total and returns `0` rates rather
than dividing. The `Stats` fields are `float64` precisely so the caller cannot
re-introduce truncation by storing a rate in an `int`.

Create `rollup.go`:

```go
package rollup

import (
	"math"
	"time"
)

// Window accumulates request and error counts over an elapsed duration.
type Window struct {
	requests int64
	errors   int64
	elapsed  time.Duration
}

// NewWindow starts a window covering the given elapsed duration.
func NewWindow(elapsed time.Duration) *Window {
	return &Window{elapsed: elapsed}
}

// Record adds one observation: ok=false counts as an error too.
func (w *Window) Record(ok bool) {
	w.requests++
	if !ok {
		w.errors++
	}
}

// Stats is a computed snapshot. RPS and ErrorRate are float64 so a caller cannot
// truncate them by accident; ErrorRateBps is the rounded basis-points figure a
// dashboard tile wants as an integer.
type Stats struct {
	RPS          float64
	ErrorRate    float64
	ErrorRateBps int
}

// Snapshot computes the rates in floating point. It converts a count to float64
// BEFORE dividing, and guards both zero denominators.
func (w *Window) Snapshot() Stats {
	var s Stats

	secs := w.elapsed.Seconds() // float64
	if secs > 0 {
		s.RPS = float64(w.requests) / secs // float division, not int
	}

	if w.requests > 0 {
		s.ErrorRate = float64(w.errors) / float64(w.requests)
		s.ErrorRateBps = int(math.Round(s.ErrorRate * 10000))
	}

	return s
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/rollup"
)

func main() {
	w := rollup.NewWindow(10 * time.Second)
	for range 5 {
		w.Record(true)
	}
	w.Record(false) // one error, six requests total

	s := w.Snapshot()
	fmt.Printf("rps=%.2f error_rate=%.4f error_bps=%d\n", s.RPS, s.ErrorRate, s.ErrorRateBps)

	// The truncating version, for contrast:
	naive := int64(6) / int64(10)
	fmt.Printf("naive int rps (wrong) = %d\n", naive)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rps=0.60 error_rate=0.1667 error_bps=1667
naive int rps (wrong) = 0
```

The second line is the exact truncation this module exists to prevent: `6 / 10` in
integer arithmetic is `0`, while the snapshot's float `RPS` is `0.60`.

## Tests

The key test contrasts the truncating integer expression with the correct float
one on the same numbers, so the difference between `0` and `0.5` is explicit.
Others cover the error rate, the basis-points rounding, and the zero-window and
zero-total guards.

Create `rollup_test.go`:

```go
package rollup

import (
	"testing"
	"time"
)

func TestRPSIsNotTruncated(t *testing.T) {
	t.Parallel()

	w := NewWindow(10 * time.Second)
	for range 5 {
		w.Record(true)
	}

	s := w.Snapshot()
	var _ float64 = s.RPS // pin

	if truncated := int64(5) / int64(10); truncated != 0 {
		t.Fatalf("sanity: expected integer division to truncate to 0, got %d", truncated)
	}
	if s.RPS != 0.5 {
		t.Fatalf("RPS = %v, want 0.5 (float division, not truncated)", s.RPS)
	}
}

func TestErrorRate(t *testing.T) {
	t.Parallel()

	w := NewWindow(time.Second)
	for range 997 {
		w.Record(true)
	}
	for range 3 {
		w.Record(false)
	}

	s := w.Snapshot()
	if s.ErrorRate != 0.003 {
		t.Fatalf("ErrorRate = %v, want 0.003", s.ErrorRate)
	}
	if s.ErrorRateBps != 30 {
		t.Fatalf("ErrorRateBps = %d, want 30", s.ErrorRateBps)
	}
}

func TestZeroWindowNoPanic(t *testing.T) {
	t.Parallel()

	w := NewWindow(0)
	w.Record(true)

	s := w.Snapshot() // must not panic on divide-by-zero window
	if s.RPS != 0 {
		t.Fatalf("RPS = %v, want 0 for zero window", s.RPS)
	}
}

func TestZeroRequestsNoNaN(t *testing.T) {
	t.Parallel()

	w := NewWindow(time.Second)
	s := w.Snapshot() // no requests recorded
	if s.ErrorRate != 0 {
		t.Fatalf("ErrorRate = %v, want 0 for zero requests", s.ErrorRate)
	}
}
```

## Review

The rollup is correct when every rate is computed in `float64` with the conversion
applied *before* the division, and when both denominators are guarded. The contrast
test is the proof: on the same `5` and `10`, integer division yields `0` while the
snapshot yields `0.5`. Keeping the `Stats` fields `float64` closes the last hole —
a caller cannot silently truncate a rate back to an integer by storing it in an
`int`. The basis-points figure is the only integer, and it is produced by
`math.Round` at the end, not by truncating division in the middle.

## Resources

- [Go Specification: Arithmetic operators](https://go.dev/ref/spec#Arithmetic_operators) — integer division truncates toward zero.
- [time.Duration.Seconds](https://pkg.go.dev/time#Duration.Seconds) — returns `float64`, which floats the whole expression.
- [math.Round](https://pkg.go.dev/math#Round) — rounding to the nearest integer for reporting.

---

Back to [05-log-level-iota-enum.md](05-log-level-iota-enum.md) | Next: [07-exponential-backoff-duration-arith.md](07-exponential-backoff-duration-arith.md)
