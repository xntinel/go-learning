# Exercise 5: Weighted fair scheduling across QoS priority classes

Two-channel priority generalizes to N classes with weights: a premium tenant
should get proportionally more throughput than a free tenant, but no tenant should
ever be starved to zero. This exercise builds that scheduler with deficit
round-robin over a slice of channels, so a class of weight 3 is served roughly
three times as often as a class of weight 1 while every class is served each frame.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
qos/                         module example.com/qos
  go.mod
  scheduler.go               type Scheduler[T]; NewScheduler; (*Scheduler[T]).Next
  cmd/
    demo/
      main.go                three weighted classes, prints observed per-class shares
  scheduler_test.go          proportional shares, empty-high fallthrough, block, cancel
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `Scheduler[T any]` over `[]<-chan T` with per-class integer weights,
using deficit round-robin so throughput is proportional to weight with no
starvation. `Next(ctx) (T, int, bool)` returns the next item and its class index.
Test: saturated classes are served in proportion to their weights (within
tolerance) and every class is served at least once; an empty high class falls
through to a lower one; an item arriving later unblocks a waiting `Next`;
cancellation exits.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/08-select-priority-and-starvation/05-weighted-fair-scheduler/cmd/demo
cd go-solutions/14-select-and-context/08-select-priority-and-starvation/05-weighted-fair-scheduler
```

### Deficit round-robin without reflect

The two-channel consecutive-high counter from Exercise 2 does not generalize
cleanly to N classes with arbitrary weights. The standard mechanism that does is
**deficit round-robin (DRR)**: each class carries a budget for the current frame,
initialized to its weight. The scheduler scans the classes in order; a class is
eligible while its remaining budget is positive. When it serves a class, it
decrements that class's budget. When a full scan finds no eligible-and-ready
class, the frame is over: every budget is refilled to its weight and a new frame
begins. Over one frame, class `i` is served up to `weight[i]` times, so across
many frames the throughput ratio converges on the weight ratio, and because every
weight is at least 1, every class is served at least once per frame — the
no-starvation floor.

The scan uses a non-blocking peek (`select` with `default`) per class, so an
*empty* class is skipped without consuming its budget or blocking the scheduler:
an empty high class immediately yields to a ready lower one. If a whole scan
serves nothing because the budgeted classes happen to be empty, the frame is
refreshed and retried once; if there is genuinely nothing ready anywhere, `Next`
must not busy-spin — it parks on a short backoff that also watches `ctx.Done()`,
then rechecks.

That backoff is the deliberate trade-off of a static (non-`reflect`) scheduler:
with N channels known only as a slice, you cannot write a fixed blocking `select`
over all of them, so you either poll with a backoff (done here) or drop to
`reflect.Select` (Exercise 8). For a scheduler whose channels are usually
saturated, the backoff path is almost never taken; when it is, a few-millisecond
poll is far cheaper than a hot spin. Exercise 8 shows the `reflect` alternative
for sets that are also *dynamic*.

Create `scheduler.go`:

```go
package qos

import (
	"context"
	"time"
)

// Scheduler serves items from several classes of channel using deficit
// round-robin: throughput is proportional to per-class weight, and no class with
// a positive weight is starved. It is a single-consumer type; call Next from one
// goroutine.
type Scheduler[T any] struct {
	chans     []<-chan T
	weights   []int
	remaining []int         // per-class budget left in the current frame
	idx       int           // round-robin cursor
	poll      time.Duration // backoff when nothing is ready
}

// NewScheduler builds a Scheduler over chans with matching weights (each >= 1).
// poll is the backoff used when every class is momentarily empty. A weight below
// 1 is normalized to 1.
func NewScheduler[T any](chans []<-chan T, weights []int, poll time.Duration) *Scheduler[T] {
	if len(chans) != len(weights) {
		panic("qos: chans and weights length mismatch")
	}
	w := make([]int, len(weights))
	rem := make([]int, len(weights))
	for i, x := range weights {
		if x < 1 {
			x = 1
		}
		w[i] = x
		rem[i] = x
	}
	if poll <= 0 {
		poll = time.Millisecond
	}
	return &Scheduler[T]{chans: chans, weights: w, remaining: rem, poll: poll}
}

// Next returns the next item and its class index. It returns (zero, -1, false)
// when ctx is cancelled.
func (s *Scheduler[T]) Next(ctx context.Context) (T, int, bool) {
	var zero T
	for {
		select {
		case <-ctx.Done():
			return zero, -1, false
		default:
		}
		if v, class, ok := s.tryServe(); ok {
			return v, class, true
		}
		// Nothing ready anywhere: back off, still watching cancellation.
		select {
		case <-ctx.Done():
			return zero, -1, false
		case <-time.After(s.poll):
		}
	}
}

// tryServe attempts one DRR pick without blocking. It refreshes the frame once
// if a full scan under the current budgets finds nothing, so a class whose turn
// was spent but which still has data is not stalled by empty peers.
func (s *Scheduler[T]) tryServe() (T, int, bool) {
	var zero T
	for attempt := 0; attempt < 2; attempt++ {
		for range s.chans {
			i := s.idx
			s.idx = (s.idx + 1) % len(s.chans)
			if s.remaining[i] <= 0 {
				continue
			}
			select {
			case v := <-s.chans[i]:
				s.remaining[i]--
				return v, i, true
			default:
			}
		}
		// A full scan served nothing under current budgets: start a new frame.
		for i := range s.remaining {
			s.remaining[i] = s.weights[i]
		}
	}
	return zero, -1, false
}
```

### The runnable demo

The demo builds three saturated classes with weights 3, 2, 1 and dispatches 600
items, printing the observed share per class. With DRR over full frames of size
`3+2+1 = 6`, the shares land on the 3:2:1 ratio — 300, 200, 100.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/qos"
)

func fill(n int) <-chan int {
	ch := make(chan int, n)
	for i := range n {
		ch <- i
	}
	return ch
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const draws = 600
	chans := []<-chan int{fill(draws), fill(draws), fill(draws)}
	weights := []int{3, 2, 1}

	s := qos.NewScheduler(chans, weights, time.Millisecond)
	counts := make([]int, 3)
	for range draws {
		_, class, ok := s.Next(ctx)
		if !ok {
			break
		}
		counts[class]++
	}
	fmt.Printf("weights 3:2:1 over %d draws -> class0=%d class1=%d class2=%d\n",
		draws, counts[0], counts[1], counts[2])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
weights 3:2:1 over 600 draws -> class0=300 class1=200 class2=100
```

### Tests

`TestProportionalShares` saturates every class and asserts observed shares match
weights within a tolerance and that no class is served zero times.
`TestEmptyHighFallsThrough` gives the highest-weight class no data and asserts a
lower class is served instead. `TestBlocksUntilReady` starts `Next` on empty
channels and delivers an item shortly after, proving the backoff path wakes.
`TestCancellationExits` asserts a cancelled context returns `(_, -1, false)`.

Create `scheduler_test.go`:

```go
package qos

import (
	"context"
	"testing"
	"time"
)

func fill(n int) <-chan int {
	ch := make(chan int, n)
	for i := range n {
		ch <- i
	}
	return ch
}

func TestProportionalShares(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const draws = 600
	chans := []<-chan int{fill(draws), fill(draws), fill(draws)}
	weights := []int{3, 2, 1}
	want := []int{300, 200, 100}

	s := NewScheduler(chans, weights, time.Millisecond)
	counts := make([]int, 3)
	for i := range draws {
		_, class, ok := s.Next(ctx)
		if !ok {
			t.Fatalf("draw %d: ok = false", i)
		}
		counts[class]++
	}
	for c := range counts {
		if counts[c] == 0 {
			t.Fatalf("class %d starved (0 served)", c)
		}
		tol := want[c] / 10 // 10% tolerance
		if counts[c] < want[c]-tol || counts[c] > want[c]+tol {
			t.Fatalf("class %d served %d, want ~%d (+/-%d)", c, counts[c], want[c], tol)
		}
	}
}

func TestEmptyHighFallsThrough(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	high := make(chan int) // empty, open, highest weight
	low := fill(1)
	chans := []<-chan int{high, low}
	s := NewScheduler(chans, []int{5, 1}, time.Millisecond)

	_, class, ok := s.Next(ctx)
	if !ok {
		t.Fatal("ok = false, want a served item")
	}
	if class != 1 {
		t.Fatalf("served class %d, want 1 (empty high falls through)", class)
	}
}

func TestBlocksUntilReady(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan int)
	s := NewScheduler([]<-chan int{ch}, []int{1}, time.Millisecond)

	go func() {
		time.Sleep(20 * time.Millisecond)
		ch <- 42
	}()

	got, class, ok := s.Next(ctx)
	if !ok || class != 0 || got != 42 {
		t.Fatalf("Next = %d,%d,%v, want 42,0,true", got, class, ok)
	}
}

func TestCancellationExits(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan int)
	s := NewScheduler([]<-chan int{ch}, []int{1}, time.Millisecond)

	if _, class, ok := s.Next(ctx); ok || class != -1 {
		t.Fatalf("Next after cancel = class %d ok %v, want -1,false", class, ok)
	}
}
```

## Review

The scheduler is correct when saturated classes are served in proportion to their
weights with no class at zero (`TestProportionalShares`) and an empty high class
yields to a lower one (`TestEmptyHighFallsThrough`). The subtle bug to avoid is a
frame that never refreshes: if `tryServe` returns false whenever the budgeted
classes are momentarily empty — without refreshing and retrying — a class whose
budget was spent but which still holds data can be stalled by empty peers, which
looks like intermittent starvation. Refreshing the frame once per `tryServe` call
(the `attempt < 2` loop) fixes that. The backoff in `Next` is the price of a
static, non-`reflect` scheduler over a slice of channels: `TestBlocksUntilReady`
exercises it, and it never busy-spins because it parks on `time.After`. For a set
that is also dynamic at runtime, Exercise 8 replaces the poll with a blocking
`reflect.Select`.

## Resources

- [Deficit round robin](https://en.wikipedia.org/wiki/Deficit_round_robin) — the weighted-fair-queueing algorithm this implements.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking peeks via `default`.
- [Go generics tutorial](https://go.dev/doc/tutorial/generics) — the `[T any]` payload parameterization.

---

Back to [04-error-channel-precedence.md](04-error-channel-precedence.md) | Next: [06-rate-limited-priority-dispatcher.md](06-rate-limited-priority-dispatcher.md)
