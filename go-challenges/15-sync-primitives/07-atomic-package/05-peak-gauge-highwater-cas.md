# Exercise 5: High-Water-Mark Peak Gauge (there is no atomic max)

Operators want to know the *peak*: peak in-flight requests, peak queue depth, peak
latency in a window. That is a running maximum updated from many goroutines at once.
`sync/atomic` has `Add` but no `Max`, so this is the canonical case where you build
the missing operation from a `CompareAndSwap` retry loop.

This module is fully self-contained.

## What you'll build

```text
peakgauge/                 independent module: example.com/peakgauge
  go.mod
  gauge.go                 type PeakGauge; Observe (CAS loop), Peak, Reset
  cmd/
    demo/
      main.go              observes a few values and prints the peak
  gauge_test.go            concurrent-true-max test, retry-forcing test, Example
```

- Files: `gauge.go`, `cmd/demo/main.go`, `gauge_test.go`.
- Implement: a `PeakGauge` over `atomic.Int64`; `Observe(v)` raises the stored max via a CAS loop only when `v` is larger; `Peak` reads it; `Reset` zeroes it.
- Test: many goroutines observe a shuffled set and `Peak` equals the true maximum; a second test interleaves increasing values to force CAS retries.
- Verify: `go test -count=1 -race ./...`

### Building max from CompareAndSwap

There is no `atomic.Int64.Max`. The operation "set the stored value to `v` if `v`
is larger, otherwise leave it" is a conditional read-compute-write, and every one of
those is built from a CAS loop:

```go
for {
	cur := g.max.Load()
	if v <= cur {
		return          // already at least v; nothing to do
	}
	if g.max.CompareAndSwap(cur, v) {
		return          // we raised the mark
	}
	// someone else raised it between Load and CAS; re-read and retry
}
```

Two exits and one retry. The fast exit `v <= cur` means an `Observe` that does not
beat the current peak is a single `Load` and done — the common case is cheap. When
`v` does beat the current peak, we try to install it with `CompareAndSwap(cur, v)`.
If a concurrent `Observe` raised the mark in the meantime, our CAS fails; we loop and
re-read. Critically, on retry the `v <= cur` check runs again against the *new*
current value: if the other goroutine already pushed the peak to something `>= v`,
we now take the fast exit and correctly do nothing. That is why the loop converges
to the true maximum regardless of the order values arrive.

This is a benign-intermediate-states design (from the concepts): the stored value
only ever moves up, so there is no ABA hazard to worry about — a stale `cur` that
lost a race just means we re-read a larger value and back off. `Peak` is a plain
`Load`; `Reset` a plain `Store(0)` for the start of a new window.

Create `gauge.go`:

```go
package peakgauge

import "sync/atomic"

// PeakGauge tracks the maximum value ever observed — peak in-flight requests,
// peak queue depth, peak latency. Safe for concurrent Observe from many
// goroutines. There is no atomic Max, so Observe uses a CompareAndSwap loop.
type PeakGauge struct {
	max atomic.Int64
}

// Observe records v, raising the stored peak if v exceeds it.
func (g *PeakGauge) Observe(v int64) {
	for {
		cur := g.max.Load()
		if v <= cur {
			return
		}
		if g.max.CompareAndSwap(cur, v) {
			return
		}
	}
}

// Peak returns the maximum observed so far.
func (g *PeakGauge) Peak() int64 {
	return g.max.Load()
}

// Reset zeroes the gauge, e.g. at the start of a reporting window.
func (g *PeakGauge) Reset() {
	g.max.Store(0)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/peakgauge"
)

func main() {
	var g peakgauge.PeakGauge
	for _, v := range []int64{3, 7, 2, 9, 4, 9, 1} {
		g.Observe(v)
	}
	fmt.Println("peak:", g.Peak())

	g.Reset()
	fmt.Println("after reset:", g.Peak())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
peak: 9
after reset: 0
```

### Tests

`TestConcurrentTrueMax` has many goroutines observe a shuffled permutation of
`1..N`; whatever the interleaving, `Peak` must equal `N`. `TestForcesRetries` has
each goroutine observe a strictly increasing stream so that late-arriving larger
values force real CAS retries — exercising the loop's retry path — and the final
peak must be the global maximum.

Create `gauge_test.go`:

```go
package peakgauge

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

func TestConcurrentTrueMax(t *testing.T) {
	t.Parallel()

	var g PeakGauge
	const n = 5000
	values := make([]int64, n)
	for i := range values {
		values[i] = int64(i + 1) // 1..n
	}
	rand.Shuffle(n, func(i, j int) { values[i], values[j] = values[j], values[i] })

	const goroutines = 50
	chunk := n / goroutines
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for w := range goroutines {
		lo := w * chunk
		hi := lo + chunk
		if w == goroutines-1 {
			hi = n
		}
		go func() {
			defer wg.Done()
			for _, v := range values[lo:hi] {
				g.Observe(v)
			}
		}()
	}
	wg.Wait()

	if got := g.Peak(); got != int64(n) {
		t.Fatalf("Peak() = %d, want %d", got, n)
	}
}

func TestForcesRetries(t *testing.T) {
	t.Parallel()

	var g PeakGauge
	const goroutines = 20
	const per = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for v := int64(1); v <= per; v++ {
				g.Observe(v)
			}
		}()
	}
	wg.Wait()

	if got := g.Peak(); got != per {
		t.Fatalf("Peak() = %d, want %d", got, per)
	}
}

func ExamplePeakGauge() {
	var g PeakGauge
	g.Observe(5)
	g.Observe(2)
	g.Observe(8)
	g.Observe(3)
	fmt.Println(g.Peak())
	// Output: 8
}
```

## Review

The gauge is correct when `Peak` equals the true maximum of everything observed,
regardless of concurrency and arrival order — `TestConcurrentTrueMax` proves it
against a shuffled `1..N` under `-race`. The mistake that breaks a hand-rolled max is
dropping the loop: `if v > g.max.Load() { g.max.Store(v) }` races — two goroutines
both read a smaller current value and the larger store can be overwritten by the
smaller one. The CAS loop with the `v <= cur` re-check on every retry is what makes
the maximum monotonic and correct.

## Resources

- [`atomic.Int64.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Int64.CompareAndSwap) — the primitive the max is built from.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `Shuffle` for the permutation test.
- [The Go Memory Model](https://go.dev/ref/mem) — the total order the CAS loop relies on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-monotonic-sequence-generator.md](04-monotonic-sequence-generator.md) | Next: [06-admission-inflight-limiter.md](06-admission-inflight-limiter.md)
