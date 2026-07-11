# Exercise 2: Anti-starvation drain of a high-priority job queue

A worker draining a high-priority queue must not permanently starve the
low-priority one. This exercise turns a strict-priority selector into a stateful
`Drainer` that, after N high items in a row, forces one low item when available —
bounded fairness from a single counter. It fixes the classic latent bug where a
`fairness` argument is accepted but never actually consulted.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fairdrain/                   module example.com/fairdrain
  go.mod
  drainer.go                 type Item; type Drainer; NewDrainer; (*Drainer).Next
  cmd/
    demo/
      main.go                saturated hi vs hi+lo, shows fairness bounding the run
  drainer_test.go            bounded-starvation, empty-hi, normalization, cancel, regression
```

Files: `drainer.go`, `cmd/demo/main.go`, `drainer_test.go`.
Implement: a `Drainer` holding a consecutive-high counter and `Next(ctx, hi, lo)
(Item, bool, error)` that serves `hi` preferentially but, after `fairness`
consecutive high items, forces a low item if one is ready.
Test: with `hi` saturated, `lo` is served at least once every `fairness+1` items;
with `hi` empty, `lo` is served immediately; `fairness < 1` normalizes to 1;
cancellation returns `context.Canceled`; a regression test documents that strict
priority (the old no-op) starves `lo`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fairdrain/cmd/demo
cd ~/go-exercises/fairdrain
go mod init example.com/fairdrain
```

### The bug this fixes: a knob that does nothing

The tempting-but-broken version exposes `fairness int` and then ignores it — the
loop is pure strict priority and the argument changes nothing. Under a
continuously-fed `hi`, `lo` is served exactly never, and the operator who set
`fairness = 3` believes the low class is protected. A fairness parameter is only
real if the dispatch loop *reads state derived from it on every iteration.*

The real mechanism is a consecutive-high counter, `highRun`. Every time the
drainer serves a high item, `highRun++`. Before the high peek, if `highRun` has
reached `fairness`, the drainer first tries a non-blocking receive on `lo`; if a
low item is ready it serves that and resets `highRun` to 0. So the invariant is:
**at most `fairness` high items are served consecutively before a low item gets a
turn** — provided a low item is available. If `lo` is empty when the forced turn
comes, there is nothing to be fair about, so the drainer falls back to high; the
counter keeps its value and the forced check retries next call.

Serving a low item at *any* point — forced or via the normal fall-through — resets
`highRun`, because a low item breaking through is exactly the event fairness is
counting toward.

Create `drainer.go`:

```go
package fairdrain

import "context"

// Item is a unit of work drawn from one of two priority queues.
type Item struct {
	Label string
	N     int
}

// Drainer serves a high-priority queue preferentially but bounds starvation of
// the low-priority queue: after fairness consecutive high items it forces one
// low item when available. It is a single-consumer type; do not call Next from
// multiple goroutines concurrently (highRun is not synchronized).
type Drainer struct {
	fairness int
	highRun  int // consecutive high items served since the last low item
}

// NewDrainer builds a Drainer. A fairness below 1 is normalized to 1, which
// forces a low item after every high item (maximum fairness).
func NewDrainer(fairness int) *Drainer {
	if fairness < 1 {
		fairness = 1
	}
	return &Drainer{fairness: fairness}
}

// Next returns the next Item. It prefers hi, except that after fairness
// consecutive high items it serves a ready low item first. It returns
// (Item{}, false, ctx.Err()) when ctx is cancelled.
func (d *Drainer) Next(ctx context.Context, hi, lo <-chan Item) (Item, bool, error) {
	for {
		// 1. Cancellation, strictly first.
		select {
		case <-ctx.Done():
			return Item{}, false, ctx.Err()
		default:
		}
		// 2. Fairness override: if we've served fairness high items in a row,
		//    force a ready low item before peeking high again.
		if d.highRun >= d.fairness {
			select {
			case v := <-lo:
				d.highRun = 0
				return v, true, nil
			default:
				// No low item ready; fall back to normal priority.
			}
		}
		// 3. High-priority peek, non-blocking.
		select {
		case v := <-hi:
			d.highRun++
			return v, true, nil
		default:
		}
		// 4. Blocking fall-through: the only place this loop parks.
		select {
		case <-ctx.Done():
			return Item{}, false, ctx.Err()
		case v := <-hi:
			d.highRun++
			return v, true, nil
		case v := <-lo:
			d.highRun = 0
			return v, true, nil
		}
	}
}
```

### The runnable demo

The demo saturates `hi` and `lo` with more items than it draws, so `hi` never
empties during the run — the only thing that lets `lo` through is the fairness
override. With `fairness = 3`, the pattern is three highs then one low, repeating,
so over 12 draws you see 9 high and 3 low.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/fairdrain"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hi := make(chan fairdrain.Item, 100)
	lo := make(chan fairdrain.Item, 100)
	for i := range 100 {
		hi <- fairdrain.Item{Label: "hi", N: i}
		lo <- fairdrain.Item{Label: "lo", N: i}
	}

	d := fairdrain.NewDrainer(3)
	hiCount, loCount := 0, 0
	for range 12 {
		got, ok, err := d.Next(ctx, hi, lo)
		if err != nil || !ok {
			break
		}
		if got.Label == "hi" {
			hiCount++
		} else {
			loCount++
		}
	}
	fmt.Printf("fairness=3 over 12 draws: hi=%d lo=%d\n", hiCount, loCount)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fairness=3 over 12 draws: hi=9 lo=3
```

### Tests

`TestBoundsStarvationUnderSaturation` is the core proof: with both queues holding
far more items than are drawn (so `hi` never empties), it draws many items and
asserts the *maximum run of consecutive high items never exceeds `fairness`* and
that `lo` is served. `TestStrictPriorityStarvesLow` documents the failure the
`Drainer` removes: an inline strict-priority selector over the same saturated
input serves `lo` exactly zero times. `TestEmptyHighServesLowImmediately`,
`TestFairnessNormalized`, and `TestNextReturnsErrorOnCancel` pin the remaining
contracts.

Create `drainer_test.go`:

```go
package fairdrain

import (
	"context"
	"errors"
	"testing"
)

// fill returns a buffered channel preloaded with n items labelled label.
func fill(label string, n int) chan Item {
	ch := make(chan Item, n)
	for i := range n {
		ch <- Item{Label: label, N: i}
	}
	return ch
}

func TestBoundsStarvationUnderSaturation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const fairness = 3
	const draws = 200
	hi := fill("hi", draws+fairness+10) // never empties during the run
	lo := fill("lo", draws+fairness+10)

	d := NewDrainer(fairness)
	run, maxRun, loSeen := 0, 0, 0
	for i := range draws {
		got, ok, err := d.Next(ctx, hi, lo)
		if err != nil || !ok {
			t.Fatalf("draw %d: ok=%v err=%v", i, ok, err)
		}
		if got.Label == "hi" {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
			loSeen++
		}
	}
	if maxRun > fairness {
		t.Fatalf("max consecutive high = %d, want <= %d", maxRun, fairness)
	}
	if loSeen == 0 {
		t.Fatal("low queue was never served (starved)")
	}
}

func TestStrictPriorityStarvesLow(t *testing.T) {
	t.Parallel()

	// The old no-op behavior: strict priority, no fairness state. Documented
	// here to show exactly the failure the Drainer removes.
	strictNext := func(hi, lo <-chan Item) Item {
		select {
		case v := <-hi:
			return v
		default:
		}
		select {
		case v := <-hi:
			return v
		case v := <-lo:
			return v
		}
	}

	hi := fill("hi", 300)
	lo := fill("lo", 300)
	loSeen := 0
	for range 200 { // hi never empties, so low never gets a turn
		if strictNext(hi, lo).Label == "lo" {
			loSeen++
		}
	}
	if loSeen != 0 {
		t.Fatalf("strict priority served low %d times; expected total starvation", loSeen)
	}
}

func TestEmptyHighServesLowImmediately(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hi := make(chan Item) // empty, open
	lo := fill("lo", 1)

	d := NewDrainer(5)
	got, ok, err := d.Next(ctx, hi, lo)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Label != "lo" {
		t.Fatalf("got %q, want lo", got.Label)
	}
}

func TestFairnessNormalized(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hi := fill("hi", 100)
	lo := fill("lo", 100)

	// fairness 0 normalizes to 1: at most one high before a forced low.
	d := NewDrainer(0)
	run, maxRun := 0, 0
	for range 40 {
		got, _, _ := d.Next(ctx, hi, lo)
		if got.Label == "hi" {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	if maxRun > 1 {
		t.Fatalf("max consecutive high = %d, want <= 1 after normalization", maxRun)
	}
}

func TestNextReturnsErrorOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hi := make(chan Item)
	lo := make(chan Item)

	d := NewDrainer(3)
	_, ok, err := d.Next(ctx, hi, lo)
	if ok {
		t.Fatal("ok = true after cancel, want false")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

## Review

The drainer is correct when `TestBoundsStarvationUnderSaturation` shows a maximum
consecutive-high run of exactly `fairness` under a `hi` channel that never empties
— that is the fairness guarantee made concrete, and `TestStrictPriorityStarvesLow`
is its foil, showing the no-op version serving `lo` zero times on the same input.
The trap to avoid is resetting `highRun` in the wrong place or not at all: reset
it whenever a low item is served (forced or fall-through), never when a high item
is served, or the counter drifts and the bound is lost. Note `Next` is
single-consumer by contract; `highRun` is deliberately unsynchronized, so a
concurrent caller would race — run each `Drainer` from one goroutine. The
fairness override deliberately *falls back* to high when `lo` is empty, so a
momentarily-empty low queue never stalls the worker.

## Resources

- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking receive via `default`.
- [Priority queueing and starvation](https://en.wikipedia.org/wiki/Starvation_(computer_science)) — why bounded fairness matters under sustained load.
- [Deficit round robin](https://en.wikipedia.org/wiki/Deficit_round_robin) — the weighted generalization of the consecutive-high counter, built in Exercise 5.

---

Back to [01-priority-peek-consumer.md](01-priority-peek-consumer.md) | Next: [03-graceful-shutdown-drain.md](03-graceful-shutdown-drain.md)
