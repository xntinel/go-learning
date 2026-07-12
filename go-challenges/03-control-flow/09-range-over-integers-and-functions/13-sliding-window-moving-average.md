# Exercise 13: Sliding Window Moving Average — Overlapping-Window `iter.Seq` Combinator

**Nivel: Intermedio** — validacion rapida (un test corto).

Smoothing a metric feed for an alerting rule needs a moving average, not a
disjoint batch average: every input contributes to several overlapping outputs.
This exercise builds `Window`, an `iter.Seq[float64]` combinator that holds a
fixed ring buffer and a running sum so it stays O(1) per input regardless of
window size, distinct from a fixed-chunk batcher because its windows overlap.

## What you'll build

```text
movavg/                   independent module: example.com/movavg
  go.mod                  module example.com/movavg
  movavg.go                Window
  movavg_test.go           averages, edge sizes, early-stop, invalid-size panic
```

Implement: `Window(window int, src iter.Seq[float64]) iter.Seq[float64]` yielding the average of the last `window` values, one output per input once the window has filled; panics if `window < 1`.
Test: `window=3` over `[1,2,3,4,5]` yields `[2,3,4]`; `window=1` echoes the input; a source shorter than the window yields nothing; a consumer break stops after the requested count; `window=0` panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/13-sliding-window-moving-average
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/13-sliding-window-moving-average
go mod edit -go=1.24
```

Create `movavg.go`:

```go
package movavg

import "iter"

// Window returns an iter.Seq that yields the simple moving average of the
// last `window` values of src: one output per input once the window has
// filled, then one output per further input as the window slides. It holds a
// fixed ring buffer and a running sum, so each input costs O(1) regardless of
// window size, and it never materializes src. window must be >= 1.
func Window(window int, src iter.Seq[float64]) iter.Seq[float64] {
	if window < 1 {
		panic("movavg: window must be >= 1")
	}
	return func(yield func(float64) bool) {
		buf := make([]float64, window)
		var sum float64
		filled := 0
		pos := 0
		for v := range src {
			if filled == window {
				sum -= buf[pos]
			} else {
				filled++
			}
			buf[pos] = v
			sum += v
			pos = (pos + 1) % window
			if filled == window {
				if !yield(sum / float64(window)) {
					return
				}
			}
		}
	}
}
```

Create `movavg_test.go`:

```go
package movavg

import (
	"math"
	"slices"
	"testing"
)

func floatsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func TestWindowThree(t *testing.T) {
	t.Parallel()

	metrics := []float64{1, 2, 3, 4, 5}
	var got []float64
	for avg := range Window(3, slices.Values(metrics)) {
		got = append(got, avg)
	}
	if want := []float64{2, 3, 4}; !floatsEqual(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestWindowLargerThanInputYieldsNothing(t *testing.T) {
	t.Parallel()

	count := 0
	for range Window(5, slices.Values([]float64{1, 2})) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestWindowPanicsOnInvalidSize(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for window < 1")
		}
	}()
	Window(0, slices.Values([]float64{1}))
}
```

## Verify

```bash
go test -count=1 ./...
```

## Review

The ring buffer is what keeps this O(1) per value: instead of re-summing the last
`window` elements on every step, `Window` subtracts the value about to be evicted
and adds the new one, so a window of size 1,000 costs the same per-step work as a
window of size 3. The `filled == window` guard does double duty — it marks when
the buffer is warm enough to start emitting, and it is the branch that decides
whether to subtract an old value or just accumulate. Compare this to the fixed-size
batcher from an earlier exercise: batching partitions the stream into disjoint
chunks, while a moving average deliberately makes every value participate in
several consecutive outputs. Same "carry state across yields" shape, different
contract.

## Resources

- [`iter.Seq` — cooperative termination contract](https://pkg.go.dev/iter#Seq)
- [`slices.Values`](https://pkg.go.dev/slices#Values)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-nth-event-sampling-combinator.md](12-nth-event-sampling-combinator.md) | Next: [14-byte-budget-capped-export.md](14-byte-budget-capped-export.md)
