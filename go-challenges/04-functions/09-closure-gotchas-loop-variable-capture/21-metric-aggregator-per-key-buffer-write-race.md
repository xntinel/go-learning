# Exercise 21: Metric Aggregator: Per-Key Shared Buffer Writes Using Loop Index

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A metric aggregator sums samples by key into a preallocated buffer,
dispatching one goroutine per sample. Each worker needs to know which slot
its key maps to. The trap: if every worker computes that slot from a single
shared index variable instead of its own, every sample's value lands in the
SAME slot — whichever key the dispatch loop last got to — and every other
key's total is silently zero.

## What you'll build

```text
metricagg/                   independent module: example.com/metricagg
  go.mod                     go 1.24
  metricagg.go                 Sample, Aggregate, AggregateBuggy
  cmd/
    demo/
      main.go                runnable demo: aggregate samples, print per-key totals
  metricagg_test.go            correct sums, shared-index collapse, single-key edge case
```

- Files: `metricagg.go`, `cmd/demo/main.go`, `metricagg_test.go`.
- Implement: `Aggregate(samples, keyIndex)` spawning one goroutine per sample that adds into its own `atomic.Int64` slot passed as a parameter; `AggregateBuggy` spawning goroutines that all read a single shared `idx` variable instead.
- Test: assert per-key totals are correct for the correct version, `-race` clean; assert the buggy version collapses every sample's value into the last key's slot, deterministically, using a barrier so the test never flakes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metricagg/cmd/demo
cd ~/go-exercises/metricagg
go mod init example.com/metricagg
go mod edit -go=1.24
```

### Why the slot index must be a parameter, and why atomic fits here

Unlike a fan-out where every worker owns a disjoint slot, multiple samples
for the SAME key genuinely need to add into the SAME counter concurrently.
`Aggregate` passes each sample's `idx` and `Value` to its goroutine as
arguments and uses `atomic.Int64.Add`, which is safe for concurrent
same-slot increments with no lock at all — the standard tool when several
goroutines legitimately need to update one shared number.

`AggregateBuggy` recreates the classic mistake deliberately: `idx` is
declared once, outside the loop, and every worker reads it instead of its
own sample's key index. A worker never runs before the dispatch loop finishes
assigning the rest, so this exercise makes the worst case *deterministic*
instead of just possible: every worker blocks on a `release` channel until
dispatch has finished advancing `idx` to the last sample's key, then they all
read it. The write itself is mutex-protected so it is not a data race; the
bug is entirely in *which* slot every worker targets. Because addition is
commutative, the buggy version's collapsed slot deterministically ends up
holding the sum of every sample's value, and every other slot stays at zero.

Create `metricagg.go`:

```go
package metricagg

import (
	"sync"
	"sync/atomic"
)

// Sample is one metric observation for a key.
type Sample struct {
	Key   string
	Value int64
}

// Aggregate sums every sample's value into its key's own slot concurrently.
// Each worker goroutine receives its slot index as a parameter and adds into
// counters[i] with atomic.Int64.Add, so concurrent samples for the SAME key
// (a genuinely shared slot) accumulate correctly with no lock, and samples
// for different keys never contend at all.
func Aggregate(samples []Sample, keyIndex map[string]int) []int64 {
	counters := make([]atomic.Int64, len(keyIndex))
	var wg sync.WaitGroup
	for _, s := range samples {
		idx := keyIndex[s.Key]
		wg.Add(1)
		go func(i int, v int64) {
			defer wg.Done()
			counters[i].Add(v)
		}(idx, s.Value)
	}
	wg.Wait()

	out := make([]int64, len(counters))
	for i := range counters {
		out[i] = counters[i].Load()
	}
	return out
}

// AggregateBuggy sums samples too, but every worker computes its target slot
// from a SINGLE shared `idx` variable declared outside the loop instead of
// its own key's index. A worker never runs before the dispatch loop finishes
// assigning the rest, so this exercise makes that worst case deterministic
// with a barrier: every worker blocks until dispatch has finished advancing
// idx to the LAST sample's key, then they all read it. The result: every
// sample's value lands in the SAME slot -- the last sample's key -- and every
// other key's total is silently zero.
func AggregateBuggy(samples []Sample, keyIndex map[string]int) []int64 {
	buf := make([]int64, len(keyIndex))
	var idx int // BUG: one shared index for every worker instead of its own
	var mu sync.Mutex
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range samples {
		idx = keyIndex[s.Key]
		wg.Add(1)
		go func(v int64) {
			defer wg.Done()
			<-release // wait for dispatch to finish advancing idx
			mu.Lock()
			buf[idx] += v // BUG: idx now holds the LAST sample's slot for every worker
			mu.Unlock()
		}(s.Value)
	}
	close(release) // only now do workers read idx; every read sees the final value
	wg.Wait()
	return buf
}
```

### The runnable demo

The demo aggregates five samples across three keys with both variants and
prints the totals in `[cpu mem disk]` order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metricagg"
)

func main() {
	keyIndex := map[string]int{"cpu": 0, "mem": 1, "disk": 2}
	samples := []metricagg.Sample{
		{Key: "cpu", Value: 10},
		{Key: "mem", Value: 5},
		{Key: "cpu", Value: 3},
		{Key: "disk", Value: 7},
		{Key: "mem", Value: 2},
	}

	correct := metricagg.Aggregate(samples, keyIndex)
	fmt.Println("correct totals [cpu mem disk]:", correct)

	buggy := metricagg.AggregateBuggy(samples, keyIndex)
	fmt.Println("buggy   totals [cpu mem disk]:", buggy)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct totals [cpu mem disk]: [13 7 7]
buggy   totals [cpu mem disk]: [0 27 0]
```

### Tests

`TestAggregate` asserts the correct per-key sums. `TestAggregateBuggy
CollapsesIntoLastKey` asserts every slot except the last sample's key stays
at zero, and that slot holds the sum of every sample's value regardless of
key. `TestAggregateSingleKeyEdgeCase` covers the boundary where the bug
cannot manifest because there is only one key to collapse into anyway.

Create `metricagg_test.go`:

```go
package metricagg

import "testing"

func testSamples() ([]Sample, map[string]int) {
	keyIndex := map[string]int{"cpu": 0, "mem": 1, "disk": 2}
	samples := []Sample{
		{Key: "cpu", Value: 10},
		{Key: "mem", Value: 5},
		{Key: "cpu", Value: 3},
		{Key: "disk", Value: 7},
		{Key: "mem", Value: 2},
	}
	return samples, keyIndex
}

func TestAggregate(t *testing.T) {
	samples, keyIndex := testSamples()
	got := Aggregate(samples, keyIndex)
	want := []int64{13, 7, 7} // cpu: 10+3, mem: 5+2, disk: 7
	if len(got) != len(want) {
		t.Fatalf("totals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("totals = %v, want %v", got, want)
		}
	}
}

func TestAggregateBuggyCollapsesIntoLastKey(t *testing.T) {
	samples, keyIndex := testSamples()
	got := AggregateBuggy(samples, keyIndex)

	var total int64
	for _, s := range samples {
		total += s.Value
	}
	lastIdx := keyIndex[samples[len(samples)-1].Key]

	for i, v := range got {
		if i == lastIdx {
			if v != total {
				t.Fatalf("totals[%d] = %d, want %d (every sample's value, since all workers shared idx)", i, v, total)
			}
			continue
		}
		if v != 0 {
			t.Fatalf("totals[%d] = %d, want 0 (never targeted by the shared index)", i, v)
		}
	}
}

func TestAggregateSingleKeyEdgeCase(t *testing.T) {
	// With exactly one key, the shared-index bug has no other slot to
	// collapse into, so both variants must agree.
	keyIndex := map[string]int{"solo": 0}
	samples := []Sample{{Key: "solo", Value: 4}, {Key: "solo", Value: 6}}

	correct := Aggregate(samples, keyIndex)
	buggy := AggregateBuggy(samples, keyIndex)
	if correct[0] != 10 {
		t.Fatalf("correct[0] = %d, want 10", correct[0])
	}
	if buggy[0] != 10 {
		t.Fatalf("buggy[0] = %d, want 10", buggy[0])
	}
}
```

## Review

The aggregator is correct when every key's total sums exactly its own
samples, no matter how many goroutines race to update it —
`atomic.Int64.Add` is what makes concurrent same-slot increments safe without
a lock. The bug in `AggregateBuggy` is not about atomicity at all: the write
itself is mutex-protected, so there is no data race. It is about which slot
every worker targets — a shared `idx` read after the dispatch loop has moved
on means every worker targets whatever key the loop finished on, and because
addition is commutative, they all silently pile into that one slot. The
barrier only makes this deterministic for testing; without it the same bug
just becomes timing-dependent instead of guaranteed. Run `go test -race` —
the atomic version and the mutex-guarded buggy version must both be clean.

## Resources

- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — safe concurrent increments to a shared counter without a lock.
- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes, not when the goroutine runs.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why the slot index is safe per-iteration but still best passed explicitly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-request-handler-context-cancel-goroutine-escape.md](20-request-handler-context-cancel-goroutine-escape.md) | Next: [22-cache-key-invalidation-callback-registration.md](22-cache-key-invalidation-callback-registration.md)
